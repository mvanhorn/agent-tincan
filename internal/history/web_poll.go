package history

// The reply wait's timing policy: how often a web agent reads the
// conversation while the reply is written, how it backs off on rate limits
// and other failures, and how it keeps its relay presence during the wait.

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// DefaultWebPollSchedule is how the conversation is read while the reply
// is being written: the first read 5s after the send, then 5s, 8s and 12s
// apart, then every 20s. Each read is a request to the site on the owner's
// account, so the wait backs off instead of hammering it.
var DefaultWebPollSchedule = []time.Duration{5 * time.Second, 5 * time.Second, 8 * time.Second, 12 * time.Second, 20 * time.Second}

// DefaultClaudeStablePolls and DefaultClaudeStableFor: a claude.ai reply
// with no stop_reason is finished once the same text is read on 4
// consecutive polls spanning at least 10 seconds, so a pause mid-reply is
// not taken for the end. At DefaultWebPollSchedule's cadence 4 polls
// always span more than 10 seconds; the time rule matters for a fast
// PollInterval.
const (
	DefaultClaudeStablePolls = 4
	DefaultClaudeStableFor   = 10 * time.Second
)

// A 429 while waiting for a reply waits the site's Retry-After, or backs
// off from RateLimitBackoffStart, doubling up to RateLimitBackoffMax.
const (
	RateLimitBackoffStart = 30 * time.Second
	RateLimitBackoffMax   = 5 * time.Minute
)

// maxErrorBackoff caps the backoff after other transient read failures
// (an HTTP 5xx, a flaky read).
const maxErrorBackoff = 2 * time.Minute

// DefaultPresenceInterval is how often a web agent busy with a request
// tells the relay it is still there (a peek that claims nothing), so it
// does not show offline during a long wait. It is well inside the relay's
// online window (its poll hold plus 30s).
const DefaultPresenceInterval = 30 * time.Second

// webClock is the reply wait's time source.
type webClock interface {
	Now() time.Time
	// Sleep waits d, or until ctx ends and then returns ctx's error.
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (w *WebAgent) clk() webClock {
	if w.clock != nil {
		return w.clock
	}
	return realClock{}
}

// keepPresence refreshes the agent's relay presence every
// PresenceInterval until the returned stop is called. While a request
// waits minutes for its reply the agent is not long-polling and would
// otherwise show offline. The refresh is a peek with no hold, which claims
// nothing, so requests that arrive meanwhile stay queued for the next
// poll.
func (w *WebAgent) keepPresence(ctx context.Context) (stop func()) {
	every := w.PresenceInterval
	if every <= 0 {
		every = DefaultPresenceInterval
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
				if _, err := w.Relay.Peek(pctx, 0); err != nil && ctx.Err() == nil {
					w.logf("presence: %v", err)
				}
				pcancel()
			}
		}
	}()
	return func() { cancel(); <-done }
}

// pollDelay is the wait before the next detail read once reads have
// succeeded: DefaultWebPollSchedule (or the fixed PollInterval).
func (w *WebAgent) pollDelay(reads int) time.Duration {
	if w.PollInterval > 0 {
		return w.PollInterval
	}
	return DefaultWebPollSchedule[min(reads, len(DefaultWebPollSchedule)-1)]
}

// rateLimitBackoff is the wait after the n-th consecutive 429 (n >= 1)
// that carried no Retry-After.
func rateLimitBackoff(n int) time.Duration {
	d := RateLimitBackoffStart
	for i := 1; i < n && d < RateLimitBackoffMax; i++ {
		d *= 2
	}
	return min(d, RateLimitBackoffMax)
}

// waitReply reads the conversation on the poll schedule until the reply
// to this request's message is finished, and returns that read and the id
// of this request's user message. A claude.ai reply without a stop_reason
// counts as finished once the same reply is read on ClaudeStablePolls
// consecutive polls spanning at least ClaudeStableFor. onBind, when set,
// is called once with the user message id when it is first seen. Errors
// that will not clear (logged out, the API changed, the extension gone)
// end the wait at once, as does a later user turn with no reply to this
// one. A 429 waits the site's Retry-After or backs off exponentially, and
// holds every request to the site back meanwhile; when that wait would
// outlast ctx the rate limit is returned. Other failures (an HTTP 5xx)
// back off too, and are retried until ctx ends.
func (w *WebAgent) waitReply(ctx context.Context, convID string, a replyAnchor, onBind func(string)) (json.RawMessage, string, error) {
	clock := w.clk()
	// The budget is ctx's deadline on the wait's own clock.
	budgetEnd := clock.Now().Add(DefaultWebRequestTimeout)
	if dl, ok := ctx.Deadline(); ok {
		budgetEnd = clock.Now().Add(time.Until(dl))
	}
	stablePolls := w.ClaudeStablePolls
	if stablePolls <= 0 {
		stablePolls = DefaultClaudeStablePolls
	}
	stableFor := w.ClaudeStableFor
	if stableFor <= 0 {
		stableFor = DefaultClaudeStableFor
	}
	op := w.live().detailOp
	prev, seen := "", 0
	var first time.Time
	reads, limited, failures := 0, 0, 0
	delay := w.pollDelay(0)
	for {
		if clock.Now().Add(delay).After(budgetEnd) {
			return nil, a.bound, context.DeadlineExceeded
		}
		if err := clock.Sleep(ctx, delay); err != nil {
			return nil, a.bound, err
		}
		raw, err := w.Native.Request(ctx, op, OpArgs{ID: convID})
		if cerr := ctx.Err(); cerr != nil {
			return nil, a.bound, cerr
		}
		after, isLimit := rateLimited(err)
		switch {
		case err == nil:
			reads++
			limited, failures = 0, 0
			delay = w.pollDelay(reads)
			p, perr := w.progress(raw, a)
			if perr != nil {
				return nil, a.bound, unavailable(w.Site, ErrEndpointChanged, "unexpected conversation shape")
			}
			if p.userID != "" && a.bound == "" {
				a.bound = p.userID
				if onBind != nil {
					onBind(a.bound)
				}
			}
			if p.orphaned {
				return nil, a.bound, errOrphaned
			}
			if p.finished {
				return raw, a.bound, nil
			}
			switch {
			case !p.found || w.Site != SourceClaudeAI:
				prev, seen = "", 0
			case p.sig == prev:
				seen++
				if seen >= stablePolls && clock.Now().Sub(first) >= stableFor {
					return raw, a.bound, nil
				}
			default:
				prev, seen, first = p.sig, 1, clock.Now()
			}
		case isLimit:
			limited++
			if after <= 0 {
				after = rateLimitBackoff(limited)
			}
			// Everything else in this process holds back as long.
			w.Native.cooldown().Note(w.Site, after)
			if clock.Now().Add(after).After(budgetEnd) {
				w.logf("conversation %s: %s is rate-limiting this account; the wait of %s is past this request's budget", convID, siteLabel(w.Site), after)
				return nil, a.bound, err
			}
			w.logf("conversation %s: detail: %v (rate limited; next read in %s)", convID, err, after)
			delay = after
		case errors.Is(err, ErrNotLoggedIn), errors.Is(err, ErrEndpointChanged), errors.Is(err, ErrExtensionNotConnected), errors.Is(err, ErrChromeNotRunning), errors.Is(err, ErrRejected):
			return nil, a.bound, err
		default:
			failures++
			delay = min(w.pollDelay(reads)<<min(failures, 10), maxErrorBackoff)
			kind := "retrying"
			if serverError(err) {
				kind = "server error; retrying"
			}
			w.logf("conversation %s: detail: %v (%s in %s)", convID, err, kind, delay)
		}
	}
}
