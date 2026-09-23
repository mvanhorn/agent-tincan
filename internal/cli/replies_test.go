package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// answered has grokbot ask muse and muse answer it, and returns the request.
func answered(t *testing.T, m *testrelay.Mesh, ask, answer string) envelope.Request {
	t.Helper()
	ctx := context.Background()
	muse := m.Client(t, "muse")
	req, err := m.Client(t, "grokbot").Send(ctx, "muse", ask, envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(ctx, req.ID, answer, envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	return req
}

// tincan inbox shows unseen replies first, then new requests, and a second
// inbox shows neither again.
func TestInboxPrintsRepliesThenRequests(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	req := answered(t, m, "call the garage", "Tue 3pm works")
	incoming, _ := m.Client(t, "instinct").Send(ctx, "grokbot", "summarize the report", envelope.KindAsk, "")

	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	cmd := inboxCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	heading := strings.Index(got, "Replies to your requests:")
	reply := strings.Index(got, "Tue 3pm works")
	request := strings.Index(got, "summarize the report")
	if heading < 0 || reply < heading || request < reply {
		t.Fatalf("inbox should list replies under the heading before requests:\n%s", got)
	}
	for _, want := range []string{req.ID, "muse", "answered", "call the garage", incoming.ID} {
		if !strings.Contains(got, want) {
			t.Fatalf("inbox missing %q:\n%s", want, got)
		}
	}

	out.Reset()
	cmd = inboxCmd()
	cmd.SetOut(&out)
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "Tue 3pm") || strings.Contains(got, "Replies") || !strings.Contains(got, "No requests waiting") {
		t.Fatalf("second inbox = %q", got)
	}
}

// A fresh session woken by a reply learns which request it was handling:
// codex (grokbot) asks hermes (instinct), hermes, holding the claim, asks
// claude-code (muse) without naming a parent, claude-code replies later, and
// hermes's next inbox shows the reply with the instruction to reply to
// codex's request.
func TestInboxReplyTellsAgentToFinishParent(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	codex, hermes, claude := m.Client(t, "grokbot"), m.Client(t, "instinct"), m.Client(t, "muse")
	parent, err := codex.Send(ctx, "instinct", "book the flight to Tokyo", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hermes.Claim(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	child, err := hermes.Send(ctx, "muse", "which airline does Matt prefer?", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claude.Claim(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := claude.Reply(ctx, child.ID, "ANA, aisle seat", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}

	useConfig(t, client.Config{Relay: m.URL("instinct"), Agent: "instinct"})
	cmd := inboxCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"ANA, aisle seat",
		"You asked: which airline does Matt prefer?",
		"while handling request " + parent.ID + " from grokbot: book the flight to Tokyo.",
		"That request is still open (status claimed).",
		"`tincan reply " + parent.ID + " \"...\"`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("inbox missing %q:\n%s", want, got)
		}
	}
}

// tincan wait exits when a reply to the agent's own request lands, tells it
// to run check_inbox, and leaves the reply for that check.
func TestWaitExitsOnUnseenReplyWithoutTakingIt(t *testing.T) {
	m := testrelay.New(t, relay.Config{PollHold: 2 * time.Second})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	cmd := waitCmd()
	cmd.SetArgs([]string{"--timeout", "10s"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	go func() {
		time.Sleep(200 * time.Millisecond)
		answered(t, m, "call the garage", "Tue 3pm works")
	}()
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "1 reply") || !strings.Contains(got, "check_inbox") || strings.Contains(got, "Tue 3pm") {
		t.Fatalf("wait printed %q", got)
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("wait must not consume the reply: unseen = %d", n)
	}
}

// tincan listen fires for an unseen reply too, counting it in
// TINCAN_WAITING, without marking it seen.
func TestListenFiresForUnseenReply(t *testing.T) {
	m := testrelay.New(t, relay.Config{PollHold: 2 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	answered(t, m, "call the garage", "Tue 3pm works")
	m.Client(t, "instinct").Send(ctx, "grokbot", "summarize the report", envelope.KindAsk, "")
	out := filepath.Join(t.TempDir(), "nudged")
	if err := listen(ctx, m.Client(t, "grokbot"), `echo "$TINCAN_WAITING" > `+out, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "2" {
		t.Fatalf("command saw TINCAN_WAITING=%q, want 2 (one request, one reply)", got)
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("listen must not consume the reply: unseen = %d", n)
	}
}

// The Claude Code channel pushes a short count-only event when a reply lands
// and leaves the reply unseen until check_inbox reads it.
func TestChannelPushesReplyWaiting(t *testing.T) {
	m := testrelay.New(t, relay.Config{PollHold: 2 * time.Second})
	srvT, cliT := mcp.NewInMemoryTransports()
	ch := mcpserver.NewChannelTransport(srvT)
	grok := m.Client(t, "grokbot")
	srv := mcpserver.NewWithOptions(grok, "test", mcpserver.ChannelOptions())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ss, err := srv.Connect(ctx, ch, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	got := make(chan string, 1)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "cc"}, nil).Connect(ctx, &pushTap{Transport: cliT, got: got}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	go pushRequests(ctx, grok, ch)
	go pushReplies(ctx, grok, ch)
	time.Sleep(100 * time.Millisecond)
	req := answered(t, m, "call the garage", "Tue 3pm works")
	select {
	case params := <-got:
		if !strings.Contains(params, "1 reply") || !strings.Contains(params, "check_inbox") || strings.Contains(params, "Tue 3pm") {
			t.Fatalf("channel event = %s", params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no channel event for the reply")
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("the push must not mark the reply seen: unseen = %d", n)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "check_inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "Tue 3pm works") || !strings.Contains(text, req.ID) {
		t.Fatalf("check_inbox = %q", text)
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 0 {
		t.Fatalf("check_inbox should mark the reply seen: unseen = %d", n)
	}
}

type hooks struct {
	mu   sync.Mutex
	msgs []string
}

func (h *hooks) server(t *testing.T) string {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		json.Unmarshal(raw, &body)
		h.mu.Lock()
		h.msgs = append(h.msgs, body["message"])
		h.mu.Unlock()
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func (h *hooks) got() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.msgs...)
}

// The live failure: a fresh-session asker (webhook wake) asks a slow
// teammate, its inline wait ends, and the answer lands later. The asker is
// woken for the reply, and its next inbox shows the reply exactly once. A
// reply that lands inside the inline wait wakes nobody.
func TestSlowReplyWakesAskerAndShowsOnce(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	var h hooks
	w := wake.New(wake.Config{"grokbot": {Method: wake.Webhook, URL: h.server(t)}}, m.Store,
		wake.Options{Debounce: time.Millisecond, ReplyGrace: 50 * time.Millisecond, Online: m.Server.Online, UnseenReplies: m.Server.UnseenReplies, ReplyRetries: []time.Duration{}})
	m.Server.SetEvents(w)
	ctx := context.Background()
	grok, muse := m.Client(t, "grokbot"), m.Client(t, "muse")

	res, err := grok.Ask(ctx, "muse", "call the garage and book a slot", "", time.Second)
	if err != nil || res.Reply != nil {
		t.Fatalf("inline ask = %+v, %v; want no reply yet", res, err)
	}
	if _, err := muse.Claim(ctx, res.Request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(ctx, res.Request.ID, "booked Tue 3pm", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if msgs := h.got(); len(msgs) != 1 || msgs[0] != wake.WaitingMessage(0, 1) {
		t.Fatalf("wakes = %q, want one reply nudge", msgs)
	}

	var first, second bytes.Buffer
	if err := checkInbox(ctx, grok, 0, &first, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Count(first.String(), "booked Tue 3pm") != 1 || !strings.Contains(first.String(), res.Request.ID) {
		t.Fatalf("first inbox = %q", first.String())
	}
	if err := checkInbox(ctx, grok, 0, &second, io.Discard); err != nil || strings.Contains(second.String(), "booked Tue 3pm") {
		t.Fatalf("second inbox = %q, %v", second.String(), err)
	}

	// A reply read inside the inline wait: no wake.
	go func() {
		time.Sleep(300 * time.Millisecond)
		reqs, _ := muse.Poll(ctx, 2*time.Second)
		for _, r := range reqs.Requests {
			muse.Claim(ctx, r.ID)
			muse.Reply(ctx, r.ID, "quick answer", envelope.StatusAnswered)
		}
	}()
	res, err = grok.Ask(ctx, "muse", "quick one", "", 5*time.Second)
	if err != nil || res.Reply == nil {
		t.Fatalf("inline ask = %+v, %v; want the reply inline", res, err)
	}
	w.Flush()
	if msgs := h.got(); len(msgs) != 1 {
		t.Fatalf("wakes = %q; a reply read inline must not wake the asker", msgs)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// tincan inbox acknowledges replies only after printing them: when the
// output cannot be written, the reply stays unseen for the next inbox.
func TestInboxAcksRepliesOnlyAfterPrinting(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	answered(t, m, "call the garage", "Tue 3pm works")
	grok := m.Client(t, "grokbot")
	if err := checkInbox(ctx, grok, 0, failWriter{}, io.Discard); err == nil {
		t.Fatal("want the write error")
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("an unprinted reply must stay unseen: unseen = %d", n)
	}
	var out bytes.Buffer
	if err := checkInbox(ctx, grok, 0, &out, io.Discard); err != nil || !strings.Contains(out.String(), "Tue 3pm works") {
		t.Fatalf("retry inbox = %q, %v", out.String(), err)
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 0 {
		t.Fatalf("a printed reply is acked: unseen = %d", n)
	}
}

// A poll that takes replies but never acks them (the client died) leaves
// them for the next poll.
func TestTakeWithoutAckLeavesRepliesUnseen(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	req := answered(t, m, "call the garage", "Tue 3pm works")
	grok := m.Client(t, "grokbot")
	for range 2 {
		in, err := grok.Poll(ctx, 0)
		if err != nil || len(in.Replies) != 1 || in.Replies[0].Request.ID != req.ID {
			t.Fatalf("poll = %+v, %v", in, err)
		}
	}
	in, _ := grok.Poll(ctx, 0)
	if err := grok.AckReplies(ctx, in.ReplyIDs()); err != nil {
		t.Fatal(err)
	}
	if in, err := grok.Poll(ctx, 0); err != nil || !in.Empty() {
		t.Fatalf("poll after ack = %+v, %v", in, err)
	}
}

// A wait that ends with a request and a reply together prints the claimed
// request and a count of the reply, without the reply text.
func TestFormatWaitWithRequestAndReply(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	answered(t, m, "call the garage", "Tue 3pm works")
	incoming, _ := m.Client(t, "instinct").Send(ctx, "grokbot", "summarize the report", envelope.KindAsk, "")
	grok := m.Client(t, "grokbot")
	in, err := grok.PollReplies(ctx, 0, client.RepliesKeep)
	if err != nil || len(in.Requests) != 1 || len(in.Replies) != 1 {
		t.Fatalf("poll = %+v, %v", in, err)
	}
	got := formatWait(ctx, grok, in)
	for _, want := range []string{incoming.ID, "summarize the report", wake.WaitingMessage(0, 1)} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatWait missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Tue 3pm works") || strings.Contains(got, "Replies to your requests") {
		t.Fatalf("formatWait must leave the reply text for check_inbox:\n%s", got)
	}
	if n := m.Server.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("unseen = %d, want 1", n)
	}
}

// flakyPusher fails its first push and records the rest.
type flakyPusher struct {
	ready chan struct{}
	mu    sync.Mutex
	tries int
	ok    []map[string]string
}

func (f *flakyPusher) Ready() <-chan struct{} { return f.ready }

func (f *flakyPusher) Push(_ context.Context, _ string, meta map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries++
	if f.tries == 1 {
		return io.ErrClosedPipe
	}
	f.ok = append(f.ok, meta)
	return nil
}

func (f *flakyPusher) pushed() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.ok...)
}

// A reply notice that fails to reach the session is pushed again later,
// rather than being remembered as pushed.
func TestChannelRetriesFailedReplyPush(t *testing.T) {
	old := replyRecheck
	replyRecheck = 50 * time.Millisecond
	t.Cleanup(func() { replyRecheck = old })
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	req := answered(t, m, "call the garage", "Tue 3pm works")
	f := &flakyPusher{ready: make(chan struct{})}
	close(f.ready)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); pushReplies(ctx, m.Client(t, "grokbot"), f) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.pushed()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if got := f.pushed(); len(got) == 0 || got[0]["request_ids"] != req.ID {
		t.Fatalf("pushes after a failed one = %v, want the reply %s again", got, req.ID)
	}
}

// A relay restart drops the waker's in-memory timers. On start the relay
// reschedules a reply wake for every agent still holding an unseen reply, so
// a restart inside the grace window still wakes the asker. Agents whose
// replies were read, or that do not wake relay-side, get nothing.
func TestResumeReplyWakesAfterRestart(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	answered(t, m, "call the garage", "Tue 3pm works") // grokbot's, unseen
	read := answered(t, m, "check the calendar", "free")
	if err := m.Client(t, "grokbot").AckReplies(ctx, []string{read.ID}); err != nil {
		t.Fatal(err)
	}
	inst, _ := m.Client(t, "instinct").Send(ctx, "muse", "book a table", envelope.KindAsk, "")
	muse := m.Client(t, "muse")
	muse.Claim(ctx, inst.ID)
	muse.Reply(ctx, inst.ID, "7pm", envelope.StatusAnswered) // instinct's, unseen, but wait-method

	var h hooks
	// The restarted relay's waker has no pending timers.
	w := wake.New(wake.Config{"grokbot": {Method: wake.Webhook, URL: h.server(t)}, "instinct": {Method: wake.Wait}}, m.Store,
		wake.Options{ReplyGrace: 50 * time.Millisecond, UnseenReplies: m.Server.UnseenReplies, ReplyRetries: []time.Duration{}})
	if err := resumeReplyWakes(ctx, m.Store, w); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if msgs := h.got(); len(msgs) != 1 || msgs[0] != wake.WaitingMessage(0, 1) {
		t.Fatalf("wakes after restart = %q, want one reply nudge for grokbot", msgs)
	}
}
