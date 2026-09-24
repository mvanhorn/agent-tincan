package wake

import (
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
)

// openclawHook records what an OpenClaw gateway's POST /hooks/agent would
// see, and checks auth the way the gateway does: a nonempty Bearer token,
// else x-openclaw-token; a token query parameter is a 400.
type openclawHook struct {
	token string
	fail  atomic.Int32

	mu    sync.Mutex
	calls []openclawCall
}

type openclawCall struct {
	path, auth, idem, contentType string
	query                         string
	body                          map[string]any
}

func (h *openclawHook) server(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		h.mu.Lock()
		h.calls = append(h.calls, openclawCall{
			path: r.URL.Path, auth: r.Header.Get("Authorization"), idem: r.Header.Get("Idempotency-Key"),
			contentType: r.Header.Get("Content-Type"), query: r.URL.RawQuery, body: body,
		})
		h.mu.Unlock()
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Has("token") {
			http.Error(w, "token query rejected", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+h.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if msg, _ := body["message"].(string); strings.TrimSpace(msg) == "" {
			http.Error(w, `{"ok":false,"error":"message required"}`, http.StatusBadRequest)
			return
		}
		if h.fail.Load() > 0 {
			h.fail.Add(-1)
			http.Error(w, `{"ok":false,"error":"gateway_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"runId":"run-1"}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (h *openclawHook) snapshot() []openclawCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]openclawCall(nil), h.calls...)
}

// The OpenClaw format posts the /hooks/agent payload: message (required),
// agentId for routing, a name for gateway logs, and deliver false so the
// run's output is not announced into the main session; the reply goes back
// through tincan instead. Auth is the hook token as a bearer header.
func TestOpenClawWebhookPayloadAndAuth(t *testing.T) {
	h := &openclawHook{token: "hook-tok"}
	ts := h.server(t)
	w := New(Config{"claw": {Method: Webhook, Format: FormatOpenClaw, URL: ts.URL + "/hooks/agent", BearerToken: "hook-tok", AgentID: "main"}}, nil, Options{Debounce: time.Millisecond})
	queued(w, "claw", 2)
	w.Flush()
	calls := h.snapshot()
	if len(calls) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.path != "/hooks/agent" || c.query != "" {
		t.Errorf("path = %q query = %q", c.path, c.query)
	}
	if c.auth != "Bearer hook-tok" {
		t.Errorf("auth = %q", c.auth)
	}
	if c.contentType != "application/json" {
		t.Errorf("content-type = %q", c.contentType)
	}
	if c.idem == "" {
		t.Error("no Idempotency-Key header")
	}
	b := c.body
	if b["message"] != Message(2) {
		t.Errorf("message = %v", b["message"])
	}
	if b["agentId"] != "main" || b["name"] != "Agent Tincan" || b["deliver"] != false || b["source"] != "agent-tincan" {
		t.Errorf("body = %v", b)
	}
	for _, unwanted := range []string{"sessionKey", "sessionMode", "channel", "to", "wakeMode"} {
		if _, ok := b[unwanted]; ok {
			t.Errorf("body sets %s; the gateway defaults (isolated session, no destination) are what we want: %v", unwanted, b)
		}
	}
}

// A retry after a gateway error reuses the Idempotency-Key, so a lost
// response never starts a second turn for the same nudge; the next nudge
// gets a fresh key.
func TestOpenClawRetryReusesIdempotencyKey(t *testing.T) {
	h := &openclawHook{token: "tok"}
	ts := h.server(t)
	h.fail.Store(1)
	w := New(Config{"claw": {Method: Webhook, Format: FormatOpenClaw, URL: ts.URL + "/hooks/agent", BearerToken: "tok"}}, nil, Options{Debounce: time.Millisecond, RetryDelay: time.Millisecond})
	queued(w, "claw", 1)
	w.Flush()
	queued(w, "claw", 1)
	w.Flush()
	calls := h.snapshot()
	if len(calls) != 3 {
		t.Fatalf("hook calls = %d, want 3 (fail, retry, next nudge)", len(calls))
	}
	if calls[0].idem == "" || calls[0].idem != calls[1].idem {
		t.Errorf("retry key %q != first key %q", calls[1].idem, calls[0].idem)
	}
	if calls[2].idem == calls[0].idem {
		t.Errorf("next nudge reused key %q", calls[2].idem)
	}
	if _, ok := calls[0].body["agentId"]; ok {
		t.Errorf("agentId sent without agent_id configured: %v", calls[0].body)
	}
}

// deliver: true opts back in to OpenClaw's completion announcement.
func TestOpenClawDeliverOptIn(t *testing.T) {
	h := &openclawHook{token: "tok"}
	ts := h.server(t)
	yes := true
	w := New(Config{"claw": {Method: Webhook, Format: FormatOpenClaw, URL: ts.URL + "/hooks/agent", BearerToken: "tok", Deliver: &yes}}, nil, Options{Debounce: time.Millisecond})
	queued(w, "claw", 1)
	w.Flush()
	if calls := h.snapshot(); len(calls) != 1 || calls[0].body["deliver"] != true {
		t.Fatalf("calls = %+v", calls)
	}
}

// A reply-only wake carries the reply count, not a request count.
func TestOpenClawReplyWakeMessage(t *testing.T) {
	h := &openclawHook{token: "tok"}
	ts := h.server(t)
	w := New(Config{"claw": {Method: Webhook, Format: FormatOpenClaw, URL: ts.URL + "/hooks/agent", BearerToken: "tok"}}, nil,
		Options{Debounce: time.Millisecond, ReplyGrace: time.Millisecond, ReplyRetries: []time.Duration{}})
	w.ReplyWaiting("claw")
	w.Flush()
	if calls := h.snapshot(); len(calls) != 1 || calls[0].body["message"] != WaitingMessage(0, 1) {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestLoadConfigOpenClawFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "wake.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"claw":{"method":"webhook","format":"openclaw","url":"http://h:18789/hooks/agent","bearer_token":"t","agent_id":"main","deliver":true}}`)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c["claw"]; got.Format != FormatOpenClaw || got.AgentID != "main" || got.Deliver == nil || !*got.Deliver {
		t.Fatalf("parsed %+v", got)
	}
	for body, want := range map[string]string{
		`{"claw":{"method":"webhook","format":"openclaw","url":"http://h/hooks/agent"}}`:                                      "bearer_token",
		`{"claw":{"method":"webhook","format":"openclaw","url":"http://h/hooks/agent","bearer_token":"t","hmac_secret":"s"}}`: "hmac_secret",
		`{"claw":{"method":"webhook","format":"openclaw","url":"http://h/hooks/agent?token=t","bearer_token":"t"}}`:           "query",
		`{"claw":{"method":"webhook","format":"nope","url":"http://h/x"}}`:                                                    "format",
		`{"claw":{"method":"email","format":"openclaw","email_to":"a","agentmail_inbox":"b","agentmail_key":"c"}}`:            "format",
	} {
		write(body)
		if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadConfig(%s) = %v, want error naming %q", body, err, want)
		}
	}
}
