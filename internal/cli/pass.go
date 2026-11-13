package cli

import (
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
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/pass"
)

// passReader is the live reader for a chat site; tests replace it.
var passReader = func(src history.Source) (history.Reader, bool) {
	return history.NewLiveReader(src, history.NewClient(), historyNow)
}

// DefaultPassWait is how long tincan pass waits for the destination to
// open the new chat.
const DefaultPassWait = 10 * time.Minute

func passCmd() *cobra.Command {
	var to, id string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "pass <chat-url | source> --to <web agent>",
		Short: "Move one of the owner's chats into a new chat on another web chat teammate",
		Long: "Reads one of the owner's chats through the Tincan Chrome extension (a chat URL on " + history.LiveSourcesLabel() + ",\n" +
			"or a source name for its newest chat, or a source with --id), packs it into one message (its title, the\n" +
			"first line of each earlier prompt and the last 6 turns verbatim, cut to fit the destination's message cap),\n" +
			"and asks the destination web chat teammate, as this agent, to open a new chat with it. It prints the\n" +
			"conversation the destination opened; continue it in the browser.\n\n" +
			"The destination must be one of the six web chat teammates, signed in, not out of messages and running\n" +
			"tincan " + pass.GuardVersion + " or later; anything else is refused before the chat is read. The chat goes to the destination's vendor. See docs/adapters/pass.md.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return errors.New("--to names the web chat teammate to pass the chat to")
			}
			src, convID, err := passSource(args[0], id)
			if err != nil {
				return err
			}
			r, _, err := connect()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			roster, err := r.Agents(ctx)
			if err != nil {
				return err
			}
			dst, err := pass.Destination(roster, to, time.Now())
			if err != nil {
				return err
			}
			if err := pass.Guarded(roster, to); err != nil {
				return fmt.Errorf("nothing was passed: %w", err)
			}
			rd, ok := passReader(src)
			if !ok {
				return fmt.Errorf("%s is not a chat site tincan reads (want %s)", src, history.LiveSourcesLabel())
			}
			conv, err := pass.Read(ctx, rd, convID, history.Window{})
			if err != nil {
				return err
			}
			seed, err := pass.Pack(conv, pass.Budget(dst))
			if err != nil {
				return err
			}
			sent, err := r.SendGrouped(ctx, to, seed, envelope.KindAsk, "", pass.NewGroup())
			if err != nil {
				return err
			}
			cmd.PrintErrf("Passing the %s conversation %s to %s (request %s)\n", history.SourceLabel(src), conv.ID, to, sent.ID)
			res, err := waitFinal(cmd, r, sent.ID, wait)
			if err != nil {
				return err
			}
			if !res.Done() {
				return fmt.Errorf("%s has not opened the chat after %s; check it later with tincan get %s", to, wait, sent.ID)
			}
			body := ""
			if res.Reply != nil {
				body = res.Reply.Body
			}
			if res.Status != envelope.StatusAnswered {
				return fmt.Errorf("%s did not open the chat (%s): %s", to, res.Status, body)
			}
			newID, ok := pass.NewConversation(body, dst)
			if !ok {
				return fmt.Errorf("%s answered without naming its conversation; look for the new chat in %s", to, history.SourceLabel(dst))
			}
			cmd.Printf("Passed to %s. %s: %s\n", to, history.ConversationName(dst), newID)
			return nil
		},
	}
	cmd.AddCommand(passServeCmd(), passInstallCmd())
	cmd.Flags().StringVar(&to, "to", "", "the web chat teammate that opens the new chat (chatgpt-web, claude-web, ...)")
	cmd.Flags().StringVar(&id, "id", "", "with a source name, the conversation to pass (default: the newest)")
	cmd.Flags().DurationVar(&wait, "wait", DefaultPassWait, "how long to wait for the destination to open the chat")
	return cmd
}

// passSource reads the chat a pass names: a chat URL, or a source name
// with an optional id ("" means its newest conversation).
func passSource(ref, id string) (history.Source, string, error) {
	if strings.Contains(ref, "://") {
		if id != "" {
			return "", "", errors.New("--id goes with a source name, not a chat URL")
		}
		return history.ResolveChatURL(ref)
	}
	src := history.Source(ref)
	if !history.IsLiveSource(src) {
		return "", "", fmt.Errorf("%q is not a chat URL or a chat site (want a chat URL, or one of %s)", ref, liveSourceNames())
	}
	return src, id, nil
}

// liveSourceNames lists the chat sites a pass reads, by source name.
func liveSourceNames() string {
	var names []string
	for _, s := range history.Sources {
		if history.IsLiveSource(s) {
			names = append(names, string(s))
		}
	}
	return strings.Join(names, ", ")
}

// waitFinal waits up to wait for request id to finish, a long poll at a
// time; it returns its last state either way.
func waitFinal(cmd *cobra.Command, r *client.Relay, id string, wait time.Duration) (client.Result, error) {
	deadline := time.Now().Add(wait)
	for {
		left := time.Until(deadline)
		res, err := r.Get(cmd.Context(), id, client.ClampWait(left))
		if err != nil || res.Done() || left <= 0 {
			return res, err
		}
	}
}

// defaultPassAllowlistPath is the owner's list of agents that may ask
// the pass service, beside the history allowlist.
func defaultPassAllowlistPath() string {
	return filepath.Join(filepath.Dir(history.DefaultAllowlistPath()), "pass-allow.txt")
}

func passServeCmd() *cobra.Command {
	var configPath, allowPath, historyAllowPath, windowPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the pass agent: move a chat a teammate names to a web chat teammate, once the owner approves",
		Long: "Long-polls the relay as the pass agent. The relay holds every ask to it for the owner's approval.\n" +
			"For each approved ask it, in order:\n" +
			"  1. declines unless every agent in the relay-recorded chain is on the pass allowlist and on the history allowlist;\n" +
			"  2. reads the request: \"from: <chat URL>\" (or \"from: <source> <id>\") and \"to: <web agent>\", never \"latest\";\n" +
			"  3. refuses a destination that is not a web chat teammate, is signed out or out of messages, or runs a build\n" +
			"     older than " + pass.GuardVersion + ", before reading anything;\n" +
			"  4. reads the chat through the Tincan Chrome extension and refuses it when it was last updated outside the\n" +
			"     owner's history window;\n" +
			"  5. asks the destination to open a new chat with it, grouped so it never fails over, and replies with the\n" +
			"     new conversation's id only.\n" +
			"It refuses to run unless the relay stores kind pass for this agent. Normally started by the service\n" +
			"definition tincan pass install writes. See docs/adapters/pass.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defer useServiceLog()()
			if configPath == "" {
				configPath = serviceConfigPath(pass.DefaultAgentName)
			}
			configPath = expandHome(configPath)
			cfg, err := client.LoadConfigFrom(configPath)
			if err != nil {
				return fmt.Errorf("pass config %s: %w", configPath, err)
			}
			if cfg.Relay == "" {
				return fmt.Errorf("no relay configured in %s: run TINCAN_CONFIG=%s tincan join <code> --relay http://tincan-relay", configPath, configPath)
			}
			if cfg.Agent != "" && cfg.Agent != pass.DefaultAgentName {
				return wrongServeAgent("pass serve", pass.DefaultAgentName, configPath+" is joined as", cfg.Agent)
			}
			if allowPath == "" {
				allowPath = defaultPassAllowlistPath()
			}
			if historyAllowPath == "" {
				historyAllowPath = history.DefaultAllowlistPath()
			}
			allowPath, historyAllowPath = expandHome(allowPath), expandHome(historyAllowPath)
			allowed, err := history.LoadAllowlist(allowPath)
			if err != nil {
				return fmt.Errorf("pass allowlist: %w", err)
			}
			historyAllowed, err := history.LoadAllowlist(historyAllowPath)
			if err != nil {
				return fmt.Errorf("history allowlist: %w", err)
			}
			if windowPath == "" {
				windowPath = history.DefaultWindowPath()
			}
			windowPath = expandHome(windowPath)
			r, err := client.NewRelayForFile(cfg, configPath)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if client.NeedsRelayInfo(cfg) {
				learnRelayKeyWithin(ctx, r)
			}
			// The relay, not the config file, says who this machine is.
			me, err := r.WhoAmI(ctx)
			if err != nil {
				return fmt.Errorf("pass serve: could not confirm this agent's identity with the relay: %w", client.RejoinHint(err, cfg.Relay))
			}
			if me.Name != pass.DefaultAgentName {
				return wrongServeAgent("pass serve", pass.DefaultAgentName, "the relay knows the machine using "+configPath+" as", me.Name)
			}
			svc := &pass.Service{
				Relay:            r,
				Reader:           passReader,
				Allowlist:        history.FileAllowlist(allowPath),
				HistoryAllowlist: history.FileAllowlist(historyAllowPath),
				WindowFile:       windowPath,
				Log:              cmd.ErrOrStderr(),
			}
			cmd.PrintErrf("%stincan pass: serving as %s on %s (%s; %s)\n", serviceStamp(), me.Name, cfg.Relay,
				history.DescribeAllowlist(allowPath, allowed), history.DescribeAllowlist(historyAllowPath, historyAllowed))
			w, werr := history.LoadWindow(windowPath)
			cmd.PrintErrf("%stincan pass: %s\n", serviceStamp(), history.DescribeWindow(windowPath, w, werr))
			err = svc.Run(ctx)
			if ctx.Err() != nil {
				cmd.PrintErrf("%stincan pass: stopped\n", serviceStamp())
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "the pass agent's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/pass.json)")
	cmd.Flags().StringVar(&allowPath, "allowlist", "", "file of agents allowed to ask for a pass (default: ~/.config/tincan/pass-allow.txt; missing or * means every joined agent)")
	cmd.Flags().StringVar(&historyAllowPath, "history-allowlist", "", "the history allowlist every asker must also be on (default: ~/.config/tincan/history-allow.txt)")
	cmd.Flags().StringVar(&windowPath, "window-file", "", `the owner's history window, {"days": N, "max": N} (default: ~/.config/tincan/history-window.json)`)
	return cmd
}

func passInstallCmd() *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the pass service definition (launchd on macOS, systemd on Linux); does not start it",
		Long: "Writes a launchd agent (" + pass.ServiceLabel + ") on macOS, or a systemd user unit on Linux, that runs\n" +
			"tincan pass serve with TINCAN_CONFIG=~/.config/tincan/pass.json and keeps it running. It prints the\n" +
			"command that starts the service and never starts it itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := pass.InstallService(history.ServiceOptions{Binary: binary})
			if err != nil {
				return err
			}
			cmd.Printf("pass service definition: %s (not started)\n", res.Path)
			home, _ := os.UserHomeDir()
			cfgPath := filepath.Join(home, ".config", "tincan", "pass.json")
			if cfg, err := client.LoadConfigFrom(cfgPath); err != nil || cfg.Relay == "" {
				cmd.Printf("pass is not joined yet; first invite it with tincan invite pass --kind pass, then run:\n  TINCAN_CONFIG=%s tincan join <code> --relay <relay url>\n", cfgPath)
			}
			cmd.Printf("start it with:\n  %s\n", res.Next)
			return nil
		},
	}
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	return cmd
}
