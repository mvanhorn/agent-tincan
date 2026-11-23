package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const (
	defaultVerifyTimeout          = 5 * time.Second
	defaultDeliverTimeout         = 8 * time.Second
	defaultTerminateTimeout       = 3 * time.Second
	defaultMaxPerHour             = 60
	defaultMaxConsecutiveFailures = 10
	defaultPollInterval           = time.Second
	defaultSweepInterval          = time.Minute
	maxVerifyResponseBytes        = 64 << 10
)

var defaultRetryBackoff = []time.Duration{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
}

// EventHubOptions configures EventHub's relay lookups, rate limits, and timers.
type EventHubOptions struct {
	Now                    func() time.Time
	QueuedCount            func(agent string) int
	IsReplyUnseen          func(agent, requestID string) bool
	ReplyGrace             time.Duration
	MaxPerHour             int
	RetryBackoff           []time.Duration
	MaxConsecutiveFailures int
	PollInterval           time.Duration
	SweepInterval          time.Duration
	VerifyTimeout          time.Duration
	DeliverTimeout         time.Duration
}

// EventHub receives relay queue events (Queued, Requeued, Replied, NeedsInput),
// persists metadata-only deliveries for matching active MCP Events subscriptions,
// and delivers signed Standard Webhooks POSTs from a background worker.
type EventHub struct {
	store                  *EventStore
	now                    func() time.Time
	queuedCount            func(agent string) int
	isReplyUnseen          func(agent, requestID string) bool
	replyGrace             time.Duration
	maxPerHour             int
	retryBackoff           []time.Duration
	maxConsecutiveFailures int
	pollInterval           time.Duration
	sweepInterval          time.Duration

	clientMu      sync.RWMutex
	validateURL   func(string) (*url.URL, error)
	verifyClient  *http.Client
	deliverClient *http.Client

	rateMu    sync.Mutex
	sentTimes map[string][]time.Time

	inflightMu    sync.Mutex
	inflightSubs  map[string]struct{}
	subPending    map[string]bool
	gapRetryAfter map[string]time.Time
	semSaturated  bool
	sem           chan struct{}

	wakeCh     chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	deliveryWG sync.WaitGroup
	stopOnce   sync.Once
}

// NewEventHub opens the gateway events tables in db and starts the background
// delivery and expiry sweep worker. Call Stop on shutdown.
func NewEventHub(db *sql.DB, opts EventHubOptions) (*EventHub, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	store, err := NewEventStore(db, now)
	if err != nil {
		return nil, err
	}
	maxPerHour := opts.MaxPerHour
	if maxPerHour <= 0 {
		maxPerHour = defaultMaxPerHour
	}
	backoff := opts.RetryBackoff
	if len(backoff) == 0 {
		backoff = append([]time.Duration(nil), defaultRetryBackoff...)
	}
	maxFail := opts.MaxConsecutiveFailures
	if maxFail <= 0 {
		maxFail = defaultMaxConsecutiveFailures
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	sweepInterval := opts.SweepInterval
	if sweepInterval <= 0 {
		sweepInterval = defaultSweepInterval
	}
	verifyTimeout := opts.VerifyTimeout
	if verifyTimeout <= 0 {
		verifyTimeout = defaultVerifyTimeout
	}
	deliverTimeout := opts.DeliverTimeout
	if deliverTimeout <= 0 {
		deliverTimeout = defaultDeliverTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &EventHub{
		store:                  store,
		now:                    now,
		queuedCount:            opts.QueuedCount,
		isReplyUnseen:          opts.IsReplyUnseen,
		replyGrace:             opts.ReplyGrace,
		maxPerHour:             maxPerHour,
		retryBackoff:           backoff,
		maxConsecutiveFailures: maxFail,
		pollInterval:           pollInterval,
		sweepInterval:          sweepInterval,
		validateURL:            ValidateCallbackURL,
		verifyClient:           SafeWebhookClient(verifyTimeout),
		deliverClient:          SafeWebhookClient(deliverTimeout),
		sentTimes:              map[string][]time.Time{},
		inflightSubs:           map[string]struct{}{},
		subPending:             map[string]bool{},
		gapRetryAfter:          map[string]time.Time{},
		sem:                    make(chan struct{}, maxParallelDeliveries),
		wakeCh:                 make(chan struct{}, 1),
		ctx:                    ctx,
		cancel:                 cancel,
	}
	h.wg.Add(1)
	go h.run()
	return h, nil
}

// Stop stops the background worker and waits for in-flight deliveries to finish.
func (h *EventHub) Stop() {
	h.stopOnce.Do(func() {
		h.cancel()
		h.wg.Wait()
		h.deliveryWG.Wait()
	})
}

// HasActiveSubscription reports whether agent has at least one unexpired MCP
// Events subscription. Used by the composite WakeNamer to report wake=mcp-events.
func (h *EventHub) HasActiveSubscription(agent string) bool {
	if h == nil || agent == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ok, err := h.store.HasActiveSubscription(ctx, agent)
	return err == nil && ok
}

// Summary returns sanitized metadata for all active subscriptions (never secrets or query strings).
func (h *EventHub) Summary(ctx context.Context) ([]EventSubscriptionSummary, error) {
	if h == nil {
		return nil, nil
	}
	return h.store.Summary(ctx)
}

// Queued implements relay.Events for newly queued requests.
func (h *EventHub) Queued(ctx context.Context, req envelope.Request) {
	h.enqueueInboxRequest(ctx, req)
}

// Requeued implements relay.Requeuer for requests returned to the queue after a lease expired.
func (h *EventHub) Requeued(ctx context.Context, req envelope.Request) {
	h.enqueueInboxRequest(ctx, req)
}

// Replied implements relay.Replier for terminal replies to an agent's asks.
func (h *EventHub) Replied(ctx context.Context, req envelope.Request) {
	if h == nil || req.From == "" || req.ID == "" || req.Kind == envelope.KindPing {
		return
	}
	// Clarifications emit request.needs_input via NeedsInput, not request.replied.
	if req.Status == envelope.StatusNeedsInput {
		return
	}
	subs, err := h.store.ActiveSubscriptionsForEvent(ctx, req.From, EventRequestReplied)
	if err != nil || len(subs) == 0 {
		return
	}
	status := string(req.Status)
	if status == "" {
		status = string(envelope.StatusAnswered)
	}
	now := h.now().UTC()
	nextAttempt := now.Add(h.replyGrace)
	for _, sub := range subs {
		if !matchesRequestIDFilter(sub.Filters, req.ID) {
			continue
		}
		eventID := newEventID("evt_")
		payload, err := buildEventEnvelope(eventID, EventRequestReplied, now, map[string]any{
			"request_id": req.ID,
			"from":       req.To,
			"status":     status,
			"replied_at": now.Format(time.RFC3339),
			"hint":       "Call get_reply to read the response.",
		})
		if err != nil {
			continue
		}
		if err := h.store.EnqueueDelivery(ctx, EventDelivery{
			EventID:        eventID,
			SubscriptionID: sub.ID,
			Agent:          req.From,
			EventName:      EventRequestReplied,
			PayloadJSON:    payload,
			NextAttemptAt:  nextAttempt,
			CreatedAt:      now,
		}); err != nil {
			log.Printf("gateway events: enqueue %s for %s: %v", EventRequestReplied, req.From, err)
		}
	}
	h.signalWorker()
}

// NeedsInput implements relay.Clarifier when a teammate asks a clarification question.
func (h *EventHub) NeedsInput(ctx context.Context, req envelope.Request) {
	if h == nil || req.From == "" || req.ID == "" || req.Kind == envelope.KindPing {
		return
	}
	subs, err := h.store.ActiveSubscriptionsForEvent(ctx, req.From, EventRequestNeedsInput)
	if err != nil || len(subs) == 0 {
		return
	}
	now := h.now().UTC()
	nextAttempt := now.Add(h.replyGrace)
	for _, sub := range subs {
		if !matchesRequestIDFilter(sub.Filters, req.ID) {
			continue
		}
		eventID := newEventID("evt_")
		payload, err := buildEventEnvelope(eventID, EventRequestNeedsInput, now, map[string]any{
			"request_id": req.ID,
			"from":       req.To,
			"updated_at": now.Format(time.RFC3339),
			"hint":       "Call check_inbox or get_reply to answer the clarification.",
		})
		if err != nil {
			continue
		}
		if err := h.store.EnqueueDelivery(ctx, EventDelivery{
			EventID:        eventID,
			SubscriptionID: sub.ID,
			Agent:          req.From,
			EventName:      EventRequestNeedsInput,
			PayloadJSON:    payload,
			NextAttemptAt:  nextAttempt,
			CreatedAt:      now,
		}); err != nil {
			log.Printf("gateway events: enqueue %s for %s: %v", EventRequestNeedsInput, req.From, err)
		}
	}
	h.signalWorker()
}

func (h *EventHub) enqueueInboxRequest(ctx context.Context, req envelope.Request) {
	if h == nil || req.To == "" || req.ID == "" {
		return
	}
	// Defense-in-depth: held requests must never trigger external wake events.
	if req.Status == envelope.StatusHeld {
		return
	}
	subs, err := h.store.ActiveSubscriptionsForEvent(ctx, req.To, EventInboxRequest)
	if err != nil || len(subs) == 0 {
		return
	}
	now := h.now().UTC()
	queuedAt := req.CreatedAt.UTC()
	if queuedAt.IsZero() {
		queuedAt = now
	}
	kind := string(req.Kind)
	if kind == "" {
		kind = string(envelope.KindAsk)
	}
	queuedCount := 1
	if h.queuedCount != nil {
		if n := h.queuedCount(req.To); n > 0 {
			queuedCount = n
		}
	}
	for _, sub := range subs {
		if urgentOnly, _ := sub.Filters["urgent_only"].(bool); urgentOnly && !req.Urgent {
			continue
		}
		eventID := newEventID("evt_")
		payload, err := buildEventEnvelope(eventID, EventInboxRequest, now, map[string]any{
			"request_id":   req.ID,
			"from":         req.From,
			"kind":         kind,
			"urgent":       req.Urgent,
			"queued_count": queuedCount,
			"queued_at":    queuedAt.Format(time.RFC3339),
			"hint":         "Call check_inbox to read pending requests.",
		})
		if err != nil {
			continue
		}
		if err := h.store.EnqueueDelivery(ctx, EventDelivery{
			EventID:        eventID,
			SubscriptionID: sub.ID,
			Agent:          req.To,
			EventName:      EventInboxRequest,
			PayloadJSON:    payload,
			NextAttemptAt:  now,
			CreatedAt:      now,
		}); err != nil {
			log.Printf("gateway events: enqueue %s for %s: %v", EventInboxRequest, req.To, err)
		}
	}
	h.signalWorker()
}

func matchesRequestIDFilter(filters map[string]any, reqID string) bool {
	want, _ := filters["request_id"].(string)
	return want == "" || want == reqID
}

func buildEventEnvelope(eventID, name string, ts time.Time, data map[string]any) (string, error) {
	b, err := json.Marshal(map[string]any{
		"type":      "event",
		"eventId":   eventID,
		"name":      name,
		"timestamp": ts.UTC().Format(time.RFC3339),
		"cursor":    eventID,
		"data":      data,
	})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TerminateAgent deletes all active subscriptions for agent and sends best-effort
// terminated notices to their callback URLs.
func (h *EventHub) TerminateAgent(ctx context.Context, agent, reason string) {
	if h == nil || agent == "" {
		return
	}
	subs, err := h.store.DeleteAgentSubscriptions(ctx, agent)
	if err != nil {
		log.Printf("gateway events: delete subscriptions for %s: %v", agent, err)
		return
	}
	now := h.now().UTC()
	for _, sub := range subs {
		h.clearGapDefer(sub.ID)
		if !sub.ExpiresAt.After(now) {
			continue
		}
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultTerminateTimeout)
		h.sendTerminatedNotice(tctx, sub, reason)
		cancel()
	}
}

func (h *EventHub) signalWorker() {
	select {
	case h.wakeCh <- struct{}{}:
	default:
	}
}

func (h *EventHub) run() {
	defer h.wg.Done()
	pollTicker := time.NewTicker(h.pollInterval)
	defer pollTicker.Stop()
	sweepTicker := time.NewTicker(h.sweepInterval)
	defer sweepTicker.Stop()

	// Process any pending deliveries persisted before a relay restart.
	h.processDue(h.ctx)

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-h.wakeCh:
			h.processDue(h.ctx)
		case <-pollTicker.C:
			h.processDue(h.ctx)
		case <-sweepTicker.C:
			if n, err := h.store.SweepExpired(h.ctx); err != nil {
				log.Printf("gateway events: sweep expired: %v", err)
			} else if n > 0 {
				log.Printf("gateway events: swept %d expired subscriptions", n)
			}
			h.pruneGapDefers(h.now().UTC())
		}
	}
}

const (
	maxParallelDeliveries = 8
	deliveryBatchSize     = 32
)

func (h *EventHub) tryStartSub(subID string) bool {
	h.inflightMu.Lock()
	if _, busy := h.inflightSubs[subID]; busy {
		h.subPending[subID] = true
		h.inflightMu.Unlock()
		return false
	}
	select {
	case h.sem <- struct{}{}:
		h.inflightSubs[subID] = struct{}{}
		delete(h.subPending, subID)
		h.inflightMu.Unlock()
		return true
	default:
		h.semSaturated = true
		h.inflightMu.Unlock()
		return false
	}
}

func (h *EventHub) finishSub(subID string, hasMore bool) {
	<-h.sem
	h.inflightMu.Lock()
	delete(h.inflightSubs, subID)
	pending := h.subPending[subID]
	delete(h.subPending, subID)
	wake := hasMore || pending || h.semSaturated
	h.semSaturated = false
	h.inflightMu.Unlock()
	if wake && h.ctx.Err() == nil {
		h.signalWorker()
	}
}

func (h *EventHub) deferGap(subID string, retryAt time.Time) {
	h.inflightMu.Lock()
	h.gapRetryAfter[subID] = retryAt
	h.inflightMu.Unlock()
}

func (h *EventHub) clearGapDefer(subID string) {
	h.inflightMu.Lock()
	delete(h.gapRetryAfter, subID)
	h.inflightMu.Unlock()
}

func (h *EventHub) pruneGapDefers(now time.Time) {
	h.inflightMu.Lock()
	for id, retryAt := range h.gapRetryAfter {
		if !retryAt.After(now) {
			delete(h.gapRetryAfter, id)
		}
	}
	h.inflightMu.Unlock()
}

func (h *EventHub) isGapDeferred(subID string, now time.Time) bool {
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	retryAt, ok := h.gapRetryAfter[subID]
	if !ok {
		return false
	}
	if retryAt.After(now) {
		return true
	}
	delete(h.gapRetryAfter, subID)
	return false
}

func (h *EventHub) processDue(ctx context.Context) {
	subIDs, err := h.store.DueDeliverySubscriptionIDs(ctx, 32)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("gateway events: load due deliveries: %v", err)
		}
		return
	}

	hasDue := make(map[string]struct{}, len(subIDs))
	for _, id := range subIDs {
		hasDue[id] = struct{}{}
	}

	gapSubs, gerr := h.store.DueGapSubscriptions(ctx, h.backoffFor(0), 16)
	if gerr != nil && ctx.Err() == nil {
		log.Printf("gateway events: load due gap notices: %v", gerr)
	}

	// Dispatch independent subscriptions concurrently (bounded by h.sem)
	// without blocking the scheduler loop on slow callbacks. Each worker
	// re-reads due rows after acquiring its per-subscription lock so a
	// finished delivery can never be replayed from a stale snapshot.
	for _, subID := range subIDs {
		if ctx.Err() != nil {
			return
		}
		if !h.tryStartSub(subID) {
			continue
		}
		h.deliveryWG.Add(1)
		go func(id string) {
			defer h.deliveryWG.Done()
			hasMore := false
			defer func() { h.finishSub(id, hasMore) }()
			items, lerr := h.store.DueDeliveriesForSubscription(ctx, id, deliveryBatchSize)
			if lerr != nil {
				if ctx.Err() == nil {
					log.Printf("gateway events: load due deliveries for %s: %v", id, lerr)
				}
				return
			}
			for _, d := range items {
				if ctx.Err() != nil {
					return
				}
				h.deliverOne(ctx, d)
			}
			hasMore = len(items) == deliveryBatchSize
		}(subID)
	}

	now := h.now().UTC()
	for _, sub := range gapSubs {
		if ctx.Err() != nil {
			return
		}
		if _, ok := hasDue[sub.ID]; ok {
			continue
		}
		if h.isGapDeferred(sub.ID, now) {
			continue
		}
		if !h.tryStartSub(sub.ID) {
			continue
		}
		h.deliveryWG.Add(1)
		go func(id string) {
			defer h.deliveryWG.Done()
			defer func() { h.finishSub(id, false) }()
			h.deliverStandaloneGap(ctx, id)
		}(sub.ID)
	}
}

func (h *EventHub) deliverStandaloneGap(ctx context.Context, subID string) {
	sub, due, err := h.store.DueGapSubscription(ctx, subID, h.backoffFor(0))
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("gateway events: lookup gap sub %s: %v", subID, err)
		}
		return
	}
	if !due {
		return
	}
	now := h.now().UTC()
	if !sub.ExpiresAt.After(now) || h.isGapDeferred(sub.ID, now) {
		return
	}
	if allowed, retryAt := h.reserveRateSlot(sub.Agent, now); !allowed {
		h.deferGap(sub.ID, retryAt)
		return
	}
	status, err := h.sendGapNotice(ctx, sub)
	if err == nil {
		h.clearGapDefer(sub.ID)
		if cerr := h.store.ClearHasGap(ctx, sub.ID); cerr != nil {
			log.Printf("gateway events: clear gap sub=%s: %v", sub.ID, cerr)
		}
		return
	}
	if status == http.StatusGone {
		h.terminateSubscription(ctx, sub.ID, "endpoint_gone")
		return
	}
	consecutive, rerr := h.store.RecordGapFailure(ctx, sub.ID, err.Error())
	if rerr != nil {
		log.Printf("gateway events: record gap failure sub=%s: %v", sub.ID, rerr)
		return
	}
	log.Printf("gateway events: recovery gap notice failed agent=%s sub=%s callback=%s err=%v",
		sub.Agent, sub.ID, SanitizeCallbackURL(sub.CallbackURL), err)
	if consecutive >= h.maxConsecutiveFailures {
		h.terminateSubscription(ctx, sub.ID, "delivery_failed")
	}
}

func (h *EventHub) deliverOne(ctx context.Context, d EventDelivery) {
	now := h.now().UTC()
	sub, found, err := h.store.Subscription(ctx, d.SubscriptionID)
	if err != nil {
		log.Printf("gateway events: lookup sub %s: %v", d.SubscriptionID, err)
		return
	}
	if !found || !sub.ExpiresAt.After(now) {
		_ = h.store.DeleteDelivery(ctx, d.EventID)
		return
	}

	// Reply grace suppression: skip waking if the reply was already read before dispatch.
	if (d.EventName == EventRequestReplied || d.EventName == EventRequestNeedsInput) && h.isReplyUnseen != nil {
		if reqID := extractPayloadRequestID(d.PayloadJSON); reqID != "" && !h.isReplyUnseen(d.Agent, reqID) {
			_ = h.store.DeleteDelivery(ctx, d.EventID)
			return
		}
	}

	// Per-agent hourly rate cap.
	if allowed, retryAt := h.reserveRateSlot(d.Agent, now); !allowed {
		updatedPayload := ""
		if d.EventName == EventInboxRequest && h.queuedCount != nil {
			updatedPayload = refreshQueuedCount(d.PayloadJSON, h.queuedCount(d.Agent))
		}
		_ = h.store.DeferDelivery(ctx, d.EventID, retryAt, updatedPayload)
		return
	}

	// If earlier events were dropped/aged out on this subscription, send a gap
	// notice before delivering the recovered event.
	if sub.HasGap {
		status, err := h.sendGapNotice(ctx, sub)
		if err != nil {
			if status == http.StatusGone {
				h.terminateSubscription(ctx, sub.ID, "endpoint_gone")
				return
			}
			consecutive, _ := h.store.RecordGapFailure(ctx, sub.ID, err.Error())
			if consecutive >= h.maxConsecutiveFailures {
				h.terminateSubscription(ctx, sub.ID, "delivery_failed")
				return
			}
			nextAttempt := now.Add(h.backoffFor(d.Attempts))
			_ = h.store.DeferDelivery(ctx, d.EventID, nextAttempt, "")
			return
		}
		h.clearGapDefer(sub.ID)
		_ = h.store.ClearHasGap(ctx, sub.ID)
	}

	status, err := h.postSigned(ctx, h.getDeliverClient(), sub.CallbackURL, sub.SigningSecret, sub.ID, d.EventID, []byte(d.PayloadJSON))
	if err == nil {
		if err := h.store.MarkDelivered(ctx, d.EventID, sub.ID); err != nil {
			log.Printf("gateway events: mark delivered %s: %v", d.EventID, err)
		}
		return
	}
	if status == http.StatusGone {
		h.terminateSubscription(ctx, sub.ID, "endpoint_gone")
		return
	}

	maxAttempts := len(h.retryBackoff) + 1
	nextAttempt := now.Add(h.backoffFor(d.Attempts))
	dropped, consecutive, rerr := h.store.RecordAttemptFailure(ctx, d.EventID, sub.ID, nextAttempt, maxAttempts, err.Error())
	if rerr != nil {
		log.Printf("gateway events: record failure for %s: %v", d.EventID, rerr)
		return
	}
	log.Printf("gateway events: delivery failed agent=%s sub=%s event=%s attempt=%d/%d callback=%s err=%v",
		sub.Agent, sub.ID, d.EventID, d.Attempts+1, maxAttempts, SanitizeCallbackURL(sub.CallbackURL), err)
	if dropped {
		log.Printf("gateway events: dropped event=%s sub=%s; marking gap for recovery", d.EventID, sub.ID)
	}
	if consecutive >= h.maxConsecutiveFailures {
		h.terminateSubscription(ctx, sub.ID, "delivery_failed")
	}
}

func (h *EventHub) terminateSubscription(ctx context.Context, subID, reason string) {
	h.clearGapDefer(subID)
	sub, deleted, err := h.store.ForceDeleteSubscription(ctx, subID)
	if err != nil || !deleted {
		return
	}
	log.Printf("gateway events: terminating sub=%s agent=%s reason=%s callback=%s",
		sub.ID, sub.Agent, reason, SanitizeCallbackURL(sub.CallbackURL))
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultTerminateTimeout)
	defer cancel()
	h.sendTerminatedNotice(tctx, sub, reason)
}

func (h *EventHub) sendGapNotice(ctx context.Context, sub EventSubscription) (int, error) {
	eventID := newEventID("evt_")
	body, err := json.Marshal(map[string]any{
		"type":           "gap",
		"eventId":        eventID,
		"subscriptionId": sub.ID,
		"truncated":      true,
		"timestamp":      h.now().UTC().Format(time.RFC3339),
		"hint":           "Some events were dropped; call check_inbox to reconcile state.",
	})
	if err != nil {
		return 0, err
	}
	return h.postSigned(ctx, h.getDeliverClient(), sub.CallbackURL, sub.SigningSecret, sub.ID, eventID, body)
}

func (h *EventHub) sendTerminatedNotice(ctx context.Context, sub EventSubscription, reason string) {
	eventID := newEventID("evt_")
	body, err := json.Marshal(map[string]any{
		"type":           "terminated",
		"eventId":        eventID,
		"subscriptionId": sub.ID,
		"reason":         reason,
		"timestamp":      h.now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	if _, err := h.postSigned(ctx, h.getDeliverClient(), sub.CallbackURL, sub.SigningSecret, sub.ID, eventID, body); err != nil {
		log.Printf("gateway events: best-effort terminated notice failed sub=%s callback=%s: %v",
			sub.ID, SanitizeCallbackURL(sub.CallbackURL), err)
	}
}

func (h *EventHub) checkCallbackURL(raw string) (*url.URL, error) {
	h.clientMu.RLock()
	fn := h.validateURL
	h.clientMu.RUnlock()
	return fn(raw)
}

func (h *EventHub) getVerifyClient() *http.Client {
	h.clientMu.RLock()
	c := h.verifyClient
	h.clientMu.RUnlock()
	return c
}

func (h *EventHub) getDeliverClient() *http.Client {
	h.clientMu.RLock()
	c := h.deliverClient
	h.clientMu.RUnlock()
	return c
}

// verifyCallback performs the SEP-3415 verification challenge handshake.
// On failure it returns a machine-readable reason for JSON-RPC -32027 data.reason.
func (h *EventHub) verifyCallback(ctx context.Context, subID, callbackURL, secret string) (string, error) {
	if _, err := h.checkCallbackURL(callbackURL); err != nil {
		return "invalid_url", err
	}
	challenge := newChallenge()
	eventID := newEventID("evt_")
	payload, err := json.Marshal(map[string]any{
		"type":           "verification",
		"eventId":        eventID,
		"subscriptionId": subID,
		"challenge":      challenge,
	})
	if err != nil {
		return "challenge_failed", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, bytes.NewReader(payload))
	if err != nil {
		return "invalid_url", err
	}
	if err := ApplyWebhookHeaders(req, secret, subID, eventID, h.now().UTC(), payload); err != nil {
		return "challenge_failed", err
	}
	resp, err := h.getVerifyClient().Do(req)
	if err != nil {
		return classifyCallbackError(err), sanitizeWebhookError(err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxVerifyResponseBytes))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return "http_4xx", fmt.Errorf("callback returned HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "http_5xx", fmt.Errorf("callback returned HTTP %d", resp.StatusCode)
	}
	var echo struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(respBody, &echo); err != nil || echo.Challenge != challenge {
		return "challenge_failed", errors.New("callback did not echo the verification challenge")
	}
	return "", nil
}

// sanitizeWebhookError strips callback URLs (and any query-string tokens in
// them) from net/http *url.Error wrappers before errors reach logs, last_error,
// the admin API, or JSON-RPC responses.
func sanitizeWebhookError(err error) error {
	if err == nil {
		return nil
	}
	if ue, ok := errors.AsType[*url.Error](err); ok {
		op := ue.Op
		if op == "" {
			op = "Post"
		}
		return fmt.Errorf("%s %s: %w", op, SanitizeCallbackURL(ue.URL), ue.Err)
	}
	return err
}

func classifyCallbackError(err error) string {
	switch {
	case errors.Is(err, ErrInvalidCallbackURL), errors.Is(err, ErrBlockedCallbackIP), errors.Is(err, ErrRedirectsDisallowed):
		return "invalid_url"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var (
		x509Unknown x509.UnknownAuthorityError
		x509Host    x509.HostnameError
		x509Invalid x509.CertificateInvalidError
		tlsRec      tls.RecordHeaderError
		tlsAlert    tls.AlertError
	)
	if errors.As(err, &x509Unknown) || errors.As(err, &x509Host) || errors.As(err, &x509Invalid) ||
		errors.As(err, &tlsRec) || errors.As(err, &tlsAlert) || strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:") {
		return "tls_error"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	return "connection_refused"
}

func (h *EventHub) postSigned(ctx context.Context, client *http.Client, callbackURL, secret, subID, eventID string, body []byte) (int, error) {
	if _, err := h.checkCallbackURL(callbackURL); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if err := ApplyWebhookHeaders(req, secret, subID, eventID, h.now().UTC(), body); err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, sanitizeWebhookError(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
}

func (h *EventHub) backoffFor(attempts int) time.Duration {
	if len(h.retryBackoff) == 0 {
		return 5 * time.Second
	}
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= len(h.retryBackoff) {
		return h.retryBackoff[len(h.retryBackoff)-1]
	}
	return h.retryBackoff[attempts]
}

func (h *EventHub) reserveRateSlot(agent string, now time.Time) (bool, time.Time) {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	cutoff := now.Add(-time.Hour)
	times := h.sentTimes[agent]
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= h.maxPerHour {
		h.sentTimes[agent] = kept
		return false, kept[0].Add(time.Hour)
	}
	h.sentTimes[agent] = append(kept, now)
	return true, time.Time{}
}

func extractPayloadRequestID(payloadJSON string) string {
	var env struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &env); err != nil {
		return ""
	}
	return env.Data.RequestID
}

func refreshQueuedCount(payloadJSON string, count int) string {
	if count <= 0 {
		return ""
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &env); err != nil {
		return ""
	}
	data, ok := env["data"].(map[string]any)
	if !ok {
		return ""
	}
	data["queued_count"] = count
	b, err := json.Marshal(env)
	if err != nil {
		return ""
	}
	return string(b)
}

func newEventID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

func newChallenge() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
