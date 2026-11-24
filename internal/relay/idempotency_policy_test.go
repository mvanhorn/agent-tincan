package relay_test

// These tests need the real policy, which imports relay, so they live apart
// from idempotency_test.go.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// postSend posts body to the relay as the agent behind url and decodes the
// answer.
func postSend(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url+"/v1/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

// requestEvents counts the audit rows of event for request id.
func requestEvents(t *testing.T, m *testrelay.Mesh, event, id string) int {
	t.Helper()
	var n int
	if err := m.Store.DB().QueryRow(`SELECT COUNT(*) FROM audit WHERE event = ? AND request_id = ?`, event, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSendIdempotencyKeyReplayWhileHeld: a retry of a keyed send that is
// held for the owner's approval gets the held request back, answered as the
// first send was, and does not hold it again or notify the approver twice.
func TestSendIdempotencyKeyReplayWhileHeld(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}},"notify":"instinct","hold_ttl":"1h"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	const send = `{"to":"muse","body":"deploy build 42","idempotency_key":"held-1"}`
	code, first := postSend(t, m.URL("grokbot"), send)
	if code != http.StatusCreated || first["status"] != string(envelope.StatusHeld) {
		t.Fatalf("first send to a gated agent: %d %v, want 201 held", code, first)
	}
	code, again := postSend(t, m.URL("grokbot"), send)
	if code != http.StatusOK || again["id"] != first["id"] || again["status"] != string(envelope.StatusHeld) {
		t.Fatalf("retry of a held send: %d %v, want 200 with the held original %v", code, again, first)
	}
	if _, had := first["target"]; had != (again["target"] != nil) {
		t.Fatalf("retry target %v, first send's %v: a retry is answered as the first send was", again["target"], first["target"])
	}
	id := first["id"].(string)
	for _, event := range []string{"held", "approval_notified"} {
		if n := requestEvents(t, m, event, id); n != 1 {
			t.Fatalf("%d %s events for one held send and its retry, want 1", n, event)
		}
	}
	notices, err := m.Client(t, "instinct").Poll(t.Context(), time.Millisecond)
	if err != nil || len(notices.Requests) != 1 || !strings.Contains(notices.Requests[0].Body, id) {
		t.Fatalf("approver was handed %+v (%v), want one notice for %s", notices.Requests, err, id)
	}
	if n := m.Server.QueuedCount("muse"); n != 0 {
		t.Fatalf("muse has %d queued, want the send still held", n)
	}
}

// TestSendIdempotencyKeyConcurrentRetriesUnderRealPolicy: retries racing
// under a rate limit of one send a minute all get the one request, none a
// 429: only the first reaches the rate limit.
func TestSendIdempotencyKeyConcurrentRetriesUnderRealPolicy(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{PerMinute: 1}))
	const n = 16
	codes := make([]int, n)
	ids := make([]any, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			var out map[string]any
			codes[i], out = postSend(t, m.URL("grokbot"), `{"to":"instinct","body":"deploy","idempotency_key":"race-policy"}`)
			ids[i] = out["id"]
		})
	}
	wg.Wait()
	created := 0
	for i := range n {
		switch codes[i] {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Fatalf("send %d: status %d, want 201 or 200, not a rate-limit refusal", i, codes[i])
		}
		if ids[i] == nil || ids[i] != ids[0] {
			t.Fatalf("send %d returned id %v, want %v", i, ids[i], ids[0])
		}
	}
	if created != 1 {
		t.Fatalf("%d sends were created, want 1", created)
	}
	if got := m.Server.QueuedCount("instinct"); got != 1 {
		t.Fatalf("instinct has %d queued, want 1", got)
	}
}
