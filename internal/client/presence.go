package client

import (
	"context"
	"time"
)

// DefaultPresenceInterval is how often an agent busy with work tells the
// relay it is still there. It is well inside the relay's online window
// (its poll hold plus 30s), so the agent never shows offline between
// refreshes.
const DefaultPresenceInterval = 30 * time.Second

// presencePeekTimeout bounds one presence refresh.
const presencePeekTimeout = 15 * time.Second

// Presence configures KeepPresence.
type Presence struct {
	// Every is the refresh interval (DefaultPresenceInterval when zero).
	Every time.Duration
	// Cap, when set, stops the refreshes after this long even if stop has
	// not been called, so an agent stuck on hung work falls offline again.
	Cap time.Duration
	// Logf receives refresh errors and the line saying the cap was reached
	// (nothing is logged when nil).
	Logf func(format string, args ...any)
}

// KeepPresence refreshes this agent's relay presence every p.Every until
// the returned stop is called, ctx ends, or p.Cap passes. While an agent
// works on something it is not long-polling and would otherwise show
// offline. Each refresh is a peek with no hold, which claims nothing,
// delivers nothing and marks no reply seen, so requests and replies that
// arrive meanwhile stay waiting for the agent's own check. It never uses
// Poll, which would put waiting requests on a delivery lease. stop waits
// for the refresh goroutine to end.
func (r *Relay) KeepPresence(ctx context.Context, p Presence) (stop func()) {
	every := p.Every
	if every <= 0 {
		every = DefaultPresenceInterval
	}
	logf := p.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		var capped <-chan time.Time
		if p.Cap > 0 {
			ct := time.NewTimer(p.Cap)
			defer ct.Stop()
			capped = ct.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-capped:
				logf("stopped keeping this agent online after %s; it shows offline until it polls again", p.Cap)
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, presencePeekTimeout)
				if _, err := r.Peek(pctx, 0); err != nil && ctx.Err() == nil {
					logf("presence: %v", err)
				}
				pcancel()
			}
		}
	}()
	return func() { cancel(); <-done }
}
