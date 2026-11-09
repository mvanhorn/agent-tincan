package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/macapp"
)

// relayServiceLabel is the launchd label of the relay service.
const relayServiceLabel = "com.agenttincan.relay"

// relaySystemdUnit is the systemd user unit name of the relay service.
const relaySystemdUnit = "tincan-relay.service"

// relayLaunchdTemplate is the relay's launchd agent. Placeholders:
// __TINCAN_BINARY__, __HOME__ and __PATH__, XML-escaped when filled, and
// __ARGS__, the relay flags, one <string> each.
const relayLaunchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Agent Tincan relay: runs "tincan relay" under launchd.
  "tincan relay install" writes this file into ~/Library/LaunchAgents
  with the real values filled in. It does not load it; start it with:
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.relay.plist
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.agenttincan.relay</string>
  <key>ProgramArguments</key>
  <array>
    <string>__TINCAN_BINARY__</string>
    <string>relay</string>
    __ARGS__
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>__PATH__</string>
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
  <string>__HOME__/Library/Logs/tincan-relay.log</string>
  <key>StandardErrorPath</key>
  <string>__HOME__/Library/Logs/tincan-relay.log</string>
</dict>
</plist>
`

// relaySystemdTemplate is the Linux user unit. Restart=always also brings
// the relay back after tincan relay-upgrade with --upgrade-exit.
const relaySystemdTemplate = `[Unit]
Description=Agent Tincan relay
After=network-online.target

[Service]
ExecStart="__TINCAN_BINARY__" relay__ARGS__
Environment="PATH=__PATH__"
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`

func relayServiceDef(args []string) history.ServiceDef {
	return history.ServiceDef{
		Label:       relayServiceLabel,
		Unit:        relaySystemdUnit,
		Launchd:     relayLaunchdTemplate,
		Systemd:     relaySystemdTemplate,
		Args:        args,
		Unsupported: "no relay service definition for " + runtime.GOOS + "; run tincan relay under your own service manager",
	}
}

func relayInstallCmd() *cobra.Command {
	var f relayFlags
	var binary string
	cmd := &cobra.Command{
		Use:   "install [relay flags]",
		Short: "Write the relay's service definition (launchd on macOS, systemd on Linux); does not start it",
		Long: `Writes a launchd agent (` + relayServiceLabel + `) on macOS, or the systemd user unit
` + relaySystemdUnit + ` on Linux, that runs tincan relay with the flags given here and keeps
it running. Pass the same flags you would pass to tincan relay; flags you leave
out keep the relay's defaults. It prints the command that starts the service
and never starts it itself.

On macOS a release build writes the plist so the service starts through
Agent Tincan.app, and Login Items lists it as "Agent Tincan" rather than under
the signing certificate's name. A relay plist written by hand moves over with
tincan services refresh.

The service never stores TS_AUTHKEY. If the relay has not joined the tailnet
yet, run it once in a terminal with TS_AUTHKEY set, as install prints, before
starting the service.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := f.validate(); err != nil {
				return err
			}
			if err := relayHostErr(runtime.GOOS); err != nil {
				return err
			}
			// The state dir is the relay's identity, so the service always
			// names it: under launchd or systemd the default could resolve
			// elsewhere (systemd --user does not see a shell's
			// XDG_CONFIG_HOME). The service runs from /, so directories
			// given as relative paths are written absolute.
			if err := cmd.Flags().Set("state-dir", f.stateDir); err != nil {
				return err
			}
			for _, name := range []string{"state-dir", "dist"} {
				fl := cmd.Flags().Lookup(name)
				if v := fl.Value.String(); fl.Changed && v != "" && !filepath.IsAbs(v) {
					abs, err := filepath.Abs(v)
					if err != nil {
						return err
					}
					if err := cmd.Flags().Set(name, abs); err != nil {
						return err
					}
				}
			}
			args := relayServiceArgs(cmd.Flags())
			if err := checkServiceArgs(args); err != nil {
				return err
			}
			res, err := history.InstallServiceDef(history.ServiceOptions{Binary: binary}, relayServiceDef(args))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "relay service definition: %s (not started)\n", res.Path)
			if runtime.GOOS == "darwin" {
				if b, err := os.ReadFile(res.Path); err == nil && !strings.Contains(string(b), macapp.BundleID) {
					fmt.Fprintln(out, "It runs tincan directly, not through Agent Tincan.app (a development build, or a --binary the app will not run), so Login Items will show the signer's name; tincan doctor says why.")
				}
				warnOtherRelayAgents(out, binary)
			}
			if relayNeedsJoin(f) {
				exe := binary
				if exe == "" {
					exe, _ = os.Executable()
				}
				fmt.Fprintf(out, "This relay has not joined your tailnet yet, and the service never stores TS_AUTHKEY. Run it once in a terminal first, stop it with Ctrl-C once it is up, then start the service:\n  TS_AUTHKEY=tskey-auth-... %s\n", shellCommand(append([]string{exe, "relay"}, args...)))
			}
			fmt.Fprintf(out, "start it with:\n  %s\n", res.Next)
			return nil
		},
	}
	addRelayFlags(cmd.Flags(), &f)
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	return cmd
}

// relayServiceArgs is the relay flags the operator set on fs, as
// --name=value arguments in flag-name order. Flags left alone are not
// written, so the service keeps the relay's own defaults.
func relayServiceArgs(fs *pflag.FlagSet) []string {
	var args []string
	fs.Visit(func(fl *pflag.Flag) {
		if fl.Name == "binary" {
			return
		}
		if sv, ok := fl.Value.(pflag.SliceValue); ok {
			for _, v := range sv.GetSlice() {
				args = append(args, "--"+fl.Name+"="+csvField(v))
			}
			return
		}
		args = append(args, "--"+fl.Name+"="+fl.Value.String())
	})
	return args
}

// csvField quotes v the way pflag's slice flags read it back, so a value
// holding a comma or a quote stays one element.
func csvField(v string) string {
	if !strings.ContainsAny(v, ",\"\r\n") && strings.TrimSpace(v) == v {
		return v
	}
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

// checkServiceArgs refuses arguments a service definition cannot carry.
func checkServiceArgs(args []string) error {
	for _, a := range args {
		if strings.ContainsAny(a, "\n\r\x00") {
			return fmt.Errorf("%q: a service argument cannot hold a newline or NUL", a)
		}
	}
	return nil
}

// relayNeedsJoin reports whether a tsnet node the relay starts has no
// state yet, so its first start needs TS_AUTHKEY or an interactive login.
func relayNeedsJoin(f relayFlags) bool {
	missing := func(dir string) bool {
		// tsnet keeps the node's key in tailscaled.state once it has logged in.
		_, err := os.Stat(filepath.Join(f.stateDir, dir, "tailscaled.state"))
		return err != nil
	}
	if f.listen == "" && missing("tsnet") {
		return true
	}
	return f.gateway && f.gatewayListen == "" && missing("tsnet-gateway")
}

// warnOtherRelayAgents names LaunchAgents other than the relay service that
// already run tincan relay with this binary: two relays on one state dir
// would fight over it.
func warnOtherRelayAgents(out io.Writer, binary string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	exe := binary
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return
		}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	for _, p := range launchAgentPlists(home, exe) {
		m, err := readPlistJSON(p)
		if err != nil {
			continue
		}
		label, _ := m["Label"].(string)
		if label == "" || label == relayServiceLabel {
			continue
		}
		if j, ok := parseJob(m); ok && j.runs(exe) && len(j.args) > 0 && j.args[0] == "relay" {
			fmt.Fprintf(out, "%s (%s) already runs tincan relay. Start only one: boot it out and delete it before starting this service (launchctl bootout gui/%d/%s), or keep it and run tincan services refresh so it shows as Agent Tincan.\n", label, p, os.Getuid(), label)
		}
	}
}

// shellCommand quotes args for a POSIX shell where they need it.
func shellCommand(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_=./,:@+") == "" {
			q[i] = a
			continue
		}
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}
