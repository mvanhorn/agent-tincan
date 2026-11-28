package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/t3bridge"
)

const t3ServiceLabel = "com.agenttincan.t3"

type t3ServiceOptions struct {
	binary, config, path, home string
	t3URL, t3Home, defaultProj string
}

func t3ServicePlist(o t3ServiceOptions) string {
	args := []string{o.binary, "t3", "serve", "--config", o.config}
	if o.t3URL != "" {
		args = append(args, "--t3-url", o.t3URL)
	}
	if o.t3Home != "" {
		args = append(args, "--t3-home", o.t3Home)
	}
	if o.defaultProj != "" {
		args = append(args, "--default-project", o.defaultProj)
	}
	var a strings.Builder
	for _, s := range args {
		fmt.Fprintf(&a, "    <string>%s</string>\n", xmlText(s))
	}
	logPath := filepath.Join(o.home, "Library", "Logs", "tincan-t3.log")
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Agent Tincan T3 Code bridge: runs "tincan t3 serve" under launchd.
  Written by "tincan t3 install". See docs/adapters/t3-code.md.
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + t3ServiceLabel + `</string>
  <key>ProgramArguments</key>
  <array>
` + a.String() + `  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>TINCAN_CONFIG</key>
    <string>` + xmlText(o.config) + `</string>
    <key>PATH</key>
    <string>` + xmlText(o.path) + `</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>` + xmlText(logPath) + `</string>
  <key>StandardErrorPath</key>
  <string>` + xmlText(logPath) + `</string>
</dict>
</plist>
`
}

func t3ServiceSystemd(o t3ServiceOptions) string {
	args := []string{shellQuote(o.binary), "t3", "serve", "--config", shellQuote(o.config)}
	if o.t3URL != "" {
		args = append(args, "--t3-url", shellQuote(o.t3URL))
	}
	if o.t3Home != "" {
		args = append(args, "--t3-home", shellQuote(o.t3Home))
	}
	if o.defaultProj != "" {
		args = append(args, "--default-project", shellQuote(o.defaultProj))
	}
	return fmt.Sprintf(`[Unit]
Description=Agent Tincan T3 Code bridge
After=network-online.target

[Service]
ExecStart=%s
Environment="TINCAN_CONFIG=%s"
Environment="PATH=%s"
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`, strings.Join(args, " "), o.config, o.path)
}

func t3InstallCmd() *cobra.Command {
	var configPath, binary, t3URL, t3HomeFlag, defaultProj string
	var wireProviders, overrideShared, start bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Wire T3 providers for outbound Tincan and install the tincan t3 serve background service",
		Long: `Install the tincan t3 serve background service (launchd on macOS, systemd user
unit on Linux) and wire Agent Tincan MCP across T3 Code's configured providers
(Antigravity isolated profiles, Claude homePaths, OpenCode, Codex) without
overwriting existing standalone teammates unless --override-shared is passed.

Join the t3 teammate and authorize with T3 Code first:
  tincan invite t3 --kind t3-code
  TINCAN_CONFIG=~/.config/tincan/t3.json tincan join <code> --relay <relay url>
  tincan t3 auth --config ~/.config/tincan/t3.json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			cfgPath := resolveT3ConfigPath(configPath)
			if abs, err := filepath.Abs(cfgPath); err == nil {
				cfgPath = abs
			}
			cfg, err := client.LoadConfigFrom(cfgPath)
			if err != nil || cfg.Relay == "" {
				return fmt.Errorf("t3 teammate is not joined in %s; invite it with `tincan invite t3 --kind t3-code` and join with `TINCAN_CONFIG=%s tincan join <code> --relay <relay url>`", cfgPath, cfgPath)
			}
			if t3URL != "" {
				if t3URL, err = t3bridge.ValidateOrigin(t3URL); err != nil {
					return err
				}
			}
			t3Home, err := resolveT3Home(t3HomeFlag)
			if err != nil {
				return err
			}
			if binary == "" {
				if binary, err = os.Executable(); err != nil {
					return err
				}
				if p, err := filepath.EvalSymlinks(binary); err == nil {
					binary = p
				}
			}

			// 1. Wire T3 provider MCP configs so every T3 thread can send to Agent Tincan.
			if wireProviders {
				statuses, err := t3bridge.WireProviders(t3bridge.WireOptions{
					Home:           home,
					T3Home:         t3Home,
					TincanBinary:   binary,
					TincanConfig:   cfgPath,
					OverrideShared: overrideShared,
				})
				if err != nil {
					return fmt.Errorf("wire T3 providers: %w", err)
				}
				for _, st := range statuses {
					cmd.Printf("provider %s (%s): %s [%s]\n", st.InstanceID, st.Driver, st.Action, st.ConfigPath)
					if st.Detail != "" {
						cmd.Printf("  %s\n", st.Detail)
					}
				}
			}

			// 2. Write service definition.
			pathEnv := os.Getenv("PATH")
			if pathEnv == "" {
				pathEnv = "/usr/bin:/bin:/usr/sbin:/sbin"
			}
			opts := t3ServiceOptions{
				binary:      binary,
				config:      cfgPath,
				path:        pathEnv,
				home:        home,
				t3URL:       t3URL,
				t3Home:      t3HomeFlag,
				defaultProj: defaultProj,
			}

			switch runtime.GOOS {
			case "darwin":
				dir := filepath.Join(home, "Library", "LaunchAgents")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return err
				}
				plistPath := filepath.Join(dir, t3ServiceLabel+".plist")
				if err := os.WriteFile(plistPath, []byte(t3ServicePlist(opts)), 0o644); err != nil {
					return err
				}
				cmd.Printf("Wrote %s\n", plistPath)
				if start {
					l := launchctl{uid: os.Getuid()}
					if l.Loaded(t3ServiceLabel) {
						_ = l.Bootout(t3ServiceLabel)
					}
					if err := l.Bootstrap(plistPath); err != nil {
						return fmt.Errorf("start %s: %w", t3ServiceLabel, err)
					}
					cmd.Printf("Started %s\n", t3ServiceLabel)
				} else {
					cmd.Printf("Load it with: launchctl bootstrap gui/%d %s\n", os.Getuid(), plistPath)
				}
			case "linux":
				dir := filepath.Join(home, ".config", "systemd", "user")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return err
				}
				unitPath := filepath.Join(dir, "tincan-t3.service")
				if err := os.WriteFile(unitPath, []byte(t3ServiceSystemd(opts)), 0o644); err != nil {
					return err
				}
				cmd.Printf("Wrote %s\n", unitPath)
				cmd.Println("Enable and start it with: systemctl --user daemon-reload && systemctl --user enable --now tincan-t3.service")
			default:
				return errors.New("tincan t3 install supports macOS (launchd) and Linux (systemd) only")
			}

			authPath := t3bridge.AuthPathForConfig(cfgPath)
			if _, err := t3bridge.LoadAuth(authPath); err != nil {
				cmd.Printf("Note: T3 Code is not authorized yet (%v). Run `tincan t3 auth --config %s` next.\n", err, cfgPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "t3 teammate config (default: $TINCAN_CONFIG, else ~/.config/tincan/t3.json)")
	cmd.Flags().StringVar(&binary, "binary", "", "path to tincan binary (default: current executable)")
	cmd.Flags().StringVar(&t3URL, "t3-url", "", "T3 Code server origin (default: discovered from server-runtime.json)")
	cmd.Flags().StringVar(&t3HomeFlag, "t3-home", "", "T3 Code data directory (default: $T3CODE_HOME, else ~/.t3)")
	cmd.Flags().StringVar(&defaultProj, "default-project", "", "default T3 project title or folder basename for asks without a project")
	cmd.Flags().BoolVar(&wireProviders, "wire-providers", true, "add agent-tincan to unconfigured T3 provider MCP configs")
	cmd.Flags().BoolVar(&overrideShared, "override-shared", false, "also repoint shared configs (e.g. ~/.claude.json, ~/.codex/config.toml) that already have another agent-tincan entry")
	cmd.Flags().BoolVar(&start, "start", false, "load/restart the launchd service right away (macOS)")
	return cmd
}
