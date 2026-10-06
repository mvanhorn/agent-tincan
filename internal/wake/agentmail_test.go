package wake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The inbox calls send the key only as a bearer header, list received mail
// only (never spam or unauthenticated), follow page tokens, and put an
// Idempotency-Key on a reply.
func TestAgentMailCalls(t *testing.T) {
	var mu sync.Mutex
	var seen []*http.Request
	var replyBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(context.Background()))
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/inboxes/relay@agentmail.to/messages":
			if r.URL.Query().Get("page_token") == "" {
				w.Write([]byte(`{"count":1,"messages":[{"message_id":"<a@x>","from":"Instinct <i@example.com>","subject":"s1","labels":["received"],"timestamp":"2026-10-06T18:00:00Z"}],"next_page_token":"p2"}`))
				return
			}
			w.Write([]byte(`{"count":1,"messages":[{"message_id":"<b@x>","subject":"s2","labels":["received"]}]}`))
		case r.Method == "GET" && r.URL.Path == "/inboxes/relay@agentmail.to/messages/<a@x>":
			w.Write([]byte(`{"message_id":"<a@x>","extracted_text":"the answer","attachments":[{"attachment_id":"att1"}]}`))
		case r.Method == "POST" && r.URL.Path == "/inboxes/relay@agentmail.to/messages/<a@x>/reply":
			json.NewDecoder(r.Body).Decode(&replyBody)
			w.Write([]byte(`{"message_id":"<c@x>","thread_id":"t"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	am := NewAgentMail(srv.URL, "relay@agentmail.to", "am_KEY", srv.Client())
	ctx := t.Context()
	after := time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC)
	msgs, err := am.Received(ctx, after)
	if err != nil || len(msgs) != 2 || msgs[0].MessageID != "<a@x>" || msgs[1].MessageID != "<b@x>" || !msgs[0].HasLabel("received") {
		t.Fatalf("Received = %+v, %v", msgs, err)
	}
	if !msgs[0].Timestamp.Equal(time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)) || msgs[0].ExtractedText != nil {
		t.Fatalf("list item = %+v", msgs[0])
	}
	m, err := am.Message(ctx, "<a@x>")
	if err != nil || m.ExtractedText == nil || *m.ExtractedText != "the answer" || len(m.Attachments) != 1 {
		t.Fatalf("Message = %+v, %v", m, err)
	}
	if err := am.Reply(ctx, "<a@x>", "key-1", "recorded"); err != nil {
		t.Fatal(err)
	}
	if replyBody["text"] != "recorded" {
		t.Fatalf("reply body = %v", replyBody)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("calls = %d, want 4", len(seen))
	}
	for i, r := range seen {
		if r.Header.Get("Authorization") != "Bearer am_KEY" || strings.Contains(r.URL.String(), "am_KEY") {
			t.Fatalf("call %d: key outside the Authorization header", i)
		}
	}
	q := seen[0].URL.Query()
	if q.Get("labels") != "received" || q.Get("after") != "2026-10-05T18:00:00Z" || q.Has("include_spam") || q.Has("include_unauthenticated") {
		t.Fatalf("list query = %v", q)
	}
	if seen[1].URL.Query().Get("page_token") != "p2" {
		t.Fatalf("second page query = %v", seen[1].URL.Query())
	}
	if got := seen[3].Header.Get("Idempotency-Key"); got != "key-1" {
		t.Fatalf("Idempotency-Key = %q", got)
	}
	if seen[2].Header.Get("Idempotency-Key") != "" {
		t.Fatal("a read carried an Idempotency-Key")
	}
}

// A 429 is waited out for its Retry-After and retried; an error names
// only the host and status, never the inbox, the message or the key.
func TestAgentMailRateLimitAndErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/reply") {
			http.Error(w, "am_KEY leaked in body", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"messages":[]}`))
	}))
	defer srv.Close()
	am := NewAgentMail(srv.URL, "relay@agentmail.to", "am_KEY", srv.Client())
	var waits []time.Duration
	am.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	if _, err := am.Received(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != 2*time.Second || calls != 2 {
		t.Fatalf("waits = %v after %d calls, want one 2s wait", waits, calls)
	}
	err := am.Reply(t.Context(), "<secret-id@x>", "k", "text")
	if err == nil {
		t.Fatal("want an error for a 500")
	}
	for _, s := range []string{"am_KEY", "relay@agentmail.to", "secret-id", "leaked"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error %q names %q", err, s)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for h, want := range map[string]time.Duration{
		"2": 2 * time.Second, "": time.Second, "junk": time.Second, "0": time.Second, "100000": maxRetryAfter,
		now.Add(5 * time.Second).Format(http.TimeFormat): 5 * time.Second,
	} {
		if got := retryAfter(h, now); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", h, got, want)
		}
	}
}
