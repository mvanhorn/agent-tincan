package notes

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// ServiceLabel is the launchd label of the notes service.
const ServiceLabel = "com.agenttincan.notes"

// systemdUnit is the systemd user unit name of the notes service.
const systemdUnit = "tincan-notes.service"

// healthFile is the health file's name in the Application Support root.
const healthFile = "tincan-notes-health.json"

// launchdTemplate is the notes service's launchd agent. Placeholders:
// __TINCAN_BINARY__, __HOME__, __PATH__, __LIBRARY_ROOT__, __HELPER__, each
// XML-escaped when filled.
const launchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Agent Tincan notes agent: runs "tincan notes serve" under launchd.
  "tincan notes install" writes this file into ~/Library/LaunchAgents with
  the real paths filled in. It does not load it; start it with:
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.notes.plist
  See docs/adapters/notes.md.
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.agenttincan.notes</string>
  <key>ProgramArguments</key>
  <array>
    <string>__TINCAN_BINARY__</string>
    <string>notes</string>
    <string>serve</string>
    <string>--library-root</string>
    <string>__LIBRARY_ROOT__</string>
    <string>--helper</string>
    <string>__HELPER__</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>TINCAN_CONFIG</key>
    <string>__HOME__/.config/tincan/notes.json</string>
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
  <string>__HOME__/Library/Logs/tincan-notes.log</string>
  <key>StandardErrorPath</key>
  <string>__HOME__/Library/Logs/tincan-notes.log</string>
</dict>
</plist>
`

// systemdTemplate is the Linux user unit. %h is systemd's home specifier.
const systemdTemplate = `[Unit]
Description=Agent Tincan notes agent
After=network-online.target

[Service]
ExecStart="__TINCAN_BINARY__" notes serve --library-root "__LIBRARY_ROOT__" --helper "__HELPER__"
Environment=TINCAN_CONFIG=%h/.config/tincan/notes.json
Environment="PATH=__PATH__"
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`

// ServiceOptions controls InstallService. Backend defaults to agent-notes,
// which needs LibraryRoot; HelperPath defaults to DefaultHelperPath. The
// other backends take neither.
type ServiceOptions struct {
	history.ServiceOptions
	Backend     string
	LibraryRoot string
	HelperPath  string
}

// The agent-notes arguments in each template, and what replaces them for
// another backend.
const (
	launchdHelperArgs = `    <string>--library-root</string>
    <string>__LIBRARY_ROOT__</string>
    <string>--helper</string>
    <string>__HELPER__</string>
`
	launchdBackendArgs = `    <string>--backend</string>
    <string>__BACKEND__</string>
`
	systemdHelperArgs  = `--library-root "__LIBRARY_ROOT__" --helper "__HELPER__"`
	systemdBackendArgs = `--backend "__BACKEND__"`
)

// InstallService writes the notes service definition, which runs
// tincan notes serve against o.LibraryRoot as the agent in
// ~/.config/tincan/notes.json. It never loads or starts it.
func InstallService(o ServiceOptions) (history.ServiceResult, error) {
	if o.Backend == "" {
		o.Backend = BackendAgentNotes
	}
	if err := CheckBackendName(o.Backend); err != nil {
		return history.ServiceResult{}, err
	}
	goos := o.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	launchd, systemd := launchdTemplate, systemdTemplate
	var vars []string
	if o.Backend == BackendAgentNotes {
		if o.HelperPath == "" {
			o.HelperPath = DefaultHelperPath
		}
		for flag, p := range map[string]string{"library root": o.LibraryRoot, "helper": o.HelperPath} {
			if err := checkServicePath(goos, flag, p); err != nil {
				return history.ServiceResult{}, err
			}
		}
		vars = []string{"__LIBRARY_ROOT__", o.LibraryRoot, "__HELPER__", o.HelperPath}
	} else {
		if goos != "darwin" {
			return history.ServiceResult{}, fmt.Errorf("the %s backend runs only on macOS", o.Backend)
		}
		launchd = strings.Replace(launchd, launchdHelperArgs, launchdBackendArgs, 1)
		systemd = strings.Replace(systemd, systemdHelperArgs, systemdBackendArgs, 1)
		vars = []string{"__BACKEND__", o.Backend}
	}
	return history.InstallServiceDef(o.ServiceOptions, history.ServiceDef{
		Label:       ServiceLabel,
		Unit:        systemdUnit,
		Launchd:     launchd,
		Systemd:     systemd,
		Vars:        vars,
		Unsupported: "no notes service definition for " + goos + "; run tincan notes serve under your own service manager",
		NoLauncher:  true,
	})
}

// checkServicePath refuses a path the service definition cannot carry
// faithfully: relative ones, control characters, and on Linux a $, which
// systemd would expand.
func checkServicePath(goos, what, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("no %s given", what)
	case !filepath.IsAbs(p):
		return fmt.Errorf("%s %q must be an absolute path", what, p)
	case strings.ContainsAny(p, "\n\r\x00"):
		return fmt.Errorf("%s %q holds a control character", what, p)
	case goos == "linux" && strings.Contains(p, "$"):
		return fmt.Errorf("%s %q holds a $, which systemd would expand", what, p)
	}
	return nil
}

// DefaultAppSupportRoot is the service's own Application Support root on
// goos: ~/Library/Application Support/tincan-notes on macOS, else
// ~/.local/share/tincan-notes. It is tincan's, never the app's, so the
// helper never reads an in-app agent session's context (KTD8).
func DefaultAppSupportRoot(goos, home string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "tincan-notes")
	}
	return filepath.Join(home, ".local", "share", "tincan-notes")
}

// HealthPathIn is the health file the service writes in appSupport.
func HealthPathIn(appSupport string) string { return filepath.Join(appSupport, healthFile) }

// SpoolDirIn is the default spool directory in appSupport.
func SpoolDirIn(appSupport string) string { return filepath.Join(appSupport, "spool") }
