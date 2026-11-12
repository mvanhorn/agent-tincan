package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// fallbackCmd is tincan fallback: the owner's per-agent choice of where an
// ask moves when its target leaves it untaken.
func fallbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fallback",
		Short: "Move asks an agent leaves untaken to another agent (admin devices only)",
		Long: `Name a teammate that takes the asks an agent leaves untaken.

When an ask to an agent that wakes itself (channel, wait, command or none)
sits queued and never claimed past the relay's notice grace (--wake-grace,
default 10 minutes; --urgent-wake-grace for an urgent ask), the relay moves
it once to the agent's fallback, if that agent is online and the move passes
the approval gate and guest rules. The ask keeps its id, so the answer goes
back to the original asker, and the asker is told it moved. Otherwise the
asker gets the usual note that the ask is untaken.

The common setup is a Claude Code agent whose channel only reaches idle
sessions, with a window agent that opens a new window per ask:

  tincan fallback set claude-code claude-window

"tincan agents" shows each agent's fallback.`,
	}
	cmd.AddCommand(fallbackSetCmd("set <agent> <fallback>", "Set the agent that takes asks <agent> leaves untaken", 2),
		fallbackSetCmd("clear <agent>", "Stop moving untaken asks off <agent>", 1))
	return cmd
}

func fallbackSetCmd(use, short string, nargs int) *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(nargs),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			to := ""
			if len(args) == 2 {
				to = args[1]
				if to == "" {
					return errors.New(`name the fallback agent, or use tincan fallback clear ` + args[0])
				}
			}
			if err := r.SetFallback(cmd.Context(), args[0], to); err != nil {
				return olderRelayFallback(err)
			}
			if to == "" {
				cmd.Printf("Asks %q leaves untaken are no longer moved.\n", args[0])
				return nil
			}
			cmd.Printf("Asks %q leaves untaken now move to %q.\n", args[0], to)
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	return cmd
}

// olderRelayFallback turns an older relay's plain "404 page not found" for
// the fallback route into an upgrade hint. The route's own 404, for an agent
// the relay does not know, comes as a JSON error and passes through.
func olderRelayFallback(err error) error {
	apiErr, ok := errors.AsType[*client.APIError](err)
	if !ok || apiErr.Code != http.StatusNotFound || !strings.Contains(apiErr.Message, "page not found") {
		return err
	}
	return fmt.Errorf("%w. The relay is older than this tincan and cannot store fallbacks yet: upgrade the relay to this release and restart it", err)
}
