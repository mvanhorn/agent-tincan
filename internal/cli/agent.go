package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

func connect() (*client.Relay, client.Config, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, cfg, err
	}
	if cfg.Relay == "" {
		return nil, cfg, errors.New("no relay configured: run `tincan join <code> --relay http://tincan-relay` or set TINCAN_RELAY")
	}
	r, err := client.NewRelayFor(cfg)
	return r, cfg, err
}

func agentCmds() []*cobra.Command {
	return []*cobra.Command{joinCmd(), inviteCmd(), kindCmd(), removeCmd(), agentsCmd(), askCmd(), getCmd(), inboxCmd(), replyCmd(), cancelCmd(), waitCmd()}
}

func joinCmd() *cobra.Command {
	var relayURL, proxy string
	var replace bool
	cmd := &cobra.Command{
		Use:   "join <code>",
		Short: "Join this machine to the mesh with an invite code",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := client.LoadConfig()
			if relayURL != "" {
				cfg.Relay = relayURL
			}
			if cmd.Flags().Changed("proxy") {
				cfg.Proxy = proxy
			}
			if cfg.Relay == "" {
				return errors.New("--relay is required the first time (for example http://tincan-relay)")
			}
			r, err := client.NewRelay(cfg.Relay, cfg.Proxy)
			if err != nil {
				return err
			}
			name, err := r.Join(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			// The invite code does not say which agent it joins, so the check
			// comes after the relay answers. Refusing before saving keeps the
			// agent already using this config from acting as the new one.
			if cfg.Agent != "" && cfg.Agent != name && !replace {
				return fmt.Errorf("%s already belongs to agent %q, so joining as %q here would make %q act as %q. "+
					"For a second agent on this machine, set TINCAN_CONFIG to a new file and save it there without a new invite: "+
					"TINCAN_CONFIG=<new file> tincan rejoin --relay %s --name %s. "+
					"To repoint this config to %q instead, run tincan join again with --replace",
					client.ConfigPath(), cfg.Agent, name, cfg.Agent, name, cfg.Relay, name, name)
			}
			cfg.Agent = name
			if err := client.SaveConfig(cfg); err != nil {
				return err
			}
			cmd.Printf("Joined as %q. Config saved to %s\n", name, client.ConfigPath())
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL, e.g. http://tincan-relay")
	cmd.Flags().StringVar(&proxy, "proxy", "", "proxy for relay traffic (for sandboxes whose default proxy cannot reach the tailnet)")
	cmd.Flags().BoolVar(&replace, "replace", false, "repoint a config that already names a different agent (use TINCAN_CONFIG=<new file> for a second agent instead)")
	return cmd
}

func inviteCmd() *cobra.Command {
	var relayURL, socket, kind string
	cmd := &cobra.Command{
		Use:   "invite <name>",
		Short: "Create a one-time code that joins a machine as <name> (admin devices only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			code, err := r.InviteKind(cmd.Context(), args[0], kind)
			if err != nil {
				return err
			}
			valid := "valid 10 minutes"
			if kind != "" {
				valid = "kind " + kind + ", " + valid
			}
			cmd.Printf("Invite code for %q (%s): %s\nOn that machine run:\n  tincan join %s --relay <relay URL>\n"+
				"(for a second agent on a machine that already runs one, prefix with TINCAN_CONFIG=<new file>)\n", args[0], valid, code, code)
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&kind, "kind", "", "the agent's runtime (hermes, codex, ...), recorded on join so tincan onboard tailors its block")
	return cmd
}

func removeCmd() *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an agent from the mesh immediately (admin devices only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			if err := r.Remove(cmd.Context(), args[0]); err != nil {
				return err
			}
			cmd.Printf("Removed %q.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	return cmd
}

func relayFor(override string) (*client.Relay, client.Config, error) {
	if override == "" {
		return connect()
	}
	cfg, _ := client.LoadConfig()
	cfg.Relay = override
	r, err := client.NewRelayFor(cfg)
	return r, cfg, err
}

func agentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "agents",
		Short: "List agents in the mesh, whether they are online, how they wake, and when each last called the relay",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			agents, err := r.Agents(cmd.Context())
			if err != nil {
				return err
			}
			cmd.Print(formatAgents(agents, time.Now()))
			return nil
		},
	}
}

func formatAgents(agents []client.AgentInfo, now time.Time) string {
	var b strings.Builder
	for _, a := range agents {
		fmt.Fprintf(&b, "%-14s %-8s wake=%s %s", a.Name, a.State(), a.Wake, a.LastSeen(now))
		if a.Kind != "" {
			fmt.Fprintf(&b, " kind=%s", a.Kind)
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "No agents have joined yet.\n"
	}
	return b.String()
}

func askCmd() *cobra.Command {
	var wait time.Duration
	var parent string
	var notify bool
	cmd := &cobra.Command{
		Use:   "ask <agent> <message...>",
		Short: "Ask another agent to do something and wait briefly for the reply",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			body := strings.Join(args[1:], " ")
			if notify {
				req, err := r.Send(cmd.Context(), args[0], body, envelope.KindNotify, parent)
				if err != nil {
					return err
				}
				cmd.Printf("Sent to %s (request %s).\n", args[0], req.ID)
				return nil
			}
			res, err := r.Ask(cmd.Context(), args[0], body, parent, client.ClampWait(wait))
			if err != nil {
				return err
			}
			cmd.Print(client.FormatResult(res))
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", client.MaxInlineWait, "how long to wait for the reply (max 20s)")
	cmd.Flags().StringVar(&parent, "parent", "", "the request you are handling, if this continues it (usually automatic)")
	cmd.Flags().BoolVar(&notify, "notify", false, "send without waiting for a reply")
	return cmd
}

func getCmd() *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "get <request-id>",
		Short: "Check on a request you sent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			res, err := r.Get(cmd.Context(), args[0], client.ClampWait(wait))
			if err != nil {
				return err
			}
			cmd.Print(client.FormatResult(res))
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 0, "wait up to this long for a reply (max 20s)")
	return cmd
}

func inboxCmd() *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Get requests from teammates (claims them) and replies to your own requests",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			return checkInbox(cmd.Context(), r, client.ClampWait(wait), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 0, "wait up to this long for a request (max 20s)")
	return cmd
}

// checkInbox polls once, claims what arrived, prints it to out, and only
// then acknowledges the replies it printed, so a reply lost on the way (a
// dropped connection, a crash before printing) shows again next time. A
// failed ack is reported on errOut; the replies may show again.
func checkInbox(ctx context.Context, r *client.Relay, wait time.Duration, out, errOut io.Writer) error {
	in, err := r.Poll(ctx, wait)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(out, client.FormatInbox(ctx, r, in)); err != nil {
		return err
	}
	if err := r.AckReplies(ctx, in.ReplyIDs()); err != nil {
		fmt.Fprintf(errOut, "tincan inbox: could not mark replies read (they may show again): %v\n", err)
	}
	return nil
}

// formatWait renders what ended a wait: the requests, claimed, and a count of
// replies to the agent's own requests, which stay unseen for check_inbox.
func formatWait(ctx context.Context, r *client.Relay, in client.Inbox) string {
	var b strings.Builder
	if len(in.Requests) > 0 {
		b.WriteString(client.FormatInbox(ctx, r, client.Inbox{Requests: in.Requests}))
	}
	if len(in.Replies) > 0 {
		b.WriteString(wake.WaitingMessage(0, len(in.Replies)) + "\n")
	}
	return b.String()
}

func replyCmd() *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:   "reply <request-id> <message...>",
		Short: "Answer a request from a teammate",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			rep, err := r.Reply(cmd.Context(), args[0], strings.Join(args[1:], " "), envelope.Status(status))
			if err != nil {
				return err
			}
			cmd.Printf("Replied to %s (%s).\n", args[0], rep.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "answered", "answered, failed, or declined")
	return cmd
}

func cancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <request-id>",
		Short: "Withdraw a request you sent that nobody has picked up yet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			if err := r.Cancel(cmd.Context(), args[0]); err != nil {
				return err
			}
			cmd.Printf("Cancelled %s.\n", args[0])
			return nil
		},
	}
}

func waitCmd() *cobra.Command {
	var limit time.Duration
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until a teammate's request or a reply to your own request arrives, print it, and exit",
		Long: `Block until a request arrives, claim and print it, then exit. A reply to
one of your own requests also ends the wait: it prints a count and leaves the
reply for check_inbox (or "tincan inbox") to show.

For agents that get a new turn when a background command finishes (like
Muse): run "tincan wait &" and the arriving request or reply wakes you. Start
it again after handling it. Network errors are retried with backoff, so a
flaky tailnet path does not end the wait.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if limit > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, limit)
				defer cancel()
			}
			in, err := waitForInbox(ctx, r, client.DefaultPollHold, client.RepliesKeep)
			if err != nil {
				return err
			}
			cmd.Print(formatWait(cmd.Context(), r, in))
			return nil
		},
	}
	cmd.Flags().DurationVar(&limit, "timeout", 0, "give up after this long (0 waits forever)")
	return cmd
}

// waitForInbox long-polls until requests or unseen replies arrive (replies
// says what the poll does with replies), retrying transient errors with
// jittered backoff. It gives up only on ctx or a hard refusal
// (for example, this machine is not a joined agent).
func waitForInbox(ctx context.Context, r *client.Relay, hold time.Duration, replies string) (client.Inbox, error) {
	backoff := time.Second
	for {
		in, err := r.PollReplies(ctx, hold, replies)
		switch {
		case err == nil && !in.Empty():
			return in, nil
		case err == nil:
			backoff = time.Second
			continue
		case ctx.Err() != nil:
			return client.Inbox{}, ctx.Err()
		case client.IsStatus(err, 403):
			return client.Inbox{}, err
		}
		select {
		case <-ctx.Done():
			return client.Inbox{}, ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(time.Now().UnixNano()%int64(d/2+1))
}
