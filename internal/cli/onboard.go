package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

func onboardCmd() *cobra.Command {
	var asJSON, offline bool
	var section, operator, owner, relayURL, socket string
	var kinds []string
	cmd := &cobra.Command{
		Use:   "onboard",
		Short: "Print the Agent Tincan operator prompt, a join kit for every agent, and add-agent recipes",
		Long: `Print the setup kit for this team, built from the live roster:

  operator  the standing prompt for the Agent Tincan operator role
  agents    for every joined agent: how it joins, wakes, and what to paste
            into its standing instructions
  recipes   step by step: invite, join and wake setup for each kind of agent,
            plus hosting the relay

Onboarding only reads the roster. It never invites, joins, or removes agents;
run "tincan invite <name>" from an admin device for that. Works from any
joined agent or admin device, and on the relay host with no flags (when no
relay is saved or given, its local admin socket is used). --offline skips the roster
(no network) and prints the operator prompt and recipes, for setting up
before anyone joins.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			overrides, err := parseKinds(kinds)
			if err != nil {
				return err
			}
			cfg, err := client.LoadConfig()
			if err != nil {
				return err
			}
			if relayURL != "" {
				cfg.Relay = relayURL
			}
			o := onboard.Options{RelayURL: cfg.Relay, Owner: owner, Operator: operator, KindOverrides: overrides, Offline: offline}
			var roster mcpserver.Roster
			if !offline {
				r, relay, err := onboardRoster(cmd.Context(), socket, cfg)
				if err != nil {
					return err
				}
				roster, o.RelayURL = r, relay
			}
			k, err := mcpserver.Onboard(cmd.Context(), roster, o, section)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd, k)
			}
			cmd.Print(onboard.Render(k, section))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the kit as JSON (the same structure the MCP onboard tool returns)")
	cmd.Flags().StringVar(&section, "section", "all", "which part to print: "+strings.Join(onboard.Sections, ", "))
	cmd.Flags().StringVar(&operator, "operator", "", "the agent that runs the Agent Tincan operator prompt (for example grokbot)")
	cmd.Flags().StringVar(&owner, "owner", "", "the person who owns the team, used in the generated text")
	cmd.Flags().StringArrayVar(&kinds, "kind", nil, "name=kind to tailor an agent's block, overriding its stored kind (repeatable; kinds: "+strings.Join(onboard.Kinds, ", ")+")")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL to read the roster from and print in the kit (default: saved config)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket to read the roster through (default on the relay host: its own)")
	cmd.Flags().BoolVar(&offline, "offline", false, "skip the roster and make no network call: operator prompt and recipes only")
	return cmd
}

// onboardRoster picks where the roster is read from, and the relay URL the
// kit prints: --socket, else the saved config or --relay (already in cfg),
// else, on the relay host, its local admin socket, which is how the admin
// commands work there too. Over a socket the kit names the relay by the
// address the relay advertises to agents, since the socket's own address
// means nothing to them.
func onboardRoster(ctx context.Context, socket string, cfg client.Config) (mcpserver.Roster, string, error) {
	if socket == "" && cfg.Relay == "" {
		socket = localAdminSocket()
	}
	if socket == "" {
		if cfg.Relay == "" {
			return nil, "", errors.New("no relay configured: pass --relay <url>, run `tincan join <code> --relay <url>`, or use --offline to print the operator prompt and recipes without the roster")
		}
		r, err := client.NewRelayFor(cfg)
		return r, cfg.Relay, err
	}
	r := client.NewRelaySocket(socket)
	return r, advertisedRelayURL(ctx, r), nil
}

// advertisedRelayURL asks the relay behind an admin socket where agents
// reach it, falling back to a placeholder the kit's reader fills in.
func advertisedRelayURL(ctx context.Context, r *client.Relay) string {
	var urls struct {
		RelayURLs []string `json:"relay_urls"`
	}
	if r.Raw(ctx, "GET", "/v1/admin/urls", nil, &urls) == nil && len(urls.RelayURLs) > 0 {
		return urls.RelayURLs[0]
	}
	return "<relay URL>"
}

// parseKinds turns repeated name=kind flags into overrides. onboard.Build
// checks the kinds themselves.
func parseKinds(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		name, kind, ok := strings.Cut(p, "=")
		if !ok || name == "" || kind == "" {
			return nil, fmt.Errorf("--kind %q: want name=kind, for example --kind muse=proxy-sandbox", p)
		}
		out[name] = kind
	}
	return out, nil
}

func kindCmd() *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "kind <name> <kind>",
		Short: "Set an agent's kind so onboarding tailors its block (admin devices only)",
		Long: `Record which runtime an agent is, so "tincan onboard" gives it the right
join, wake, and instructions block. Pass "" as the kind to clear it.

Kinds: ` + strings.Join(onboard.Kinds, ", "),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			if err := r.SetKind(cmd.Context(), args[0], args[1]); err != nil {
				return olderRelayKind(err, args[0], args[1], false)
			}
			if args[1] == "" {
				cmd.Printf("Cleared the kind of %q.\n", args[0])
				return nil
			}
			cmd.Printf("%q is now kind %s.\n", args[0], args[1])
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	return cmd
}

func goodAtCmd() *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "good-at <name> <line>",
		Short: "Set the line saying what an agent is good at (admin devices only)",
		Long: `Record one short line saying what an agent is good at, shown on its entry in
"tincan agents" and the list_agents tool so teammates can pick the right one.
Pass "" as the line to clear it. Services and product tools (Claude Code, Codex,
Gemini CLI, Grok CLI, the ChatGPT connector) then show their default line;
hosting shapes, Hermes and OpenClaw show none.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			stored, err := r.SetGoodAt(cmd.Context(), args[0], args[1])
			if err != nil {
				return olderRelayGoodAt(err)
			}
			if stored == "" {
				cmd.Printf("Cleared the good-at line of %q.\n", args[0])
				return nil
			}
			cmd.Printf("%q is now good at: %s\n", args[0], stored)
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	return cmd
}
