package client_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// A client with a session names it on every call; one without sends no
// session header at all.
func TestClientSendsItsSession(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"agents":[]}`))
	}))
	defer srv.Close()
	r, err := client.NewRelayFor(client.Config{Relay: srv.URL, Agent: "grokbot"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := got[client.SessionHeader]; ok {
		t.Fatalf("a client without a session sent %q", got.Get(client.SessionHeader))
	}
	r.SetSession("mcp-1")
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got.Get(client.SessionHeader) != "mcp-1" {
		t.Fatalf("headers = %v", got)
	}
}

// Two sessions of one agent each ask muse and each poll back only the reply
// to its own ask; a sessionless poll still sees both.
func TestSessionsGetTheirOwnReplies(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a, b := m.Client(t, "grokbot"), m.Client(t, "grokbot")
	a.SetSession("mcp-a")
	b.SetSession("mcp-b")
	muse := m.Client(t, "muse")
	ctx := t.Context()
	var sent []envelope.Request
	for _, c := range []*client.Relay{a, b} {
		req, err := c.Send(ctx, "muse", "which day?", envelope.KindAsk, "", false)
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, req)
		if _, err := muse.Claim(ctx, req.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := muse.Reply(ctx, req.ID, "Tuesday", envelope.StatusAnswered); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []*client.Relay{a, b} {
		in, err := c.PollReplies(ctx, 0, client.RepliesKeep)
		if err != nil {
			t.Fatal(err)
		}
		if len(in.Replies) != 1 || in.Replies[0].Request.ID != sent[i].ID {
			t.Fatalf("session %s got %+v, want only %s", c.Session(), in.Replies, sent[i].ID)
		}
	}
	in, err := m.Client(t, "grokbot").PollReplies(ctx, 0*time.Second, client.RepliesKeep)
	if err != nil || len(in.Replies) != 2 {
		t.Fatalf("sessionless poll = %+v, %v; want both", in.Replies, err)
	}
}
