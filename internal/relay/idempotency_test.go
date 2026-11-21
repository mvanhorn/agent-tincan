package relay

// These tests speak only the wire format, so they also compile against a
// relay that predates idempotency keys and fail there on behavior.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// keyedBody is a send of body to to under key.
func keyedBody(to, body, key string) string {
	return `{"to":"` + to + `","body":"` + body + `","idempotency_key":"` + key + `"}`
}

// sendKeyed sends body to to from addr under key and expects want.
func (h *harness) sendKeyed(addr, to, body, key string, want int) envelope.SendResponse {
	h.t.Helper()
	var out envelope.SendResponse
	h.do(addr, "POST", "/v1/send", keyedBody(to, body, key), want, &out)
	return out
}

// delivered polls addr once and returns the requests it was handed.
func (h *harness) delivered(addr string) []envelope.Request {
	h.t.Helper()
	req := httptest.NewRequest("GET", "/v1/poll?hold=0", nil)
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	switch rec.Code {
	case http.StatusNoContent:
		return nil
	case http.StatusOK:
	default:
		h.t.Fatalf("poll from %s: status %d: %s", addr, rec.Code, rec.Body.String())
	}
	var got pollResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		h.t.Fatal(err)
	}
	return got.Requests
}

// sentBy counts the requests the store holds from sender.
func (h *harness) sentBy(sender string) int {
	h.t.Helper()
	var n int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM requests WHERE from_agent = ?`, sender).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// TestSendIdempotencyKeyReplayReturnsOriginal: a send retried under the same
// key, after a timeout or a dropped connection lost the relay's answer,
// queues nothing new. The first send answers 201; each retry answers 200
// with the original request, and the recipient is handed exactly one.
func TestSendIdempotencyKeyReplayReturnsOriginal(t *testing.T) {
	h := newHarness(t, Config{})
	first := h.sendKeyed(grokAddr, "instinct", "deploy build 42", "deploy-42", http.StatusCreated)
	if first.ID == "" {
		t.Fatalf("first send returned no id: %+v", first)
	}
	// The retries' codes are recorded rather than checked one by one, so
	// the count of requests the recipient is handed is asserted first.
	var codes []int
	var retries []envelope.SendResponse
	for range 3 {
		req := httptest.NewRequest("POST", "/v1/send", strings.NewReader(keyedBody("instinct", "deploy build 42", "deploy-42")))
		req.RemoteAddr = grokAddr
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		var out envelope.SendResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("retry: status %d: %s", rec.Code, rec.Body.String())
		}
		codes, retries = append(codes, rec.Code), append(retries, out)
	}
	if got := h.delivered(instinctAddr); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("one send and 3 retries under one key: recipient was handed %d requests, want only the original %s", len(got), first.ID)
	}
	for i, again := range retries {
		if codes[i] != http.StatusOK {
			t.Fatalf("retry %d: status %d, want 200", i, codes[i])
		}
		if again.ID != first.ID || again.TraceID != first.TraceID || !again.CreatedAt.Equal(first.CreatedAt) ||
			again.Body != "deploy build 42" || again.To != "instinct" || again.From != "grokbot" || again.Status != envelope.StatusQueued {
			t.Fatalf("retry %d returned %+v, want the queued original %+v", i, again.Request, first.Request)
		}
	}
}

// TestSendIdempotencyKeyDifferentSendConflicts: a key names one send. A
// later send under it that asks for anything else is refused with 409 and
// the original's id, rather than answered with the original, which would
// drop the new send while telling the caller it worked. Nothing is queued,
// and an exact retry still gets the original.
func TestSendIdempotencyKeyDifferentSendConflicts(t *testing.T) {
	h, _ := attachHarness(t, Config{})
	a := h.upload(grokAddr, "a.png", "image/png", pngBytes)
	b := h.upload(grokAddr, "b.png", "image/png", pngBytes)
	withKey := func(send string) string { return strings.TrimSuffix(send, "}") + `,"idempotency_key":"k1"}` }
	original := withKey(sendWith("instinct", "deploy", a.ID, b.ID))
	var first envelope.SendResponse
	h.do(grokAddr, "POST", "/v1/send", original, http.StatusCreated, &first)
	for name, send := range map[string]string{
		"body":              withKey(sendWith("instinct", "deploy again", a.ID, b.ID)),
		"to":                withKey(sendWith("muse", "deploy", a.ID, b.ID)),
		"fewer attachments": withKey(sendWith("instinct", "deploy", a.ID)),
		"attachment order":  withKey(sendWith("instinct", "deploy", b.ID, a.ID)),
		"kind":              withKey(strings.TrimSuffix(sendWith("instinct", "deploy", a.ID, b.ID), "}") + `,"kind":"notify"}`),
		"urgent":            withKey(strings.TrimSuffix(sendWith("instinct", "deploy", a.ID, b.ID), "}") + `,"urgent":true}`),
		"group":             withKey(strings.TrimSuffix(sendWith("instinct", "deploy", a.ID, b.ID), "}") + `,"group":"g1"}`),
		"parent_id":         withKey(strings.TrimSuffix(sendWith("instinct", "deploy", a.ID, b.ID), "}") + `,"parent_id":"other"}`),
	} {
		var out struct {
			Error string `json:"error"`
			ID    string `json:"id"`
		}
		h.do(grokAddr, "POST", "/v1/send", send, http.StatusConflict, &out)
		if out.ID != first.ID || !strings.Contains(out.Error, "idempotency key") {
			t.Fatalf("%s differs: got %+v, want a conflict naming the original %s", name, out, first.ID)
		}
	}
	if n := h.sentBy("grokbot"); n != 1 {
		t.Fatalf("store holds %d requests from grokbot after conflicting sends, want 1", n)
	}
	// The same send as the relay reads it: an explicit kind ask is the
	// default kind, and an empty project is no project.
	var again envelope.SendResponse
	h.do(grokAddr, "POST", "/v1/send", strings.TrimSuffix(original, "}")+`,"kind":"ask","project":""}`, http.StatusOK, &again)
	if again.ID != first.ID || len(again.Attachments) != 2 {
		t.Fatalf("exact retry returned %+v, want the original %+v", again.Request, first.Request)
	}
}

// TestSendWithoutIdempotencyKeyUnchanged: a send without a key is a new
// request every time, as before.
func TestSendWithoutIdempotencyKeyUnchanged(t *testing.T) {
	h := newHarness(t, Config{})
	a := h.send(grokAddr, "instinct", "same body")
	b := h.send(grokAddr, "instinct", "same body")
	var c envelope.SendResponse
	h.do(grokAddr, "POST", "/v1/send", `{"to":"instinct","body":"same body","idempotency_key":""}`, http.StatusCreated, &c)
	if a.ID == b.ID || b.ID == c.ID || a.ID == c.ID {
		t.Fatalf("three sends without a key share ids: %s %s %s", a.ID, b.ID, c.ID)
	}
	if got := h.delivered(instinctAddr); len(got) != 3 {
		t.Fatalf("recipient was handed %d requests, want 3", len(got))
	}
}

// TestSendIdempotencyKeyScopedToSender: the key is looked up under the
// sender WhoIs named. Another agent sending under the same key, even the
// same send, gets its own request: never the first agent's request, its id,
// or a conflict that would show the key is in use.
func TestSendIdempotencyKeyScopedToSender(t *testing.T) {
	h := newHarness(t, Config{})
	fromGrok := h.sendKeyed(grokAddr, "instinct", "deploy", "shared-key", http.StatusCreated)
	same := h.sendKeyed(museAddr, "instinct", "deploy", "shared-key", http.StatusCreated)
	other := h.sendKeyed(museAddr, "grokbot", "something else", "grok-key", http.StatusCreated)
	h.sendKeyed(grokAddr, "instinct", "deploy", "grok-key", http.StatusCreated)
	for _, got := range []envelope.SendResponse{same, other} {
		if got.ID == fromGrok.ID || got.From != "muse" {
			t.Fatalf("muse's send under a key grokbot used returned %+v", got.Request)
		}
	}
	if got := h.delivered(instinctAddr); len(got) != 3 {
		t.Fatalf("instinct was handed %d requests, want grokbot's 2 and muse's 1", len(got))
	}
	// Each sender's retry still finds its own request.
	if again := h.sendKeyed(museAddr, "instinct", "deploy", "shared-key", http.StatusOK); again.ID != same.ID {
		t.Fatalf("muse's retry returned %s, want its own %s", again.ID, same.ID)
	}
	if again := h.sendKeyed(grokAddr, "instinct", "deploy", "shared-key", http.StatusOK); again.ID != fromGrok.ID {
		t.Fatalf("grokbot's retry returned %s, want its own %s", again.ID, fromGrok.ID)
	}
}

// TestSendIdempotencyKeyReplayAfterAnswer: a retry after the recipient
// answered still returns the original, with its current status, and queues
// nothing.
func TestSendIdempotencyKeyReplayAfterAnswer(t *testing.T) {
	h := newHarness(t, Config{})
	first := h.sendKeyed(grokAddr, "instinct", "rows?", "ask-rows", http.StatusCreated)
	h.do(instinctAddr, "POST", "/v1/requests/"+first.ID+"/claim", "", http.StatusOK, nil)
	h.do(instinctAddr, "POST", "/v1/requests/"+first.ID+"/reply", `{"body":"3 rows"}`, http.StatusOK, nil)
	again := h.sendKeyed(grokAddr, "instinct", "rows?", "ask-rows", http.StatusOK)
	if again.ID != first.ID || again.Status != envelope.StatusAnswered {
		t.Fatalf("retry after the answer returned id %s status %q, want %s answered", again.ID, again.Status, first.ID)
	}
	if got := h.delivered(instinctAddr); len(got) != 0 {
		t.Fatalf("retry after the answer queued %+v", got)
	}
}

// TestSendIdempotencyKeyStaysOnRelay: the key is never returned or
// delivered, and a retry writes no second audit event.
func TestSendIdempotencyKeyStaysOnRelay(t *testing.T) {
	h := newHarness(t, Config{})
	first := h.do(grokAddr, "POST", "/v1/send", keyedBody("instinct", "deploy", "private-key-1"), http.StatusCreated, nil)
	var sent envelope.Request
	if err := json.Unmarshal(first.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []*httptest.ResponseRecorder{
		first,
		h.do(grokAddr, "POST", "/v1/send", keyedBody("instinct", "deploy", "private-key-1"), http.StatusOK, nil),
		h.do(grokAddr, "GET", "/v1/requests/"+sent.ID, "", http.StatusOK, nil),
		h.do(instinctAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil),
		h.do(instinctAddr, "GET", "/v1/requests/"+sent.ID, "", http.StatusOK, nil),
	} {
		if body := rec.Body.String(); strings.Contains(body, "private-key-1") || strings.Contains(body, "idempotency_key") {
			t.Fatalf("the key left the relay: %s", body)
		}
	}
	var queued int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM audit WHERE event = 'queued' AND request_id = ?`, sent.ID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("%d queued audit events for one send and its retry, want 1", queued)
	}
}

// TestSendIdempotencyKeyConcurrentRetries: retries racing each other create
// one request, and every one of them is told its id.
func TestSendIdempotencyKeyConcurrentRetries(t *testing.T) {
	h := newHarness(t, Config{})
	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	ids := make([]string, n)
	for i := range n {
		wg.Go(func() {
			req := httptest.NewRequest("POST", "/v1/send", strings.NewReader(keyedBody("instinct", "deploy", "race")))
			req.RemoteAddr = grokAddr
			rec := httptest.NewRecorder()
			h.h.ServeHTTP(rec, req)
			codes[i] = rec.Code
			var out envelope.Request
			if json.Unmarshal(rec.Body.Bytes(), &out) == nil {
				ids[i] = out.ID
			}
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
			t.Fatalf("send %d: status %d", i, codes[i])
		}
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("send %d returned id %q, want every send to return %q", i, ids[i], ids[0])
		}
	}
	if created != 1 {
		t.Fatalf("%d sends were created, want exactly 1", created)
	}
	if got := h.delivered(instinctAddr); len(got) != 1 {
		t.Fatalf("recipient was handed %d requests, want 1", len(got))
	}
}

// TestSendIdempotencyKeyRejectsBadKey: a key the relay would not store is
// refused, not ignored, so a client never believes a send is deduplicated
// when it is not.
func TestSendIdempotencyKeyRejectsBadKey(t *testing.T) {
	h := newHarness(t, Config{})
	for _, key := range []string{strings.Repeat("k", 129), "has space", `tab\tkey`, "slash/key", "caf" + string(rune(0xe9))} {
		h.do(grokAddr, "POST", "/v1/send", keyedBody("instinct", "hi", key), http.StatusBadRequest, nil)
	}
	for _, key := range []string{strings.Repeat("k", 128), "0f8fad5b-d9cb-469f-a165-70867728950e", "job:7.attempt_a"} {
		h.sendKeyed(grokAddr, "instinct", "hi", key, http.StatusCreated)
	}
	if n := h.sentBy("grokbot"); n != 3 {
		t.Fatalf("store holds %d requests from grokbot, want the 3 with good keys", n)
	}
}

// TestSendIdempotencyKeySurvivesRestart: the key is stored with the
// request, so a retry after the relay restarts still finds the original.
func TestSendIdempotencyKeySurvivesRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "relay.db")
	who := identitytest.New(map[string]identity.Node{
		macAddr:      {ID: "nMAC", Name: "macbook-pro-44"},
		grokAddr:     {ID: "nGROK", Name: "grok-bot"},
		instinctAddr: {ID: "nINST", Name: "instinct"},
	})
	start := func() *harness {
		st, err := store.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		srv := New(identity.NewDirectory(st, who, identity.Config{Admins: []string{"macbook-pro-44"}}), st, Config{})
		return &harness{t: t, srv: srv, h: srv.Handler(), st: st, who: who}
	}
	h := start()
	h.joinAt(grokAddr, "grokbot")
	h.joinAt(instinctAddr, "instinct")
	first := h.sendKeyed(grokAddr, "instinct", "deploy build 42", "deploy-42", http.StatusCreated)
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}

	h = start()
	t.Cleanup(func() { h.st.Close() })
	if again := h.sendKeyed(grokAddr, "instinct", "deploy build 42", "deploy-42", http.StatusOK); again.ID != first.ID {
		t.Fatalf("retry after a restart returned %s, want the original %s", again.ID, first.ID)
	}
	if got := h.delivered(instinctAddr); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("recipient was handed %+v after the restart, want only the original", got)
	}
}

// TestCapabilitiesAdvertiseIdempotencyKey: a client can tell a relay that
// honors the key from an older one that would ignore it.
func TestCapabilitiesAdvertiseIdempotencyKey(t *testing.T) {
	h := newHarness(t, Config{})
	var caps map[string]any
	h.do(grokAddr, "GET", "/v1/capabilities", "", http.StatusOK, &caps)
	if caps["idempotency_key"] != true {
		t.Fatalf("capabilities %+v do not advertise idempotency_key", caps)
	}
}
