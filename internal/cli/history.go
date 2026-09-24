package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/history"
)

// historyNow is the clock for history reads; tests pin it.
var historyNow = time.Now

// historyReader returns the reader for a source name. The live sources go
// through the Tincan Chrome extension's native host.
func historyReader(source string) (history.Reader, error) {
	switch history.Source(source) {
	case history.SourceChatGPT:
		r := history.NewChatGPT(history.NewClient())
		r.Now = historyNow
		return r, nil
	case history.SourceClaudeAI:
		r := history.NewClaudeAI(history.NewClient())
		r.Now = historyNow
		return r, nil
	case history.SourceCodex:
		r := history.NewCodex()
		r.Now = historyNow
		return r, nil
	case history.SourceClaudeCode:
		r := history.NewClaudeCode()
		r.Now = historyNow
		return r, nil
	}
	return nil, fmt.Errorf("unknown history source %q (want chatgpt, claude-ai, codex or claude-code)", source)
}

func historyCmd() *cobra.Command {
	var list int
	var all, latest, asJSON bool
	var search, id, imagesDir string
	cmd := &cobra.Command{
		Use:   "history <chatgpt|claude-ai|codex|claude-code>",
		Short: "Read the owner's ChatGPT, claude.ai, Codex or Claude Code history",
		Long: "Read the owner's ChatGPT, claude.ai, Codex or Claude Code history. With no mode flag it shows the latest prompt the owner typed.\n" +
			"Unattended runs (codex exec wakes, Claude Code SDK sessions) are left out unless --all is given.\n" +
			"chatgpt and claude-ai are read live through the Tincan Chrome extension and the user's logged-in Chrome; run tincan history install once.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			modes := 0
			if cmd.Flags().Changed("list") {
				modes++
			}
			for _, on := range []bool{latest, search != "", id != ""} {
				if on {
					modes++
				}
			}
			if modes > 1 {
				return errors.New("use only one of --list, --latest, --search, --id")
			}
			r, err := historyReader(args[0])
			if err != nil {
				return err
			}
			opts := history.Options{All: all}
			var convs []history.Conversation
			if cmd.Flags().Changed("list") {
				if list <= 0 || list > 200 {
					return errors.New("--list must be between 1 and 200")
				}
				convs, err = r.List(cmd.Context(), list, opts)
			} else {
				q := history.Query{Source: r.Source(), Mode: history.ModeLatest, WantImages: imagesDir != ""}
				switch {
				case search != "":
					q.Mode, q.Terms = history.ModeSearch, strings.Fields(search)
				case id != "":
					q.Mode, q.ConversationID = history.ModeConversation, id
				}
				if err := q.Validate(); err != nil {
					return err
				}
				convs, err = r.Read(cmd.Context(), q, opts)
				if err == nil && imagesDir != "" {
					err = history.SaveImages(imagesDir, convs)
				}
			}
			if err != nil {
				return err
			}
			if asJSON {
				if convs == nil {
					convs = []history.Conversation{}
				}
				return printJSON(cmd, convs)
			}
			printHistory(cmd, convs, cmd.Flags().Changed("list"))
			return nil
		},
	}
	cmd.Flags().IntVar(&list, "list", 20, "list the N most recent conversations with their working directory")
	cmd.Flags().BoolVar(&all, "all", false, "include unattended runs (codex exec, Claude Code SDK)")
	cmd.Flags().BoolVar(&latest, "latest", false, "show the latest prompt and its reply (default)")
	cmd.Flags().StringVar(&search, "search", "", "find recent conversations whose title or a prompt contains every word")
	cmd.Flags().StringVar(&id, "id", "", "show one conversation by id")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().StringVar(&imagesDir, "images-dir", "", "save the selected turn's images here (created 0700, files 0600)")
	cmd.AddCommand(historyInstallCmd(), historyNativeHostCmd(), historyServeCmd())
	return cmd
}

func historyInstallCmd() *cobra.Command {
	var extID, binary, extDir string
	var noService bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register the native messaging host the Tincan Chrome extension talks to",
		Long: "Writes the Chrome native messaging host manifest for " + history.NativeHostName + " into this user's Chrome\n" +
			"NativeMessagingHosts directory (no admin rights), allowed only for the Tincan extension, pointing at a\n" +
			"wrapper that runs tincan history native-host. On Windows it also prints the registry entry to add.\n" +
			"It also writes the history service definition (a launchd agent on macOS, a systemd user unit on Linux)\n" +
			"that runs tincan history serve, and prints the command that starts it. It never starts it itself.\n" +
			"With --extension-dir (the unpacked extension folder; default ./extension when it is the Tincan extension),\n" +
			"the host reloads the extension whenever the files there change, so updates need no Reload click.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if extDir == "" {
				extDir = detectExtensionDir(extID)
			} else if abs, err := filepath.Abs(extDir); err == nil {
				extDir = abs
			}
			res, err := history.InstallNativeHost(history.InstallOptions{ExtensionID: extID, Binary: binary, ExtensionDir: extDir})
			if err != nil {
				return err
			}
			cmd.Printf("native host manifest: %s\n", res.ManifestPath)
			cmd.Printf("native host wrapper: %s\n", res.WrapperPath)
			if extDir != "" {
				cmd.Printf("unpacked extension: %s (the extension reloads itself when these files change)\n", extDir)
			}
			if res.Note != "" {
				cmd.Println(res.Note)
			}
			if noService {
				return nil
			}
			svc, err := history.InstallService(history.ServiceOptions{Binary: binary})
			if err != nil {
				cmd.Printf("history service: not written: %v\n", err)
				return nil
			}
			cmd.Printf("history service definition: %s (not started)\n", svc.Path)
			cmd.Printf("start it with:\n  %s\n", svc.Next)
			return nil
		},
	}
	cmd.Flags().StringVar(&extID, "extension-id", history.DefaultExtensionID, "Chrome extension id allowed to start the host")
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the host and the service run (default: this executable)")
	cmd.Flags().BoolVar(&noService, "no-service", false, "do not write the history service definition")
	cmd.Flags().StringVar(&extDir, "extension-dir", "", "the unpacked extension folder to keep Chrome in sync with (default ./extension when it holds the Tincan extension)")
	return cmd
}

// detectExtensionDir returns ./extension, absolute, when its manifest key
// gives extID, so running install from a repo checkout wires up automatic
// reloads. Anything else returns "".
func detectExtensionDir(extID string) string {
	b, err := os.ReadFile(filepath.Join("extension", "manifest.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(b, &m) != nil || m.Key == "" {
		return ""
	}
	if id, err := history.ExtensionIDFromKey(m.Key); err != nil || id != extID {
		return ""
	}
	abs, err := filepath.Abs("extension")
	if err != nil {
		return ""
	}
	return abs
}

// defaultHistoryConfig is the history agent's own client config:
// TINCAN_CONFIG when set, else ~/.config/tincan/history.json.
func defaultHistoryConfig() string {
	if os.Getenv("TINCAN_CONFIG") != "" {
		return client.ConfigPath()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return client.ConfigPath()
	}
	return filepath.Join(home, ".config", "tincan", "history.json")
}

func historyServeCmd() *cobra.Command {
	var configPath, allowPath, codexBin string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the history agent: answer teammates' history questions over the relay",
		Long: "Long-polls the relay as the history agent and answers each request itself, in order:\n" +
			"  1. with an allowlist file of names, every agent in the request's relay-set chain must be listed, or the request is declined;\n" +
			"     with no file (or a * entry) any joined agent may ask;\n" +
			"  2. a tool-less codex exec call turns the question text (and only that) into a structured query;\n" +
			"  3. the matching source is read (ChatGPT and claude.ai through the Tincan Chrome extension);\n" +
			"  4. the reply is filled in from a fixed template, with the images attached.\n" +
			"Retrieved chat content is never sent to an LLM. The allowlist file is reread for every request.\n" +
			"Normally started by the service definition tincan history install writes. See docs/adapters/history.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				configPath = defaultHistoryConfig()
			}
			configPath = expandHome(configPath)
			cfg, err := client.LoadConfigFrom(configPath)
			if err != nil {
				return fmt.Errorf("history config %s: %w", configPath, err)
			}
			if cfg.Relay == "" {
				return fmt.Errorf("no relay configured in %s: run TINCAN_CONFIG=%s tincan join <code> --relay http://tincan-relay", configPath, configPath)
			}
			if cfg.Agent != "" && cfg.Agent != "history" {
				return wrongHistoryAgent(configPath+" is joined as", cfg.Agent)
			}
			if allowPath == "" {
				allowPath = history.DefaultAllowlistPath()
			}
			allowed, err := history.LoadAllowlist(allowPath)
			if err != nil {
				return fmt.Errorf("history allowlist: %w", err)
			}
			r, err := client.NewRelayFor(cfg)
			if err != nil {
				return err
			}
			readers := map[history.Source]history.Reader{}
			for _, src := range history.Sources {
				rd, err := historyReader(string(src))
				if err != nil {
					return err
				}
				readers[src] = rd
			}
			ext := history.NewCodexExtractor()
			ext.Binary = codexBin
			svc := &history.Service{
				Relay:     r,
				Extractor: ext,
				Readers:   readers,
				Allowlist: history.FileAllowlist(allowPath),
				Log:       cmd.ErrOrStderr(),
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// The relay, not the config file, says who this machine is. Serving
			// as any other agent would poll and claim that agent's inbox.
			me, err := r.WhoAmI(ctx)
			if err != nil {
				return fmt.Errorf("history serve: could not confirm this agent's identity with the relay: %w", client.RejoinHint(err, cfg.Relay))
			}
			if me.Name != "history" {
				return wrongHistoryAgent("the relay knows the machine using "+configPath+" as", me.Name)
			}
			cmd.PrintErrf("tincan history: serving as %s on %s (%s)\n", me.Name, cfg.Relay, history.DescribeAllowlist(allowPath, allowed))
			err = svc.Run(ctx)
			if ctx.Err() != nil {
				cmd.PrintErrln("tincan history: stopped")
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "the history agent's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/history.json)")
	cmd.Flags().StringVar(&allowPath, "allowlist", "", "file of agents allowed to read history, one per line (default: ~/.config/tincan/history-allow.txt; missing or * means every joined agent)")
	cmd.Flags().StringVar(&codexBin, "codex", "codex", "codex binary used for the tool-less query step")
	return cmd
}

// wrongHistoryAgent is the refusal when history serve would run as another
// agent and so poll and claim that agent's requests.
func wrongHistoryAgent(who, agent string) error {
	return fmt.Errorf("history serve refuses to run: %s %q, not \"history\", so it would claim that agent's requests. "+
		"Point --config (or TINCAN_CONFIG) at the history agent's own config, normally ~/.config/tincan/history.json "+
		"(join it with TINCAN_CONFIG=~/.config/tincan/history.json tincan join <code> --relay <relay url>)", who, agent)
}

func historyNativeHostCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "native-host",
		Short:  "The native messaging host Chrome starts for the Tincan extension (not run by hand)",
		Hidden: true,
		// Chrome passes the extension origin (and on Windows a
		// --parent-window flag); none of it is a tincan flag, and stdout
		// belongs to the native messaging protocol.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			host := &history.NativeHost{
				SocketPath:   history.DefaultSocketPath(),
				In:           os.Stdin,
				Out:          os.Stdout,
				ExtensionDir: os.Getenv(history.ExtensionDirEnv),
				Log:          os.Stderr,
			}
			return host.Run(ctx)
		},
	}
}

func printHistory(cmd *cobra.Command, convs []history.Conversation, listing bool) {
	if len(convs) == 0 {
		cmd.Println("Nothing found.")
		return
	}
	for i, c := range convs {
		if listing {
			cwd := c.Cwd
			if cwd == "" {
				cwd = "-"
			}
			flag := ""
			if c.Automated {
				flag = "  [" + c.Originator + "]"
			}
			cmd.Printf("%s  %s  %s  %s%s\n", c.UpdatedAt.Local().Format("2006-01-02 15:04"), c.ID, cwd, oneLine(c.Title), flag)
			continue
		}
		if i > 0 {
			cmd.Println()
		}
		cmd.Printf("%s  %s  %s\n", c.Source, c.ID, c.Title)
		if c.Cwd != "" {
			cmd.Printf("cwd: %s\n", c.Cwd)
		}
		if c.Automated {
			cmd.Printf("originator: %s (unattended)\n", c.Originator)
		}
		for _, m := range c.Messages {
			ts := ""
			if !m.Time.IsZero() {
				ts = " " + m.Time.Local().Format("2006-01-02 15:04")
			}
			cmd.Printf("[%s%s] %s\n", m.Role, ts, m.Text)
			for _, im := range m.Images {
				cmd.Printf("  image: %s (%s, %d bytes)\n", im.Path, im.MIME, im.Size)
			}
		}
	}
}
