package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// computeStatus turns the facts the roster already gathers into one status
// and its detail, the same for every surface.
func TestComputeStatus(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	settled := now.Add(-time.Hour) // the relay's quiet window is over
	for _, tc := range []struct {
		name  string
		f     statusFacts
		quiet time.Time
		want  agentStatus
	}{
		{"idle", statusFacts{online: true, wake: "wait"}, settled, agentStatus{status: client.StatusIdle}},
		{"offline", statusFacts{wake: "command"}, settled, agentStatus{status: client.StatusOffline}},
		{"busy names the asker", statusFacts{online: true, wake: "wait", stat: store.QueueStat{Claimed: 1, OldestClaim: now.Add(-5 * time.Minute), ClaimAsker: "claude-code"}}, settled,
			agentStatus{status: client.StatusBusy, busyFor: "claude-code", busySince: now.Add(-5 * time.Minute)}},
		{"busy while offline", statusFacts{wake: "webhook", stat: store.QueueStat{Claimed: 1, OldestClaim: now, ClaimAsker: "codex"}}, settled,
			agentStatus{status: client.StatusBusy, busyFor: "codex", busySince: now}},
		{"host busy", statusFacts{online: true, wake: "channel", hostBusy: true}, settled, agentStatus{status: client.StatusBusy}},
		{"claim lapsed", statusFacts{online: true, wake: "wait", stat: store.QueueStat{Queued: 1, OldestQueued: now.Add(-40 * time.Minute), Lapsed: 1}}, settled,
			agentStatus{status: client.StatusStuck, reason: "claim lapsed"}},
		{"lapsed beats a live claim", statusFacts{online: true, wake: "wait", stat: store.QueueStat{Queued: 1, Lapsed: 1, Claimed: 1, OldestClaim: now, ClaimAsker: "codex"}}, settled,
			agentStatus{status: client.StatusStuck, reason: "claim lapsed"}},
		{"woken silent", statusFacts{wake: "webhook", stat: store.QueueStat{Queued: 1, OldestQueued: now.Add(-15 * time.Minute)}, target: envelope.Target{WokenAt: now.Add(-15 * time.Minute), WakeResult: "ok", Unanswered: true}}, settled,
			agentStatus{status: client.StatusStuck, reason: "woken, silent 15m"}},
		{"overdue check", statusFacts{wake: "schedule", target: envelope.Target{CheckEverySeconds: 300, Overdue: true}}, settled,
			agentStatus{status: client.StatusStuck, reason: "overdue check"}},
		{"queued while offline", statusFacts{wake: "command", stat: store.QueueStat{Queued: 3, OldestQueued: now.Add(-time.Minute)}}, settled,
			agentStatus{status: client.StatusStuck, reason: "3 queued while offline"}},
		{"relay wakes it", statusFacts{wake: "webhook", stat: store.QueueStat{Queued: 3, OldestQueued: now.Add(-time.Minute)}}, settled,
			agentStatus{status: client.StatusOffline}},
		{"schedule on time", statusFacts{wake: "schedule", stat: store.QueueStat{Queued: 1, OldestQueued: now.Add(-time.Minute)}, target: envelope.Target{CheckEverySeconds: 300}}, settled,
			agentStatus{status: client.StatusOffline}},
		{"stale queue", statusFacts{online: true, wake: "wait", stat: store.QueueStat{Queued: 2, OldestQueued: now.Add(-2 * time.Hour)}}, settled,
			agentStatus{status: client.StatusStuck, reason: "oldest queued 2h"}},
		{"quiet after a restart", statusFacts{online: true, wake: "wait", stat: store.QueueStat{Queued: 1, Lapsed: 1}}, now.Add(time.Minute),
			agentStatus{status: client.StatusIdle}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeStatus(tc.f, now, tc.quiet); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// statusHarness is wakeHarness with the store on the same fake clock, so
// leases run out as the test advances it, moved past the quiet window that
// follows the relay's start.
func statusHarness(t *testing.T) (*harness, *fakeClock, *fakeWaker) {
	t.Helper()
	h, clk, fw := wakeHarness(t)
	h.st.SetClock(clk.Now)
	clk.advance(DefaultClaimLease)
	return h, clk, fw
}

// An agent with a claim taken 5 minutes ago under a 30-minute lease is busy
// on the asker's request.
func TestRosterBusyNamesAsker(t *testing.T) {
	h, clk, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "draft the release note")
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	claimed := clk.Now().Truncate(time.Millisecond) // the store keeps milliseconds
	clk.advance(5 * time.Minute)
	a := agentInfo(t, h, macAddr, "muse")
	if a.Status != client.StatusBusy || a.BusyFor != "grokbot" || !a.BusySince.Equal(claimed) || a.StatusReason != "" {
		t.Fatalf("muse = %+v", a)
	}
}

// AE4: a claim whose lease lapses with no progress is requeued by Sweep,
// and the agent shows stuck (claim lapsed) until it claims that ask again.
func TestRosterStuckClaimLapsed(t *testing.T) {
	h, clk, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "summarize this paper")
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(DefaultClaimLease + time.Second)
	h.srv.Sweep(t.Context())
	if a := agentInfo(t, h, macAddr, "muse"); a.Status != client.StatusStuck || a.StatusReason != "claim lapsed" {
		t.Fatalf("after the lapse: %+v", a)
	}
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	if a := agentInfo(t, h, macAddr, "muse"); a.Status != client.StatusBusy || a.BusyFor != "grokbot" {
		t.Fatalf("claimed again: %+v", a)
	}
}

// A claim 45 minutes old whose lease a progress note renewed 2 minutes ago
// is still busy.
func TestRosterBusyAfterProgress(t *testing.T) {
	h, clk, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "long job")
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(20 * time.Minute)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/progress", `{"note":"halfway"}`, http.StatusOK, nil)
	clk.advance(23 * time.Minute)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/progress", `{"note":"nearly"}`, http.StatusOK, nil)
	clk.advance(2 * time.Minute)
	h.srv.Sweep(t.Context())
	if a := agentInfo(t, h, macAddr, "muse"); a.Status != client.StatusBusy || a.BusyFor != "grokbot" {
		t.Fatalf("muse = %+v", a)
	}
}

// A webhook agent woken 15 minutes ago that never polled is stuck.
func TestRosterStuckWokenSilent(t *testing.T) {
	h, clk, fw := statusHarness(t)
	h.send(instinctAddr, "grokbot", "check the logs")
	fw.set("grokbot", store.Wake{At: clk.Now(), Result: envelope.WakeOK})
	clk.advance(15 * time.Minute)
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Status != client.StatusStuck || a.StatusReason != "woken, silent 15m" {
		t.Fatalf("grokbot = %+v", a)
	}
}

// An offline command-wake agent with queued work is stuck; with nothing
// queued it is offline.
func TestRosterOfflineCommandAgent(t *testing.T) {
	h, _, fw := statusHarness(t)
	fw.methods["muse"] = "command"
	if a := agentInfo(t, h, macAddr, "muse"); a.Status != client.StatusOffline || a.StatusReason != "" {
		t.Fatalf("nothing queued: %+v", a)
	}
	h.send(grokAddr, "muse", "one")
	if a := agentInfo(t, h, macAddr, "muse"); a.Status != client.StatusStuck || a.StatusReason != "1 queued while offline" {
		t.Fatalf("queued while offline: %+v", a)
	}
}

// An agent whose only open ask waits on the asker's answer (needs_input) is
// idle: nothing waits on it.
func TestRosterNeedsInputIsIdle(t *testing.T) {
	h, _, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "book a table")
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"status":"needs_input","body":"which night?"}`, http.StatusOK, nil)
	if a := agentInfo(t, h, macAddr, "muse"); a.Status != client.StatusIdle {
		t.Fatalf("muse = %+v", a)
	}
}

// Right after a relay starts, a claim that lapsed before it does not show
// stuck until a lease window has passed: the agent may not have had a
// chance to call this relay yet.
func TestRosterQuietAfterRestart(t *testing.T) {
	h, clk, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "summarize")
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(DefaultClaimLease + time.Second)
	h.srv.Sweep(t.Context())
	restarted := New(h.srv.dir, h.st, Config{Now: clk.Now})
	roster := func() client.AgentInfo {
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.RemoteAddr = macAddr
		rec := httptest.NewRecorder()
		restarted.Handler().ServeHTTP(rec, req)
		var out client.Roster
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode roster: %v: %s", err, rec.Body.String())
		}
		for _, a := range out.Agents {
			if a.Name == "muse" {
				return a
			}
		}
		t.Fatalf("muse missing: %s", rec.Body.String())
		return client.AgentInfo{}
	}
	if a := roster(); a.Status == client.StatusStuck {
		t.Fatalf("just restarted: %+v", a)
	}
	clk.advance(DefaultClaimLease + time.Second)
	if a := roster(); a.Status != client.StatusStuck || a.StatusReason != "claim lapsed" {
		t.Fatalf("a lease window after the restart: %+v", a)
	}
}

// A roster read by a joined, non-admin agent says whose request a busy
// agent holds but never its text or progress note.
func TestRosterStatusCarriesNoRequestText(t *testing.T) {
	h, _, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "secret-body-text")
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/progress", `{"note":"secret-progress-note"}`, http.StatusOK, nil)
	rec := h.do(instinctAddr, "GET", "/v1/agents", "", http.StatusOK, nil)
	body := rec.Body.String()
	if strings.Contains(body, "secret-body-text") || strings.Contains(body, "secret-progress-note") {
		t.Fatalf("roster leaks request text: %s", body)
	}
	if !strings.Contains(body, `"status":"busy"`) || !strings.Contains(body, `"busy_for":"grokbot"`) {
		t.Fatalf("roster lacks the status: %s", body)
	}
}
