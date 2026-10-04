package client_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	res, err := grok.Ask(context.Background(), "instinct", "what is in the report", "", 5*time.Second, false)
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
	res, err := m.Client(t, "grokbot").Ask(context.Background(), "muse", "call Joe's Garage", "", time.Second, false)
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
	_, err := m.Client(t, "grokbot").Send(context.Background(), "nobody", "x", envelope.KindAsk, "", false)
	if !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("want 404 APIError, got %v", err)
	}
	_, err = m.Client(t, "admin").Send(context.Background(), "muse", "x", envelope.KindAsk, "", false)
	if !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("unjoined sender: want 403, got %v", err)
	}
}

func TestFormatRequestCarriesTeammateFraming(t *testing.T) {
	out := client.FormatRequest(envelope.Request{ID: "r9", From: "instinct", Hop: 2, Chain: []string{"instinct", "muse"}, Body: "new time Tue 3pm"})
	for _, want := range []string{"from instinct (your teammate)", "instinct -> muse", "as you would a request from the owner", "tincan reply r9", "new time Tue 3pm"} {
		if !strings.Contains(out, want) {
			t.Errorf("framing missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "untrusted") {
		t.Error("teammate requests must not be framed as untrusted")
	}
	if strings.Contains(out, "Matt") {
		t.Errorf("request framing names a specific person:\n%s", out)
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
	r.Send(ctx, "grokbot", "hi", envelope.KindAsk, "", false)
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
		req, err := c.Send(ctx, "grokbot", "hi from "+name, envelope.KindAsk, "", false)
		if err != nil || req.From != name {
			t.Fatalf("%s send: from %q, %v", name, req.From, err)
		}
	}
	sent, err := m.Client(t, "grokbot").Send(ctx, "codex", "for codex", envelope.KindAsk, "", false)
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

func TestAgentLastSeen(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	for _, tc := range []struct {
		last time.Time
		want string
	}{
		{time.Time{}, "never seen"},
		{now.Add(-20 * time.Second), "last seen just now"},
		{now.Add(-12 * time.Minute), "last seen 12m ago"},
		{now.Add(-3*time.Hour - 10*time.Minute), "last seen 3h ago"},
		{now.Add(-50 * time.Hour), "last seen 2d ago"},
		{now.Add(time.Minute), "last seen just now"}, // clock skew
	} {
		if got := (client.AgentInfo{LastPoll: tc.last}).LastSeen(now); got != tc.want {
			t.Errorf("LastSeen(%v) = %q, want %q", now.Sub(tc.last), got, tc.want)
		}
	}
}

// LastSeen counts any call to the relay, so it reports the newer of the last
// poll and the last activity. A webhook agent never polls but still shows up.
func TestAgentLastSeenUsesNewerOfPollAndActivity(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	for _, tc := range []struct {
		poll, active time.Time
		want         string
	}{
		{time.Time{}, now.Add(-3 * time.Minute), "last seen 3m ago"},
		{now.Add(-3 * time.Hour), now.Add(-5 * time.Minute), "last seen 5m ago"},
		{now.Add(-7 * time.Minute), now.Add(-2 * time.Hour), "last seen 7m ago"},
		{time.Time{}, time.Time{}, "never seen"},
	} {
		if got := (client.AgentInfo{LastPoll: tc.poll, LastActive: tc.active}).LastSeen(now); got != tc.want {
			t.Errorf("LastSeen(poll %v, active %v) = %q, want %q", tc.poll, tc.active, got, tc.want)
		}
	}
}

// Against a relay without reply generations every reply arrives with
// generation 0; those must be acknowledged by id, the only form it reads.
func TestAckRepliesFallsBackToIDs(t *testing.T) {
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	if err := r.AckReplies(t.Context(), nil, envelope.ReplyAck{ID: "old"}, envelope.ReplyAck{ID: "new", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	if string(got["ids"]) != `["old"]` || string(got["acks"]) != `[{"id":"new","generation":7}]` {
		t.Fatalf("ack body = ids %s acks %s", got["ids"], got["acks"])
	}
	got = nil
	if err := r.AckReplies(t.Context(), nil, envelope.ReplyAck{ID: "old"}); err != nil {
		t.Fatal(err)
	}
	if string(got["ids"]) != `["old"]` || got["acks"] != nil {
		t.Fatalf("older relay ack = %v", got)
	}
}

func TestAskWithoutWaitReportsHeld(t *testing.T) {
	for _, status := range []envelope.Status{envelope.StatusHeld, ""} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(envelope.Request{ID: "r1", To: "muse", Status: status})
		}))
		r, _ := client.NewRelay(srv.URL, "")
		res, err := r.Ask(t.Context(), "muse", "call the restaurant", "", 0, false)
		srv.Close()
		want := envelope.StatusQueued
		if status == envelope.StatusHeld {
			want = envelope.StatusHeld
		}
		if err != nil || res.Status != want {
			t.Fatalf("relay status %q: ask = %s, %v; want %s", status, res.Status, err, want)
		}
	}
}

func TestProgressOldRelay(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/capabilities" {
					t.Errorf("unexpected call: %s", r.URL.Path)
				}
				w.WriteHeader(status)
				fmt.Fprint(w, "{}")
			}))
			defer srv.Close()
			r, err := client.NewRelay(srv.URL, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Progress(context.Background(), "r1", "working"); err == nil || !strings.Contains(err.Error(), "upgrade the relay") {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func TestAskReturnsLatestProgress(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: time.Second})
	grok, muse := m.Client(t, "grokbot"), m.Client(t, "muse")
	done := make(chan error, 1)
	go func() {
		in, err := muse.Poll(context.Background(), 2*time.Second)
		if err != nil {
			done <- err
			return
		}
		if len(in.Requests) != 1 {
			done <- fmt.Errorf("requests: %d", len(in.Requests))
			return
		}
		id := in.Requests[0].ID
		if _, err = muse.Claim(context.Background(), id); err == nil {
			time.Sleep(100 * time.Millisecond)
			err = muse.Progress(context.Background(), id, "calling now")
		}
		done <- err
	}()
	res, err := grok.Ask(context.Background(), "muse", "call restaurant", "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := client.FormatResult(res); !strings.Contains(got, "claimed by muse") || !strings.Contains(got, "calling now") {
		t.Fatalf("ask: %s", got)
	}
}

func TestExplicitUrgencyAcrossSendMethods(t *testing.T) {
	methods := map[string]func(*client.Relay, bool) (envelope.Request, error){
		"Send": func(r *client.Relay, urgent bool) (envelope.Request, error) {
			return r.Send(t.Context(), "muse", "message", envelope.KindAsk, "", urgent)
		},
		"Ask": func(r *client.Relay, urgent bool) (envelope.Request, error) {
			result, err := r.Ask(t.Context(), "muse", "message", "", 0, urgent)
			return result.Request, err
		},
		"SendAttached": func(r *client.Relay, urgent bool) (envelope.Request, error) {
			return r.SendAttached(t.Context(), "muse", "message", envelope.KindAsk, "", nil, urgent)
		},
		"AskAttached": func(r *client.Relay, urgent bool) (envelope.Request, error) {
			result, err := r.AskAttached(t.Context(), "muse", "message", "", nil, 0, urgent)
			return result.Request, err
		},
	}
	for name, send := range methods {
		for _, urgent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/urgent=%v", name, urgent), func(t *testing.T) {
				m := testrelay.New(t, relay.Config{})
				req, err := send(m.Client(t, "grokbot"), urgent)
				if err != nil || req.Urgent != urgent {
					t.Fatalf("request = %+v, %v; want urgent=%v", req, err, urgent)
				}
			})
		}
	}
}

// A send response's target facts reach the Result that Ask returns, whether
// Ask waited (and so returns a separate Get) or not.
func TestAskCarriesScheduleTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/send" {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"r1","to":"fo","status":"queued","target":{"check_every_seconds":300,"expect_reply_seconds":600}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(envelope.Result{Request: envelope.Request{ID: "r1", To: "fo"}, Status: envelope.StatusQueued})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	want := envelope.Target{CheckEverySeconds: 300, ExpectReplySeconds: 600}
	for _, wait := range []time.Duration{0, time.Second} {
		res, err := r.Ask(t.Context(), "fo", "hi", "", wait, false)
		if err != nil || res.Request.ID != "r1" || res.Target == nil || *res.Target != want {
			t.Errorf("Ask wait %v: target %+v, err %v", wait, res.Target, err)
		}
		res, err = r.AskAttached(t.Context(), "fo", "hi", "", nil, wait, false)
		if err != nil || res.Request.ID != "r1" || res.Target == nil || *res.Target != want {
			t.Errorf("AskAttached wait %v: target %+v, err %v", wait, res.Target, err)
		}
	}
}

// A service kind lists with its stock good-at line until an admin sets
// one, and clearing it brings the stock line back (AE1). A general kind lists
// with no line until an admin sets one. A non-admin cannot set a line (AE3).
func TestGoodAtLinesThroughClient(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	admin := m.Client(t, "admin")
	code, err := admin.InviteKind(ctx, "history", "history")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "stranger").Join(ctx, code); err != nil {
		t.Fatal(err)
	}
	goodAt := func(name string) string {
		t.Helper()
		agents, err := admin.Agents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range agents {
			if a.Name == name {
				return a.GoodAt
			}
		}
		t.Fatalf("no %s in %+v", name, agents)
		return ""
	}
	stock := goodAt("history")
	if !strings.Contains(stock, "ChatGPT") {
		t.Fatalf("history stock line = %q", stock)
	}
	if _, err := admin.SetGoodAt(ctx, "history", "past chats, including images"); err != nil {
		t.Fatal(err)
	}
	if got := goodAt("history"); got != "past chats, including images" {
		t.Fatalf("history line after set = %q", got)
	}
	if _, err := admin.SetGoodAt(ctx, "history", ""); err != nil {
		t.Fatal(err)
	}
	if got := goodAt("history"); got != stock {
		t.Fatalf("history line after clear = %q, want stock %q", got, stock)
	}

	if got := goodAt("muse"); got != "" {
		t.Fatalf("muse line before set = %q, want none", got)
	}
	if _, err := admin.SetGoodAt(ctx, "muse", "phone calls; fast pickup"); err != nil {
		t.Fatal(err)
	}
	if got := goodAt("muse"); got != "phone calls; fast pickup" {
		t.Fatalf("muse line after set = %q", got)
	}

	if _, err := m.Client(t, "grokbot").SetGoodAt(ctx, "muse", "anything"); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("non-admin set good-at: %v", err)
	}
	if got := goodAt("muse"); got != "phone calls; fast pickup" {
		t.Fatalf("non-admin changed muse line to %q", got)
	}
}
