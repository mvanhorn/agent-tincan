package history

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const webProbeInterval = 2 * time.Minute

type webAuthState struct {
	mu                sync.Mutex
	latest            envelope.WebStatus
	reported          time.Time
	fresh             time.Time
	probeUnsupported  bool
	reportUnsupported bool
}

func (w *WebAgent) observeAuth(err error) {
	if err != nil && !errors.Is(err, ErrNotLoggedIn) {
		return
	}
	state := "authenticated"
	if err != nil {
		state = "signed_out"
	}
	w.auth.mu.Lock()
	defer w.auth.mu.Unlock()
	now := w.clk().Now()
	if !now.After(w.auth.latest.ObservedAt) {
		now = w.auth.latest.ObservedAt.Add(time.Millisecond)
	}
	if state != w.auth.latest.State {
		if state == "signed_out" {
			w.logf("signed out of %s; sign in there in Chrome without restarting Chrome", w.site().host)
		} else if w.auth.latest.State != "" {
			w.logf("authenticated session restored on %s", w.site().host)
		}
	}
	w.auth.latest = envelope.WebStatus{Site: w.site().host, State: state, ObservedAt: now}
	if err == nil {
		w.auth.fresh = now
	}
}

func (w *WebAgent) probeSession(ctx context.Context) {
	w.observeAuth(w.sessionError(ctx))
}

func (w *WebAgent) sessionError(ctx context.Context) error {
	w.auth.mu.Lock()
	disabled := w.auth.probeUnsupported
	w.auth.mu.Unlock()
	if disabled {
		return ErrRejected
	}
	timeout := 30 * time.Second
	if w.site().listInTab {
		timeout = TabReadClientTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := w.Native.Request(ctx, w.site().op(opSession), OpArgs{})
	if errors.Is(err, ErrRejected) {
		w.auth.mu.Lock()
		if !w.auth.probeUnsupported {
			w.logf("session probes unavailable; upgrade the Chrome extension; traffic detection remains active")
		}
		w.auth.probeUnsupported = true
		w.auth.mu.Unlock()
	}
	return err
}

// authRequest observes failures before callers convert or swallow them.
// Public or cached detail reads need a fresh session check to clear a failure.
func (w *WebAgent) authRequest(ctx context.Context, op Op, args OpArgs) (json.RawMessage, error) {
	raw, err := w.Native.Request(ctx, op, args)
	if err != nil {
		w.observeAuth(err)
	} else if w.Site == SourceChatGPT || w.Site == SourceDots {
		// These reads fetch chatgptAuth on every operation, including old extensions.
		w.observeAuth(nil)
	} else {
		w.auth.mu.Lock()
		needs := w.auth.latest.State != "authenticated" || w.clk().Now().Sub(w.auth.fresh) >= webProbeInterval
		w.auth.mu.Unlock()
		if needs {
			w.probeSession(ctx)
		}
	}
	return raw, err
}

func (w *WebAgent) authSend(ctx context.Context, src Source, message, conv string, newChat bool) (SendResult, error) {
	result, err := w.Native.Send(ctx, src, message, conv, newChat)
	w.observeAuth(err)
	return result, err
}

// runWebStatus keeps relay I/O outside the site lock. A single reporter sends
// the latest snapshot in order and bounds retries even when the relay is down.
func (w *WebAgent) runWebStatus(ctx context.Context) {
	reporterDone := make(chan struct{})
	go func() { defer close(reporterDone); w.reportWebStatus(ctx) }()
	defer func() { <-reporterDone }()
	next := time.Time{}
	for ctx.Err() == nil {
		if !w.clk().Now().Before(next) {
			next = w.probeIdle(ctx)
		}

		if w.clk().Sleep(ctx, time.Second) != nil {
			return
		}
	}
}

func (w *WebAgent) reportWebStatus(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		w.auth.mu.Lock()
		snapshot := w.auth.latest
		pending := snapshot.ObservedAt.After(w.auth.reported) && !w.auth.reportUnsupported
		w.auth.mu.Unlock()
		if pending && w.Relay != nil {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := w.Relay.ReportWebStatus(rctx, snapshot)
			cancel()
			w.auth.mu.Lock()
			switch {
			case err == nil:
				w.auth.reported = snapshot.ObservedAt
				backoff = time.Second
			case errors.Is(err, client.ErrWebStatusUnsupported):
				w.auth.reportUnsupported = true
				w.logf("%v; continuing to serve", err)
			default:
				backoff = min(backoff*2, time.Minute)
			}
			w.auth.mu.Unlock()
		}
		if w.clk().Sleep(ctx, backoff) != nil {
			return
		}
	}
}

// probeIdle starts only while idle, but never makes a send wait for browser I/O.
func (w *WebAgent) probeIdle(ctx context.Context) time.Time {
	now := w.clk().Now()
	if !w.mu.TryLock() {
		return now.Add(time.Second)
	}
	generation := w.sendGeneration
	w.mu.Unlock()
	if w.Native == nil {
		return now.Add(webProbeInterval)
	}
	if left := w.Native.CooldownRemaining(w.Site); left > 0 {
		return now.Add(left)
	}
	w.auth.mu.Lock()
	fresh := w.auth.fresh
	w.auth.mu.Unlock()
	if !fresh.IsZero() && now.Sub(fresh) < webProbeInterval {
		return fresh.Add(webProbeInterval)
	}
	err := w.sessionError(ctx)
	if w.mu.TryLock() {
		if generation == w.sendGeneration {
			w.observeAuth(err)
		}
		w.mu.Unlock()
	}
	return w.clk().Now().Add(webProbeInterval)
}
