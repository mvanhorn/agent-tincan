package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func listenCmd() *cobra.Command {
	var execCmd string
	var once, perRequest bool
	var worktree string
	var maxWorkers int
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "listen --exec <command>",
		Short: "Stay connected and run a command whenever teammates' requests or replies are waiting",
		Long: `Hold a long-poll to the relay and run --exec when requests, or replies to
this agent's own requests, are waiting. Nothing is taken, so the agent picks
them up itself with check_inbox or "tincan inbox". The command gets
TINCAN_WAITING (requests plus unseen replies) in its environment and runs
through "sh -c", so listen refuses on Windows. A new relay release also nudges the command, with its version
in TINCAN_UPGRADE_AVAILABLE (empty on other nudges), even if TINCAN_WAITING is zero.

Use this for agents whose runtime stays up and can be nudged by a command,
for example opening a Claude Code session in cmux. For agents that get a new
turn when a background command exits (Muse), use "tincan wait" instead.

With --per-request the listener takes the requests itself: it starts one fresh
process through "sh -c" for each ask, unattended, as this agent. The process
gets the request body on stdin, and TINCAN_REQUEST_ID, TINCAN_FROM and its own
TINCAN_SESSION in its environment, with TINCAN_FROM_GUEST=1 on a request a
guest's ask led to, and replies with "tincan reply". One that exits without
replying fails the request with its exit code. Pings are answered
as usual, and notifies and replies stay for the agent's inbox. --worktree runs
each process in its own new git worktree of a repository, removed afterwards
unless the process left changes or commits in it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := needsShell(runtime.GOOS, "tincan listen"); err != nil {
				return err
			}
			if execCmd == "" {
				return errors.New("--exec is required")
			}
			if !perRequest {
				for _, name := range []string{"worktree", "max-workers", "timeout"} {
					if cmd.Flags().Changed(name) {
						return fmt.Errorf("--%s needs --per-request", name)
					}
				}
				r, _, err := connect()
				if err != nil {
					return err
				}
				return listen(cmd.Context(), r, execCmd, once)
			}
			if err := checkWorkerPlatform(); err != nil {
				return err
			}
			if once {
				return errors.New("--once does not apply to --per-request")
			}
			if maxWorkers < 1 {
				return errors.New("--max-workers must be at least 1")
			}
			if timeout <= 0 {
				return errors.New("--timeout must be longer than zero")
			}
			o := workerOptions{exec: execCmd, maxWorkers: maxWorkers, timeout: timeout, newClient: workerClient}
			if worktree != "" {
				var err error
				if o.repo, err = checkWorktreeRepo(cmd.Context(), worktree); err != nil {
					return err
				}
			}
			r, cfg, err := connect()
			if err != nil {
				return err
			}
			if o.config, o.worktrees, err = workerPaths(cfg.Agent); err != nil {
				return err
			}
			// Workers run in process groups of their own, out of reach of a
			// signal to this one, so a signal has to stop them through ctx.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return listenPerRequest(ctx, r, o)
		},
	}
	cmd.Flags().StringVar(&execCmd, "exec", "", "command to run when requests or replies are waiting")
	cmd.Flags().BoolVar(&once, "once", false, "exit after the first nudge")
	cmd.Flags().BoolVar(&perRequest, "per-request", false, "take each ask and run --exec once for it, as a fresh unattended process")
	cmd.Flags().StringVar(&worktree, "worktree", "", "with --per-request, run each process in a new git worktree of this repository")
	cmd.Flags().IntVar(&maxWorkers, "max-workers", 2, "with --per-request, how many processes run at once")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "with --per-request, how long a process may run before it and its children are killed")
	return cmd
}

// needsShell refuses a command that runs its work through sh -c on a platform
// that has no sh.
func needsShell(goos, command string) error {
	if goos == "windows" {
		return fmt.Errorf("%s runs commands with sh -c, so it does not work on Windows; use tincan wait or the MCP tools there", command)
	}
	return nil
}

// workerPaths returns the absolute config path workers are given, and where
// agent's worktrees go, beside it. TINCAN_CONFIG may be relative, and neither
// git run from the repository nor a worker in a worktree shares this
// directory.
func workerPaths(agent string) (config, worktrees string, err error) {
	config, err = filepath.Abs(client.ConfigPath())
	if err != nil {
		return "", "", err
	}
	return config, filepath.Join(filepath.Dir(config), "worktrees", agentFileName(agent)), nil
}

// workerClient is a client of the saved config in session. It reads the
// config each time, so a worker follows a relay that moved under this
// listener.
func workerClient(session string) (*client.Relay, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, err
	}
	r, err := client.NewRelayFor(cfg)
	if err == nil {
		r.SetSession(session)
	}
	return r, err
}

// listen's timing and log. Tests override them.
var (
	// listenPresenceEvery is how often listen refreshes the agent's relay
	// presence while its command runs and during the cooldown after it.
	listenPresenceEvery = client.DefaultPresenceInterval
	// listenPresenceCap bounds that refresh for one nudge, so an agent
	// whose command hangs falls offline again.
	listenPresenceCap = 30 * time.Minute
	// listenCooldown is the wait after a nudge before listen looks again.
	listenCooldown = 30 * time.Second
	// listenLog receives listen's own log lines.
	listenLog io.Writer = os.Stderr
)

func listen(ctx context.Context, r *client.Relay, execCmd string, once bool) error {
	backoff := time.Second
	// Failed pongs retry in the background while the loop keeps peeking; the
	// listener waits for them before it returns, --once included.
	var retries client.PongRetries
	defer retries.Wait()
	for {
		w, err := r.Peek(ctx, client.DefaultPollHold)
		n := w.Total - w.Pings
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case client.IsStatus(err, 403), client.IsProxyAuth(err):
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
		if answerPendingPings(ctx, r, w.Pending, &retries) && n <= 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jitter(backoff)):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		nudged := false
		err = client.ReportUpgradeOn(client.UpgradeSurfaceListen, w.UpgradeAvailable, func(string) error {
			nudged = true
			return nudge(ctx, r, execCmd, n, once, w.UpgradeAvailable)
		})
		if !nudged && n > 0 {
			nudged = true
			err = nudge(ctx, r, execCmd, n, once, "")
		}
		if err != nil {
			if once || ctx.Err() != nil {
				return err
			}
			continue
		}
		if !nudged {
			continue
		}
		if once {
			return nil
		}
	}
}

// answerPendingPings answers the pings in a peek's pending list and reports
// whether any failed. Failed pongs retry on retries.
func answerPendingPings(ctx context.Context, r *client.Relay, pending []envelope.Pending, retries *client.PongRetries) (failed bool) {
	for _, p := range pending {
		if p.Kind != envelope.KindPing {
			continue
		}
		_, retry, err := client.AnswerPings(ctx, r, client.Inbox{Requests: []envelope.Request{{ID: p.ID, Kind: envelope.KindPing}}}, "listen")
		if err != nil {
			failed = true
		}
		retries.Go(ctx, retry)
	}
	return failed
}

// nudge runs the command for n waiting items and then, unless once, waits
// out the cooldown. The agent stays online throughout, up to
// listenPresenceCap: it is busy with what it was nudged about, not gone.
func nudge(ctx context.Context, r *client.Relay, execCmd string, n int, once bool, upgrade string) error {
	defer r.KeepPresence(ctx, client.Presence{
		Every: listenPresenceEvery,
		Cap:   listenPresenceCap,
		Logf:  listenLogf,
	})()
	c := exec.CommandContext(ctx, "sh", "-c", execCmd)
	c.Env = append(os.Environ(), "TINCAN_WAITING="+strconv.Itoa(n), "TINCAN_UPGRADE_AVAILABLE="+upgrade)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	err := c.Run()
	if err != nil {
		// A failed nudge is retried on the next loop; nothing was taken.
		listenLogf("command failed: %v", err)
	}
	if once {
		return err
	}
	// Give the agent time to pick them up before nudging again.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(listenCooldown):
		return err
	}
}

func listenLogf(format string, args ...any) {
	_, _ = fmt.Fprintf(listenLog, "tincan listen: "+format+"\n", args...)
}
