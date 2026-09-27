// Package cli holds the tincan commands.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// Version is set by main at link time.
var Version = "0.0.1-dev"

// Root returns the tincan command tree.
func Root() *cobra.Command {
	client.Version = Version // every relay call names this build
	root := &cobra.Command{
		Use:           "tincan",
		Short:         "Let AI agents on one tailnet ask each other to do things",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Command output goes to stdout so `tincan onboard > kit.txt` and
	// `$(tincan version)` work. Cobra's Print* default to stderr; errors
	// stay there (main prints them to stderr).
	root.SetOut(os.Stdout)
	root.AddCommand(relayCmd(), versionCmd())
	root.AddCommand(agentCmds()...)
	root.AddCommand(mcpCmd(), doctorCmd(), traceCmd(), auditCmd(), listenCmd(), connectCmd(), onboardCmd(), rejoinCmd(), upgradeCmd(), historyCmd(), webCmd(), attachmentCmd())
	withRejoinHints(root)
	return root
}

// ExitError asks main to exit with Code. A Silent one prints nothing more,
// for commands whose stdout already says what happened (a --json outcome).
// Err, when set, is the message main prints for a non-silent one.
type ExitError struct {
	Code   int
	Silent bool
	Err    error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

func (e *ExitError) Unwrap() error { return e.Err }

// ExitStatus is the process exit code for err returned by Root, and whether
// main should print nothing. An ExitError sets its own code, even when
// wrapped; any other error exits 1 with its message.
func ExitStatus(err error) (code int, silent bool) {
	if err == nil {
		return 0, true
	}
	if e, ok := errors.AsType[*ExitError](err); ok {
		return e.Code, e.Silent
	}
	return 1, false
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
		// onboard with no relay already says what to do (--relay, join, or
		// --offline), and is what a fresh admin runs before anyone joins,
		// so a rebuilt-machine hint there is only noise.
		if c.Name() == "onboard" && cfg.Relay == "" && !c.Flags().Changed("relay") {
			return err
		}
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

// printJSON prints v as indented JSON and a newline.
func printJSON(cmd *cobra.Command, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	cmd.Println(string(b))
	return nil
}
