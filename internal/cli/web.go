package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/history"
)

func webCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Run ChatGPT or Claude as a teammate through your logged-in browser",
		Long: "A web agent makes chatgpt.com or claude.ai a teammate: a request's text is typed into your logged-in\n" +
			"site in a background tab the Tincan Chrome extension opens, and the reply comes back as the answer,\n" +
			"with generated images attached. It acts as you there, and the chats show up in your history.\n" +
			"See docs/adapters/web-agents.md.",
	}
	cmd.AddCommand(webServeCmd(), webInstallCmd())
	return cmd
}

func webSiteFlag(site string) (history.Source, error) {
	if site == "" {
		return "", errors.New("--site is required (chatgpt or claude-ai)")
	}
	return history.ParseWebSite(site)
}

// webConfigPath is a web agent's own client config: TINCAN_CONFIG when set,
// else ~/.config/tincan/<agent>.json.
func webConfigPath(agent string) string {
	if os.Getenv("TINCAN_CONFIG") != "" {
		return client.ConfigPath()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return client.ConfigPath()
	}
	return filepath.Join(home, ".config", "tincan", agent+".json")
}

func webServeCmd() *cobra.Command {
	var site, configPath, allowPath, statePath, name string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run a web agent: answer teammates by asking ChatGPT or Claude in your browser",
		Long: "Long-polls the relay as the web agent and handles one request at a time:\n" +
			"  1. with an allowlist file of names, every agent in the request's relay-set chain must be listed, or the request is declined;\n" +
			"     with no file (or a * entry) any joined agent may ask;\n" +
			"  2. the body is the message. A first line \"new chat\" or \"conversation: <id>\" picks the conversation;\n" +
			"     otherwise it continues the one this asker used last (remembered in a 0600 state file);\n" +
			"  3. the Tincan Chrome extension types it into the site in a background tab and sends it; the service then reads the conversation until the reply is finished;\n" +
			"  4. the reply text (capped) comes back with generated images as attachments.\n" +
			"Normally started by the service definition tincan web install writes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, err := webSiteFlag(site)
			if err != nil {
				return err
			}
			if name == "" {
				name = history.WebAgentName(src)
			}
			if configPath == "" {
				configPath = webConfigPath(name)
			}
			configPath = expandHome(configPath)
			cfg, err := client.LoadConfigFrom(configPath)
			if err != nil {
				return fmt.Errorf("%s config %s: %w", name, configPath, err)
			}
			if cfg.Relay == "" {
				return fmt.Errorf("no relay configured in %s: run TINCAN_CONFIG=%s tincan join <code> --relay http://tincan-relay", configPath, configPath)
			}
			if cfg.Agent != "" && cfg.Agent != name {
				return wrongWebAgent(name, configPath+" is joined as", cfg.Agent)
			}
			if allowPath == "" {
				allowPath = history.DefaultWebAllowlistPath(name)
			}
			allowPath = expandHome(allowPath)
			allowed, err := history.LoadAllowlist(allowPath)
			if err != nil {
				return fmt.Errorf("%s allowlist: %w", name, err)
			}
			if statePath == "" {
				statePath = history.DefaultWebStatePath(name)
			}
			r, err := client.NewRelayFor(cfg)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			me, err := r.WhoAmI(ctx)
			if err != nil {
				return fmt.Errorf("web serve: could not confirm this agent's identity with the relay: %w", client.RejoinHint(err, cfg.Relay))
			}
			if me.Name != name {
				return wrongWebAgent(name, "the relay knows the machine using "+configPath+" as", me.Name)
			}
			agent := &history.WebAgent{
				Relay:       r,
				Site:        src,
				Name:        name,
				Native:      history.NewClient(),
				Allowlist:   history.FileAllowlist(allowPath),
				StatePath:   expandHome(statePath),
				JournalPath: history.DefaultWebJournalPath(name),
				Log:         cmd.ErrOrStderr(),
			}
			cmd.PrintErrf("tincan web %s: serving %s on %s (%s)\n", name, src, cfg.Relay, history.DescribeAllowlist(allowPath, allowed))
			err = agent.Run(ctx)
			if ctx.Err() != nil {
				cmd.PrintErrf("tincan web %s: stopped\n", name)
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&site, "site", "", "chatgpt or claude-ai")
	cmd.Flags().StringVar(&name, "name", "", "this agent's name (default chatgpt-web or claude-web)")
	cmd.Flags().StringVar(&configPath, "config", "", "the agent's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/<name>.json)")
	cmd.Flags().StringVar(&allowPath, "allowlist", "", "file of agents allowed to ask, one per line (default ~/.config/tincan/<name>-allow.txt; missing or * means every joined agent)")
	cmd.Flags().StringVar(&statePath, "state", "", "where each asker's last conversation id is kept (default ~/.config/tincan/<name>-state.json)")
	return cmd
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// wrongWebAgent is the refusal when web serve would run as another agent
// and so poll and claim that agent's requests.
func wrongWebAgent(name, who, agent string) error {
	return fmt.Errorf("web serve refuses to run: %s %q, not %q, so it would claim that agent's requests. "+
		"Point --config (or TINCAN_CONFIG) at %s's own config, normally ~/.config/tincan/%s.json "+
		"(join it with TINCAN_CONFIG=~/.config/tincan/%s.json tincan join <code> --relay <relay url>)", who, agent, name, name, name, name)
}

func webInstallCmd() *cobra.Command {
	var site, binary string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the service definition for a web agent (never starts it)",
		Long: "Writes a launchd agent on macOS (a systemd user unit on Linux) that runs tincan web serve --site <site>\n" +
			"with TINCAN_CONFIG=~/.config/tincan/<agent>.json, and prints the command that starts it. The Tincan\n" +
			"Chrome extension and its native host come from tincan history install.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, err := webSiteFlag(site)
			if err != nil {
				return err
			}
			res, err := history.InstallWebService(src, history.ServiceOptions{Binary: binary})
			if err != nil {
				return err
			}
			agent := history.WebAgentName(src)
			cmd.Printf("%s service definition: %s (not started)\n", agent, res.Path)
			cmd.Printf("join it first (if not yet):\n  TINCAN_CONFIG=~/.config/tincan/%s.json tincan join <code> --relay <relay url>\n", agent)
			cmd.Printf("start it with:\n  %s\n", res.Next)
			return nil
		},
	}
	cmd.Flags().StringVar(&site, "site", "", "chatgpt or claude-ai")
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	return cmd
}
