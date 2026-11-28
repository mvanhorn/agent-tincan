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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const (
	// BridgeSession is the X-Tincan-Session used by tincan t3 serve.
	BridgeSession = "t3-serve"

	defaultPollHold          = 25 * time.Second
	defaultLateReplyGrace    = 5 * time.Second
	defaultRetryInterval     = 3 * time.Second
	defaultUnmatchedReplyTTL = 30 * time.Second
	maxReplyBodyChars        = 16000
	maxSeenEventIDs          = 2048
	maxPendingEvents         = 256
	unmatchedAckThreshold    = 3
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
	Relay             RelayClient
	T3                *MCPClient
	State             *StateStore
	Routes            *RouteStore
	DefaultProject    string
	Logger            *log.Logger
	PollHold          time.Duration
	LateReplyGrace    time.Duration
	RetryInterval     time.Duration
	UnmatchedReplyTTL time.Duration
	HeartbeatMs       int
	Now               func() time.Time
}

type pendingEvent struct {
	eventName string
	ev        EventOccurrence
}

type pendingRecheck struct {
	threadID  string
	prevRunID string
}

// Bridge connects the Agent Tincan relay to T3 Code's /mcp server and SEP-3415 event streams.
type Bridge struct {
	cfg             Config
	handleMu        sync.Mutex
	seenMu          sync.Mutex
	seenIDs         map[string]struct{}
	seenList        []string
	pendingMu       sync.Mutex
	pendingEvents   []pendingEvent
	inFlightPending map[string]int
	pendingRechecks []pendingRecheck
	unmatchedMu     sync.Mutex
	unmatchedSeen   map[string]int
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
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = defaultRetryInterval
	}
	if cfg.UnmatchedReplyTTL <= 0 {
		cfg.UnmatchedReplyTTL = max(cfg.LateReplyGrace*2, defaultUnmatchedReplyTTL)
	}
	if cfg.HeartbeatMs <= 0 {
		cfg.HeartbeatMs = defaultHeartbeatMs
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Bridge{
		cfg:             cfg,
		seenIDs:         map[string]struct{}{},
		inFlightPending: map[string]int{},
		unmatchedSeen:   map[string]int{},
	}, nil
}

// Run starts the relay poller, retry loop, and the 4 SEP-3415 event stream
// workers, blocking until ctx is canceled.
func (b *Bridge) Run(ctx context.Context) error {
	b.cfg.Relay.SetSession(BridgeSession)

	var wg sync.WaitGroup
	wg.Go(func() {
		b.runRetryLoop(ctx)
	})
	for _, evName := range WatchedMilestoneEvents {
		wg.Go(func() {
			b.runEventStream(ctx, evName)
		})
	}

	b.runRelayLoop(ctx)
	wg.Wait()
	return ctx.Err()
}

func (b *Bridge) runRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(b.cfg.RetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.flushPendingEvents(ctx)
			b.flushPendingRechecks(ctx)
		}
	}
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
		deferred := 0
		if len(inbox.Replies) > 0 {
			deferred = b.handleUnseenReplies(ctx, inbox.Replies)
		}
		if len(inbox.Requests) == 0 && deferred > 0 {
			pause := 200 * time.Millisecond
			if b.cfg.LateReplyGrace > 0 {
				pause = min(max(b.cfg.LateReplyGrace/2, 200*time.Millisecond), 2*time.Second)
			}
			sleepCtx(ctx, pause)
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
				if c != "" && !b.hasPendingFor(eventName) {
					b.saveCursor(eventName, c)
				}
			},
			OnEvent: func(ev EventOccurrence) {
				if b.handleMilestoneEvent(ctx, eventName, ev) {
					if ev.Cursor != "" && !b.hasPendingFor(eventName) {
						b.saveCursor(eventName, ev.Cursor)
					}
				} else {
					b.enqueuePendingEvent(eventName, ev)
				}
			},
			OnHeartbeat: func(c string) {
				if c != "" && !b.hasPendingFor(eventName) {
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
		allHandled := true
		lastHandledCursor := ""
		for _, ev := range res.Events {
			if b.handleMilestoneEvent(ctx, eventName, ev) {
				if allHandled && ev.Cursor != "" {
					lastHandledCursor = ev.Cursor
				}
			} else {
				allHandled = false
				b.enqueuePendingEvent(eventName, ev)
			}
		}
		if allHandled && res.Cursor != "" && !b.hasPendingFor(eventName) {
			cursor = res.Cursor
			b.saveCursor(eventName, cursor)
		} else if lastHandledCursor != "" && !b.hasPendingFor(eventName) {
			cursor = lastHandledCursor
			b.saveCursor(eventName, cursor)
		}
		if !allHandled || !res.HasMore {
			return
		}
	}
}

func (b *Bridge) enqueuePendingEvent(eventName string, ev EventOccurrence) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if ev.EventID != "" {
		for _, existing := range b.pendingEvents {
			if existing.eventName == eventName && existing.ev.EventID == ev.EventID {
				return
			}
		}
	}
	if len(b.pendingEvents) >= maxPendingEvents {
		b.pendingEvents = b.pendingEvents[1:]
	}
	b.pendingEvents = append(b.pendingEvents, pendingEvent{eventName: eventName, ev: ev})
}

func (b *Bridge) hasPendingFor(eventName string) bool {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if b.inFlightPending[eventName] > 0 {
		return true
	}
	for _, pe := range b.pendingEvents {
		if pe.eventName == eventName {
			return true
		}
	}
	return false
}

func (b *Bridge) flushPendingEvents(ctx context.Context) {
	b.pendingMu.Lock()
	if len(b.pendingEvents) == 0 {
		b.pendingMu.Unlock()
		return
	}
	batch := make([]pendingEvent, len(b.pendingEvents))
	copy(batch, b.pendingEvents)
	b.pendingEvents = b.pendingEvents[:0]
	for _, pe := range batch {
		b.inFlightPending[pe.eventName]++
	}
	b.pendingMu.Unlock()

	failedByType := map[string]bool{}
	var stillPending []pendingEvent
	lastGoodCursor := map[string]string{}

	for _, pe := range batch {
		if ctx.Err() != nil {
			stillPending = append(stillPending, pe)
			failedByType[pe.eventName] = true
			continue
		}
		if b.handleMilestoneEvent(ctx, pe.eventName, pe.ev) {
			if !failedByType[pe.eventName] && pe.ev.Cursor != "" {
				lastGoodCursor[pe.eventName] = pe.ev.Cursor
			}
		} else {
			stillPending = append(stillPending, pe)
			failedByType[pe.eventName] = true
		}
	}

	b.pendingMu.Lock()
	if len(stillPending) > 0 {
		b.pendingEvents = append(stillPending, b.pendingEvents...)
		if len(b.pendingEvents) > maxPendingEvents {
			b.pendingEvents = b.pendingEvents[len(b.pendingEvents)-maxPendingEvents:]
		}
	}
	for _, pe := range batch {
		b.inFlightPending[pe.eventName]--
		if b.inFlightPending[pe.eventName] <= 0 {
			delete(b.inFlightPending, pe.eventName)
		}
	}
	b.pendingMu.Unlock()

	for evName, cur := range lastGoodCursor {
		if !failedByType[evName] && !b.hasPendingFor(evName) {
			b.saveCursor(evName, cur)
		}
	}
}

func (b *Bridge) enqueuePendingRecheck(threadID, prevRunID string) {
	if threadID == "" {
		return
	}
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	for _, r := range b.pendingRechecks {
		if r.threadID == threadID && r.prevRunID == prevRunID {
			return
		}
	}
	if len(b.pendingRechecks) >= maxPendingEvents {
		b.pendingRechecks = b.pendingRechecks[1:]
	}
	b.pendingRechecks = append(b.pendingRechecks, pendingRecheck{threadID: threadID, prevRunID: prevRunID})
}

func (b *Bridge) flushPendingRechecks(ctx context.Context) {
	b.pendingMu.Lock()
	if len(b.pendingRechecks) == 0 {
		b.pendingMu.Unlock()
		return
	}
	batch := make([]pendingRecheck, len(b.pendingRechecks))
	copy(batch, b.pendingRechecks)
	b.pendingRechecks = b.pendingRechecks[:0]
	b.pendingMu.Unlock()

	for _, r := range batch {
		if ctx.Err() != nil {
			b.enqueuePendingRecheck(r.threadID, r.prevRunID)
			continue
		}
		b.recheckThread(ctx, r.threadID, r.prevRunID)
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

func (b *Bridge) isEventSeen(eventID string) bool {
	if eventID == "" {
		return false
	}
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	_, ok := b.seenIDs[eventID]
	return ok
}

func (b *Bridge) recordEventSeen(eventID string) {
	if eventID == "" {
		return
	}
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	if _, ok := b.seenIDs[eventID]; ok {
		return
	}
	b.seenIDs[eventID] = struct{}{}
	b.seenList = append(b.seenList, eventID)
	if len(b.seenList) > maxSeenEventIDs {
		oldest := b.seenList[0]
		b.seenList = b.seenList[1:]
		delete(b.seenIDs, oldest)
	}
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
		ID:          "ask-" + req.ID,
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
	// Recheck after saving so a thread that settled between the initial read
	// and saving the watch cannot be stranded.
	b.recheckThread(ctx, dir.ThreadID, "")
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
	// Capture the prior run id (best-effort) and register the ask with
	// Sending=true + IgnoreRunID BEFORE calling SendThreadMessage so:
	// (a) an in-flight previous run settling cannot answer this new ask, and
	// (b) once SendThreadMessage succeeds and Sending clears, recheckThread
	// catches any fast new run that settled before Sending cleared.
	prevTR, _ := b.cfg.T3.ReadThread(ctx, dir.ThreadID, 1)
	prevRunID := latestRunID(prevTR)
	now := b.cfg.Now().UTC()
	if err := b.cfg.State.Update(func(d *StateData) error {
		d.Asks[req.ID] = DispatchedAsk{
			RequestID:   req.ID,
			From:        req.From,
			ThreadID:    dir.ThreadID,
			Link:        prevTR.Thread.Link,
			IgnoreRunID: prevRunID,
			Sending:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		return nil
	}); err != nil {
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, fmt.Sprintf("Failed to record ask state for T3 thread %s: %v", dir.ThreadID, err), envelope.StatusFailed)
		return
	}
	clientReqID := "tincan-send-" + req.ID
	if err := b.cfg.T3.SendThreadMessage(ctx, dir.ThreadID, dir.Message, clientReqID); err != nil {
		_ = b.cfg.State.Update(func(d *StateData) error {
			delete(d.Asks, req.ID)
			return nil
		})
		_, _ = b.cfg.Relay.Reply(ctx, req.ID, fmt.Sprintf("Failed to send message to T3 thread %s: %v", dir.ThreadID, err), envelope.StatusFailed)
		return
	}
	_ = b.cfg.State.Update(func(d *StateData) error {
		if a, ok := d.Asks[req.ID]; ok {
			a.Sending = false
			a.UpdatedAt = b.cfg.Now().UTC()
			d.Asks[req.ID] = a
		}
		return nil
	})
	link := threadDisplay(dir.ThreadID, prevTR.Thread.Link)
	_ = b.cfg.Relay.Progress(ctx, req.ID, fmt.Sprintf("Sent message to T3 thread %s.", link))
	b.recheckThread(ctx, dir.ThreadID, prevRunID)
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
	// Recheck after saving so an early settle/needs_input event that fired
	// before LaunchThread returned cannot strand the request.
	b.recheckThread(ctx, threadID, "")
}

// recheckThread inspects threadID right after saving an ask or watch to catch
// fast runs or input prompts that occurred before state persistence completed.
// When prevRunID is non-empty (send to an existing thread), a settled state is
// only acted on if a newer run completed after prevRunID.
func (b *Bridge) recheckThread(ctx context.Context, threadID, prevRunID string) {
	b.handleMu.Lock()
	defer b.handleMu.Unlock()

	tr, err := b.cfg.T3.ReadThread(ctx, threadID, 10)
	if err != nil {
		if !isThreadGoneError(err) {
			b.enqueuePendingRecheck(threadID, prevRunID)
		}
		return
	}
	snap, err := b.cfg.State.Snapshot()
	if err != nil {
		b.enqueuePendingRecheck(threadID, prevRunID)
		return
	}
	if tr.Thread.PendingRequestCount > 0 {
		if !b.onThreadNeedsInputLocked(ctx, snap, MilestoneData{
			Type:     "thread.needs_input",
			ThreadID: threadID,
		}, "") {
			b.enqueuePendingRecheck(threadID, prevRunID)
		}
		return
	}
	if tr.IsRunning() {
		return
	}
	curRunID := latestRunID(tr)
	if prevRunID != "" {
		if curRunID == "" || curRunID == prevRunID {
			return
		}
	} else {
		hasWatch := len(snap.Watches[threadID]) > 0
		if !hasWatch && len(tr.RecentRuns) == 0 && tr.LatestAssistantText() == "" {
			return
		}
	}
	if !b.onThreadSettledLocked(ctx, snap, MilestoneData{
		Type:     "thread.run_settled",
		ThreadID: threadID,
		RunID:    curRunID,
		Outcome:  settledOutcome(tr),
	}, "") {
		b.enqueuePendingRecheck(threadID, prevRunID)
	}
}

func (b *Bridge) handleMilestoneEvent(ctx context.Context, eventName string, ev EventOccurrence) bool {
	if b.isEventSeen(ev.EventID) {
		return true
	}
	m, err := ev.ParseMilestone()
	if err != nil {
		b.cfg.Logger.Printf("t3 bridge: %v", err)
		b.recordEventSeen(ev.EventID)
		return true
	}
	if m.Type == "" {
		m.Type = eventName
	}
	if m.ThreadID == "" {
		b.recordEventSeen(ev.EventID)
		return true
	}

	b.handleMu.Lock()
	defer b.handleMu.Unlock()

	snap, err := b.cfg.State.Snapshot()
	if err != nil {
		b.cfg.Logger.Printf("t3 bridge: read state for event %s failed: %v", ev.EventID, err)
		return false
	}

	handled := true
	switch m.Type {
	case "thread.run_settled":
		handled = b.onThreadSettledLocked(ctx, snap, m, ev.EventID)
	case "thread.needs_input":
		handled = b.onThreadNeedsInputLocked(ctx, snap, m, ev.EventID)
	case "thread.input_resolved":
		b.onThreadInputResolvedLocked(ctx, snap, m)
	case "pr.watch_wake":
		handled = b.onPRWatchWakeLocked(ctx, snap, m, ev.EventID)
	}
	if handled {
		b.recordEventSeen(ev.EventID)
	}
	return handled
}

func (b *Bridge) onThreadSettledLocked(ctx context.Context, snap StateData, m MilestoneData, eventID string) bool {
	var matchingAsks []DispatchedAsk
	for _, a := range snap.Asks {
		if a.Done || a.Sending || a.ThreadID != m.ThreadID {
			continue
		}
		if a.IgnoreRunID != "" && m.RunID != "" && m.RunID == a.IgnoreRunID {
			continue
		}
		matchingAsks = append(matchingAsks, a)
	}
	watches := snap.Watches[m.ThreadID]
	if len(matchingAsks) == 0 && len(watches) == 0 {
		return true
	}

	tr, err := b.cfg.T3.ReadThread(ctx, m.ThreadID, 10)
	if err != nil {
		if !isThreadGoneError(err) {
			b.cfg.Logger.Printf("t3 bridge: read settled thread %s failed (will retry): %v", m.ThreadID, err)
			return false
		}
		// Thread was deleted; report failure instead of retrying forever.
		goneMsg := fmt.Sprintf("T3 thread %s no longer exists: %v", m.ThreadID, err)
		allOK := true
		for _, ask := range matchingAsks {
			if _, rErr := b.cfg.Relay.Reply(ctx, ask.RequestID, goneMsg, envelope.StatusFailed); rErr != nil {
				b.cfg.Logger.Printf("t3 bridge: reply gone thread for ask %s failed: %v", ask.RequestID, rErr)
				allOK = false
				continue
			}
			b.markAskDone(ask.RequestID)
		}
		consumed := map[string]struct{}{}
		for _, w := range watches {
			if !watchWantsEvent(w, "thread.run_settled") {
				continue
			}
			if w.OneShot && w.RequestID != "" {
				if _, rErr := b.cfg.Relay.Reply(ctx, w.RequestID, goneMsg, envelope.StatusFailed); rErr != nil {
					allOK = false
					continue
				}
				consumed[w.Key()] = struct{}{}
				continue
			}
			if sent, sErr := b.cfg.Relay.Send(ctx, w.NotifyAgent, goneMsg, envelope.KindAsk, "", false); sErr != nil {
				allOK = false
			} else {
				_ = b.cfg.Routes.RecordNotice(sent.ID, w.NotifyAgent)
				consumed[w.Key()] = struct{}{}
			}
		}
		b.removeWatchesByKey(m.ThreadID, consumed)
		return allOK
	}

	status := envelope.StatusAnswered
	if m.Outcome != "" && m.Outcome != "completed" {
		status = envelope.StatusFailed
	}

	allOK := true
	curRunID := latestRunID(tr)
	for _, ask := range matchingAsks {
		if ask.IgnoreRunID != "" && curRunID != "" && curRunID == ask.IgnoreRunID {
			continue
		}
		body := formatSettledSummary(tr, m.Outcome, "")
		if body == "" {
			body = fmt.Sprintf("T3 thread %s finished (%s).", threadDisplay(m.ThreadID, ask.Link), m.Outcome)
		}
		if _, err := b.cfg.Relay.Reply(ctx, ask.RequestID, body, status); err != nil {
			b.cfg.Logger.Printf("t3 bridge: reply to ask %s failed (will retry): %v", ask.RequestID, err)
			allOK = false
			continue
		}
		b.markAskDone(ask.RequestID)
	}

	consumed := map[string]struct{}{}
	for _, w := range watches {
		if !watchWantsEvent(w, "thread.run_settled") {
			continue
		}
		body := formatSettledSummary(tr, m.Outcome, w.Note)
		if w.OneShot && w.RequestID != "" {
			if _, err := b.cfg.Relay.Reply(ctx, w.RequestID, body, status); err != nil {
				b.cfg.Logger.Printf("t3 bridge: reply to watch ask %s failed (will retry): %v", w.RequestID, err)
				allOK = false
				continue
			}
			consumed[w.Key()] = struct{}{}
			continue
		}
		notifyKey := ""
		if eventID != "" {
			notifyKey = "watch-settled:" + w.Key() + ":" + eventID
		}
		if notifyKey == "" || !b.isEventSeen(notifyKey) {
			sent, err := b.cfg.Relay.Send(ctx, w.NotifyAgent, body, envelope.KindAsk, "", false)
			if err != nil {
				b.cfg.Logger.Printf("t3 bridge: notify %s for settled thread %s failed (will retry): %v", w.NotifyAgent, m.ThreadID, err)
				allOK = false
				continue
			}
			_ = b.cfg.Routes.RecordNotice(sent.ID, w.NotifyAgent)
			if notifyKey != "" {
				b.recordEventSeen(notifyKey)
			}
		}
		if w.OneShot {
			consumed[w.Key()] = struct{}{}
		}
	}
	b.removeWatchesByKey(m.ThreadID, consumed)
	return allOK
}

func (b *Bridge) removeWatchesByKey(threadID string, consumed map[string]struct{}) {
	if len(consumed) == 0 {
		return
	}
	_ = b.cfg.State.Update(func(d *StateData) error {
		cur := d.Watches[threadID]
		var kept []ThreadWatch
		for _, w := range cur {
			if _, drop := consumed[w.Key()]; drop {
				continue
			}
			kept = append(kept, w)
		}
		if len(kept) == 0 {
			delete(d.Watches, threadID)
		} else {
			d.Watches[threadID] = kept
		}
		return nil
	})
}

func (b *Bridge) onThreadNeedsInputLocked(ctx context.Context, snap StateData, m MilestoneData, eventID string) bool {
	var matchingAsks []DispatchedAsk
	for _, a := range snap.Asks {
		if !a.Done && !a.Sending && a.ThreadID == m.ThreadID {
			matchingAsks = append(matchingAsks, a)
		}
	}
	watches := snap.Watches[m.ThreadID]
	if len(matchingAsks) == 0 && len(watches) == 0 {
		return true
	}

	// R5 & R6: T3 omits permission/approval prompts from t3_pending_request(list).
	pendingIDs, err := b.cfg.T3.ListPendingQuestions(ctx, m.ThreadID)
	if err != nil && !isThreadGoneError(err) {
		b.cfg.Logger.Printf("t3 bridge: list pending requests for %s failed (will retry): %v", m.ThreadID, err)
		return false
	}
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

	allOK := true
	for _, ask := range matchingAsks {
		link := threadDisplay(m.ThreadID, ask.Link)
		if isUserQuestion {
			if ask.PendingRequestID == m.RequestID {
				continue
			}
			body := fmt.Sprintf("T3 thread %s needs your answer:\n\n%s", link, questionBody)
			if _, err := b.cfg.Relay.Reply(ctx, ask.RequestID, body, envelope.StatusNeedsInput); err != nil {
				b.cfg.Logger.Printf("t3 bridge: post needs_input for %s failed (will retry): %v", ask.RequestID, err)
				allOK = false
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
			if ask.PermissionNoted {
				continue
			}
			note := fmt.Sprintf("T3 thread %s is paused waiting for the owner to approve a permission prompt directly in T3 Code (permission prompts are never approved over Agent Tincan).", link)
			if err := b.cfg.Relay.Progress(ctx, ask.RequestID, note); err != nil {
				b.cfg.Logger.Printf("t3 bridge: post permission notice for %s failed (will retry): %v", ask.RequestID, err)
				allOK = false
				continue
			}
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
		notifyKey := ""
		if eventID != "" {
			notifyKey = "watch-input:" + w.Key() + ":" + eventID
		} else if m.RequestID != "" {
			notifyKey = "watch-input:" + w.Key() + ":" + m.RequestID
		}
		if notifyKey != "" && b.isEventSeen(notifyKey) {
			continue
		}
		var notice string
		if isUserQuestion {
			notice = fmt.Sprintf("Watched T3 thread %s needs input (reply via `thread: %s`):\n\n%s", m.ThreadID, m.ThreadID, questionBody)
		} else {
			notice = fmt.Sprintf("Watched T3 thread %s is waiting for owner permission approval directly in T3 Code (cannot be approved over Agent Tincan).", m.ThreadID)
		}
		sent, err := b.cfg.Relay.Send(ctx, w.NotifyAgent, notice, envelope.KindAsk, "", false)
		if err != nil {
			b.cfg.Logger.Printf("t3 bridge: notify %s for needs_input on %s failed (will retry): %v", w.NotifyAgent, m.ThreadID, err)
			allOK = false
			continue
		}
		_ = b.cfg.Routes.RecordNotice(sent.ID, w.NotifyAgent)
		if notifyKey != "" {
			b.recordEventSeen(notifyKey)
		}
	}
	return allOK
}

func (b *Bridge) onThreadInputResolvedLocked(ctx context.Context, snap StateData, m MilestoneData) {
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

func (b *Bridge) onPRWatchWakeLocked(ctx context.Context, snap StateData, m MilestoneData, eventID string) bool {
	watches := snap.Watches[m.ThreadID]
	if len(watches) == 0 {
		return true
	}
	tr, err := b.cfg.T3.ReadThread(ctx, m.ThreadID, 3)
	if err != nil && !isThreadGoneError(err) {
		b.cfg.Logger.Printf("t3 bridge: read thread %s for pr.watch_wake failed (will retry): %v", m.ThreadID, err)
		return false
	}
	link := threadDisplay(m.ThreadID, tr.Thread.Link)
	allOK := true
	for _, w := range watches {
		if !watchWantsEvent(w, "pr.watch_wake") {
			continue
		}
		notifyKey := ""
		if eventID != "" {
			notifyKey = "watch-pr:" + w.Key() + ":" + eventID
		}
		if notifyKey != "" && b.isEventSeen(notifyKey) {
			continue
		}
		msg := fmt.Sprintf("PR watch woke T3 thread %s (%s).", link, tr.Thread.Title)
		if m.Detail != "" {
			msg += "\n" + m.Detail
		}
		sent, err := b.cfg.Relay.Send(ctx, w.NotifyAgent, msg, envelope.KindAsk, "", false)
		if err != nil {
			b.cfg.Logger.Printf("t3 bridge: notify %s for pr.watch_wake on %s failed (will retry): %v", w.NotifyAgent, m.ThreadID, err)
			allOK = false
			continue
		}
		_ = b.cfg.Routes.RecordNotice(sent.ID, w.NotifyAgent)
		if notifyKey != "" {
			b.recordEventSeen(notifyKey)
		}
	}
	return allOK
}

func (b *Bridge) handleUnseenReplies(ctx context.Context, replies []client.Result) int {
	now := b.cfg.Now()
	deferred := 0
	for _, res := range replies {
		if res.Reply == nil {
			continue
		}
		gen := res.Reply.Generation
		if gen <= 0 {
			gen = 1
		}
		unmatchedKey := res.Request.ID + ":" + strconv.FormatInt(gen, 10)

		rt, ok, err := b.cfg.Routes.Lookup(res.Request.ID)
		if err != nil {
			b.cfg.Logger.Printf("t3 bridge: lookup route for reply %s failed: %v", res.Request.ID, err)
			deferred++
			continue
		}
		if !ok || rt.Notice || rt.InlineDone {
			if ok && (rt.Notice || rt.InlineDone) {
				_ = b.cfg.Relay.AckReplies(ctx, nil, envelope.ReplyAck{ID: res.Request.ID, Generation: res.Reply.Generation})
				b.clearUnmatched(unmatchedKey)
				continue
			}
			// No route found yet: allow a bounded retry window in case the
			// outbound ask process is still flushing t3-routes.json, then ack so
			// orphaned replies never jam the relay batch or spin Poll.
			attempts := b.bumpUnmatched(unmatchedKey)
			tooOld := res.Reply.CreatedAt.IsZero() || now.Sub(res.Reply.CreatedAt) >= b.cfg.UnmatchedReplyTTL
			if attempts >= unmatchedAckThreshold && tooOld {
				b.cfg.Logger.Printf("t3 bridge: acking unmatched reply %s (no T3 thread route after %d polls)", res.Request.ID, attempts)
				_ = b.cfg.Relay.AckReplies(ctx, nil, envelope.ReplyAck{ID: res.Request.ID, Generation: res.Reply.Generation})
				b.clearUnmatched(unmatchedKey)
				continue
			}
			deferred++
			continue
		}
		b.clearUnmatched(unmatchedKey)
		if rt.ThreadID == "" {
			_ = b.cfg.Relay.AckReplies(ctx, nil, envelope.ReplyAck{ID: res.Request.ID, Generation: res.Reply.Generation})
			continue
		}
		// Short grace so synchronous inline ask waiters finish first without races.
		if b.cfg.LateReplyGrace > 0 && !res.Reply.CreatedAt.IsZero() && now.Sub(res.Reply.CreatedAt) < b.cfg.LateReplyGrace {
			deferred++
			continue
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
			deferred++
			continue
		}
		_ = b.cfg.Routes.MarkDelivered(res.Request.ID, gen)
		if err := b.cfg.Relay.AckReplies(ctx, nil, envelope.ReplyAck{ID: res.Request.ID, Generation: res.Reply.Generation}); err != nil {
			b.cfg.Logger.Printf("t3 bridge: ack reply %s failed (marked delivered so retry will not double-send): %v", res.Request.ID, err)
		}
	}
	return deferred
}

func (b *Bridge) bumpUnmatched(key string) int {
	b.unmatchedMu.Lock()
	defer b.unmatchedMu.Unlock()
	b.unmatchedSeen[key]++
	return b.unmatchedSeen[key]
}

func (b *Bridge) clearUnmatched(key string) {
	b.unmatchedMu.Lock()
	defer b.unmatchedMu.Unlock()
	delete(b.unmatchedSeen, key)
}

func latestRunID(tr ThreadReadResult) string {
	if tr.Thread.ActiveRunID != "" {
		return tr.Thread.ActiveRunID
	}
	if tr.Thread.LatestRunID != "" {
		return tr.Thread.LatestRunID
	}
	if len(tr.RecentRuns) > 0 {
		return tr.RecentRuns[0].RunID
	}
	return ""
}

func settledOutcome(tr ThreadReadResult) string {
	if len(tr.RecentRuns) > 0 && strings.TrimSpace(tr.RecentRuns[0].Status) != "" {
		return strings.TrimSpace(tr.RecentRuns[0].Status)
	}
	return "completed"
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
