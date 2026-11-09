package cli

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// windowServiceLabel is the launchd label of tincan window serve.
const windowServiceLabel = "com.agenttincan.window"

// windowServiceOptions fills the window service's LaunchAgent.
type windowServiceOptions struct {
	binary, config, path, home string
	launch                     windowLaunch
	maxWindows                 int
	timeout                    time.Duration
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// windowServicePlist is the LaunchAgent that keeps tincan window serve
// running as the window agent.
func windowServicePlist(o windowServiceOptions) string {
	args := []string{o.binary, "window", "serve",
		"--dir", o.launch.dir, "--terminal", o.launch.terminal, "--cmux", o.launch.cmux,
		"--claude", o.launch.claude, "--starts", o.launch.starts,
		"--max-windows", fmt.Sprint(o.maxWindows), "--timeout", o.timeout.String()}
	var a strings.Builder
	for _, s := range args {
		fmt.Fprintf(&a, "    <string>%s</string>\n", xmlText(s))
	}
	logPath := filepath.Join(o.home, "Library", "Logs", "tincan-window.log")
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Agent Tincan window agent: runs "tincan window serve" under launchd, which
  opens a new Claude Code window in cmux for each ask to this agent.
  Written by "tincan window install". See docs/adapters/claude-code.md.
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + windowServiceLabel + `</string>
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
  <string>Interactive</string>
  <key>StandardOutPath</key>
  <string>` + xmlText(logPath) + `</string>
  <key>StandardErrorPath</key>
  <string>` + xmlText(logPath) + `</string>
</dict>
</plist>
`
}

func windowInstallCmd() *cobra.Command {
	var w windowLaunch
	var config, binary string
	var maxWindows int
	var timeout time.Duration
	var start bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Keep tincan window serve running under launchd (macOS)",
		Long: `Write the LaunchAgent ` + windowServiceLabel + ` into ~/Library/LaunchAgents. It runs
"tincan window serve" as the window agent (TINCAN_CONFIG=--config) at login and
keeps it running. --start also loads it now, replacing a copy already loaded.

Join the window agent first, from an admin device's invite:
  TINCAN_CONFIG=~/.config/tincan/window.json tincan join <code> --relay <relay url>`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "darwin" {
				return errors.New("tincan window install writes a launchd agent, so it works on macOS only")
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if w.dir == "" {
				w.dir = home
			}
			if err := w.resolve(); err != nil {
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
			if config, err = filepath.Abs(expandHome(config)); err != nil {
				return err
			}
			// The service's PATH: this shell's, so the tools a window's
			// helpers run are found, then the system's.
			path := os.Getenv("PATH")
			if path == "" {
				path = "/usr/bin:/bin:/usr/sbin:/sbin"
			}
			plist := windowServicePlist(windowServiceOptions{
				binary: binary, config: config, path: path, home: home,
				launch: w, maxWindows: maxWindows, timeout: timeout,
			})
			dir := filepath.Join(home, "Library", "LaunchAgents")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			file := filepath.Join(dir, windowServiceLabel+".plist")
			if err := os.WriteFile(file, []byte(plist), 0o644); err != nil {
				return err
			}
			cmd.Printf("window service definition: %s\n", file)
			if cfg, err := client.LoadConfigFrom(config); err != nil || cfg.Relay == "" {
				cmd.Printf("the window agent is not joined yet; first run:\n  TINCAN_CONFIG=%s tincan join <code> --relay <relay url>\n", config)
			}
			l := launchctl{uid: os.Getuid()}
			if !start {
				cmd.Printf("start it with:\n  launchctl bootstrap gui/%d %s\n", os.Getuid(), file)
				return nil
			}
			if l.Loaded(windowServiceLabel) {
				if err := l.Bootout(windowServiceLabel); err != nil {
					return fmt.Errorf("stop the running window service: %w", err)
				}
				time.Sleep(time.Second) // bootout finishes after it returns
			}
			if err := l.Bootstrap(file); err != nil {
				return fmt.Errorf("start the window service: %w", err)
			}
			cmd.Printf("started; it logs to %s\n", filepath.Join(home, "Library", "Logs", "tincan-window.log"))
			return nil
		},
	}
	w.flags(cmd)
	cmd.Flags().StringVar(&w.dir, "dir", "", "working directory every window opens in (default: your home directory)")
	cmd.Flags().StringVar(&config, "config", "~/.config/tincan/window.json", "the window agent's config")
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	cmd.Flags().IntVar(&maxWindows, "max-windows", 3, "how many windows can work on asks at once")
	cmd.Flags().DurationVar(&timeout, "timeout", 12*time.Hour, "how long a window may hold a request before it is failed")
	cmd.Flags().BoolVar(&start, "start", false, "also load the service now")
	return cmd
}
