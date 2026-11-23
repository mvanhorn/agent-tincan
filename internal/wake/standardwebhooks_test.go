package wake

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/webhooksign"
)

// The Standard Webhooks spec's reference secret. Its key decodes to 24 bytes.
const swSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"

// swHook is a Standard Webhooks receiver the way an OpenBot routine
// webhook checks one: a valid v1 signature over id.timestamp.body, and a
// JSON object body with a string type. Anything else is a 400 or 401.
type swHook struct {
	secret string
	fail   atomic.Int32 // answer 503 (OpenBot host offline) this many times
	answer string       // 2xx body; default {"ok":true}

	mu    sync.Mutex
	calls []swCall
}

type swCall struct {
	id, ts, sig string
	verified    bool
	raw         []byte
	body        map[string]any
}

func (h *swHook) server(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := swCall{id: r.Header.Get("webhook-id"), ts: r.Header.Get("webhook-timestamp"), sig: r.Header.Get("webhook-signature"), raw: raw}
		unix, err := strconv.ParseInt(c.ts, 10, 64)
		c.verified = err == nil && webhooksign.Verify(h.secret, c.id, unix, raw, c.sig)
		_ = json.Unmarshal(raw, &c.body)
		h.mu.Lock()
		h.calls = append(h.calls, c)
		h.mu.Unlock()
		if !c.verified {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		if typ, ok := c.body["type"].(string); !ok || typ == "" || len(raw) > 64<<10 {
			http.Error(w, "body must be a JSON object with a string type", http.StatusBadRequest)
			return
		}
		if h.fail.Load() > 0 {
			h.fail.Add(-1)
			http.Error(w, "host offline", http.StatusServiceUnavailable)
			return
		}
		answer := h.answer
		if answer == "" {
			answer = `{"ok":true}`
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(answer))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (h *swHook) snapshot() []swCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

func swTarget(u string) Target {
	return Target{Method: Webhook, Format: FormatStandardWebhooks, URL: u, SigningSecret: swSecret}
}

// The posted body is a signed JSON object with a string type, the counts
// and the covered request ids, and nothing from the requests themselves:
// no text, sender or kind.
func TestStandardWebhooksBodyIsSignedMetadata(t *testing.T) {
	h := &swHook{secret: swSecret}
	ts := h.server(t)
	ids := &idsFake{queued: []string{"req-1", "req-2"}, replies: []string{"req-9"}}
	w := New(Config{"bot": swTarget(ts.URL + "/v1/webhooks/x")}, nil, Options{
		Debounce: time.Millisecond, WakeGrace: skipFollowUp, ReplyRetries: []time.Duration{},
		UnseenReplies: func(string) int { return 1 },
		QueuedIDs:     ids.queuedIDs, UnseenReplyIDs: ids.replyIDs,
	})
	t.Cleanup(w.Stop)
	queued(w, "bot", 2)
	w.Flush()
	calls := h.snapshot()
	if len(calls) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if !c.verified {
		t.Fatalf("signature %q over id %q ts %q did not verify", c.sig, c.id, c.ts)
	}
	if !strings.HasPrefix(c.id, "agent-tincan-") {
		t.Errorf("webhook-id = %q, want the nudge key", c.id)
	}
	if c.body["type"] != "tincan.request" {
		t.Errorf("type = %v", c.body["type"])
	}
	if c.body["waiting"] != 2.0 || c.body["unseen_replies"] != 1.0 || c.body["urgent"] != 0.0 {
		t.Errorf("counts = %v", c.body)
	}
	if got := c.body["request_ids"]; !slices.Equal(anyStrings(got), []string{"req-1", "req-2"}) {
		t.Errorf("request_ids = %v", got)
	}
	if got := c.body["reply_request_ids"]; !slices.Equal(anyStrings(got), []string{"req-9"}) {
		t.Errorf("reply_request_ids = %v", got)
	}
	for _, k := range []string{"message", "text", "body", "from", "sender", "kind"} {
		if _, ok := c.body[k]; ok {
			t.Errorf("body has %q: %s", k, c.raw)
		}
	}
	for _, leak := range []string{"SECRET", "Joe's Garage", "555-0100", "check_inbox"} {
		if strings.Contains(string(c.raw), leak) {
			t.Errorf("body carries request or message text %q: %s", leak, c.raw)
		}
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// A 503 (OpenBot host offline) and the in-call retry share one webhook-id,
// so OpenBot sees one delivery; the next nudge gets a new id, so a re-wake
// is not deduped away.
func TestStandardWebhooksRetryReusesWebhookID(t *testing.T) {
	h := &swHook{secret: swSecret}
	ts := h.server(t)
	h.fail.Store(1)
	w := New(Config{"bot": swTarget(ts.URL)}, nil, Options{Debounce: time.Millisecond, RetryDelay: time.Millisecond})
	t.Cleanup(w.Stop)
	queued(w, "bot", 1)
	w.Flush()
	queued(w, "bot", 1)
	w.Flush()
	calls := h.snapshot()
	if len(calls) != 3 {
		t.Fatalf("hook calls = %d, want 3 (503, retry, next nudge)", len(calls))
	}
	for i, c := range calls {
		if !c.verified {
			t.Errorf("call %d signature did not verify", i)
		}
	}
	if calls[0].id == "" || calls[0].id != calls[1].id {
		t.Errorf("retry webhook-id %q != first %q", calls[1].id, calls[0].id)
	}
	if calls[2].id == calls[0].id {
		t.Errorf("next nudge reused webhook-id %q", calls[2].id)
	}
}

// A request follow-up (the relay re-waking a still-queued request) is a
// new nudge with a new webhook-id.
func TestStandardWebhooksFollowUpGetsNewWebhookID(t *testing.T) {
	h := &swHook{secret: swSecret}
	ts := h.server(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"bot": swTarget(ts.URL)}, nil, Options{
		Debounce:   time.Millisecond,
		RetryDelay: time.Millisecond,
		WakeGrace:  20 * time.Millisecond,
		Queued:     func(string) int { return int(queuedN.Load()) },
		LastPoll:   func(string) time.Time { return time.Time{} },
	})
	t.Cleanup(w.Stop)
	queued(w, "bot", 1)
	deadline := time.Now().Add(5 * time.Second)
	for len(h.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	queuedN.Store(0)
	w.Flush()
	calls := h.snapshot()
	if len(calls) < 2 {
		t.Fatalf("hook calls = %d, want the wake and a follow-up", len(calls))
	}
	if calls[1].id == calls[0].id {
		t.Errorf("follow-up reused webhook-id %q", calls[1].id)
	}
}

// A 2xx answer that echoes the signing secret, its key or the signature
// does not put them in the woke row.
func TestStandardWebhooksAnswerRedactsSecret(t *testing.T) {
	key := strings.TrimPrefix(swSecret, "whsec_")
	h := &swHook{secret: swSecret}
	ts := h.server(t)
	st := auditStore(t)
	w := New(Config{"bot": swTarget(ts.URL)}, st, Options{Debounce: time.Millisecond, WakeGrace: skipFollowUp})
	t.Cleanup(w.Stop)
	// The answer echoes the secret and its bare key.
	h.answer = `{"secret":"` + swSecret + `","key":"` + key + `"}`
	queued(w, "bot", 1)
	w.Flush()
	rows := wokeRows(t, st)
	if len(rows) != 1 {
		t.Fatalf("woke rows = %+v", rows)
	}
	if want := `response: {"secret":"[redacted]","key":"[redacted]"}`; !strings.HasSuffix(rows[0].Detail, want) {
		t.Errorf("detail = %q, want it to end %q", rows[0].Detail, want)
	}
	if strings.Contains(rows[0].Detail, key) {
		t.Errorf("detail leaks the key: %q", rows[0].Detail)
	}

	// webhookSecrets names the secret, its key and the decoded key, and
	// send adds the signature it computed.
	u, _ := url.Parse(ts.URL)
	secrets := webhookSecrets(swTarget(ts.URL), u)
	decoded, _ := webhooksign.ParseSecret(swSecret)
	for _, want := range []string{swSecret, key, string(decoded)} {
		if !slices.Contains(secrets, want) {
			t.Errorf("webhookSecrets misses %q", want)
		}
	}
	// Each send is signed afresh, so a receiver that echoes the signature
	// it got stands in for one that echoes the signature back.
	ts2 := httptest.NewServer(sigEcho{})
	t.Cleanup(ts2.Close)
	w2 := New(Config{"bot": swTarget(ts2.URL)}, st, Options{Debounce: time.Millisecond, WakeGrace: skipFollowUp})
	t.Cleanup(w2.Stop)
	queued(w2, "bot", 1)
	w2.Flush()
	rows = wokeRows(t, st)
	if len(rows) != 2 {
		t.Fatalf("woke rows = %+v", rows)
	}
	if want := `response: {"sig":"[redacted]","mac":"[redacted]"}`; !strings.HasSuffix(rows[1].Detail, want) {
		t.Errorf("detail = %q, want it to end %q", rows[1].Detail, want)
	}
}

// sigEcho answers 202 with the signature it was sent, whole and as its
// bare base64 part.
type sigEcho struct{}

func (sigEcho) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sig := r.Header.Get("webhook-signature")
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"sig":"` + sig + `","mac":"` + strings.TrimPrefix(sig, "v1,") + `"}`))
}

func TestLoadConfigStandardWebhooksFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "wake.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"bot":{"method":"webhook","format":"standard-webhooks","url":"https://signal.openbot.run/v1/webhooks/u","signing_secret":"` + swSecret + `"}}`)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c["bot"]; got.Format != FormatStandardWebhooks || got.SigningSecret != swSecret {
		t.Fatalf("parsed %+v", got)
	}
	const u = `"url":"https://signal.openbot.run/v1/webhooks/u"`
	for body, want := range map[string]string{
		`{"bot":{"method":"webhook","format":"standard-webhooks",` + u + `}}`:                                                                                  "needs signing_secret",
		`{"bot":{"method":"webhook","format":"standard-webhooks",` + u + `,"signing_secret":"MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"}}`:                              "signing_secret must start with whsec_",
		`{"bot":{"method":"webhook","format":"standard-webhooks",` + u + `,"signing_secret":"whsec_not base64!!"}}`:                                            "signing_secret: invalid webhook signing secret: base64",
		`{"bot":{"method":"webhook","format":"standard-webhooks",` + u + `,"signing_secret":"` + swSecret + `","bearer_token":"t"}}`:                           "bearer_token",
		`{"bot":{"method":"webhook","format":"standard-webhooks",` + u + `,"signing_secret":"` + swSecret + `","hmac_secret":"s"}}`:                            "hmac_secret",
		`{"bot":{"method":"webhook",` + u + `,"signing_secret":"` + swSecret + `"}}`:                                                                           "signing_secret is only used by format standard-webhooks",
		`{"bot":{"method":"email","format":"standard-webhooks","email_to":"a","agentmail_inbox":"b","agentmail_key":"c","signing_secret":"` + swSecret + `"}}`: "format",
	} {
		write(body)
		_, err := LoadConfig(p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadConfig(%s) = %v, want error naming %q", body, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "MfKQ9r8") {
			t.Errorf("error quotes the secret: %v", err)
		}
	}
}

// TestStandardWebhooksBodyStaysUnder64KiB: OpenBot refuses a body over
// 64 KiB, so a big backlog lists at most maxBodyIDs ids of each kind while
// the counts stay exact, and unknown ids are an empty list, not null.
func TestStandardWebhooksBodyStaysUnder64KiB(t *testing.T) {
	long := func(prefix string, n int) []string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = prefix + strings.Repeat("x", 120) + strconv.Itoa(i)
		}
		return ids
	}
	raw := standardWebhooksBody(wakeNote{waiting: 5000, urgent: 3, unseen: 5000, requestIDs: long("q", 5000), replyIDs: long("r", 5000)})
	if len(raw) >= 64<<10 {
		t.Fatalf("body is %d bytes, want under 64 KiB", len(raw))
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if got := len(anyStrings(body["request_ids"])); got != maxBodyIDs {
		t.Errorf("request_ids lists %d, want %d", got, maxBodyIDs)
	}
	if got := len(anyStrings(body["reply_request_ids"])); got != maxBodyIDs {
		t.Errorf("reply_request_ids lists %d, want %d", got, maxBodyIDs)
	}
	if body["waiting"] != 5000.0 || body["unseen_replies"] != 5000.0 || body["urgent"] != 3.0 {
		t.Errorf("counts = %v, want the exact backlog", body)
	}

	raw = standardWebhooksBody(wakeNote{waiting: 1})
	if !strings.Contains(string(raw), `"request_ids":[]`) || !strings.Contains(string(raw), `"reply_request_ids":[]`) {
		t.Errorf("unknown ids: body %s, want empty lists", raw)
	}
}
