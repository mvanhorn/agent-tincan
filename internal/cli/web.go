package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

func webCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Run ChatGPT, Claude, Grok, Gemini, Perplexity or Copilot as a teammate through your logged-in browser",
		Long: "A web agent makes chatgpt.com, claude.ai, grok.com, gemini.google.com, www.perplexity.ai or copilot.com a teammate: a request's text is typed into your logged-in\n" +
			"site in a background tab the Tincan Chrome extension opens, and the reply comes back as the answer,\n" +
			"with generated images attached from ChatGPT and Grok (Gemini's when they can be fetched), and Perplexity's and Copilot's answers ending with their source links.\n" +
			"It acts as you there, and the chats show up in your history.\n" +
			"See docs/adapters/web-agents.md.",
	}
	cmd.AddCommand(webServeCmd(), webInstallCmd())
	return cmd
}

func webSiteFlag(site string) (history.Source, error) {
	if site == "" {
		return "", fmt.Errorf("--site is required (%s)", history.WebSiteNames())
	}
	return history.ParseWebSite(site)
}

// webThreadFlag checks --thread against the site: required and well formed
// for a site whose agent serves one thread (dots), refused for the others.
func webThreadFlag(src history.Source, thread string) (string, error) {
	if !history.WebSiteTakesThread(src) {
		if thread != "" {
			return "", fmt.Errorf("--thread applies only to --site dots, not %s", src)
		}
		return "", nil
	}
	id, ok := history.ParseWebThread(src, thread)
	if !ok {
		if thread == "" {
			return "", fmt.Errorf("--site %s needs --thread <id>, the id in https://chatgpt.com/dots/<id> when you open the dot's DM", src)
		}
		return "", fmt.Errorf("--thread %q is not a dot thread id (hex and hyphens, the id in https://chatgpt.com/dots/<id>)", thread)
	}
	return id, nil
}

// serviceConfigPath is a service agent's own client config (web agents,
// notes): TINCAN_CONFIG when set, else ~/.config/tincan/<agent>.json.
func serviceConfigPath(agent string) string {
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
	var site, configPath, allowPath, statePath, name, thread string
	var teach bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run a web agent: answer teammates by asking ChatGPT, Claude, Grok, Gemini, Perplexity or Copilot in your browser",
		Long: "Long-polls the relay as the web agent and handles one request at a time:\n" +
			"  1. with an allowlist file of names, every agent in the request's relay-set chain must be listed, or the request is declined;\n" +
			"     with no file (or a * entry) any joined agent may ask;\n" +
			"  2. the body is the message. A first line \"new chat\" or \"conversation: <id>\" picks the conversation;\n" +
			"     otherwise it continues the one this asker used last (remembered in a 0600 state file).\n" +
			"     --site dots (your OpenAI dot) needs --thread <id>: every request goes into that DM, sent as is;\n" +
			"     the dot can ask teammates too: a dot message whose first line is \"@tincan ask <agent>\" is asked\n" +
			"     (agents listed in ~/.config/tincan/<name>-send.txt; no file means any joined agent) and the answer typed back;\n" +
			"  3. the Tincan Chrome extension types it into the site in a background tab and sends it; the service then reads the conversation until the reply is finished;\n" +
			"  4. the reply text (capped) comes back; ChatGPT and Grok attach their generated images, Gemini's when they can be fetched,\n" +
			"     and Perplexity's and Copilot's answers end with their source links.\n" +
			"Normally started by the service definition tincan web install writes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, err := webSiteFlag(site)
			if err != nil {
				return err
			}
			thread, err := webThreadFlag(src, thread)
			if err != nil {
				return err
			}
			if teach && !history.WebSiteTakesThread(src) {
				return fmt.Errorf("--teach applies only to --site dots, not %s", src)
			}
			if name == "" {
				name = history.WebAgentName(src)
			}
			if configPath == "" {
				configPath = serviceConfigPath(name)
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
				return wrongServeAgent("web serve", name, configPath+" is joined as", cfg.Agent)
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
			// The service's own config file, not ConfigPath(), is where the
			// relay key and a moved relay's address are saved.
			r, err := client.NewRelayForFile(cfg, configPath)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if client.NeedsRelayInfo(cfg) {
				// Nothing else runs as this agent, so the service learns the
				// relay key itself; without it a moved relay is never found.
				learnRelayKeyWithin(ctx, r)
			}
			me, err := r.WhoAmI(ctx)
			if err != nil {
				return fmt.Errorf("web serve: could not confirm this agent's identity with the relay: %w", client.RejoinHint(err, cfg.Relay))
			}
			if me.Name != name {
				return wrongServeAgent("web serve", name, "the relay knows the machine using "+configPath+" as", me.Name)
			}
			// The relay holds a request to the dot for the owner's approval
			// only when it knows the agent's kind is dot-web; under any other
			// kind (an invite without --kind leaves it empty) teammates' asks
			// would reach the dot unapproved, so refuse to start.
			if history.WebSiteTakesThread(src) && me.Kind != onboard.KindDotWeb {
				kind := me.Kind
				if kind == "" {
					kind = "none"
				}
				return fmt.Errorf("web serve: %s's kind on the relay is %q, so requests to it would not be held for the owner's approval; run `tincan kind %s %s` from an admin device (the relay must run this tincan build or later to accept the kind) and start again", name, kind, name, onboard.KindDotWeb)
			}
			// A connected extension that reports the site ungranted cannot
			// serve it, so the service waits here for the grant rather than
			// exiting (the service manager would restart it every few
			// seconds, forever). With no extension connected (launchd starts
			// this at login, often before Chrome) it starts, and each request
			// gets the matching reply.
			native := history.NewClient()
			if err := waitForSiteGrant(ctx, native, src, cmd.ErrOrStderr(), name); err != nil {
				cmd.PrintErrf("tincan web %s: stopped\n", name)
				return nil
			}
			agent := &history.WebAgent{
				Relay:       r,
				Site:        src,
				Name:        name,
				Native:      native,
				Allowlist:   history.FileAllowlist(allowPath),
				StatePath:   expandHome(statePath),
				Thread:      thread,
				JournalPath: history.DefaultWebJournalPath(name),
				UsedPath:    history.DefaultWebUsedPath(src),
				Log:         cmd.ErrOrStderr(),
			}
			cmd.PrintErrf("tincan web %s: serving %s on %s (%s)\n", name, src, cfg.Relay, history.DescribeAllowlist(allowPath, allowed))
			if history.WebSiteTakesThread(src) {
				// The dot asks teammates by writing "@tincan ask <agent>" in
				// its DM; the watcher asks them and types the answers back.
				sendPath := history.DefaultDotSendAllowlistPath(name)
				agent.OutPath = history.DefaultDotOutPath(name)
				agent.SendAllowlist = history.FileAllowlist(sendPath)
				agent.SendAllowlistPath = sendPath
				agent.Teach = teach
				if sendAllowed, err := history.LoadAllowlist(sendPath); err != nil {
					cmd.PrintErrf("tincan web %s: send allowlist: %v; the dot's asks are refused until it is fixed\n", name, err)
				} else {
					cmd.PrintErrf("tincan web %s: watching the dot's DM for @tincan asks (send %s)\n", name, history.DescribeAllowlist(sendPath, sendAllowed))
				}
			}
			err = agent.Run(ctx)
			if ctx.Err() != nil {
				cmd.PrintErrf("tincan web %s: stopped\n", name)
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&site, "site", "", history.WebSiteNames())
	cmd.Flags().StringVar(&name, "name", "", "this agent's name (default "+history.WebAgentNames()+")")
	cmd.Flags().StringVar(&configPath, "config", "", "the agent's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/<name>.json)")
	cmd.Flags().StringVar(&allowPath, "allowlist", "", "file of agents allowed to ask, one per line (default ~/.config/tincan/<name>-allow.txt; missing or * means every joined agent)")
	cmd.Flags().StringVar(&statePath, "state", "", "where each asker's last conversation id is kept (default ~/.config/tincan/<name>-state.json)")
	cmd.Flags().StringVar(&thread, "thread", "", "--site dots only (required there): the dot's thread id, the <id> in https://chatgpt.com/dots/<id>")
	cmd.Flags().BoolVar(&teach, "teach", false, "--site dots only: type the setup message that teaches the dot to ask teammates into its DM again (it is sent once per thread on its own)")
	return cmd
}

// webGrantRecheck is how often web serve asks the extension again while
// it waits for a missing site grant. Tests shorten it.
var webGrantRecheck = time.Minute

// waitForSiteGrant returns once src can be served: at once when the
// extension grants it or no extension answers, else after logging the
// missing grant once (naming the options page) and asking again every
// webGrantRecheck until it is granted or the extension goes away. It
// fails only when ctx ends first.
func waitForSiteGrant(ctx context.Context, native *history.Client, src history.Source, log io.Writer, name string) error {
	err := native.CheckSiteGrant(ctx, src)
	if err == nil {
		return nil
	}
	fmt.Fprintf(log, "tincan web %s: waiting for the site grant, checking every %s: %v\n", name, webGrantRecheck, err)
	t := time.NewTicker(webGrantRecheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		if native.CheckSiteGrant(ctx, src) == nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(log, "tincan web %s: %s is granted (or no extension is connected); starting\n", name, src)
			return nil
		}
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// wrongServeAgent is the refusal when a service's serve command (web serve,
// notes serve) would run as another agent and so poll and claim that
// agent's requests.
func wrongServeAgent(command, name, who, agent string) error {
	return fmt.Errorf("%s refuses to run: %s %q, not %q, so it would claim that agent's requests. "+
		"Point --config (or TINCAN_CONFIG) at %s's own config, normally ~/.config/tincan/%s.json "+
		"(join it with TINCAN_CONFIG=~/.config/tincan/%s.json tincan join <code> --relay <relay url>)", command, who, agent, name, name, name, name)
}

func webInstallCmd() *cobra.Command {
	var site, binary, thread string
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
			thread, err := webThreadFlag(src, thread)
			if err != nil {
				return err
			}
			res, err := history.InstallWebService(src, history.ServiceOptions{Binary: binary, Thread: thread})
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
	cmd.Flags().StringVar(&site, "site", "", history.WebSiteNames())
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	cmd.Flags().StringVar(&thread, "thread", "", "--site dots only (required there): the dot's thread id, kept in the service definition")
	return cmd
}
