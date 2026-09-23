package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func rejoinCmd() *cobra.Command {
	var relayURL, proxy, name string
	cmd := &cobra.Command{
		Use:   "rejoin",
		Short: "Reconnect this machine to the relay after a rebuild or a lost config (no invite needed)",
		Long: `Reconnect this machine to the relay and save the config.

A rebuilt machine (for example a sandbox recreated from scratch) comes back as
a new Tailscale node. When it keeps its machine name and owner, and its old
node is offline or gone, the relay re-admits it as the same agent on the
first call. rejoin makes that call, confirms who the relay says this machine
is, and saves the relay, proxy, and agent name to the config (TINCAN_CONFIG
when set).

Only a machine that was never joined needs a first-time invite from an admin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _ := client.LoadConfig()
			if relayURL != "" {
				cfg.Relay = relayURL
			}
			if cmd.Flags().Changed("proxy") {
				cfg.Proxy = proxy
			}
			if cmd.Flags().Changed("name") {
				cfg.Agent = name
			}
			if cfg.Relay == "" {
				return errors.New("--relay is required (for example http://tincan-relay)")
			}
			r, err := client.NewRelayFor(cfg)
			if err != nil {
				return err
			}
			me, err := r.WhoAmI(cmd.Context())
			if err != nil {
				return rejoinError(err, cfg)
			}
			cfg.Agent = me.Name
			if err := client.SaveConfig(cfg); err != nil {
				return err
			}
			kind := ""
			if me.Kind != "" {
				kind = " (kind " + me.Kind + ")"
			}
			cmd.Printf("Rejoined as %q%s. Config saved to %s\n", me.Name, kind, client.ConfigPath())
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL, e.g. http://tincan-relay (default: saved config)")
	cmd.Flags().StringVar(&proxy, "proxy", "", "proxy for relay traffic (for sandboxes whose default proxy cannot reach the tailnet)")
	cmd.Flags().StringVar(&name, "name", "", "the agent to rejoin as, when this machine ran several")
	return cmd
}

// oldNodeOnlineText is the part of the identity rebind refusal that says the
// agent's old node is still online. It crosses HTTP as text, so there is no
// sentinel to match with errors.Is.
const oldNodeOnlineText = ", which is online: "

// rejoinError explains a failed rejoin. Not joined is the one case that
// needs a person, unless the relay says the old machine is still online.
func rejoinError(err error, cfg client.Config) error {
	if !client.IsNotJoined(err) {
		return err
	}
	if strings.Contains(err.Error(), oldNodeOnlineText) {
		return fmt.Errorf("%w\nThe relay still sees this agent's old machine online. Shut the old machine down or wait for it to go offline, then run tincan rejoin again", err)
	}
	as := ""
	if cfg.Agent != "" {
		as = fmt.Sprintf(" as %q", cfg.Agent)
	}
	return fmt.Errorf("%w\nThis machine has never joined %s%s, so it needs a first-time invite from an admin. "+
		"Ask an admin to run `tincan invite <name>` on an admin device, then run `tincan join <code> --relay %s` here. "+
		"This is the only case that needs a person", err, cfg.Relay, as, cfg.Relay)
}
