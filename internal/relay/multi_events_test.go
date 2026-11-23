package relay_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

type spyEvents struct {
	mu         sync.Mutex
	queued     []string
	requeued   []string
	replied    []string
	needsInput []string
}

func (s *spyEvents) Queued(_ context.Context, req envelope.Request) {
	s.mu.Lock()
	s.queued = append(s.queued, req.ID)
	s.mu.Unlock()
}

func (s *spyEvents) Requeued(_ context.Context, req envelope.Request) {
	s.mu.Lock()
	s.requeued = append(s.requeued, req.ID)
	s.mu.Unlock()
}

func (s *spyEvents) Replied(_ context.Context, req envelope.Request) {
	s.mu.Lock()
	s.replied = append(s.replied, req.ID)
	s.mu.Unlock()
}

func (s *spyEvents) NeedsInput(_ context.Context, req envelope.Request) {
	s.mu.Lock()
	s.needsInput = append(s.needsInput, req.ID)
	s.mu.Unlock()
}

func TestMultiEventsFanOutAndClarifier(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: 2 * time.Second})
	a := &spyEvents{}
	b := &spyEvents{}
	m.Server.SetEvents(relay.MultiEvents{a, b})

	ctx := context.Background()
	muse := m.Client(t, "muse")
	instinct := m.Client(t, "instinct")

	req, err := muse.Send(ctx, "instinct", "check the deployment", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instinct.Poll(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := instinct.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := instinct.Reply(ctx, req.ID, "which environment?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Answer(ctx, req.ID, "staging"); err != nil {
		t.Fatal(err)
	}
	if _, err := instinct.Poll(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := instinct.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := instinct.Reply(ctx, req.ID, "staging looks good", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}

	for name, s := range map[string]*spyEvents{"a": a, "b": b} {
		s.mu.Lock()
		if len(s.queued) != 2 {
			t.Errorf("%s queued = %v, want 2 (initial ask + answer requeue)", name, s.queued)
		}
		if len(s.needsInput) != 1 || s.needsInput[0] != req.ID {
			t.Errorf("%s needsInput = %v, want [%s]", name, s.needsInput, req.ID)
		}
		if len(s.replied) != 2 {
			t.Errorf("%s replied = %v, want 2 (needs_input + answered)", name, s.replied)
		}
		s.mu.Unlock()
	}
}

func TestWithMCPEventsWakeAndAdminEndpoint(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: 2 * time.Second})
	waker := wake.New(wake.Config{
		"grokbot": {Method: wake.Webhook, URL: "https://example.invalid/hook"},
		"muse":    {Method: wake.None},
	}, m.Store, wake.Options{})
	defer waker.Stop()

	subscribed := map[string]bool{"muse": true, "grokbot": true}
	m.Server.SetWakeNamer(relay.WithMCPEventsWake(waker, func(agent string) bool { return subscribed[agent] }))
	m.Server.SetGatewayEvents(func(context.Context) (any, error) {
		return []map[string]any{{"agent": "muse", "event_name": "inbox.request"}}, nil
	})

	ctx := context.Background()
	agents, err := m.Client(t, "admin").Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, a := range agents {
		byName[a.Name] = a.Wake
	}
	if byName["muse"] != relay.WakeMCPEvents {
		t.Fatalf("muse wake = %q, want %q", byName["muse"], relay.WakeMCPEvents)
	}
	// Explicit non-none wake.json override wins over mcp-events.
	if byName["grokbot"] != wake.Webhook {
		t.Fatalf("grokbot wake = %q, want %q", byName["grokbot"], wake.Webhook)
	}

	// Unsubscribing reverts muse to wake=none.
	subscribed["muse"] = false
	agents, err = m.Client(t, "admin").Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Name == "muse" && a.Wake != wake.None {
			t.Fatalf("muse wake after unsubscribe = %q, want none", a.Wake)
		}
	}

	// Admin endpoint works for admin and rejects non-admin.
	var out struct {
		Subscriptions []map[string]any `json:"subscriptions"`
	}
	if err := m.Client(t, "admin").Raw(ctx, "GET", "/v1/admin/gateway/events", nil, &out); err != nil {
		t.Fatalf("admin gateway events: %v", err)
	}
	if len(out.Subscriptions) != 1 {
		t.Fatalf("subscriptions = %+v, want 1", out.Subscriptions)
	}
	if err := m.Client(t, "muse").Raw(ctx, "GET", "/v1/admin/gateway/events", nil, &out); err == nil {
		t.Fatal("non-admin should be refused GET /v1/admin/gateway/events")
	}
}
