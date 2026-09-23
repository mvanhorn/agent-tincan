package wake

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
	auth   []string
	sigs   []string
	fail   atomic.Int32 // fail this many requests first
}

func (rc *recorder) server(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.bodies = append(rc.bodies, string(raw))
		rc.paths = append(rc.paths, r.URL.Path)
		rc.auth = append(rc.auth, r.Header.Get("Authorization"))
		rc.sigs = append(rc.sigs, r.Header.Get("X-Hub-Signature-256"))
		rc.mu.Unlock()
		if rc.fail.Load() > 0 {
			rc.fail.Add(-1)
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (rc *recorder) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.bodies)
}

func queued(w *Waker, to string, n int) {
	for i := range n {
		w.Queued(context.Background(), envelope.Request{ID: "r" + string(rune('a'+i)), To: to, Body: "SECRET call Joe's Garage at 555-0100"})
	}
}

func auditStore(t *testing.T) *store.Store {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func events(t *testing.T, st *store.Store) []string {
	evs, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Event)
	}
	return out
}

func TestBurstGivesOneWebhookWithoutRequestText(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL, BearerToken: "tok"}}, st, Options{Debounce: 50 * time.Millisecond})
	queued(w, "grokbot", 5)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	var body map[string]string
	json.Unmarshal([]byte(rc.bodies[0]), &body)
	if !strings.Contains(body["message"], "5 requests") || strings.Contains(rc.bodies[0], "SECRET") || strings.Contains(rc.bodies[0], "555") {
		t.Fatalf("wake body = %s", rc.bodies[0])
	}
	if rc.auth[0] != "Bearer tok" {
		t.Fatalf("auth = %q", rc.auth[0])
	}
	if got := strings.Join(events(t, st), ","); got != "woke" {
		t.Fatalf("audit = %s", got)
	}
}

func TestWebhookRetriesOnceThenAuditsFailure(t *testing.T) {
	var rc recorder
	rc.fail.Store(1)
	ts := rc.server(t)
	st := auditStore(t)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{Debounce: time.Millisecond, RetryDelay: time.Millisecond})
	queued(w, "grokbot", 1)
	w.Flush()
	if rc.count() != 2 || strings.Join(events(t, st), ",") != "woke" {
		t.Fatalf("calls=%d audit=%v", rc.count(), events(t, st))
	}
	rc.fail.Store(2)
	queued(w, "grokbot", 1)
	w.Flush()
	if got := events(t, st); got[len(got)-1] != "wake_failed" {
		t.Fatalf("audit = %v, want wake_failed last", got)
	}
}

// OpenClaw reads text, Grok Bot reads message; both carry the same count.
func TestWebhookBodyMirrorsTextWithBearerOnly(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"openclaw": {Method: Webhook, URL: ts.URL, BearerToken: "tok"}}, nil, Options{Debounce: time.Millisecond})
	queued(w, "openclaw", 2)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(rc.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["source"] != "agent-tincan" || body["message"] != Message(2) || body["text"] != body["message"] {
		t.Fatalf("wake body = %s", rc.bodies[0])
	}
	if rc.auth[0] != "Bearer tok" || rc.sigs[0] != "" {
		t.Fatalf("auth = %q sig = %q", rc.auth[0], rc.sigs[0])
	}
}

// Hermes verifies webhooks with the GitHub HMAC scheme.
func TestWebhookHMACSignsExactBody(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL, HMACSecret: "hush"}}, nil, Options{Debounce: time.Millisecond})
	queued(w, "hermes", 1)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	sign := func(secret string) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(rc.bodies[0]))
		return "sha256=" + hex.EncodeToString(m.Sum(nil))
	}
	if rc.sigs[0] != sign("hush") {
		t.Fatalf("sig = %q, want %q", rc.sigs[0], sign("hush"))
	}
	if hmac.Equal([]byte(rc.sigs[0]), []byte(sign("wrong"))) {
		t.Fatal("signature verified with the wrong secret")
	}
	if rc.auth[0] != "" {
		t.Fatalf("auth = %q, want none", rc.auth[0])
	}
	if strings.Contains(rc.bodies[0], "SECRET") || strings.Contains(rc.bodies[0], "555") || strings.Contains(rc.bodies[0], "hush") {
		t.Fatalf("wake body leaks: %s", rc.bodies[0])
	}
}

// Instinct wakes on email; the relay sends it through Grok Bot's AgentMail.
func TestEmailWakeUsesAgentMail(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"instinct": {Method: Email, EmailTo: "agent@example.com", AgentMailFrom: "bot@agentmail.to", AgentMailKey: "am_key"}},
		nil, Options{Debounce: time.Millisecond, AgentMailAPI: ts.URL + "/v0"})
	queued(w, "instinct", 2)
	w.Flush()
	if rc.count() != 1 || rc.paths[0] != "/v0/inboxes/bot@agentmail.to/messages/send" || rc.auth[0] != "Bearer am_key" {
		t.Fatalf("paths=%v auth=%v", rc.paths, rc.auth)
	}
	var body map[string]string
	json.Unmarshal([]byte(rc.bodies[0]), &body)
	if body["to"] != "agent@example.com" || !strings.Contains(body["text"], "2 requests") || strings.Contains(rc.bodies[0], "SECRET") {
		t.Fatalf("email body = %s", rc.bodies[0])
	}
}

func TestBudgetStopsWakesButNotQueueing(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	now := time.Unix(1_790_000_000, 0)
	w := New(Config{"instinct": {Method: Webhook, URL: ts.URL, MaxPerHour: 2}}, st, Options{Debounce: time.Millisecond, Now: func() time.Time { return now }})
	for range 3 {
		queued(w, "instinct", 1)
		w.Flush()
	}
	if rc.count() != 2 {
		t.Fatalf("wakes = %d, want 2", rc.count())
	}
	if got := events(t, st); got[len(got)-1] != "wake_skipped" {
		t.Fatalf("audit = %v", got)
	}
	now = now.Add(61 * time.Minute)
	queued(w, "instinct", 1)
	w.Flush()
	if rc.count() != 3 {
		t.Fatalf("after the hour, wakes = %d, want 3", rc.count())
	}
}

func TestAgentSideMethodsAndOnlineAgentsNeedNoRelayWake(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{
		"muse":        {Method: Wait},
		"claude-code": {Method: Channel},
		"grokbot":     {Method: Webhook, URL: ts.URL},
	}, nil, Options{Debounce: time.Millisecond, Online: func(a string) bool { return a == "grokbot" }})
	queued(w, "muse", 1)
	queued(w, "claude-code", 1)
	queued(w, "grokbot", 1) // online: its poller has it
	queued(w, "chatgpt", 1) // not configured: none
	w.Flush()
	if rc.count() != 0 {
		t.Fatalf("unexpected wakes: %d", rc.count())
	}
	for agent, want := range map[string]string{"muse": Wait, "claude-code": Channel, "grokbot": Webhook, "chatgpt": None} {
		if got := w.WakeMethod(agent); got != want {
			t.Errorf("%s wake = %s, want %s", agent, got, want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	if c, err := LoadConfig(filepath.Join(dir, "missing.json")); err != nil || len(c) != 0 {
		t.Fatalf("missing file: %v %v", c, err)
	}
	p := filepath.Join(dir, "wake.json")
	os.WriteFile(p, []byte(`{"grokbot":{"method":"webhook","url":"http://x"}}`), 0o644)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable config should be refused: %v", err)
	}
	os.Chmod(p, 0o600)
	if c, err := LoadConfig(p); err != nil || c["grokbot"].Method != Webhook {
		t.Fatalf("load: %v %v", c, err)
	}
	os.WriteFile(p, []byte(`{"hermes":{"method":"webhook","url":"http://x","hmac_secret":"hush"}}`), 0o600)
	if c, err := LoadConfig(p); err != nil || c["hermes"].HMACSecret != "hush" {
		t.Fatalf("hmac_secret: %v %v", c, err)
	}
	for _, bad := range []string{`{"a":{"method":"webhook"}}`, `{"a":{"method":"email","email_to":"x"}}`, `{"a":{"method":"smoke-signal"}}`} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("config %s should be rejected", bad)
		}
	}
}
