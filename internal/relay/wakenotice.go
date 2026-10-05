package relay

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// MaxNoticeTeammates caps the online teammates one wake notice names.
const MaxNoticeTeammates = 5

// TellAskers is the waker's Unanswered hook: agent, which the relay wakes,
// was woken at wk, and a follow-up found it still silent with requests
// queued. Each queued ask that has waited a full grace since the later of
// its creation and the wake (UrgentWakeGrace when urgent, WakeGrace
// otherwise) gets a note, once per request: it is kept on the request,
// where get_reply shows it to the asker, and sent to the asker as a notice
// from the relay, which check_inbox shows and which wakes a webhook or
// email asker. The request keeps its status and its target; the asker
// decides whether to cancel it and ask someone else. The note names the
// teammates online now, other than the asker and agent.
func (s *Server) TellAskers(agent string, wk store.Wake) {
	if s.wake == nil || !relayWoken(s.wake.WakeMethod(agent)) {
		return
	}
	if last := s.lastPollSince(agent, wk.At); !last.IsZero() && !last.Before(wk.At) {
		return // it checked in after all
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if a, found, err := s.dir.Agent(ctx, agent); err != nil || !found || wk.At.Before(a.JoinedAt) {
		return // gone, or the wake went to an earlier agent of the same name
	}
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
		note := s.wakeNote(agent, wk, s.onlineTeammates(roster, now, req.From, agent), now)
		added, err := s.store.AddWakeNotice(ctx, req.ID, note, now)
		if err != nil {
			log.Printf("wake notice %s: %v", req.ID, err)
			continue
		}
		if !added {
			continue // another follow-up got there first
		}
		s.record(ctx, "wake_notice", req.ID, req.TraceID, "relay", store.DetailJSON(map[string]any{"to": req.From, "agent": agent}))
		s.noticeAsker(ctx, req, note)
	}
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
	result := method + " ok"
	if wk.Result != envelope.WakeOK {
		result = method + " failed: " + wk.Result
	}
	note := fmt.Sprintf("%s was woken at %s and has not checked in (%s). The request is still queued.", agent, utcClock(wk.At, now), result)
	if len(online) == 0 {
		return note + " If it can't wait, cancel it and ask another teammate whose good_at fits; none is online right now, and list_agents shows how each one wakes."
	}
	return note + " If it can't wait, cancel it and ask a teammate who is online and good at this: " + strings.Join(online, ", ") + "."
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
// session knows which request it is about.
func (s *Server) noticeAsker(ctx context.Context, req envelope.Request, note string) {
	body := fmt.Sprintf("About your request %s to %s (you asked: %s): %s No reply needed.",
		req.ID, req.To, strings.Join(strings.Fields(approvalPreview(req.Body)), " "), note)
	n := envelope.Request{From: "relay", To: req.From, Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: body}
	n, err := s.store.Enqueue(ctx, n, s.requestTTL(ctx, n.To))
	if err != nil {
		log.Printf("wake notice %s: tell %s: %v", req.ID, req.From, err)
		s.record(ctx, "wake_notice_failed", req.ID, req.TraceID, "relay", "")
		return
	}
	s.record(ctx, "queued", n.ID, n.TraceID, "relay", store.DetailJSON(map[string]any{"wake_notice_for": req.ID, "to": n.To}))
	s.hub.notify(inboxKey(n.To))
	if s.events != nil {
		s.events.Queued(ctx, n)
	}
}

// utcClock is t as "16:40 UTC", with the date when it is not now's day:
// "Oct 2 16:40 UTC". The relay cannot know the reader's zone.
func utcClock(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04 UTC")
	}
	return t.Format("Jan 2 15:04 UTC")
}
