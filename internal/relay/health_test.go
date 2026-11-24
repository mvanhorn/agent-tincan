package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// getURL fetches url with a bounded client. The bound matters: a handler that
// waits on a wedged store would hang a plain http.Get forever, and a probe
// that never comes back is not a signal.
func getURL(t *testing.T, url string) (int, []byte, time.Duration) {
	t.Helper()
	c := &http.Client{Timeout: 15 * time.Second}
	start := time.Now()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body, time.Since(start)
}

func decodeMetrics(t *testing.T, body []byte) relay.Metrics {
	t.Helper()
	var m relay.Metrics
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("metrics body %q: %v", body, err)
	}
	return m
}

// onlyError fails unless body is a refusal that carries an error and nothing
// else.
func onlyError(t *testing.T, who string, body []byte) {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil || len(out) != 1 || out["error"] == nil {
		t.Fatalf("%s: refusal carries more than an error: %s", who, body)
	}
}

// runRelay starts the relay's sweep loop, as tincan relay does, and stops it
// when the test ends.
func runRelay(t *testing.T, m *testrelay.Mesh) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Server.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// adminSocket serves the relay's local admin handler, as the admin socket on
// the relay host does, and returns its base URL.
func adminSocket(t *testing.T, m *testrelay.Mesh) string {
	t.Helper()
	srv := httptest.NewServer(m.Server.AdminHandler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// waitMetrics polls the admin /metrics until ok accepts an answer or the
// deadline passes, and returns the last answer.
func waitMetrics(t *testing.T, url string, within time.Duration, ok func(int, relay.Metrics) bool) (int, relay.Metrics) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		code, body, _ := getURL(t, url)
		got := decodeMetrics(t, body)
		if ok(code, got) || time.Now().After(deadline) {
			return code, got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// /healthz needs no join and says only that the process serves HTTP. Every
// caller that may use it gets the same bytes: no agent or machine name, no
// count, no version, no address and nothing derived from the relay key. A
// guest is refused it like any route not written for guests.
func TestHealthzIsLivenessOnly(t *testing.T) {
	m := testrelay.NewWithAdminLogin(t, relay.Config{Version: "9.9.9-test"})
	m.Server.SetURLs([]string{"http://tincan-relay.example.ts.net", "http://100.0.0.50"})
	m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	key := relay.KeyForTest(m.Server)
	urls := map[string]string{"admin socket": adminSocket(t, m)}
	for _, who := range []string{"stranger", "grokbot", "admin"} {
		urls[who] = m.URL(who)
	}
	forbidden := []string{key, "9.9.9", "grokbot", "instinct", "muse", "grok-bot", "macbook-pro-44", "tincan-relay", "100.0.0", testrelay.Login}
	for who, base := range urls {
		code, body, _ := getURL(t, base+"/healthz")
		got := strings.TrimSpace(string(body))
		if code != http.StatusOK || got != `{"status":"ok"}` {
			t.Fatalf("%s /healthz: %d %s, want 200 {\"status\":\"ok\"}", who, code, got)
		}
		for _, s := range forbidden {
			if strings.Contains(got, s) {
				t.Fatalf("%s /healthz carries %q: %s", who, s, got)
			}
		}
	}
	if code, body, _ := getURL(t, m.URL("friend")+"/healthz"); code != http.StatusForbidden {
		t.Fatalf("guest /healthz: %d %s, want 403", code, body)
	}
	resp, err := http.Post(m.URL("stranger")+"/healthz", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz: %d, want 405", resp.StatusCode)
	}
}

// /metrics is for admin devices and the local admin socket. A stranger, a
// joined agent and a guest get 403 with only an error. The admin's answer
// holds counts and ages: no request text, id or agent name, and nothing
// derived from the relay key.
func TestMetricsIsAdminOnly(t *testing.T) {
	m := testrelay.NewWithAdminLogin(t, relay.Config{})
	m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	const secret = "the vault combination is 4-8-15-16"
	req, err := m.Client(t, "grokbot").Send(t.Context(), "instinct", secret, envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"stranger", "grokbot", "instinct", "friend"} {
		code, body, _ := getURL(t, m.URL(who)+"/metrics")
		if code != http.StatusForbidden {
			t.Fatalf("%s /metrics: %d %s, want 403", who, code, body)
		}
		onlyError(t, who+" /metrics", body)
	}

	code, body, _ := getURL(t, m.URL("admin")+"/metrics")
	got := decodeMetrics(t, body)
	if code != http.StatusOK || !got.Healthy || !got.Store.OK || got.Queue.Queued != 1 {
		t.Fatalf("admin device /metrics: %d %s, want 200, healthy, one queued", code, body)
	}
	for _, s := range []string{"vault", "4-8-15-16", req.ID, req.TraceID, "grokbot", "instinct", "friend", relay.KeyForTest(m.Server)} {
		if strings.Contains(string(body), s) {
			t.Fatalf("admin /metrics carries %q: %s", s, body)
		}
	}

	// The local admin socket, as curl --unix-socket <state-dir>/admin.sock
	// reaches it on the relay host.
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := client.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: m.Server.AdminHandler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	var local relay.Metrics
	if err := client.NewRelaySocket(sock).Raw(t.Context(), "GET", "/metrics", nil, &local); err != nil {
		t.Fatalf("admin socket /metrics: %v", err)
	}
	if !local.Healthy || !local.Store.OK || local.Queue.Queued != 1 {
		t.Fatalf("admin socket /metrics: %+v", local)
	}
}

// An idle relay and a wedged one looked the same from outside: no traffic,
// no error, the process up. Here the relay is idle and healthy, then its
// store is wedged the way a stuck SQLite connection wedges it (the store has
// one connection and something holds it). /metrics on the local admin socket
// must say so promptly, while /healthz cannot tell the difference by design.
// Over the tailnet the admin check cannot read the store, so /metrics there
// refuses promptly instead of hanging. Releasing the store brings the relay
// back to healthy on its own.
func TestMetricsTellsWedgedFromIdle(t *testing.T) {
	const stallAfter = time.Second
	m := testrelay.New(t, relay.Config{SweepEvery: 20 * time.Millisecond, StallAfter: stallAfter})
	runRelay(t, m)
	metrics := adminSocket(t, m) + "/metrics"

	code, idle := waitMetrics(t, metrics, 5*time.Second, func(code int, got relay.Metrics) bool {
		return code == http.StatusOK && got.Sweep.Passes > 0
	})
	if code != http.StatusOK || !idle.Healthy || len(idle.Problems) != 0 || !idle.Store.OK {
		t.Fatalf("idle relay: %d %+v, want 200 and healthy", code, idle)
	}
	if idle.Sweep.LastOKAgeSeconds > stallAfter.Seconds() || idle.Queue.Queued != 0 || idle.Queue.Claimed != 0 {
		t.Fatalf("idle relay: %+v, want a fresh sweep and an empty queue", idle)
	}

	conn, err := m.Store.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			conn.Close()
		}
	}
	t.Cleanup(release)
	time.Sleep(stallAfter + 300*time.Millisecond)

	code, body, took := getURL(t, metrics)
	wedged := decodeMetrics(t, body)
	if code != http.StatusServiceUnavailable || wedged.Healthy || wedged.Store.OK {
		t.Fatalf("wedged relay: %d %s, want 503, not healthy, store not ok", code, body)
	}
	if took > 5*time.Second {
		t.Fatalf("wedged relay: /metrics took %s; it must not wait on the store", took)
	}
	if wedged.Sweep.LastOKAgeSeconds <= stallAfter.Seconds() || len(wedged.Problems) != 2 {
		t.Fatalf("wedged relay: %s, want the stalled sweep and the store named", body)
	}

	if code, body, _ := getURL(t, m.URL("stranger")+"/healthz"); code != http.StatusOK || strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Fatalf("/healthz while wedged: %d %s, want the same 200 as idle", code, body)
	}
	code, body, took = getURL(t, m.URL("admin")+"/metrics")
	if code != http.StatusInternalServerError || took > 3*time.Second {
		t.Fatalf("admin device /metrics while wedged: %d after %s %s, want a prompt 500", code, took, body)
	}
	onlyError(t, "admin device /metrics while wedged", body)

	release()
	code, back := waitMetrics(t, metrics, 5*time.Second, func(code int, got relay.Metrics) bool {
		return code == http.StatusOK && got.Healthy
	})
	if code != http.StatusOK || !back.Healthy {
		t.Fatalf("released relay: %d %+v, want healthy again", code, back)
	}
}

// A sweep loop that stops while the store is fine is a stall too: /metrics
// names the sweep alone, and one completed pass clears it.
func TestMetricsReportsAStoppedSweepLoop(t *testing.T) {
	m := testrelay.New(t, relay.Config{StallAfter: 50 * time.Millisecond})
	time.Sleep(100 * time.Millisecond)
	code, body, _ := getURL(t, m.URL("admin")+"/metrics")
	got := decodeMetrics(t, body)
	if code != http.StatusServiceUnavailable || got.Healthy || !got.Store.OK || len(got.Problems) != 1 || !strings.Contains(got.Problems[0], "sweep loop") {
		t.Fatalf("no sweep pass past StallAfter: %d %s, want 503 naming only the sweep loop", code, body)
	}
	m.Server.Sweep(t.Context())
	code, body, _ = getURL(t, m.URL("admin")+"/metrics")
	got = decodeMetrics(t, body)
	if code != http.StatusOK || !got.Healthy || got.Sweep.Passes != 1 {
		t.Fatalf("after one pass: %d %s, want 200, healthy, one pass", code, body)
	}
}

// failingLookups fails the directory's per-node agents read on demand, the
// read the guest gate and the admin check make on every call.
type failingLookups struct {
	identity.Store
	fail atomic.Bool
}

func (f *failingLookups) AgentsByNode(ctx context.Context, node string) ([]identity.Agent, error) {
	if f.fail.Load() {
		return nil, errors.New("database is locked")
	}
	return f.Store.AgentsByNode(ctx, node)
}

// When the store cannot read the caller's agents, /healthz still answers
// anyone. /metrics over the tailnet is refused, the admin device included:
// without the read the relay cannot rule out a guest on it. The local admin
// socket needs no read and still serves /metrics. Every route that refuses
// answers 500, the relay's failure, not a 403 that reads as the caller's.
func TestHealthRoutesAnswerWhenCallerLookupFails(t *testing.T) {
	lookups := &failingLookups{}
	m := testrelay.NewWithIdentityStore(t, relay.Config{}, func(st identity.Store) identity.Store {
		lookups.Store = st
		return lookups
	})
	m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	lookups.fail.Store(true)

	for _, who := range []string{"stranger", "grokbot", "admin", "friend"} {
		code, body, _ := getURL(t, m.URL(who)+"/healthz")
		if code != http.StatusOK || strings.TrimSpace(string(body)) != `{"status":"ok"}` {
			t.Fatalf("%s /healthz with lookups failing: %d %s, want 200 ok", who, code, body)
		}
	}
	for _, who := range []string{"admin", "stranger", "grokbot", "friend"} {
		code, body, _ := getURL(t, m.URL(who)+"/metrics")
		if code != http.StatusInternalServerError {
			t.Fatalf("%s /metrics with lookups failing: %d %s, want 500", who, code, body)
		}
		onlyError(t, who+" /metrics", body)
	}
	code, body, _ := getURL(t, adminSocket(t, m)+"/metrics")
	if got := decodeMetrics(t, body); code != http.StatusOK || !got.Healthy || !got.Store.OK {
		t.Fatalf("admin socket /metrics with lookups failing: %d %s, want 200", code, body)
	}
	for _, who := range []string{"grokbot", "admin"} {
		err := m.Client(t, who).Raw(t.Context(), "GET", "/v1/whoami", nil, nil)
		if !client.IsStatus(err, http.StatusInternalServerError) || !strings.Contains(err.Error(), identity.ErrLookupFailed.Error()) {
			t.Fatalf("%s whoami with lookups failing: %v, want 500 naming the failed read", who, err)
		}
	}

	lookups.fail.Store(false)
	if err := m.Client(t, "grokbot").Raw(t.Context(), "GET", "/v1/whoami", nil, nil); err != nil {
		t.Fatalf("whoami once lookups work: %v", err)
	}
	if code, body, _ := getURL(t, m.URL("admin")+"/metrics"); code != http.StatusOK {
		t.Fatalf("admin device /metrics once lookups work: %d %s, want 200", code, body)
	}
	if code, body, _ := getURL(t, m.URL("friend")+"/healthz"); code != http.StatusForbidden {
		t.Fatalf("guest /healthz once lookups work: %d %s, want 403", code, body)
	}
}

// metricsWaker names wake methods and reports the last wakes the test sets,
// so /metrics has a relay-woken agent to count.
type metricsWaker struct {
	methods map[string]string
	wakes   map[string]store.Wake
}

func (w *metricsWaker) WakeMethod(agent string) string {
	if m := w.methods[agent]; m != "" {
		return m
	}
	return "none"
}

func (w *metricsWaker) LastWake(agent string) (store.Wake, bool) {
	wk, ok := w.wakes[agent]
	return wk, ok
}

func (w *metricsWaker) Forget(string)   {}
func (w *metricsWaker) Unforget(string) {}
func (w *metricsWaker) Joined(string)   {}

// /metrics makes two store reads, the backlog probe and the agents read
// behind wake_failed_agents. When only the second fails (the agents table is
// gone, the requests table the probe reads is not), the relay must not
// report healthy with a wake_failed_agents of 0 it never measured. It reads
// through the local admin socket: with the agents table gone, the tailnet
// admin check cannot run.
func TestMetricsAgentsReadFailureIsNotHealthy(t *testing.T) {
	m := testrelay.New(t, relay.Config{StallAfter: time.Hour})
	metrics := adminSocket(t, m) + "/metrics"
	m.Server.SetWakeNamer(&metricsWaker{
		methods: map[string]string{"grokbot": "webhook", "muse": "webhook"},
		wakes: map[string]store.Wake{
			"grokbot": {At: time.Now().Add(time.Minute), Result: "failed"},
			"muse":    {At: time.Now().Add(time.Minute), Result: envelope.WakeOK},
		},
	})
	code, body, _ := getURL(t, metrics)
	got := decodeMetrics(t, body)
	if code != http.StatusOK || !got.Healthy || got.WakeFailedAgents != 1 {
		t.Fatalf("both reads working: %d %s, want 200, healthy, one failed wake", code, body)
	}

	db := m.Store.DB()
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE agents RENAME TO agents_unreadable`); err != nil {
		t.Fatal(err)
	}
	code, body, took := getURL(t, metrics)
	got = decodeMetrics(t, body)
	if code != http.StatusServiceUnavailable || got.Healthy || got.Store.OK || got.WakeFailedAgents != 0 {
		t.Fatalf("agents read failing: %d %s, want 503, store not ok, no count", code, body)
	}
	if len(got.Problems) != 1 || !strings.Contains(got.Problems[0], "wake_failed_agents") {
		t.Fatalf("agents read failing: problems %v, want the unknown wake_failed_agents named", got.Problems)
	}
	if took > 5*time.Second {
		t.Fatalf("agents read failing: /metrics took %s", took)
	}

	if _, err := db.ExecContext(t.Context(), `ALTER TABLE agents_unreadable RENAME TO agents`); err != nil {
		t.Fatal(err)
	}
	code, body, _ = getURL(t, metrics)
	if got = decodeMetrics(t, body); code != http.StatusOK || !got.Healthy || got.WakeFailedAgents != 1 {
		t.Fatalf("agents read restored: %d %s, want 200, healthy, one failed wake", code, body)
	}
}
