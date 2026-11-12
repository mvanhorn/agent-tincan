package relay

// Fallback: the owner can name, per agent, another agent that takes an ask
// the first one leaves untaken (tincan fallback set claude-code
// claude-window). The sweep's untaken check (tellUntaken) moves such an ask
// once, keeping its id, sender and chain, instead of only telling the asker
// it waits. A move that any rule refuses leaves the ask where it is with
// the plain untaken note. See docs/plans/2026-11-12-0100-feat-untaken-fallback-plan.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// noFallbackKinds are kinds that cannot be a fallback: a phone never
// answers, a council, dot-web and duet-web wait for the owner's approval on
// every ask, and the history, notes and pass services parse a fixed form an
// ask meant for another agent would not fit.
var noFallbackKinds = map[string]bool{
	onboard.KindPhone: true, onboard.KindCouncil: true, onboard.KindDotWeb: true, onboard.KindDuetWeb: true,
	onboard.KindHistory: true, onboard.KindNotes: true, onboard.KindPass: true,
}

// handleSetFallback records the agent an untaken ask to {name} moves to
// (admin only): {"fallback": "claude-window"}, or "" to clear it. A body
// without fallback is refused rather than read as a clear.
func (s *Server) handleSetFallback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Fallback *string `json:"fallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Fallback == nil {
		writeErr(w, http.StatusBadRequest, errors.New(`fallback is required; send "" to clear it`))
		return
	}
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	name, to := r.PathValue("name"), *in.Fallback
	if to != "" {
		b, ok, err := s.dir.Agent(r.Context(), to)
		switch {
		case err != nil:
			writeErr(w, http.StatusInternalServerError, err)
			return
		case ok && noFallbackKinds[b.Kind]:
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%s is a %s agent and cannot be a fallback", to, b.Kind))
			return
		}
	}
	if err := s.dir.SetFallback(r.Context(), s.remote(r), name, to); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "fallback", "", "", name, store.DetailJSON(map[string]any{"fallback": to}))
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "fallback": to})
}

// rerouteRefusal says why req, an untaken ask to its target, may not move
// to the target's fallback to, or "" when it may. roster is the joined
// agents. It runs the policy's move checks (loop, hop, approval.json and
// kind holds) last, since they read the store.
func (s *Server) rerouteRefusal(ctx context.Context, req envelope.Request, roster []identity.Agent, to string, now time.Time) string {
	i := slices.IndexFunc(roster, func(a identity.Agent) bool { return a.Name == to })
	if i < 0 {
		return "the fallback is not a joined agent"
	}
	b := roster[i]
	switch {
	case to == req.To:
		return "the fallback is the target"
	case to == req.From:
		return "the fallback is the asker"
	case b.Guest.Set() || noFallbackKinds[b.Kind]:
		return "the fallback cannot take asks moved to it"
	case len(req.MovedFrom) > 0:
		return "it moved once already"
	case req.Group != "":
		return "it is part of a group"
	case req.RoomPostID != 0:
		return "it came from a room mention"
	}
	s.mu.Lock()
	online := s.polledRecently(s.lastPoll[to], now)
	s.mu.Unlock()
	if !online {
		return "the fallback is offline"
	}
	if why := guestRerouteRefusal(req, roster, to); why != "" {
		return why
	}
	if err := s.checkMove(ctx, req, to); err != nil {
		return "the move checks refused it: " + err.Error()
	}
	return ""
}

// guestRerouteRefusal applies the guest hold a fresh send to to would get.
// A guest's own ask moves only to a teammate its list names, and only for
// a guest invited with --no-hold: for any other guest the owner approved
// the ask for its first target, not for to. A teammate's ask marked as
// coming from a guest's work moves only when it is guest_approved.
func guestRerouteRefusal(req envelope.Request, roster []identity.Agent, to string) string {
	if !req.FromGuest {
		return ""
	}
	i := slices.IndexFunc(roster, func(a identity.Agent) bool { return a.Name == req.From })
	if i >= 0 && roster[i].Guest.Set() {
		g := roster[i].Guest
		switch {
		case !slices.Contains(g.Asks, to):
			return "the guest may not ask the fallback"
		case !g.NoHold:
			return "the owner approved this guest's ask for its first target only"
		}
		return ""
	}
	if !req.GuestApproved {
		return "it comes from a guest's ask the owner has not approved"
	}
	return ""
}

// rerouteUntaken moves req, an ask its target left untaken past the
// notice grace, to the target's fallback to. done reports that the caller
// has nothing left to do: the ask moved, or it changed under the sweep
// (claimed, answered or cancelled since it was read) and needs no note.
// When a rule refuses the move, or the write fails, nothing is written and
// why says what stopped it; the caller gives the plain untaken note.
func (s *Server) rerouteUntaken(ctx context.Context, req envelope.Request, roster []identity.Agent, to string, now time.Time) (done bool, why string) {
	if why := s.rerouteRefusal(ctx, req, roster, to, now); why != "" {
		return false, why
	}
	from := req.To
	since, err := s.store.UntakenSince(ctx, req.ID)
	if err != nil {
		log.Printf("fallback %s: %v", req.ID, err)
		return false, "the move failed"
	}
	out, err := s.store.RerouteUntaken(ctx, req.ID, from, to)
	if errors.Is(err, store.ErrWrongState) {
		// Taken, answered or cancelled since the read: nothing to tell. One
		// still queued for from that the write's conditions refused gets
		// the note, so it is not tried again on every sweep.
		if cur, st, err := s.store.Request(ctx, req.ID); err == nil && st == envelope.StatusQueued && cur.To == from {
			return false, "the request changed"
		}
		return true, ""
	}
	if err != nil {
		log.Printf("fallback %s: %v", req.ID, err)
		return false, "the move failed"
	}
	s.record(ctx, "rerouted", out.ID, out.TraceID, "relay", store.DetailJSON(map[string]any{"from": from, "to": to, "reason": "untaken"}))
	note := fmt.Sprintf("%s had not taken this request in %s, so the relay moved it to %s, the teammate the owner set to take what %s leaves untaken. Nothing had worked on it before the move. It keeps its id, and %s's reply comes back on this request. Do not send it again.",
		from, ageText(now.Sub(since)), to, from, to)
	if guestAsker(roster, out.From) {
		// How the owner routes a teammate's asks is not a guest's to read.
		note = fmt.Sprintf("%s had not picked this up, so the relay passed it to %s. It keeps its id, and %s's reply comes back on this request. Do not send it again.", from, to, to)
	}
	s.noteAndTell(ctx, out, store.NoteRerouted, 0, note, now)
	s.hub.notify(requestKey(out.ID))
	s.hub.notify(inboxKey(to))
	if s.events != nil {
		s.events.Queued(ctx, out)
	}
	return true, ""
}
