package cli

import (
	"github.com/spf13/cobra"

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
	var socket, relayURL string
	cmd := &cobra.Command{
		Use:   "connect <name>",
		Short: "Connect a cloud agent that cannot join the tailnet (ChatGPT, Sesame) through the gateway (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			var out struct {
				Code string `json:"code"`
				URL  string `json:"url"`
			}
			if err := r.Raw(cmd.Context(), "POST", "/v1/admin/connect", map[string]string{"name": args[0]}, &out); err != nil {
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
	return cmd
}
