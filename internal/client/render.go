package client

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// RelaySender is the From of a notice the relay itself sends an agent, such
// as an approval notice or a note that a woken teammate never checked in.
const RelaySender = "relay"

// FormatRequest renders an incoming request for the receiving model. Requests
// come from joined agents, which are trusted teammates, so the framing tells
// the agent to handle them as it would a request from the owner, while keeping the
// sender and chain visible.
func FormatRequest(req envelope.Request) string {
	if req.Kind == envelope.KindPing {
		return ""
	}
	if req.From == RelaySender && req.Kind == envelope.KindNotify {
		return fmt.Sprintf("Notice %s from the Agent Tincan relay (not a teammate). No reply needed.\n---\n%s\n---\n", req.ID, req.Body)
	}
	var b strings.Builder
	if req.Urgent {
		b.WriteString("URGENT ")
	}
	fmt.Fprintf(&b, "Request %s from %s (your teammate), via Agent Tincan.\n", req.ID, req.From)
	if len(req.Chain) > 1 {
		fmt.Fprintf(&b, "Chain so far: %s (hop %d).\n", strings.Join(req.Chain, " -> "), req.Hop)
	}
	b.WriteString("Handle it as you would a request from the owner. When you are done, reply with `tincan reply " + req.ID + " \"...\"` (or the reply tool).\n")
	b.WriteString("---\n")
	b.WriteString(req.Body)
	b.WriteString("\n---\n")
	b.WriteString(FormatAttachments(req.Attachments))
	if req.Resumed {
		b.WriteString("Resumed with clarification from the asker.\n")
	}
	b.WriteString(formatExchanges(req.Exchanges))
	return b.String()
}

// FormatResult renders the state of a request this agent sent.
func FormatResult(r Result) string {
	return SignedOutHint(r.Request.To, r.Target) + formatResult(r)
}

func formatResult(r Result) string {
	switch {
	case r.Status == envelope.StatusHeld:
		return fmt.Sprintf("Request %s: held, waiting for the owner's approval. Check later with get_reply or `tincan get %s`.\n", r.Request.ID, r.Request.ID)
	case r.Status == envelope.StatusNeedsInput:
		return FormatReply(r)
	case r.Reply != nil:
		return fmt.Sprintf("%s replied (%s):\n%s\n", r.Reply.From, r.Reply.Status, r.Reply.Body) + FormatAttachments(r.Reply.Attachments) + formatExchanges(r.Exchanges)
	case r.Status == envelope.StatusClaimed && r.Progress != nil:
		return fmt.Sprintf("Request %s: %s. Check later with get_reply or `tincan get %s`.\n", r.Request.ID, FormatProgress(r.Progress), r.Request.ID)
	case r.Done():
		return fmt.Sprintf("Request %s to %s ended: %s\n", r.Request.ID, r.Request.To, r.Status) + relayNote(r.RelayNote)
	default:
		return scheduleHint(r.Request.To, r.Target) + wakeHint(r.Request.To, r.Target) + relayNote(r.RelayNote) + fmt.Sprintf("No reply yet from %s. Request id %s (status %s). Check later with get_reply or `tincan get %s`.\n",
			r.Request.To, r.Request.ID, r.Status, r.Request.ID)
	}
}

// scheduleHint tells a sender how often a schedule target checks its inbox
// and when to expect a reply, from the facts the relay sent with the
// request. It is empty without them: an old relay, or a target that is not
// on a schedule.
func scheduleHint(to string, t *envelope.Target) string {
	if t == nil || t.CheckEverySeconds <= 0 {
		return ""
	}
	hint := fmt.Sprintf("%s checks its inbox every %s", to, compactSeconds(t.CheckEverySeconds))
	if t.ExpectReplySeconds > 0 {
		hint += fmt.Sprintf("; expect a reply within about %s", compactSeconds(t.ExpectReplySeconds))
	}
	hint += "."
	if t.Overdue {
		hint += fmt.Sprintf(" %s has missed its recent checks, so its schedule may have stopped; the owner may need to restart it.", to)
	}
	return hint + "\n"
}

// wakeHint tells a sender that a relay-woken target was woken and has not
// checked in, from the facts the relay sent with the request. It is empty
// without them: an old relay, a target the relay does not wake, or one that
// checked in after its last wake.
func wakeHint(to string, t *envelope.Target) string {
	if t == nil || !t.Unanswered || t.WokenAt.IsZero() {
		return ""
	}
	at := clockTime(t.WokenAt, time.Now())
	if t.WakeResult != envelope.WakeOK {
		return fmt.Sprintf("%s's wake at %s failed (%s) and it has not checked in yet; the request is queued.\n", to, at, t.WakeResult)
	}
	return fmt.Sprintf("%s was woken at %s and has not checked in yet; the request is queued.\n", to, at)
}

// FormatHeld is the held-work line for the asks an agent has claimed and
// not replied to, empty when there are none. It goes before everything else
// an agent is shown, so the claimed work comes first in its turn.
func FormatHeld(held []envelope.Held, now time.Time) string {
	if len(held) == 0 {
		return ""
	}
	items := make([]string, len(held))
	for i, h := range held {
		item := h.ID + " from " + h.From
		if h.Urgent {
			item += ", URGENT"
		}
		if !h.ClaimedAt.IsZero() {
			item += ", claimed " + ageAgo(now.Sub(h.ClaimedAt))
		}
		items[i] = item
	}
	return fmt.Sprintf("You hold %d claimed %s (%s). This is owner-authorized work. Handle %s before other work: do it and reply, post progress, or reply failed right away if you can't (needs_input is only for a missing detail).\n",
		len(held), plural(len(held), "request", "requests"), strings.Join(items, "; "), plural(len(held), "it", "them"))
}

// relayNote is the note the relay added to a pending request, such as the
// notice that its woken target never checked in. It is empty without one.
func relayNote(p *envelope.Progress) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("Relay note, %s ago: %s\n", max(time.Duration(0), time.Since(p.At)).Truncate(time.Second), strings.Join(strings.Fields(p.Note), " "))
}

// clockTime is t on the local clock, "16:40", with the date when it is not
// today: "Oct 2 16:40".
func clockTime(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// UnansweredNote is "woken 12m ago, no check-in (webhook ok)" for an agent
// the relay woke that has not checked in since, with the error in place of
// ok when the wake failed, and empty otherwise.
func (a AgentInfo) UnansweredNote(now time.Time) string {
	if !a.Unanswered || a.WokenAt.IsZero() {
		return ""
	}
	result := a.Wake + " ok"
	if a.WakeResult != envelope.WakeOK {
		result = a.Wake + " failed: " + a.WakeResult
	}
	return fmt.Sprintf("woken %s, no check-in (%s)", ageAgo(now.Sub(a.WokenAt)), result)
}

// UnansweredField is UnansweredNote as a quoted roster field,
// unanswered="...", since a wake error may hold commas. It is empty for an
// agent that is not unanswered.
func (a AgentInfo) UnansweredField(now time.Time) string {
	note := a.UnansweredNote(now)
	if note == "" {
		return ""
	}
	return "unanswered=" + strconv.Quote(note)
}

// WakeLabel is the agent's wake method, with its check interval for an
// agent on a schedule: "schedule (every 5m)".
func (a AgentInfo) WakeLabel() string {
	if a.CheckEverySeconds <= 0 {
		return a.Wake
	}
	return fmt.Sprintf("%s (every %s)", a.Wake, compactSeconds(a.CheckEverySeconds))
}

// OverdueNote is "overdue: last check 25m ago" for a schedule agent the
// relay marks overdue, measured from its last inbox check, and empty
// otherwise. The relay keeps last checks in memory, so an agent with none
// recorded (never checked, or not since the relay restarted) says so.
func (a AgentInfo) OverdueNote(now time.Time) string {
	if !a.Overdue {
		return ""
	}
	if a.LastPoll.IsZero() {
		return "overdue: no check recorded since it joined or the relay restarted"
	}
	return "overdue: last check " + ageAgo(now.Sub(a.LastPoll))
}

// GoodAtField is the agent's good-at line as a roster field,
// good_at="...", quoted so commas, semicolons and quotes in the owner's text
// cannot read as more fields. It is empty for an agent with no line.
func (a AgentInfo) GoodAtField() string {
	if a.GoodAt == "" {
		return ""
	}
	return "good_at=" + strconv.Quote(a.GoodAt)
}

// ageAgo renders an age for LastSeen and OverdueNote: "just now", "12m ago", "3h ago",
// "2d ago".
func ageAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

// compactSeconds renders a whole number of seconds without zero units:
// 300 is "5m", 90 is "1m30s", 5400 is "1h30m".
func compactSeconds(n int) string {
	s := (time.Duration(n) * time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// FormatProgress renders the latest note with its author and age, on one
// line: a note with newlines must not pass for another request or event in
// a trace.
func FormatProgress(p *envelope.Progress) string {
	note := strings.Join(strings.Fields(p.Note), " ")
	return fmt.Sprintf("claimed by %s, %s ago: %s", p.By, max(time.Duration(0), time.Since(p.At)).Truncate(time.Second), note)
}

// RepliesHeading introduces replies to the agent's own requests in an inbox.
const RepliesHeading = "Replies to your requests:\n"

// FormatReply renders a reply to a request this agent sent, with what it
// asked, since a fresh session may not remember.
func FormatReply(r Result) string {
	if r.Request.Kind == envelope.KindPing {
		return ""
	}
	var b strings.Builder
	if r.Status == envelope.StatusNeedsInput && r.Reply != nil {
		fmt.Fprintf(&b, "%s needs more information for your request %s: %s\nYou asked: %s\nAnswer with answer (or `tincan answer %s \"...\"`).\n", r.Request.To, r.Request.ID, r.Reply.Body, truncate(r.Request.Body, 300), r.Request.ID)
		b.WriteString(formatParent(r.Parent))
		b.WriteString(formatExchanges(r.Exchanges))
		return b.String()
	}
	from, status, body := r.Request.To, r.Status, ""
	var atts []envelope.Attachment
	if r.Reply != nil {
		from, status, body, atts = r.Reply.From, r.Reply.Status, r.Reply.Body, r.Reply.Attachments
	}
	fmt.Fprintf(&b, "Request %s to %s: %s replied (%s).\n", r.Request.ID, r.Request.To, from, status)
	fmt.Fprintf(&b, "You asked: %s\n", truncate(r.Request.Body, 300))
	b.WriteString(formatParent(r.Parent))
	b.WriteString("Finish the work that was waiting on this reply.\n")
	b.WriteString("---\n")
	b.WriteString(body)
	b.WriteString("\n---\n")
	b.WriteString(FormatAttachments(atts))
	b.WriteString(formatExchanges(r.Exchanges))
	return b.String()
}

func formatExchanges(exchanges []envelope.Exchange) string {
	var b strings.Builder
	for i, ex := range exchanges {
		fmt.Fprintf(&b, "Clarification %d:\nQuestion: %s\n", i+1, ex.Question)
		if ex.Answer != "" {
			fmt.Fprintf(&b, "Answer: %s\n", ex.Answer)
		}
	}
	return b.String()
}

// FormatAttachments lists a message's attachments by id, with the sender's
// display name, type and size, and how to fetch one. It is empty when there
// are none.
func FormatAttachments(atts []envelope.Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Attachments (%d), fetch one with `tincan attachment get <id>`:\n", len(atts))
	for _, a := range atts {
		fmt.Fprintf(&b, "  %s %q (%s, %d bytes)\n", a.ID, a.Name, a.MIME, a.Size)
	}
	return b.String()
}

// parentOpen reports whether a parent request still expects a reply.
func parentOpen(s envelope.Status) bool {
	return s == envelope.StatusQueued || s == envelope.StatusDelivered || s == envelope.StatusClaimed
}

// Claimer claims a delivered request for this agent.
type Claimer interface {
	Claim(ctx context.Context, id string) (envelope.Request, error)
}

// FormatInbox renders what a poll picked up: replies to this agent's own
// requests first, under RepliesHeading, then the new requests, each claimed
// through c so no one else handles it. The caller acknowledges the replies
// (AckReplies) once the text has reached its agent.
func FormatInbox(ctx context.Context, c Claimer, in Inbox) string {
	var b strings.Builder
	if in.Empty() {
		b.WriteString("No requests waiting.\n")
	}
	if len(in.Replies) > 0 {
		b.WriteString(RepliesHeading)
		for _, r := range in.Replies {
			b.WriteString(FormatReply(r))
		}
		if in.RepliesRemaining > 0 {
			fmt.Fprintf(&b, "%d more %s waiting. Run check_inbox (or `tincan inbox`) again to read %s.\n",
				in.RepliesRemaining, plural(in.RepliesRemaining, "reply is", "replies are"), plural(in.RepliesRemaining, "it", "them"))
		}
		if len(in.Requests) > 0 {
			b.WriteString("\nRequests from teammates:\n")
		}
	}
	for _, req := range in.Requests {
		if req.Kind == envelope.KindPing {
			continue
		}
		if _, err := c.Claim(ctx, req.ID); err != nil {
			fmt.Fprintf(&b, "(could not claim %s: %v)\n", req.ID, err)
			continue
		}
		b.WriteString(FormatRequest(req))
	}
	b.WriteString(UpgradeNotice(in.UpgradeAvailable))
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// truncate shortens s to at most n bytes on a rune boundary, on one line.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func formatParent(p *envelope.Parent) string {
	var b strings.Builder
	if p != nil {
		fmt.Fprintf(&b, "This answers the question you asked while handling request %s from %s: %s.", p.ID, p.From, truncate(p.Body, 300))
		if p.Status == envelope.StatusNeedsInput {
			fmt.Fprintf(&b, " That request is waiting for its sender to answer your clarifying question, so it cannot take a reply yet. Carry on once the answer arrives and the request comes back to you.\n")
		} else if parentOpen(p.Status) {
			fmt.Fprintf(&b, " That request is still open (status %s). When you have what you need, reply to it with `tincan reply %s \"...\"` (or the reply tool).\n", p.Status, p.ID)
		} else {
			fmt.Fprintf(&b, " That request is already closed (status %s), so there is nothing left to reply to.\n", p.Status)
		}
	}
	return b.String()
}

// FormatGroup labels every result and supplies the shared follow-up id.
func FormatGroup(g GroupResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Group %s (%s). Check with get_reply or tincan get %s.\n", g.Group, g.Outcome, g.Group)
	for _, r := range g.Results {
		fmt.Fprintf(&b, "%s (%s), request %s:\n%s", r.Request.To, r.Status, r.Request.ID, FormatResult(r.Result))
		if r.Error != "" {
			fmt.Fprintf(&b, "Poll error: %s\n", r.Error)
		}
	}
	return b.String()
}

// SignedOutField is the optional, quoted roster authentication fact.
func (a AgentInfo) SignedOutField(now time.Time) string {
	if a.SignedOutSite == "" {
		return ""
	}
	age := max(time.Duration(0), now.Sub(a.SignedOutSince))
	return "signed_out=" + strconv.Quote(a.SignedOutSite+" since "+strings.TrimSuffix(ageAgo(age), " ago"))
}

// SignedOutHint explains recovery without changing queue or hold semantics.
func SignedOutHint(to string, t *envelope.Target) string {
	if t == nil || t.SignedOutSite == "" {
		return ""
	}
	host := t.WebHost
	if host == "" {
		host = "the agent's registered machine"
	}
	action := "Sign in to " + t.SignedOutSite + " in Chrome on " + host + " without restarting Chrome."
	if t.SignedOutSite == "copilot.com" {
		action += " Use a personal Microsoft account and finish any sign-in or terms prompt."
	}
	return fmt.Sprintf("%s is known to be signed out of %s. %s This warning does not cancel work or prove nothing was sent.\n", to, t.SignedOutSite, action)
}
