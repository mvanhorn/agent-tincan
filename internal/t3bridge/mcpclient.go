package t3bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// ProtocolVersion20260728 is the stateless MCP Streamable HTTP revision
	// T3 Code Nightly serves for tools and SEP-3415 MCP Events.
	ProtocolVersion20260728 = "2026-07-28"

	defaultCallTimeout      = 30 * time.Second
	defaultHeartbeatMs      = 15000
	maxRPCResponseBytes     = 16 << 20
	maxSSEScannerTokenBytes = 4 << 20
)

// AuthError indicates T3 Code rejected the bearer credential (401/403).
// It never includes the raw token.
type AuthError struct {
	Status int
	Detail string
}

func (e *AuthError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("T3 Code authentication rejected (HTTP %d): %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("T3 Code authentication rejected (HTTP %d)", e.Status)
}

// RPCError is a JSON-RPC error returned by T3 Code's /mcp endpoint.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("T3 MCP error %d: %s", e.Code, e.Message)
}

// MCPClient talks to a T3 Code server's /mcp endpoint using stateless
// 2026-07-28 Streamable HTTP JSON-RPC and SSE.
type MCPClient struct {
	origin     string
	token      string
	version    string
	httpClient *http.Client
	streamHTTP *http.Client
	nextID     atomic.Int64
}

// NewMCPClient creates a client for origin (validated as loopback http or https).
func NewMCPClient(origin, token, clientVersion string) (*MCPClient, error) {
	validOrigin, err := ValidateOrigin(origin)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("T3 MCP bearer token is empty")
	}
	if clientVersion == "" {
		clientVersion = "0.0.1-dev"
	}
	return &MCPClient{
		origin:     validOrigin,
		token:      token,
		version:    clientVersion,
		httpClient: newNoRedirectClient(defaultCallTimeout),
		streamHTTP: newNoRedirectClient(0),
	}, nil
}

// Origin returns the validated T3 server origin.
func (c *MCPClient) Origin() string { return c.origin }

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int64          `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// ServerInfo is returned in _meta["io.modelcontextprotocol/serverInfo"].
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// EventDescriptor is one entry returned by events/list.
type EventDescriptor struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Delivery    []string `json:"delivery"`
}

// ListEventsResult is the decoded response from events/list.
type ListEventsResult struct {
	Events     []EventDescriptor
	ServerInfo ServerInfo
}

// ListEvents queries the server's SEP-3415 event catalog and verifies server identity.
func (c *MCPClient) ListEvents(ctx context.Context) (ListEventsResult, error) {
	raw, err := c.doRPC(ctx, "events/list", "", map[string]any{})
	if err != nil {
		return ListEventsResult{}, err
	}
	var res struct {
		Meta struct {
			ServerInfo ServerInfo `json:"io.modelcontextprotocol/serverInfo"`
		} `json:"_meta"`
		Events []EventDescriptor `json:"events"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return ListEventsResult{}, fmt.Errorf("decode events/list: %w", err)
	}
	return ListEventsResult{Events: res.Events, ServerInfo: res.Meta.ServerInfo}, nil
}

// EventOccurrence is one delivered SEP-3415 event from events/poll or events/stream.
type EventOccurrence struct {
	EventID   string          `json:"eventId"`
	Timestamp string          `json:"timestamp"`
	Cursor    string          `json:"cursor"`
	Data      json.RawMessage `json:"data"`
}

// MilestoneData is the parsed payload common to T3 Code thread/PR milestones.
type MilestoneData struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	OccurredAt     string `json:"occurredAt"`
	ProjectID      string `json:"projectId"`
	ThreadID       string `json:"threadId"`
	ParentThreadID string `json:"parentThreadId"`
	RunID          string `json:"runId"`
	Outcome        string `json:"outcome"`
	RequestID      string `json:"requestId"`
	Kind           string `json:"kind"`
	Resolution     string `json:"resolution"`
	Detail         string `json:"detail"`
}

// ParseMilestone decodes an EventOccurrence's Data field.
func (e EventOccurrence) ParseMilestone() (MilestoneData, error) {
	var m MilestoneData
	if err := json.Unmarshal(e.Data, &m); err != nil {
		return MilestoneData{}, fmt.Errorf("decode milestone %s: %w", e.EventID, err)
	}
	return m, nil
}

// PollEventsResult is returned by events/poll.
type PollEventsResult struct {
	Events     []EventOccurrence `json:"events"`
	Cursor     string            `json:"cursor"`
	Truncated  bool              `json:"truncated"`
	HasMore    bool              `json:"hasMore"`
	NextPollMs int               `json:"nextPollMs"`
}

// PollEvents reads up to maxEvents occurrences of eventName after cursor.
func (c *MCPClient) PollEvents(ctx context.Context, eventName, cursor string, args map[string]any, maxEvents int) (PollEventsResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	params := map[string]any{
		"name":      eventName,
		"arguments": args,
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	if maxEvents > 0 {
		params["maxEvents"] = maxEvents
	}
	raw, err := c.doRPC(ctx, "events/poll", eventName, params)
	if err != nil {
		return PollEventsResult{}, err
	}
	var out PollEventsResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return PollEventsResult{}, fmt.Errorf("decode events/poll (%s): %w", eventName, err)
	}
	return out, nil
}

// StreamHandlers receives SEP-3415 push notifications from StreamEvents.
type StreamHandlers struct {
	OnActive    func(cursor string, truncated bool, heartbeatMs int)
	OnEvent     func(ev EventOccurrence)
	OnHeartbeat func(cursor string)
}

// ProbeEventStream verifies that events/stream completes its initial
// notifications/events/active handshake for eventName and closes cleanly.
func (c *MCPClient) ProbeEventStream(ctx context.Context, eventName string) error {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var active atomic.Bool
	err := c.StreamEvents(probeCtx, eventName, "", map[string]any{}, 5000, StreamHandlers{
		OnActive: func(string, bool, int) {
			active.Store(true)
			cancel()
		},
	})
	if active.Load() {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("events/stream (%s) closed before notifications/events/active", eventName)
}

// StreamEvents opens a persistent SSE push stream for eventName and blocks
// until ctx is canceled, the server terminates the stream, or a heartbeat stalls.
func (c *MCPClient) StreamEvents(ctx context.Context, eventName, cursor string, args map[string]any, heartbeatMs int, h StreamHandlers) error {
	if args == nil {
		args = map[string]any{}
	}
	if heartbeatMs <= 0 {
		heartbeatMs = defaultHeartbeatMs
	}
	params := map[string]any{
		"name":        eventName,
		"arguments":   args,
		"heartbeatMs": heartbeatMs,
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	body, err := c.encodeRequest("events/stream", params)
	if err != nil {
		return err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.origin+"/mcp", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(req, "events/stream", eventName)

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("open events/stream (%s): %w", eventName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &AuthError{Status: resp.StatusCode}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(ct, "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var rpcResp rpcResponse
		if json.Unmarshal(raw, &rpcResp) == nil && rpcResp.Error != nil {
			return rpcResp.Error
		}
		return fmt.Errorf("events/stream (%s) returned HTTP %d (%s)", eventName, resp.StatusCode, ct)
	}

	// Watchdog: cancel the stream if no SSE traffic arrives for 3x heartbeatMs.
	watchdogDur := max(time.Duration(heartbeatMs)*3*time.Millisecond, 15*time.Second)
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var stalled atomic.Bool

	doneWatch := make(chan struct{})
	defer close(doneWatch)
	go func() {
		tick := time.NewTicker(max(watchdogDur/3, 50*time.Millisecond))
		defer tick.Stop()
		for {
			select {
			case <-doneWatch:
				return
			case <-streamCtx.Done():
				return
			case <-tick.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > watchdogDur {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSEScannerTokenBytes)
	var dataLines []string

	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		return dispatchSSENotification(payload, h)
	}

	for scanner.Scan() {
		lastActivity.Store(time.Now().UnixNano())
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimPrefix(v, " "))
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if stalled.Load() {
		return fmt.Errorf("events/stream (%s) heartbeat stalled", eventName)
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read events/stream (%s): %w", eventName, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return io.EOF
}

func dispatchSSENotification(payload string, h StreamHandlers) error {
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Error  *RPCError       `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		return fmt.Errorf("decode SSE notification: %w", err)
	}
	if msg.Error != nil {
		return msg.Error
	}
	switch msg.Method {
	case "notifications/events/active":
		var p struct {
			Cursor      string `json:"cursor"`
			Truncated   bool   `json:"truncated"`
			HeartbeatMs int    `json:"heartbeatMs"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return fmt.Errorf("decode events/active: %w", err)
		}
		if h.OnActive != nil {
			h.OnActive(p.Cursor, p.Truncated, p.HeartbeatMs)
		}
	case "notifications/events/event":
		var ev EventOccurrence
		if err := json.Unmarshal(msg.Params, &ev); err != nil {
			return fmt.Errorf("decode events/event: %w", err)
		}
		if h.OnEvent != nil {
			h.OnEvent(ev)
		}
	case "notifications/events/heartbeat":
		var p struct {
			Cursor string `json:"cursor"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return fmt.Errorf("decode events/heartbeat: %w", err)
		}
		if h.OnHeartbeat != nil {
			h.OnHeartbeat(p.Cursor)
		}
	case "notifications/events/error":
		var p struct {
			Error *RPCError `json:"error"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.Error != nil {
			return p.Error
		}
		return errors.New("T3 event stream reported an error")
	case "notifications/events/terminated":
		var p struct {
			Error *RPCError `json:"error"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.Error != nil {
			return fmt.Errorf("T3 event stream terminated: %w", p.Error)
		}
		return errors.New("T3 event stream terminated")
	}
	return nil
}

// CallTool invokes a T3 MCP tool and decodes its structured JSON result into out.
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any, out any) error {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := c.doRPC(ctx, "tools/call", name, map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return err
	}
	var res struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("decode tools/call (%s): %w", name, err)
	}
	if res.IsError {
		msg := "tool returned an error"
		if len(res.Content) > 0 && res.Content[0].Text != "" {
			msg = res.Content[0].Text
		}
		return fmt.Errorf("T3 tool %s failed: %s", name, msg)
	}
	if out == nil {
		return nil
	}
	if len(res.StructuredContent) > 0 && string(res.StructuredContent) != "null" {
		if err := json.Unmarshal(res.StructuredContent, out); err == nil {
			return nil
		}
	}
	for _, block := range res.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			if err := json.Unmarshal([]byte(block.Text), out); err == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("T3 tool %s returned no decodable JSON payload", name)
}

func (c *MCPClient) doRPC(ctx context.Context, method, mcpName string, params map[string]any) (json.RawMessage, error) {
	body, err := c.encodeRequest(method, params)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, method, mcpName)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("T3 MCP %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &AuthError{Status: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read T3 MCP %s response: %w", method, err)
	}
	var rpcResp rpcResponse
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("T3 MCP %s returned HTTP %d", method, resp.StatusCode)
		}
		return nil, fmt.Errorf("decode T3 MCP %s JSON-RPC: %w", method, err)
	}
	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("T3 MCP %s returned HTTP %d", method, resp.StatusCode)
	}
	return rpcResp.Result, nil
}

func (c *MCPClient) encodeRequest(method string, params map[string]any) ([]byte, error) {
	p := make(map[string]any, len(params)+1)
	maps.Copy(p, params)
	p["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    ProtocolVersion20260728,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo": map[string]any{
			"name":    "agent-tincan",
			"version": c.version,
		},
	}
	return json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextID.Add(1),
		Method:  method,
		Params:  p,
	})
}

func (c *MCPClient) setHeaders(req *http.Request, method, mcpName string) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion20260728)
	req.Header.Set("MCP-Method", method)
	if mcpName != "" {
		req.Header.Set("MCP-Name", mcpName)
	}
}

// --- Typed T3 Code tool wrappers ---

// T3Project is one entry returned by t3_project_list.
type T3Project struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspaceRoot"`
}

// ProjectListResult is the decoded response from t3_project_list.
type ProjectListResult struct {
	Projects []T3Project `json:"projects"`
}

// ListProjects returns T3 Code's registered projects.
func (c *MCPClient) ListProjects(ctx context.Context) ([]T3Project, error) {
	var out ProjectListResult
	if err := c.CallTool(ctx, "t3_project_list", map[string]any{"limit": 100}, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

// LaunchThreadInput configures a t3_thread_launch call.
type LaunchThreadInput struct {
	Title           string
	Message         string
	ProjectID       string
	Scratch         bool
	ClientRequestID string
	Attachments     []T3AttachmentRef
}

// T3AttachmentRef is an uploaded attachment descriptor accepted by T3 thread tools.
type T3AttachmentRef struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
}

// LaunchThreadResult is the decoded response from t3_thread_launch.
type LaunchThreadResult struct {
	ThreadID string `json:"threadId"`
	Link     string `json:"link"`
	Title    string `json:"title"`
	RunID    string `json:"runId"`
	Thread   *struct {
		ThreadID string `json:"threadId"`
		Link     string `json:"link"`
		Title    string `json:"title"`
	} `json:"thread,omitempty"`
}

// ResolvedThreadID returns the thread id from either top-level or nested thread fields.
func (r LaunchThreadResult) ResolvedThreadID() string {
	if r.ThreadID != "" {
		return r.ThreadID
	}
	if r.Thread != nil {
		return r.Thread.ThreadID
	}
	return ""
}

// ResolvedLink returns the thread link from either top-level or nested thread fields.
func (r LaunchThreadResult) ResolvedLink() string {
	if r.Link != "" {
		return r.Link
	}
	if r.Thread != nil {
		return r.Thread.Link
	}
	return ""
}

// LaunchThread starts a new T3 Code thread.
func (c *MCPClient) LaunchThread(ctx context.Context, in LaunchThreadInput) (LaunchThreadResult, error) {
	args := map[string]any{
		"title":   in.Title,
		"message": in.Message,
	}
	if in.ClientRequestID != "" {
		args["clientRequestId"] = in.ClientRequestID
	}
	if in.ProjectID != "" {
		args["projectId"] = in.ProjectID
	} else if in.Scratch {
		args["scratch"] = true
	}
	if len(in.Attachments) > 0 {
		args["attachments"] = in.Attachments
	}
	var out LaunchThreadResult
	if err := c.CallTool(ctx, "t3_thread_launch", args, &out); err != nil {
		return LaunchThreadResult{}, err
	}
	return out, nil
}

// SendThreadMessage sends a message into an existing T3 thread with mode "auto".
func (c *MCPClient) SendThreadMessage(ctx context.Context, threadID, message, clientRequestID string) error {
	args := map[string]any{
		"threadId": threadID,
		"message":  message,
		"mode":     "auto",
	}
	if clientRequestID != "" {
		args["clientRequestId"] = clientRequestID
	}
	return c.CallTool(ctx, "t3_thread_send", args, nil)
}

// ThreadSummary is the thread object inside t3_thread_read.
type ThreadSummary struct {
	ThreadID            string `json:"threadId"`
	ProjectID           string `json:"projectId"`
	Title               string `json:"title"`
	Link                string `json:"link"`
	Status              string `json:"status"`
	ActiveRunID         string `json:"activeRunId"`
	LatestRunID         string `json:"latestRunId"`
	PendingRequestCount int    `json:"pendingRequestCount"`
	Settled             bool   `json:"settled"`
}

// ThreadRun is one entry in t3_thread_read.recentRuns.
type ThreadRun struct {
	RunID   string `json:"runId"`
	Ordinal int    `json:"ordinal"`
	Status  string `json:"status"`
}

// ThreadItem is one message item in t3_thread_read.items.
type ThreadItem struct {
	Position      int    `json:"position"`
	ItemID        string `json:"itemId"`
	RunID         string `json:"runId"`
	CreatedBy     string `json:"createdBy"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	Text          string `json:"text"`
	TextTruncated bool   `json:"textTruncated"`
}

// ThreadReadResult is the decoded response from t3_thread_read.
type ThreadReadResult struct {
	Thread     ThreadSummary `json:"thread"`
	RecentRuns []ThreadRun   `json:"recentRuns"`
	Items      []ThreadItem  `json:"items"`
}

// LatestAssistantText returns the newest non-empty assistant message text.
func (r ThreadReadResult) LatestAssistantText() string {
	for _, it := range slices.Backward(r.Items) {
		if (it.Type == "assistant_message" || it.CreatedBy == "agent") && strings.TrimSpace(it.Text) != "" {
			return strings.TrimSpace(it.Text)
		}
	}
	return ""
}

// IsRunning reports whether the thread currently has an active run.
func (r ThreadReadResult) IsRunning() bool {
	if r.Thread.ActiveRunID != "" {
		return true
	}
	switch strings.ToLower(r.Thread.Status) {
	case "running", "starting", "preparing", "queued":
		return true
	}
	return false
}

// ReadThread fetches a thread's state and latest messages.
func (c *MCPClient) ReadThread(ctx context.Context, threadID string, limit int) (ThreadReadResult, error) {
	if limit <= 0 {
		limit = 10
	}
	var out ThreadReadResult
	err := c.CallTool(ctx, "t3_thread_read", map[string]any{
		"threadId":        threadID,
		"view":            "messages",
		"limit":           limit,
		"maxCharsPerItem": 20000,
	}, &out)
	return out, err
}

// PendingRequestList is the response from t3_pending_request(action: "list").
// Note: T3 Code omits permission/approval prompts from this list.
type PendingRequestList struct {
	RequestIDs []string `json:"requestIds"`
}

// ListPendingQuestions returns answerable pending user question IDs for threadID.
func (c *MCPClient) ListPendingQuestions(ctx context.Context, threadID string) ([]string, error) {
	var out PendingRequestList
	err := c.CallTool(ctx, "t3_pending_request", map[string]any{
		"action":   "list",
		"threadId": threadID,
	}, &out)
	return out.RequestIDs, err
}

// PendingQuestion holds the prompt details from t3_pending_request(action: "read").
type PendingQuestion struct {
	RequestID string `json:"requestId"`
	Questions []struct {
		Header   string `json:"header"`
		Question string `json:"question"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
	Raw json.RawMessage `json:"-"`
}

// ReadPendingQuestion reads one answerable user question from a T3 thread.
func (c *MCPClient) ReadPendingQuestion(ctx context.Context, threadID, requestID string) (PendingQuestion, error) {
	var raw json.RawMessage
	err := c.CallTool(ctx, "t3_pending_request", map[string]any{
		"action":    "read",
		"threadId":  threadID,
		"requestId": requestID,
	}, &raw)
	if err != nil {
		return PendingQuestion{}, err
	}
	var pq PendingQuestion
	_ = json.Unmarshal(raw, &pq)
	pq.Raw = raw
	if pq.RequestID == "" {
		pq.RequestID = requestID
	}
	return pq, nil
}

// RespondPendingQuestion answers one pending user question (never approves permissions).
func (c *MCPClient) RespondPendingQuestion(ctx context.Context, threadID, requestID string, answers map[string]any) error {
	return c.CallTool(ctx, "t3_pending_request", map[string]any{
		"action":    "respond",
		"threadId":  threadID,
		"requestId": requestID,
		"answers":   answers,
	}, nil)
}

// UploadAttachment uploads bytes to T3 Code and returns a T3AttachmentRef.
func (c *MCPClient) UploadAttachment(ctx context.Context, name, mimeType string, data []byte) (T3AttachmentRef, error) {
	kind := "file"
	if strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		kind = "image"
	}
	var prep struct {
		AttachmentID string `json:"attachmentId"`
		UploadURL    string `json:"uploadUrl"`
		RelativeURL  string `json:"relativeUrl"`
	}
	err := c.CallTool(ctx, "t3_attachment_prepare_upload", map[string]any{
		"upload": map[string]any{
			"type":      kind,
			"name":      name,
			"mimeType":  mimeType,
			"sizeBytes": len(data),
		},
	}, &prep)
	if err != nil {
		return T3AttachmentRef{}, err
	}
	target := prep.UploadURL
	if target == "" && prep.RelativeURL != "" {
		target = c.origin + "/" + strings.TrimLeft(prep.RelativeURL, "/")
	}
	if err := validateSameOriginEndpoint(c.origin, target); err != nil {
		return T3AttachmentRef{}, fmt.Errorf("attachment upload URL rejected: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return T3AttachmentRef{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mimeType)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return T3AttachmentRef{}, fmt.Errorf("upload attachment bytes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return T3AttachmentRef{}, fmt.Errorf("upload attachment returned HTTP %d", resp.StatusCode)
	}
	return T3AttachmentRef{
		Type:      kind,
		ID:        prep.AttachmentID,
		Name:      name,
		MimeType:  mimeType,
		SizeBytes: int64(len(data)),
	}, nil
}
