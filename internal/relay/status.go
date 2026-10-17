package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
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
// yet. Busy means a live claim or the host saying so; the rest is idle or
// offline by whether the agent is polling.
func computeStatus(f statusFacts, now, quietUntil time.Time) agentStatus {
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
	relayWakes := f.wake == "webhook" || f.wake == "email" || f.wake == "schedule"
	if !f.online && f.stat.Queued > 0 && !relayWakes {
		return fmt.Sprintf("%d queued while offline", f.stat.Queued)
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
	var in struct {
		Busy bool `json:"busy"`
	}
	if err == nil {
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		if err = d.Decode(&in); err == nil {
			var extra any
			if d.Decode(&extra) != io.EOF {
				err = errors.New("expected one host status object")
			}
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	now := s.cfg.Now()
	s.mu.Lock()
	m := s.hostStatus[name]
	if m == nil {
		m = map[string]hostStatusEntry{}
		s.hostStatus[name] = m
	}
	m[sess] = hostStatusEntry{busy: in.Busy, at: now}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
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
