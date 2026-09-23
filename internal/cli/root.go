// Package cli holds the tincan commands.
package cli

import (
	"github.com/spf13/cobra"
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
	root.AddCommand(mcpCmd(), traceCmd(), auditCmd(), listenCmd(), connectCmd(), onboardCmd())
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tincan version",
		Run:   func(cmd *cobra.Command, _ []string) { cmd.Println(Version) },
	}
}
