package t3bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const (
	// BridgeSession is the X-Tincan-Session used by tincan t3 serve.
	BridgeSession = "t3-serve"

	defaultPollHold       = 25 * time.Second
	defaultLateReplyGrace = 5 * time.Second
	maxReplyBodyChars     = 16000
	maxSeenEventIDs       = 2048
)

// WatchedMilestoneEvents are the four T3 Code SEP-3415 event types the bridge streams.
var WatchedMilestoneEvents = []string{
	"thread.run_settled",
	"thread.needs_input",
	"thread.input_resolved",
	"pr.watch_wake",
}

// RelayClient is the subset of *client.Relay the bridge uses.
type RelayClient interface {
	SetSession(session string)
	Poll(ctx context.Context, hold time.Duration) (client.Inbox, error)
	Claim(ctx context.Context, id string) (envelope.Request, error)
	Progress(ctx context.Context, id, note string) error
	Reply(ctx context.Context, id, body string, status envelope.Status) (envelope.Reply, error)
	Send(ctx context.Context, to, body string, kind envelope.Kind, parent string, urgent bool) (envelope.Request, error)
	AckReplies(ctx context.Context, ids []string, acks ...envelope.ReplyAck) error
	FetchAttachment(ctx context.Context, id string) ([]byte, client.DownloadedAttachment, error)
}

// Config configures a Bridge instance.
type Config struct {
	Relay          RelayClient
	T3             *MCPClient
	State          *StateStore
	Routes         *RouteStore
	DefaultProject string
	Logger         *log.Logger
	PollHold       time.Duration
	LateReplyGrace time.Duration
	HeartbeatMs    int
	Now            func() time.Time
}

// Bridge connects the Agent Tincan relay to T3 Code's /mcp server and SEP-3415 event streams.
type Bridge struct {
	cfg      Config
	seenMu   sync.Mutex
	seenIDs  map[string]struct{}
	seenList []string
}

// NewBridge creates a Bridge from cfg.
func NewBridge(cfg Config) (*Bridge, error) {
	if cfg.Relay == nil {
		return nil, errors.New("bridge requires a Relay client")
	}
	if cfg.T3 == nil {
		return nil, errors.New("bridge requires a T3 MCP client")
	}
	if cfg.State == nil {
		return nil, errors.New("bridge requires a StateStore")
	}
	if cfg.Routes == nil {
		return nil, errors.New("bridge requires a RouteStore")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.PollHold <= 0 {
		cfg.PollHold = defaultPollHold
	}
	if cfg.LateReplyGrace < 0 {
		cfg.LateReplyGrace = 0
	}
	if cfg.HeartbeatMs <= 0 {
		cfg.HeartbeatMs = defaultHeartbeatMs
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Bridge{
		cfg:     cfg,
		seenIDs: map[string]struct{}{},
	}, nil
}

// Run starts the relay poller and the 4 SEP-3415 event stream workers, blocking
// until ctx is canceled.
func (b *Bridge) Run(ctx context.Context) error {
	b.cfg.Relay.SetSession(BridgeSession)

	var wg sync.WaitGroup
	for _, evName := range WatchedMilestoneEvents {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			b.runEventStream(ctx, name)
		}(evName)
	}

	b.runRelayLoop(ctx)
	wg.Wait()
	return ctx.Err()
}

func (b *Bridge) runRelayLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		inbox, err := b.cfg.Relay.Poll(ctx, b.cfg.PollHold)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.cfg.Logger.Printf("t3 bridge: relay poll failed: %v", err)
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		for _, req := range inbox.Requests {
			b.handleInboundRequest(ctx, req)
		}
		if len(inbox.Replies) > 0 {
			b.handleUnseenReplies(ctx, inbox.Replies)
		}
	}
}

func (b *Bridge) runEventStream(ctx context.Context, eventName string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		cursor := b.loadCursor(eventName)
		needPoll := false

		err := b.cfg.T3.StreamEvents(ctx, eventName, cursor, map[string]any{}, b.cfg.HeartbeatMs, StreamHandlers{
			OnActive: func(c string, truncated bool, _ int) {
				backoff = time.Second
				if truncated {
					needPoll = true
				}
				if c != "" {
					b.saveCursor(eventName, c)
				}
			},
			OnEvent: func(ev EventOccurrence) {
				if ev.Cursor != "" {
					b.saveCursor(eventName, ev.Cursor)
				}
				b.handleMilestoneEvent(ctx, eventName, ev)
			},
			OnHeartbeat: func(c string) {
				if c != "" {
					b.saveCursor(eventName, c)
				}
			},
		})
		if ctx.Err() != nil {
			return
		}
		if needPoll || err != nil {
			b.pollCatchUp(ctx, eventName)
		}
		if err != nil {
			if authErr, ok := errors.AsType[*AuthError](err); ok {
				b.cfg.Logger.Printf("t3 bridge: T3 auth rejected on %s (%v); run tincan t3 auth", eventName, authErr)
				sleepCtx(ctx, 30*time.Second)
				continue
			}
			b.cfg.Logger.Printf("t3 bridge: stream %s ended (%v); reconnecting in %s", eventName, err, backoff)
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

func (b *Bridge) pollCatchUp(ctx context.Context, eventName string) {
	cursor := b.loadCursor(eventName)
	for range 5 {
		if ctx.Err() != nil {
			return
		}
		res, err := b.cfg.T3.PollEvents(ctx, eventName, cursor, map[string]any{}, 50)
		if err != nil {
			b.cfg.Logger.Printf("t3 bridge: poll catch-up (%s) failed: %v", eventName, err)
			return
		}
		for _, ev := range res.Events {
			b.handleMilestoneEvent(ctx, eventName, ev)
		}
		if res.Cursor != "" {
			cursor = res.Cursor
			b.saveCursor(eventName, cursor)
		}
		if !res.HasMore {
			return
		}
	}
}

func (b *Bridge) loadCursor(eventName string) string {
	snap, err := b.cfg.State.Snapshot()
	if err != nil {
		return ""
	}
	return snap.Cursors[eventName]
}

func (b *Bridge) saveCursor(eventName, cursor string) {
	_ = b.cfg.State.Update(func(d *StateData) error {
		d.Cursors[eventName] = cursor
		return nil
	})
}

func (b *Bridge) markEventSeen(eventID string) bool {
	if eventID == "" {
		return true
	}
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	if _, ok := b.seenIDs[eventID]; ok {
		return false
	}
	b.seenIDs[eventID] = struct{}{}
	b.seenList = append(b.seenList, eventID)
	if len(b.seenList) > maxSeenEventIDs {
		oldest := b.seenList[0]
		b.seenList = b.seenList[1:]
		delete(b.seenIDs, oldest)
	}
	return true
}

func (b *Bridge) handleInboundRequest(ctx context.Context, req envelope.Request) {
	claimed, err := b.cfg.Relay.Claim(ctx, req.ID)
	if err != nil {
		b.cfg.Logger.Printf("t3 bridge: claim %s failed: %v", req.ID, err)
		return
	}
	req = claimed

	// Check if this is a requeued clarification answer for an existing dispatched ask.
	snap, err := b.cfg.State.Snapshot()
	if err == nil {
		if existing, ok := snap.Asks[req.ID]; ok && !existing.Done && existing.ThreadID != "" {
			b.handleClarificationAnswer(ctx, req, existing)
			return
		}
	}

	dir := ParseDirective(req.Body, req.Project)
	if req.FromGuest {
		if inner := unwrapGuestText(req.Body); inner != "" {
			if innerDir := ParseDirective(inner, ""); innerDir.TargetsExistingThread() {
				dir = innerDir
			}
		}
	}

	// Guest isolation (R12): guests can never inspect, watch, or steer existing private T3 threads.
	if req.FromGuest && dir.TargetsExistingThread() {
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, "Declined: guests cannot read, watch, or message existing T3 Code threads.", envelope.StatusDeclined)
		return
	}

	switch dir.Kind {
	case DirectiveWatch:
		b.handleWatchDirective(ctx, req, dir)
	case DirectiveUnwatch:
		b.handleUnwatchDirective(ctx, req, dir)
	case DirectiveStatus:
		b.handleStatusDirective(ctx, req, dir)
	case DirectiveSend:
		b.handleSendDirective(ctx, req, dir)
	default:
		b.handleLaunchDirective(ctx, req, dir)
	}
}

func (b *Bridge) handleClarificationAnswer(ctx context.Context, req envelope.Request, existing DispatchedAsk) {
	answered := req.AnsweredExchanges()
	if answered <= existing.AnsweredCount || len(req.Exchanges) == 0 {
		return
	}
	answerText := ""
	for _, ex := range slices.Backward(req.Exchanges) {
		if strings.TrimSpace(ex.Answer) != "" {
			answerText = strings.TrimSpace(ex.Answer)
			break
		}
	}
	if answerText == "" {
		return
	}

	var sendErr error
	if existing.PendingRequestID != "" {
		// Verify the pending user question is still open in T3 before responding.
		pendingIDs, listErr := b.cfg.T3.ListPendingQuestions(ctx, existing.ThreadID)
		if listErr == nil && slices.Contains(pendingIDs, existing.PendingRequestID) {
			sendErr = b.cfg.T3.RespondPendingQuestion(ctx, existing.ThreadID, existing.PendingRequestID, map[string]any{
				"answer": answerText,
			})
		} else {
			clientReqID := fmt.Sprintf("tincan-answer-%s-%d", req.ID, answered)
			sendErr = b.cfg.T3.SendThreadMessage(ctx, existing.ThreadID, fmt.Sprintf("Answer from %s:\n%s", req.From, answerText), clientReqID)
		}
	} else {
		clientReqID := fmt.Sprintf("tincan-answer-%s-%d", req.ID, answered)
		sendErr = b.cfg.T3.SendThreadMessage(ctx, existing.ThreadID, fmt.Sprintf("Answer from %s:\n%s", req.From, answerText), clientReqID)
	}
	if sendErr != nil {
		b.cfg.Logger.Printf("t3 bridge: forward answer for %s to thread %s failed: %v", req.ID, existing.ThreadID, sendErr)
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, fmt.Sprintf("Failed to deliver answer to T3 thread %s: %v", threadDisplay(existing.ThreadID, existing.Link), sendErr), envelope.StatusFailed)
		b.markAskDone(req.ID)
		return
	}
	_ = b.cfg.State.Update(func(d *StateData) error {
		if a, ok := d.Asks[req.ID]; ok {
			a.AnsweredCount = answered
			a.PendingRequestID = ""
			a.UpdatedAt = b.cfg.Now().UTC()
			d.Asks[req.ID] = a
		}
		return nil
	})
	_ = b.cfg.Relay.Progress(ctx, req.ID, fmt.Sprintf("Forwarded answer to T3 thread %s.", threadDisplay(existing.ThreadID, existing.Link)))
}

func (b *Bridge) handleWatchDirective(ctx context.Context, req envelope.Request, dir Directive) {
	tr, err := b.cfg.T3.ReadThread(ctx, dir.ThreadID, 5)
	if err != nil {
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, fmt.Sprintf("Could not read T3 thread %s: %v", dir.ThreadID, err), envelope.StatusFailed)
		return
	}
	link := threadDisplay(dir.ThreadID, tr.Thread.Link)
	// R8: If the thread already settled before the watch started, reply immediately.
	if !tr.IsRunning() && tr.Thread.PendingRequestCount == 0 {
		msg := formatSettledSummary(tr, "completed", dir.Note)
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, msg, envelope.StatusAnswered)
		return
	}
	watch := ThreadWatch{
		ThreadID:    dir.ThreadID,
		NotifyAgent: req.From,
		RequestID:   req.ID,
		OneShot:     true,
		Note:        dir.Note,
		Events:      []string{"thread.run_settled", "thread.needs_input", "pr.watch_wake"},
		CreatedAt:   b.cfg.Now().UTC(),
	}
	if err := b.cfg.State.Update(func(d *StateData) error {
		d.Watches[dir.ThreadID] = append(d.Watches[dir.ThreadID], watch)
		return nil
	}); err != nil {
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, fmt.Sprintf("Could not save watch for %s: %v", link, err), envelope.StatusFailed)
		return
	}
	_ = b.cfg.Relay.Progress(ctx, req.ID, fmt.Sprintf("Watching T3 thread %s (%s). Will notify on settle, input, or PR watch.", link, tr.Thread.Title))
}

func (b *Bridge) handleUnwatchDirective(ctx context.Context, req envelope.Request, dir Directive) {
	removed := 0
	_ = b.cfg.State.Update(func(d *StateData) error {
		list := d.Watches[dir.ThreadID]
		var kept []ThreadWatch
		for _, w := range list {
			if w.NotifyAgent == req.From {
				removed++
				continue
			}
			kept = append(kept, w)
		}
		if len(kept) == 0 {
			delete(d.Watches, dir.ThreadID)
		} else {
			d.Watches[dir.ThreadID] = kept
		}
		return nil
	})
	_, _ = b.cfg.Relay.Reply(ctx, req.ID, fmt.Sprintf("Removed %d watch(es) on T3 thread %s.", removed, dir.ThreadID), envelope.StatusAnswered)
}

func (b *Bridge) handleStatusDirective(ctx context.Context, req envelope.Request, dir Directive) {
	tr, err := b.cfg.T3.ReadThread(ctx, dir.ThreadID, 5)
	if err != nil {
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, fmt.Sprintf("Could not read T3 thread %s: %v", dir.ThreadID, err), envelope.StatusFailed)
		return
	}
	body := fmt.Sprintf("T3 thread %s\nTitle: %s\nStatus: %s\nPending requests: %d",
		threadDisplay(dir.ThreadID, tr.Thread.Link), tr.Thread.Title, tr.Thread.Status, tr.Thread.PendingRequestCount)
	if txt := tr.LatestAssistantText(); txt != "" {
		body += "\n\nLatest message:\n" + truncateText(txt, 2000)
	}
	_, _ = b.cfg.Relay.Reply(ctx, req.ID, body, envelope.StatusAnswered)
}

func (b *Bridge) handleSendDirective(ctx context.Context, req envelope.Request, dir Directive) {
	if strings.TrimSpace(dir.Message) == "" {
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, "No message body provided after thread directive.", envelope.StatusFailed)
		return
	}
	clientReqID := "tincan-send-" + req.ID
	if err := b.cfg.T3.SendThreadMessage(ctx, dir.ThreadID, dir.Message, clientReqID); err != nil {
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, fmt.Sprintf("Failed to send message to T3 thread %s: %v", dir.ThreadID, err), envelope.StatusFailed)
		return
	}
	tr, _ := b.cfg.T3.ReadThread(ctx, dir.ThreadID, 1)
	link := threadDisplay(dir.ThreadID, tr.Thread.Link)
	now := b.cfg.Now().UTC()
	_ = b.cfg.State.Update(func(d *StateData) error {
		d.Asks[req.ID] = DispatchedAsk{
			RequestID: req.ID,
			From:      req.From,
			ThreadID:  dir.ThreadID,
			Link:      tr.Thread.Link,
			CreatedAt: now,
			UpdatedAt: now,
		}
		return nil
	})
	_ = b.cfg.Relay.Progress(ctx, req.ID, fmt.Sprintf("Sent message to T3 thread %s.", link))
}

func (b *Bridge) handleLaunchDirective(ctx context.Context, req envelope.Request, dir Directive) {
	// Resolve project (guests are always forced to isolated Scratch).
	projectID := ""
	scratch := true
	if !req.FromGuest {
		wantProj := dir.Project
		if wantProj == "" {
			wantProj = b.cfg.DefaultProject
		}
		if wantProj != "" {
			projects, err := b.cfg.T3.ListProjects(ctx)
			if err != nil {
				b.cfg.Logger.Printf("t3 bridge: list projects failed (falling back to Scratch): %v", err)
			} else if matched := matchProject(projects, wantProj); matched != "" {
				projectID = matched
				scratch = false
			}
		}
	}

	var uploaded []T3AttachmentRef
	for _, att := range req.Attachments {
		data, dl, err := b.cfg.Relay.FetchAttachment(ctx, att.ID)
		if err != nil {
			b.cfg.Logger.Printf("t3 bridge: download attachment %s failed: %v", att.ID, err)
			continue
		}
		mime := att.MIME
		if mime == "" {
			mime = dl.MIME
		}
		if mime == "" {
			mime = "application/octet-stream"
		}
		name := att.Name
		if name == "" {
			name = att.ID
		}
		ref, err := b.cfg.T3.UploadAttachment(ctx, name, mime, data)
		if err != nil {
			b.cfg.Logger.Printf("t3 bridge: upload attachment %s to T3 failed: %v", name, err)
			continue
		}
		uploaded = append(uploaded, ref)
	}

	title := makeThreadTitle(req.From, dir.Message)
	prompt := dir.Message
	if req.FromGuest && !strings.Contains(prompt, envelope.GuestLine) {
		prompt = envelope.GuestPrefix + envelope.GuestText(prompt)
	}

	res, err := b.cfg.T3.LaunchThread(ctx, LaunchThreadInput{
		Title:           title,
		Message:         prompt,
		ProjectID:       projectID,
		Scratch:         scratch,
		ClientRequestID: "tincan-ask-" + req.ID,
		Attachments:     uploaded,
	})
	if err != nil {
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, fmt.Sprintf("Failed to launch T3 thread: %v", err), envelope.StatusFailed)
		return
	}
	threadID := res.ResolvedThreadID()
	link := res.ResolvedLink()
	if threadID == "" {
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, "T3 Code launched a thread but returned no threadId.", envelope.StatusFailed)
		return
	}
	now := b.cfg.Now().UTC()
	_ = b.cfg.State.Update(func(d *StateData) error {
		d.Asks[req.ID] = DispatchedAsk{
			RequestID: req.ID,
			From:      req.From,
			ThreadID:  threadID,
			Link:      link,
			CreatedAt: now,
			UpdatedAt: now,
		}
		return nil
	})
	_ = b.cfg.Relay.Progress(ctx, req.ID, fmt.Sprintf("Started T3 thread: %s", threadDisplay(threadID, link)))
}

func (b *Bridge) handleMilestoneEvent(ctx context.Context, eventName string, ev EventOccurrence) {
	if !b.markEventSeen(ev.EventID) {
		return
	}
	m, err := ev.ParseMilestone()
	if err != nil {
		b.cfg.Logger.Printf("t3 bridge: %v", err)
		return
	}
	if m.Type == "" {
		m.Type = eventName
	}
	if m.ThreadID == "" {
		return
	}
	snap, err := b.cfg.State.Snapshot()
	if err != nil {
		return
	}

	switch m.Type {
	case "thread.run_settled":
		b.onThreadSettled(ctx, snap, m)
	case "thread.needs_input":
		b.onThreadNeedsInput(ctx, snap, m)
	case "thread.input_resolved":
		b.onThreadInputResolved(ctx, snap, m)
	case "pr.watch_wake":
		b.onPRWatchWake(ctx, snap, m)
	}
}

func (b *Bridge) onThreadSettled(ctx context.Context, snap StateData, m MilestoneData) {
	var matchingAsks []DispatchedAsk
	for _, a := range snap.Asks {
		if !a.Done && a.ThreadID == m.ThreadID {
			matchingAsks = append(matchingAsks, a)
		}
	}
	watches := snap.Watches[m.ThreadID]
	if len(matchingAsks) == 0 && len(watches) == 0 {
		return
	}

	tr, err := b.cfg.T3.ReadThread(ctx, m.ThreadID, 10)
	if err != nil {
		b.cfg.Logger.Printf("t3 bridge: read settled thread %s failed: %v", m.ThreadID, err)
	}
	status := envelope.StatusAnswered
	if m.Outcome != "" && m.Outcome != "completed" {
		status = envelope.StatusFailed
	}

	for _, ask := range matchingAsks {
		body := formatSettledSummary(tr, m.Outcome, "")
		if body == "" {
			body = fmt.Sprintf("T3 thread %s finished (%s).", threadDisplay(m.ThreadID, ask.Link), m.Outcome)
		}
		if _, err := b.cfg.Relay.Reply(ctx, ask.RequestID, body, status); err != nil {
			b.cfg.Logger.Printf("t3 bridge: reply to ask %s failed: %v", ask.RequestID, err)
			continue
		}
		b.markAskDone(ask.RequestID)
	}

	var remaining []ThreadWatch
	for _, w := range watches {
		if !watchWantsEvent(w, "thread.run_settled") {
			remaining = append(remaining, w)
			continue
		}
		body := formatSettledSummary(tr, m.Outcome, w.Note)
		if w.OneShot && w.RequestID != "" {
			if _, err := b.cfg.Relay.Reply(ctx, w.RequestID, body, status); err != nil {
				b.cfg.Logger.Printf("t3 bridge: reply to watch ask %s failed: %v", w.RequestID, err)
				remaining = append(remaining, w)
			}
			continue
		}
		if _, err := b.cfg.Relay.Send(ctx, w.NotifyAgent, body, envelope.KindAsk, "", false); err != nil {
			b.cfg.Logger.Printf("t3 bridge: notify %s for settled thread %s failed: %v", w.NotifyAgent, m.ThreadID, err)
		}
		if !w.OneShot {
			remaining = append(remaining, w)
		}
	}
	if len(watches) > 0 {
		_ = b.cfg.State.Update(func(d *StateData) error {
			if len(remaining) == 0 {
				delete(d.Watches, m.ThreadID)
			} else {
				d.Watches[m.ThreadID] = remaining
			}
			return nil
		})
	}
}

func (b *Bridge) onThreadNeedsInput(ctx context.Context, snap StateData, m MilestoneData) {
	var matchingAsks []DispatchedAsk
	for _, a := range snap.Asks {
		if !a.Done && a.ThreadID == m.ThreadID {
			matchingAsks = append(matchingAsks, a)
		}
	}
	watches := snap.Watches[m.ThreadID]
	if len(matchingAsks) == 0 && len(watches) == 0 {
		return
	}

	// R5 & R6: T3 omits permission/approval prompts from t3_pending_request(list).
	pendingIDs, err := b.cfg.T3.ListPendingQuestions(ctx, m.ThreadID)
	isUserQuestion := err == nil && m.RequestID != "" && slices.Contains(pendingIDs, m.RequestID)
	if !isUserQuestion && err == nil && m.RequestID == "" && len(pendingIDs) > 0 {
		m.RequestID = pendingIDs[0]
		isUserQuestion = true
	}

	var questionBody string
	if isUserQuestion {
		pq, readErr := b.cfg.T3.ReadPendingQuestion(ctx, m.ThreadID, m.RequestID)
		if readErr == nil {
			questionBody = formatPendingQuestion(m.ThreadID, pq)
		} else {
			questionBody = fmt.Sprintf("T3 thread %s asked a question (request %s).", m.ThreadID, m.RequestID)
		}
	}

	for _, ask := range matchingAsks {
		link := threadDisplay(m.ThreadID, ask.Link)
		if isUserQuestion {
			body := fmt.Sprintf("T3 thread %s needs your answer:\n\n%s", link, questionBody)
			if _, err := b.cfg.Relay.Reply(ctx, ask.RequestID, body, envelope.StatusNeedsInput); err != nil {
				b.cfg.Logger.Printf("t3 bridge: post needs_input for %s failed: %v", ask.RequestID, err)
				continue
			}
			_ = b.cfg.State.Update(func(d *StateData) error {
				if a, ok := d.Asks[ask.RequestID]; ok {
					a.PendingRequestID = m.RequestID
					a.UpdatedAt = b.cfg.Now().UTC()
					d.Asks[ask.RequestID] = a
				}
				return nil
			})
		} else {
			// R6: Permission/approval prompt - NEVER approve over Tincan.
			note := fmt.Sprintf("T3 thread %s is paused waiting for the owner to approve a permission prompt directly in T3 Code (permission prompts are never approved over Agent Tincan).", link)
			_ = b.cfg.Relay.Progress(ctx, ask.RequestID, note)
			_ = b.cfg.State.Update(func(d *StateData) error {
				if a, ok := d.Asks[ask.RequestID]; ok {
					a.PermissionNoted = true
					a.UpdatedAt = b.cfg.Now().UTC()
					d.Asks[ask.RequestID] = a
				}
				return nil
			})
		}
	}

	for _, w := range watches {
		if !watchWantsEvent(w, "thread.needs_input") {
			continue
		}
		var notice string
		if isUserQuestion {
			notice = fmt.Sprintf("Watched T3 thread %s needs input (reply via `thread: %s`):\n\n%s", m.ThreadID, m.ThreadID, questionBody)
		} else {
			notice = fmt.Sprintf("Watched T3 thread %s is waiting for owner permission approval directly in T3 Code (cannot be approved over Agent Tincan).", m.ThreadID)
		}
		if _, err := b.cfg.Relay.Send(ctx, w.NotifyAgent, notice, envelope.KindAsk, "", false); err != nil {
			b.cfg.Logger.Printf("t3 bridge: notify %s for needs_input on %s failed: %v", w.NotifyAgent, m.ThreadID, err)
		}
	}
}

func (b *Bridge) onThreadInputResolved(ctx context.Context, snap StateData, m MilestoneData) {
	for _, ask := range snap.Asks {
		if !ask.Done && ask.ThreadID == m.ThreadID && ask.PermissionNoted {
			_ = b.cfg.Relay.Progress(ctx, ask.RequestID, fmt.Sprintf("T3 thread %s input resolved (%s); continuing.", threadDisplay(m.ThreadID, ask.Link), m.Resolution))
			_ = b.cfg.State.Update(func(d *StateData) error {
				if a, ok := d.Asks[ask.RequestID]; ok {
					a.PermissionNoted = false
					a.UpdatedAt = b.cfg.Now().UTC()
					d.Asks[ask.RequestID] = a
				}
				return nil
			})
		}
	}
}

func (b *Bridge) onPRWatchWake(ctx context.Context, snap StateData, m MilestoneData) {
	watches := snap.Watches[m.ThreadID]
	if len(watches) == 0 {
		return
	}
	tr, _ := b.cfg.T3.ReadThread(ctx, m.ThreadID, 3)
	link := threadDisplay(m.ThreadID, tr.Thread.Link)
	for _, w := range watches {
		if !watchWantsEvent(w, "pr.watch_wake") {
			continue
		}
		msg := fmt.Sprintf("PR watch woke T3 thread %s (%s).", link, tr.Thread.Title)
		if m.Detail != "" {
			msg += "\n" + m.Detail
		}
		if _, err := b.cfg.Relay.Send(ctx, w.NotifyAgent, msg, envelope.KindAsk, "", false); err != nil {
			b.cfg.Logger.Printf("t3 bridge: notify %s for pr.watch_wake on %s failed: %v", w.NotifyAgent, m.ThreadID, err)
		}
	}
}

func (b *Bridge) handleUnseenReplies(ctx context.Context, replies []client.Result) {
	now := b.cfg.Now()
	for _, res := range replies {
		if res.Reply == nil {
			continue
		}
		rt, ok, err := b.cfg.Routes.Lookup(res.Request.ID)
		if err != nil || !ok || rt.ThreadID == "" {
			continue
		}
		if rt.InlineDone {
			_ = b.cfg.Relay.AckReplies(ctx, nil, envelope.ReplyAck{ID: res.Request.ID, Generation: res.Reply.Generation})
			continue
		}
		// Short grace so synchronous inline ask waiters finish first without races.
		if b.cfg.LateReplyGrace > 0 && !res.Reply.CreatedAt.IsZero() && now.Sub(res.Reply.CreatedAt) < b.cfg.LateReplyGrace {
			continue
		}
		gen := res.Reply.Generation
		if gen <= 0 {
			gen = 1
		}
		if rt.DeliveredGen >= gen {
			// Previously delivered to T3 before a crash; finish acking now without re-sending.
			_ = b.cfg.Relay.AckReplies(ctx, nil, envelope.ReplyAck{ID: res.Request.ID, Generation: res.Reply.Generation})
			continue
		}

		msg := formatLateReply(res)
		clientReqID := fmt.Sprintf("tincan-reply-%s-g%d", res.Request.ID, gen)
		if err := b.cfg.T3.SendThreadMessage(ctx, rt.ThreadID, msg, clientReqID); err != nil {
			if isThreadGoneError(err) {
				b.cfg.Logger.Printf("t3 bridge: originating thread %s gone for reply %s; acking", rt.ThreadID, res.Request.ID)
				_ = b.cfg.Relay.AckReplies(ctx, nil, envelope.ReplyAck{ID: res.Request.ID, Generation: res.Reply.Generation})
				continue
			}
			b.cfg.Logger.Printf("t3 bridge: deliver late reply %s to thread %s failed: %v", res.Request.ID, rt.ThreadID, err)
			continue
		}
		_ = b.cfg.Routes.MarkDelivered(res.Request.ID, gen)
		if err := b.cfg.Relay.AckReplies(ctx, nil, envelope.ReplyAck{ID: res.Request.ID, Generation: res.Reply.Generation}); err != nil {
			b.cfg.Logger.Printf("t3 bridge: ack reply %s failed (marked delivered so retry will not double-send): %v", res.Request.ID, err)
		}
	}
}

func (b *Bridge) markAskDone(reqID string) {
	_ = b.cfg.State.Update(func(d *StateData) error {
		if a, ok := d.Asks[reqID]; ok {
			a.Done = true
			a.UpdatedAt = b.cfg.Now().UTC()
			d.Asks[reqID] = a
		}
		return nil
	})
}

func matchProject(projects []T3Project, query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return ""
	}
	for _, p := range projects {
		if strings.ToLower(p.ID) == q || strings.ToLower(strings.TrimSpace(p.Title)) == q {
			return p.ID
		}
	}
	for _, p := range projects {
		if p.WorkspaceRoot != "" && strings.ToLower(filepath.Base(p.WorkspaceRoot)) == q {
			return p.ID
		}
	}
	return ""
}

func makeThreadTitle(from, body string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	line = strings.TrimSpace(line)
	if len(line) > 72 {
		line = line[:72] + "..."
	}
	if line == "" {
		return "Agent Tincan ask from " + from
	}
	return fmt.Sprintf("[%s] %s", from, line)
}

func threadDisplay(threadID, link string) string {
	if strings.TrimSpace(link) != "" {
		return strings.TrimSpace(link)
	}
	return threadID
}

func formatSettledSummary(tr ThreadReadResult, outcome, note string) string {
	var sb strings.Builder
	if outcome == "" {
		outcome = "completed"
	}
	link := threadDisplay(tr.Thread.ThreadID, tr.Thread.Link)
	if txt := tr.LatestAssistantText(); txt != "" {
		sb.WriteString(truncateText(txt, maxReplyBodyChars))
		sb.WriteString("\n\n")
	}
	if tr.Thread.Title != "" {
		fmt.Fprintf(&sb, "T3 thread (%s): %s (%s)", outcome, tr.Thread.Title, link)
	} else {
		fmt.Fprintf(&sb, "T3 thread (%s): %s", outcome, link)
	}
	if strings.TrimSpace(note) != "" {
		fmt.Fprintf(&sb, "\nWatch note: %s", strings.TrimSpace(note))
	}
	return sb.String()
}

func formatPendingQuestion(threadID string, pq PendingQuestion) string {
	if len(pq.Questions) == 0 {
		var compact string
		if len(pq.Raw) > 0 {
			var b strings.Builder
			if json.Valid(pq.Raw) {
				b.Write(pq.Raw)
				compact = truncateText(b.String(), 2000)
			}
		}
		if compact != "" {
			return compact
		}
		return fmt.Sprintf("Pending question %s on thread %s.", pq.RequestID, threadID)
	}
	var sb strings.Builder
	for i, q := range pq.Questions {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		if q.Header != "" {
			sb.WriteString(q.Header)
			sb.WriteString(": ")
		}
		sb.WriteString(q.Question)
		for j, opt := range q.Options {
			fmt.Fprintf(&sb, "\n%d. %s", j+1, opt.Label)
			if opt.Description != "" {
				sb.WriteString(" - " + opt.Description)
			}
		}
	}
	return sb.String()
}

func formatLateReply(res client.Result) string {
	from := res.Reply.From
	if from == "" {
		from = res.Request.To
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Agent Tincan reply from %s (request %s, status %s):\n\n", from, res.Request.ID, res.Reply.Status)
	sb.WriteString(truncateText(strings.TrimSpace(res.Reply.Body), maxReplyBodyChars))
	return sb.String()
}

func watchWantsEvent(w ThreadWatch, ev string) bool {
	if len(w.Events) == 0 {
		return true
	}
	return slices.Contains(w.Events, ev)
}

func isThreadGoneError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "deleted") || strings.Contains(msg, "unknown thread")
}

func unwrapGuestText(body string) string {
	_, after, ok := strings.Cut(body, "<<<GUEST ")
	if !ok {
		return ""
	}
	nonce, content, ok := strings.Cut(after, ">>>\n")
	if !ok {
		return ""
	}
	closer := "\n<<<END GUEST " + nonce + ">>>"
	if before, _, ok := strings.Cut(content, closer); ok {
		return strings.TrimSpace(before)
	}
	return strings.TrimSpace(content)
}

func truncateText(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n... (truncated)"
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
