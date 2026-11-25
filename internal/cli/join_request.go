package cli

// Join requests (#227): tincan join --request on a machine that has not
// joined asks the relay to join it under new names, and the owner approves
// on the phone page or with tincan approve-join on the relay host. The
// requesting machine talks only to the relay it was given (or the one its
// pending file names), and gains nothing until the owner approves. See
// docs/trust-model.md, "Join requests".

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

// joinRequestLifetime is identity.JoinRequestTTL as the CLI says it.
var joinRequestLifetime = fmt.Sprintf("%d minutes", int(identity.JoinRequestTTL.Minutes()))

// defaultJoinStatusHold is how long one status read waits for the owner.
// The relay caps it at its own long-poll hold.
const defaultJoinStatusHold = 25 * time.Second

var (
	// joinStatusHold is the wait of each status read; tests shorten it.
	joinStatusHold = defaultJoinStatusHold
	// saveJoinConfig writes one delivered agent's config; tests make it
	// fail.
	saveJoinConfig = client.SaveConfigTo
)

// errJoinRequestProxy refuses a join request through a proxy: the relay
// would record the proxy's machine as the one asking, and bind the agents
// to it.
var errJoinRequestProxy = errors.New("tincan join --request cannot go through a proxy: the relay would record the proxy's machine as the one asking and bind the agents to it. " +
	"A machine that reaches the relay through a proxy joins with an invite instead: ask the owner to run tincan invite <name> --kind proxy-sandbox, " +
	"then run tincan join <code> --relay <relay URL> --proxy <proxy> here")

// joinTarget is one requested agent and the config file it is saved to.
type joinTarget struct {
	agent client.JoinRequestAgent
	path  string
}

// defaultJoinKind is the kind of a bare --request entry: the name itself
// when it is a known kind, else none. The relay does not default it.
func defaultJoinKind(name string) string {
	if name != "" && onboard.KnownKind(name) {
		return name
	}
	return ""
}

// parseJoinEntries reads --request entries, each name or name:kind.
func parseJoinEntries(entries []string) ([]client.JoinRequestAgent, error) {
	var agents []client.JoinRequestAgent
	for _, e := range entries {
		e = strings.TrimSpace(e)
		name, kind, hasKind := strings.Cut(e, ":")
		if !hasKind {
			kind = defaultJoinKind(name)
		}
		switch {
		case !identity.ValidName(name):
			return nil, fmt.Errorf("--request %q: an agent name is up to 32 lowercase letters, digits and dashes, optionally followed by :kind", e)
		case !onboard.KnownKind(kind):
			return nil, fmt.Errorf("--request %q: unknown kind %q (want one of %s)", e, kind, strings.Join(onboard.Kinds, ", "))
		case !onboard.RequestableKind(kind):
			return nil, fmt.Errorf("--request %q: a %s agent joins with an invite, not a join request (tincan invite %s --kind %s on an admin device)", e, kind, name, kind)
		case slices.ContainsFunc(agents, func(a client.JoinRequestAgent) bool { return a.Name == name }):
			return nil, fmt.Errorf("--request names %s twice", name)
		}
		agents = append(agents, client.JoinRequestAgent{Name: name, Kind: kind})
	}
	switch {
	case len(agents) == 0:
		return nil, errors.New("--request needs at least one agent name, such as --request codex")
	case len(agents) > identity.MaxJoinAgents:
		return nil, fmt.Errorf("a join request names at most %d agents", identity.MaxJoinAgents)
	}
	return agents, nil
}

// joinEntry is the --request entry that asks for a again.
func joinEntry(a client.JoinRequestAgent) string {
	if a.Kind == defaultJoinKind(a.Name) {
		return a.Name
	}
	return a.Name + ":" + a.Kind
}

// joinRequestCommand is the command that files, or resumes, a request for
// agents with relay, prefixed with this config's TINCAN_CONFIG when one is
// set (the pending file lives beside it).
func joinRequestCommand(agents []client.JoinRequestAgent, relay string) string {
	entries := make([]string, len(agents))
	for i, a := range agents {
		entries[i] = joinEntry(a)
	}
	return configEnvPrefix() + "tincan join --request " + strings.Join(entries, ",") + " --relay " + relay
}

// configEnvPrefix is "TINCAN_CONFIG=<path> " when TINCAN_CONFIG is set.
func configEnvPrefix() string {
	if os.Getenv("TINCAN_CONFIG") == "" {
		return ""
	}
	return "TINCAN_CONFIG=" + client.ConfigPath() + " "
}

// describeJoinAgents lists agents with their kinds, for people.
func describeJoinAgents(agents []client.JoinRequestAgent) string {
	parts := make([]string, len(agents))
	for i, a := range agents {
		kind := "no kind"
		if a.Kind != "" {
			kind = "kind " + a.Kind
		}
		parts[i] = fmt.Sprintf("%s (%s)", heldText(a.Name), heldText(kind))
	}
	return strings.Join(parts, ", ")
}

// joinConfigPaths is where each requested agent's config goes: one name
// to ConfigPath(), exactly as tincan join; several to <name>.json each in
// its folder.
func joinConfigPaths(agents []client.JoinRequestAgent) []joinTarget {
	if len(agents) == 1 {
		return []joinTarget{{agents[0], client.ConfigPath()}}
	}
	dir := filepath.Dir(client.ConfigPath())
	out := make([]joinTarget, len(agents))
	for i, a := range agents {
		out[i] = joinTarget{a, filepath.Join(dir, a.Name+".json")}
	}
	return out
}

// joinTargets checks every config the request would write before anything
// is filed: none may already belong to a different agent.
func joinTargets(agents []client.JoinRequestAgent, relay string) ([]joinTarget, error) {
	targets := joinConfigPaths(agents)
	pending := client.JoinRequestPath()
	for _, t := range targets {
		if t.path == pending {
			return nil, fmt.Errorf("%s is where the pending join request is kept; pick another name than %s", pending, t.agent.Name)
		}
		cfg, err := client.LoadConfigFrom(t.path)
		switch {
		case err != nil:
			return nil, fmt.Errorf("cannot read %s, where %s's config would go: %w", t.path, t.agent.Name, err)
		case cfg.Proxy != "":
			return nil, errJoinRequestProxy
		case cfg.Agent != "" && cfg.Agent != t.agent.Name:
			next := "Move that file away, or pick another name"
			if len(targets) == 1 {
				next = fmt.Sprintf("For a second agent on this machine, give it its own config: TINCAN_CONFIG=<new file> tincan join --request %s --relay %s", joinEntry(t.agent), relay)
			}
			return nil, fmt.Errorf("%s already belongs to agent %q, so saving %q there would make %q act as %q. %s",
				t.path, cfg.Agent, t.agent.Name, cfg.Agent, t.agent.Name, next)
		}
	}
	return targets, nil
}

// joinRequestRelay is a client for the relay at url that sends nothing
// through a proxy on another machine.
func joinRequestRelay(url string) (*client.Relay, error) {
	r, err := client.NewRelay(url, "")
	if err != nil {
		return nil, err
	}
	if r.ThroughRemoteProxy() {
		return nil, errJoinRequestProxy
	}
	return r, nil
}

// olderRelayJoin is the stop for a relay without join requests: it never
// falls back to anything else.
func olderRelayJoin(err error, relay string, agents []client.JoinRequestAgent) error {
	return fmt.Errorf("%w. The relay at %s does not support join requests (it is older than this tincan). "+
		"Ask the owner for an invite instead: they run tincan invite %s on an admin device, then you run tincan join <code> --relay %s here",
		err, relay, agents[0].Name, relay)
}

// fileJoinError explains a refused filing.
func fileJoinError(err error, relay string, agents []client.JoinRequestAgent) error {
	switch {
	case client.IsOlderRelay(err):
		return olderRelayJoin(err, relay, agents)
	case client.IsStatus(err, http.StatusTooManyRequests):
		return fmt.Errorf("%w. A machine may have %d open join request and file %d an hour, and the relay holds %d at a time: wait for the open ones to finish or expire (%s after filing)",
			err, identity.MaxOpenJoinRequestsPerNode, identity.MaxJoinRequestsPerHour, identity.MaxPendingJoinRequests, joinRequestLifetime)
	}
	return err
}

// runJoinRequest is tincan join --request: file (or resume) a request,
// wait for the owner, collect the agents and save their configs.
func runJoinRequest(cmd *cobra.Command, relayURL string, entries []string, proxy string) error {
	ctx := cmd.Context()
	cfg, err := client.LoadConfig()
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", client.ConfigPath(), err)
	}
	if proxy != "" || cfg.Proxy != "" {
		return errJoinRequestProxy
	}
	agents, err := parseJoinEntries(entries)
	if err != nil {
		return err
	}
	pendingPath := client.JoinRequestPath()
	p, found, err := client.LoadPendingJoinRequest(pendingPath)
	if err != nil {
		return fmt.Errorf("%w. Remove it to file a new join request", err)
	}
	if found {
		r, err := joinRequestRelay(p.Relay)
		if err != nil {
			return err
		}
		sameRelay := relayURL == "" || strings.TrimRight(relayURL, "/") == strings.TrimRight(p.Relay, "/")
		if sameRelay && slices.Equal(p.Agents, agents) {
			targets, err := joinTargets(agents, p.Relay)
			if err != nil {
				return err
			}
			cmd.Printf("Resuming join request %s with %s for %s.\n", p.ID, p.Relay, describeJoinAgents(p.Agents))
			printJoinTargets(cmd, targets)
			printJoinCode(cmd, p)
			return followJoinRequest(ctx, cmd, r, p, targets, pendingPath)
		}
		if err := clearFinishedJoinRequest(ctx, cmd, r, p, pendingPath); err != nil {
			return err
		}
	}
	if relayURL == "" {
		return errors.New("--relay is required: the relay to ask, for example --relay http://tincan-relay")
	}
	r, err := joinRequestRelay(relayURL)
	if err != nil {
		return err
	}
	targets, err := joinTargets(agents, r.Base())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0o700); err != nil {
		return err
	}
	st, err := r.FileJoinRequest(ctx, agents)
	if err != nil {
		return fileJoinError(err, r.Base(), agents)
	}
	p = client.PendingJoinRequest{Relay: r.Base(), ID: st.ID, Code: st.Code, Agents: agents, FiledAt: st.FiledAt, ExpiresAt: st.ExpiresAt}
	if err := client.SavePendingJoinRequest(pendingPath, p); err != nil {
		cmd.PrintErrf("Warning: could not save %s (%v), so if this command stops before the request finishes it cannot pick it up again.\n", pendingPath, err)
	}
	cmd.Printf("Filed join request %s with %s for %s.\n", p.ID, p.Relay, describeJoinAgents(p.Agents))
	printJoinTargets(cmd, targets)
	printJoinCode(cmd, p)
	return followJoinRequest(ctx, cmd, r, p, targets, pendingPath)
}

// printJoinTargets says where several agents' configs go.
func printJoinTargets(cmd *cobra.Command, targets []joinTarget) {
	if len(targets) < 2 {
		return
	}
	cmd.Printf("Each agent gets its own config; run its commands with its line:\n")
	for _, t := range targets {
		cmd.Printf("  TINCAN_CONFIG=%s   (%s)\n", t.path, t.agent.Name)
	}
}

// printJoinCode shows the matching code and what the owner compares it with.
func printJoinCode(cmd *cobra.Command, p client.PendingJoinRequest) {
	cmd.Printf("\n    Matching code: %s\n\n", p.Code)
	cmd.Printf("Ask the owner to approve it, on the Tincan phone page or with tincan join-requests and tincan approve-join %s on the relay host.\n"+
		"They should approve only if the code they see there is %s. The code is not a password: nobody types it.\n", p.ID, p.Code)
	until := ""
	if !p.ExpiresAt.IsZero() {
		until = " (the request expires at " + p.ExpiresAt.Local().Format("15:04:05") + ")"
	}
	cmd.Printf("Waiting for the owner%s. Ctrl-C stops waiting; run the same command again to pick it up.\n", until)
}

// clearFinishedJoinRequest lets a different request be filed when the one
// in the pending file has finished, and refuses while it is still open.
func clearFinishedJoinRequest(ctx context.Context, cmd *cobra.Command, r *client.Relay, p client.PendingJoinRequest, pendingPath string) error {
	what := fmt.Sprintf("join request %s with %s for %s", p.ID, p.Relay, describeJoinAgents(p.Agents))
	again := joinRequestCommand(p.Agents, p.Relay)
	st, err := r.JoinRequestStatus(ctx, p.ID, 0)
	switch {
	case client.IsStatus(err, http.StatusNotFound):
		// Unknown to the relay (or a relay without join requests): nothing
		// more can come of it.
	case err != nil:
		return fmt.Errorf("cannot read the %s: %w. It must finish or expire before you file another; to keep waiting for it, run %s", what, err, again)
	case st.Status == client.JoinPending || st.Status == client.JoinApproved:
		until := ""
		if !p.ExpiresAt.IsZero() {
			until = " until " + p.ExpiresAt.Local().Format("15:04:05")
		}
		return fmt.Errorf("the %s is still %s%s: it must finish or expire before you file another. To keep waiting for it, run %s", what, st.Status, until, again)
	case st.Status == client.JoinDelivered:
		return fmt.Errorf("the %s was delivered, but its configs were not saved here. Finish it first: run %s", what, again)
	}
	if err := client.RemovePendingJoinRequest(pendingPath); err != nil {
		return err
	}
	status := st.Status
	if status == "" {
		status = "unknown to the relay"
	}
	cmd.Printf("The earlier %s is %s; filing the new one.\n", what, status)
	return nil
}

// followJoinRequest waits for the owner's decision, then collects the
// agents and saves their configs. Interrupted, it leaves the request open.
func followJoinRequest(ctx context.Context, cmd *cobra.Command, r *client.Relay, p client.PendingJoinRequest, targets []joinTarget, pendingPath string) error {
	stillOpen := func() error {
		return fmt.Errorf("stopped waiting. Join request %s is still open until it expires; to keep waiting for it, run %s", p.ID, joinRequestCommand(p.Agents, p.Relay))
	}
	backoff := time.Second
	retry := func(err error) error {
		cmd.PrintErrf("Could not reach the relay (%v); trying again.\n", err)
		select {
		case <-ctx.Done():
			return stillOpen()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
		return nil
	}
	for {
		st, err := r.JoinRequestStatus(ctx, p.ID, joinStatusHold)
		if err != nil {
			if ctx.Err() != nil {
				return stillOpen()
			}
			if final, ferr := joinStatusError(err, p, pendingPath); final {
				return ferr
			}
			if err := retry(err); err != nil {
				return err
			}
			continue
		}
		backoff = time.Second
		switch st.Status {
		case client.JoinPending:
			continue
		case client.JoinApproved:
			cmd.Printf("Approved. Collecting the agents.\n")
			got, err := r.ClaimJoinRequest(ctx, p.ID)
			if err != nil {
				if ctx.Err() != nil {
					return stillOpen()
				}
				if final, ferr := joinStatusError(err, p, pendingPath); final {
					return ferr
				}
				// The claim may have landed with its answer lost: the
				// next status read says delivered, and that is recovered.
				if err := retry(err); err != nil {
					return err
				}
				continue
			}
			switch {
			case got.Status == client.JoinDelivered && len(got.Agents) > 0:
				return saveJoinedConfigs(ctx, cmd, p, targets, pendingPath)
			case got.Status == client.JoinDelivered:
				return recoverDelivered(ctx, cmd, p, targets, pendingPath)
			case got.Status == client.JoinPending || got.Status == client.JoinApproved:
				continue
			}
			return finishJoinRequest(got, p, pendingPath)
		case client.JoinDelivered:
			return recoverDelivered(ctx, cmd, p, targets, pendingPath)
		default:
			return finishJoinRequest(st, p, pendingPath)
		}
	}
}

// joinStatusError reads a failed status or claim call: final says waiting
// cannot help, with the error to return; otherwise the call is retried.
func joinStatusError(err error, p client.PendingJoinRequest, pendingPath string) (final bool, _ error) {
	e, ok := errors.AsType[*client.APIError](err)
	switch {
	case !ok || e.Code >= 500:
		return false, nil
	case client.IsOlderRelay(err):
		_ = client.RemovePendingJoinRequest(pendingPath)
		return true, olderRelayJoin(err, p.Relay, p.Agents)
	case e.Code == http.StatusNotFound:
		_ = client.RemovePendingJoinRequest(pendingPath)
		return true, fmt.Errorf("%w. The relay at %s no longer knows join request %s (it finished over an hour ago, or the relay lost it), so nothing more can be collected. "+
			"If its agents were already delivered to this machine, save each with %s; otherwise file again: %s",
			err, p.Relay, p.ID, rejoinLines(p, joinConfigPaths(p.Agents)), joinRequestCommand(p.Agents, p.Relay))
	case e.Code == http.StatusForbidden:
		return true, fmt.Errorf("%w. If this machine was rebuilt since it filed join request %s, remove %s and file again: %s", err, p.ID, pendingPath, joinRequestCommand(p.Agents, p.Relay))
	}
	return true, err
}

// rejoinLines is the rejoin command for each target, joined with " and ".
func rejoinLines(p client.PendingJoinRequest, targets []joinTarget) string {
	lines := make([]string, len(targets))
	for i, t := range targets {
		lines[i] = rejoinLine(p.Relay, t)
	}
	return strings.Join(lines, " and ")
}

func rejoinLine(relay string, t joinTarget) string {
	return fmt.Sprintf("TINCAN_CONFIG=%s tincan rejoin --relay %s --name %s", t.path, relay, t.agent.Name)
}

// finishJoinRequest ends a request that reached a final state without
// delivering: it removes the pending file and says what happened.
func finishJoinRequest(st client.JoinRequestState, p client.PendingJoinRequest, pendingPath string) error {
	_ = client.RemovePendingJoinRequest(pendingPath)
	again := joinRequestCommand(p.Agents, p.Relay)
	switch st.Status {
	case client.JoinDenied:
		return fmt.Errorf("the owner denied join request %s, so nothing was joined. Ask the owner before filing again", p.ID)
	case client.JoinExpired:
		return fmt.Errorf("join request %s expired before it was approved and collected (a request lasts %s), so nothing was joined. File again: %s", p.ID, joinRequestLifetime, again)
	case client.JoinVoided:
		return fmt.Errorf("join request %s was voided: this machine's Tailscale node key changed after it filed, so the relay no longer trusts it as the machine that asked. Nothing was joined. File again: %s", p.ID, again)
	case client.JoinFailed:
		why := "a name could no longer be joined"
		if st.Error != "" {
			why = st.Error
		}
		return fmt.Errorf("join request %s failed when its agents were collected (%s), so nothing was joined. Pick names that are free and file again", p.ID, why)
	}
	return fmt.Errorf("join request %s ended as %q, so nothing was joined. File again: %s", p.ID, st.Status, again)
}

// recoverDelivered finishes a request the relay delivered whose answer this
// machine never got: it confirms each name with whoami, as rejoin does,
// then saves the configs.
func recoverDelivered(ctx context.Context, cmd *cobra.Command, p client.PendingJoinRequest, targets []joinTarget, pendingPath string) error {
	cmd.Printf("The relay already delivered join request %s (the answer to collecting it was lost); confirming each name with the relay.\n", p.ID)
	for _, t := range targets {
		r, err := client.NewRelayForFile(client.Config{Relay: p.Relay, Agent: t.agent.Name}, t.path)
		if err != nil {
			return err
		}
		me, err := r.WhoAmI(ctx)
		if err == nil && me.Name != t.agent.Name {
			err = fmt.Errorf("the relay says this machine is %q", me.Name)
		}
		if err != nil {
			return fmt.Errorf("join request %s was delivered, but the relay does not confirm %s on this machine: %w. Try %s", p.ID, t.agent.Name, err, rejoinLine(p.Relay, t))
		}
	}
	return saveJoinedConfigs(ctx, cmd, p, targets, pendingPath)
}

// saveJoinedConfigs writes each delivered agent's config, then removes the
// pending file. A config that cannot be written gets its rejoin line, and
// the pending file stays so doctor and a rerun can finish the job.
func saveJoinedConfigs(ctx context.Context, cmd *cobra.Command, p client.PendingJoinRequest, targets []joinTarget, pendingPath string) error {
	var unsaved []string
	for _, t := range targets {
		cfg, err := client.LoadConfigFrom(t.path)
		if err == nil {
			setRelay(&cfg, p.Relay)
			cfg.Agent = t.agent.Name
			err = saveJoinConfig(t.path, cfg)
		}
		if err != nil {
			unsaved = append(unsaved, fmt.Sprintf("  could not save %s to %s: %v\n  %s", t.agent.Name, t.path, err, rejoinLine(p.Relay, t)))
			continue
		}
		if r, err := client.NewRelayForFile(cfg, t.path); err == nil {
			learnRelayKeyWithin(ctx, r)
		}
		kind := ""
		if t.agent.Kind != "" {
			kind = " (kind " + t.agent.Kind + ")"
		}
		if len(targets) == 1 {
			cmd.Printf("Joined as %q%s. Config saved to %s\n", t.agent.Name, kind, t.path)
		} else {
			cmd.Printf("Joined as %q%s. Config saved to %s; run its commands with TINCAN_CONFIG=%s\n", t.agent.Name, kind, t.path, t.path)
		}
	}
	if len(unsaved) > 0 {
		return fmt.Errorf("the relay joined every name of join request %s, but %d config(s) could not be saved. Save each with the line below it once the problem is fixed (or run %s again):\n%s",
			p.ID, len(unsaved), joinRequestCommand(p.Agents, p.Relay), strings.Join(unsaved, "\n"))
	}
	if err := client.RemovePendingJoinRequest(pendingPath); err != nil {
		cmd.PrintErrf("Warning: could not remove %s: %v\n", pendingPath, err)
	}
	return nil
}

// joinOwnerRelay is the relay host's admin socket, which the join request
// owner commands use; a decision from another device over the tailnet is
// refused by the relay, and is taken on the phone page instead.
func joinOwnerRelay(socket, relayURL string) (*client.Relay, error) {
	if relayURL != "" {
		return nil, errors.New("join requests are listed and decided on the relay host itself, over its admin socket, or on the phone page: " +
			"the relay refuses a decision from another device over the tailnet, so --relay is not taken here")
	}
	if socket == "" {
		socket = localAdminSocket()
	}
	if socket == "" {
		return nil, errors.New("no relay runs on this machine: run this on the relay host (or pass --socket <admin socket>), or approve on the phone page")
	}
	return client.NewRelaySocket(socket), nil
}

// olderRelayJoinOwner is the upgrade hint for a relay host whose relay has
// no join requests.
func olderRelayJoinOwner(err error) error {
	if !client.IsOlderRelay(err) {
		return err
	}
	return fmt.Errorf("%w. The relay is older than this tincan and has no join requests: upgrade the relay to this release and restart it", err)
}

func joinRequestsCmd() *cobra.Command {
	var socket, relayURL string
	cmd := &cobra.Command{
		Use:   "join-requests",
		Short: "List machines asking to join, with their matching codes (on the relay host)",
		Long: `List the machines asking to join the team with tincan join --request, on the
relay host over its admin socket. Each shows its matching code, the machine
and its owning login, tags and tailnet addresses, the names (with kinds) it
asks for, and any agents already on it. Approve only when the code matches
the one on the requesting machine's screen: tincan approve-join <id>, or
tincan deny-join <id>. The phone page shows the same requests.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := joinOwnerRelay(socket, relayURL)
			if err != nil {
				return err
			}
			list, err := r.JoinRequests(cmd.Context())
			if err != nil {
				return olderRelayJoinOwner(err)
			}
			if len(list) == 0 {
				cmd.Printf("No join requests are waiting.\n")
				return nil
			}
			for _, l := range list {
				printJoinListing(cmd, l, time.Now())
			}
			cmd.Printf("Approve only if the code matches the one on the requesting machine's screen.\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (default: this relay host's)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "not taken: join requests are listed on the relay host or the phone page")
	_ = cmd.Flags().MarkHidden("relay")
	return cmd
}

// printJoinListing prints one pending request. Every requester-chosen
// value goes through heldText, so control characters show as codes.
func printJoinListing(cmd *cobra.Command, l client.JoinRequestListing, now time.Time) {
	age := time.Duration(l.AgeSeconds) * time.Second
	left := l.ExpiresAt.Sub(now).Round(time.Second)
	cmd.Printf("%s  code %s  filed %s ago, expires in %s\n", heldText(l.ID), heldText(l.Code), age, max(left, 0))
	machine := heldText(l.NodeName)
	if l.NodeUser != "" {
		machine += " (" + heldText(l.NodeUser) + ")"
	}
	cmd.Printf("  machine: %s\n", machine)
	if len(l.Tags) > 0 {
		cmd.Printf("  tags: %s\n", heldText(strings.Join(l.Tags, ", ")))
	}
	if len(l.Addrs) > 0 {
		cmd.Printf("  addresses: %s\n", heldText(strings.Join(l.Addrs, ", ")))
	}
	cmd.Printf("  asks to join as: %s\n", describeJoinAgents(l.Agents))
	if len(l.NodeAgents) > 0 {
		cmd.Printf("  already on that machine: %s\n", heldText(strings.Join(l.NodeAgents, ", ")))
	}
	cmd.Printf("  approve: tincan approve-join %s    deny: tincan deny-join %s\n\n", heldText(l.ID), heldText(l.ID))
}

// joinDecisionCmd is approve-join or deny-join.
func joinDecisionCmd(approve bool) *cobra.Command {
	var socket, relayURL string
	use, short, done := "deny-join <id>", "Deny a machine's join request (on the relay host); final", "Denied join request %s. Nothing was joined.\n"
	if approve {
		use, short, done = "approve-join <id>", "Approve a machine's join request (on the relay host)",
			"Approved join request %s. The machine collects its agents when it next checks (within seconds while tincan join --request is waiting there).\n"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := joinOwnerRelay(socket, relayURL)
			if err != nil {
				return err
			}
			if _, err := r.DecideJoinRequest(cmd.Context(), args[0], approve); err != nil {
				return olderRelayJoinOwner(err)
			}
			cmd.Printf(done, heldText(args[0]))
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (default: this relay host's)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "not taken: join requests are decided on the relay host or the phone page")
	_ = cmd.Flags().MarkHidden("relay")
	return cmd
}

// joinRequestCheck is doctor's report on a join request this machine filed
// and has not finished, read from the relay its pending file names. It
// never claims and never fails: every state warns with the next step. With
// no pending file there is no check.
func joinRequestCheck(ctx context.Context) (check, bool) {
	const name = "join request"
	path := client.JoinRequestPath()
	p, found, err := client.LoadPendingJoinRequest(path)
	if err != nil {
		return check{name, "warn", "cannot read the pending join request: " + err.Error(), "Remove " + path + ", then file again with tincan join --request <name> --relay <relay URL>."}, true
	}
	if !found {
		return check{}, false
	}
	what := fmt.Sprintf("join request %s with %s for %s", p.ID, p.Relay, describeJoinAgents(p.Agents))
	again := joinRequestCommand(p.Agents, p.Relay)
	removeAndFile := path + ", then file again: " + again
	fileAgain := "Remove " + removeAndFile
	r, err := client.NewRelay(p.Relay, "")
	if err != nil {
		return check{name, "warn", what + ": " + err.Error(), "Remove " + path + " and file again with a working relay URL."}, true
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st, err := r.JoinRequestStatus(cctx, p.ID, 0)
	switch {
	case client.IsOlderRelay(err):
		return check{name, "warn", what + ": the relay does not support join requests (it is older than this tincan)",
			fmt.Sprintf("Ask the owner for an invite: they run tincan invite %s on an admin device, then run tincan join <code> --relay %s here and remove %s.", p.Agents[0].Name, p.Relay, path)}, true
	case client.IsStatus(err, http.StatusNotFound):
		return check{name, "warn", what + ": the relay does not know this request (it finished over an hour ago, or the relay lost it)",
			fmt.Sprintf("Remove %s. If its agents were delivered to this machine, save each with %s; otherwise file again: %s", path, rejoinLines(p, joinConfigPaths(p.Agents)), again)}, true
	case client.IsStatus(err, http.StatusForbidden):
		return check{name, "warn", what + ": the relay refuses to show it to this machine: " + err.Error(),
			"If this machine was rebuilt since it filed, remove " + removeAndFile}, true
	case err != nil:
		return check{name, "warn", what + ": cannot read its status: " + err.Error(), "Check that the relay at " + p.Relay + " is reachable, then run tincan doctor again."}, true
	}
	code := p.Code
	if st.Code != "" {
		code = st.Code
	}
	switch st.Status {
	case client.JoinPending:
		return check{name, "warn", what + " is pending, waiting for the owner. Matching code: " + code,
			fmt.Sprintf("Ask the owner to approve it on the phone page, or with tincan join-requests and tincan approve-join %s on the relay host, if the code there is %s. Keep %s running to collect the agents (run it again if it stopped).", p.ID, code, again)}, true
	case client.JoinApproved:
		return check{name, "warn", what + " is approved but its agents are not collected yet; it expires " + joinRequestLifetime + " after filing",
			"Run " + again + " now to collect the agents and save their configs."}, true
	case client.JoinDelivered:
		return check{name, "warn", what + " was delivered: the relay joined the names, but their configs were not saved on this machine",
			fmt.Sprintf("Run %s, which confirms each name with the relay and saves its config (or save each with %s).", again, rejoinLines(p, joinConfigPaths(p.Agents)))}, true
	case client.JoinDenied:
		return check{name, "warn", what + " was denied by the owner; nothing was joined",
			"Remove " + path + ". Ask the owner before filing again (" + again + ")."}, true
	case client.JoinExpired:
		return check{name, "warn", what + " expired before it was approved and collected; nothing was joined", fileAgain}, true
	case client.JoinVoided:
		return check{name, "warn", what + " was voided: this machine's Tailscale node key changed after it filed, so the relay no longer trusts it as the machine that asked; nothing was joined", fileAgain}, true
	case client.JoinFailed:
		return check{name, "warn", what + " failed: a name could no longer be joined when its agents were collected; nothing was joined",
			"Remove " + path + ", pick names that are free, then file again: " + again}, true
	}
	return check{name, "warn", fmt.Sprintf("%s has status %q, which this tincan does not know", what, st.Status), "Run tincan upgrade, then tincan doctor again."}, true
}
