package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func openHookClient(r hookClient) func() (hookClient, error) {
	return func() (hookClient, error) { return r, nil }
}

// With nothing waiting the hook prints nothing; with teammate requests
// waiting it prints one line naming the count and the oldest's age, and
// tells the model to check its inbox first. It never claims: the requests
// are still there for check_inbox.
func TestClaudeCodeHookLine(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := t.Context()
	muse := m.Client(t, "muse")
	line, err := claudeCodeHookLine(ctx, muse, time.Now())
	if err != nil || line != "" {
		t.Fatalf("empty inbox: line %q, err %v", line, err)
	}

	for _, body := range []string{"first", "second"} {
		if _, err := m.Client(t, "grokbot").Send(ctx, "muse", body, envelope.KindAsk, "", false); err != nil {
			t.Fatal(err)
		}
	}
	line, err = claudeCodeHookLine(ctx, muse, time.Now().Add(12*time.Minute))
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
	client.Rawer
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
			got, err := claudeCodeHookLine(t.Context(), fakePeek{w: tc.w}, now)
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
		open func() (hookClient, error)
	}{
		{"relay error", openHookClient(fakePeek{err: errors.New("relay unreachable")})},
		{"no config", func() (hookClient, error) { return nil, errors.New("no relay configured") }},
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
	hookRelay = openHookClient(blockingPeek{})
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

type blockingPeek struct{ client.Rawer }

func (blockingPeek) Peek(ctx context.Context, _ time.Duration) (client.Waiting, error) {
	<-ctx.Done()
	return client.Waiting{}, ctx.Err()
}

func TestClaudeCodeStartupHookReadsVisibleTasks(t *testing.T) {
	// The held-task case loads approval.json, which the relay refuses unless
	// it has mode 0600, and Windows keeps no such bits.
	skipOnWindows(t, skipRelayHost)
	m := testrelay.New(t, relay.Config{})
	ctx := t.Context()
	lead := m.JoinOnMachineOf(t, "admin", "lead")
	lead.SetSession("previous")
	admin := m.Client(t, "admin")
	if _, err := admin.SetTaskHost(ctx, "lead", true); err != nil {
		t.Fatal(err)
	}
	worker := m.Client(t, "muse")
	for _, status := range []envelope.Status{envelope.StatusNeedsInput, envelope.StatusAnswered, envelope.StatusQueued} {
		result, err := client.AddTask(ctx, lead, false, client.TaskAdd{Title: "private title", Assignee: "muse"})
		if err != nil || result.Task.Status != envelope.TaskWorking {
			t.Fatalf("add = %+v, %v", result, err)
		}
		if status == envelope.StatusQueued {
			continue
		}
		if _, err := worker.Claim(ctx, result.Task.CurrentRequest); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.Reply(ctx, result.Task.CurrentRequest, "private reply", status); err != nil {
			t.Fatal(err)
		}
	}
	in, err := lead.Poll(ctx, 0)
	if err != nil || len(in.Replies) != 2 {
		t.Fatalf("previous session poll = %+v, %v", in, err)
	}
	if err := lead.AckReplies(ctx, nil, in.ReplyAcks()...); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= relay.MaxTaskPage; i++ {
		if _, _, err := m.Store.AddTask(ctx, envelope.Task{Title: "shared title", Filer: envelope.TaskOwner}); err != nil {
			t.Fatal(err)
		}
	}
	private, err := client.AddTask(ctx, worker, false, client.TaskAdd{Title: "other agent's task", Assignee: "instinct"})
	if err != nil || private.Task.Status != envelope.TaskWorking {
		t.Fatalf("private task = %+v, %v", private, err)
	}
	proposed, err := client.AddTask(ctx, lead, false, client.TaskAdd{Title: "proposal", Source: "mail"})
	if err != nil || proposed.Task.Status != envelope.TaskProposed {
		t.Fatalf("proposed task = %+v, %v", proposed, err)
	}
	approvalPath := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(approvalPath, []byte(`{"gate":{"instinct":{"from":"*"}},"hold_ttl":"1h"}`), 0600); err != nil {
		t.Fatal(err)
	}
	approval, err := policy.LoadApproval(approvalPath)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: approval}))
	held, err := client.AddTask(ctx, lead, false, client.TaskAdd{Title: "waiting for approval", Assignee: "instinct"})
	if err != nil || held.Task.AskStatus != envelope.StatusHeld {
		t.Fatalf("held task = %+v, %v", held, err)
	}
	saved := hookRelay
	hookRelay = openHookClient(m.Client(t, "lead"))
	t.Cleanup(func() { hookRelay = saved })
	out, err := run(t, claudeCodeHookCmd(), "--tasks")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("visible tasks: question 1 (#1); returned 1 (#2); open %d (#4, #5, #6, #7, #8) +%d more; working 1 (#3)", relay.MaxTaskPage+1, relay.MaxTaskPage-4),
		"tincan tasks show <id>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hook output %q lacks %q", out, want)
		}
	}
	for _, unwanted := range []string{"waiting", "private", "shared title", fmt.Sprintf("#%d", private.Task.ID), fmt.Sprintf("#%d", proposed.Task.ID), fmt.Sprintf("#%d", held.Task.ID)} {
		if strings.Contains(out, unwanted) {
			t.Errorf("hook output %q contains %q", out, unwanted)
		}
	}
}

// The unflagged prompt hook must never call Raw. With tasks enabled the
// old-relay capability response still leaves the inbox peek usable.
func TestClaudeCodeHookTasksCanFailIndependently(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	muse := m.Client(t, "muse")
	if _, err := m.Client(t, "grokbot").Send(t.Context(), "muse", "waiting", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	saved := hookRelay
	t.Cleanup(func() { hookRelay = saved })
	hookRelay = openHookClient(fakePeek{w: client.Waiting{Queued: 1, Total: 1}})
	if out, err := run(t, claudeCodeHookCmd()); err != nil || !strings.Contains(out, "1 teammate request waiting") || strings.Contains(out, "visible tasks") {
		t.Fatalf("prompt hook = %q, %v", out, err)
	}
	hookRelay = openHookClient(fakePeek{w: client.Waiting{Queued: 1, Total: 1}, Rawer: noTaskCapabilities{}})
	if out, err := run(t, claudeCodeHookCmd(), "--tasks"); err != nil || !strings.Contains(out, "1 teammate request waiting") || !strings.Contains(out, hookTasksUnavailable) {
		t.Fatalf("startup hook with old relay = %q, %v", out, err)
	}
	if _, err := client.AddTask(t.Context(), muse, false, client.TaskAdd{Title: "remember me"}); err != nil {
		t.Fatal(err)
	}
	hookRelay = openHookClient(fakePeek{err: errors.New("peek failed"), Rawer: muse})
	if out, err := run(t, claudeCodeHookCmd(), "--tasks"); err != nil || !strings.Contains(out, "visible tasks: open 1") || !strings.Contains(out, "peek failed") {
		t.Fatalf("startup hook with failed peek = %q, %v", out, err)
	}
}

type noTaskCapabilities struct{}

func (noTaskCapabilities) Raw(_ context.Context, method, path string, _, _ any) error {
	if method != "GET" || path != "/v1/capabilities" {
		return fmt.Errorf("unexpected request %s %s", method, path)
	}
	return nil
}
