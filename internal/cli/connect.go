package cli

import (
	"errors"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

// connectSteps are the steps tincan connect prints around the gateway URL
// and the one-time code, by the product the agent's name is. Only the
// product names themselves (chatgpt, sesame) get product steps; any other
// name gets neutral steps, whatever kind it has or is later given, because
// the name alone does not say which app will add the URL.
var connectSteps = map[string]string{
	onboard.KindChatGPT: `%q is ready to connect.
1. In ChatGPT: Settings > Apps > Advanced settings, turn on Developer mode.
2. Create a connector with this URL:
     %s
3. When ChatGPT opens the login page, enter this one-time code (valid 10 minutes):
     %s
`,
	onboard.KindSesame: `%q is ready to connect.
1. In Sesame: Apps > Add custom app. Name it Agent Tincan, enter this URL and save:
     %s
2. Choose Continue to authorization. When the login page opens, enter this one-time code (valid 10 minutes):
     %s
`,
}

const neutralConnectSteps = `%q is ready to connect.
1. In the agent's app, add a remote MCP server (a custom app or connector) with this URL:
     %s
2. When the app opens the login page, enter this one-time code (valid 10 minutes):
     %s
`

func connectCmd() *cobra.Command {
	var socket, relayURL, guestFor string
	var replace, guest bool
	var guestAsk []string
	cmd := &cobra.Command{
		Use:   "connect <name>",
		Short: "Connect a cloud agent that cannot join the tailnet (ChatGPT, Sesame) through the gateway, or with --guest an A2A client (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !guest && (len(guestAsk) > 0 || guestFor != "") {
				return errors.New("--ask and --for need --guest")
			}
			if guest && replace {
				return errors.New("--guest cannot --replace: a guest takes only a free name")
			}
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			if guest {
				terms, err := guestInvite("", guestAsk, guestFor, false)
				if err != nil {
					return err
				}
				out, err := r.ConnectGuest(cmd.Context(), args[0], terms)
				if err != nil {
					return err
				}
				printGuestConnected(cmd, args[0], out)
				return nil
			}
			var out struct {
				Code string `json:"code"`
				URL  string `json:"url"`
			}
			in := struct {
				Name    string `json:"name"`
				Replace bool   `json:"replace,omitempty"`
			}{Name: args[0], Replace: replace}
			if err := r.Raw(cmd.Context(), "POST", "/v1/admin/connect", in, &out); err != nil {
				return err
			}
			steps, ok := connectSteps[onboard.RuntimeKind(args[0])]
			if !ok {
				steps = neutralConnectSteps
			}
			cmd.Printf(steps, args[0], out.URL, out.Code)
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().BoolVar(&replace, "replace", false, "take over a name held by an agent joined from a tailnet machine")
	cmd.Flags().BoolVar(&guest, "guest", false, "connect an A2A client as a time-limited guest, off until a2a.json publishes a teammate")
	cmd.Flags().StringSliceVar(&guestAsk, "ask", nil, "with --guest: the teammates the guest may ask")
	cmd.Flags().StringVar(&guestFor, "for", "", "with --guest: how long the guest lasts, such as 72h or 30d (at most 30 days)")
	return cmd
}

// printGuestConnected prints what to hand an A2A client and what the owner
// still has to do.
func printGuestConnected(cmd *cobra.Command, name string, out client.GuestConnected) {
	cmd.Printf("%q is ready to connect over A2A until %s, and may ask: %s.\n", name, out.GuestUntil.Local().Format(time.RFC1123), strings.Join(out.GuestAsks, ", "))
	cmd.Printf("1. Give the A2A client the agent card of the teammate it should talk to:\n")
	for _, card := range out.Cards {
		cmd.Printf("     %s\n", card)
	}
	cmd.Printf("2. When the client opens the login page, enter this one-time code (valid 10 minutes):\n     %s\n\n", out.Code)
	cmd.Printf("Its asks wait for you in tincan held.\n")
	if len(out.Unpublished) > 0 {
		cmd.Printf("Warning: a2a.json does not publish %s, so that card answers 404 until you add it.\n", strings.Join(out.Unpublished, ", "))
	}
	if !out.HoldAlerts {
		cmd.Printf("Warning: nothing will tell you when a guest's ask is held. Set notify in approval.json or subscribe a phone to push, or check tincan held yourself.\n")
	}
}
