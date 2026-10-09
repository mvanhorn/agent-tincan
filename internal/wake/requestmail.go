package wake

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// Request emails.
//
// An email agent with include_requests on gets each open ask (Options.
// OpenAsks) in an email of its own on its primary email path, with the
// request's text and a reply tag in the subject (Options.RequestTag), so it
// can act on the email alone while its tailnet path to the relay is down.
// One email per ask keeps a reply to one email from quoting, and answering,
// another ask's tag.
//
// Within a wake, asks never emailed go first (urgent, then oldest), then
// re-sends, oldest first, of asks last emailed at least a wake grace ago.
// The whole wake counts once against MaxPerHour, however many emails it
// sends, so a backlog of open asks cannot starve a new ask's first email. A
// new ask that arrives while a follow-up is due gets its first email on the
// debounce (armMail), and the follow-up keeps its time.
//
// Held requests, pings and notifies never reach a request email: they and
// unseen replies are still told by the count-only email, as is everything
// on a fallback path, which never carries request text. Request text, tags
// and the AgentMail key never go to the relay log or the audit log.

// maxRequestEmailBody caps the request text one email carries.
const maxRequestEmailBody = 64 << 10

// mailsRequests reports whether agent gets request emails: an email agent
// with include_requests on, and a relay that can list its open asks and
// mint their tags.
func (w *Waker) mailsRequests(agent string) bool {
	t, ok := w.cfg[agent]
	return ok && t.Method == Email && t.IncludeRequests && w.opts.OpenAsks != nil && w.opts.RequestTag != nil
}

// armMail schedules agent's mail-only nudge on the debounce, unless one is
// already due. It emails asks that arrived while a follow-up was due without
// moving that follow-up. Caller holds w.mu.
func (w *Waker) armMail(agent string) {
	if w.stopped || w.mailTimers[agent] != nil {
		return
	}
	w.wg.Add(1)
	w.mailTimers[agent] = time.AfterFunc(w.opts.Debounce, func() {
		defer w.wg.Done()
		w.mu.Lock()
		delete(w.mailTimers, agent)
		gen := w.replyGen[agent]
		w.mu.Unlock()
		w.deliver(agent, &nudge{requests: 1, mailOnly: true}, gen)
	})
}

// sendRequestMails sends agent's request emails for nudge p, and the
// count-only email for whatever else is waiting (pings, notifies, unseen
// replies), as one wake. It reports false, having sent nothing, when the
// relay cannot list open asks or mint a tag, or none are open: the caller
// then sends the count-only email instead.
func (w *Waker) sendRequestMails(ctx context.Context, agent string, p *nudge, replies int) bool {
	asks, err := w.opts.OpenAsks(agent)
	if err != nil {
		log.Printf("wake %s: open asks: %v; sending a count-only wake", agent, err)
		return false
	}
	if len(asks) == 0 {
		return false
	}
	tags := make(map[string]string, len(asks))
	for _, a := range asks {
		if tags[a.ID] = w.opts.RequestTag(a); tags[a.ID] == "" {
			return false
		}
	}
	at := w.opts.Now()
	picked, prev, queuedAsks := w.pickRequestMails(agent, asks, at)
	others := 0
	if w.opts.Queued != nil && !p.mailOnly {
		others = max(w.opts.Queued(agent)-queuedAsks, 0)
	}
	if p.mailOnly {
		replies = 0
	}
	later := func() {
		// A mail-only nudge leaves the pending follow-up alone.
		if !p.mailOnly || !w.hasPending(agent) {
			w.followUpLater(agent, p.requests)
		}
	}
	if len(picked) == 0 && others == 0 && replies == 0 {
		later() // every open ask was emailed within its grace
		return true
	}
	w.mu.Lock()
	allowed := w.allow(agent)
	w.mu.Unlock()
	if !allowed {
		w.unpick(agent, picked, prev)
		w.record(ctx, "wake_skipped", agent, "", "hourly wake budget used up; requests and replies stay queued")
		later()
		return true
	}
	// Moves the silent episode on as any wake does; request emails still go
	// on the primary path.
	w.pathStep(agent, p.followUp)
	cfg := w.cfg[agent]
	// Read before the wake is marked in flight, as in deliver.
	detailOthers, detailReplies, ids := w.mailedIDs(agent, p, asks, picked, others, replies)
	if w.audit != nil {
		w.audit.BeginWake(agent)
	}
	var code, failedSends int
	var firstErr error
	var failed []envelope.Request // asks whose email failed, for unpick
	send := func(subject, text string) error {
		c, err := w.sendEmail(ctx, cfg, subject, text)
		if err != nil {
			select {
			case <-time.After(w.opts.RetryDelay):
				c, err = w.sendEmail(ctx, cfg, subject, text)
			case <-ctx.Done():
			}
		}
		if err != nil {
			failedSends++
			if firstErr == nil {
				firstErr = err
			}
		}
		if err == nil {
			code = c
		}
		return err
	}
	for _, a := range picked {
		subject, text := requestEmail(a, tags[a.ID])
		if send(subject, text) != nil {
			failed = append(failed, a)
		}
	}
	w.unpick(agent, failed, prev) // a failed ask counts as never emailed
	total := len(picked)
	if others > 0 || replies > 0 {
		total++
		_ = send(countOnlySubject, WaitingMessage(others, replies))
	}
	if firstErr != nil {
		reason := publicReason(firstErr)
		if failedSends < total {
			reason += fmt.Sprintf(" (%d of %d emails failed)", failedSends, total)
		}
		log.Printf("wake %s: %s", agent, reason)
		w.record(ctx, "wake_failed", agent, "", reason)
		w.rememberSend(ctx, agent, store.Wake{At: at, Result: reason}, false)
		w.endWake(ctx, agent, at)
		later()
		return true
	}
	w.rememberSend(ctx, agent, store.Wake{At: at, Result: envelope.WakeOK}, true)
	detail := fmt.Sprintf("%s, HTTP %d, %d open asks, %d request emails", pathLabel(cfg, 0), code, len(asks), len(picked))
	if detailOthers > 0 {
		detail += fmt.Sprintf(", %d other requests waiting", detailOthers)
	}
	if detailReplies > 0 {
		detail += fmt.Sprintf(", %d unseen replies", detailReplies)
	}
	requestID, idsSegment := wokeIDs(ids)
	detail += idsSegment
	log.Printf("wake %s: ok, %s", agent, detail)
	w.record(ctx, "woke", agent, requestID, detail)
	w.endWake(ctx, agent, at)
	later()
	return true
}

// mailedIDs is what a request-email wake covers, for its woke row: the asks
// it emails, then the other requests still queued and the replies still
// unseen that its count-only email tells of, with the counts of those two
// for the detail. Without the id hooks (coveredIDs), the row keeps the
// counts given and lists the emailed asks only when nothing else rides the
// wake, so a lone id is never named as its only cause. A mail-only nudge,
// and a wake that sent no count-only email, cover just their asks.
func (w *Waker) mailedIDs(agent string, p *nudge, asks, picked []envelope.Request, others, replies int) (int, int, []string) {
	ids := make([]string, 0, len(picked))
	for _, a := range picked {
		ids = append(ids, a.ID)
	}
	if p.mailOnly || (others == 0 && replies == 0) {
		// Nothing but the asks was emailed, so nothing else is named, even
		// if work turned up after the wake counted it.
		return others, replies, ids
	}
	queued, unseen, ok := w.coveredIDs(agent)
	if !ok {
		if others > 0 || replies > 0 {
			return others, replies, nil
		}
		return others, replies, ids
	}
	open := make(map[string]bool, len(asks))
	for _, a := range asks {
		open[a.ID] = true
	}
	var otherIDs []string
	for _, id := range queued {
		if !open[id] {
			otherIDs = append(otherIDs, id)
		}
	}
	ids = append(ids, otherIDs...)
	return len(otherIDs), len(unseen), append(ids, unseen...)
}

// hasPending reports whether agent has a nudge waiting for its timer.
func (w *Waker) hasPending(agent string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending[agent] != nil
}

// pickRequestMails chooses which of agent's open asks to email now: those
// never emailed, in the order given (urgent, then oldest), then those last
// emailed at least their grace ago, oldest first. It marks the picked asks
// emailed at, so a wake running alongside does not email them too, and
// returns their previous times for unpick. It forgets asks no longer open,
// and counts the open asks still queued.
func (w *Waker) pickRequestMails(agent string, asks []envelope.Request, at time.Time) (picked []envelope.Request, prev map[string]time.Time, queuedAsks int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	last := w.mailed[agent]
	kept := make(map[string]time.Time, len(asks))
	prev = map[string]time.Time{}
	var resend []envelope.Request
	for _, a := range asks {
		if a.Status == envelope.StatusQueued {
			queuedAsks++
		}
		when, ok := last[mailKey(a)]
		switch {
		case !ok:
			picked = append(picked, a)
		case w.resendDue(a, at.Sub(when)):
			resend = append(resend, a)
			prev[mailKey(a)] = when
		default:
			kept[mailKey(a)] = when
			continue
		}
		kept[mailKey(a)] = at
	}
	slices.SortStableFunc(resend, func(a, b envelope.Request) int { return a.CreatedAt.Compare(b.CreatedAt) })
	w.mailed[agent] = kept
	return append(picked, resend...), prev, queuedAsks
}

// mailKey keys an ask in Waker.mailed by its id and clarification round.
// Answering a clarification starts a new round whose tag the earlier email
// does not carry, so the ask then counts as never emailed and its next
// request email goes out on the debounce, not after a wake grace.
func mailKey(a envelope.Request) string {
	return a.ID + "#" + strconv.Itoa(a.AnsweredExchanges())
}

// unpick puts asks whose email was not sent back to their previous email
// time, or to never emailed.
func (w *Waker) unpick(agent string, asks []envelope.Request, prev map[string]time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := w.mailed[agent]
	if m == nil {
		return
	}
	for _, a := range asks {
		if when, ok := prev[mailKey(a)]; ok {
			m[mailKey(a)] = when
		} else {
			delete(m, mailKey(a))
		}
	}
}

// resendDue reports whether an ask last emailed age ago is due again: after
// WakeGrace, or UrgentWakeGrace for an urgent ask when that is shorter.
// With follow-up off nothing is re-sent. Caller holds w.mu.
func (w *Waker) resendDue(a envelope.Request, age time.Duration) bool {
	grace := w.opts.WakeGrace
	if grace <= 0 {
		return false
	}
	if a.Urgent && w.opts.UrgentWakeGrace > 0 && w.opts.UrgentWakeGrace < grace {
		grace = w.opts.UrgentWakeGrace
	}
	return age >= grace
}

// requestEmail is the subject and text of req's request email. The subject
// carries the id and tag a reply is matched by. The text carries the
// request, cut at maxRequestEmailBody, its earlier clarification rounds, how
// many attachments it has (never their names), and how to answer.
func requestEmail(req envelope.Request, tag string) (subject, text string) {
	subject = fmt.Sprintf("Agent Tincan: request from %s [tincan %s.%s]", req.From, req.ID, tag)
	var b strings.Builder
	fmt.Fprintf(&b, "Agent Tincan: request %s from %s. Your teammate is waiting for your answer.\n", req.ID, req.From)
	if req.Urgent {
		b.WriteString("It is URGENT: handle it before anything else.\n")
	}
	b.WriteString("\nRequest:\n")
	body, cut := capText(req.Body, maxRequestEmailBody)
	b.WriteString(body)
	if cut {
		fmt.Fprintf(&b, "\n\n[The request is longer than 64 KiB and was cut here: fetch the rest with `tincan get %s`.]", req.ID)
	}
	b.WriteString("\n")
	if len(req.Exchanges) > 0 {
		b.WriteString("\nEarlier clarification:\n")
		for i, ex := range req.Exchanges {
			fmt.Fprintf(&b, "Q%d (you): %s\n", i+1, ex.Question)
			if ex.Answer != "" {
				fmt.Fprintf(&b, "A%d (%s): %s\n", i+1, req.From, ex.Answer)
			}
		}
	}
	if n := len(req.Attachments); n > 0 {
		fmt.Fprintf(&b, "\n%d %s: fetch with tincan (`tincan get %s` lists them).\n", n, plural(n, "attachment", "attachments"), req.ID)
	}
	b.WriteString("\nTo answer: Reply to this email in the same thread and keep the subject as it is, since it carries this request's tag. " +
		"Your reply becomes your answer. To fail or decline the request instead, start the first line with \"failed:\" or \"declined:\" and the reason. " +
		"Email cannot ask a clarifying question; if you need one, ask it through tincan. " +
		"When tincan can reach the relay you can answer there instead (`tincan inbox`).\n")
	return subject, b.String()
}

// capText cuts s to at most n bytes on a rune boundary and reports whether
// it cut anything.
func capText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
