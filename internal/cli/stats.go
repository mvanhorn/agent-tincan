package cli

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func statsCmd() *cobra.Command {
	var relayURL, socket, since string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stats [agent]",
		Short: "Show each teammate's answered, failed and expired asks and its median reply time",
		Long: `Show what happened to the asks sent to each teammate in a window (the last 7
days by default): how many it answered, how many failed and how many expired,
and its median reply time over the answered ones. Pings, notifies, declines,
cancels, open requests and asks that expired waiting on the asker's answer to
a clarification do not count. Time a request waited for the owner's approval
does not count against a teammate. It shows counts and durations, never text,
and the relay refreshes them at most once a minute and reads at most 8 windows
other than 7 days a minute.

Works from any joined agent, or from an admin device or the relay host as
tincan agents does.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			out, err := r.Stats(cmd.Context(), since)
			if err != nil {
				return olderRelayStats(err)
			}
			if len(args) == 1 {
				out.Agents = slices.DeleteFunc(out.Agents, func(a client.AgentStats) bool { return a.Agent != args[0] })
				if len(out.Agents) == 0 {
					return fmt.Errorf("no joined agent named %s", args[0])
				}
			}
			if asJSON {
				return printJSON(cmd, out)
			}
			cmd.Print(formatStats(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "7d", "how far back to look, such as 7d or 12h")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config or TINCAN_RELAY)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	return cmd
}

// olderRelayStats turns an older relay's plain "404 page not found" for the
// stats route into an upgrade hint.
func olderRelayStats(err error) error {
	apiErr, ok := errors.AsType[*client.APIError](err)
	if !ok || apiErr.Code != http.StatusNotFound || !strings.Contains(apiErr.Message, "page not found") {
		return err
	}
	return fmt.Errorf("%w. The relay is older than this tincan and has no stats yet: upgrade the relay to this release and restart it", err)
}

func formatStats(out client.StatsReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Asks finished since %s\n", wakeTime(out.Since))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tANSWERED\tFAILED\tEXPIRED\tMEDIAN")
	for _, a := range out.Agents {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\n", a.Agent, a.Answered, a.Failed, a.Expired, dash(a.MedianReply()))
	}
	_ = tw.Flush()
	return b.String()
}
