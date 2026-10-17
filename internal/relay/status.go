package relay

import (
	"fmt"
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

// applyStatus fills info's status fields from st.
func applyStatus(info *client.AgentInfo, st agentStatus) {
	info.Status, info.StatusReason, info.BusyFor, info.BusySince = st.status, st.reason, st.busyFor, st.busySince
}
