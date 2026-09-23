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
		wake.Options{Debounce: time.Millisecond, ReplyGrace: 50 * time.Millisecond, Online: m.Server.Online, UnseenReplies: m.Server.UnseenReplies})
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

	first, err := checkInbox(ctx, grok, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(first, "booked Tue 3pm") != 1 || !strings.Contains(first, res.Request.ID) {
		t.Fatalf("first inbox = %q", first)
	}
	second, err := checkInbox(ctx, grok, 0)
	if err != nil || strings.Contains(second, "booked Tue 3pm") {
		t.Fatalf("second inbox = %q, %v", second, err)
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
