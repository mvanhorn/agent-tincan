package cli

import (
	"context"
	"fmt"
	"strings"
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
	return &cobra.Command{
		Use:   "claude-code",
		Short: "Tell a Claude Code session when teammate requests are waiting (SessionStart and UserPromptSubmit hooks)",
		Long: `Tell a Claude Code session when teammate requests or replies are waiting.

Run it from Claude Code's SessionStart and UserPromptSubmit hooks. It looks at
the relay once, without taking anything, and when items are waiting prints one
line, which Claude Code adds to the session's context:

  Agent Tincan: 2 teammate requests waiting (oldest 12m). Call check_inbox before anything else.

With nothing waiting it prints nothing. It never blocks the prompt: it gives up
after 3 seconds, and on any error it writes to stderr only and exits 0.

This reaches sessions a channel notice cannot: a session that was busy when
the notice came, one started without channels, and SDK or desktop sessions.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), hookTimeout)
			defer cancel()
			line, err := claudeCodeHookLine(ctx, hookRelay, time.Now())
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "tincan hook claude-code: %v\n", client.RejoinHint(err, ""))
				return nil
			}
			if line != "" {
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
			return nil
		},
	}
}

// peeker is the one relay call the hook makes.
type peeker interface {
	Peek(ctx context.Context, hold time.Duration) (client.Waiting, error)
}

// hookRelay is the relay client for a hook: the saved config with this
// session's project, and no relay-info refresh, which could outlast the
// hook's timeout.
var hookRelay = func() (peeker, error) {
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
func claudeCodeHookLine(ctx context.Context, open func() (peeker, error), now time.Time) (string, error) {
	r, err := open()
	if err != nil {
		return "", err
	}
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

// countNoun is "1 teammate request" or "2 teammate requests".
func countNoun(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
