package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
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
	return []*cobra.Command{joinCmd(), inviteCmd(), removeCmd(), agentsCmd(), askCmd(), getCmd(), inboxCmd(), replyCmd(), cancelCmd(), waitCmd()}
}

func joinCmd() *cobra.Command {
	var relayURL, proxy string
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
	return cmd
}

func inviteCmd() *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "invite <name>",
		Short: "Create a one-time code that joins a machine as <name> (admin devices only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			code, err := r.Invite(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			cmd.Printf("Invite code for %q (valid 10 minutes): %s\nOn that machine run:\n  tincan join %s --relay <relay URL>\n", args[0], code, code)
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
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
		Short: "List agents in the mesh, whether they are online, and how they wake",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			agents, err := r.Agents(cmd.Context())
			if err != nil {
				return err
			}
			cmd.Print(formatAgents(agents))
			return nil
		},
	}
}

func formatAgents(agents []client.AgentInfo) string {
	var b strings.Builder
	for _, a := range agents {
		fmt.Fprintf(&b, "%-14s %-8s wake=%s\n", a.Name, a.State(), a.Wake)
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
		Short: "Get requests from teammates (claims them so no one else handles them)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			out, err := checkInbox(cmd.Context(), r, client.ClampWait(wait))
			if err != nil {
				return err
			}
			cmd.Print(out)
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 0, "wait up to this long for a request (max 20s)")
	return cmd
}

// checkInbox polls once, claims what arrived, and renders it.
func checkInbox(ctx context.Context, r *client.Relay, wait time.Duration) (string, error) {
	reqs, err := r.Poll(ctx, wait)
	if err != nil {
		return "", err
	}
	return claimAndFormat(ctx, r, reqs), nil
}

func claimAndFormat(ctx context.Context, r *client.Relay, reqs []envelope.Request) string {
	if len(reqs) == 0 {
		return "No requests waiting.\n"
	}
	var b strings.Builder
	for _, req := range reqs {
		if _, err := r.Claim(ctx, req.ID); err != nil {
			fmt.Fprintf(&b, "(could not claim %s: %v)\n", req.ID, err)
			continue
		}
		b.WriteString(client.FormatRequest(req))
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
		Short: "Block until a teammate's request arrives, print it, and exit",
		Long: `Block until a request arrives, claim and print it, then exit.

For agents that get a new turn when a background command finishes (like
Muse): run "tincan wait &" and the arriving request wakes you. Start it again
after handling the request. Network errors are retried with backoff, so a
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
			reqs, err := waitForRequests(ctx, r, client.DefaultPollHold)
			if err != nil {
				return err
			}
			cmd.Print(claimAndFormat(cmd.Context(), r, reqs))
			return nil
		},
	}
	cmd.Flags().DurationVar(&limit, "timeout", 0, "give up after this long (0 waits forever)")
	return cmd
}

// waitForRequests long-polls until something arrives, retrying transient
// errors with jittered backoff. It gives up only on ctx or a hard refusal
// (for example, this machine is not a joined agent).
func waitForRequests(ctx context.Context, r *client.Relay, hold time.Duration) ([]envelope.Request, error) {
	backoff := time.Second
	for {
		reqs, err := r.Poll(ctx, hold)
		switch {
		case err == nil && len(reqs) > 0:
			return reqs, nil
		case err == nil:
			backoff = time.Second
			continue
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case client.IsStatus(err, 403):
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(time.Now().UnixNano()%int64(d/2+1))
}
