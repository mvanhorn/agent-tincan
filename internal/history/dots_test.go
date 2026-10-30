package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const (
	dotThread = "0d0d0d0d-1111-7222-8333-000000000001"
	dotOwner  = "user-owner"
	dotBot    = "user-dot"
)

// dotItem builds one item of the extension's dots.detail result.
func dotItem(id, from, text string, at time.Time) map[string]any {
	return map[string]any{"id": id, "at": at.UTC().Format(time.RFC3339), "from": from, "text": text, "attachments": 0}
}

func dotsDetailJSON(items ...map[string]any) json.RawMessage {
	if items == nil {
		items = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"thread": dotThread, "room": "room-1", "owner": dotOwner, "paused": false, "items": items})
	return b
}

// A run of dot messages after an owner message is one reply, its texts
// joined by a blank line and dated by its last message.
func TestDotsNodesJoinsARunOfDotMessages(t *testing.T) {
	at := time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)
	nodes, err := dotsNodes(dotsDetailJSON(
		dotItem("m1", dotOwner, "A", at),
		dotItem("m2", dotBot, "B", at.Add(time.Second)),
		dotItem("m3", dotBot, "C", at.Add(2*time.Second)),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || !nodes[0].user || nodes[0].text != "A" || nodes[0].id != "m1" {
		t.Fatalf("nodes %+v", nodes)
	}
	r := nodes[1]
	if !r.reply || r.user || r.text != "B\n\nC" || !r.at.Equal(at.Add(2*time.Second)) {
		t.Fatalf("reply node %+v", r)
	}
	p := progressOf(nodes, replyAnchor{bound: "m1"})
	if !p.found || p.orphaned || p.finished {
		t.Fatalf("progress %+v", p)
	}
}

// The owner sending again with no dot answer between leaves the first
// message orphaned; a dot answer before the owner's next message is the
// first message's reply, and what comes after is not.
func TestDotsNodesStopAtTheOwnersNextMessage(t *testing.T) {
	at := time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)
	nodes, err := dotsNodes(dotsDetailJSON(
		dotItem("m1", dotOwner, "A", at),
		dotItem("m2", dotOwner, "D", at.Add(time.Second)),
		dotItem("m3", dotBot, "E", at.Add(2*time.Second)),
	))
	if err != nil {
		t.Fatal(err)
	}
	if p := progressOf(nodes, replyAnchor{bound: "m1"}); !p.orphaned || p.found {
		t.Fatalf("owner A, owner D: progress %+v", p)
	}
	nodes, err = dotsNodes(dotsDetailJSON(
		dotItem("m1", dotOwner, "A", at),
		dotItem("m2", dotBot, "B", at.Add(time.Second)),
		dotItem("m3", dotOwner, "D", at.Add(2*time.Second)),
		dotItem("m4", dotBot, "E", at.Add(3*time.Second)),
	))
	if err != nil {
		t.Fatal(err)
	}
	p := progressOf(nodes, replyAnchor{bound: "m1"})
	if !p.found || p.orphaned || !strings.HasSuffix(p.sig, "\nB") {
		t.Fatalf("owner A, dot B, owner D, dot E: progress %+v", p)
	}
	th, err := parseDotsDetail(dotThread, dotsDetailJSON(
		dotItem("m1", dotOwner, "A", at),
		dotItem("m2", dotBot, "B", at.Add(time.Second)),
		dotItem("m3", dotOwner, "D", at.Add(2*time.Second)),
		dotItem("m4", dotBot, "E", at.Add(3*time.Second)),
	))
	if err != nil || len(th.turns) != 2 || th.turns[0].promptID != "m1" || th.turns[0].reply.Text != "B" || th.turns[1].reply.Text != "E" {
		t.Fatalf("thread %+v, %v", th, err)
	}
}

// A reply run that ends in the dot's own @tincan ask keeps the ask line
// and notes that the dot is waiting on that teammate and where its final
// answer will be; a plain answer gets no note.
func TestDotsNodesNoteAnAskEndingTheReply(t *testing.T) {
	at := time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)
	nodes, err := dotsNodes(dotsDetailJSON(
		dotItem("m1", dotOwner, "A", at),
		dotItem("m2", dotBot, "@tincan ask muse check x", at.Add(time.Second)),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes %+v", nodes)
	}
	want := "@tincan ask muse check x\n\n(your dot asked muse for help; tincan will pass muse's answer to the dot, and the dot's final answer will be in its DM: https://chatgpt.com/dots/" + dotThread + ")"
	if got := nodes[1].text; got != want {
		t.Fatalf("reply %q, want %q", got, want)
	}
	for _, text := range []string{"Friday is free.", "@tincan ask <agent> hi", "I will @tincan ask muse later"} {
		nodes, err = dotsNodes(dotsDetailJSON(
			dotItem("m1", dotOwner, "A", at),
			dotItem("m2", dotBot, text, at.Add(time.Second)),
		))
		if err != nil {
			t.Fatal(err)
		}
		if got := nodes[1].text; got != text {
			t.Fatalf("reply %q, want %q with no note", got, text)
		}
	}
}

// A detail result without an owner is not read as a feed.
func TestParseDotFeedNeedsAnOwner(t *testing.T) {
	if _, err := ParseDotFeed(json.RawMessage(`{"thread":"x","room":"r","owner":"","paused":false,"items":[]}`)); err == nil {
		t.Fatal("feed without an owner accepted")
	}
	f, err := ParseDotFeed(dotsDetailJSON(dotItem("m1", dotOwner, "A", time.Now()), dotItem("m2", dotBot, "B", time.Now())))
	if err != nil || len(f.Messages) != 2 || !f.Messages[0].Owner || f.Messages[1].Owner || f.Messages[1].From != dotBot {
		t.Fatalf("feed %+v, %v", f, err)
	}
}

// dots is a web-only site under ChatGPT's grant: not a history source,
// and its send always needs the thread.
func TestDotsSiteIsWebOnlyUnderChatGPTsGrant(t *testing.T) {
	if strings.Contains(SourceNames(), "dots") || IsLiveSource(SourceDots) || strings.Contains(LiveSourcesLabel(), "dot") {
		t.Fatalf("dots listed as a history source: %s / %s", SourceNames(), LiveSourcesLabel())
	}
	for _, s := range Sources {
		if s == SourceDots {
			t.Fatal("dots in Sources")
		}
	}
	if !strings.Contains(WebSiteNames(), "dots") || WebAgentName(SourceDots) != "dot-web" {
		t.Fatalf("dots not a web site: %s", WebSiteNames())
	}
	if !(ExtensionStatus{Hello: true, Granted: []string{"chatgpt"}}).granted(SourceDots) {
		t.Fatal("dots not granted under chatgpt's grant")
	}
	if (ExtensionStatus{Hello: true, Granted: []string{"claudeai", "dots"}}).granted(SourceDots) {
		t.Fatal("dots granted without chatgpt")
	}
	if ValidateOp(OpDotsSend, OpArgs{Message: "hi"}) == nil || ValidateOp(OpDotsSend, OpArgs{Message: "hi", NewChat: true}) == nil {
		t.Fatal("dots.send without its thread accepted")
	}
	if err := ValidateOp(OpDotsSend, OpArgs{Message: "hi", ConversationID: dotThread}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []Op{"dots.list", "dots.file"} {
		if op.source() != "" {
			t.Fatalf("%s resolves", op)
		}
	}
	if id, ok := ParseWebThread(SourceDots, strings.ToUpper(dotThread)); !ok || id != dotThread {
		t.Fatalf("thread = %q, %v", id, ok)
	}
	for _, bad := range []string{"", "abc", "not/a/thread", strings.Repeat("a", 65)} {
		if _, ok := ParseWebThread(SourceDots, bad); ok {
			t.Fatalf("thread %q accepted", bad)
		}
	}
}

// dots.primary is the dots site's own: argument-free, and no other site
// has it.
func TestDotsPrimaryIsDotsOnlyAndTakesNoArguments(t *testing.T) {
	if err := ValidateOp(OpDotsPrimary, OpArgs{}); err != nil {
		t.Fatal(err)
	}
	if ValidateOp(OpDotsPrimary, OpArgs{ID: dotThread}) == nil {
		t.Fatal("dots.primary with an argument accepted")
	}
	for _, op := range []Op{"chatgpt.primary", "claudeai.primary", "copilot.primary"} {
		if err := ValidateOp(op, OpArgs{}); err == nil || !strings.Contains(err.Error(), "unknown operation") {
			t.Fatalf("%s: %v", op, err)
		}
	}
}

// DotPrimary asks the extension for the account's primary dot and
// canonicalizes its thread; a malformed answer is an error.
func TestDotPrimaryReadsTheThreadAndName(t *testing.T) {
	answer := func(result string) Channel {
		return channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
			if req.Op != OpDotsPrimary || req.Args != (OpArgs{}) {
				t.Fatalf("request %+v", req)
			}
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(result)})
			return err
		})
	}
	upper := strings.ToUpper(dotThread)
	p, err := (&Client{Channel: answer(`{"thread":"` + upper + `","dot":"a~b","name":"Orbit"}`)}).DotPrimary(context.Background())
	if err != nil || p.Thread != dotThread || p.Name != "Orbit" {
		t.Fatalf("%+v, %v", p, err)
	}
	for _, bad := range []string{`{"name":"Orbit"}`, `{"thread":"not/a/thread"}`, `[]`} {
		if _, err := (&Client{Channel: answer(bad)}).DotPrimary(context.Background()); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if _, err := (&Client{}).DotPrimary(context.Background()); !errors.Is(err, ErrExtensionNotConnected) {
		t.Fatalf("no channel: %v", err)
	}
}

// dotsBrowser plays the extension for dot-web: dots.send adds the owner's
// message to the feed (stored with its text reworded, so only the
// returned message_id can find it) and schedules the dot's messages, each
// shown from the read after the send numbered in its at.
type dotsBrowser struct {
	mu      sync.Mutex
	items   []map[string]any
	sends   []OpArgs
	closes  []string
	details int
	seq     int
	// replies are the dot's answer to the next send.
	replies []dotReply
	pending []pendingDot
	// sendErr fails every send with that code (clicked marks it after
	// the click).
	sendErr string
	clicked bool
	// detailErr fails every detail read with that code (retryAfter
	// seconds with it).
	detailErr  string
	retryAfter int
	// duet makes it the extension for duet-web: the duet ops, the
	// channel's id as the thread, and a working flag in each read.
	duet bool
	// busyReads is how many detail reads after a send say Claude is still
	// working (duet only).
	busyReads int
	sentAt    int
}

type dotReply struct {
	text string
	// at is the post-send detail read the message first shows on.
	at int
	// owner: the owner wrote it in the DM meanwhile.
	owner bool
}

type pendingDot struct {
	item map[string]any
	at   int
}

func (b *dotsBrowser) sent() []OpArgs {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]OpArgs(nil), b.sends...)
}

func (b *dotsBrowser) Exchange(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	fail := func(code string, clicked bool) error {
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: code, Message: code, Clicked: clicked}})
		return err
	}
	if err := ValidateOp(req.Op, req.Args); err != nil {
		return fail("bad_request", false)
	}
	thread := dotThread
	if b.duet {
		thread = duetChan
	}
	switch req.Op {
	case OpDotsSend, OpDuetSend:
		b.sends = append(b.sends, req.Args)
		if b.sendErr != "" {
			return fail(b.sendErr, b.clicked)
		}
		if req.Args.ConversationID != thread {
			return fail("not_found", false)
		}
		b.sentAt = b.details
		now := time.Now()
		b.seq++
		id := fmt.Sprintf("msg-%d", b.seq)
		b.items = append(b.items, dotItem(id, dotOwner, "(as stored) "+req.Args.Message, now))
		for i, r := range b.replies {
			from := dotBot
			if r.owner {
				from = dotOwner
			}
			b.pending = append(b.pending, pendingDot{item: dotItem(fmt.Sprintf("%s-dot-%d", id, i), from, r.text, now.Add(time.Second)), at: b.details + r.at})
		}
		b.replies = nil
		res, _ := json.Marshal(map[string]any{"conversation_id": thread, "url": "https://example.test/" + thread, "submitted_at": now.UnixMilli(), "message_id": id})
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: res})
		return err
	case OpDotsDetail, OpDuetDetail:
		if b.detailErr != "" {
			_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: b.detailErr, Message: b.detailErr, RetryAfter: b.retryAfter}})
			return err
		}
		b.details++
		var keep []pendingDot
		for _, p := range b.pending {
			if b.details >= p.at {
				b.items = append(b.items, p.item)
			} else {
				keep = append(keep, p)
			}
		}
		b.pending = keep
		if b.duet {
			working := b.details-b.sentAt <= b.busyReads
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: duetDetailJSON(working, b.items...)})
			return err
		}
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: dotsDetailJSON(b.items...)})
		return err
	case OpDotsClose, OpDuetClose:
		b.closes = append(b.closes, req.Args.ConversationID)
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(`{"closed":1}`)})
		return err
	}
	return fail("bad_request", false)
}

func dotsRig(t *testing.T) (*webRig, *dotsBrowser) {
	t.Helper()
	rig := newWebRig(t)
	b := &dotsBrowser{}
	rig.agent.Site = SourceDots
	rig.agent.Name = "dot-web"
	rig.agent.Thread = dotThread
	rig.agent.Native = &Client{Channel: b, Cooldown: &SiteCooldown{}}
	rig.agent.ClaudeStableFor = time.Nanosecond
	return rig, b
}

// A request goes into the dot's thread (a leading "new chat" line
// dropped, all else as is), binds to the message id the send returns,
// waits for the dot's reply to hold still, replies with it and closes the
// tab.
func TestDotWebSendsIntoTheThreadAndRepliesWithTheDotsAnswer(t *testing.T) {
	rig, b := dotsRig(t)
	b.replies = []dotReply{{text: "Hello from your dot.", at: 1}}
	res := rig.ask(t, "codex", "new chat\nhello dot")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	want := "Hello from your dot.\n\nyour dot's DM: " + dotThread
	if res.Reply.Body != want {
		t.Fatalf("reply\n%s\nwant\n%s", res.Reply.Body, want)
	}
	if got := b.sent(); len(got) != 1 || got[0] != (OpArgs{Message: "[tincan request " + res.Request.ID + " from codex]\nhello dot", ConversationID: dotThread}) {
		t.Fatalf("sends %+v", got)
	}
	if len(b.closes) != 1 || b.closes[0] != dotThread {
		t.Fatalf("closes %v", b.closes)
	}
	// A conversation: line is text for the dot too.
	b.replies = []dotReply{{text: "ok", at: 1}}
	res = rig.ask(t, "codex", "conversation: abc\nsecond")
	if res.Status != envelope.StatusAnswered || !strings.HasPrefix(res.Reply.Body, "ok\n\n") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if got := b.sent(); len(got) != 2 || got[1] != (OpArgs{Message: "[tincan request " + res.Request.ID + " from codex]\nconversation: abc\nsecond", ConversationID: dotThread}) {
		t.Fatalf("sends %+v", got)
	}
}

// A leading bare "new chat" line (any case, with or without a colon) is
// dropped before the send; a "conversation:" line and a sentence that
// only mentions a new chat are sent as is. A body that is only "new chat"
// is an empty request: nothing is sent.
func TestDotWebDropsALeadingNewChatLine(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"new chat\nWhat is X?", "What is X?"},
		{"New Chat:\n\nhi", "hi"},
		{"  NEW CHAT  \nhi there", "hi there"},
		{"conversation: abc\nhi", "conversation: abc\nhi"},
		{"please start a new chat about X", "please start a new chat about X"},
		{"new chat about X\nhi", "new chat about X\nhi"},
	} {
		rig, b := dotsRig(t)
		b.replies = []dotReply{{text: "ok", at: 1}}
		res := rig.ask(t, "codex", tc.body)
		if res.Status != envelope.StatusAnswered {
			t.Fatalf("%q: %s %q", tc.body, res.Status, res.Reply.Body)
		}
		if got := b.sent(); len(got) != 1 || got[0] != (OpArgs{Message: "[tincan request " + res.Request.ID + " from codex]\n" + tc.want, ConversationID: dotThread}) {
			t.Fatalf("%q: sends %+v, want %q", tc.body, got, tc.want)
		}
	}
	for _, body := range []string{"new chat", "New chat:\n  \n", "   "} {
		rig, b := dotsRig(t)
		res := rig.ask(t, "codex", body)
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "there is no message to send") {
			t.Fatalf("%q: %s %q", body, res.Status, res.Reply.Body)
		}
		if len(b.sent()) != 0 {
			t.Fatalf("%q: sends %+v", body, b.sent())
		}
	}
}

// A dot that answers in two bursts inside the stability window gets both
// into one reply.
func TestDotWebTwoBurstsAreOneReply(t *testing.T) {
	rig, b := dotsRig(t)
	// The site's 3 polls (the span is shortened by dotsRig): the second
	// burst lands on the third read, before the first holds still.
	b.replies = []dotReply{{text: "First part.", at: 1}, {text: "Second part.", at: 3}}
	res := rig.ask(t, "codex", "tell me two things")
	if res.Status != envelope.StatusAnswered || !strings.HasPrefix(res.Reply.Body, "First part.\n\nSecond part.\n\n") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

// A paused dot fails at once with what the owner has to do; the send is
// not retried and nothing is left to close.
func TestDotWebPausedFailsAtOnce(t *testing.T) {
	rig, b := dotsRig(t)
	b.sendErr = "paused"
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "your dot is paused; unpause it in ChatGPT and ask again") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if len(b.sent()) != 1 || len(b.closes) != 0 {
		t.Fatalf("sends %d closes %v", len(b.sent()), b.closes)
	}
}

// A send that timed out after its click was sent: the reply says so and
// to ask later, and the message is not sent again.
func TestDotWebClickedSendTimeoutSaysAskLater(t *testing.T) {
	rig, b := dotsRig(t)
	b.sendErr, b.clicked = "timeout", true
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "was sent to your dot") || !strings.Contains(res.Reply.Body, "ask for the reply later") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if len(b.sent()) != 1 {
		t.Fatalf("sends %d", len(b.sent()))
	}
}

// No dot answer in the request's time: the message was sent, so the
// reply says to ask later, and the tab is closed.
func TestDotWebNoAnswerInTimeSaysAskLater(t *testing.T) {
	rig, b := dotsRig(t)
	rig.agent.RequestTimeout = 300 * time.Millisecond
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "The message was sent to your dot") || !strings.Contains(res.Reply.Body, "ask for the reply later") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if len(b.sent()) != 1 || len(b.closes) != 1 {
		t.Fatalf("sends %d closes %v", len(b.sent()), b.closes)
	}
}

// Not signed in to chatgpt.com: the reply says to sign in there.
func TestDotWebNotLoggedInSaysSignInToChatGPT(t *testing.T) {
	rig, b := dotsRig(t)
	b.sendErr = "not_logged_in"
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "sign in to chatgpt.com in Chrome") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

// An agent with no thread sends nothing.
func TestDotWebWithoutThreadSendsNothing(t *testing.T) {
	rig, b := dotsRig(t)
	rig.agent.Thread = ""
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "--thread") || len(b.sent()) != 0 {
		t.Fatalf("%s %q sends %d", res.Status, res.Reply.Body, len(b.sent()))
	}
}

// The owner chatting with the dot after a request: a dot answer before
// the owner's next message is the request's reply; the owner's message
// first, with no answer between, leaves the request without one.
func TestDotWebOwnerMessagesMeanwhile(t *testing.T) {
	rig, b := dotsRig(t)
	b.replies = []dotReply{{text: "B", at: 1}, {text: "D", at: 2, owner: true}, {text: "E", at: 2}}
	res := rig.ask(t, "codex", "A")
	if res.Status != envelope.StatusAnswered || !strings.HasPrefix(res.Reply.Body, "B\n\nyour dot's DM: ") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	b.replies = []dotReply{{text: "D", at: 1, owner: true}, {text: "E", at: 2}}
	res = rig.ask(t, "codex", "A again")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "another message was sent to your dot") || len(b.sent()) != 2 {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

// Replies about the dot name its DM and ChatGPT's rate limit, never "the
// your dot conversation".
func TestDotWebWordingNamesTheDM(t *testing.T) {
	rig, b := dotsRig(t)
	b.sendErr = "rate_limited"
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "ChatGPT is rate-limiting this account") || strings.Contains(res.Reply.Body, "your dot is rate-limiting") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	rig.agent.Native.Cooldown = &SiteCooldown{}
	b.sendErr, b.clicked = "send_failed", true
	res = rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "check your dot's DM before sending it again") || strings.Contains(res.Reply.Body, "the your dot") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if got := (&UnavailableError{Source: SourceDots, Kind: ErrRateLimited}).Error(); !strings.Contains(got, "ChatGPT is rate-limiting this account") {
		t.Fatal(got)
	}
}
