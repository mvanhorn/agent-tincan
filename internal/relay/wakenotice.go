package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// MaxNoticeTeammates caps the online teammates one wake notice names.
const MaxNoticeTeammates = 5

// Notes for askers. The relay tells an asker, once, when its ask is stuck:
// the target was woken and never checked in (TellAskers), a target that
// wakes itself left it untaken (tellUntaken), the target's claim went
// stale, or the request expired with no reply (tellSwept). Each
// note is kept on the request, where get_reply shows it to the asker, and
// sent to the asker as a notice from the relay, which check_inbox shows and
// which wakes a webhook or email asker. No note changes the request's
// status or target.

// TellAskers is the waker's Unanswered hook: agent, which the relay wakes,
// was woken at wk, and a follow-up found it still silent with requests
// queued. Each queued ask that has waited a full grace since the later of
// its creation and the wake (UrgentWakeGrace when urgent, WakeGrace
// otherwise) gets a note, once per request, naming the teammates online
// now other than the asker and agent.
func (s *Server) TellAskers(agent string, wk store.Wake) {
	if s.wake == nil || !relayWoken(s.wake.WakeMethod(agent)) {
		return
	}
	if last := s.lastPollSince(agent, wk.At); !last.IsZero() && !last.Before(wk.At) {
		s.mu.Lock()
		delete(s.silent, agent) // it checked in after all
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if a, found, err := s.dir.Agent(ctx, agent); err != nil || !found || wk.At.Before(a.JoinedAt) {
		return // gone, or the wake went to an earlier agent of the same name
	}
	s.tellOwner(ctx, agent, wk)
	reqs, err := s.store.UnnoticedAsks(ctx, agent)
	if err != nil {
		log.Printf("wake notice for %s: %v", agent, err)
		return
	}
	now := s.cfg.Now()
	var roster []identity.Agent
	for _, req := range reqs {
		since := req.CreatedAt
		if wk.At.After(since) {
			since = wk.At
		}
		if now.Sub(since) < s.noticeGrace(req.Urgent) || req.From == agent || !s.isAgent(ctx, req.From) {
			continue
		}
		if roster == nil {
			if roster, err = s.dir.Agents(ctx); err != nil {
				log.Printf("wake notice for %s: roster: %v", agent, err)
				return
			}
		}
		online := s.onlineTeammates(noticeRoster(roster, req.From), now, req.From, agent)
		var note string
		if guestAsker(roster, req.From) {
			// How a teammate wakes and how that went are not a guest's to read.
			note = fmt.Sprintf("%s has not picked this up yet. The request is still queued.", agent) + suggest(online, "If it can't wait, cancel it and ask")
		} else {
			note = s.wakeNote(agent, wk, online, now)
		}
		s.noteAndTell(ctx, req, store.NoteWake, 0, note, now)
	}
}

// agentSideWake reports a wake method the relay does not drive and has no
// overdue check for: channel, wait, command, none and the like. The relay
// cannot tell whether such an agent is looking at its queue, so an ask it
// leaves untaken is the asker's to hear about.
func agentSideWake(method string) bool {
	return !relayWoken(method) && method != wake.Schedule
}

// tellUntaken is the sweep's check on agents that wake themselves. Each
// live queued ask to one of them that nothing has claimed, past the notice
// grace (UrgentWakeGrace when urgent, WakeGrace otherwise), moves to the
// owner's fallback for that agent when one is set and the move passes
// rerouteUntaken's checks, and otherwise gets a note for its asker, once
// per request. An ask TellAskers already noted is left
// alone, and so are held asks, self-asks, asks that were claimed once (the
// stale-claim note covers those) and asks from anyone not on the roster.
func (s *Server) tellUntaken(ctx context.Context) {
	if s.wake == nil {
		return
	}
	roster, err := s.dir.Agents(ctx)
	if err != nil {
		log.Printf("untaken notice: roster: %v", err)
		return
	}
	var agents []string
	fallbacks := map[string]string{}
	for _, a := range roster {
		if agentSideWake(s.wake.WakeMethod(a.Name)) {
			agents = append(agents, a.Name)
			fallbacks[a.Name] = a.Fallback
		}
	}
	now := s.cfg.Now()
	reqs, err := s.store.UntakenAsks(ctx, agents, now.Add(-s.noticeGrace(false)), now.Add(-s.noticeGrace(true)))
	if err != nil {
		log.Printf("untaken notice: %v", err)
		return
	}
	for _, req := range reqs {
		if !slices.ContainsFunc(roster, func(a identity.Agent) bool { return a.Name == req.From }) {
			continue // the relay's own notices, or an asker since removed
		}
		if fb := fallbacks[req.To]; fb != "" {
			done, why := s.rerouteUntaken(ctx, req, roster, fb, now)
			if done {
				continue
			}
			s.record(ctx, "reroute_skipped", req.ID, req.TraceID, "relay", store.DetailJSON(map[string]any{"from": req.To, "to": fb, "reason": why}))
		}
		online := s.onlineTeammates(noticeRoster(roster, req.From), now, req.From, req.To)
		var note string
		if guestAsker(roster, req.From) {
			// How a teammate wakes is not a guest's to read.
			note = fmt.Sprintf("%s has not picked this up yet. The request is still queued.", req.To) + suggest(online, "If it can't wait, cancel it and ask")
		} else {
			note = untakenNote(req.To, s.wake.WakeMethod(req.To), online)
		}
		s.noteAndTell(ctx, req, store.NoteUntaken, 0, note, now)
	}
}

// untakenNote is the text an asker gets when agent, which wakes itself by
// method, has left its ask untaken. It says that nothing is working on the
// request, because the roster can show the agent busy or online for other
// reasons: another session's turn, or a channel loop that answers pings.
func untakenNote(agent, method string, online []string) string {
	note := fmt.Sprintf("%s has not taken this request. It is still queued and nothing is working on it yet; %s showing as busy or online does not mean it is working on this request.", agent, agent)
	if method == wake.Channel {
		note += " " + agent + " wakes by channel, and a Claude Code session sees a channel notice only while it is idle and was started with channels."
	}
	return note + suggest(online, "If it can't wait, cancel it and ask")
}

// silentEpisode is one relay-woken agent's current run of unanswered
// wakes.
type silentEpisode struct {
	at    int64 // the episode's wake time, Unix ms, fixed while it is silent
	wakes int   // wakes it has left unanswered
	told  bool  // the owner's notice is queued
}

// DefaultOwnerNoticeAfter is how many wakes in a row a relay-woken agent
// may leave unanswered before the owner is told.
const DefaultOwnerNoticeAfter = 3

// tellOwner counts one more unanswered wake in agent's silent episode,
// which wk's time names, and when the count reaches OwnerNoticeAfter tells
// the owner once: a notify from the relay to the approval policy's notify
// destination. A stored record keyed by agent and episode keeps it to once
// across follow-ups and restarts.
func (s *Server) tellOwner(ctx context.Context, agent string, wk store.Wake) {
	episode := wk.At.UnixMilli()
	s.mu.Lock()
	e := s.silent[agent]
	if e.at != episode {
		e = silentEpisode{at: episode}
	}
	e.wakes++
	s.silent[agent] = e
	s.mu.Unlock()
	// Every follow-up past the threshold tries again, so a notice that
	// could not be queued is retried; the stored notice keeps it to one.
	if e.wakes < s.cfg.OwnerNoticeAfter || e.told {
		return
	}
	to := s.notifyDestination()
	if to == "" {
		if e.wakes > s.cfg.OwnerNoticeAfter {
			return // said once already
		}
		log.Printf("owner wake notice for %s: %d wakes unanswered, but no notify destination is configured in the approval policy", agent, e.wakes)
		return
	}
	anchor, found, err := s.store.OldestQueued(ctx, agent)
	if err != nil {
		log.Printf("owner wake notice for %s: %v", agent, err)
		return
	}
	if !found {
		return
	}
	note := s.ownerWakeNote(ctx, agent, wk, e.wakes, s.cfg.Now())
	n := envelope.Request{From: "relay", To: to, Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: note}
	n, added, err := s.store.EnqueueOwnerNotice(ctx, agent, episode, n, s.requestTTL(ctx, to))
	if err != nil {
		log.Printf("owner wake notice for %s: tell %s: %v", agent, to, err)
		s.record(ctx, "owner_wake_notice_failed", anchor.ID, anchor.TraceID, "relay", store.DetailJSON(map[string]any{"agent": agent, "to": to}))
		return // nothing was recorded, so the next follow-up tries again
	}
	if !added {
		s.markTold(agent, episode) // sent before, by this relay or one before a restart
		return
	}
	s.markTold(agent, episode)
	s.record(ctx, "queued", n.ID, n.TraceID, "relay", store.DetailJSON(map[string]any{"owner_wake_notice_for": agent, "to": to}))
	s.noticeQueued(ctx, n)
}

// markTold records that the owner has been told about agent's episode, so
// later follow-ups in it skip the store.
func (s *Server) markTold(agent string, episode int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.silent[agent]; cur.at == episode {
		cur.told = true
		s.silent[agent] = cur
	}
}

// ownerWakeNote is the text the owner gets when agent has left that many
// wakes unanswered in a row. It names the wake method only, the stored
// result, and, when the last wake sent was answered 2xx, the webhook's reply
// as the waker kept it (secrets redacted, or the whole reply withheld); none
// of these carries a URL, token or key.
func (s *Server) ownerWakeNote(ctx context.Context, agent string, wk store.Wake, wakes int, now time.Time) string {
	reply := ""
	if e, found, err := s.store.LastAuditFor(ctx, agent, "woke", "wake_failed"); err == nil && found && e.Event == "woke" {
		if r := wakeEntry(e, "").Reply; r != ReplyNotRecorded {
			reply = "; last webhook reply: " + r
		}
	}
	return fmt.Sprintf("%s has not checked in after %d wakes in a row since %s (wake path: %s; last wake result: %s%s). Its requests are still queued. Run tincan wakes %s for its wake history. No reply needed.",
		agent, wakes, client.UTCClock(wk.At, now), s.wake.WakeMethod(agent), wk.Result, reply, agent)
}

// noteAndTell records note on req for kind and episode and, the first time
// only, sends it to req's asker. A note that is already there (a second
// follow-up, a restarted relay) is not sent again.
func (s *Server) noteAndTell(ctx context.Context, req envelope.Request, kind string, episode int64, note string, now time.Time) {
	added, err := s.store.AddRelayNote(ctx, req.ID, kind, episode, note, now)
	if err != nil {
		log.Printf("relay note %s %s: %v", kind, req.ID, err)
		return
	}
	if !added {
		return
	}
	s.record(ctx, "relay_note", req.ID, req.TraceID, "relay", store.DetailJSON(map[string]any{"kind": kind, "to": req.From, "agent": req.To}))
	s.noticeAsker(ctx, req, kind, note, now)
}

// tellSwept tells askers about what a sweep did to their asks: a claim
// whose lease ran out with no reply or progress (requeued), and a request
// that expired with no reply. Pings, notifies, held requests and the
// relay's own notices are left alone.
func (s *Server) tellSwept(ctx context.Context, t store.Transition) {
	if t.Held || t.Kind != envelope.KindAsk || !s.isAgent(ctx, t.From) {
		return
	}
	staleClaim := t.Status == envelope.StatusQueued && t.Prev == envelope.StatusClaimed
	if !staleClaim && t.Status != envelope.StatusExpired {
		return
	}
	req, status, err := s.store.Request(ctx, t.ID)
	if err != nil {
		log.Printf("relay note %s: %v", t.ID, err)
		return
	}
	if staleClaim && requestClosed(status) {
		return // a reply or cancel landed after the requeue; the lapse is old news
	}
	now := s.cfg.Now()
	roster, err := s.dir.Agents(ctx)
	if err != nil {
		log.Printf("relay note %s: roster: %v", t.ID, err)
	}
	online := s.onlineTeammates(noticeRoster(roster, req.From), now, req.From, req.To)
	if staleClaim {
		note := fmt.Sprintf("%s claimed this %s ago and did not reply or post progress before its claim lease ran out; it has been requeued", req.To, ageText(now.Sub(t.ClaimedAt)))
		if s.wake != nil && relayWoken(s.wake.WakeMethod(req.To)) {
			note += " and woken again"
		}
		note += "." + suggest(online, "If it can't wait, cancel it and ask")
		s.noteAndTell(ctx, req, store.NoteStaleClaim, t.ClaimedAt.UnixMilli(), note, now)
		return
	}
	var held string
	switch t.Prev {
	case envelope.StatusNeedsInput:
		held = fmt.Sprintf("It was waiting for your answer to %s's question.", req.To)
	case envelope.StatusClaimed:
		held = fmt.Sprintf("%s claimed it %s ago and never replied.", req.To, ageText(now.Sub(t.ClaimedAt)))
	case envelope.StatusDelivered:
		held = fmt.Sprintf("%s received it but never claimed it.", req.To)
	default:
		held = fmt.Sprintf("%s never picked it up.", req.To)
	}
	note := fmt.Sprintf("This request expired after %s with no reply. %s", ageText(now.Sub(req.CreatedAt)), held) + suggest(online, "If you still need it, ask again or ask")
	s.noteAndTell(ctx, req, store.NoteExpired, 0, note, now)
}

// requestClosed reports a request the asker needs no stale-claim notice
// for: it was answered, declined, failed or cancelled.
func requestClosed(st envelope.Status) bool {
	return st.Terminal() || st == envelope.StatusCancelled
}

// recordSuperseded audits each of the relay's queued notices about request
// id that its reply or cancel (why) closed without delivery.
func (s *Server) recordSuperseded(ctx context.Context, id, why string, closed []string) {
	for _, n := range closed {
		s.record(ctx, "notice_superseded", n, "", "relay", store.DetailJSON(map[string]any{"notice_for": id, "by": why}))
	}
}

// suggest is " <lead> a teammate who is online and good at this: a, b." or,
// with no one online, a pointer to list_agents.
func suggest(online []string, lead string) string {
	if len(online) == 0 {
		return " " + lead + " another teammate whose good_at fits; none is online right now, and list_agents shows how each one wakes."
	}
	return " " + lead + " a teammate who is online and good at this: " + strings.Join(online, ", ") + "."
}

// ageText is a short duration for a note: "under a minute", "12m", "2h5m".
func ageText(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	if m := int(d%time.Hour) / int(time.Minute); m != 0 {
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), m)
	}
	return fmt.Sprintf("%dh", int(d/time.Hour))
}

// noticeGrace is how long a queued ask waits in silence before its asker is
// told: UrgentWakeGrace for an urgent one when that is shorter, WakeGrace
// otherwise. It matches the waker's follow-up grace.
func (s *Server) noticeGrace(urgent bool) time.Duration {
	if urgent && s.cfg.UrgentWakeGrace > 0 && s.cfg.UrgentWakeGrace < s.cfg.WakeGrace {
		return s.cfg.UrgentWakeGrace
	}
	return s.cfg.WakeGrace
}

// wakeNote is the text an asker gets about agent's silence.
func (s *Server) wakeNote(agent string, wk store.Wake, online []string, now time.Time) string {
	method := s.wake.WakeMethod(agent)
	result := method + " " + wk.Result // "webhook ok", or with a fallback marker
	if !envelope.WakeAccepted(wk.Result) {
		result = method + " failed: " + wk.Result
	}
	note := fmt.Sprintf("%s was woken at %s and has not checked in (%s). The request is still queued.", agent, client.UTCClock(wk.At, now), result)
	return note + suggest(online, "If it can't wait, cancel it and ask")
}

// onlineTeammates names up to MaxNoticeTeammates agents on the roster that
// polled recently, in roster order, leaving out the names in skip.
func (s *Server) onlineTeammates(roster []identity.Agent, now time.Time, skip ...string) []string {
	var out []string
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range roster {
		if len(out) == MaxNoticeTeammates {
			break
		}
		if !slices.Contains(skip, a.Name) && s.polledRecently(s.lastPoll[a.Name], now) {
			out = append(out, a.Name)
		}
	}
	return out
}

// polledRecently reports a poll recent enough that the agent counts as
// online: within one poll hold plus slack. Caller may hold s.mu.
func (s *Server) polledRecently(last, now time.Time) bool {
	return !last.IsZero() && now.Sub(last) < s.cfg.PollHold+30*time.Second
}

// noticeAsker sends req's asker note as a notify from the relay, the way an
// approval notice goes out: check_inbox shows it, a held poll returns it,
// and the waker wakes a webhook or email asker for it. It carries the
// asker's own request id and a preview of what they asked, so a fresh
// session knows which request it is about. A stale-claim notice is linked
// to req, so a reply or cancel that closes req first closes it unsent.
func (s *Server) noticeAsker(ctx context.Context, req envelope.Request, kind, note string, now time.Time) {
	n := askerNotice(req, note, now)
	if kind == store.NoteStaleClaim {
		n.NoticeFor, n.NoticeKind = req.ID, kind
	}
	n, err := s.store.Enqueue(ctx, n, s.requestTTL(ctx, n.To))
	if errors.Is(err, store.ErrNoticeSuperseded) {
		log.Printf("wake notice %s: not sent to %s, the request closed first", req.ID, req.From)
		return
	}
	if err != nil {
		log.Printf("wake notice %s: tell %s: %v", req.ID, req.From, err)
		s.record(ctx, "wake_notice_failed", req.ID, req.TraceID, "relay", "")
		return
	}
	s.record(ctx, "queued", n.ID, n.TraceID, "relay", store.DetailJSON(map[string]any{"wake_notice_for": req.ID, "to": n.To}))
	s.noticeQueued(ctx, n)
}

// askerNotice is the notify noticeAsker sends. It says when the relay made
// it, so an asker reading it late can tell it from news.
func askerNotice(req envelope.Request, note string, now time.Time) envelope.Request {
	body := fmt.Sprintf("About your request %s to %s (you asked: %s): %s Sent by the relay at %s. No reply needed.",
		req.ID, req.To, strings.Join(strings.Fields(approvalPreview(req.Body)), " "), note, client.UTCClock(now, now))
	return envelope.Request{From: "relay", To: req.From, Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: body}
}
