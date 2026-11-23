package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/gateway"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

func setupWithEvents(t *testing.T) (*env, *gateway.EventHub) {
	t.Helper()
	e := setupWith(t, relay.Config{MaxWait: 3 * time.Second})
	hub, err := gateway.NewEventHub(e.m.Store.DB(), gateway.EventHubOptions{
		QueuedCount:  e.m.Server.QueuedCount,
		PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.Stop)
	e.g.SetEvents(hub)
	e.conn.Events = hub
	e.m.Server.SetConnector(e.conn)
	return e, hub
}

func postMCP(t *testing.T, url, tok string, headers map[string]string, body any) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func decodeMCPResponse(t *testing.T, resp *http.Response, body []byte) map[string]any {
	t.Helper()
	raw := bytes.TrimSpace(body)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var dataLines []string
		for line := range strings.SplitSeq(string(raw), "\n") {
			if after, ok := strings.CutPrefix(line, "data:"); ok {
				dataLines = append(dataLines, strings.TrimSpace(after))
			}
		}
		raw = []byte(strings.Join(dataLines, "\n"))
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode MCP response (%s): %v\nbody:\n%s", resp.Header.Get("Content-Type"), err, body)
	}
	return m
}

func TestInitializeDualCompatCapabilities(t *testing.T) {
	e, _ := setupWithEvents(t)
	code, _, err := e.conn.Connect(context.Background(), "chatgpt", false)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.login(t, code)

	// Protocol 2026-07-28 negotiates 2026-07-28 and advertises BOTH locations.
	resp, raw := postMCP(t, e.gw.URL+"/mcp", tok, nil, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2026-07-28",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "chatgpt", "version": "1.0"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("2026-07-28 initialize status %d: %s", resp.StatusCode, raw)
	}
	msg := decodeMCPResponse(t, resp, raw)
	res, _ := msg["result"].(map[string]any)
	if res["protocolVersion"] != "2026-07-28" {
		t.Fatalf("protocolVersion = %v, want 2026-07-28", res["protocolVersion"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["events"]; !ok {
		t.Fatalf("missing legacy capabilities.events: %+v", caps)
	}
	exts, _ := caps["extensions"].(map[string]any)
	ext, _ := exts["io.modelcontextprotocol/events"].(map[string]any)
	if ext == nil {
		t.Fatalf("missing capabilities.extensions[io.modelcontextprotocol/events]: %+v", caps)
	}
	modes, _ := ext["deliveryModes"].([]any)
	if len(modes) != 1 || modes[0] != "webhook" {
		t.Fatalf("deliveryModes = %+v, want [webhook]", modes)
	}

	// Legacy 2025-06-18 advertises neither.
	respOld, rawOld := postMCP(t, e.gw.URL+"/mcp", tok, nil, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "legacy", "version": "1.0"},
		},
	})
	if respOld.StatusCode != http.StatusOK {
		t.Fatalf("2025-06-18 initialize status %d: %s", respOld.StatusCode, rawOld)
	}
	msgOld := decodeMCPResponse(t, respOld, rawOld)
	resOld, _ := msgOld["result"].(map[string]any)
	if resOld["protocolVersion"] == "2026-07-28" {
		t.Fatalf("legacy initialize unexpectedly negotiated 2026-07-28: %+v", resOld)
	}
	capsOld, _ := resOld["capabilities"].(map[string]any)
	if _, ok := capsOld["events"]; ok {
		t.Fatalf("legacy initialize advertised capabilities.events: %+v", capsOld)
	}
	if extsOld, _ := capsOld["extensions"].(map[string]any); extsOld != nil {
		if _, ok := extsOld["io.modelcontextprotocol/events"]; ok {
			t.Fatalf("legacy initialize advertised SEP-3415 extension: %+v", capsOld)
		}
	}

	// Existing MCP tool calls continue working normally.
	cs := e.mcpSession(t, tok)
	listRes, err := cs.ListTools(t.Context(), nil)
	if err != nil || len(listRes.Tools) == 0 {
		t.Fatalf("ListTools failed with events enabled: %v %+v", err, listRes)
	}
}

func TestEventsListAndLifecycle(t *testing.T) {
	e, hub := setupWithEvents(t)
	codeA, _, err := e.conn.Connect(context.Background(), "chatgpt", false)
	if err != nil {
		t.Fatal(err)
	}
	tokA, _ := e.login(t, codeA)

	codeB, _, err := e.conn.Connect(context.Background(), "sesame", false)
	if err != nil {
		t.Fatal(err)
	}
	tokB, _ := e.login(t, codeB)

	secret, err := gateway.GenerateSigningSecret()
	if err != nil {
		t.Fatal(err)
	}

	// Callback server verifying Standard Webhooks headers and echoing challenge.
	var verifiedHeaders http.Header
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		verifiedHeaders = r.Header.Clone()
		var in struct {
			Type      string `json:"type"`
			EventID   string `json:"eventId"`
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(body, &in)
		ts, _ := strconv.ParseInt(r.Header.Get("webhook-timestamp"), 10, 64)
		if r.Header.Get("webhook-id") != in.EventID {
			http.Error(w, "webhook-id mismatch", http.StatusBadRequest)
			return
		}
		if !gateway.VerifyWebhookSignature(secret, in.EventID, ts, body, r.Header.Get("webhook-signature")) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": in.Challenge})
	}))
	defer cb.Close()
	gateway.AllowUnsafeCallbacksForTest(hub, cb.Client())

	// events/list rejected on explicit 2025-06-18 header.
	resp, raw := postMCP(t, e.gw.URL+"/mcp", tokA, map[string]string{"MCP-Protocol-Version": "2025-06-18"}, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "events/list",
	})
	msg := decodeMCPResponse(t, resp, raw)
	if msg["error"] == nil {
		t.Fatalf("events/list on 2025-06-18 should fail: %+v", msg)
	}

	// events/list succeeds on 2026-07-28 and returns all 3 events.
	resp, raw = postMCP(t, e.gw.URL+"/mcp", tokA, map[string]string{"MCP-Protocol-Version": "2026-07-28"}, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "events/list",
	})
	msg = decodeMCPResponse(t, resp, raw)
	eventsList := msg["result"].(map[string]any)["events"].([]any)
	if len(eventsList) != 3 {
		t.Fatalf("events/list returned %d events, want 3", len(eventsList))
	}

	// events/subscribe with mismatched Mcp-Name fails with -32602.
	resp, raw = postMCP(t, e.gw.URL+"/mcp", tokA, map[string]string{
		"MCP-Protocol-Version": "2026-07-28",
		"Mcp-Name":             "request.replied",
	}, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "events/subscribe",
		"params": map[string]any{
			"name":        "inbox.request",
			"callbackUrl": cb.URL,
			"secret":      secret,
		},
	})
	msg = decodeMCPResponse(t, resp, raw)
	if code := int(msg["error"].(map[string]any)["code"].(float64)); code != -32602 {
		t.Fatalf("mismatched Mcp-Name error code = %d, want -32602", code)
	}

	// events/subscribe with matching Mcp-Name succeeds.
	resp, raw = postMCP(t, e.gw.URL+"/mcp", tokA, map[string]string{
		"MCP-Protocol-Version": "2026-07-28",
		"Mcp-Name":             "inbox.request",
	}, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "events/subscribe",
		"params": map[string]any{
			"name":        "inbox.request",
			"callbackUrl": cb.URL,
			"secret":      secret,
			"filters":     map[string]any{"urgent_only": false},
		},
	})
	msg = decodeMCPResponse(t, resp, raw)
	if msg["error"] != nil {
		t.Fatalf("subscribe with matching Mcp-Name failed: %+v", msg)
	}
	subID := msg["result"].(map[string]any)["subscriptionId"].(string)
	if subID == "" {
		t.Fatalf("empty subscriptionId: %+v", msg)
	}
	if verifiedHeaders.Get("X-MCP-Subscription-Id") != subID {
		t.Fatalf("verification X-MCP-Subscription-Id = %q, want %q", verifiedHeaders.Get("X-MCP-Subscription-Id"), subID)
	}

	// events/subscribe without Mcp-Name header also succeeds.
	resp, raw = postMCP(t, e.gw.URL+"/mcp", tokA, nil, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "events/subscribe",
		"params": map[string]any{
			"name":         "request.replied",
			"callback_url": cb.URL,
			"secret":       secret,
		},
	})
	msg = decodeMCPResponse(t, resp, raw)
	if msg["error"] != nil {
		t.Fatalf("subscribe without Mcp-Name failed: %+v", msg)
	}

	// Agent B cannot refresh or unsubscribe Agent A's subscription.
	resp, raw = postMCP(t, e.gw.URL+"/mcp", tokB, nil, map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "events/refresh",
		"params": map[string]any{"subscriptionId": subID},
	})
	msg = decodeMCPResponse(t, resp, raw)
	if msg["error"] == nil {
		t.Fatalf("agent B refreshed agent A's subscription: %+v", msg)
	}
	resp, raw = postMCP(t, e.gw.URL+"/mcp", tokB, nil, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "events/unsubscribe",
		"params": map[string]any{"subscriptionId": subID},
	})
	msg = decodeMCPResponse(t, resp, raw)
	if msg["error"] == nil {
		t.Fatalf("agent B unsubscribed agent A's subscription: %+v", msg)
	}

	// Agent A refreshes and then unsubscribes its own subscription.
	resp, raw = postMCP(t, e.gw.URL+"/mcp", tokA, nil, map[string]any{
		"jsonrpc": "2.0", "id": 8, "method": "events/refresh",
		"params": map[string]any{"subscriptionId": subID, "ttlSeconds": 7200},
	})
	msg = decodeMCPResponse(t, resp, raw)
	if msg["error"] != nil {
		t.Fatalf("agent A refresh failed: %+v", msg)
	}
	resp, raw = postMCP(t, e.gw.URL+"/mcp", tokA, nil, map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "events/unsubscribe",
		"params": map[string]any{"subscriptionId": subID},
	})
	msg = decodeMCPResponse(t, resp, raw)
	if msg["error"] != nil {
		t.Fatalf("agent A unsubscribe failed: %+v", msg)
	}
}

func TestEventsSubscribeVerificationErrors(t *testing.T) {
	e, hub := setupWithEvents(t)
	code, _, _ := e.conn.Connect(context.Background(), "chatgpt", false)
	tok, _ := e.login(t, code)
	secret, _ := gateway.GenerateSigningSecret()

	// 1. SSRF URL rejected with -32027 and data.reason = invalid_url (with default SSRF guard active!).
	resp, raw := postMCP(t, e.gw.URL+"/mcp", tok, nil, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "events/subscribe",
		"params": map[string]any{
			"name":        "inbox.request",
			"callbackUrl": "https://100.64.0.1/events",
			"secret":      secret,
		},
	})
	msg := decodeMCPResponse(t, resp, raw)
	errObj := msg["error"].(map[string]any)
	if int(errObj["code"].(float64)) != -32027 || errObj["data"].(map[string]any)["reason"] != "invalid_url" {
		t.Fatalf("SSRF subscribe error = %+v, want -32027 invalid_url", errObj)
	}

	// Now enable local httptest callbacks to test handshake failure reasons.
	wrongEcho := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"challenge":"wrong"}`))
	}))
	defer wrongEcho.Close()

	status404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer status404.Close()

	status503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer status503.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()

	c := &http.Client{Timeout: 80 * time.Millisecond}
	gateway.AllowUnsafeCallbacksForTest(hub, c)

	for _, tc := range []struct {
		name   string
		url    string
		reason string
	}{
		{"wrong_challenge", wrongEcho.URL, "challenge_failed"},
		{"http_404", status404.URL, "http_4xx"},
		{"http_503", status503.URL, "http_5xx"},
		{"timeout", slow.URL, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := postMCP(t, e.gw.URL+"/mcp", tok, nil, map[string]any{
				"jsonrpc": "2.0", "id": 10, "method": "events/subscribe",
				"params": map[string]any{
					"name":        "inbox.request",
					"callbackUrl": tc.url,
					"secret":      secret,
				},
			})
			msg := decodeMCPResponse(t, resp, raw)
			errObj, _ := msg["error"].(map[string]any)
			if errObj == nil {
				t.Fatalf("expected error for %s, got %+v", tc.name, msg)
			}
			if int(errObj["code"].(float64)) != -32027 {
				t.Fatalf("%s code = %v, want -32027", tc.name, errObj["code"])
			}
			if gotReason := errObj["data"].(map[string]any)["reason"]; gotReason != tc.reason {
				t.Fatalf("%s reason = %v, want %q", tc.name, gotReason, tc.reason)
			}
		})
	}

	// Verify no subscription rows were persisted by any failed handshake.
	if hub.HasActiveSubscription("chatgpt") {
		t.Fatal("failed verification handshakes should persist zero subscriptions")
	}
}

func TestGuestRefusedOnEvents(t *testing.T) {
	e, _ := setupWithEvents(t)
	code, _, err := e.conn.ConnectGuest(context.Background(), "visitor", identity.GuestTerms{
		Until: time.Now().Add(time.Hour),
		Asks:  []string{"muse"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.login(t, code)

	resp, _ := postMCP(t, e.gw.URL+"/mcp", tok, map[string]string{"MCP-Protocol-Version": "2026-07-28"}, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "events/list",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("guest events/list status = %d, want 403", resp.StatusCode)
	}
}

func TestEndToEndGatewayWakeRosterAndRemove(t *testing.T) {
	e, hub := setupWithEvents(t)
	waker := wake.New(wake.Config{"chatgpt": {Method: wake.None}}, e.m.Store, wake.Options{})
	t.Cleanup(waker.Stop)
	e.m.Server.SetEvents(relay.MultiEvents{waker, hub})
	e.m.Server.SetWakeNamer(relay.WithMCPEventsWake(waker, hub.HasActiveSubscription))
	e.m.Server.SetGatewayEvents(func(ctx context.Context) (any, error) { return hub.Summary(ctx) })

	var mu sync.Mutex
	var webhooks []map[string]any
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg map[string]any
		_ = json.Unmarshal(body, &msg)
		mu.Lock()
		webhooks = append(webhooks, msg)
		mu.Unlock()
		if msg["type"] == "verification" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": msg["challenge"]})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cb.Close()
	gateway.AllowUnsafeCallbacksForTest(hub, cb.Client())

	ctx := context.Background()
	code, _, err := e.conn.Connect(ctx, "chatgpt", false)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.login(t, code)

	wakeOf := func(name string) string {
		agents, err := e.m.Client(t, "admin").Agents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range agents {
			if a.Name == name {
				return a.Wake
			}
		}
		return ""
	}
	if got := wakeOf("chatgpt"); got != "none" {
		t.Fatalf("before subscribe: wake = %q, want none", got)
	}

	secret, _ := gateway.GenerateSigningSecret()
	resp, raw := postMCP(t, e.gw.URL+"/mcp", tok, map[string]string{"MCP-Protocol-Version": "2026-07-28"}, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "events/subscribe",
		"params": map[string]any{
			"name":        "inbox.request",
			"callbackUrl": cb.URL + "?token=secret-query",
			"secret":      secret,
		},
	})
	if msg := decodeMCPResponse(t, resp, raw); msg["error"] != nil {
		t.Fatalf("subscribe failed: %+v", msg)
	}
	if got := wakeOf("chatgpt"); got != relay.WakeMCPEvents {
		t.Fatalf("while subscribed: wake = %q, want %q", got, relay.WakeMCPEvents)
	}

	// Admin gateway events summary hides query string and signing secret.
	var summaryResp struct {
		Subscriptions []gateway.EventSubscriptionSummary `json:"subscriptions"`
	}
	if err := e.m.Client(t, "admin").Raw(ctx, "GET", "/v1/admin/gateway/events", nil, &summaryResp); err != nil {
		t.Fatalf("GET /v1/admin/gateway/events: %v", err)
	}
	if len(summaryResp.Subscriptions) != 1 || summaryResp.Subscriptions[0].Callback != cb.URL {
		t.Fatalf("summary = %+v", summaryResp.Subscriptions)
	}

	// Asking chatgpt delivers an inbox.request webhook with zero body text.
	if _, err := e.m.Client(t, "muse").Send(ctx, "chatgpt", "CONFIDENTIAL_ASK_TEXT", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(webhooks)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Removing chatgpt terminates subscriptions and sends a terminated notice.
	if err := e.m.Client(t, "admin").Remove(ctx, "chatgpt"); err != nil {
		t.Fatal(err)
	}
	if hub.HasActiveSubscription("chatgpt") {
		t.Fatal("chatgpt still has active subscription after Remove")
	}
	mu.Lock()
	var sawEvent, sawTerminated bool
	for _, msg := range webhooks {
		b, _ := json.Marshal(msg)
		if strings.Contains(string(b), "CONFIDENTIAL_ASK_TEXT") {
			t.Fatalf("webhook leaked request body: %s", b)
		}
		if msg["type"] == "event" && msg["name"] == "inbox.request" {
			sawEvent = true
		}
		if msg["type"] == "terminated" {
			sawTerminated = true
		}
	}
	mu.Unlock()
	if !sawEvent || !sawTerminated {
		t.Fatalf("sawEvent=%v sawTerminated=%v webhooks=%+v", sawEvent, sawTerminated, webhooks)
	}
}

func TestSubscribeRefusedWhenRevokedDuringVerification(t *testing.T) {
	e, hub := setupWithEvents(t)
	ctx := context.Background()
	code, _, err := e.conn.Connect(ctx, "chatgpt", false)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.login(t, code)
	secret, _ := gateway.GenerateSigningSecret()

	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate agent removal / token revocation racing with the verification challenge.
		if err := e.oauth.RevokeAgent(ctx, "chatgpt"); err != nil {
			t.Errorf("RevokeAgent: %v", err)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"challenge": body["challenge"]})
	}))
	defer cb.Close()
	gateway.AllowUnsafeCallbacksForTest(hub, cb.Client())

	resp, _ := postMCP(t, e.gw.URL+"/mcp", tok, nil, map[string]any{
		"jsonrpc": "2.0", "id": 99, "method": "events/subscribe",
		"params": map[string]any{
			"name":        "inbox.request",
			"callbackUrl": cb.URL,
			"secret":      secret,
		},
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 after revocation mid-challenge", resp.StatusCode)
	}
	if hub.HasActiveSubscription("chatgpt") {
		t.Fatal("revoked token must not persist a subscription")
	}
}

func TestCallbackQuerySecretStrippedFromErrors(t *testing.T) {
	e, hub := setupWithEvents(t)
	ctx := context.Background()
	code, _, err := e.conn.Connect(ctx, "chatgpt", false)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.login(t, code)
	secret, _ := gateway.GenerateSigningSecret()

	// Closed server produces a *url.Error carrying the full URL + query token.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL + "/hook?token=SUPER_SECRET_QUERY_TOKEN"
	closed.Close()
	gateway.AllowUnsafeCallbacksForTest(hub, &http.Client{Timeout: 200 * time.Millisecond})

	_, raw := postMCP(t, e.gw.URL+"/mcp", tok, nil, map[string]any{
		"jsonrpc": "2.0", "id": 100, "method": "events/subscribe",
		"params": map[string]any{
			"name":        "inbox.request",
			"callbackUrl": closedURL,
			"secret":      secret,
		},
	})
	if strings.Contains(string(raw), "SUPER_SECRET_QUERY_TOKEN") {
		t.Fatalf("callback query secret leaked into error response: %s", raw)
	}
}

// Keep unused import check happy if mcp package is referenced above.
var _ = (*mcp.ClientSession)(nil)
