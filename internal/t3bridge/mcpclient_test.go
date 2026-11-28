package t3bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPClientListEventsAndTools(t *testing.T) {
	const secretTok = "secret-bearer-xyz-999"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+secretTok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("MCP-Protocol-Version") != ProtocolVersion20260728 {
			http.Error(w, "bad protocol version", http.StatusBadRequest)
			return
		}
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		meta, ok := req.Params["_meta"].(map[string]any)
		if !ok || meta["io.modelcontextprotocol/protocolVersion"] != ProtocolVersion20260728 {
			http.Error(w, "missing _meta", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "events/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"_meta": map[string]any{
						"io.modelcontextprotocol/serverInfo": map[string]any{"name": "T3 Code", "version": "0.1.13-nightly"},
					},
					"events": []map[string]any{
						{"name": "thread.run_settled", "delivery": []string{"poll", "push", "webhook"}},
						{"name": "thread.needs_input", "delivery": []string{"poll", "push", "webhook"}},
					},
				},
			})
		case "tools/call":
			tool := req.Params["name"].(string)
			if r.Header.Get("MCP-Name") != tool {
				http.Error(w, "missing MCP-Name", http.StatusBadRequest)
				return
			}
			switch tool {
			case "t3_thread_read":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"structuredContent": map[string]any{
							"thread": map[string]any{
								"threadId": "thr-1",
								"title":    "Fix test",
								"link":     "https://app.t3.codes/threads/env-1/thr-1",
								"status":   "idle",
							},
							"items": []map[string]any{
								{"type": "user_message", "createdBy": "user", "text": "Fix it"},
								{"type": "assistant_message", "createdBy": "agent", "text": "All tests pass now."},
							},
						},
					},
				})
			case "t3_pending_request":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"structuredContent": map[string]any{"requestIds": []string{"req-q1"}},
					},
				})
			}
		}
	}))
	defer srv.Close()

	c, err := NewMCPClient(srv.URL, secretTok, "0.34.0")
	if err != nil {
		t.Fatal(err)
	}
	list, err := c.ListEvents(context.Background())
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if list.ServerInfo.Name != "T3 Code" || len(list.Events) != 2 {
		t.Fatalf("unexpected ListEvents: %+v", list)
	}
	tr, err := c.ReadThread(context.Background(), "thr-1", 5)
	if err != nil {
		t.Fatalf("ReadThread: %v", err)
	}
	if tr.LatestAssistantText() != "All tests pass now." || tr.IsRunning() {
		t.Fatalf("unexpected ReadThread: %+v", tr)
	}
	ids, err := c.ListPendingQuestions(context.Background(), "thr-1")
	if err != nil || len(ids) != 1 || ids[0] != "req-q1" {
		t.Fatalf("ListPendingQuestions = %v, %v", ids, err)
	}

	// Verify 401 returns AuthError and never leaks the bearer token.
	badClient, err := NewMCPClient(srv.URL, "wrong-secret-token-777", "0.34.0")
	if err != nil {
		t.Fatal(err)
	}
	_, err = badClient.ListEvents(context.Background())
	var authErr *AuthError
	if !errors.As(err, &authErr) || authErr.Status != 401 {
		t.Fatalf("want AuthError 401, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong-secret-token-777") {
		t.Fatalf("error leaked bearer token: %v", err)
	}
}

func TestMCPClientStreamEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("MCP-Method") != "events/stream" || r.Header.Get("MCP-Name") != "thread.run_settled" {
			http.Error(w, "bad headers", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, ": keepalive comment\n\n")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/events/active\",\"params\":{\"cursor\":\"c1\",\"truncated\":true,\"heartbeatMs\":5000}}\n\n")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/events/event\",\"params\":{\"eventId\":\"evt_1\",\"cursor\":\"c2\",\"timestamp\":\"2026-11-28T04:00:00Z\",\"data\":{\"type\":\"thread.run_settled\",\"threadId\":\"thr-9\",\"outcome\":\"completed\"}}}\n\n")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/events/heartbeat\",\"params\":{\"cursor\":\"c2\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c, err := NewMCPClient(srv.URL, "tok", "0.34.0")
	if err != nil {
		t.Fatal(err)
	}
	var gotActiveCursor, gotHeartbeat string
	var gotTruncated bool
	var gotEvents []EventOccurrence
	err = c.StreamEvents(context.Background(), "thread.run_settled", "", nil, 5000, StreamHandlers{
		OnActive: func(cursor string, truncated bool, _ int) {
			gotActiveCursor = cursor
			gotTruncated = truncated
		},
		OnEvent:     func(ev EventOccurrence) { gotEvents = append(gotEvents, ev) },
		OnHeartbeat: func(cursor string) { gotHeartbeat = cursor },
	})
	if err != nil && !errors.Is(err, context.Canceled) && err.Error() != "EOF" {
		t.Fatalf("StreamEvents error: %v", err)
	}
	if gotActiveCursor != "c1" || !gotTruncated || gotHeartbeat != "c2" || len(gotEvents) != 1 {
		t.Fatalf("unexpected stream callbacks: active=%q trunc=%v hb=%q events=%d", gotActiveCursor, gotTruncated, gotHeartbeat, len(gotEvents))
	}
	m, err := gotEvents[0].ParseMilestone()
	if err != nil || m.ThreadID != "thr-9" || m.Outcome != "completed" {
		t.Fatalf("ParseMilestone = %+v, %v", m, err)
	}
}

func TestMCPClientRejectsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:9999/steal", http.StatusFound)
	}))
	defer srv.Close()

	c, err := NewMCPClient(srv.URL, "tok-never-redirect", "0.34.0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.ListEvents(ctx); err == nil {
		t.Fatal("expected redirect refusal")
	}
}
