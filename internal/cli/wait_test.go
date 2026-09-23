package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestChannelPushesClaimedRequests(t *testing.T) {
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
	go pushRequests(ctx, m.Client(t, "muse"), ch)
	sent, _ := m.Client(t, "instinct").Send(ctx, "muse", "call the dentist", envelope.KindAsk, "")
	select {
	case params := <-got:
		if !strings.Contains(params, sent.ID) || !strings.Contains(params, "call the dentist") || !strings.Contains(params, `"from":"instinct"`) {
			t.Fatalf("channel event = %s", params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no channel event")
	}
	res, _ := m.Client(t, "instinct").Get(ctx, sent.ID, 0)
	if res.Status != envelope.StatusClaimed {
		t.Fatalf("pushed request status = %s, want claimed", res.Status)
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
