package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func listenCmd() *cobra.Command {
	var execCmd string
	var once bool
	cmd := &cobra.Command{
		Use:   "listen --exec <command>",
		Short: "Stay connected and run a command whenever teammates' requests or replies are waiting",
		Long: `Hold a long-poll to the relay and run --exec when requests, or replies to
this agent's own requests, are waiting. Nothing is taken, so the agent picks
them up itself with check_inbox or "tincan inbox". The command gets
TINCAN_WAITING (requests plus unseen replies) in its environment and runs
through "sh -c".

Use this for agents whose runtime stays up and can be nudged by a command,
for example opening a Claude Code session in cmux. For agents that get a new
turn when a background command exits (Muse), use "tincan wait" instead.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if execCmd == "" {
				return errors.New("--exec is required")
			}
			r, _, err := connect()
			if err != nil {
				return err
			}
			return listen(cmd.Context(), r, execCmd, once)
		},
	}
	cmd.Flags().StringVar(&execCmd, "exec", "", "command to run when requests or replies are waiting")
	cmd.Flags().BoolVar(&once, "once", false, "exit after the first nudge")
	return cmd
}

func listen(ctx context.Context, r *client.Relay, execCmd string, once bool) error {
	backoff := time.Second
	for {
		w, err := r.Peek(ctx, client.DefaultPollHold)
		n := w.Total
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case client.IsStatus(err, 403):
			return err
		case err != nil:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jitter(backoff)):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if n == 0 {
			continue
		}
		c := exec.CommandContext(ctx, "sh", "-c", execCmd)
		c.Env = append(os.Environ(), "TINCAN_WAITING="+strconv.Itoa(n))
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			// A failed nudge is retried on the next loop; nothing was taken.
			os.Stderr.WriteString("tincan listen: command failed: " + err.Error() + "\n")
		}
		if once {
			return nil
		}
		// Give the agent time to pick them up before nudging again.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}
