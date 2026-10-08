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

	"github.com/mvanhorn/agent-tincan/internal/macapp"
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
	// Thread is a web agent's fixed thread (--thread), for a site whose
	// agent serves one (dots); InstallWebService requires it there and
	// refuses it elsewhere.
	Thread string
	// Launcher is the Agent Tincan.app launcher a macOS service starts
	// through, so Login Items shows "Agent Tincan" rather than the signer's
	// name. Default: install the app this build embeds and use it; builds
	// without it write the plain plist.
	Launcher string
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
	goos := o.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	return InstallServiceDef(o, ServiceDef{
		Label:       ServiceLabel,
		Unit:        systemdUnit,
		Launchd:     launchdTemplate,
		Systemd:     systemdTemplate,
		Unsupported: "no history service definition for " + goos + "; run tincan history serve under your own service manager",
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

// ServiceDef is another package's service definition for InstallServiceDef.
// Its templates may use __TINCAN_BINARY__, __HOME__ and __PATH__, filled in
// from the options, and the placeholder/value pairs in Vars.
type ServiceDef struct {
	Label, Unit      string
	Launchd, Systemd string
	Vars             []string
	Unsupported      string
	// NoLauncher keeps the plist running the tincan binary directly. The
	// notes service sets it: macOS may attribute a privacy grant to the
	// app, which would let every wrapped service share notes' folder access.
	NoLauncher bool
}

// InstallServiceDef writes d the way InstallService writes the history
// service: a launchd agent on macOS, a systemd user unit on Linux, with the
// same PATH. It does not load or enable it.
func InstallServiceDef(o ServiceOptions, d ServiceDef) (ServiceResult, error) {
	if err := o.fill(); err != nil {
		return ServiceResult{}, err
	}
	o.findTools()
	path := servicePath(o.GOOS, o.Home, o.CodexDir, o.ClaudeDir)
	launcher := ""
	if o.GOOS == "darwin" && !d.NoLauncher {
		launcher = o.Launcher
		if launcher == "" {
			l, err := macapp.Ensure(macapp.Options{Home: o.Home, GOOS: o.GOOS})
			switch {
			case err == nil:
				launcher = l
			case !errors.Is(err, macapp.ErrUnavailable):
				return ServiceResult{}, fmt.Errorf("install Agent Tincan.app: %w", err)
			}
		}
	}
	return installServiceDef(o, serviceDef{
		launcher:    launcher,
		label:       d.Label,
		unit:        d.Unit,
		launchd:     d.Launchd,
		systemd:     d.Systemd,
		vars:        append([]string{"__TINCAN_BINARY__", o.Binary, "__HOME__", o.Home, "__PATH__", path}, d.Vars...),
		unsupported: d.Unsupported,
	})
}

// serviceDef is one service definition: its launchd label and plist
// template, its systemd unit name and template, and the placeholder/value
// pairs both templates are filled with (XML-escaped for the plist).
type serviceDef struct {
	launcher         string
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
		if d.launcher != "" {
			var err error
			if body, err = wrapLaunchd(body, d.launcher); err != nil {
				return ServiceResult{}, fmt.Errorf("%s: %w", d.label, err)
			}
		}
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

// wrapLaunchd makes a rendered plist start through launcher: the launcher
// becomes the first program argument, ahead of the tincan binary, and the
// app's bundle id is named in AssociatedBundleIdentifiers.
func wrapLaunchd(body, launcher string) (string, error) {
	const args, end = "<key>ProgramArguments</key>\n  <array>\n", "</dict>\n</plist>\n"
	i := strings.Index(body, args)
	if i < 0 || !strings.HasSuffix(body, end) {
		return "", errors.New("launchd template has no ProgramArguments array to wrap")
	}
	i += len(args)
	body = body[:i] + "    <string>" + xmlEscape(launcher) + "</string>\n" + body[i:]
	assoc := "  <key>AssociatedBundleIdentifiers</key>\n  <array>\n    <string>" + macapp.BundleID + "</string>\n  </array>\n"
	return strings.TrimSuffix(body, end) + assoc + end, nil
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
    <string>__SITE__</string>__THREADARGS__
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
ExecStart="__TINCAN_BINARY__" web serve --site __SITE____THREADARGS__
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
	goos := o.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	// A fixed thread goes into the definition as --thread <id>; the id
	// is checked to hex and hyphens, and filled (escaped) like any value.
	launchd, systemd := webLaunchdTemplate, webSystemdTemplate
	launchdThread, systemdThread := "", ""
	thread := ""
	switch {
	case WebSiteTakesThread(site):
		id, ok := ParseWebThread(site, o.Thread)
		if !ok {
			return ServiceResult{}, fmt.Errorf("--site %s needs --thread <id>, the id in https://chatgpt.com/dots/<id> (got %q)", site, o.Thread)
		}
		thread = id
		launchdThread = "\n    <string>--thread</string>\n    <string>__THREAD__</string>"
		systemdThread = " --thread __THREAD__"
	case o.Thread != "":
		return ServiceResult{}, fmt.Errorf("--thread applies only to --site dots, not %s", site)
	}
	launchd = strings.Replace(launchd, "__THREADARGS__", launchdThread, 1)
	systemd = strings.Replace(systemd, "__THREADARGS__", systemdThread, 1)
	agent := WebAgentName(site)
	return InstallServiceDef(o, ServiceDef{
		Label:       WebServiceLabel(site),
		Unit:        "tincan-" + agent + ".service",
		Launchd:     launchd,
		Systemd:     systemd,
		Vars:        []string{"__SITE__", string(site), "__AGENT__", agent, "__THREAD__", thread},
		Unsupported: "no web agent service definition for " + goos + "; run tincan web serve under your own service manager",
	})
}
