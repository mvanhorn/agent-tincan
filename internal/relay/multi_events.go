package relay

import (
	"context"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// WakeMCPEvents is the roster wake method reported while a gateway agent holds
// at least one active MCP Events subscription and has no non-none override in
// wake.json.
const WakeMCPEvents = "mcp-events"

// MultiEvents fans relay queue events out to every non-nil listener, forwarding
// optional Requeuer, Replier, and Clarifier calls to listeners that implement
// them.
type MultiEvents []Events

// Queued forwards a queued request to each listener.
func (m MultiEvents) Queued(ctx context.Context, req envelope.Request) {
	for _, e := range m {
		if e != nil {
			e.Queued(ctx, req)
		}
	}
}

// Requeued forwards a requeued request to each Requeuer listener, falling back
// to Queued for listeners that only implement Events.
func (m MultiEvents) Requeued(ctx context.Context, req envelope.Request) {
	for _, e := range m {
		if e == nil {
			continue
		}
		if rq, ok := e.(Requeuer); ok {
			rq.Requeued(ctx, req)
		} else {
			e.Queued(ctx, req)
		}
	}
}

// Replied forwards a reply notification to each Replier listener.
func (m MultiEvents) Replied(ctx context.Context, req envelope.Request) {
	for _, e := range m {
		if rp, ok := e.(Replier); ok {
			rp.Replied(ctx, req)
		}
	}
}

// NeedsInput forwards a clarification notification to each Clarifier listener.
func (m MultiEvents) NeedsInput(ctx context.Context, req envelope.Request) {
	for _, e := range m {
		if cl, ok := e.(Clarifier); ok {
			cl.NeedsInput(ctx, req)
		}
	}
}

// WithMCPEventsWake wraps base so agents with an active MCP Events subscription
// report wake=mcp-events unless base already configures an explicit non-none
// wake method. Optional Scheduler, WakeReporter, and WakeResumer interfaces on
// base are preserved transparently.
func WithMCPEventsWake(base WakeNamer, hasSub func(agent string) bool) WakeNamer {
	return &mcpEventsWakeNamer{base: base, hasSub: hasSub}
}

type mcpEventsWakeNamer struct {
	base   WakeNamer
	hasSub func(agent string) bool
}

func (w *mcpEventsWakeNamer) WakeMethod(agent string) string {
	method := ""
	if w.base != nil {
		method = w.base.WakeMethod(agent)
	}
	if (method == "" || method == "none") && w.hasSub != nil && w.hasSub(agent) {
		return WakeMCPEvents
	}
	if method == "" {
		return "none"
	}
	return method
}

func (w *mcpEventsWakeNamer) CheckEvery(agent string) time.Duration {
	if sc, ok := w.base.(Scheduler); ok {
		return sc.CheckEvery(agent)
	}
	return 0
}

func (w *mcpEventsWakeNamer) LastWake(agent string) (store.Wake, bool) {
	if wr, ok := w.base.(WakeReporter); ok {
		return wr.LastWake(agent)
	}
	return store.Wake{}, false
}

func (w *mcpEventsWakeNamer) Forget(agent string) {
	if wr, ok := w.base.(WakeReporter); ok {
		wr.Forget(agent)
	}
}

func (w *mcpEventsWakeNamer) Unforget(agent string) {
	if wr, ok := w.base.(WakeReporter); ok {
		wr.Unforget(agent)
	}
}

func (w *mcpEventsWakeNamer) Joined(agent string) {
	if wr, ok := w.base.(WakeReporter); ok {
		wr.Joined(agent)
	}
}

func (w *mcpEventsWakeNamer) RequestsWaiting(agent string) {
	if wr, ok := w.base.(WakeResumer); ok {
		wr.RequestsWaiting(agent)
	}
}

func (w *mcpEventsWakeNamer) ReplyWaiting(agent string) {
	if wr, ok := w.base.(WakeResumer); ok {
		wr.ReplyWaiting(agent)
	}
}
