package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestWaitAnswersPingAndKeepsWaiting(t *testing.T) {
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	target, sender := m.Client(t, "muse"), m.Client(t, "grokbot")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := target.Peek(ctx, 0); err != nil {
		t.Fatal(err)
	}
	ping, err := sender.Send(ctx, "muse", "", envelope.KindPing, "", false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan client.Inbox, 1)
	go func() { in, _, _ := waitForInbox(ctx, target, time.Second, client.RepliesKeep); done <- in }()
	pong, err := sender.Get(ctx, ping.ID, 2*time.Second)
	if err != nil || pong.Reply == nil {
		t.Fatalf("pong: %+v %v", pong, err)
	}
	select {
	case in := <-done:
		t.Fatalf("wait ended on ping: %+v", in)
	default:
	}
	ask, err := sender.Send(ctx, "muse", "real work", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case in := <-done:
		if len(in.Requests) != 1 || in.Requests[0].ID != ask.ID {
			t.Fatalf("inbox: %+v", in)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestListenAnswersPingsWithoutExec(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	target, sender := m.Client(t, "muse"), m.Client(t, "grokbot")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := target.Peek(ctx, 0); err != nil {
		t.Fatal(err)
	}
	var last envelope.Request
	for range 12 {
		var err error
		last, err = sender.Send(ctx, "muse", "", envelope.KindPing, "", false)
		if err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(t.TempDir(), "executed")
	done := make(chan error, 1)
	go func() { done <- listen(ctx, target, "touch "+marker, true) }()
	pong, err := sender.Get(ctx, last.ID, 2*time.Second)
	if err != nil || pong.Reply == nil {
		t.Fatalf("pong: %+v %v", pong, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("exec ran for ping: %v", err)
	}
	if _, err := sender.Send(ctx, "muse", "real work", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
}

func TestTraceHidesPings(t *testing.T) {
	in := []envelope.Result{{Request: envelope.Request{Kind: envelope.KindPing}}, {Request: envelope.Request{Kind: envelope.KindAsk}}}
	if got := filterPingTraces(in, false); len(got) != 1 || got[0].Request.Kind != envelope.KindAsk {
		t.Fatalf("filtered: %+v", got)
	}
	if got := filterPingTraces(in, true); len(got) != 2 {
		t.Fatalf("all: %+v", got)
	}
}

func TestPingCommand(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmtBool(jsonOutput), func(t *testing.T) {
			m := testrelay.New(t, relay.Config{PollHold: time.Second})
			target := m.Client(t, "muse")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if _, err := target.Peek(ctx, 0); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "config.json"))
			t.Setenv("TINCAN_RELAY", "")
			t.Setenv("TINCAN_PROXY", "")
			if err := client.SaveConfig(client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"}); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- checkInbox(ctx, target, time.Second, &bytes.Buffer{}, &bytes.Buffer{}) }()
			cmd := pingCmd()
			args := []string{"muse", "--wait", "3s"}
			if jsonOutput {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if jsonOutput {
				var result struct {
					Result      client.Result `json:"result"`
					RoundTripMS int64         `json:"round_trip_ms"`
				}
				if err := json.Unmarshal(out.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Result.Status != envelope.StatusAnswered || result.Result.Reply == nil {
					t.Fatalf("result: %s", out.String())
				}
			} else if !strings.Contains(out.String(), "muse: pong (answered by inbox") {
				t.Fatalf("output: %s", out.String())
			}
		})
	}
}

func fmtBool(b bool) string {
	if b {
		return "json"
	}
	return "text"
}

func TestInboxJSONFiltersPingAmongWork(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	target, sender := m.Client(t, "muse"), m.Client(t, "grokbot")
	if _, err := target.Peek(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	ping, err := sender.Send(t.Context(), "muse", "", envelope.KindPing, "", false)
	if err != nil {
		t.Fatal(err)
	}
	ask, err := sender.Send(t.Context(), "muse", "real work", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := checkInboxJSON(t.Context(), target, 0, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), ping.ID) || !strings.Contains(out.String(), ask.ID) {
		t.Fatalf("inbox: %s", out.String())
	}
}

func TestTraceFilterBeforeLimit(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	target, sender := m.Client(t, "muse"), m.Client(t, "grokbot")
	if _, err := target.Peek(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	ask, err := sender.Send(t.Context(), "muse", "real work", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(t.Context(), "muse", "", envelope.KindPing, "", false); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Traces []envelope.Result `json:"traces"`
	}
	if err := m.Client(t, "admin").Raw(t.Context(), "GET", "/v1/trace?limit=1&exclude_pings=true", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Traces) != 1 || out.Traces[0].Request.ID != ask.ID {
		t.Fatalf("traces: %+v", out)
	}
}

func TestPingEndedIdentifiesRequest(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmtBool(asJSON), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					fmt.Fprint(w, `{"id":"ping-id","to":"hermes","kind":"ping"}`)
					return
				}
				fmt.Fprint(w, `{"request":{"id":"ping-id","to":"hermes","kind":"ping"},"status":"expired"}`)
			}))
			defer ts.Close()
			t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "config.json"))
			t.Setenv("TINCAN_RELAY", "")
			t.Setenv("TINCAN_PROXY", "")
			if err := client.SaveConfig(client.Config{Relay: ts.URL, Agent: "grokbot"}); err != nil {
				t.Fatal(err)
			}
			cmd := pingCmd()
			args := []string{"hermes"}
			if asJSON {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.ExecuteContext(t.Context())
			if err == nil || err.Error() != "ping hermes (request ping-id) ended: expired" {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

// A Claude Code session's channel loop peeks with ping support, so the relay
// admits pings for it; the loop must answer them itself, without a notice or
// a model turn.
func TestChannelAnswersPingsWithoutNotice(t *testing.T) {
	fastChannel(t, 20*time.Millisecond, time.Hour)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	target, sender := m.Client(t, "muse"), m.Client(t, "grokbot")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := target.Peek(ctx, 0); err != nil {
		t.Fatal(err)
	}
	ping, err := sender.Send(ctx, "muse", "", envelope.KindPing, "", false)
	if err != nil {
		t.Fatal(err)
	}
	p := newRecordPusher()
	startWaiting(t, target, p)
	pong, err := sender.Get(ctx, ping.ID, 2*time.Second)
	if err != nil || pong.Reply == nil || !strings.Contains(pong.Reply.Body, "answered by channel") {
		t.Fatalf("pong: %+v %v", pong, err)
	}
	if got := p.pushes(); len(got) != 0 {
		t.Fatalf("ping announced: %v", got)
	}
	ask, err := sender.Send(ctx, "muse", "real work", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPushes(t, p, 1); len(got) != 1 || got[0]["request_ids"] != ask.ID || got[0]["count"] != "1" {
		t.Fatalf("pushes = %v", got)
	}
}
