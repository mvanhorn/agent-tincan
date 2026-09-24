package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// fakeBrowser plays the extension for the web agent: it answers send,
// detail, file and close operations the way the real one would. A send
// returns as soon as the message is in; the detail shows the new turn
// after lag polls, then in progress for inProgress polls, then finished.
type fakeBrowser struct {
	mu     sync.Mutex
	sends  []OpArgs
	ops    []Op
	closes []string
	convs  map[string][]*fakeTurn // conversation id -> turns
	// lag is how many detail polls a new turn stays invisible; inProgress
	// how many more it shows as still being written (-1: forever).
	lag, inProgress int
	seq             int
	// reply makes the assistant's answer to a prompt.
	reply func(prompt string) string
	// image, when set, is generated with every reply.
	image []byte
	// sendErr, when set, fails the next sends with this code.
	sendErr string
	// block makes sends wait for the request context to end.
	block bool
}

type fakeTurn struct {
	prompt, reply, imageID string
	sentAt                 time.Time
	lag, pending           int
}

func newFakeBrowser() *fakeBrowser {
	return &fakeBrowser{convs: map[string][]*fakeTurn{}, inProgress: 1, reply: func(p string) string { return "You said: " + p }}
}

func (f *fakeBrowser) closed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closes...)
}

func (f *fakeBrowser) opCount(op Op) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.ops {
		if o == op {
			n++
		}
	}
	return n
}

func (f *fakeBrowser) sent() []OpArgs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]OpArgs(nil), f.sends...)
}

func (f *fakeBrowser) exchanges() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ops)
}

func (f *fakeBrowser) Exchange(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	f.mu.Lock()
	f.ops = append(f.ops, req.Op)
	f.mu.Unlock()
	if err := ValidateOp(req.Op, req.Args); err != nil {
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request", Message: err.Error()}})
		return err
	}
	switch req.Op {
	case OpChatGPTSend, OpClaudeAISend:
		f.mu.Lock()
		f.sends = append(f.sends, req.Args)
		block, code := f.block, f.sendErr
		f.mu.Unlock()
		if block {
			<-ctx.Done()
			return ctx.Err()
		}
		if code != "" {
			_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: code, Message: "fake " + code}})
			return err
		}
		f.mu.Lock()
		id := req.Args.ConversationID
		if id == "" {
			f.seq++
			id = fmt.Sprintf("conv-%d", f.seq)
		} else if _, ok := f.convs[id]; !ok {
			f.mu.Unlock()
			_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "not_found", Message: "conversation not found"}})
			return err
		}
		now := time.Now()
		t := &fakeTurn{prompt: req.Args.Message, reply: f.reply(req.Args.Message), sentAt: now, lag: f.lag, pending: f.inProgress}
		if f.image != nil {
			t.imageID = fmt.Sprintf("file-Gen%d", len(f.convs[id])+f.seq)
		}
		f.convs[id] = append(f.convs[id], t)
		f.mu.Unlock()
		res, _ := json.Marshal(SendResult{ConversationID: id, URL: "https://chatgpt.com/c/" + id, SubmittedAt: now.UnixMilli()})
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: res})
		return err
	case OpChatGPTDetail:
		f.mu.Lock()
		turns, ok := f.convs[req.Args.ID]
		var view []fakeTurn
		for _, t := range turns {
			switch {
			case t.lag > 0:
				t.lag--
				continue
			case t.pending > 0:
				view = append(view, *t)
				t.pending--
				continue
			}
			view = append(view, *t)
		}
		f.mu.Unlock()
		if !ok {
			_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "not_found"}})
			return err
		}
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: chatgptDetailJSON(req.Args.ID, view)})
		return err
	case OpChatGPTClose, OpClaudeAIClose:
		f.mu.Lock()
		f.closes = append(f.closes, req.Args.ConversationID)
		f.mu.Unlock()
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(`{"closed":1}`)})
		return err
	case OpChatGPTFile:
		for _, fr := range chunkFrames(f.image, "image/png", 1<<10) {
			fr.ID = req.ID
			if done, err := recv(fr); done || err != nil {
				return err
			}
		}
		return nil
	}
	_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request", Message: "unexpected op"}})
	return err
}

// chatgptDetailJSON renders turns in the shape of chatgpt.com's
// conversation detail: a mapping on one branch, with generated images as
// image pointers in a tool message. A pending turn's reply is half written
// and in_progress; a finished one is finished_successfully with end_turn.
func chatgptDetailJSON(id string, turns []fakeTurn) json.RawMessage {
	mapping := map[string]any{"root": map[string]any{"message": nil, "parent": nil}}
	parent := "root"
	add := func(nid string, msg map[string]any) {
		mapping[nid] = map[string]any{"message": msg, "parent": parent}
		parent = nid
	}
	for i, t := range turns {
		at := float64(t.sentAt.UnixMilli()) / 1000
		uid := fmt.Sprintf("u%d", i)
		add(uid, map[string]any{"id": uid, "author": map[string]any{"role": "user"}, "create_time": at + 0.5, "content": map[string]any{"content_type": "text", "parts": []any{t.prompt}}, "recipient": "all", "status": "finished_successfully"})
		if t.pending != 0 {
			// Still being written: part of the text, no image yet.
			add(fmt.Sprintf("a%d", i), map[string]any{"author": map[string]any{"role": "assistant"}, "create_time": at + 1, "content": map[string]any{"content_type": "text", "parts": []any{t.reply[:len(t.reply)/2]}}, "recipient": "all", "status": "in_progress", "end_turn": nil})
			continue
		}
		if t.imageID != "" {
			add(fmt.Sprintf("t%d", i), map[string]any{"author": map[string]any{"role": "tool"}, "content": map[string]any{"content_type": "multimodal_text", "parts": []any{map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + t.imageID}}}, "recipient": "all", "status": "finished_successfully"})
		}
		add(fmt.Sprintf("a%d", i), map[string]any{"author": map[string]any{"role": "assistant"}, "create_time": at + 2, "content": map[string]any{"content_type": "text", "parts": []any{t.reply}}, "recipient": "all", "status": "finished_successfully", "end_turn": true, "metadata": map[string]any{"finish_details": map[string]any{"type": "stop"}}})
	}
	b, _ := json.Marshal(map[string]any{"title": "Web agent chat", "conversation_id": id, "current_node": parent, "mapping": mapping})
	return b
}

type webRig struct {
	mesh    *testrelay.Mesh
	agent   *WebAgent
	browser *fakeBrowser
	state   string
}

func newWebRig(t *testing.T) *webRig {
	t.Helper()
	return newWebRigWith(t, relay.Config{}, true)
}

func newWebRigWith(t *testing.T, cfg relay.Config, attachments bool) *webRig {
	t.Helper()
	m := testrelay.New(t, cfg)
	if attachments {
		m.Server.SetAttachmentDir(t.TempDir())
	}
	me := m.JoinOnMachineOf(t, "instinct", "chatgpt-web")
	m.JoinOnMachineOf(t, "instinct", "codex")
	fb := newFakeBrowser()
	state := filepath.Join(t.TempDir(), "cfg", "chatgpt-web-state.json")
	return &webRig{
		mesh:    m,
		browser: fb,
		state:   state,
		agent: &WebAgent{
			Relay:       me,
			Site:        SourceChatGPT,
			Name:        "chatgpt-web",
			Native:      &Client{Channel: fb},
			Allowlist:   StaticAllowlist(DefaultAllowlist...),
			StatePath:   state,
			JournalPath: filepath.Join(filepath.Dir(state), "chatgpt-web-journal.json"),
			TempDir:     t.TempDir(),
			Log:         testLog{t},
			// Fast polls for tests; the default is 2s.
			PollInterval: 5 * time.Millisecond,
		},
	}
}

func (r *webRig) ask(t *testing.T, from, body string) client.Result {
	t.Helper()
	c := r.mesh.Client(t, from)
	req, err := c.Send(t.Context(), "chatgpt-web", body, envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	return r.serve(t, c, req.ID)
}

func (r *webRig) serve(t *testing.T, c *client.Relay, id string) client.Result {
	t.Helper()
	n, err := r.agent.PollOnce(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("PollOnce = %d, %v", n, err)
	}
	res, err := c.Get(t.Context(), id, 0)
	if err != nil || res.Reply == nil {
		t.Fatalf("no reply: %+v %v", res, err)
	}
	return res
}

func TestWebAllowedAskerGetsReplyWithImage(t *testing.T) {
	rig := newWebRig(t)
	img := fakePNG(2000)
	rig.browser.image = img
	res := rig.ask(t, "grokbot", "Draw a fox logo")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	body := res.Reply.Body
	for _, want := range []string{"You said: Draw a fox logo", "ChatGPT conversation: conv-1", "1 image attached."} {
		if !strings.Contains(body, want) {
			t.Errorf("reply missing %q:\n%s", want, body)
		}
	}
	if n := rig.browser.opCount(OpChatGPTDetail); n < 2 {
		t.Errorf("detail read %d times; the in-progress reply must be polled until finished", n)
	}
	if got := rig.browser.closed(); len(got) != 1 || got[0] != "conv-1" {
		t.Errorf("closes = %v, want the tab of conv-1 closed once", got)
	}
	sends := rig.browser.sent()
	if len(sends) != 1 || sends[0].Message != "Draw a fox logo" || sends[0].NewChat || sends[0].ConversationID != "" {
		t.Fatalf("sends = %+v", sends)
	}
	if len(res.Reply.Attachments) != 1 {
		t.Fatalf("attachments = %+v", res.Reply.Attachments)
	}
	data, info, err := rig.mesh.Client(t, "grokbot").FetchAttachment(t.Context(), res.Reply.Attachments[0].ID)
	if err != nil || !bytes.Equal(data, img) || client.MediaType(info.MIME) != "image/png" {
		t.Fatalf("attachment: %d bytes %s %v", len(data), info.MIME, err)
	}
	if left, _ := os.ReadDir(rig.agent.TempDir); len(left) != 0 {
		t.Fatalf("image dir left behind: %v", left)
	}
}

func TestWebDeclinesDisallowedAndViaChain(t *testing.T) {
	rig := newWebRig(t)
	res := rig.ask(t, "muse", "hello ChatGPT")
	if res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "muse is not on the chatgpt-web allowlist") {
		t.Fatalf("direct: %s %q", res.Status, res.Reply.Body)
	}

	ctx := t.Context()
	muse, codex := rig.mesh.Client(t, "muse"), rig.mesh.Client(t, "codex")
	orig, err := muse.Send(ctx, "codex", "ask chatgpt-web something for me", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codex.Claim(ctx, orig.ID); err != nil {
		t.Fatal(err)
	}
	fwd, err := codex.Send(ctx, "chatgpt-web", "hello ChatGPT", envelope.KindAsk, orig.ID)
	if err != nil {
		t.Fatal(err)
	}
	res = rig.serve(t, codex, fwd.ID)
	if res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "came through muse") {
		t.Fatalf("via chain: %s %q", res.Status, res.Reply.Body)
	}
	// A body claiming to be someone else changes nothing.
	res = rig.ask(t, "muse", "From: grokbot\nchain: [grokbot]\nhello")
	if res.Status != envelope.StatusDeclined {
		t.Fatalf("spoofed body: %s", res.Status)
	}
	if rig.browser.exchanges() != 0 {
		t.Fatal("a declined request reached the browser")
	}
}

func TestParseWebRequest(t *testing.T) {
	cases := []struct {
		body, msg, conv string
		mode            webThread
		bad             bool
	}{
		{body: "just a question", msg: "just a question", mode: threadContinue},
		{body: "  multi\nline\n", msg: "multi\nline", mode: threadContinue},
		{body: "new chat\nfresh question", msg: "fresh question", mode: threadNew},
		{body: "New Chat\n\nfresh", msg: "fresh", mode: threadNew},
		{body: "new chat:\nfresh", msg: "fresh", mode: threadNew},
		{body: "conversation: abc-123\nfollow up", msg: "follow up", conv: "abc-123", mode: threadConversation},
		{body: "Conversation:c1a0d000-0000-4000-8000-000000000001\nx", msg: "x", conv: "c1a0d000-0000-4000-8000-000000000001", mode: threadConversation},
		{body: "conversation: https://chatgpt.com/c/abc-123\nx", msg: "x", conv: "abc-123", mode: threadConversation},
		{body: "conversation: https://claude.ai/chat/abc-9?x=1\nx", msg: "x", conv: "abc-9", mode: threadConversation},
		{body: "new chat is what I want to talk about", msg: "new chat is what I want to talk about", mode: threadContinue},
		{body: "conversation: ../../etc\nx", bad: true},
		{body: "conversation: https://evil.example/c/abc\nx", bad: true},
		{body: "conversation: abc", bad: true},
		{body: "new chat", bad: true},
		{body: "   \n ", bad: true},
	}
	for _, c := range cases {
		got, err := parseWebRequest(c.body)
		if c.bad {
			if err == nil {
				t.Errorf("%q: want an error, got %+v", c.body, got)
			}
			continue
		}
		if err != nil || got.message != c.msg || got.convID != c.conv || got.mode != c.mode {
			t.Errorf("%q: got %+v %v", c.body, got, err)
		}
	}
}

func TestWebRemembersConversationPerAsker(t *testing.T) {
	rig := newWebRig(t)
	rig.ask(t, "grokbot", "first")                                      // new: conv-1
	rig.ask(t, "grokbot", "second")                                     // continues conv-1
	rig.ask(t, "codex", "codex's own")                                  // new: conv-2
	res := rig.ask(t, "grokbot", "new chat\nthird")                     // new: conv-3
	rig.ask(t, "grokbot", "fourth")                                     // continues conv-3
	rig.ask(t, "codex", "conversation: conv-1\npeek at grokbot's chat") // explicit
	rig.ask(t, "codex", "back to mine?")                                // conv-1 now, last used

	want := []OpArgs{
		{Message: "first"},
		{Message: "second", ConversationID: "conv-1"},
		{Message: "codex's own"},
		{Message: "third", NewChat: true},
		{Message: "fourth", ConversationID: "conv-3"},
		{Message: "peek at grokbot's chat", ConversationID: "conv-1"},
		{Message: "back to mine?", ConversationID: "conv-1"},
	}
	got := rig.browser.sent()
	if len(got) != len(want) {
		t.Fatalf("sends = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("send %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !strings.Contains(res.Reply.Body, "ChatGPT conversation: conv-3") {
		t.Fatalf("new chat reply: %q", res.Reply.Body)
	}
	st, err := os.Stat(rig.state)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", st, err)
	}
	b, _ := os.ReadFile(rig.state)
	if !strings.Contains(string(b), `"grokbot"`) || !strings.Contains(string(b), `"conv-3"`) || strings.Contains(string(b), "fourth") {
		t.Fatalf("state holds ids only, not messages: %s", b)
	}
}

func TestWebRememberedConversationGoneStartsNewChat(t *testing.T) {
	rig := newWebRig(t)
	if err := os.MkdirAll(filepath.Dir(rig.state), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rig.state, []byte(`{"conversations":{"grokbot":{"id":"deleted-1"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := rig.ask(t, "grokbot", "still there?")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "was not found, so this went to a new chat") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	got := rig.browser.sent()
	if len(got) != 2 || got[0].ConversationID != "deleted-1" || !got[1].NewChat {
		t.Fatalf("sends = %+v", got)
	}
	// An explicit id that is gone is an error, not a silent new chat.
	res = rig.ask(t, "grokbot", "conversation: gone-2\nhello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "No ChatGPT conversation with id gone-2") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

func TestWebTimeoutRepliesOnFreshContext(t *testing.T) {
	rig := newWebRig(t)
	rig.agent.RequestTimeout = 200 * time.Millisecond
	rig.browser.block = true
	res := rig.ask(t, "grokbot", "a slow question")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "did not finish answering in time") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if got := rig.browser.closed(); len(got) != 0 {
		t.Fatalf("a send that never returned an id closed %v", got)
	}
}

// The send went through but the reply never finishes: the request fails
// on a fresh context, names the conversation, and still closes the tab.
func TestWebReplyNeverFinishesTimesOutAndClosesTab(t *testing.T) {
	rig := newWebRig(t)
	rig.agent.RequestTimeout = 300 * time.Millisecond
	rig.browser.inProgress = -1
	res := rig.ask(t, "grokbot", "a question that never ends")
	body := res.Reply.Body
	if res.Status != envelope.StatusFailed || !strings.Contains(body, "did not finish answering in time") || !strings.Contains(body, "conv-1") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if n := rig.browser.opCount(OpChatGPTDetail); n < 3 {
		t.Fatalf("detail polled %d times", n)
	}
	if got := rig.browser.closed(); len(got) != 1 || got[0] != "conv-1" {
		t.Fatalf("closes = %v", got)
	}
	// The conversation is remembered, so a follow-up continues it.
	b, _ := os.ReadFile(rig.state)
	if !strings.Contains(string(b), `"conv-1"`) {
		t.Fatalf("state %s", b)
	}
}

// Continuing a conversation whose last turn is the same text: the reply
// is the new one, not the finished old one still shown before the new
// turn lands in the detail.
func TestWebContinuedConversationWaitsForTheNewTurn(t *testing.T) {
	rig := newWebRig(t)
	n := 0
	rig.browser.reply = func(string) string { n++; return fmt.Sprintf("answer number %d", n) }
	rig.ask(t, "grokbot", "same question")
	rig.browser.lag = 3
	res := rig.ask(t, "grokbot", "same question")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "answer number 2") || strings.Contains(res.Reply.Body, "answer number 1") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if got := rig.browser.closed(); len(got) != 2 {
		t.Fatalf("closes = %v", got)
	}
}

func TestWebSourceUnavailableAndPageErrors(t *testing.T) {
	for code, want := range map[string]string{
		"not_logged_in":      "source unavailable: chatgpt: not logged in to chatgpt.com in Chrome",
		"composer_not_found": "source unavailable: chatgpt: no message box on the chatgpt.com page",
		"send_failed":        "source unavailable: chatgpt: the message could not be sent on chatgpt.com",
	} {
		rig := newWebRig(t)
		rig.browser.sendErr = code
		res := rig.ask(t, "grokbot", "hello")
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, want) {
			t.Errorf("%s: %s %q", code, res.Status, res.Reply.Body)
		}
		if got := rig.browser.closed(); len(got) != 0 {
			t.Errorf("%s: a failed send has no tab to close, got %v", code, got)
		}
	}
	rig := newWebRig(t)
	rig.agent.Native = &Client{Channel: nil}
	res := rig.ask(t, "grokbot", "hello")
	if !strings.Contains(res.Reply.Body, "the Tincan Chrome extension is not connected") {
		t.Fatalf("%q", res.Reply.Body)
	}
}

func TestWebRefusesOversizeAndEmptyMessages(t *testing.T) {
	rig := newWebRig(t)
	res := rig.ask(t, "grokbot", strings.Repeat("x", MaxSendMessage+1))
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "over the 32768 byte limit") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	res = rig.ask(t, "grokbot", "new chat\n   ")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "conversation: <id>") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if rig.browser.exchanges() != 0 {
		t.Fatal("a refused message reached the browser")
	}
}

func TestWebLongReplyIsTruncatedWithNote(t *testing.T) {
	rig := newWebRig(t)
	rig.browser.reply = func(string) string { return strings.Repeat("word ", 30000) + "TAIL" }
	res := rig.ask(t, "grokbot", "write a lot")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || strings.Contains(body, "TAIL") || !strings.Contains(body, "(reply truncated: showing") {
		t.Fatalf("%s len %d tail %q", res.Status, len(body), body[max(0, len(body)-200):])
	}
	if !strings.Contains(body, "ChatGPT conversation: conv-1") || len(body) > envelope.DefaultMaxBody {
		t.Fatalf("footer missing or body too big (%d)", len(body))
	}
}

// When the detail read fails for good after a successful send, the
// request fails saying the message went through, and the tab is closed.
func TestWebDetailFailureAfterSendClosesTab(t *testing.T) {
	rig := newWebRig(t)
	rig.agent.Native = &Client{Channel: channelFunc(func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		if req.Op == OpChatGPTDetail {
			_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "endpoint_changed", Message: "moved"}})
			return err
		}
		return rig.browser.Exchange(ctx, req, recv)
	})}
	res := rig.ask(t, "grokbot", "hi")
	body := res.Reply.Body
	if res.Status != envelope.StatusFailed || !strings.Contains(body, "chatgpt.com changed its API") || !strings.Contains(body, "the reply could not be read") || !strings.Contains(body, "conv-1") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if got := rig.browser.closed(); len(got) != 1 || got[0] != "conv-1" {
		t.Fatalf("closes = %v", got)
	}
}

// claudeDetail renders a claude.ai conversation: one earlier finished turn,
// then the new human message and, when reply is not nil, an assistant
// message with that text (and stop_reason when given).
func claudeDetail(id string, sentAt time.Time, message string, reply *string, stopReason string) json.RawMessage {
	msgs := []map[string]any{
		{"uuid": "h0", "sender": "human", "index": 0, "created_at": sentAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano), "content": []any{map[string]any{"type": "text", "text": message}}, "parent_message_uuid": "root"},
		{"uuid": "a0", "sender": "assistant", "index": 1, "created_at": sentAt.Add(-time.Hour + 5*time.Second).UTC().Format(time.RFC3339Nano), "content": []any{map[string]any{"type": "text", "text": "old finished answer"}}, "parent_message_uuid": "h0", "stop_reason": "end_turn"},
		{"uuid": "h1", "sender": "human", "index": 2, "created_at": sentAt.Add(300 * time.Millisecond).UTC().Format(time.RFC3339Nano), "content": []any{map[string]any{"type": "text", "text": message}}, "parent_message_uuid": "a0"},
	}
	leaf := "h1"
	if reply != nil {
		a := map[string]any{"uuid": "a1", "sender": "assistant", "index": 3, "created_at": sentAt.Add(time.Second).UTC().Format(time.RFC3339Nano), "content": []any{map[string]any{"type": "text", "text": *reply}}, "parent_message_uuid": "h1"}
		if stopReason != "" {
			a["stop_reason"] = stopReason
		}
		msgs = append(msgs, a)
		leaf = "a1"
	}
	b, _ := json.Marshal(map[string]any{"uuid": id, "name": "Chat", "current_leaf_message_uuid": leaf, "chat_messages": msgs})
	return b
}

// claudeRig serves claude.ai sends and details: each detail poll shows
// the next entry of frames (the last one repeats). nil is "no assistant
// message yet".
func claudeRig(t *testing.T, frames []*string, stopReason string) (*webRig, func() []Op, func() []string) {
	t.Helper()
	const id = "c1a0d000-0000-4000-8000-000000000002"
	var mu sync.Mutex
	var ops []Op
	var closes []string
	var sentAt time.Time
	polls := 0
	rig := newWebRig(t)
	rig.agent.Site = SourceClaudeAI
	rig.agent.ClaudeStableFor = time.Nanosecond
	rig.agent.Native = &Client{Channel: channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		mu.Lock()
		ops = append(ops, req.Op)
		mu.Unlock()
		switch req.Op {
		case OpClaudeAISend:
			mu.Lock()
			sentAt = time.Now()
			mu.Unlock()
			res, _ := json.Marshal(SendResult{ConversationID: id, URL: "https://claude.ai/chat/" + id, SubmittedAt: sentAt.UnixMilli()})
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: res})
			return err
		case OpClaudeAIDetail:
			mu.Lock()
			f := frames[min(polls, len(frames)-1)]
			polls++
			at := sentAt
			mu.Unlock()
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: claudeDetail(id, at, "hello claude", f, stopReason)})
			return err
		case OpClaudeAIClose:
			mu.Lock()
			closes = append(closes, req.Args.ConversationID)
			mu.Unlock()
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(`{"closed":1}`)})
			return err
		}
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request"}})
		return err
	})}
	get := func() []Op { mu.Lock(); defer mu.Unlock(); return append([]Op(nil), ops...) }
	getCloses := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), closes...) }
	return rig, get, getCloses
}

// claude.ai without stop_reason: the reply is done once its text is the
// same on four polls in a row (the default ClaudeStablePolls).
func TestWebClaudeWaitsForStableReply(t *testing.T) {
	rig, ops, closes := claudeRig(t, []*string{nil, new("Hel"), new("Hello the"), new("Hello there, friend."), new("Hello there, friend.")}, "")
	res := rig.ask(t, "grokbot", "new chat\nhello claude")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "Hello there, friend.") || strings.Contains(body, "old finished answer") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if !strings.Contains(body, "claude.ai conversation: c1a0d000-0000-4000-8000-000000000002") {
		t.Fatalf("%q", body)
	}
	got := ops()
	details := 0
	for _, o := range got {
		if o == OpClaudeAIDetail {
			details++
		}
	}
	if got[0] != OpClaudeAISend || details != 7 || got[len(got)-1] != OpClaudeAIClose {
		t.Fatalf("ops = %v", got)
	}
	if c := closes(); len(c) != 1 || c[0] != "c1a0d000-0000-4000-8000-000000000002" {
		t.Fatalf("closes = %v", c)
	}
}

// claude.ai with stop_reason on the assistant message: done on first sight.
func TestWebClaudeStopReasonFinishesAtOnce(t *testing.T) {
	rig, ops, closes := claudeRig(t, []*string{nil, new("All done.")}, "end_turn")
	res := rig.ask(t, "grokbot", "new chat\nhello claude")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "All done.") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	details := 0
	for _, o := range ops() {
		if o == OpClaudeAIDetail {
			details++
		}
	}
	if details != 2 || len(closes()) != 1 {
		t.Fatalf("details %d closes %v", details, closes())
	}
}

// claude.ai that keeps changing its text never passes the stability check
// and times out, still closing the tab.
func TestWebClaudeTimeoutClosesTab(t *testing.T) {
	var frames []*string
	for i := range 2000 {
		frames = append(frames, new(strings.Repeat("x", i+1)))
	}
	rig, _, closes := claudeRig(t, frames, "")
	rig.agent.RequestTimeout = 200 * time.Millisecond
	res := rig.ask(t, "grokbot", "new chat\nhello claude")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "claude.ai did not finish answering in time") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if len(closes()) != 1 {
		t.Fatalf("closes = %v", closes())
	}
}

// cgm is one message of a hand-built chatgpt.com conversation.
type cgm struct {
	id, role, text string
	at             time.Time
	// status is in_progress or finished_successfully; done adds end_turn
	// and finish_details.
	status string
	done   bool
}

// cgChain renders msgs as one chatgpt.com branch, root first.
func cgChain(convID string, msgs ...cgm) json.RawMessage {
	mapping := map[string]any{"root": map[string]any{"message": nil, "parent": nil}}
	parent := "root"
	for _, m := range msgs {
		msg := map[string]any{"id": m.id, "author": map[string]any{"role": m.role}, "create_time": float64(m.at.UnixMilli()) / 1000, "content": map[string]any{"content_type": "text", "parts": []any{m.text}}, "recipient": "all", "status": m.status}
		if m.done {
			msg["end_turn"] = true
			msg["metadata"] = map[string]any{"finish_details": map[string]any{"type": "stop"}}
		}
		mapping[m.id] = map[string]any{"message": msg, "parent": parent}
		parent = m.id
	}
	b, _ := json.Marshal(map[string]any{"title": "Chat", "conversation_id": convID, "current_node": parent, "mapping": mapping})
	return b
}

// scriptRig serves sends and details for site from a script: detail(n,
// sentAt) is the n-th detail read (reads before the send see a zero
// sentAt). It records every op and closed conversation.
type scriptRig struct {
	*webRig
	mu     sync.Mutex
	ops    []Op
	closes []string
}

const scriptConv = "c1a0d000-0000-4000-8000-000000000003"

func newScriptRig(t *testing.T, site Source, detail func(n int, sentAt time.Time) (json.RawMessage, *NativeError)) *scriptRig {
	t.Helper()
	s := &scriptRig{webRig: newWebRig(t)}
	s.agent.Site = site
	s.agent.ClaudeStableFor = time.Nanosecond
	var sentAt time.Time
	polls := 0
	s.agent.Native = &Client{Channel: channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		s.mu.Lock()
		s.ops = append(s.ops, req.Op)
		s.mu.Unlock()
		switch req.Op {
		case OpChatGPTSend, OpClaudeAISend:
			s.mu.Lock()
			sentAt = time.Now()
			s.mu.Unlock()
			res, _ := json.Marshal(SendResult{ConversationID: scriptConv, SubmittedAt: sentAt.UnixMilli()})
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: res})
			return err
		case OpChatGPTDetail, OpClaudeAIDetail:
			s.mu.Lock()
			n, at := polls, sentAt
			polls++
			s.mu.Unlock()
			raw, nerr := detail(n, at)
			if nerr != nil {
				_, err := recv(NativeResponse{ID: req.ID, Error: nerr})
				return err
			}
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: raw})
			return err
		case OpChatGPTClose, OpClaudeAIClose:
			s.mu.Lock()
			s.closes = append(s.closes, req.Args.ConversationID)
			s.mu.Unlock()
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(`{"closed":1}`)})
			return err
		}
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request"}})
		return err
	})}
	return s
}

func (s *scriptRig) closed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.closes...)
}

// The owner (or another client) posts in the same ChatGPT conversation
// while the agent waits: the reply is the answer to this request's own
// message, never the later turn's.
func TestWebChatGPTIgnoresLaterUserTurn(t *testing.T) {
	rig := newScriptRig(t, SourceChatGPT, func(_ int, at time.Time) (json.RawMessage, *NativeError) {
		return cgChain(scriptConv,
			cgm{id: "u1", role: "user", text: "what is two plus two?", at: at, status: "finished_successfully"},
			cgm{id: "a1", role: "assistant", text: "Four.", at: at.Add(time.Second), status: "finished_successfully", done: true},
			cgm{id: "u2", role: "user", text: "the owner's own question", at: at.Add(2 * time.Second), status: "finished_successfully"},
			cgm{id: "a2", role: "assistant", text: "The owner's answer.", at: at.Add(3 * time.Second), status: "finished_successfully", done: true},
		), nil
	})
	res := rig.ask(t, "grokbot", "new chat\nwhat is two plus two?")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "Four.") || strings.Contains(body, "owner's answer") {
		t.Fatalf("%s %q", res.Status, body)
	}
}

// A later user turn arrives while this request's reply is still being
// written: the agent keeps waiting for this turn's own reply.
func TestWebChatGPTKeepsWaitingForOwnReplyPastLaterTurn(t *testing.T) {
	rig := newScriptRig(t, SourceChatGPT, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		mine := cgm{id: "a1", role: "assistant", text: "Fo", at: at.Add(time.Second), status: "in_progress"}
		if n >= 3 {
			mine = cgm{id: "a1", role: "assistant", text: "Four, finally.", at: at.Add(time.Second), status: "finished_successfully", done: true}
		}
		return cgChain(scriptConv,
			cgm{id: "u1", role: "user", text: "what is two plus two?", at: at, status: "finished_successfully"},
			mine,
			cgm{id: "u2", role: "user", text: "the owner's own question", at: at.Add(2 * time.Second), status: "finished_successfully"},
			cgm{id: "a2", role: "assistant", text: "The owner's answer.", at: at.Add(3 * time.Second), status: "finished_successfully", done: true},
		), nil
	})
	res := rig.ask(t, "grokbot", "new chat\nwhat is two plus two?")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "Four, finally.") || strings.Contains(body, "owner's answer") {
		t.Fatalf("%s %q", res.Status, body)
	}
}

// A later user turn follows this request's message with no reply between
// them: there will never be a reply to this one, so the request fails
// saying so instead of returning the later turn's answer.
func TestWebChatGPTInterveningTurnWithoutReplyFails(t *testing.T) {
	rig := newScriptRig(t, SourceChatGPT, func(_ int, at time.Time) (json.RawMessage, *NativeError) {
		return cgChain(scriptConv,
			cgm{id: "u1", role: "user", text: "what is two plus two?", at: at, status: "finished_successfully"},
			cgm{id: "u2", role: "user", text: "the owner's own question", at: at.Add(2 * time.Second), status: "finished_successfully"},
			cgm{id: "a2", role: "assistant", text: "The owner's answer.", at: at.Add(3 * time.Second), status: "finished_successfully", done: true},
		), nil
	})
	rig.agent.RequestTimeout = 2 * time.Second
	res := rig.ask(t, "grokbot", "new chat\nwhat is two plus two?")
	body := res.Reply.Body
	if res.Status != envelope.StatusFailed || strings.Contains(body, "owner's answer") || !strings.Contains(body, "another message was sent") || !strings.Contains(body, scriptConv) {
		t.Fatalf("%s %q", res.Status, body)
	}
	if c := rig.closed(); len(c) != 1 {
		t.Fatalf("closes = %v", c)
	}
}

// A user message with the same text as this request's, dated well before
// the send (beyond the clock-skew allowance), is not this request's.
func TestWebClockSkewGuardRejectsOldMatchingMessage(t *testing.T) {
	rig := newScriptRig(t, SourceChatGPT, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		msgs := []cgm{
			{id: "u0", role: "user", text: "ping", at: at.Add(-webClockSkew - time.Minute), status: "finished_successfully"},
			{id: "a0", role: "assistant", text: "stale pong", at: at.Add(-webClockSkew - 50*time.Second), status: "finished_successfully", done: true},
		}
		if n >= 2 {
			// Dated a minute early (inside the allowance): still this one.
			msgs = append(msgs,
				cgm{id: "u1", role: "user", text: "ping", at: at.Add(-time.Minute), status: "finished_successfully"},
				cgm{id: "a1", role: "assistant", text: "fresh pong", at: at.Add(time.Second), status: "finished_successfully", done: true})
		}
		return cgChain(scriptConv, msgs...), nil
	})
	res := rig.ask(t, "grokbot", "new chat\nping")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "fresh pong") || strings.Contains(body, "stale pong") {
		t.Fatalf("%s %q", res.Status, body)
	}
}

// The pre-send read fails with something other than not found: the agent
// cannot know the last user message before the send, so it binds to the
// first message after the send's time whose text is the one it sent.
func TestWebAnchorFallbackWhenPreSendReadFails(t *testing.T) {
	rig := newWebRig(t)
	failed := false
	fb := rig.browser
	rig.agent.Native = &Client{Channel: channelFunc(func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		if req.Op == OpChatGPTDetail && len(fb.sent()) == 1 && !failed {
			failed = true
			_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "source_failed", Message: "flaky"}})
			return err
		}
		return fb.Exchange(ctx, req, recv)
	})}
	rig.ask(t, "grokbot", "question A")
	fb.lag = 3
	res := rig.ask(t, "grokbot", "question B")
	if !failed {
		t.Fatal("the pre-send read never failed")
	}
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "You said: question B") || strings.Contains(res.Reply.Body, "question A") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

// end_turn:false vetoes an otherwise finished-looking ChatGPT message: the
// agent keeps polling until end_turn is no longer false.
func TestWebChatGPTEndTurnFalseKeepsPolling(t *testing.T) {
	rig := newScriptRig(t, SourceChatGPT, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		raw := cgChain(scriptConv,
			cgm{id: "u1", role: "user", text: "hi", at: at, status: "finished_successfully"},
			cgm{id: "a1", role: "assistant", text: "partial so far", at: at.Add(time.Second), status: "finished_successfully", done: true},
		)
		if n < 3 {
			raw = []byte(strings.Replace(string(raw), `"end_turn":true`, `"end_turn":false`, 1))
		}
		return raw, nil
	})
	res := rig.ask(t, "grokbot", "new chat\nhi")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	details := 0
	rig.mu.Lock()
	for _, o := range rig.ops {
		if o == OpChatGPTDetail {
			details++
		}
	}
	rig.mu.Unlock()
	if details != 4 {
		t.Fatalf("detail read %d times; end_turn:false must keep the agent polling", details)
	}
}

// claudeScript renders a claude.ai conversation: an old finished turn,
// then msgs, each a child of the one before.
func claudeScript(sentAt time.Time, msgs ...map[string]any) json.RawMessage {
	all := []map[string]any{
		{"uuid": "h0", "sender": "human", "index": 0, "created_at": sentAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano), "content": []any{map[string]any{"type": "text", "text": "long ago"}}, "parent_message_uuid": "root"},
		{"uuid": "a0", "sender": "assistant", "index": 1, "created_at": sentAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano), "content": []any{map[string]any{"type": "text", "text": "old finished answer"}}, "parent_message_uuid": "h0", "stop_reason": "end_turn"},
	}
	parent := "a0"
	for i, m := range msgs {
		m["index"] = i + 2
		m["parent_message_uuid"] = parent
		if _, ok := m["created_at"]; !ok {
			m["created_at"] = sentAt.Add(time.Duration(i+1) * time.Second).UTC().Format(time.RFC3339Nano)
		}
		parent = m["uuid"].(string)
		all = append(all, m)
	}
	b, _ := json.Marshal(map[string]any{"uuid": scriptConv, "name": "Chat", "current_leaf_message_uuid": parent, "chat_messages": all})
	return b
}

func caMsg(uuid, sender, text, stop string) map[string]any {
	m := map[string]any{"uuid": uuid, "sender": sender, "content": []any{map[string]any{"type": "text", "text": text}}}
	if stop != "" {
		m["stop_reason"] = stop
	}
	return m
}

// claude.ai: a later human turn after this request's reply is ignored; the
// answer is the reply to this request's own message.
func TestWebClaudeIgnoresLaterHumanTurn(t *testing.T) {
	rig := newScriptRig(t, SourceClaudeAI, func(_ int, at time.Time) (json.RawMessage, *NativeError) {
		return claudeScript(at,
			caMsg("h1", "human", "hello claude", ""),
			caMsg("a1", "assistant", "Hello to you.", "end_turn"),
			caMsg("h2", "human", "the owner's own question", ""),
			caMsg("a2", "assistant", "The owner's answer.", "end_turn"),
		), nil
	})
	res := rig.ask(t, "grokbot", "new chat\nhello claude")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "Hello to you.") || strings.Contains(body, "owner's answer") {
		t.Fatalf("%s %q", res.Status, body)
	}
}

// claude.ai without stop_reason: a reply that pauses for a few polls
// mid-generation is not taken as finished; only text that stays the same
// across the configured number of polls is.
func TestWebClaudePauseIsNotFinished(t *testing.T) {
	frames := []*string{nil, new("Hel"), new("Hello"), new("Hello"), new("Hello"), new("Hello there, friend.")}
	rig, _, _ := claudeRig(t, frames, "")
	rig.agent.ClaudeStableFor = time.Nanosecond
	res := rig.ask(t, "grokbot", "new chat\nhello claude")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "Hello there, friend.") {
		t.Fatalf("a paused partial reply was returned as finished: %s %q", res.Status, body)
	}
}

// claude.ai without stop_reason: the same text must also hold for at least
// ClaudeStableFor, however many polls see it.
func TestWebClaudeStableWindowSpansTime(t *testing.T) {
	var frames []*string
	frames = append(frames, nil)
	for range 8 {
		frames = append(frames, new("Hello")) // ~40ms at 5ms polls
	}
	frames = append(frames, new("Hello there, friend."))
	rig, _, _ := claudeRig(t, frames, "")
	rig.agent.ClaudeStablePolls = 2
	rig.agent.ClaudeStableFor = 150 * time.Millisecond
	res := rig.ask(t, "grokbot", "new chat\nhello claude")
	if body := res.Reply.Body; res.Status != envelope.StatusAnswered || !strings.Contains(body, "Hello there, friend.") {
		t.Fatalf("%s %q", res.Status, body)
	}
}

// The agent crashes (or its reply is lost) after the extension confirmed
// the send; the relay requeues the request when the claim lease runs out.
// The second delivery must not type the message into the site again: it
// resumes from the send journal, waits for the reply in the journaled
// conversation and answers.
func TestWebRequeuedRequestIsNotSentAgain(t *testing.T) {
	rig := newWebRigWith(t, relay.Config{ClaimLease: time.Millisecond}, true)
	fb := rig.browser
	crash := true
	rig.agent.Native = &Client{Channel: channelFunc(func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		if req.Op == OpChatGPTDetail && crash {
			panic("agent died mid-request")
		}
		return fb.Exchange(ctx, req, recv)
	})}
	ctx := t.Context()
	grok := rig.mesh.Client(t, "grokbot")
	sent, err := grok.Send(ctx, "chatgpt-web", "only once, please", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	// First delivery: the send goes through, then the agent dies before
	// it replies (no handleSafely, so no failure reply either).
	n, err := pollAndHandle(ctx, rig.agent.Relay, time.Second, func(ctx context.Context, req envelope.Request) {
		defer func() { _ = recover() }()
		rig.agent.Handle(ctx, req)
	})
	if err != nil || n != 1 {
		t.Fatalf("first delivery: %d %v", n, err)
	}
	if got := fb.sent(); len(got) != 1 {
		t.Fatalf("first delivery sends = %+v", got)
	}
	st, err := os.Stat(rig.agent.JournalPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("journal: %v %v", st, err)
	}
	if b, _ := os.ReadFile(rig.agent.JournalPath); !strings.Contains(string(b), sent.ID) || !strings.Contains(string(b), "conv-1") || strings.Contains(string(b), "only once") {
		t.Fatalf("journal holds the request id and conversation, not the message: %s", b)
	}

	// The claim lease runs out and the relay requeues the request.
	time.Sleep(5 * time.Millisecond)
	rig.mesh.Server.Sweep(ctx)
	crash = false
	res := rig.serve(t, grok, sent.ID)
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "You said: only once, please") || !strings.Contains(res.Reply.Body, "conv-1") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if got := fb.sent(); len(got) != 1 {
		t.Fatalf("the requeued request was sent to the browser again: %+v", got)
	}
	if b, _ := os.ReadFile(rig.agent.JournalPath); !strings.Contains(string(b), `"answered"`) {
		t.Fatalf("journal not marked answered: %s", b)
	}
}

// Journal entries older than the retention are pruned; newer ones stay.
func TestWebJournalPrunesOldEntries(t *testing.T) {
	rig := newWebRig(t)
	if err := os.MkdirAll(filepath.Dir(rig.agent.JournalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-rig.agent.journalRetention() - time.Minute).UTC().Format(time.RFC3339Nano)
	recent := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	seed := fmt.Sprintf(`{"requests":{"req-old":{"conversation_id":"conv-9","recorded":%q,"state":"sent"},"req-new":{"conversation_id":"conv-8","recorded":%q,"state":"answered"}}}`, old, recent)
	if err := os.WriteFile(rig.agent.JournalPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.ask(t, "grokbot", "hello")
	b, _ := os.ReadFile(rig.agent.JournalPath)
	if strings.Contains(string(b), "req-old") || !strings.Contains(string(b), "req-new") {
		t.Fatalf("journal after prune: %s", b)
	}
}

// A requeued request the journal says was already answered (its reply was
// lost on the way to the relay) is answered again from the conversation,
// without sending anything to the site.
func TestWebRequeuedAnsweredRequestRepliesWithoutSending(t *testing.T) {
	rig := newWebRig(t)
	rig.ask(t, "grokbot", "hello") // conv-1, user message u0
	ctx := t.Context()
	grok := rig.mesh.Client(t, "grokbot")
	sent, err := grok.Send(ctx, "chatgpt-web", "hello", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	rig.agent.journal(sent.ID, webSend{ConversationID: "conv-1", UserMessageID: "u0", Recorded: time.Now().UTC(), State: webSendAnswered})
	res := rig.serve(t, grok, sent.ID)
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "You said: hello") || !strings.Contains(res.Reply.Body, "conv-1") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if got := rig.browser.sent(); len(got) != 1 {
		t.Fatalf("sends = %+v, want only the first ask", got)
	}
}

var attachedClaim = regexp.MustCompile(`\d+ images? attached`)

// webImages is a reply's generated images, as readReply hands them to
// attachImages.
func webImages(imgs ...Image) []Conversation {
	return []Conversation{{Source: SourceChatGPT, ID: "conv-1", Messages: []Message{{Role: RoleAssistant, Images: imgs}}}}
}

// Each way attaching the reply's images can fail leaves a note in the
// body, never a claim that images were attached, and no image dir behind.
func TestWebAttachImagesFailures(t *testing.T) {
	png := Image{Name: "fox.png", MIME: "image/png", Data: fakePNG(2000)}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		setup func(r *webRig)
		conv  []Conversation
		want  string
	}{
		{name: "temp dir", setup: func(r *webRig) { r.agent.TempDir = filepath.Join(blocker, "tmp") }, conv: webImages(png), want: "The images could not be attached."},
		{name: "save images", conv: webImages(png, Image{Name: "evil.sh", Data: []byte("#!/bin/sh\nrm -rf /\n")}), want: "The images could not be attached."},
		{name: "upload error", setup: func(r *webRig) { r.mesh.Server.SetAttachmentDir(filepath.Join(blocker, "blobs")) }, conv: webImages(png), want: "the upload to the relay failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newWebRig(t)
			if c.setup != nil {
				c.setup(rig)
			}
			body := "answer"
			ids := rig.agent.attachImages(t.Context(), envelope.Request{ID: "req-1"}, c.conv, &body)
			if len(ids) != 0 || !strings.Contains(body, c.want) || attachedClaim.MatchString(body) {
				t.Fatalf("ids %v body %q", ids, body)
			}
			if left, _ := os.ReadDir(rig.agent.TempDir); len(left) != 0 {
				t.Fatalf("image dir left behind: %v", left)
			}
		})
	}
	t.Run("unsupported", func(t *testing.T) {
		rig := newWebRigWith(t, relay.Config{}, false)
		body := "answer"
		ids := rig.agent.attachImages(t.Context(), envelope.Request{ID: "req-1"}, webImages(png), &body)
		if len(ids) != 0 || !strings.Contains(body, "this relay does not support attachments") {
			t.Fatalf("ids %v body %q", ids, body)
		}
		if left, _ := os.ReadDir(rig.agent.TempDir); len(left) != 0 {
			t.Fatalf("image dir left behind: %v", left)
		}
	})
}

// A finished read that cannot be parsed into turns fails the request with
// a clear message instead of answering "(the reply was empty)".
func TestWebReadReplyUnparseable(t *testing.T) {
	rig := newWebRig(t)
	_, _, err := rig.agent.readReply(t.Context(), "conv-1", "u0", json.RawMessage(`{"mapping":null}`))
	if !errors.Is(err, errReplyUnreadable) {
		t.Fatalf("err = %v", err)
	}
	_, _, err = rig.agent.readReply(t.Context(), "conv-1", "missing", chatgptDetailJSON("conv-1", []fakeTurn{{prompt: "hi", reply: "hello", sentAt: time.Now()}}))
	if !errors.Is(err, errReplyUnreadable) {
		t.Fatalf("turn not found: err = %v", err)
	}
	msg := rig.agent.waitFailure(err, "conv-1")
	if !strings.Contains(msg, "the reply could not be read") || !strings.Contains(msg, "conv-1") || strings.Contains(msg, "empty") {
		t.Fatalf("%q", msg)
	}
}
