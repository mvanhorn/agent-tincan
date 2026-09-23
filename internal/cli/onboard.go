package cli

import (
	"encoding/json"
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
	var section, operator, owner, relayURL string
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
joined agent or admin device. --offline skips the roster (no network) and
prints the operator prompt and recipes, for setting up before anyone joins.`,
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
				if cfg.Relay == "" {
					return errors.New("no relay configured: pass --relay <url>, run `tincan join <code> --relay <url>`, or use --offline to print the operator prompt and recipes without the roster")
				}
				r, err := client.NewRelayFor(cfg)
				if err != nil {
					return err
				}
				roster = r
			}
			k, err := mcpserver.Onboard(cmd.Context(), roster, o, section)
			if err != nil {
				return err
			}
			if asJSON {
				raw, err := json.MarshalIndent(k, "", "  ")
				if err != nil {
					return err
				}
				cmd.Println(string(raw))
				return nil
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
	cmd.Flags().BoolVar(&offline, "offline", false, "skip the roster and make no network call: operator prompt and recipes only")
	return cmd
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
				return err
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
