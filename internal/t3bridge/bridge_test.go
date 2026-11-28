package t3bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

type fakeT3Server struct {
	t   *testing.T
	srv *httptest.Server

	mu             sync.Mutex
	projects       []T3Project
	threads        map[string]ThreadReadResult
	pendingIDs     map[string][]string
	pendingDetails map[string]PendingQuestion
	launchCalls    []map[string]any
	sendCalls      []map[string]any
	respondCalls   []map[string]any
	readCalls      []string
	streamChans    map[string][]chan EventOccurrence
	nextThreadSeq  int
}

func newFakeT3Server(t *testing.T) *fakeT3Server {
	t.Helper()
	f := &fakeT3Server{
		t:              t,
		projects:       []T3Project{{ID: "proj-1", Title: "Agent Tincan", WorkspaceRoot: "/Users/test/agent-tincan"}},
		threads:        map[string]ThreadReadResult{},
		pendingIDs:     map[string][]string{},
		pendingDetails: map[string]PendingQuestion{},
		streamChans:    map[string][]chan EventOccurrence{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeT3Server) emit(eventName string, ev EventOccurrence) {
	f.mu.Lock()
	chans := append([]chan EventOccurrence(nil), f.streamChans[eventName]...)
	f.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (f *fakeT3Server) waitStreamReady(eventName string) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.streamChans[eventName])
		f.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timed out waiting for stream %s", eventName)
}

func (f *fakeT3Server) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	switch req.Method {
	case "events/stream":
		name, _ := req.Params["name"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		ch := make(chan EventOccurrence, 16)
		f.mu.Lock()
		f.streamChans[name] = append(f.streamChans[name], ch)
		f.mu.Unlock()
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/events/active\",\"params\":{\"cursor\":\"c0\",\"truncated\":false,\"heartbeatMs\":5000}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				b, _ := json.Marshal(map[string]any{
					"jsonrpc": "2.0",
					"method":  "notifications/events/event",
					"params":  ev,
				})
				fmt.Fprintf(w, "data: %s\n\n", b)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	case "events/poll":
		writeRPC(w, req.ID, map[string]any{"events": []any{}, "cursor": "c0", "truncated": false, "hasMore": false})
	case "tools/call":
		tool, _ := req.Params["name"].(string)
		args, _ := req.Params["arguments"].(map[string]any)
		f.handleTool(w, req.ID, tool, args)
	default:
		http.Error(w, "unknown method", 404)
	}
}

func (f *fakeT3Server) handleTool(w http.ResponseWriter, id int64, tool string, args map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch tool {
	case "t3_project_list":
		writeTool(w, id, ProjectListResult{Projects: f.projects})
	case "t3_thread_launch":
		f.launchCalls = append(f.launchCalls, args)
		f.nextThreadSeq++
		tid := fmt.Sprintf("thr-%d", f.nextThreadSeq)
		link := "https://app.t3.codes/threads/env-1/" + tid
		title, _ := args["title"].(string)
		f.threads[tid] = ThreadReadResult{
			Thread: ThreadSummary{ThreadID: tid, Title: title, Link: link, Status: "running", ActiveRunID: "run-1"},
		}
		writeTool(w, id, LaunchThreadResult{ThreadID: tid, Link: link, Title: title})
	case "t3_thread_send":
		f.sendCalls = append(f.sendCalls, args)
		tid, _ := args["threadId"].(string)
		if tid == "deleted-thr" {
			writeRPC(w, id, map[string]any{
				"isError": true,
				"content": []map[string]any{{"type": "text", "text": "thread not found"}},
			})
			return
		}
		writeTool(w, id, map[string]any{"ok": true})
	case "t3_thread_read":
		tid, _ := args["threadId"].(string)
		f.readCalls = append(f.readCalls, tid)
		tr, ok := f.threads[tid]
		if !ok {
			writeRPC(w, id, map[string]any{
				"isError": true,
				"content": []map[string]any{{"type": "text", "text": "thread not found"}},
			})
			return
		}
		writeTool(w, id, tr)
	case "t3_pending_request":
		action, _ := args["action"].(string)
		tid, _ := args["threadId"].(string)
		switch action {
		case "list":
			writeTool(w, id, PendingRequestList{RequestIDs: f.pendingIDs[tid]})
		case "read":
			rid, _ := args["requestId"].(string)
			pq := f.pendingDetails[rid]
			pq.RequestID = rid
			writeTool(w, id, pq)
		case "respond":
			f.respondCalls = append(f.respondCalls, args)
			writeTool(w, id, map[string]any{"ok": true})
		}
	default:
		http.Error(w, "unknown tool "+tool, 404)
	}
}

func writeRPC(w http.ResponseWriter, id int64, res any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
}

func writeTool(w http.ResponseWriter, id int64, sc any) {
	writeRPC(w, id, map[string]any{"structuredContent": sc, "isError": false})
}

func TestBridgeInboundLaunchQuestionPermissionAndSettle(t *testing.T) {
	mesh := testrelay.New(t, relay.Config{})
	t3Relay := mesh.JoinOnMachineOf(t, "muse", "t3")
	grok := mesh.Client(t, "grokbot")

	fake := newFakeT3Server(t)
	mcpCli, err := NewMCPClient(fake.srv.URL, "tok", "0.34.0")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	br, err := NewBridge(Config{
		Relay:          t3Relay,
		T3:             mcpCli,
		State:          NewStateStore(filepath.Join(dir, "t3-state.json")),
		Routes:         NewRouteStore(filepath.Join(dir, "t3-routes.json")),
		PollHold:       50 * time.Millisecond,
		LateReplyGrace: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- br.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	for _, ev := range WatchedMilestoneEvents {
		fake.waitStreamReady(ev)
	}

	// AE1: Grok Bot asks t3 with project directive.
	sent, err := grok.Send(ctx, "t3", "project: agent-tincan\nSummarize the last 3 commits", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatalf("grok.Send: %v", err)
	}

	// Wait for launch call.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := len(fake.launchCalls)
		fake.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	fake.mu.Lock()
	if len(fake.launchCalls) != 1 || fake.launchCalls[0]["projectId"] != "proj-1" {
		t.Fatalf("unexpected launch calls: %+v", fake.launchCalls)
	}
	// AE2a: Emit an UNLISTED permission prompt -> must NOT call respond or needs_input.
	fake.pendingIDs["thr-1"] = nil
	fake.mu.Unlock()

	fake.emit("thread.needs_input", EventOccurrence{
		EventID: "evt_perm_1",
		Cursor:  "c1",
		Data:    json.RawMessage(`{"type":"thread.needs_input","threadId":"thr-1","requestId":"perm-99","kind":"approval"}`),
	})
	time.Sleep(120 * time.Millisecond)
	fake.mu.Lock()
	if len(fake.respondCalls) != 0 {
		t.Fatalf("bridge attempted to approve permission prompt: %+v", fake.respondCalls)
	}
	// AE2b: Emit a LISTED user question -> bridge posts needs_input and forwards grok's answer.
	fake.pendingIDs["thr-1"] = []string{"q-42"}
	var pq PendingQuestion
	_ = json.Unmarshal([]byte(`{"requestId":"q-42","questions":[{"header":"Scope","question":"Include merge commits?"}]}`), &pq)
	fake.pendingDetails["q-42"] = pq
	fake.mu.Unlock()

	fake.emit("thread.needs_input", EventOccurrence{
		EventID: "evt_q_1",
		Cursor:  "c2",
		Data:    json.RawMessage(`{"type":"thread.needs_input","threadId":"thr-1","requestId":"q-42","kind":"question"}`),
	})

	res, err := grok.Get(ctx, sent.ID, 3*time.Second)
	if err != nil || res.Status != envelope.StatusNeedsInput {
		t.Fatalf("want needs_input, got status=%s err=%v", res.Status, err)
	}
	if !strings.Contains(res.Reply.Body, "Include merge commits?") {
		t.Fatalf("question missing in reply body: %q", res.Reply.Body)
	}

	// Grok answers the question.
	if _, err := grok.Answer(ctx, sent.ID, "No, skip merge commits"); err != nil {
		t.Fatalf("grok.Answer: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := len(fake.respondCalls)
		fake.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	fake.mu.Lock()
	if len(fake.respondCalls) != 1 || fake.respondCalls[0]["requestId"] != "q-42" {
		t.Fatalf("expected respond call for q-42, got %+v", fake.respondCalls)
	}
	// Mark thread completed with final assistant message and emit thread.run_settled.
	tr := fake.threads["thr-1"]
	tr.Thread.Status = "idle"
	tr.Thread.ActiveRunID = ""
	tr.Items = []ThreadItem{{Type: "assistant_message", CreatedBy: "agent", Text: "Summary of 3 commits."}}
	fake.threads["thr-1"] = tr
	fake.mu.Unlock()

	fake.emit("thread.run_settled", EventOccurrence{
		EventID: "evt_settle_1",
		Cursor:  "c3",
		Data:    json.RawMessage(`{"type":"thread.run_settled","threadId":"thr-1","outcome":"completed"}`),
	})

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r2, err := grok.Get(ctx, sent.ID, 100*time.Millisecond)
		if err == nil && r2.Status == envelope.StatusAnswered {
			if !strings.Contains(r2.Reply.Body, "Summary of 3 commits.") || !strings.Contains(r2.Reply.Body, "https://app.t3.codes/threads/env-1/thr-1") {
				t.Fatalf("unexpected final reply: %q", r2.Reply.Body)
			}
			return
		}
	}
	t.Fatal("timed out waiting for answered reply")
}

func TestBridgeWatchAlreadySettledAndGuestRefusal(t *testing.T) {
	mesh := testrelay.NewWithAdminLogin(t, relay.Config{})
	t3Relay := mesh.JoinOnMachineOf(t, "muse", "t3")
	grok := mesh.Client(t, "grokbot")
	guest := mesh.JoinGuest(t, "guest1", client.GuestInvite{Ask: []string{"t3"}, ForSeconds: 3600, NoHold: true})

	fake := newFakeT3Server(t)
	fake.threads["thr-done"] = ThreadReadResult{
		Thread: ThreadSummary{
			ThreadID: "thr-done",
			Title:    "Overnight build",
			Link:     "https://app.t3.codes/threads/env-1/thr-done",
			Status:   "idle",
		},
		Items: []ThreadItem{{Type: "assistant_message", CreatedBy: "agent", Text: "Build succeeded."}},
	}
	mcpCli, _ := NewMCPClient(fake.srv.URL, "tok", "0.34.0")
	dir := t.TempDir()
	br, err := NewBridge(Config{
		Relay:          t3Relay,
		T3:             mcpCli,
		State:          NewStateStore(filepath.Join(dir, "t3-state.json")),
		Routes:         NewRouteStore(filepath.Join(dir, "t3-routes.json")),
		PollHold:       50 * time.Millisecond,
		LateReplyGrace: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- br.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	// AE5: Guest watch directive must be declined with ZERO T3 read calls.
	gRes, err := guest.Ask(ctx, "t3", "watch https://app.t3.codes/threads/env-1/thr-done", "", 3*time.Second, false)
	if err != nil || gRes.Status != envelope.StatusDeclined {
		t.Fatalf("guest watch: status=%s err=%v, want declined", gRes.Status, err)
	}
	fake.mu.Lock()
	if len(fake.readCalls) != 0 {
		t.Fatalf("guest caused T3 read calls: %v", fake.readCalls)
	}
	fake.mu.Unlock()

	// AE3: Teammate watch on already-settled thread replies immediately.
	res, err := grok.Ask(ctx, "t3", "watch https://app.t3.codes/threads/env-1/thr-done", "", 3*time.Second, false)
	if err != nil || res.Status != envelope.StatusAnswered {
		t.Fatalf("watch already-settled: status=%s err=%v", res.Status, err)
	}
	if !strings.Contains(res.Reply.Body, "Build succeeded.") {
		t.Fatalf("watch reply missing content: %q", res.Reply.Body)
	}
}

func TestBridgeLateReplyRoutingAndIdempotency(t *testing.T) {
	mesh := testrelay.New(t, relay.Config{})
	t3Relay := mesh.JoinOnMachineOf(t, "muse", "t3")
	grok := mesh.Client(t, "grokbot")

	fake := newFakeT3Server(t)
	mcpCli, _ := NewMCPClient(fake.srv.URL, "tok", "0.34.0")
	dir := t.TempDir()
	routes := NewRouteStore(filepath.Join(dir, "t3-routes.json"))
	br, err := NewBridge(Config{
		Relay:          t3Relay,
		T3:             mcpCli,
		State:          NewStateStore(filepath.Join(dir, "t3-state.json")),
		Routes:         routes,
		PollHold:       50 * time.Millisecond,
		LateReplyGrace: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an outbound ask sent by a T3 thread (sessionless per KTD4) and recorded in t3-routes.json.
	outboundClient := mesh.Client(t, "t3")
	sent, err := outboundClient.Send(context.Background(), "grokbot", "check deploy status", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := routes.Record(sent.ID, "thr-origin-7", "grokbot"); err != nil {
		t.Fatal(err)
	}

	// Grok Bot claims and replies later after the T3 thread went idle.
	in, err := grok.Poll(context.Background(), 50*time.Millisecond)
	if err != nil || len(in.Requests) != 1 {
		t.Fatalf("grok poll: %+v, %v", in, err)
	}
	if _, err := grok.Claim(context.Background(), sent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := grok.Reply(context.Background(), sent.ID, "Deploy is green.", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- br.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	// AE4: Bridge delivers reply back into thr-origin-7 once and acks it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := len(fake.sendCalls)
		fake.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sendCalls) != 1 {
		t.Fatalf("expected exactly 1 t3_thread_send call, got %d: %+v", len(fake.sendCalls), fake.sendCalls)
	}
	if fake.sendCalls[0]["threadId"] != "thr-origin-7" || !strings.Contains(fake.sendCalls[0]["message"].(string), "Deploy is green.") {
		t.Fatalf("unexpected send call: %+v", fake.sendCalls[0])
	}
}
