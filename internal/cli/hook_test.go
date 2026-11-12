package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// openPeeker hands claudeCodeHookLine an existing client.
func openPeeker(p peeker) func() (peeker, error) {
	return func() (peeker, error) { return p, nil }
}

// With nothing waiting the hook prints nothing; with teammate requests
// waiting it prints one line naming the count and the oldest's age, and
// tells the model to check its inbox first. It never claims: the requests
// are still there for check_inbox.
func TestClaudeCodeHookLine(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := t.Context()
	muse := m.Client(t, "muse")
	line, err := claudeCodeHookLine(ctx, openPeeker(muse), time.Now())
	if err != nil || line != "" {
		t.Fatalf("empty inbox: line %q, err %v", line, err)
	}

	for _, body := range []string{"first", "second"} {
		if _, err := m.Client(t, "grokbot").Send(ctx, "muse", body, envelope.KindAsk, "", false); err != nil {
			t.Fatal(err)
		}
	}
	line, err = claudeCodeHookLine(ctx, openPeeker(muse), time.Now().Add(12*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Agent Tincan: 2 teammate requests waiting (oldest 12m). Call check_inbox before anything else."; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	in, err := muse.Poll(ctx, 0)
	if err != nil || len(in.Requests) != 2 {
		t.Fatalf("after the hook, poll = %+v, %v; want both requests still queued", in, err)
	}
}

type fakePeek struct {
	w   client.Waiting
	err error
}

func (f fakePeek) Peek(context.Context, time.Duration) (client.Waiting, error) { return f.w, f.err }

// Pings are not the model's; replies are named apart from requests; a relay
// that sends no created_at gets no age.
func TestClaudeCodeHookLineCounts(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		w    client.Waiting
		want string
	}{
		{"only pings", client.Waiting{Total: 1, Queued: 1, Pings: 1, Pending: []envelope.Pending{{ID: "p", Kind: envelope.KindPing, CreatedAt: now}}}, ""},
		{"one request", client.Waiting{Total: 1, Queued: 1, Pending: []envelope.Pending{{ID: "a", Kind: envelope.KindAsk, CreatedAt: now.Add(-3 * time.Minute)}}},
			"Agent Tincan: 1 teammate request waiting (oldest 3m). Call check_inbox before anything else."},
		{"older relay", client.Waiting{Total: 1, Queued: 1}, "Agent Tincan: 1 teammate request waiting. Call check_inbox before anything else."},
		{"a reply", client.Waiting{Total: 1, Replies: []client.Result{{}}}, "Agent Tincan: 1 reply to your requests waiting. Call check_inbox before anything else."},
		{"both", client.Waiting{Total: 4, Queued: 1, Replies: []client.Result{{}, {}}, Pending: []envelope.Pending{{ID: "a", Kind: envelope.KindAsk, CreatedAt: now.Add(-time.Hour)}}},
			"Agent Tincan: 1 teammate request and 3 replies to your requests waiting (oldest 1h). Call check_inbox before anything else."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := claudeCodeHookLine(t.Context(), openPeeker(fakePeek{w: tc.w}), now)
			if err != nil || got != tc.want {
				t.Fatalf("line = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// A relay that errors, or no config at all, leaves stdout empty, says why on
// stderr, and exits 0, so the user's prompt goes through.
func TestClaudeCodeHookNeverFailsThePrompt(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func() (peeker, error)
	}{
		{"relay error", openPeeker(fakePeek{err: errors.New("relay unreachable")})},
		{"no config", func() (peeker, error) { return nil, errors.New("no relay configured") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := hookRelay
			hookRelay = tc.open
			t.Cleanup(func() { hookRelay = saved })
			var stdout, stderr bytes.Buffer
			cmd := Root()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"hook", "claude-code"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("hook returned %v, want nil", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "tincan hook claude-code:") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

// A relay that never answers is given up on within the hook's timeout.
func TestClaudeCodeHookTimesOut(t *testing.T) {
	saved := hookRelay
	hookRelay = openPeeker(blockingPeek{})
	t.Cleanup(func() { hookRelay = saved })
	var stdout, stderr bytes.Buffer
	cmd := Root()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"hook", "claude-code"})
	start := time.Now()
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hook returned %v", err)
	}
	if d := time.Since(start); d > hookTimeout+time.Second {
		t.Fatalf("hook took %v", d)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "deadline") {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

type blockingPeek struct{}

func (blockingPeek) Peek(ctx context.Context, _ time.Duration) (client.Waiting, error) {
	<-ctx.Done()
	return client.Waiting{}, ctx.Err()
}
