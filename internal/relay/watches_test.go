package relay

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// relayNotices are the relay's notices queued for agent, oldest first.
func relayNotices(h *harness, agent string) []envelope.Request {
	h.t.Helper()
	ids, err := h.st.QueuedIDs(h.t.Context(), agent)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []envelope.Request
	for _, id := range ids {
		req, _, err := h.st.Request(h.t.Context(), id)
		if err != nil {
			h.t.Fatal(err)
		}
		if req.From == client.RelaySender {
			out = append(out, req)
		}
	}
	return out
}

// watch asks, as the agent at addr, to be told when agent is next idle.
func watch(h *harness, addr, agent string, want int) {
	h.t.Helper()
	h.do(addr, "POST", "/v1/watches", `{"agent":"`+agent+`"}`, want, nil)
}

// A watch on a busy agent fires once, right after it answers its claim,
// and the watcher gets one notice naming it and nothing else.
func TestWatchFiresWhenBusyAgentAnswers(t *testing.T) {
	h, _, _ := statusHarness(t)
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	sent := h.send(grokAddr, "muse", "private-ask-text")
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	watch(h, instinctAddr, "muse", http.StatusCreated)
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 0 {
		t.Fatalf("fired while busy: %+v", n)
	}
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"done"}`, http.StatusOK, nil)
	n := relayNotices(h, "instinct")
	if len(n) != 1 || n[0].Kind != envelope.KindNotify || n[0].Body != "muse is idle now. No reply needed." {
		t.Fatalf("notices = %+v", n)
	}
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 1 {
		t.Fatalf("fired again: %+v", n)
	}
}

// A watch on an agent that is already idle fires on the next sweep, and
// asking twice still sends one notice.
func TestWatchOnIdleAgentFiresOnSweep(t *testing.T) {
	h, _, _ := statusHarness(t)
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	watch(h, instinctAddr, "muse", http.StatusCreated)
	watch(h, instinctAddr, "muse", http.StatusOK)
	if n := relayNotices(h, "instinct"); len(n) != 0 {
		t.Fatalf("fired before the sweep: %+v", n)
	}
	h.srv.Sweep(t.Context())
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 1 {
		t.Fatalf("notices = %+v", n)
	}
}

// An offline agent with nothing in progress or waiting is free too, so a
// watch on an agent the relay wakes on demand does not wait for a poll
// that never comes; the notice says it is offline.
func TestWatchOnOfflineAgent(t *testing.T) {
	h, _, _ := statusHarness(t)
	watch(h, instinctAddr, "grokbot", http.StatusCreated)
	h.srv.Sweep(t.Context())
	n := relayNotices(h, "instinct")
	if len(n) != 1 || n[0].Body != "grokbot is not busy now (offline). No reply needed." {
		t.Fatalf("notices = %+v", n)
	}
}

// The sweep and the post-reply check firing the same watch at once still
// send one notice.
func TestWatchConcurrentChecks(t *testing.T) {
	h, _, _ := statusHarness(t)
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	watch(h, instinctAddr, "muse", http.StatusCreated)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { h.srv.checkWatches(t.Context(), "") })
	}
	wg.Wait()
	if n := relayNotices(h, "instinct"); len(n) != 1 {
		t.Fatalf("notices = %+v", n)
	}
}

// A watch on an agent that stays stuck lapses after 24 hours unsent.
func TestWatchExpiresUnfired(t *testing.T) {
	h, clk, fw := statusHarness(t)
	fw.methods["muse"] = "command"
	h.send(grokAddr, "muse", "queued while offline")
	watch(h, instinctAddr, "muse", http.StatusCreated)
	h.srv.Sweep(t.Context())
	clk.advance(WatchTTL + time.Second)
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 0 {
		t.Fatalf("notices = %+v", n)
	}
	if pending, _ := h.st.PendingWatches(t.Context()); len(pending) != 0 {
		t.Fatalf("pending = %+v", pending)
	}
}

// Removing the target drops the watches on it.
func TestWatchDroppedWithTarget(t *testing.T) {
	h, _, _ := statusHarness(t)
	watch(h, instinctAddr, "muse", http.StatusCreated)
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"muse"}`, http.StatusOK, nil)
	if pending, _ := h.st.PendingWatches(t.Context()); len(pending) != 0 {
		t.Fatalf("pending = %+v", pending)
	}
}

// Only a joined agent watches, only an agent on the roster can be watched,
// and the relay advertises the route.
func TestWatchRoute(t *testing.T) {
	h, _, _ := statusHarness(t)
	watch(h, instinctAddr, "nobody", http.StatusNotFound)
	watch(h, strangerAddr, "muse", http.StatusForbidden)
	h.do(instinctAddr, "POST", "/v1/watches", `{"agent":"muse","extra":1}`, http.StatusBadRequest, nil)
	var caps client.Capabilities
	h.do(museAddr, "GET", "/v1/capabilities", "", http.StatusOK, &caps)
	if !caps.Watches {
		t.Fatalf("capabilities = %+v", caps)
	}
}

// An offline agent the relay wakes on demand is not free while it has
// work queued or claimed: the watch waits until its queue drains, then
// sends one notice.
func TestWatchOfflineTargetWithWork(t *testing.T) {
	for _, method := range []string{"webhook", "email", "schedule"} {
		t.Run(method, func(t *testing.T) {
			h, _, fw := statusHarness(t)
			fw.methods["grokbot"] = method
			sent := h.send(museAddr, "grokbot", "fresh ask")
			watch(h, instinctAddr, "grokbot", http.StatusCreated)
			h.srv.Sweep(t.Context())
			if n := relayNotices(h, "instinct"); len(n) != 0 {
				t.Fatalf("fired with work queued: %+v", n)
			}
			h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
			h.srv.Sweep(t.Context())
			if n := relayNotices(h, "instinct"); len(n) != 0 {
				t.Fatalf("fired with work claimed: %+v", n)
			}
			h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"done"}`, http.StatusOK, nil)
			h.srv.Sweep(t.Context())
			if n := relayNotices(h, "instinct"); len(n) != 1 {
				t.Fatalf("after the queue drained: %+v", n)
			}
		})
	}
}

// Inside the quiet window after a relay start, an offline agent with work
// queued gets no offline notice, since stuck is not reported yet either.
func TestWatchOfflineQueuedInQuietWindow(t *testing.T) {
	h, _, _ := wakeHarness(t)
	h.send(museAddr, "grokbot", "fresh ask")
	watch(h, instinctAddr, "grokbot", http.StatusCreated)
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 0 {
		t.Fatalf("notices = %+v", n)
	}
}

// Host-busy reports and polls live in memory, so right after a relay
// start a busy agent looks offline until its client reports again. No
// watch fires until every client has had a keepalive to re-report.
func TestWatchWaitsForReportsAfterStart(t *testing.T) {
	h, clk, _ := wakeHarness(t)
	watch(h, instinctAddr, "muse", http.StatusCreated)
	h.srv.Sweep(t.Context())
	clk.advance(time.Minute)
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 0 {
		t.Fatalf("fired before muse could re-report: %+v", n)
	}
	hostStatus(h, museAddr, "s1", `{"busy":true}`, http.StatusNoContent)
	clk.advance(3 * time.Minute)
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 0 {
		t.Fatalf("fired while host-busy: %+v", n)
	}
	hostStatus(h, museAddr, "s1", `{"busy":false}`, http.StatusNoContent)
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 1 {
		t.Fatalf("after muse went idle: %+v", n)
	}
}
