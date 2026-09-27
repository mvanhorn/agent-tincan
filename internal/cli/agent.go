package cli

import (
	"context"
	"encoding/json"
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
	if err == nil && client.NeedsRelayInfo(cfg) {
		// Learn the relay key and addresses (and refresh them daily), so
		// this agent can find the relay again if its address changes.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client.LearnRelayKey(ctx, r)
		cancel()
	}
	return r, cfg, err
}

// learnRelayInfo asks the relay for its key and addresses and saves them to
// the config cfg was just written to, so the agent can find the relay again
// if its address changes. join and rejoin call it right away rather than
// leaving it to the next command, which a service agent (history serve, web
// serve) never runs. Quiet on failure: the next connect tries again.
func learnRelayInfo(ctx context.Context, cfg client.Config) {
	r, err := client.NewRelayFor(cfg)
	if err != nil {
		return
	}
	learnRelayKeyWithin(ctx, r)
}

// learnRelayKeyWithin is client.LearnRelayKey with the same 5-second bound
// connect uses, so a relay that accepts the connection but never answers
// cannot hold up join, rejoin or a service's startup.
func learnRelayKeyWithin(ctx context.Context, r *client.Relay) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client.LearnRelayKey(ctx, r)
}

// setRelay points cfg at url. The relay key and addresses belong to the old
// relay, so a different url drops them: if learnRelayInfo then fails, the
// config must not pair the new address with the old relay's key.
func setRelay(cfg *client.Config, url string) {
	if strings.TrimRight(url, "/") != strings.TrimRight(cfg.Relay, "/") {
		cfg.RelayKey, cfg.RelayURLs, cfg.RelayInfoAt = "", nil, time.Time{}
	}
	cfg.Relay = url
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
				setRelay(&cfg, relayURL)
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
			learnRelayInfo(cmd.Context(), cfg)
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
			relay := inviteRelayURL(relayURL, socket)
			if relayURL == "" {
				// Over an admin socket (explicit or this machine's own), ask
				// the relay where agents reach it.
				var urls struct {
					RelayURLs []string `json:"relay_urls"`
				}
				if r.Raw(cmd.Context(), "GET", "/v1/admin/urls", nil, &urls) == nil && len(urls.RelayURLs) > 0 && (socket != "" || strings.HasPrefix(r.Base(), "http://tincan-admin")) {
					relay = urls.RelayURLs[0]
				}
			}
			cmd.Printf("Invite code for %q (%s): %s\nOn that machine run:\n  tincan join %s --relay %s\n"+
				"(for a second agent on a machine that already runs one, prefix with TINCAN_CONFIG=<new file>)\n\n"+
				"Or paste this into the agent:\n\n  Join my Agent Tincan team as %s. Your invite code is %s (valid 10 minutes).\n"+
				"  The relay is %s. Follow https://agenttincan.com/agents.txt, part B.\n",
				args[0], valid, code, code, relay, args[0], code, relay)
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&kind, "kind", "", "the agent's runtime (hermes, codex, ...), recorded on join so tincan onboard tailors its block")
	return cmd
}

// inviteRelayURL is the relay URL to print in an invite's join line: the
// --relay flag, else the saved config (or TINCAN_RELAY) when the invite
// went through it, else a placeholder. An invite over the admin socket
// never falls back to the saved relay, which may name a different relay
// than the one the socket serves.
func inviteRelayURL(flag, socket string) string {
	if flag != "" {
		return flag
	}
	if socket != "" {
		return "<relay URL>"
	}
	if cfg, _ := client.LoadConfig(); cfg.Relay != "" {
		return cfg.Relay
	}
	return "<relay URL>"
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
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "List agents in the mesh, whether they are online, how they wake, when each last called the relay, and which tincan build each runs",
		Long: `List agents in the mesh, whether they are online, how they wake, when each
last called the relay, and which tincan build each last called with. The
first line, "# relay version X", names the relay's own build, so an agent
that has not run tincan upgrade stands out. It starts with "# " so scripts
can skip it; every other line is one agent.

Works from any joined agent (saved config). An admin device that never joined
passes --relay <url> (or sets TINCAN_RELAY). The relay host needs no flags:
its local admin socket is used, like the other admin commands (--socket
names one elsewhere).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if socket == "" && relayURL == "" && localAdminSocket() == "" {
				if cfg, _ := client.LoadConfig(); cfg.Relay == "" {
					return errors.New("no relay configured: on an admin device pass --relay <url> (or set TINCAN_RELAY), on the relay host pass --socket <state-dir>/admin.sock, or run `tincan join <code> --relay <url>` on an agent")
				}
			}
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			ro, err := r.Roster(cmd.Context())
			if err != nil {
				return err
			}
			cmd.Print(formatRoster(ro, time.Now()))
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config or TINCAN_RELAY)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	return cmd
}

// formatRoster is formatAgents with a first line naming the relay's own
// build, when the relay reports one. The line starts with "# " so a script
// reading one agent per line can skip it.
func formatRoster(ro client.Roster, now time.Time) string {
	out := formatAgents(ro.Agents, now)
	if ro.RelayVersion != "" {
		out = "# relay version " + ro.RelayVersion + "\n" + out
	}
	return out
}

func formatAgents(agents []client.AgentInfo, now time.Time) string {
	var b strings.Builder
	for _, a := range agents {
		fmt.Fprintf(&b, "%-14s %-8s wake=%s %s", a.Name, a.State(), a.Wake, a.LastSeen(now))
		if a.Kind != "" {
			fmt.Fprintf(&b, " kind=%s", a.Kind)
		}
		if a.Version != "" {
			fmt.Fprintf(&b, " version=%s", a.Version)
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
	var notify, asJSON bool
	var attach []string
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
			ups, err := r.UploadFiles(cmd.Context(), attach)
			if err != nil {
				return err
			}
			ids := client.AttachmentIDs(ups)
			if notify {
				req, err := r.SendAttached(cmd.Context(), args[0], body, envelope.KindNotify, parent, ids)
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), sentJSON{Outcome: "sent", Request: req})
				}
				cmd.Printf("Sent to %s (request %s).\n", args[0], req.ID)
				return nil
			}
			res, err := r.AskAttached(cmd.Context(), args[0], body, parent, ids, client.ClampWait(wait))
			if err != nil {
				return err
			}
			if asJSON {
				return printResultJSON(cmd.OutOrStdout(), res)
			}
			cmd.Print(client.FormatResult(res))
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", client.MaxInlineWait, "how long to wait for the reply (max 20s)")
	cmd.Flags().StringVar(&parent, "parent", "", "the request you are handling, if this continues it (usually automatic)")
	cmd.Flags().BoolVar(&notify, "notify", false, "send without waiting for a reply")
	cmd.Flags().StringArrayVar(&attach, "attach", nil, "a local file to attach (repeatable; images or small files)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON and exit 0 answered, 1 failed, 2 pending (--notify: 0 once sent)")
	return cmd
}

func getCmd() *cobra.Command {
	var wait time.Duration
	var asJSON bool
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
			if asJSON {
				return printResultJSON(cmd.OutOrStdout(), res)
			}
			cmd.Print(client.FormatResult(res))
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 0, "wait up to this long for a reply (max 20s)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON and exit 0 answered, 1 failed, 2 pending")
	return cmd
}

func inboxCmd() *cobra.Command {
	var wait time.Duration
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Get requests from teammates (claims them) and replies to your own requests",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			if asJSON {
				return checkInboxJSON(cmd.Context(), r, client.ClampWait(wait), cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			return checkInbox(cmd.Context(), r, client.ClampWait(wait), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 0, "wait up to this long for a request (max 20s)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the requests (claimed) and replies as JSON")
	return cmd
}

// Outcomes of ask and get --json. Each maps to an exit code.
const (
	outcomeAnswered = "answered" // exit 0
	outcomeFailed   = "failed"   // exit 1: failed, declined, cancelled or expired
	outcomePending  = "pending"  // exit 2: no final state yet
)

// resultJSON is what ask and get --json print.
type resultJSON struct {
	Outcome string        `json:"outcome"`
	Result  client.Result `json:"result"`
}

// sentJSON is what ask --notify --json prints: no reply is coming.
type sentJSON struct {
	Outcome string           `json:"outcome"`
	Request envelope.Request `json:"request"`
}

// outcome buckets a request's status for scripts.
func outcome(res client.Result) (string, int) {
	switch {
	case res.Status == envelope.StatusAnswered:
		return outcomeAnswered, 0
	case res.Done():
		return outcomeFailed, 1
	default:
		return outcomePending, 2
	}
}

// printResultJSON prints res with its outcome and returns a silent exit
// error for any outcome but answered, so the exit code tells a script what
// happened without parsing.
func printResultJSON(w io.Writer, res client.Result) error {
	name, code := outcome(res)
	if err := writeJSON(w, resultJSON{Outcome: name, Result: res}); err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code, Silent: true}
	}
	return nil
}

// writeJSON writes v as indented JSON and a newline, and reports write
// errors (unlike printJSON), so inbox never acks replies it failed to print.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// inboxRequestJSON is a request as inbox --json lists it. Claimed is false,
// with ClaimError saying why, when another session got to it first.
type inboxRequestJSON struct {
	envelope.Request
	Claimed    bool   `json:"claimed"`
	ClaimError string `json:"claim_error,omitempty"`
}

// inboxJSON is what inbox --json prints. The lists are never null.
type inboxJSON struct {
	Requests         []inboxRequestJSON `json:"requests"`
	Replies          []client.Result    `json:"replies"`
	RepliesRemaining int                `json:"replies_remaining,omitempty"`
}

// checkInboxJSON is checkInbox printing JSON.
func checkInboxJSON(ctx context.Context, r *client.Relay, wait time.Duration, out, errOut io.Writer) error {
	in, err := r.Poll(ctx, wait)
	if err != nil {
		return err
	}
	return printInboxJSON(ctx, r, in, out, errOut)
}

// printInboxJSON claims each request in in, prints the inbox as JSON, and
// only then acknowledges the replies, as checkInbox does.
func printInboxJSON(ctx context.Context, r *client.Relay, in client.Inbox, out, errOut io.Writer) error {
	doc := inboxJSON{
		Requests:         make([]inboxRequestJSON, 0, len(in.Requests)),
		Replies:          in.Replies,
		RepliesRemaining: in.RepliesRemaining,
	}
	if doc.Replies == nil {
		doc.Replies = []client.Result{}
	}
	for _, req := range in.Requests {
		item := inboxRequestJSON{Request: req, Claimed: true}
		if _, err := r.Claim(ctx, req.ID); err != nil {
			item.Claimed, item.ClaimError = false, err.Error()
		}
		doc.Requests = append(doc.Requests, item)
	}
	if err := writeJSON(out, doc); err != nil {
		return err
	}
	if err := r.AckReplies(ctx, in.ReplyIDs()); err != nil {
		fmt.Fprintf(errOut, "tincan inbox: could not mark replies read (they may show again): %v\n", err)
	}
	return nil
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
	var attach []string
	cmd := &cobra.Command{
		Use:   "reply <request-id> <message...>",
		Short: "Answer a request from a teammate",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			ups, err := r.UploadFiles(cmd.Context(), attach)
			if err != nil {
				return err
			}
			rep, err := r.ReplyAttached(cmd.Context(), args[0], strings.Join(args[1:], " "), envelope.Status(status), client.AttachmentIDs(ups))
			if err != nil {
				return err
			}
			cmd.Printf("Replied to %s (%s).\n", args[0], rep.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "answered", "answered, failed, or declined")
	cmd.Flags().StringArrayVar(&attach, "attach", nil, "a local file to attach (repeatable; images or small files)")
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
