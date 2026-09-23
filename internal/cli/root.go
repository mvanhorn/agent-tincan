// Package cli holds the tincan commands.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// Version is set by main at link time.
var Version = "0.0.1-dev"

// Root returns the tincan command tree.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "tincan",
		Short:         "Let AI agents on one tailnet ask each other to do things",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(relayCmd(), versionCmd())
	root.AddCommand(agentCmds()...)
	root.AddCommand(mcpCmd(), traceCmd(), auditCmd(), listenCmd(), connectCmd(), onboardCmd(), rejoinCmd())
	withRejoinHints(root)
	return root
}

// withRejoinHints makes every client command that fails with "not a joined
// agent" or "no relay configured" suggest tincan rejoin, so an agent on a
// rebuilt machine heals itself instead of asking a person. rejoin explains
// its own failures and the relay is not a client.
func withRejoinHints(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		withRejoinHints(c)
	}
	if cmd.RunE == nil || cmd.Name() == "rejoin" || cmd.Name() == "relay" {
		return
	}
	run := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		err := run(c, args)
		if err == nil {
			return nil
		}
		cfg, _ := client.LoadConfig()
		return client.RejoinHint(err, cfg.Relay)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tincan version",
		Run:   func(cmd *cobra.Command, _ []string) { cmd.Println(Version) },
	}
}
