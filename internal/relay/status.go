package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// staleQueue is how long a request may wait in an agent's queue, untaken,
// before the agent counts as stuck even while it is online.
const staleQueue = time.Hour

// statusFacts are the facts about one agent that its roster status is
// computed from, all of which handleAgents already gathers.
type statusFacts struct {
	online bool
	// wake is the agent's wake method name.
	wake   string
	stat   store.QueueStat
	target envelope.Target
	// hostBusy is set when the agent's host reports it is working, such as
	// a Claude Code session in the middle of a turn.
	hostBusy bool
	// untaken is the agent's queued asks it never took, timed from when
	// each last entered the queue (sent, approved or answered).
	untaken store.UntakenStat
	// untakenAfter is the notice grace: how long such an ask may wait on an
	// agent that wakes itself, with no live claim, before the agent counts
	// as stuck. Zero turns the rule off.
	untakenAfter time.Duration
}

// agentStatus is one agent's roster status: the word, why a stuck agent is
// stuck, and the asker and start of the claim a busy agent holds.
type agentStatus struct {
	status    string
	reason    string
	busyFor   string
	busySince time.Time
}

// computeStatus is the one place an agent's status is decided, so every
// surface that shows the roster agrees. Stuck means work waits on the
// agent and nothing is moving, and wins over busy so it is seen; it is
// never reported before quietUntil, the end of the first lease window
// after the relay started, since the agent may not have called this relay
// yet. A polling web agent whose site is out of messages is limited until
// the reset, ahead of everything else, since nothing it is sent goes out
// before then. Busy means a live claim or the host saying so; the rest is
// idle or offline by whether the agent is polling.
func computeStatus(f statusFacts, now, quietUntil time.Time) agentStatus {
	if f.online && f.target.LimitedUntil.After(now) {
		return agentStatus{status: client.StatusLimited, reason: "until " + client.UTCClock(f.target.LimitedUntil, now)}
	}
	if !now.Before(quietUntil) {
		if reason := stuckReason(f, now); reason != "" {
			return agentStatus{status: client.StatusStuck, reason: reason}
		}
	}
	switch {
	case f.stat.Claimed > 0:
		return agentStatus{status: client.StatusBusy, busyFor: f.stat.ClaimAsker, busySince: f.stat.OldestClaim}
	case f.hostBusy:
		return agentStatus{status: client.StatusBusy}
	case f.online:
		return agentStatus{status: client.StatusIdle}
	}
	return agentStatus{status: client.StatusOffline}
}

// stuckReason says why work waiting on the agent is not moving, or "" when
// it is. These are the rules tincan top used to apply on the client
// (QUEUED-OFFLINE, STALE) plus the relay's own wake and lease facts.
func stuckReason(f statusFacts, now time.Time) string {
	switch {
	case f.stat.Lapsed > 0:
		return "claim lapsed"
	case f.target.Unanswered && !f.target.WokenAt.IsZero():
		return "woken, silent " + client.QueueAge(now.Sub(f.target.WokenAt))
	case f.target.Overdue:
		return "overdue check"
	}
	// The relay wakes webhook and email agents, and a schedule agent that
	// keeps its checks will find its queue, so only an agent-side wake
	// that is offline has lost the process that would notice it.
	relayWakes := relayWoken(f.wake) || f.wake == wake.Schedule
	if !f.online && f.stat.Queued > 0 && !relayWakes {
		return fmt.Sprintf("%d queued while offline", f.stat.Queued)
	}
	// An agent that wakes itself can look online, and its host can report
	// busy, while no session ever looks at its queue: a Claude Code channel
	// reaches only an idle session started with channels. With no claim
	// held, a queue older than the notice grace is not moving, whatever the
	// host says.
	if !relayWakes && f.untakenAfter > 0 && f.stat.Claimed == 0 && f.untaken.Count > 0 &&
		!f.untaken.Oldest.IsZero() && now.Sub(f.untaken.Oldest) > f.untakenAfter {
		return fmt.Sprintf("%d queued, untaken %s", f.untaken.Count, client.QueueAge(now.Sub(f.untaken.Oldest)))
	}
	if f.stat.Queued > 0 && !f.stat.OldestQueued.IsZero() && now.Sub(f.stat.OldestQueued) > staleQueue {
		return "oldest queued " + client.QueueAge(now.Sub(f.stat.OldestQueued))
	}
	return ""
}

// hostStatusEntry is one session's host-reported state and when it was
// last reported.
type hostStatusEntry struct {
	busy bool
	at   time.Time
}

// handleHostStatus records whether the calling session's host says it is
// working, such as a Claude Code session in the middle of a turn (tincan
// mcp forwards the session's own registry state). The agent is the
// authenticated caller and the entry is filed under the session header, so
// no caller can set another agent's state. Entries live in memory and
// lapse after SessionGrace without a refresh, so the client re-sends its
// state on every keepalive and a relay restart costs at most one interval.
func (s *Server) handleHostStatus(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	sess, err := session(r)
	if err == nil && sess == "" {
		err = errors.New("a host status needs a session header")
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The binding that passed authentication, read before the body, which
	// a slow client can hold open across a removal and a rejoin.
	bound, found, err := s.dir.Agent(r.Context(), name)
	if err == nil && !found {
		err = identity.ErrUnknownAgent
	}
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	var in struct {
		Busy bool `json:"busy"`
	}
	if err := decodeOne(r.Body, &in, "host status object"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.setHostStatus(r.Context(), bound, sess, in.Busy, s.cfg.Now())
	w.WriteHeader(http.StatusNoContent)
}

// setHostStatus files the session's host report for bound, the binding
// that passed authentication, unless bound no longer holds its name: the
// agent was removed, or removed and joined again (a new JoinedAt or node).
// An admin removal can land after the caller was authenticated: the
// removal unbinds the agent and then, under s.mu, drops its host status.
// So the report is written under s.mu first and the binding checked
// after. Either the check runs before the unbind, and the removal's own
// cleanup, later under s.mu, drops the entry, or it sees the binding gone
// and the entry is dropped here. A busy report never outlives its binding
// until SessionGrace lapses, where a later agent joined under the name
// would show it.
func (s *Server) setHostStatus(ctx context.Context, bound identity.Agent, sess string, busy bool, now time.Time) {
	agent := bound.Name
	e := hostStatusEntry{busy: busy, at: now}
	s.mu.Lock()
	m := s.hostStatus[agent]
	if m == nil {
		m = map[string]hostStatusEntry{}
		s.hostStatus[agent] = m
	}
	m[sess] = e
	s.mu.Unlock()
	if cur, found, err := s.dir.Agent(ctx, agent); err == nil && found && sameBinding(cur, bound) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.hostStatus[agent]; m != nil && m[sess] == e {
		delete(m, sess)
		if len(m) == 0 {
			delete(s.hostStatus, agent)
		}
	}
}

// sameBinding reports whether a and b are one join of one name on one
// node, to the millisecond the store keeps JoinedAt.
func sameBinding(a, b identity.Agent) bool {
	return a.Name == b.Name && a.NodeID == b.NodeID && a.JoinedAt.UnixMilli() == b.JoinedAt.UnixMilli()
}

// hostBusy reports whether any of agent's sessions has a live busy report,
// forgetting reports quiet past SessionGrace. The caller holds s.mu.
func (s *Server) hostBusy(agent string, now time.Time) bool {
	busy := false
	for k, e := range s.hostStatus[agent] {
		if now.Sub(e.at) >= SessionGrace {
			delete(s.hostStatus[agent], k)
			continue
		}
		busy = busy || e.busy
	}
	if len(s.hostStatus[agent]) == 0 {
		delete(s.hostStatus, agent)
	}
	return busy
}

// applyStatus fills info's status fields from st.
func applyStatus(info *client.AgentInfo, st agentStatus) {
	info.Status, info.StatusReason, info.BusyFor, info.BusySince = st.status, st.reason, st.busyFor, st.busySince
}
