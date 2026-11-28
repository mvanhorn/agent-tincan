package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/t3bridge"
)

const defaultT3ConfigPath = "~/.config/tincan/t3.json"

func t3Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "t3",
		Short: "Run T3 Code as a two-way Agent Tincan teammate (threads, watches, and multi-provider outbound)",
		Long: `Connect T3 Code to your Agent Tincan team in both directions:

  tincan t3 auth      authorize Agent Tincan against T3 Code's /mcp server (OAuth 2.1 PKCE)
  tincan t3 serve     run the local bridge (relay <-> T3 Code /mcp + SEP-3415 events/stream)
  tincan t3 install   wire T3's providers for outbound Tincan and install the background service
  tincan t3 watch     opt in an interactive T3 thread to notify a teammate on settle/input/PR wake
  tincan t3 unwatch   stop watching a T3 thread
  tincan t3 doctor    check relay, T3 auth, MCP Events stream, service, and T3 provider wiring

See docs/adapters/t3-code.md.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(t3AuthCmd(), t3ServeCmd(), t3InstallCmd(), t3WatchCmd(), t3UnwatchCmd(), t3DoctorCmd())
	return cmd
}

func resolveT3ConfigPath(flagVal string) string {
	if strings.TrimSpace(flagVal) != "" {
		return expandHome(flagVal)
	}
	if env := strings.TrimSpace(os.Getenv("TINCAN_CONFIG")); env != "" {
		return expandHome(env)
	}
	return expandHome(defaultT3ConfigPath)
}

func resolveT3Home(flagVal string) (string, error) {
	if strings.TrimSpace(flagVal) != "" {
		return expandHome(flagVal), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return t3bridge.DefaultT3Home(home), nil
}

func resolveT3Origin(urlFlag, t3Home string, auth t3bridge.AuthConfig) (string, error) {
	if strings.TrimSpace(urlFlag) != "" {
		return t3bridge.ValidateOrigin(urlFlag)
	}
	// When credentials are already loaded, prefer their bound OAuth origin so a
	// remote login never sends its bearer token to a local T3 instance.
	if auth.Origin != "" {
		return t3bridge.ValidateOrigin(auth.Origin)
	}
	if rt, err := t3bridge.DiscoverLocalOrigin(t3Home); err == nil && rt.Origin != "" {
		return rt.Origin, nil
	}
	return "", fmt.Errorf("could not discover T3 Code server in %s/userdata/server-runtime.json; start T3 Code or pass --t3-url", t3Home)
}

func t3AuthCmd() *cobra.Command {
	var configPath, t3URL, t3HomeFlag, tokenFile string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Authorize Agent Tincan with T3 Code's /mcp server",
		Long: `Run T3 Code's local OAuth 2.1 PKCE login so you can choose the access
level in T3 Code's approval screen, then save the bearer token privately
(mode 0600) beside the t3 teammate's TINCAN_CONFIG.

For headless machines where you already have a bearer token file (mode 0600),
pass --token-file <path>.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath := resolveT3ConfigPath(configPath)
			t3Home, err := resolveT3Home(t3HomeFlag)
			if err != nil {
				return err
			}
			origin, err := resolveT3Origin(t3URL, t3Home, t3bridge.AuthConfig{})
			if err != nil {
				return err
			}
			authPath := t3bridge.AuthPathForConfig(cfgPath)

			var auth t3bridge.AuthConfig
			if tokenFile != "" {
				tok, err := t3bridge.LoadTokenFile(expandHome(tokenFile))
				if err != nil {
					return err
				}
				auth = t3bridge.AuthConfig{
					Origin:   origin,
					Token:    tok,
					IssuedAt: time.Now().UTC(),
				}
			} else {
				var openFn func(string) error
				if !noBrowser {
					openFn = openBrowser
				}
				auth, err = t3bridge.RunOAuthPKCE(cmd.Context(), t3bridge.OAuthOptions{
					Origin:      origin,
					ClientName:  "Agent Tincan",
					OpenBrowser: openFn,
					PrintURL: func(u string) {
						cmd.Printf("Open this URL to approve Agent Tincan in T3 Code:\n  %s\n", u)
					},
				})
				if err != nil {
					return err
				}
			}

			// Verify the token immediately with events/list before saving.
			mcpCli, err := t3bridge.NewMCPClient(auth.Origin, auth.Token, Version)
			if err != nil {
				return err
			}
			evRes, err := mcpCli.ListEvents(cmd.Context())
			if err != nil {
				return fmt.Errorf("T3 Code accepted login but events/list failed: %w", err)
			}
			auth.ConfigPath = cleanAbsPath(cfgPath)
			if cfg, err := client.LoadConfigFrom(cfgPath); err == nil && cfg.Agent != "" {
				auth.Agent = cfg.Agent
			}
			if err := t3bridge.SaveAuth(authPath, auth); err != nil {
				return err
			}
			cmd.Printf("Authorized with %s (%s) at %s (%d MCP event types available).\nSaved private credentials to %s.\n",
				nonEmptyStr(evRes.ServerInfo.Name, "T3 Code"), evRes.ServerInfo.Version, auth.Origin, len(evRes.Events), authPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "t3 teammate config (default: $TINCAN_CONFIG, else ~/.config/tincan/t3.json)")
	cmd.Flags().StringVar(&t3URL, "t3-url", "", "T3 Code server origin (default: discovered from ~/.t3/userdata/server-runtime.json)")
	cmd.Flags().StringVar(&t3HomeFlag, "t3-home", "", "T3 Code data directory (default: $T3CODE_HOME, else ~/.t3)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "read an existing bearer token from a private 0600 file instead of running OAuth")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the OAuth approval URL without opening a browser")
	return cmd
}

func t3ServeCmd() *cobra.Command {
	var configPath, t3URL, t3HomeFlag, defaultProject string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the T3 Code bridge service (relay <-> T3 Code /mcp + SEP-3415 events)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defer useServiceLog()()
			cfgPath := resolveT3ConfigPath(configPath)
			cfg, err := client.LoadConfigFrom(cfgPath)
			if err != nil {
				return fmt.Errorf("load %s: %w", cfgPath, err)
			}
			if cfg.Relay == "" {
				return fmt.Errorf("no relay configured in %s: join first with TINCAN_CONFIG=%s tincan join <code> --relay <relay url>", cfgPath, cfgPath)
			}
			r, err := client.NewRelayFor(cfg)
			if err != nil {
				return err
			}
			learnRelayKeyWithin(cmd.Context(), r)

			t3Home, err := resolveT3Home(t3HomeFlag)
			if err != nil {
				return err
			}
			authPath := t3bridge.AuthPathForConfig(cfgPath)
			auth, err := t3bridge.LoadAuth(authPath)
			if err != nil {
				return fmt.Errorf("load T3 auth (%s): %w (run tincan t3 auth first)", authPath, err)
			}
			if auth.ConfigPath == "" || auth.Agent == "" {
				auth.ConfigPath = cleanAbsPath(cfgPath)
				if cfg.Agent != "" {
					auth.Agent = cfg.Agent
				}
				_ = t3bridge.SaveAuth(authPath, auth)
			}
			origin, err := resolveT3Origin(t3URL, t3Home, auth)
			if err != nil {
				return err
			}
			mcpCli, err := t3bridge.NewMCPClient(origin, auth.Token, Version)
			if err != nil {
				return err
			}

			state := t3bridge.NewStateStore(t3bridge.StatePathForConfig(cfgPath))
			routes := t3bridge.NewRouteStore(t3bridge.RoutesPathForConfig(cfgPath))
			logger := log.New(cmd.ErrOrStderr(), serviceStamp(), 0)
			br, err := t3bridge.NewBridge(t3bridge.Config{
				Relay:          r,
				T3:             mcpCli,
				State:          state,
				Routes:         routes,
				DefaultProject: defaultProject,
				Logger:         logger,
				LateReplyGrace: 5 * time.Second,
			})
			if err != nil {
				return err
			}

			runCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			logger.Printf("t3 bridge: serving as %s against T3 Code at %s", cfg.Agent, origin)
			err = br.Run(runCtx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "t3 teammate config (default: $TINCAN_CONFIG, else ~/.config/tincan/t3.json)")
	cmd.Flags().StringVar(&t3URL, "t3-url", "", "T3 Code server origin (default: discovered from server-runtime.json)")
	cmd.Flags().StringVar(&t3HomeFlag, "t3-home", "", "T3 Code data directory (default: $T3CODE_HOME, else ~/.t3)")
	cmd.Flags().StringVar(&defaultProject, "default-project", "", "default T3 project title or folder basename for asks without a project")
	return cmd
}

func t3WatchCmd() *cobra.Command {
	var configPath, notifyAgent, note string
	var eventsCSV string
	var oneShot bool
	cmd := &cobra.Command{
		Use:   "watch <thread-id-or-url>",
		Short: "Opt an interactive T3 thread in to notify a teammate on settle, input, or PR watch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tid := t3bridge.ExtractThreadID(args[0])
			if tid == "" {
				return fmt.Errorf("invalid T3 thread id or URL %q", args[0])
			}
			if strings.TrimSpace(notifyAgent) == "" {
				return errors.New("--notify <agent> is required (for example --notify grokbot)")
			}
			cfgPath := resolveT3ConfigPath(configPath)
			events := parseWatchEvents(eventsCSV)
			state := t3bridge.NewStateStore(t3bridge.StatePathForConfig(cfgPath))
			w := t3bridge.ThreadWatch{
				ID:          fmt.Sprintf("cli-%d", time.Now().UnixNano()),
				ThreadID:    tid,
				NotifyAgent: strings.TrimSpace(notifyAgent),
				OneShot:     oneShot,
				Note:        strings.TrimSpace(note),
				Events:      events,
				CreatedAt:   time.Now().UTC(),
			}
			if err := state.Update(func(d *t3bridge.StateData) error {
				d.Watches[tid] = append(d.Watches[tid], w)
				return nil
			}); err != nil {
				return err
			}
			cmd.Printf("Watching T3 thread %s -> notify %s on %s.\n", tid, w.NotifyAgent, strings.Join(events, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "t3 teammate config (default: $TINCAN_CONFIG, else ~/.config/tincan/t3.json)")
	cmd.Flags().StringVar(&notifyAgent, "notify", "", "Agent Tincan teammate to notify (e.g. grokbot)")
	cmd.Flags().StringVar(&eventsCSV, "events", "settled,needs_input,pr_watch", "comma-separated events: settled, needs_input, pr_watch")
	cmd.Flags().StringVar(&note, "note", "", "optional instruction or context included in notifications")
	cmd.Flags().BoolVar(&oneShot, "once", false, "remove the watch after the first settled notification")
	return cmd
}

func t3UnwatchCmd() *cobra.Command {
	var configPath, notifyAgent string
	cmd := &cobra.Command{
		Use:   "unwatch <thread-id-or-url>",
		Short: "Stop watching a T3 thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tid := t3bridge.ExtractThreadID(args[0])
			if tid == "" {
				return fmt.Errorf("invalid T3 thread id or URL %q", args[0])
			}
			cfgPath := resolveT3ConfigPath(configPath)
			state := t3bridge.NewStateStore(t3bridge.StatePathForConfig(cfgPath))
			removed := 0
			if err := state.Update(func(d *t3bridge.StateData) error {
				list := d.Watches[tid]
				var kept []t3bridge.ThreadWatch
				for _, w := range list {
					if notifyAgent == "" || w.NotifyAgent == notifyAgent {
						removed++
						continue
					}
					kept = append(kept, w)
				}
				if len(kept) == 0 {
					delete(d.Watches, tid)
				} else {
					d.Watches[tid] = kept
				}
				return nil
			}); err != nil {
				return err
			}
			cmd.Printf("Removed %d watch(es) on T3 thread %s.\n", removed, tid)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "t3 teammate config (default: $TINCAN_CONFIG, else ~/.config/tincan/t3.json)")
	cmd.Flags().StringVar(&notifyAgent, "notify", "", "remove only watches notifying this agent")
	return cmd
}

func t3DoctorCmd() *cobra.Command {
	var configPath, t3URL, t3HomeFlag string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check T3 Code teammate relay, auth, MCP Events stream, service, and provider wiring",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath := resolveT3ConfigPath(configPath)
			t3Home, err := resolveT3Home(t3HomeFlag)
			if err != nil {
				return err
			}
			home, _ := os.UserHomeDir()
			checks, providers := runT3DoctorChecks(cmd.Context(), home, t3Home, cfgPath, t3URL)
			ok := true
			for _, c := range checks {
				if c.Status == "fail" {
					ok = false
				}
			}
			if asJSON {
				rep := struct {
					OK        bool                      `json:"ok"`
					Checks    []check                   `json:"checks"`
					Providers []t3bridge.ProviderStatus `json:"providers,omitempty"`
				}{OK: ok, Checks: checks, Providers: providers}
				if err := printJSON(cmd, rep); err != nil {
					return err
				}
				if !ok {
					return &ExitError{Code: 1, Silent: true}
				}
				return nil
			}
			for _, c := range checks {
				cmd.Printf("[%s] %s: %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
				if c.Fix != "" {
					cmd.Printf("      fix: %s\n", c.Fix)
				}
			}
			if len(providers) > 0 {
				cmd.Println("\nT3 providers:")
				for _, p := range providers {
					cmd.Printf("  - %s (%s): %s [%s]\n", p.InstanceID, p.Driver, p.State, p.ConfigPath)
					if p.Detail != "" {
						cmd.Printf("    %s\n", p.Detail)
					}
				}
			}
			if !ok {
				return &ExitError{Code: 1, Silent: true}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "t3 teammate config (default: $TINCAN_CONFIG, else ~/.config/tincan/t3.json)")
	cmd.Flags().StringVar(&t3URL, "t3-url", "", "T3 Code server origin (default: discovered from server-runtime.json)")
	cmd.Flags().StringVar(&t3HomeFlag, "t3-home", "", "T3 Code data directory (default: $T3CODE_HOME, else ~/.t3)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print checks as JSON")
	return cmd
}

func runT3DoctorChecks(ctx context.Context, home, t3Home, cfgPath, t3URL string) ([]check, []t3bridge.ProviderStatus) {
	var checks []check

	// 1. Tincan relay config & reachability.
	cfg, err := client.LoadConfigFrom(cfgPath)
	if err != nil || cfg.Relay == "" {
		checks = append(checks, check{
			Name:   "relay",
			Status: "fail",
			Detail: fmt.Sprintf("not joined in %s", cfgPath),
			Fix:    fmt.Sprintf("On an admin device run `tincan invite t3 --kind t3-code`, then `TINCAN_CONFIG=%s tincan join <code> --relay <relay url>`.", cfgPath),
		})
	} else {
		r, rErr := client.NewRelayFor(cfg)
		if rErr != nil {
			checks = append(checks, check{Name: "relay", Status: "fail", Detail: rErr.Error()})
		} else {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, aErr := r.Agents(pctx)
			cancel()
			if aErr != nil {
				checks = append(checks, check{Name: "relay", Status: "fail", Detail: fmt.Sprintf("relay %s unreachable as %s: %v", cfg.Relay, cfg.Agent, aErr), Fix: fmt.Sprintf("Check Tailscale or run TINCAN_CONFIG=%s tincan rejoin.", cfgPath)})
			} else {
				checks = append(checks, check{Name: "relay", Status: "ok", Detail: fmt.Sprintf("joined as %s on %s", cfg.Agent, cfg.Relay)})
			}
		}
	}

	// 2. T3 auth file & token validity (never print token).
	authPath := t3bridge.AuthPathForConfig(cfgPath)
	auth, authErr := t3bridge.LoadAuth(authPath)
	if authErr != nil {
		checks = append(checks, check{
			Name:   "t3 auth",
			Status: "fail",
			Detail: authErr.Error(),
			Fix:    fmt.Sprintf("Run `tincan t3 auth --config %s` to authorize Agent Tincan with T3 Code.", cfgPath),
		})
	} else {
		sum := auth.Summary()
		status := "ok"
		detail := fmt.Sprintf("credentials valid in %s (origin %s)", authPath, sum.Origin)
		fix := ""
		if !sum.ExpiresAt.IsZero() {
			rem := time.Until(sum.ExpiresAt)
			if rem <= 0 {
				status = "fail"
				detail = fmt.Sprintf("T3 OAuth token in %s expired at %s", authPath, sum.ExpiresAt.Format(time.RFC3339))
				fix = fmt.Sprintf("Run `tincan t3 auth --config %s` to re-authorize.", cfgPath)
			} else if rem < 72*time.Hour {
				status = "warn"
				detail = fmt.Sprintf("T3 OAuth token expires in %s (%s)", rem.Round(time.Hour), sum.ExpiresAt.Format(time.RFC3339))
				fix = fmt.Sprintf("Run `tincan t3 auth --config %s` before it expires.", cfgPath)
			}
		}
		checks = append(checks, check{Name: "t3 auth", Status: status, Detail: detail, Fix: fix})
	}

	// 3. T3 server + SEP-3415 MCP Events check.
	origin, origErr := resolveT3Origin(t3URL, t3Home, auth)
	if origErr != nil {
		checks = append(checks, check{Name: "t3 server", Status: "fail", Detail: origErr.Error(), Fix: "Start T3 Code Nightly or pass --t3-url."})
	} else if authErr == nil {
		mcpCli, cErr := t3bridge.NewMCPClient(origin, auth.Token, Version)
		if cErr != nil {
			checks = append(checks, check{Name: "t3 server", Status: "fail", Detail: cErr.Error()})
		} else {
			lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			evRes, lErr := mcpCli.ListEvents(lctx)
			cancel()
			if lErr != nil {
				checks = append(checks, check{Name: "t3 events", Status: "fail", Detail: fmt.Sprintf("events/list at %s failed: %v", origin, lErr), Fix: "Confirm T3 Code Nightly is running with MCP Events enabled and re-run `tincan t3 auth` if 401."})
			} else {
				available := make(map[string]bool, len(evRes.Events))
				for _, ev := range evRes.Events {
					available[ev.Name] = true
				}
				var missing []string
				for _, reqEv := range t3bridge.WatchedMilestoneEvents {
					if !available[reqEv] {
						missing = append(missing, reqEv)
					}
				}
				if len(missing) > 0 {
					checks = append(checks, check{
						Name:   "t3 events",
						Status: "fail",
						Detail: fmt.Sprintf("%s %s at %s is missing required MCP events: %s", nonEmptyStr(evRes.ServerInfo.Name, "T3 Code"), evRes.ServerInfo.Version, origin, strings.Join(missing, ", ")),
						Fix:    "Upgrade T3 Code Nightly to a build that exposes thread.run_settled, thread.needs_input, thread.input_resolved, and pr.watch_wake on /mcp.",
					})
				} else {
					sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
					pErr := mcpCli.ProbeEventStream(sctx, t3bridge.WatchedMilestoneEvents[0])
					scancel()
					if pErr != nil {
						checks = append(checks, check{
							Name:   "t3 events",
							Status: "fail",
							Detail: fmt.Sprintf("events/stream probe at %s failed: %v", origin, pErr),
							Fix:    "Confirm T3 Code Nightly supports SEP-3415 SSE streaming on /mcp and re-run `tincan t3 auth` if unauthorized.",
						})
					} else {
						checks = append(checks, check{
							Name:   "t3 events",
							Status: "ok",
							Detail: fmt.Sprintf("%s %s at %s (%d SEP-3415 event types; events/stream handshake ok)", nonEmptyStr(evRes.ServerInfo.Name, "T3 Code"), evRes.ServerInfo.Version, origin, len(evRes.Events)),
						})
					}
				}
			}
		}
	}

	// 4. Service status + active watches.
	if runtime.GOOS == "darwin" {
		loaded := launchctl{uid: os.Getuid()}.Loaded(t3ServiceLabel)
		if loaded {
			checks = append(checks, check{Name: "service", Status: "ok", Detail: t3ServiceLabel + " is loaded in launchd"})
		} else {
			checks = append(checks, check{Name: "service", Status: "warn", Detail: t3ServiceLabel + " is not loaded", Fix: fmt.Sprintf("Run `tincan t3 install --config %s --start`.", cfgPath)})
		}
	}
	if snap, err := t3bridge.NewStateStore(t3bridge.StatePathForConfig(cfgPath)).Snapshot(); err == nil {
		watchCount := 0
		for _, ws := range snap.Watches {
			watchCount += len(ws)
		}
		activeAsks := 0
		for _, a := range snap.Asks {
			if !a.Done {
				activeAsks++
			}
		}
		checks = append(checks, check{Name: "state", Status: "ok", Detail: fmt.Sprintf("%d active dispatched thread(s), %d watched thread(s)", activeAsks, watchCount)})
	}

	// 5. Provider MCP wiring.
	providers, pErr := t3bridge.InspectProviders(home, t3Home, cfgPath)
	if pErr != nil {
		checks = append(checks, check{Name: "t3 providers", Status: "warn", Detail: pErr.Error()})
	} else if len(providers) == 0 {
		checks = append(checks, check{Name: "t3 providers", Status: "warn", Detail: "no T3 provider MCP configs discovered yet", Fix: "Open T3 Code once, then run `tincan t3 install`."})
	} else {
		wired, missing, shared, broken := 0, 0, 0, 0
		for _, p := range providers {
			switch p.State {
			case t3bridge.ProviderWired:
				wired++
			case t3bridge.ProviderSharedOther:
				shared++
			case t3bridge.ProviderMissing:
				missing++
			case t3bridge.ProviderError:
				broken++
			}
		}
		st := "ok"
		fix := ""
		if broken > 0 {
			st = "fail"
			fix = "Fix malformed provider config JSON/TOML reported below."
		} else if missing > 0 {
			st = "warn"
			fix = fmt.Sprintf("Run `tincan t3 install --config %s` to wire the remaining T3 providers.", cfgPath)
		}
		checks = append(checks, check{
			Name:   "t3 providers",
			Status: st,
			Detail: fmt.Sprintf("%d wired to t3, %d using another tincan agent, %d missing, %d broken", wired, shared, missing, broken),
			Fix:    fix,
		})
	}

	return checks, providers
}

func parseWatchEvents(csv string) []string {
	var out []string
	for part := range strings.SplitSeq(csv, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "settled", "thread.run_settled":
			out = append(out, "thread.run_settled")
		case "needs_input", "input", "thread.needs_input":
			out = append(out, "thread.needs_input")
		case "pr_watch", "pr", "pr.watch_wake":
			out = append(out, "pr.watch_wake")
		}
	}
	if len(out) == 0 {
		return []string{"thread.run_settled", "thread.needs_input", "pr.watch_wake"}
	}
	return out
}

// t3LateReplyRoutingActive reports whether outbound asks from a T3 Code thread
// using cfgPath/cfg should record routes for tincan t3 serve. It matches only
// the T3 bridge's own agent/config, never a standalone teammate whose config
// happens to share the same directory as t3-auth.json.
func t3LateReplyRoutingActive(cfgPath string, cfg client.Config) bool {
	if cfg.Agent == "t3" || cfg.Agent == "t3-code" {
		return true
	}
	if cfgPath == "" {
		return false
	}
	auth, err := t3bridge.LoadAuth(t3bridge.AuthPathForConfig(cfgPath))
	if err != nil {
		return false
	}
	if auth.ConfigPath != "" && cleanAbsPath(auth.ConfigPath) == cleanAbsPath(cfgPath) {
		return true
	}
	if auth.Agent != "" && cfg.Agent != "" && auth.Agent == cfg.Agent {
		return true
	}
	return auth.ConfigPath == "" && auth.Agent == "" && filepath.Base(cfgPath) == "t3.json"
}

func cleanAbsPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return filepath.Clean(abs)
}

// recordT3OutboundResult records pending asks in t3-routes.json and clears or
// marks inline-answered asks so tincan t3 serve never echoes a reply already read.
func recordT3OutboundResult(routes *t3bridge.RouteStore, threadID string, res client.Result) {
	if routes == nil || threadID == "" || res.Request.ID == "" {
		return
	}
	switch res.Status {
	case envelope.StatusAnswered, envelope.StatusFailed, envelope.StatusDeclined:
		_ = routes.MarkInlineDone(res.Request.ID)
	default:
		_ = routes.Record(res.Request.ID, threadID, res.Request.To)
	}
}

func nonEmptyStr(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}
