package wake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// idsFake stands in for the relay's queued-request and unseen-reply id
// queries.
type idsFake struct {
	mu      sync.Mutex
	queued  []string
	replies []string
	err     error
}

func (f *idsFake) queuedIDs(string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queued...), f.err
}

func (f *idsFake) replyIDs(string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.replies...), f.err
}

// wokeRows is every woke row in the audit log, oldest first.
func wokeRows(t *testing.T, st *store.Store) []store.AuditEvent {
	t.Helper()
	evs, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AuditEvent
	for _, e := range evs {
		if e.Event == "woke" {
			out = append(out, e)
		}
	}
	return out
}

// idWaker is a webhook waker for grokbot with the id hooks wired to ids.
func idWaker(t *testing.T, st *store.Store, ids *idsFake, unseen int) *Waker {
	t.Helper()
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{
		Debounce: time.Millisecond, RetryDelay: time.Millisecond, ReplyGrace: time.Millisecond,
		ReplyRetries: []time.Duration{}, WakeGrace: skipFollowUp,
		UnseenReplies: func(string) int { return unseen },
		QueuedIDs:     ids.queuedIDs, UnseenReplyIDs: ids.replyIDs,
	})
	t.Cleanup(w.Stop)
	return w
}

const okResponse = `, response: {"ok":true}`

func oneWoke(t *testing.T, st *store.Store) store.AuditEvent {
	t.Helper()
	rows := wokeRows(t, st)
	if len(rows) != 1 {
		t.Fatalf("woke rows = %+v, want one", rows)
	}
	return rows[0]
}

// One queued request wakes the agent: the woke row carries its id in
// request_id, and the detail keeps its old form with no ids segment.
func TestWokeRowNamesTheOneRequest(t *testing.T) {
	st := auditStore(t)
	w := idWaker(t, st, &idsFake{queued: []string{"ra"}}, 0)
	queued(w, "grokbot", 1)
	w.Flush()
	row := oneWoke(t, st)
	if row.RequestID != "ra" || row.Detail != "webhook, HTTP 200, 1 waiting"+okResponse {
		t.Fatalf("woke row = request_id %q, detail %q", row.RequestID, row.Detail)
	}
}

// One unseen reply wakes the asker: request_id is the replied request's id.
func TestWokeRowNamesTheOneReply(t *testing.T) {
	st := auditStore(t)
	w := idWaker(t, st, &idsFake{replies: []string{"q7"}}, 1)
	w.Replied(context.Background(), envelope.Request{ID: "q7", From: "grokbot", To: "muse"})
	w.Flush()
	row := oneWoke(t, st)
	if row.RequestID != "q7" || row.Detail != "webhook, HTTP 200, 0 waiting, 1 unseen replies"+okResponse {
		t.Fatalf("woke row = request_id %q, detail %q", row.RequestID, row.Detail)
	}
}

// Two requests and one reply: no single id, so request_id stays empty and
// the detail lists all three, requests first, before the response.
func TestWokeRowListsSeveralIDs(t *testing.T) {
	st := auditStore(t)
	w := idWaker(t, st, &idsFake{queued: []string{"ra", "rb"}, replies: []string{"q7"}}, 1)
	queued(w, "grokbot", 2)
	w.Flush()
	row := oneWoke(t, st)
	if row.RequestID != "" || row.Detail != "webhook, HTTP 200, 2 waiting, 1 unseen replies, ids: ra,rb,q7"+okResponse {
		t.Fatalf("woke row = request_id %q, detail %q", row.RequestID, row.Detail)
	}
}

// A request delivered between scheduling and sending is not listed, and the
// "N waiting" count is the ids listed, not the requests queued since the
// nudge was scheduled.
func TestWokeRowCountsWhatIsStillWaiting(t *testing.T) {
	st := auditStore(t)
	w := idWaker(t, st, &idsFake{queued: []string{"rb"}}, 0)
	queued(w, "grokbot", 2) // ra was delivered before the wake went out
	w.Flush()
	row := oneWoke(t, st)
	if row.RequestID != "rb" || row.Detail != "webhook, HTTP 200, 1 waiting"+okResponse {
		t.Fatalf("woke row = request_id %q, detail %q", row.RequestID, row.Detail)
	}
}

// Every waiting id is listed, with no cap.
func TestWokeRowListsEveryID(t *testing.T) {
	st := auditStore(t)
	var all []string
	for i := range 30 {
		all = append(all, fmt.Sprintf("req%02d", i))
	}
	w := idWaker(t, st, &idsFake{queued: all}, 0)
	queued(w, "grokbot", 30)
	w.Flush()
	row := oneWoke(t, st)
	want := "webhook, HTTP 200, 30 waiting, ids: " + strings.Join(all, ",") + okResponse
	if row.RequestID != "" || row.Detail != want {
		t.Fatalf("woke row = request_id %q, detail %q", row.RequestID, row.Detail)
	}
}

// When the id lookup fails, the row keeps the nudge's count and names no
// ids, as before ids were recorded.
func TestWokeRowWithoutIDsWhenLookupFails(t *testing.T) {
	st := auditStore(t)
	w := idWaker(t, st, &idsFake{queued: []string{"ra"}, err: errors.New("db down")}, 0)
	queued(w, "grokbot", 2)
	w.Flush()
	row := oneWoke(t, st)
	if row.RequestID != "" || row.Detail != "webhook, HTTP 200, 2 waiting"+okResponse {
		t.Fatalf("woke row = request_id %q, detail %q", row.RequestID, row.Detail)
	}
}

// The audit chain verifies over a mix of woke rows written before ids were
// recorded and rows with a request_id or an ids segment.
func TestAuditVerifiesOldAndNewWokeRows(t *testing.T) {
	st := auditStore(t)
	if err := st.Audit(context.Background(), store.AuditEvent{Event: "woke", Actor: "grokbot", Detail: "webhook, HTTP 200, 1 waiting"}); err != nil {
		t.Fatal(err)
	}
	ids := &idsFake{queued: []string{"ra"}}
	w := idWaker(t, st, ids, 0)
	queued(w, "grokbot", 1)
	w.Flush()
	ids.mu.Lock()
	ids.queued = []string{"ra", "rb"}
	ids.mu.Unlock()
	queued(w, "grokbot", 2)
	w.Flush()
	rows := wokeRows(t, st)
	if len(rows) != 3 || rows[1].RequestID != "ra" || !strings.Contains(rows[2].Detail, ", ids: ra,rb") {
		t.Fatalf("woke rows = %+v", rows)
	}
	if n, err := st.VerifyAudit(context.Background()); err != nil || n != 3 {
		t.Fatalf("verify = %d, %v", n, err)
	}
}

// A request-email wake records the asks it emailed the same way: one ask in
// request_id, several in the ids segment, after any other queued request
// the count-only email told of.
func TestRequestEmailWokeRowNamesEmailedAsks(t *testing.T) {
	at := time.Unix(1_790_000_000, 0)
	for _, tc := range []struct {
		name      string
		asks      []envelope.Request
		queued    []string
		requestID string
		detail    string
	}{
		{"one ask", []envelope.Request{ask("ra", "muse", "book the boat", at)}, []string{"ra"},
			"ra", "email, HTTP 200, 1 open asks, 1 request emails"},
		{"two asks and a ping", []envelope.Request{ask("ra", "muse", "book the boat", at), ask("rb", "muse", "call the yard", at.Add(time.Second))}, []string{"ra", "rb", "rp"},
			"", "email, HTTP 200, 2 open asks, 2 request emails, 1 other requests waiting, ids: ra,rb,rp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rc recorder
			ts := rc.server(t)
			st := auditStore(t)
			asks := &asksFake{asks: tc.asks}
			ids := &idsFake{queued: tc.queued}
			w := New(Config{"instinct": optedIn()}, st, Options{
				Debounce: time.Millisecond, WakeGrace: skipFollowUp, AgentMailAPI: ts.URL + "/v0",
				OpenAsks: asks.open, RequestTag: fakeTag,
				Queued:    func(string) int { return len(tc.queued) },
				QueuedIDs: ids.queuedIDs, UnseenReplyIDs: ids.replyIDs,
			})
			t.Cleanup(w.Stop)
			for _, r := range tc.asks {
				w.Queued(context.Background(), r)
			}
			w.Flush()
			row := oneWoke(t, st)
			if row.RequestID != tc.requestID || row.Detail != tc.detail {
				t.Fatalf("woke row = request_id %q, detail %q", row.RequestID, row.Detail)
			}
		})
	}
}
