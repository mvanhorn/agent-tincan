package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// hookTimeout bounds a host hook's whole run, so a slow or unreachable
// relay never holds up the user's prompt.
const hookTimeout = 3 * time.Second

func hookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Commands for an agent host's hooks",
	}
	cmd.AddCommand(claudeCodeHookCmd())
	return cmd
}

func claudeCodeHookCmd() *cobra.Command {
	var tasks bool
	cmd := &cobra.Command{
		Use:   "claude-code",
		Short: "Tell a Claude Code session when teammate requests are waiting (SessionStart and UserPromptSubmit hooks)",
		Long: `Tell a Claude Code session when teammate requests or replies are waiting.

Run it from Claude Code's SessionStart and UserPromptSubmit hooks. It looks at
the relay once, without taking anything, and when items are waiting prints one
line, which Claude Code adds to the session's context:

  Agent Tincan: 2 teammate requests waiting (oldest 12m). Call check_inbox before anything else.

Add --tasks to SessionStart only to also show counts and IDs of visible
question, returned, open and working tasks. The hook shows the ordinary
agent view and shares the same 3-second deadline between both reads.

With nothing waiting or visible it prints nothing. It exits 0 on read
errors, with inbox errors on stderr and an unavailable task summary on stdout.

This reaches sessions a channel notice cannot: a session that was busy when
the notice came, one started without channels, and SDK or desktop sessions.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), hookTimeout)
			defer cancel()
			r, err := hookRelay()
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "tincan hook claude-code: %v\n", client.RejoinHint(err, ""))
				if tasks {
					fmt.Fprintln(cmd.OutOrStdout(), hookTasksUnavailable)
				}
				return nil
			}
			var group sync.WaitGroup
			var summary string
			if tasks {
				group.Go(func() { summary = hookTaskSummary(ctx, r) })
			}
			line, err := claudeCodeHookLine(ctx, r, time.Now())
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "tincan hook claude-code: %v\n", client.RejoinHint(err, ""))
			} else if line != "" {
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
			group.Wait()
			if summary != "" {
				fmt.Fprintln(cmd.OutOrStdout(), summary)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&tasks, "tasks", false, "also summarize visible tasks (SessionStart only)")
	return cmd
}

type hookClient interface {
	client.Rawer
	Peek(ctx context.Context, hold time.Duration) (client.Waiting, error)
}

// hookRelay is the relay client for a hook: the saved config with this
// session's project, and no relay-info refresh, which could outlast the
// hook's timeout.
var hookRelay = func() (hookClient, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Relay == "" {
		return nil, fmt.Errorf("no relay configured")
	}
	r, err := client.NewRelayFor(cfg)
	if err != nil {
		return nil, err
	}
	if label, err := sessionProject("", false); err == nil {
		r.SetProject(label)
	}
	return r, nil
}

// claudeCodeHookLine peeks at the relay once, with no hold, and returns the
// line for the model, or "" when nothing waits. Pings are left out: they
// are answered without the model.
func claudeCodeHookLine(ctx context.Context, r hookClient, now time.Time) (string, error) {
	w, err := r.Peek(ctx, 0)
	if err != nil {
		return "", err
	}
	requests := max(w.Queued-w.Pings, 0)
	var oldest time.Time
	for _, p := range w.Pending {
		if p.Kind == envelope.KindPing || p.CreatedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || p.CreatedAt.Before(oldest) {
			oldest = p.CreatedAt
		}
	}
	// waiting also counts replies past the ones the peek lists.
	replies := max(len(w.Replies), w.Total-w.Queued)
	if requests == 0 && replies == 0 {
		return "", nil
	}
	var parts []string
	if requests > 0 {
		parts = append(parts, countNoun(requests, "teammate request", "teammate requests"))
	}
	if replies > 0 {
		parts = append(parts, countNoun(replies, "reply to your requests", "replies to your requests"))
	}
	what := strings.Join(parts, " and ")
	line := "Agent Tincan: " + what + " waiting"
	if requests > 0 && !oldest.IsZero() {
		line += " (oldest " + client.QueueAge(now.Sub(oldest)) + ")"
	}
	return line + ". Call check_inbox before anything else.", nil
}

const hookTasksUnavailable = "Agent Tincan: visible tasks could not load. Try list_tasks or tincan tasks."

func hookTaskSummary(ctx context.Context, r client.Rawer) string {
	tasks, _, err := client.Tasks(ctx, r, false, false)
	if err != nil {
		return hookTasksUnavailable
	}
	const maxIDs = 5
	var groups []string
	for _, status := range []envelope.TaskStatus{envelope.TaskQuestion, envelope.TaskReturned, envelope.TaskOpen, envelope.TaskWorking} {
		var ids []string
		count := 0
		for _, task := range tasks {
			if task.Status != status || task.Withheld || task.AskStatus == envelope.StatusHeld {
				continue
			}
			count++
			if len(ids) < maxIDs {
				ids = append(ids, fmt.Sprintf("#%d", task.ID))
			}
		}
		if count == 0 {
			continue
		}
		line := fmt.Sprintf("%s %d (%s)", status, count, strings.Join(ids, ", "))
		if count > len(ids) {
			line += fmt.Sprintf(" +%d more", count-len(ids))
		}
		groups = append(groups, line)
	}
	if len(groups) == 0 {
		return ""
	}
	return "Agent Tincan: visible tasks: " + strings.Join(groups, "; ") +
		". Use list_tasks or tincan tasks to list them, or get_task or tincan tasks show <id> for details."
}

// countNoun is "1 teammate request" or "2 teammate requests".
func countNoun(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
