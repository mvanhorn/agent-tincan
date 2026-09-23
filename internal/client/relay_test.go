package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestAskGetsReplyInsideWindow(t *testing.T) {
	m := testrelay.New(t, relay.Config{PollHold: 5 * time.Second, MaxWait: 5 * time.Second})
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	go func() {
		in, err := inst.Poll(context.Background(), 5*time.Second)
		reqs := in.Requests
		if err != nil || len(reqs) != 1 {
			t.Errorf("instinct poll: %v %v", reqs, err)
			return
		}
		inst.Claim(context.Background(), reqs[0].ID)
		inst.Reply(context.Background(), reqs[0].ID, "3 rows", envelope.StatusAnswered)
	}()
	res, err := grok.Ask(context.Background(), "instinct", "what is in the report", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply == nil || res.Reply.Body != "3 rows" || res.Reply.From != "instinct" {
		t.Fatalf("ask result = %+v", res)
	}
	if got := client.FormatResult(res); !strings.Contains(got, "instinct replied") {
		t.Fatalf("formatted = %q", got)
	}
}

func TestAskWithoutReplyReturnsRequestID(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: time.Second})
	start := time.Now()
	res, err := m.Client(t, "grokbot").Ask(context.Background(), "muse", "call Joe's Garage", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Done() || res.Request.ID == "" || time.Since(start) > 3*time.Second {
		t.Fatalf("result = %+v after %v", res, time.Since(start))
	}
	if got := client.FormatResult(res); !strings.Contains(got, res.Request.ID) || !strings.Contains(got, "No reply yet") {
		t.Fatalf("formatted = %q", got)
	}
}

func TestRelayErrorsSurface(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	_, err := m.Client(t, "grokbot").Send(context.Background(), "nobody", "x", envelope.KindAsk, "")
	if !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("want 404 APIError, got %v", err)
	}
	_, err = m.Client(t, "admin").Send(context.Background(), "muse", "x", envelope.KindAsk, "")
	if !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("unjoined sender: want 403, got %v", err)
	}
}

func TestFormatRequestCarriesTeammateFraming(t *testing.T) {
	out := client.FormatRequest(envelope.Request{ID: "r9", From: "instinct", Hop: 2, Chain: []string{"instinct", "muse"}, Body: "new time Tue 3pm"})
	for _, want := range []string{"from instinct (your teammate)", "instinct -> muse", "as you would a request from Matt", "tincan reply r9", "new time Tue 3pm"} {
		if !strings.Contains(out, want) {
			t.Errorf("framing missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "untrusted") {
		t.Error("teammate requests must not be framed as untrusted")
	}
}

// Muse's default proxy cannot reach the tailnet, so relay traffic must go
// through the proxy saved in config, not HTTP_PROXY.
func TestProxyOverrideRoutesRelayTraffic(t *testing.T) {
	var seen string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.String() // a forward proxy sees the absolute URL
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[{"name":"grokbot","online":true,"wake":"webhook"}]}`))
	}))
	defer proxy.Close()
	r, err := client.NewRelay("http://tincan-relay", proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := r.Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if seen != "http://tincan-relay/v1/agents" || len(agents) != 1 || agents[0].Wake != "webhook" {
		t.Fatalf("proxy saw %q, agents %+v", seen, agents)
	}
	if _, err := client.NewRelay("", ""); err == nil {
		t.Fatal("empty relay URL should fail")
	}
}

// A client built from config names its agent on every relay call, so a
// machine running several agents is attributed per process.
func TestConfiguredAgentSentOnEveryRequest(t *testing.T) {
	seen := map[string]string{}
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = r.Header.Get(client.AgentHeader)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	r, err := client.NewRelayFor(client.Config{Relay: ts.URL, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r.Agents(ctx)
	r.Poll(ctx, 0)
	r.Peek(ctx, 0)
	r.Send(ctx, "grokbot", "hi", envelope.KindAsk, "")
	r.Get(ctx, "r1", 0)
	r.Claim(ctx, "r1")
	r.Reply(ctx, "r1", "ok", envelope.StatusAnswered)
	r.Cancel(ctx, "r1")
	r.Raw(ctx, "GET", "/v1/trace", nil, nil)
	if len(seen) != 8 { // poll and peek share a path
		t.Fatalf("calls seen = %v", seen)
	}
	for call, agent := range seen {
		if agent != "codex" {
			t.Errorf("%s sent agent header %q, want codex", call, agent)
		}
	}
	// No configured agent (before join, or an admin device): no header.
	bare, _ := client.NewRelayFor(client.Config{Relay: ts.URL})
	bare.Agents(ctx)
	if got := seen["GET /v1/agents"]; got != "" {
		t.Fatalf("unnamed client sent header %q", got)
	}
}

// claude-code and codex on one machine, through the real relay.
func TestTwoAgentsOnOneMachineThroughClient(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	codex := m.JoinOnMachineOf(t, "muse", "codex")
	muse := m.Client(t, "muse")
	ctx := context.Background()
	for name, c := range map[string]*client.Relay{"codex": codex, "muse": muse} {
		req, err := c.Send(ctx, "grokbot", "hi from "+name, envelope.KindAsk, "")
		if err != nil || req.From != name {
			t.Fatalf("%s send: from %q, %v", name, req.From, err)
		}
	}
	sent, err := m.Client(t, "grokbot").Send(ctx, "codex", "for codex", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if in, err := muse.Poll(ctx, 0); err != nil || len(in.Requests) != 0 {
		t.Fatalf("muse must not get codex's request: %+v %v", in, err)
	}
	if in, err := codex.Poll(ctx, 0); err != nil || len(in.Requests) != 1 || in.Requests[0].ID != sent.ID {
		t.Fatalf("codex poll: %+v %v", in, err)
	}
}

// An invite can carry a kind that shows in the roster after join; an admin
// can change it; a joined non-admin cannot.
func TestInviteKindAndSetKindThroughClient(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	admin := m.Client(t, "admin")
	code, err := admin.InviteKind(ctx, "cx", "hermes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "stranger").Join(ctx, code); err != nil {
		t.Fatal(err)
	}
	kindOf := func(name string) string {
		t.Helper()
		agents, err := admin.Agents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range agents {
			if a.Name == name {
				return a.Kind
			}
		}
		t.Fatalf("no %s in %+v", name, agents)
		return ""
	}
	if k := kindOf("cx"); k != "hermes" {
		t.Fatalf("kind after join = %q", k)
	}
	if err := admin.SetKind(ctx, "cx", "codex"); err != nil {
		t.Fatal(err)
	}
	if k := kindOf("cx"); k != "codex" {
		t.Fatalf("kind after set = %q", k)
	}
	if err := m.Client(t, "grokbot").SetKind(ctx, "cx", "openclaw"); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("non-admin set kind: %v", err)
	}
	if k := kindOf("cx"); k != "codex" {
		t.Fatalf("non-admin changed kind to %q", k)
	}
}

func TestBaseIsTheRelayURL(t *testing.T) {
	r, err := client.NewRelay("http://tincan-relay/", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Base() != "http://tincan-relay" {
		t.Fatalf("base = %q", r.Base())
	}
}
