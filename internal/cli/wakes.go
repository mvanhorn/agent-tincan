package cli

import (
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/relay"
)

func wakesCmd() *cobra.Command {
	var socket, relayURL, since, until string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "wakes <agent> --since <time>",
		Short: "Show each wake the relay sent an agent, what it answered, and when the agent next polled (admin)",
		Long: `Show each wake the relay sent an agent in a time window: when, the wake
path, the HTTP status, the endpoint's reply summary, when the agent next
polled before the following wake, and the requests the wake covered. Times are RFC 3339, "2006-01-02 15:04"
or "2006-01-02" in local time, or a duration ago such as 2h. --until
defaults to now.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			now := time.Now()
			from, err := parseWhen(since, now)
			if err != nil {
				return fmt.Errorf("--since: %w", err)
			}
			q := url.Values{"since": {from.Format(time.RFC3339Nano)}}
			if until != "" {
				to, err := parseWhen(until, now)
				if err != nil {
					return fmt.Errorf("--until: %w", err)
				}
				q.Set("until", to.Format(time.RFC3339Nano))
			}
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			var out relay.WakeExport
			if err := r.Raw(cmd.Context(), "GET", "/v1/admin/wakes/"+url.PathEscape(args[0])+"?"+q.Encode(), nil, &out); err != nil {
				return err
			}
			if out.Wakes == nil {
				out.Wakes = []relay.WakeEntry{}
			}
			if asJSON {
				return printJSON(cmd, out)
			}
			cmd.Print(formatWakes(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "start of the window (required)")
	cmd.Flags().StringVar(&until, "until", "", "end of the window (default now)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	_ = cmd.MarkFlagRequired("since")
	return cmd
}

// parseWhen reads an RFC 3339 time, a local "2006-01-02 15:04" or
// "2006-01-02", or a duration before now.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a time (use RFC 3339, \"2006-01-02 15:04\", or a duration such as 2h)", s)
}

// wakeTime is how the table shows a time: local, to the second.
func wakeTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05 MST") }

func formatWakes(out relay.WakeExport) string {
	var b strings.Builder
	if len(out.Wakes) == 0 {
		fmt.Fprintf(&b, "No wakes for %s between %s and %s.\n", out.Agent, wakeTime(out.Since), wakeTime(out.Until))
		return b.String()
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tEVENT\tPATH\tHTTP\tREPLY\tNEXT POLL\tERROR\tREQUEST IDS")
	for _, e := range out.Wakes {
		next := "no poll"
		if e.NextPoll != nil {
			next = wakeTime(*e.NextPoll)
			if e.NextVia != "" && e.NextVia != "poll" {
				next += " (" + e.NextVia + ")"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", wakeTime(e.At), e.Event, dash(e.Path), dash(e.Status), dash(oneLine(e.Reply)), next, dash(oneLine(e.Error)), dash(strings.Join(e.RequestIDs, ",")))
	}
	_ = tw.Flush()
	if out.Truncated {
		b.WriteString("More wakes than one export holds: these are the oldest. Run again with a later --since for the rest.\n")
	}
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
