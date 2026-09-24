package history

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// ServiceLabel is the launchd label of the history service.
const ServiceLabel = "com.agenttincan.history"

// systemdUnit is the systemd user unit name of the history service.
const systemdUnit = "tincan-history.service"

// launchdTemplate is examples/history/com.agenttincan.history.plist; a
// test keeps the two identical. Placeholders: __TINCAN_BINARY__, __HOME__,
// __PATH__, each XML-escaped when filled.
const launchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Agent Tincan history agent: runs "tincan history serve" under launchd,
  outside any Codex sandbox. "tincan history install" copies this file into
  ~/Library/LaunchAgents with the real paths filled in. It does not load it;
  start it with:
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.history.plist
  See docs/adapters/history.md.
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.agenttincan.history</string>
  <key>ProgramArguments</key>
  <array>
    <string>__TINCAN_BINARY__</string>
    <string>history</string>
    <string>serve</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>TINCAN_CONFIG</key>
    <string>__HOME__/.config/tincan/history.json</string>
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
  <string>__HOME__/Library/Logs/tincan-history.log</string>
  <key>StandardErrorPath</key>
  <string>__HOME__/Library/Logs/tincan-history.log</string>
</dict>
</plist>
`

// systemdTemplate is the Linux user unit. %h is systemd's home specifier.
const systemdTemplate = `[Unit]
Description=Agent Tincan history agent
After=network-online.target

[Service]
ExecStart="__TINCAN_BINARY__" history serve
Environment=TINCAN_CONFIG=%h/.config/tincan/history.json
Environment="PATH=__PATH__"
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`

// basePaths are the OS default bins at the end of a service's PATH. The
// Linux list carries no macOS (Homebrew) paths.
var basePaths = map[string][]string{
	"darwin": {"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"},
	"linux":  {"/usr/local/bin", "/usr/bin", "/bin"},
}

// userBins are per-user bin directories under $HOME where npm, pipx and
// friends put tools like codex, put on a service's PATH before the OS bins.
var userBins = []string{".local/bin", ".npm-global/bin", "bin"}

// ServiceOptions controls InstallService. Zero fields take the current
// user's values.
type ServiceOptions struct {
	GOOS   string
	Home   string
	Binary string
	// CodexDir is put first on the service's PATH so it finds codex (the
	// query extractor). Default: the directory of codex on PATH, if any.
	CodexDir string
	// ClaudeDir is put next on the service's PATH. Default: the directory
	// of claude on PATH, if any.
	ClaudeDir string
	// UID fills the printed launchctl command (default: os.Getuid()).
	UID int
}

// ServiceResult says what InstallService wrote and the command that
// starts it. InstallService never starts or loads the service itself.
type ServiceResult struct {
	Path string
	Next string
}

// InstallService writes the history service definition: a launchd agent
// on macOS, a systemd user unit on Linux. It does not load or enable it.
func InstallService(o ServiceOptions) (ServiceResult, error) {
	if err := o.fill(); err != nil {
		return ServiceResult{}, err
	}
	o.findTools()
	path := servicePath(o.GOOS, o.Home, o.CodexDir, o.ClaudeDir)
	return installServiceDef(o, serviceDef{
		label:       ServiceLabel,
		unit:        systemdUnit,
		launchd:     launchdTemplate,
		systemd:     systemdTemplate,
		vars:        []string{"__TINCAN_BINARY__", o.Binary, "__HOME__", o.Home, "__PATH__", path},
		unsupported: "no history service definition for " + o.GOOS + "; run tincan history serve under your own service manager",
	})
}

// findTools fills CodexDir and ClaudeDir from where codex and claude are
// found on PATH at install time, since a service does not get the login
// shell's PATH.
func (o *ServiceOptions) findTools() {
	if o.CodexDir == "" {
		if p, err := exec.LookPath("codex"); err == nil {
			o.CodexDir = filepath.Dir(p)
		}
	}
	if o.ClaudeDir == "" {
		if p, err := exec.LookPath("claude"); err == nil {
			o.ClaudeDir = filepath.Dir(p)
		}
	}
}

// fill defaults o's empty fields to the running system and checks the
// binary.
func (o *ServiceOptions) fill() error {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		o.Home = h
	}
	if o.Binary == "" {
		b, err := os.Executable()
		if err != nil {
			return err
		}
		o.Binary = b
	}
	if err := validateServiceBinary(o.Binary); err != nil {
		return err
	}
	if o.UID == 0 {
		o.UID = os.Getuid()
	}
	return nil
}

// validateServiceBinary refuses a binary path that cannot be written
// safely into a plist or a systemd ExecStart line.
func validateServiceBinary(b string) error {
	if !filepath.IsAbs(b) || strings.ContainsAny(b, "\"\n\r\x00%") {
		return fmt.Errorf("service binary %q must be an absolute path without quotes, %% or newlines", b)
	}
	return nil
}

// serviceDef is one service definition: its launchd label and plist
// template, its systemd unit name and template, and the placeholder/value
// pairs both templates are filled with (XML-escaped for the plist).
type serviceDef struct {
	label, unit      string
	launchd, systemd string
	vars             []string
	unsupported      string
}

// installServiceDef writes d for o.GOOS: the launchd agent in
// ~/Library/LaunchAgents (making ~/Library/Logs for its log) or the systemd
// user unit, and returns the command that starts it.
func installServiceDef(o ServiceOptions, d serviceDef) (ServiceResult, error) {
	switch o.GOOS {
	case "darwin":
		dst := filepath.Join(o.Home, "Library", "LaunchAgents", d.label+".plist")
		esc := make([]string, len(d.vars))
		for i, v := range d.vars {
			if i%2 == 1 {
				v = xmlEscape(v)
			}
			esc[i] = v
		}
		body := strings.NewReplacer(esc...).Replace(d.launchd)
		if err := os.MkdirAll(filepath.Join(o.Home, "Library", "Logs"), 0o755); err != nil {
			return ServiceResult{}, err
		}
		if err := writeService(dst, body); err != nil {
			return ServiceResult{}, err
		}
		return ServiceResult{Path: dst, Next: fmt.Sprintf("launchctl bootstrap gui/%d %s", o.UID, dst)}, nil
	case "linux":
		dst := filepath.Join(o.Home, ".config", "systemd", "user", d.unit)
		esc := make([]string, len(d.vars))
		for i, v := range d.vars {
			if i%2 == 1 {
				v = systemdEscape(v)
			}
			esc[i] = v
		}
		body := strings.NewReplacer(esc...).Replace(d.systemd)
		if err := writeService(dst, body); err != nil {
			return ServiceResult{}, err
		}
		return ServiceResult{Path: dst, Next: "systemctl --user daemon-reload && systemctl --user enable --now " + d.unit}, nil
	}
	return ServiceResult{}, errors.New(d.unsupported)
}

// servicePath is the PATH a service runs with on goos: the tool
// directories found at install time, then the per-user bins under home,
// then the OS default bins. The result is raw; installServiceDef escapes it
// for the plist (XML) or the quoted systemd Environment line, so spaces,
// %, $ and backslashes are kept. Only directories that cannot be written
// at all are skipped: relative ones, and those holding a colon (the PATH
// separator), a newline, a NUL or a double quote. None repeats.
func servicePath(goos, home string, toolDirs ...string) string {
	var dirs []string
	dirs = append(dirs, toolDirs...)
	if home != "" {
		for _, b := range userBins {
			dirs = append(dirs, filepath.Join(home, b))
		}
	}
	base, ok := basePaths[goos]
	if !ok {
		base = basePaths["linux"]
	}
	dirs = append(dirs, base...)
	var parts []string
	for _, d := range dirs {
		if d == "" || !filepath.IsAbs(d) || strings.ContainsAny(d, ":\n\r\x00\"") || slices.Contains(parts, d) {
			continue
		}
		parts = append(parts, d)
	}
	return strings.Join(parts, ":")
}

func writeService(dst, body string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(dst, []byte(body), 0o644)
}

// systemdEscape makes s safe inside a double-quoted systemd value (the
// ExecStart binary, the Environment="PATH=..." assignment): backslashes
// and quotes are escaped and % is doubled so it is not read as a
// specifier.
func systemdEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(s)
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// webLaunchdTemplate is the launchd agent for a web agent. Placeholders:
// __TINCAN_BINARY__, __HOME__, __PATH__, __SITE__, __AGENT__, each
// XML-escaped when filled.
const webLaunchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Agent Tincan web agent (__AGENT__): runs "tincan web serve" for the
  __SITE__ site under launchd (no double hyphen may appear in an XML comment). "tincan web install" writes this file into
  ~/Library/LaunchAgents. It does not load it; start it with:
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.web.__SITE__.plist
  See docs/adapters/web-agents.md.
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.agenttincan.web.__SITE__</string>
  <key>ProgramArguments</key>
  <array>
    <string>__TINCAN_BINARY__</string>
    <string>web</string>
    <string>serve</string>
    <string>--site</string>
    <string>__SITE__</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>TINCAN_CONFIG</key>
    <string>__HOME__/.config/tincan/__AGENT__.json</string>
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
  <string>__HOME__/Library/Logs/tincan-__AGENT__.log</string>
  <key>StandardErrorPath</key>
  <string>__HOME__/Library/Logs/tincan-__AGENT__.log</string>
</dict>
</plist>
`

// webSystemdTemplate is the Linux user unit for a web agent.
const webSystemdTemplate = `[Unit]
Description=Agent Tincan web agent (__AGENT__)
After=network-online.target

[Service]
ExecStart="__TINCAN_BINARY__" web serve --site __SITE__
Environment=TINCAN_CONFIG=%h/.config/tincan/__AGENT__.json
Environment="PATH=__PATH__"
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`

// WebServiceLabel is the launchd label of a site's web agent.
func WebServiceLabel(site Source) string { return "com.agenttincan.web." + string(site) }

// InstallWebService writes the service definition for a site's web agent
// (a launchd agent on macOS, a systemd user unit on Linux). Like
// InstallService it never loads or starts it.
func InstallWebService(site Source, o ServiceOptions) (ServiceResult, error) {
	if _, err := ParseWebSite(string(site)); err != nil {
		return ServiceResult{}, err
	}
	if err := o.fill(); err != nil {
		return ServiceResult{}, err
	}
	agent := WebAgentName(site)
	o.findTools()
	path := servicePath(o.GOOS, o.Home, o.CodexDir, o.ClaudeDir)
	return installServiceDef(o, serviceDef{
		label:       WebServiceLabel(site),
		unit:        "tincan-" + agent + ".service",
		launchd:     webLaunchdTemplate,
		systemd:     webSystemdTemplate,
		vars:        []string{"__TINCAN_BINARY__", o.Binary, "__HOME__", o.Home, "__PATH__", path, "__SITE__", string(site), "__AGENT__", agent},
		unsupported: "no web agent service definition for " + o.GOOS + "; run tincan web serve under your own service manager",
	})
}
