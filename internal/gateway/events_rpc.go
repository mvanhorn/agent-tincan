package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// MCP Events JSON-RPC error codes.
const (
	rpcEventSubscriptionNotFound = -32001
	rpcCallbackEndpointError     = -32027
	rpcSubscriptionLimitReached  = -32028
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type eventsRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// eventsMiddleware wraps the /mcp StreamableHTTPHandler to negotiate MCP Events
// on protocol version 2026-07-28 and dispatch events/* JSON-RPC methods.
// Non-POST requests, older protocol versions, and all non-event methods pass
// through to next untouched.
func (g *Gateway) eventsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.events == nil || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "application/json" {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if mbe, ok := errors.AsType[*http.MaxBytesError](err); ok {
				http.Error(w, fmt.Sprintf("request body exceeds %d bytes", mbe.Limit), http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			next.ServeHTTP(w, r)
			return
		}
		var req jsonRPCRequest
		if err := json.Unmarshal(trimmed, &req); err != nil || req.Method == "" {
			next.ServeHTTP(w, r)
			return
		}

		switch {
		case req.Method == "initialize":
			g.handleEventsInitialize(w, r, body, req.Params, next)
		case strings.HasPrefix(req.Method, "events/"):
			g.handleEventsRPC(w, r, req)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (g *Gateway) handleEventsInitialize(w http.ResponseWriter, r *http.Request, body []byte, paramsRaw json.RawMessage, next http.Handler) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(paramsRaw, &params)
	if params.ProtocolVersion < EventsProtocolVersion {
		next.ServeHTTP(w, r)
		return
	}

	origAccept := r.Header.Get("Accept")
	clientWantsSSE := strings.Contains(strings.ToLower(origAccept), "text/event-stream")

	innerReq := r.Clone(r.Context())
	innerReq.Body = io.NopCloser(bytes.NewReader(body))
	innerReq.ContentLength = int64(len(body))
	// Ensure go-sdk's StreamableHTTPHandler accepts the initialize request even
	// if a client sent Accept: application/json only.
	if !clientWantsSSE || !strings.Contains(strings.ToLower(origAccept), "application/json") {
		innerReq.Header = innerReq.Header.Clone()
		innerReq.Header.Set("Accept", "application/json, text/event-stream")
	}

	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, innerReq)

	if rec.Code != http.StatusOK {
		copyHeaders(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}

	innerCT, _, _ := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	rawResp := rec.Body.Bytes()
	var jsonPayload []byte
	if innerCT == "text/event-stream" {
		jsonPayload = extractSSEJSON(rawResp)
	} else {
		jsonPayload = bytes.TrimSpace(rawResp)
	}
	if len(jsonPayload) == 0 {
		copyHeaders(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rawResp)
		return
	}

	augmented, ok := augmentInitializeCapabilities(jsonPayload)
	if !ok {
		copyHeaders(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rawResp)
		return
	}

	copyHeadersExcept(w.Header(), rec.Header(), "Content-Type", "Content-Length")
	if innerCT == "text/event-stream" && clientWantsSSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", augmented)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(augmented, '\n'))
}

func extractSSEJSON(raw []byte) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64<<10), 2<<20)
	var dataLines [][]byte
	for scanner.Scan() {
		line := scanner.Bytes()
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			dataLines = append(dataLines, bytes.TrimSpace(after))
		}
	}
	return bytes.Join(dataLines, []byte("\n"))
}

func augmentInitializeCapabilities(payload []byte) ([]byte, bool) {
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, false
	}
	result, ok := msg["result"].(map[string]any)
	if !ok || result == nil {
		return nil, false
	}
	result["protocolVersion"] = EventsProtocolVersion
	caps, _ := result["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
		result["capabilities"] = caps
	}
	// Dual-compat discovery:
	// 1. Pre-spec ChatGPT / Vercel AI SDK key.
	caps["events"] = map[string]any{}
	// 2. Final SEP-3415 extension key.
	exts, _ := caps["extensions"].(map[string]any)
	if exts == nil {
		exts = map[string]any{}
		caps["extensions"] = exts
	}
	exts[EventsExtensionID] = map[string]any{
		"deliveryModes": []string{"webhook"},
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return nil, false
	}
	return out, true
}

func copyHeaders(dst, src http.Header) {
	copyHeadersExcept(dst, src)
}

func copyHeadersExcept(dst, src http.Header, skip ...string) {
	skipMap := map[string]bool{}
	for _, s := range skip {
		skipMap[http.CanonicalHeaderKey(s)] = true
	}
	for k, vals := range src {
		if skipMap[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func (g *Gateway) handleEventsRPC(w http.ResponseWriter, r *http.Request, req jsonRPCRequest) {
	// Protocol version gate: reject explicit older protocol versions cleanly;
	// allow absence for stateless clients.
	if pv := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version")); pv != "" && pv < EventsProtocolVersion {
		writeEventsRPC(w, req.ID, nil, &eventsRPCError{
			Code:    rpcNoMethod,
			Message: fmt.Sprintf("MCP Events requires protocol version %s or later (got %s)", EventsProtocolVersion, pv),
		})
		return
	}

	agent, _ := r.Context().Value(agentKey{}).(string)
	clientID, _ := r.Context().Value(clientIDKey{}).(string)
	if agent == "" || clientID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch req.Method {
	case "events/list":
		writeEventsRPC(w, req.ID, map[string]any{
			"events": EventCatalog(),
		}, nil)
	case "events/subscribe":
		g.handleEventsSubscribe(w, r, req.ID, req.Params, agent, clientID)
	case "events/refresh":
		g.handleEventsRefresh(w, r, req.ID, req.Params, agent, clientID)
	case "events/unsubscribe":
		g.handleEventsUnsubscribe(w, r, req.ID, req.Params, agent, clientID)
	default:
		writeEventsRPC(w, req.ID, nil, &eventsRPCError{
			Code:    rpcNoMethod,
			Message: fmt.Sprintf("unsupported events method %q", req.Method),
		})
	}
}

type subscribeParams struct {
	Name          string         `json:"name"`
	Event         string         `json:"event"`
	DeliveryMode  string         `json:"deliveryMode"`
	DeliveryMode2 string         `json:"delivery_mode"`
	CallbackURL   string         `json:"callbackUrl"`
	CallbackURL2  string         `json:"callback_url"`
	Secret        string         `json:"secret"`
	SigningSecret string         `json:"signingSecret"`
	SigningSec2   string         `json:"signing_secret"`
	Filters       map[string]any `json:"filters"`
	Input         map[string]any `json:"input"`
	Arguments     map[string]any `json:"arguments"`
	TTL           int64          `json:"ttl"`
	TTLSeconds    int64          `json:"ttlSeconds"`
	TTLSeconds2   int64          `json:"ttl_seconds"`
	Delivery      *struct {
		Mode         string `json:"mode"`
		CallbackURL  string `json:"callbackUrl"`
		CallbackURL2 string `json:"callback_url"`
		Secret       string `json:"secret"`
	} `json:"delivery"`
}

func (p subscribeParams) eventName() string {
	return firstNonEmpty(strings.TrimSpace(p.Name), strings.TrimSpace(p.Event))
}

func (p subscribeParams) mode() string {
	m := firstNonEmpty(strings.TrimSpace(p.DeliveryMode), strings.TrimSpace(p.DeliveryMode2))
	if m == "" && p.Delivery != nil {
		m = strings.TrimSpace(p.Delivery.Mode)
	}
	if m == "" {
		return "webhook"
	}
	return m
}

func (p subscribeParams) callbackURL() string {
	cb := firstNonEmpty(strings.TrimSpace(p.CallbackURL), strings.TrimSpace(p.CallbackURL2))
	if cb == "" && p.Delivery != nil {
		cb = firstNonEmpty(strings.TrimSpace(p.Delivery.CallbackURL), strings.TrimSpace(p.Delivery.CallbackURL2))
	}
	return cb
}

func (p subscribeParams) signingSecret() string {
	s := firstNonEmpty(p.Secret, p.SigningSecret, p.SigningSec2)
	if s == "" && p.Delivery != nil {
		s = p.Delivery.Secret
	}
	return s
}

func (p subscribeParams) filterMap() map[string]any {
	if p.Filters != nil {
		return p.Filters
	}
	if p.Input != nil {
		return p.Input
	}
	return p.Arguments
}

func (p subscribeParams) ttlDuration() (time.Duration, error) {
	secs := p.TTLSeconds
	if secs == 0 {
		secs = p.TTLSeconds2
	}
	if secs == 0 {
		secs = p.TTL
	}
	if secs < 0 {
		return 0, errors.New("ttl must not be negative")
	}
	if secs == 0 {
		return DefaultSubTTL, nil
	}
	return time.Duration(secs) * time.Second, nil
}

func (g *Gateway) handleEventsSubscribe(w http.ResponseWriter, r *http.Request, id, rawParams json.RawMessage, agent, clientID string) {
	var p subscribeParams
	if len(bytes.TrimSpace(rawParams)) == 0 || json.Unmarshal(rawParams, &p) != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInvalidParams, Message: "invalid subscribe params"})
		return
	}
	eventName := p.eventName()
	if !KnownEvent(eventName) {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInvalidParams, Message: fmt.Sprintf("unknown event %q", eventName)})
		return
	}
	// Dual-compat Mcp-Name check: validate when present, accept when absent.
	if hdr := strings.TrimSpace(r.Header.Get("Mcp-Name")); hdr != "" && hdr != eventName {
		writeEventsRPC(w, id, nil, &eventsRPCError{
			Code:    rpcInvalidParams,
			Message: fmt.Sprintf("Mcp-Name header %q does not match event name %q", hdr, eventName),
		})
		return
	}
	if p.mode() != "webhook" {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInvalidParams, Message: fmt.Sprintf("unsupported deliveryMode %q (only webhook is supported)", p.mode())})
		return
	}
	filters, err := ValidateEventFilters(eventName, p.filterMap())
	if err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInvalidParams, Message: err.Error()})
		return
	}
	ttl, err := p.ttlDuration()
	if err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInvalidParams, Message: err.Error()})
		return
	}
	signingSecret := p.signingSecret()
	if _, err := ParseSigningSecret(signingSecret); err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInvalidParams, Message: err.Error()})
		return
	}
	cbURL := p.callbackURL()
	if _, err := g.events.checkCallbackURL(cbURL); err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{
			Code:    rpcCallbackEndpointError,
			Message: "callback URL rejected: " + err.Error(),
			Data:    map[string]any{"reason": "invalid_url"},
		})
		return
	}

	// Enforce per-agent subscription cap before issuing any outbound verification POST.
	active, err := g.events.store.CountActiveForAgent(r.Context(), agent)
	if err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInternal, Message: "subscription store unavailable"})
		return
	}
	if active >= g.events.store.maxSubsPerAgent {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcSubscriptionLimitReached, Message: ErrTooManySubscriptions.Error()})
		return
	}

	subID := newEventID("sub_")
	if reason, err := g.events.verifyCallback(r.Context(), subID, cbURL, signingSecret); err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{
			Code:    rpcCallbackEndpointError,
			Message: "callback verification failed: " + err.Error(),
			Data:    map[string]any{"reason": reason},
		})
		return
	}

	tokenHash, _ := r.Context().Value(tokenHashKey{}).(string)
	sub, err := g.events.store.CreateSubscriptionIfTokenActive(r.Context(), tokenHash, EventSubscription{
		ID:            subID,
		Agent:         agent,
		ClientID:      clientID,
		EventName:     eventName,
		Filters:       filters,
		CallbackURL:   cbURL,
		SigningSecret: signingSecret,
	}, ttl)
	if errors.Is(err, ErrTokenRevoked) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+g.base+`/.well-known/oauth-protected-resource"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if errors.Is(err, ErrTooManySubscriptions) {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcSubscriptionLimitReached, Message: err.Error()})
		return
	}
	if err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInternal, Message: "failed to save subscription"})
		return
	}

	log.Printf("gateway events: subscribed agent=%s sub=%s event=%s callback=%s expires=%s",
		agent, sub.ID, sub.EventName, SanitizeCallbackURL(sub.CallbackURL), sub.ExpiresAt.Format(time.RFC3339))

	writeEventsRPC(w, id, map[string]any{
		"subscriptionId": sub.ID,
		"event":          sub.EventName,
		"deliveryMode":   "webhook",
		"expiresAt":      sub.ExpiresAt.Format(time.RFC3339),
		"refreshBefore":  refreshBeforeTime(sub.CreatedAt, sub.ExpiresAt).Format(time.RFC3339),
	}, nil)
}

type subIDParams struct {
	SubscriptionID  string `json:"subscriptionId"`
	SubscriptionID2 string `json:"subscription_id"`
	TTL             int64  `json:"ttl"`
	TTLSeconds      int64  `json:"ttlSeconds"`
	TTLSeconds2     int64  `json:"ttl_seconds"`
}

func (p subIDParams) subID() string {
	return firstNonEmpty(strings.TrimSpace(p.SubscriptionID), strings.TrimSpace(p.SubscriptionID2))
}

func (p subIDParams) ttlDuration() (time.Duration, error) {
	secs := p.TTLSeconds
	if secs == 0 {
		secs = p.TTLSeconds2
	}
	if secs == 0 {
		secs = p.TTL
	}
	if secs < 0 {
		return 0, errors.New("ttl must not be negative")
	}
	if secs == 0 {
		return DefaultSubTTL, nil
	}
	return time.Duration(secs) * time.Second, nil
}

func (g *Gateway) handleEventsRefresh(w http.ResponseWriter, r *http.Request, id, rawParams json.RawMessage, agent, clientID string) {
	var p subIDParams
	if len(bytes.TrimSpace(rawParams)) == 0 || json.Unmarshal(rawParams, &p) != nil || p.subID() == "" {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInvalidParams, Message: "subscriptionId is required"})
		return
	}
	ttl, err := p.ttlDuration()
	if err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInvalidParams, Message: err.Error()})
		return
	}
	if hdr := strings.TrimSpace(r.Header.Get("Mcp-Name")); hdr != "" {
		existing, err := g.events.store.GetOwnedSubscription(r.Context(), p.subID(), agent, clientID)
		if errors.Is(err, ErrSubscriptionNotFound) {
			writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcEventSubscriptionNotFound, Message: ErrSubscriptionNotFound.Error()})
			return
		}
		if err != nil {
			writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInternal, Message: "failed to look up subscription"})
			return
		}
		if hdr != existing.EventName {
			writeEventsRPC(w, id, nil, &eventsRPCError{
				Code:    rpcInvalidParams,
				Message: fmt.Sprintf("Mcp-Name header %q does not match event name %q", hdr, existing.EventName),
			})
			return
		}
	}
	sub, err := g.events.store.RefreshSubscription(r.Context(), p.subID(), agent, clientID, ttl)
	if errors.Is(err, ErrSubscriptionNotFound) {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcEventSubscriptionNotFound, Message: err.Error()})
		return
	}
	if err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInternal, Message: "failed to refresh subscription"})
		return
	}
	log.Printf("gateway events: refreshed agent=%s sub=%s event=%s expires=%s",
		agent, sub.ID, sub.EventName, sub.ExpiresAt.Format(time.RFC3339))
	writeEventsRPC(w, id, map[string]any{
		"subscriptionId": sub.ID,
		"expiresAt":      sub.ExpiresAt.Format(time.RFC3339),
		"refreshBefore":  refreshBeforeTime(sub.UpdatedAt, sub.ExpiresAt).Format(time.RFC3339),
	}, nil)
}

func (g *Gateway) handleEventsUnsubscribe(w http.ResponseWriter, r *http.Request, id, rawParams json.RawMessage, agent, clientID string) {
	var p subIDParams
	if len(bytes.TrimSpace(rawParams)) == 0 || json.Unmarshal(rawParams, &p) != nil || p.subID() == "" {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInvalidParams, Message: "subscriptionId is required"})
		return
	}
	if hdr := strings.TrimSpace(r.Header.Get("Mcp-Name")); hdr != "" {
		existing, err := g.events.store.GetOwnedSubscription(r.Context(), p.subID(), agent, clientID)
		if errors.Is(err, ErrSubscriptionNotFound) {
			writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcEventSubscriptionNotFound, Message: ErrSubscriptionNotFound.Error()})
			return
		}
		if err != nil {
			writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInternal, Message: "failed to look up subscription"})
			return
		}
		if hdr != existing.EventName {
			writeEventsRPC(w, id, nil, &eventsRPCError{
				Code:    rpcInvalidParams,
				Message: fmt.Sprintf("Mcp-Name header %q does not match event name %q", hdr, existing.EventName),
			})
			return
		}
	}
	deleted, err := g.events.store.DeleteSubscription(r.Context(), p.subID(), agent, clientID)
	if err != nil {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcInternal, Message: "failed to delete subscription"})
		return
	}
	if !deleted {
		writeEventsRPC(w, id, nil, &eventsRPCError{Code: rpcEventSubscriptionNotFound, Message: ErrSubscriptionNotFound.Error()})
		return
	}
	g.events.clearGapDefer(p.subID())
	log.Printf("gateway events: unsubscribed agent=%s sub=%s", agent, p.subID())
	writeEventsRPC(w, id, map[string]any{
		"subscriptionId": p.subID(),
		"unsubscribed":   true,
	}, nil)
}

func refreshBeforeTime(base, expires time.Time) time.Time {
	ttl := expires.Sub(base)
	lead := max(min(time.Hour, ttl/4), 0)
	return expires.Add(-lead)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func writeEventsRPC(w http.ResponseWriter, id json.RawMessage, result any, e *eventsRPCError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = result
	}
	writeJSON(w, http.StatusOK, out)
}
