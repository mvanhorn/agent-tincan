package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// A flaky tailnet path (Instinct saw this) must not end a background wait.
func TestWaitRetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			http.Error(w, "bad gateway", http.StatusBadGateway)
		case 2:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"requests":[{"id":"r1","from":"grokbot","to":"muse","body":"call Joe's Garage"}]}`))
		}
	}))
	defer ts.Close()
	r, _ := client.NewRelay(ts.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in, err := waitForInbox(ctx, r, 0, client.RepliesKeep)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].ID != "r1" {
		t.Fatalf("wait = %+v, %v after %d calls", in, err, calls.Load())
	}
}

func TestWaitStopsWhenNotJoined(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not a joined agent"}`, http.StatusForbidden)
	}))
	defer ts.Close()
	r, _ := client.NewRelay(ts.URL, "")
	if _, err := waitForInbox(context.Background(), r, 0, client.RepliesKeep); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("want 403, got %v", err)
	}
}

func TestListenRunsCommandWithoutTakingRequests(t *testing.T) {
	m := testrelay.New(t, relay.Config{PollHold: 2 * time.Second})
	muse := m.Client(t, "muse")
	out := filepath.Join(t.TempDir(), "nudged")
	go func() {
		time.Sleep(200 * time.Millisecond)
		m.Client(t, "grokbot").Send(context.Background(), "muse", "call Joe's Garage", envelope.KindAsk, "")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := listen(ctx, muse, `echo "$TINCAN_WAITING" > `+out, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("command saw TINCAN_WAITING=%q", got)
	}
	in, err := muse.Poll(context.Background(), 0)
	if err != nil || len(in.Requests) != 1 {
		t.Fatalf("request should still be waiting for muse: %+v %v", in, err)
	}
}

// The live bug: every open Claude Code session ran a channel, each claimed
// requests it pushed, and sessions that dropped the push left them claimed
// and unanswered. A channel now only announces: the request stays queued
// until the model calls check_inbox, which claims it.
func TestChannelAnnouncesRequestWithoutClaiming(t *testing.T) {
	m := testrelay.New(t, relay.Config{PollHold: 2 * time.Second})
	srvT, cliT := mcp.NewInMemoryTransports()
	ch := mcpserver.NewChannelTransport(srvT)
	srv := mcpserver.NewWithOptions(m.Client(t, "muse"), "test", mcpserver.ChannelOptions())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ss, err := srv.Connect(ctx, ch, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	got := make(chan string, 1)
	tp := &pushTap{Transport: cliT, got: got}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "cc"}, nil).Connect(ctx, tp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	startWaiting(t, m.Client(t, "muse"), ch)
	inst := m.Client(t, "instinct")
	sent, _ := inst.Send(ctx, "muse", "call the dentist", envelope.KindAsk, "")
	select {
	case params := <-got:
		for _, want := range []string{"1 Agent Tincan item waiting from instinct", "check_inbox", `"from":"instinct"`, `"count":"1"`, sent.ID} {
			if !strings.Contains(params, want) {
				t.Fatalf("channel event missing %q: %s", want, params)
			}
		}
		if strings.Contains(params, "call the dentist") {
			t.Fatalf("channel event carries the request body: %s", params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no channel event")
	}
	res, _ := inst.Get(ctx, sent.ID, 0)
	if res.Status != envelope.StatusQueued {
		t.Fatalf("announced request status = %s, want queued (the push must not claim)", res.Status)
	}
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "check_inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if text := out.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "call the dentist") {
		t.Fatalf("check_inbox = %q", text)
	}
	res, _ = inst.Get(ctx, sent.ID, 0)
	if res.Status != envelope.StatusClaimed {
		t.Fatalf("status after check_inbox = %s, want claimed", res.Status)
	}
}

// recordPusher records every push.
type recordPusher struct {
	ready chan struct{}
	mu    sync.Mutex
	got   []map[string]string
	texts []string
}

func newRecordPusher() *recordPusher {
	p := &recordPusher{ready: make(chan struct{})}
	close(p.ready)
	return p
}

func (p *recordPusher) Ready() <-chan struct{} { return p.ready }

func (p *recordPusher) Push(_ context.Context, content string, meta map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, meta)
	p.texts = append(p.texts, content)
	return nil
}

func (p *recordPusher) pushes() []map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]string(nil), p.got...)
}

func waitPushes(t *testing.T, p *recordPusher, n int) []map[string]string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(p.pushes()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return p.pushes()
}

// startWaiting runs pushWaiting until the test ends, and waits for it to
// stop before earlier cleanups (fastChannel) restore the timing vars.
func startWaiting(t *testing.T, r *client.Relay, p channelPusher) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pushWaiting(ctx, r, p) }()
	t.Cleanup(func() { cancel(); <-done })
}

func fastChannel(t *testing.T, recheck, reannounce time.Duration) {
	oldR, oldA := waitingRecheck, reannounceAfter
	waitingRecheck, reannounceAfter = recheck, reannounce
	t.Cleanup(func() { waitingRecheck, reannounceAfter = oldR, oldA })
}

// A pending set is announced once. Looking again with nothing new pushes
// nothing; a new request pushes again, naming everything still waiting.
func TestChannelDoesNotRepushSamePending(t *testing.T) {
	fastChannel(t, 20*time.Millisecond, time.Hour)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first, _ := m.Client(t, "instinct").Send(ctx, "muse", "call the dentist", envelope.KindAsk, "")
	p := newRecordPusher()
	startWaiting(t, m.Client(t, "muse"), p)
	if got := waitPushes(t, p, 1); len(got) != 1 || got[0]["request_ids"] != first.ID {
		t.Fatalf("first pushes = %v", got)
	}
	time.Sleep(200 * time.Millisecond) // about ten more looks
	if got := p.pushes(); len(got) != 1 {
		t.Fatalf("same pending set pushed again: %v", got)
	}
	second, _ := m.Client(t, "grokbot").Send(ctx, "muse", "book a table", envelope.KindAsk, "")
	got := waitPushes(t, p, 2)
	if len(got) != 2 || got[1]["count"] != "2" || got[1]["request_ids"] != first.ID+","+second.ID || got[1]["from"] != "instinct,grokbot" {
		t.Fatalf("pushes after a new request = %v", got)
	}
	res, _ := m.Client(t, "instinct").Get(ctx, first.ID, 0)
	if res.Status != envelope.StatusQueued {
		t.Fatalf("status = %s, want queued", res.Status)
	}
}

// A push the session dropped is not permanent: after a quiet period the
// channel announces what is still waiting again.
func TestChannelReannouncesAfterQuietPeriod(t *testing.T) {
	fastChannel(t, 20*time.Millisecond, 150*time.Millisecond)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sent, _ := m.Client(t, "instinct").Send(ctx, "muse", "call the dentist", envelope.KindAsk, "")
	p := newRecordPusher()
	startWaiting(t, m.Client(t, "muse"), p)
	got := waitPushes(t, p, 2)
	if len(got) < 2 || got[1]["request_ids"] != sent.ID {
		t.Fatalf("pushes = %v, want the waiting request announced again", got)
	}
}

type pushTap struct {
	mcp.Transport
	got chan string
}

type pushTapConn struct {
	mcp.Connection
	got chan string
}

func (p *pushTap) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := p.Transport.Connect(ctx)
	return &pushTapConn{Connection: c, got: p.got}, err
}

func (c *pushTapConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == mcpserver.ChannelMethod {
		select {
		case c.got <- string(req.Params):
		default:
		}
	}
	return msg, err
}
