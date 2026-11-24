package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/webhooksign"
)

type capturedWebhook struct {
	Headers http.Header
	Body    []byte
	Payload map[string]any
}

func startWebhookRecorder(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte)) (*httptest.Server, *[]capturedWebhook, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var got []capturedWebhook
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		got = append(got, capturedWebhook{
			Headers: r.Header.Clone(),
			Body:    body,
			Payload: payload,
		})
		mu.Unlock()
		if handler != nil {
			handler(w, r, body)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &mu
}

func waitForCount(t *testing.T, got *[]capturedWebhook, mu *sync.Mutex, want int) []capturedWebhook {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		if len(*got) >= want {
			cp := append([]capturedWebhook(nil), (*got)...)
			mu.Unlock()
			return cp
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("timed out waiting for %d webhooks, got %d", want, len(*got))
	return nil
}

func TestEventHubMetadataOnlyAndFilters(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv, got, mu := startWebhookRecorder(t, nil)
	hub, err := NewEventHub(st.DB(), EventHubOptions{
		QueuedCount:  func(string) int { return 3 },
		PollInterval: 15 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Stop()
	AllowUnsafeCallbacksForTest(hub, srv.Client())

	secret, _ := webhooksign.GenerateSecret()
	ctx := context.Background()
	_, err = hub.store.CreateSubscription(ctx, EventSubscription{
		ID:            "sub_urgent",
		Agent:         "chatgpt",
		ClientID:      "c1",
		EventName:     EventInboxRequest,
		Filters:       map[string]any{"urgent_only": true},
		CallbackURL:   srv.URL,
		SigningSecret: secret,
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Held request must emit nothing.
	hub.Queued(ctx, envelope.Request{
		ID:     "req_held",
		From:   "codex",
		To:     "chatgpt",
		Urgent: true,
		Status: envelope.StatusHeld,
		Body:   "TOP_SECRET_HELD_BODY",
	})

	// Non-urgent request must be filtered out by urgent_only: true.
	hub.Queued(ctx, envelope.Request{
		ID:     "req_normal",
		From:   "codex",
		To:     "chatgpt",
		Urgent: false,
		Body:   "NON_URGENT_SECRET_BODY",
	})

	// Urgent request with sensitive body and attachment must be delivered metadata-only.
	hub.Queued(ctx, envelope.Request{
		ID:          "req_urgent_1",
		From:        "codex",
		To:          "chatgpt",
		Kind:        envelope.KindAsk,
		Urgent:      true,
		Body:        "SENSITIVE_REQUEST_TEXT_NEVER_LEAK",
		Attachments: []envelope.Attachment{{ID: "att_1", Name: "secret-plan.pdf", MIME: "application/pdf", Size: 1234}},
		CreatedAt:   time.Now().UTC(),
	})

	events := waitForCount(t, got, mu, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 webhook, got %d", len(events))
	}
	ev := events[0]
	raw := string(ev.Body)
	for _, forbidden := range []string{"SENSITIVE_REQUEST_TEXT_NEVER_LEAK", "secret-plan.pdf", "att_1", "TOP_SECRET_HELD_BODY", "NON_URGENT_SECRET_BODY"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("webhook payload leaked forbidden text %q: %s", forbidden, raw)
		}
	}

	// Verify Standard Webhooks headers and webhook-id == eventId.
	eventID, _ := ev.Payload["eventId"].(string)
	if eventID == "" || ev.Headers.Get("webhook-id") != eventID {
		t.Fatalf("webhook-id %q != eventId %q", ev.Headers.Get("webhook-id"), eventID)
	}
	if ev.Headers.Get("X-MCP-Subscription-Id") != "sub_urgent" {
		t.Fatalf("X-MCP-Subscription-Id = %q, want sub_urgent", ev.Headers.Get("X-MCP-Subscription-Id"))
	}
	ts, _ := strconv.ParseInt(ev.Headers.Get("webhook-timestamp"), 10, 64)
	if !webhooksign.Verify(secret, eventID, ts, ev.Body, ev.Headers.Get("webhook-signature")) {
		t.Fatalf("signature verification failed for %s", raw)
	}
	data := ev.Payload["data"].(map[string]any)
	if data["request_id"] != "req_urgent_1" || data["from"] != "codex" || data["urgent"] != true || int(data["queued_count"].(float64)) != 3 {
		t.Fatalf("unexpected data payload: %+v", data)
	}
}

func TestEventHubReplyGraceAndNeedsInput(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv, got, mu := startWebhookRecorder(t, nil)
	var seenReq atomic.Bool
	hub, err := NewEventHub(st.DB(), EventHubOptions{
		ReplyGrace:   30 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		IsReplyUnseen: func(_ string, reqID string) bool {
			return reqID != "req_already_read" || !seenReq.Load()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Stop()
	AllowUnsafeCallbacksForTest(hub, srv.Client())

	secret, _ := webhooksign.GenerateSecret()
	ctx := context.Background()
	_, _ = hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_rep", Agent: "chatgpt", ClientID: "c1", EventName: EventRequestReplied, CallbackURL: srv.URL, SigningSecret: secret,
	}, time.Hour)
	_, _ = hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_ni", Agent: "chatgpt", ClientID: "c1", EventName: EventRequestNeedsInput, CallbackURL: srv.URL, SigningSecret: secret,
	}, time.Hour)

	// Mark req_already_read as read before grace window elapses -> must be suppressed.
	seenReq.Store(true)
	hub.Replied(ctx, envelope.Request{ID: "req_already_read", From: "chatgpt", To: "grokbot", Status: envelope.StatusAnswered, Body: "SECRET_ASK"})
	// Unread reply -> must be delivered.
	hub.Replied(ctx, envelope.Request{ID: "req_unread", From: "chatgpt", To: "grokbot", Status: envelope.StatusAnswered, Body: "SECRET_ASK_2"})
	// Clarification question -> NeedsInput emits request.needs_input; Replied ignores StatusNeedsInput.
	niReq := envelope.Request{ID: "req_clarify", From: "chatgpt", To: "codex", Status: envelope.StatusNeedsInput, Body: "SECRET_QUESTION"}
	hub.NeedsInput(ctx, niReq)
	hub.Replied(ctx, niReq)

	events := waitForCount(t, got, mu, 2)
	// Wait briefly to make sure suppressed/duplicate events don't arrive.
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	total := len(*got)
	mu.Unlock()
	if total != 2 {
		t.Fatalf("expected 2 delivered webhooks (1 replied + 1 needs_input), got %d", total)
	}
	names := map[string]bool{}
	for _, ev := range events {
		if strings.Contains(string(ev.Body), "SECRET_") {
			t.Fatalf("leaked body: %s", ev.Body)
		}
		names[ev.Payload["name"].(string)] = true
	}
	if !names[EventRequestReplied] || !names[EventRequestNeedsInput] {
		t.Fatalf("got event names %+v, want both replied and needs_input", names)
	}
}

func TestEventHubRetrySameEventIDAndGapRecovery(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var attempts atomic.Int32
	srv, got, mu := startWebhookRecorder(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		n := attempts.Add(1)
		// First event fails once (attempt 1 -> 500), succeeds on retry (attempt 2 -> 200).
		if n == 1 {
			http.Error(w, "temporary failure", http.StatusInternalServerError)
			return
		}
		// Second event fails both attempts (attempts 3 & 4 -> 500) so it drops and marks has_gap.
		if n == 3 || n == 4 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		// Recovery: gap notice (attempt 5) and third event (attempt 6) succeed.
		w.WriteHeader(http.StatusOK)
	})

	hub, err := NewEventHub(st.DB(), EventHubOptions{
		RetryBackoff: []time.Duration{20 * time.Millisecond},
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Stop()
	AllowUnsafeCallbacksForTest(hub, srv.Client())

	secret, _ := webhooksign.GenerateSecret()
	ctx := context.Background()
	_, _ = hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_retry", Agent: "chatgpt", ClientID: "c1", EventName: EventInboxRequest, CallbackURL: srv.URL, SigningSecret: secret,
	}, time.Hour)

	// Event 1: transient 500 then 200 with identical eventId/webhook-id.
	hub.Queued(ctx, envelope.Request{ID: "req_1", From: "codex", To: "chatgpt", Kind: envelope.KindAsk})
	evs := waitForCount(t, got, mu, 2)
	id1 := evs[0].Headers.Get("webhook-id")
	id2 := evs[1].Headers.Get("webhook-id")
	if id1 == "" || id1 != id2 {
		t.Fatalf("retry changed webhook-id: attempt1=%q attempt2=%q", id1, id2)
	}

	// Event 2: exhausts retries -> dropped and sets has_gap.
	hub.Queued(ctx, envelope.Request{ID: "req_2_dropped", From: "codex", To: "chatgpt", Kind: envelope.KindAsk})
	waitForCount(t, got, mu, 4)

	// Wait briefly for DB update to commit has_gap.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sub, ok, _ := hub.store.Subscription(ctx, "sub_retry")
		if ok && sub.HasGap {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Event 3: recovery sends gap notice (truncated: true) followed by event 3.
	hub.Queued(ctx, envelope.Request{ID: "req_3_recovered", From: "codex", To: "chatgpt", Kind: envelope.KindAsk})
	evs = waitForCount(t, got, mu, 6)
	gapMsg := evs[4].Payload
	if gapMsg["type"] != "gap" || gapMsg["truncated"] != true {
		t.Fatalf("expected gap notice with truncated=true, got %+v", gapMsg)
	}
	if evs[4].Headers.Get("webhook-id") != gapMsg["eventId"] {
		t.Fatalf("gap webhook-id mismatch: header=%q payload=%v", evs[4].Headers.Get("webhook-id"), gapMsg["eventId"])
	}
	recMsg := evs[5].Payload
	if recMsg["type"] != "event" || recMsg["data"].(map[string]any)["request_id"] != "req_3_recovered" {
		t.Fatalf("expected recovered event 3, got %+v", recMsg)
	}
}

func TestEventHub410GoneAndAgentRemovalTerminate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv, got, mu := startWebhookRecorder(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if m["type"] == "event" {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	hub, err := NewEventHub(st.DB(), EventHubOptions{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Stop()
	AllowUnsafeCallbacksForTest(hub, srv.Client())

	secret, _ := webhooksign.GenerateSecret()
	ctx := context.Background()
	_, _ = hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_gone", Agent: "chatgpt", ClientID: "c1", EventName: EventInboxRequest, CallbackURL: srv.URL, SigningSecret: secret,
	}, time.Hour)

	hub.Queued(ctx, envelope.Request{ID: "req_gone", From: "codex", To: "chatgpt"})
	evs := waitForCount(t, got, mu, 2)
	if evs[1].Payload["type"] != "terminated" || evs[1].Payload["reason"] != "endpoint_gone" {
		t.Fatalf("expected terminated(endpoint_gone), got %+v", evs[1].Payload)
	}
	if hub.HasActiveSubscription("chatgpt") {
		t.Fatal("subscription still active after 410 Gone")
	}

	// Agent removal triggers terminated(agent_removed) and deletes subscription.
	_, _ = hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_remove", Agent: "sesame", ClientID: "c2", EventName: EventInboxRequest, CallbackURL: srv.URL, SigningSecret: secret,
	}, time.Hour)
	hub.TerminateAgent(ctx, "sesame", "agent_removed")
	evs = waitForCount(t, got, mu, 3)
	if evs[2].Payload["type"] != "terminated" || evs[2].Payload["reason"] != "agent_removed" {
		t.Fatalf("expected terminated(agent_removed), got %+v", evs[2].Payload)
	}
	if hub.HasActiveSubscription("sesame") {
		t.Fatal("subscription still active after TerminateAgent")
	}
}

func TestEventHubResumesPendingDeliveriesAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv, got, mu := startWebhookRecorder(t, nil)
	secret, _ := webhooksign.GenerateSecret()
	es, err := NewEventStore(st.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = es.CreateSubscription(ctx, EventSubscription{
		ID: "sub_restart", Agent: "chatgpt", ClientID: "c1", EventName: EventInboxRequest, CallbackURL: srv.URL, SigningSecret: secret,
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := buildEventEnvelope("evt_persisted_1", EventInboxRequest, time.Now().UTC(), map[string]any{
		"request_id": "req_before_restart", "from": "codex", "kind": "ask", "urgent": false, "queued_count": 1, "queued_at": time.Now().UTC().Format(time.RFC3339), "hint": "Call check_inbox to read pending requests.",
	})
	// Simulate a delivery persisted just before restart (next_attempt_at slightly in the future so the new hub picks it up after test client is configured).
	if err := es.EnqueueDelivery(ctx, EventDelivery{
		EventID:        "evt_persisted_1",
		SubscriptionID: "sub_restart",
		Agent:          "chatgpt",
		EventName:      EventInboxRequest,
		PayloadJSON:    payload,
		NextAttemptAt:  time.Now().Add(40 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}

	// Start fresh EventHub on the same SQLite DB.
	hub, err := NewEventHub(st.DB(), EventHubOptions{PollInterval: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Stop()
	AllowUnsafeCallbacksForTest(hub, srv.Client())

	evs := waitForCount(t, got, mu, 1)
	if evs[0].Headers.Get("webhook-id") != "evt_persisted_1" {
		t.Fatalf("resumed webhook-id = %q, want evt_persisted_1", evs[0].Headers.Get("webhook-id"))
	}
}

func newTestHub(t *testing.T, opts EventHubOptions) *EventHub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hub, err := NewEventHub(st.DB(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.Stop)
	return hub
}

func TestEventHubParallelDeliveryAcrossSubscriptions(t *testing.T) {
	hub := newTestHub(t, EventHubOptions{PollInterval: 10 * time.Millisecond})

	slowStarted := make(chan struct{}, 1)
	releaseSlow := make(chan struct{})
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slowStarted <- struct{}{}:
		default:
		}
		select {
		case <-releaseSlow:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer slowSrv.Close()
	defer close(releaseSlow)

	fastDone := make(chan struct{}, 1)
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case fastDone <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fastSrv.Close()

	AllowUnsafeCallbacksForTest(hub, &http.Client{Timeout: 2 * time.Second})
	secret, _ := webhooksign.GenerateSecret()
	ctx := context.Background()
	_, _ = hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_slow", Agent: "slowbot", ClientID: "c1", EventName: EventInboxRequest, CallbackURL: slowSrv.URL, SigningSecret: secret,
	}, time.Hour)
	_, _ = hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_fast", Agent: "fastbot", ClientID: "c2", EventName: EventInboxRequest, CallbackURL: fastSrv.URL, SigningSecret: secret,
	}, time.Hour)

	hub.Queued(ctx, envelope.Request{ID: "req_slow", From: "codex", To: "slowbot"})
	hub.Queued(ctx, envelope.Request{ID: "req_fast", From: "codex", To: "fastbot"})

	// fastbot must receive its wake while slowbot is still blocked.
	select {
	case <-fastDone:
	case <-time.After(1 * time.Second):
		t.Fatal("fastbot wake was blocked by slowbot's slow callback")
	}
	select {
	case <-slowStarted:
	case <-time.After(1 * time.Second):
		t.Fatal("slowbot never received its delivery attempt")
	}
}

func TestEventHubStandaloneGapRecoveryNotice(t *testing.T) {
	hub := newTestHub(t, EventHubOptions{
		PollInterval:           15 * time.Millisecond,
		RetryBackoff:           []time.Duration{20 * time.Millisecond},
		MaxConsecutiveFailures: 10,
	})

	var attempts atomic.Int32
	srv, got, mu := startWebhookRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		if attempts.Add(1) <= 2 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	AllowUnsafeCallbacksForTest(hub, srv.Client())
	secret, _ := webhooksign.GenerateSecret()
	ctx := context.Background()
	_, _ = hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_gap_only", Agent: "chatgpt", ClientID: "c1", EventName: EventInboxRequest, CallbackURL: srv.URL, SigningSecret: secret,
	}, time.Hour)

	// Send one event; both attempts fail so the final delivery is dropped and has_gap=1.
	// Callback then recovers with NO new event enqueued; a standalone gap notice must be sent.
	hub.Queued(ctx, envelope.Request{ID: "req_drop_last", From: "codex", To: "chatgpt"})
	evs := waitForCount(t, got, mu, 3)
	if evs[2].Payload["type"] != "gap" || evs[2].Payload["truncated"] != true {
		t.Fatalf("expected standalone recovery gap notice, got %+v", evs[2].Payload)
	}
	// has_gap must clear after the recovery notice succeeds.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		sub, ok, _ := hub.store.Subscription(ctx, "sub_gap_only")
		if ok && !sub.HasGap {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("has_gap was not cleared after standalone recovery gap notice")
}

func TestEventHubRateLimitedStandaloneGapDoesNotSpin(t *testing.T) {
	var nowUnixNano atomic.Int64
	base := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	nowUnixNano.Store(base.UnixNano())
	fakeNow := func() time.Time { return time.Unix(0, nowUnixNano.Load()).UTC() }

	hub := newTestHub(t, EventHubOptions{
		Now:          fakeNow,
		MaxPerHour:   1,
		PollInterval: 10 * time.Millisecond,
		RetryBackoff: []time.Duration{20 * time.Millisecond},
	})
	srv, got, mu := startWebhookRecorder(t, nil)
	AllowUnsafeCallbacksForTest(hub, srv.Client())

	secret, _ := webhooksign.GenerateSecret()
	ctx := context.Background()
	_, err := hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_rate_gap", Agent: "chatgpt", ClientID: "c1", EventName: EventInboxRequest, CallbackURL: srv.URL, SigningSecret: secret,
	}, 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Consume the 1/hour budget with one normal event.
	hub.Queued(ctx, envelope.Request{ID: "req_1", From: "codex", To: "chatgpt"})
	waitForCount(t, got, mu, 1)

	// Mark has_gap and advance past the gap backoff, while still inside the 1-hour rate window.
	if err := hub.store.SetHasGap(ctx, "sub_rate_gap"); err != nil {
		t.Fatal(err)
	}
	nowUnixNano.Store(base.Add(time.Second).UnixNano())
	hub.signalWorker()

	// Wait until the hub records the gap deferral retry timestamp.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if hub.isGapDeferred("sub_rate_gap", fakeNow()) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !hub.isGapDeferred("sub_rate_gap", fakeNow()) {
		t.Fatal("expected rate-limited standalone gap to record a retry deferral")
	}

	// Across several poll ticks before retryAt, no gap webhook may be sent and no worker should spin.
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	count := len(*got)
	mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 webhook while rate-limited, got %d", count)
	}

	// Advance clock past the hourly rate window -> standalone gap notice delivers once and clears has_gap.
	nowUnixNano.Store(base.Add(time.Hour + time.Second).UnixNano())
	hub.signalWorker()
	evs := waitForCount(t, got, mu, 2)
	if evs[1].Payload["type"] != "gap" || evs[1].Payload["truncated"] != true {
		t.Fatalf("expected standalone gap notice after rate window, got %+v", evs[1].Payload)
	}
	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		sub, ok, _ := hub.store.Subscription(ctx, "sub_rate_gap")
		if ok && !sub.HasGap {
			hub.deferGap("sub_deleted", base.Add(30*time.Minute))
			hub.pruneGapDefers(base.Add(time.Hour))
			hub.inflightMu.Lock()
			_, leaked := hub.gapRetryAfter["sub_deleted"]
			hub.inflightMu.Unlock()
			if leaked {
				t.Fatal("pruneGapDefers did not remove expired gapRetryAfter entry")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("has_gap was not cleared after deferred standalone gap notice")
}

func TestEventHubDoesNotRedeliverCompletedDelivery(t *testing.T) {
	hub := newTestHub(t, EventHubOptions{PollInterval: 5 * time.Millisecond})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	AllowUnsafeCallbacksForTest(hub, srv.Client())
	secret, _ := webhooksign.GenerateSecret()
	ctx := context.Background()
	_, err := hub.store.CreateSubscription(ctx, EventSubscription{
		ID: "sub_once", Agent: "chatgpt", ClientID: "c1", EventName: EventInboxRequest, CallbackURL: srv.URL, SigningSecret: secret,
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	hub.Queued(ctx, envelope.Request{ID: "req_once", From: "codex", To: "chatgpt"})
	select {
	case <-started:
	case <-time.After(1 * time.Second):
		t.Fatal("delivery never started")
	}

	// Trigger overlapping scheduler passes while delivery 1 is in flight and across its completion.
	for range 5 {
		hub.processDue(ctx)
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	for range 5 {
		hub.processDue(ctx)
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if got := hits.Load(); got != 1 {
		t.Fatalf("expected delivery to be sent exactly once, got %d", got)
	}
}
