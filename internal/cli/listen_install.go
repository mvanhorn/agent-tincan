package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/examples"
	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// listenServicePrefix is the launchd label prefix of listener services; the
// agent's name completes it, so each agent on a machine has its own.
const listenServicePrefix = "com.agenttincan.listen."

// listenStampFile records, beside the installed scripts, the SHA-256 of each
// file as tincan wrote it. A file whose bytes no longer match was edited by
// its owner and is never overwritten.
const listenStampFile = "installed.json"

// listenLaunchdTemplate is a listener's launchd agent. Placeholders:
// __TINCAN_BINARY__, __HOME__, __PATH__, __AGENT__ and __TINCAN_CONFIG__,
// XML-escaped when filled, and __ARGS__, the listen flags.
const listenLaunchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Agent Tincan listener for __AGENT__: runs "tincan listen --exec <wake script>"
  under launchd. "tincan listen install" writes this file into
  ~/Library/LaunchAgents with the real values filled in. It does not load it.
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.agenttincan.listen.__AGENT__</string>
  <key>ProgramArguments</key>
  <array>
    <string>__TINCAN_BINARY__</string>
    <string>listen</string>
    __ARGS__
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>__PATH__</string>
    <key>TINCAN_CONFIG</key>
    <string>__TINCAN_CONFIG__</string>
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
  <string>__HOME__/Library/Logs/tincan-listen-__AGENT__.log</string>
  <key>StandardErrorPath</key>
  <string>__HOME__/Library/Logs/tincan-listen-__AGENT__.log</string>
</dict>
</plist>
`

// listenSystemdTemplate is a listener's Linux user unit.
const listenSystemdTemplate = `[Unit]
Description=Agent Tincan listener for __AGENT__
After=network-online.target

[Service]
ExecStart="__TINCAN_BINARY__" listen__ARGS__
Environment="PATH=__PATH__"
Environment="TINCAN_CONFIG=__TINCAN_CONFIG__"
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`

func listenLabel(agent string) string { return listenServicePrefix + agent }
func listenUnit(agent string) string  { return "tincan-listen-" + agent + ".service" }

// listenDir is the folder beside an agent's config that holds its wake
// scripts, so two agents on one machine never share one.
func listenDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), strings.TrimSuffix(filepath.Base(configPath), ".json")+".listen")
}

// listenExecArg is the --exec value for a script: tincan listen runs it
// through sh -c, so the path is quoted to stay one word.
func listenExecArg(script string) string { return shellQuote(script) }

func listenServiceDef(agent, script, configPath string) history.ServiceDef {
	return history.ServiceDef{
		Label:       listenLabel(agent),
		Unit:        listenUnit(agent),
		Launchd:     listenLaunchdTemplate,
		Systemd:     listenSystemdTemplate,
		Vars:        []string{"__AGENT__", agent, "__TINCAN_CONFIG__", configPath},
		Args:        []string{"--exec", listenExecArg(script)},
		Unsupported: "tincan listen install writes launchd agents (macOS) and systemd user units (Linux); run tincan listen under your own service manager",
	}
}

type wakeStamp struct {
	Wake  string            `json:"wake"`
	Agent string            `json:"agent"`
	Files map[string]string `json:"files"`
}

func readWakeStamp(dir string) (wakeStamp, error) {
	var st wakeStamp
	b, err := os.ReadFile(filepath.Join(dir, listenStampFile))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %w", filepath.Join(dir, listenStampFile), err)
	}
	if st.Files == nil {
		st.Files = map[string]string{}
	}
	return st, nil
}

func writeWakeStamp(dir string, st wakeStamp) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileReplacing(filepath.Join(dir, listenStampFile), append(b, '\n'), 0o644)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// wakeFiles is what a wake installs: its script and, when it sources it, the
// shared library.
func wakeFiles(w examples.WakeScript) map[string][]byte {
	files := map[string][]byte{w.File: w.Script}
	if w.NeedsLib {
		files[examples.WakeLibName] = examples.WakeLib()
	}
	return files
}

type listenInstallOptions struct {
	GOOS, Home, Binary string
	UID                int
	Launcher           string
	ConfigPath         string // the agent's config, absolute
	Agent              string
	DefaultConfig      string // the config tincan uses without TINCAN_CONFIG
	CodexConfig        string
	Out                io.Writer
}

// installListener writes the wake's files beside the agent's config, the
// listener service, and for Codex the approval mode on its MCP entry. It
// prints what a woken agent may do before writing anything, and never
// starts the service.
func installListener(o listenInstallOptions, w examples.WakeScript) error {
	if o.Agent == "" {
		return errors.New("this machine has not joined the team as an agent: run tincan join first, with TINCAN_CONFIG set as for this agent's other tincan commands")
	}
	if !identity.ValidName(o.Agent) {
		return fmt.Errorf("agent name %q cannot name a service", o.Agent)
	}
	if o.GOOS != "darwin" && o.GOOS != "linux" {
		return errors.New(listenServiceDef(o.Agent, "", o.ConfigPath).Unsupported)
	}
	out := o.Out
	fmt.Fprintf(out, "Before you start it: %s\n\n", w.Notice)
	if _, err := exec.LookPath(w.Tool); err != nil {
		fmt.Fprintf(out, "Note: %s is not on this PATH. The service looks for it on the PATH it is written with; install it, or put its folder on your PATH and run this again.\n", w.Tool)
	}

	dir := listenDir(o.ConfigPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	prior, err := readWakeStamp(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	st := wakeStamp{Wake: w.Name, Agent: o.Agent, Files: map[string]string{}}
	for name, body := range wakeFiles(w) {
		path := filepath.Join(dir, name)
		cur, rerr := os.ReadFile(path)
		switch {
		case rerr == nil && sha256Hex(cur) == sha256Hex(body):
			st.Files[name] = sha256Hex(body)
			fmt.Fprintf(out, "current %s\n", path)
			continue
		case rerr == nil && (prior.Files[name] == "" || sha256Hex(cur) != prior.Files[name]):
			// Not what tincan last wrote: the owner's file stays.
			st.Files[name] = prior.Files[name]
			if st.Files[name] == "" {
				st.Files[name] = sha256Hex(body)
			}
			fmt.Fprintf(out, "left %s: it differs from what tincan wrote, so your version was kept. This release's version is in the repo at examples/%s.\n", path, examplePath(w, name))
			continue
		}
		if err := writeFileReplacing(path, body, 0o755); err != nil {
			return err
		}
		st.Files[name] = sha256Hex(body)
		fmt.Fprintf(out, "wrote %s\n", path)
	}
	if err := writeWakeStamp(dir, st); err != nil {
		return err
	}

	script := filepath.Join(dir, w.File)
	res, err := history.InstallServiceDef(history.ServiceOptions{GOOS: o.GOOS, Home: o.Home, Binary: o.Binary, UID: o.UID, Launcher: o.Launcher},
		listenServiceDef(o.Agent, script, o.ConfigPath))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "listener service definition: %s (not started)\n", res.Path)

	if w.Name == "codex" {
		changed, notice, err := setCodexApproval(o.CodexConfig, o.Binary, o.ConfigPath, o.DefaultConfig)
		switch {
		case err != nil:
			fmt.Fprintf(out, "Could not update %s (%v). Add default_tools_approval_mode = \"approve\" to its [mcp_servers.agent-tincan] table yourself.\n", o.CodexConfig, err)
		case notice != "":
			fmt.Fprintln(out, notice)
		case changed:
			fmt.Fprintf(out, "set default_tools_approval_mode = \"approve\" on the agent-tincan entry in %s\n", o.CodexConfig)
		}
	}

	fmt.Fprintf(out, "\nOn the relay host, add %q: {\"method\": \"command\"} to wake.json in the relay's state directory (keep it chmod 600), then restart the relay, so teammates see that %s is woken by command.\n", o.Agent, o.Agent)
	fmt.Fprintf(out, "start it with:\n  %s\n", res.Next)
	return nil
}

// examplePath is a wake file's path under examples/ in the repo.
func examplePath(w examples.WakeScript, name string) string {
	if name == examples.WakeLibName {
		return "lib/" + name
	}
	return w.Source
}

var (
	tomlAnyTable    = regexp.MustCompile(`^\s*\[`)
	tomlApprovalKey = regexp.MustCompile(`^\s*default_tools_approval_mode\s*=`)
	tomlEnvConfig   = regexp.MustCompile(`TINCAN_CONFIG\s*=\s*"((?:[^"\\]|\\.)*)"`)
)

// codexEntry is the agent-tincan table of a Codex config.toml: its line
// range (header to before the next table that is not its own sub-table),
// whether it sets the approval mode to approve, and the TINCAN_CONFIG it
// pins ("" when it pins none, meaning the default config).
type codexEntry struct {
	start, end int
	approve    bool
	hasKey     bool
	config     string
}

func findCodexEntry(lines []string) (codexEntry, bool) {
	e := codexEntry{start: -1}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if e.start < 0 {
			if m := tomlSection.FindStringSubmatch(trimmed); m != nil && m[1]+m[2] == "agent-tincan" {
				e.start, e.end = i, len(lines)
			}
			continue
		}
		if tomlAnyTable.MatchString(line) && !strings.HasPrefix(trimmed, "[mcp_servers.agent-tincan.") && !strings.HasPrefix(trimmed, `[mcp_servers."agent-tincan".`) {
			e.end = i
			break
		}
		if tomlApprovalKey.MatchString(line) {
			e.hasKey = true
			e.approve = strings.Contains(strings.ReplaceAll(line, " ", ""), `="approve"`)
		}
		if m := tomlEnvConfig.FindStringSubmatch(line); m != nil {
			e.config = strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(m[1])
		}
	}
	return e, e.start >= 0
}

// sameConfig reports whether an MCP entry's TINCAN_CONFIG value names cfg.
// No value means the default config.
func sameConfig(pinned, cfg, def string) bool {
	if pinned == "" {
		pinned = def
	}
	if rest, ok := strings.CutPrefix(pinned, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			pinned = filepath.Join(home, rest)
		}
	}
	return filepath.Clean(pinned) == filepath.Clean(cfg)
}

func tomlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// setCodexApproval sets default_tools_approval_mode = "approve" on the
// agent-tincan entry of Codex's config.toml, line by line so the owner's
// comments and order stay, or adds the entry when none names tincan. It
// changes nothing, and returns a notice saying what to do, when a tincan
// entry has another name or the entry pins another agent's config: a wake
// would then act as that agent.
func setCodexApproval(path, binary, cfg, def string) (changed bool, notice string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, "", err
	}
	mode := os.FileMode(0o600)
	if fi, serr := os.Stat(path); serr == nil {
		mode = fi.Mode().Perm()
	}
	s := string(raw)
	lines := strings.Split(s, "\n")
	e, ok := findCodexEntry(lines)
	if !ok {
		for _, other := range tomlServers(s) {
			return false, fmt.Sprintf("%s has a tincan entry named %s, not agent-tincan, so it was left alone. Add default_tools_approval_mode = \"approve\" to that table, or rename it agent-tincan and run this again.", path, other.Name), nil
		}
		var b strings.Builder
		b.WriteString(s)
		if s != "" && !strings.HasSuffix(s, "\n") {
			b.WriteString("\n")
		}
		if s != "" {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[mcp_servers.agent-tincan]\ncommand = %s\nargs = [\"mcp\"]\nenv = { TINCAN_CONFIG = %s }\ndefault_tools_approval_mode = \"approve\"\n", tomlQuote(binary), tomlQuote(cfg))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return false, "", err
		}
		return true, "", writeFileReplacing(path, []byte(b.String()), mode)
	}
	if !sameConfig(e.config, cfg, def) {
		pinned := e.config
		if pinned == "" {
			pinned = "the default config (" + def + ")"
		}
		return false, fmt.Sprintf("The agent-tincan entry in %s runs tincan as %s, not %s, so a wake would act as another agent. It was left alone: point its TINCAN_CONFIG at %s, add default_tools_approval_mode = \"approve\", and run this again.", path, pinned, cfg, cfg), nil
	}
	if e.approve {
		return false, "", nil
	}
	const keyLine = `default_tools_approval_mode = "approve"`
	if e.hasKey {
		for i := e.start + 1; i < e.end; i++ {
			if tomlApprovalKey.MatchString(lines[i]) {
				lines[i] = keyLine
			}
		}
	} else {
		lines = append(lines[:e.start+1], append([]string{keyLine}, lines[e.start+1:]...)...)
	}
	return true, "", writeFileReplacing(path, []byte(strings.Join(lines, "\n")), mode)
}

// codexConfigPath is Codex's config file: $CODEX_HOME/config.toml, else
// ~/.codex/config.toml.
func codexConfigPath(home string) string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "config.toml")
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// defaultConfigPath is the config tincan uses when TINCAN_CONFIG is unset.
func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "tincan", "client.json")
}

var (
	plistConfigRE   = regexp.MustCompile(`<key>TINCAN_CONFIG</key>\s*<string>([^<]*)</string>`)
	systemdConfigRE = regexp.MustCompile(`(?m)^Environment="TINCAN_CONFIG=((?:[^"\\]|\\.)*)"`)
)

// installedListenerConfigs finds the config of every installed listener
// from its service file, wherever that config lives.
func installedListenerConfigs(home, goos string) []string {
	var files []string
	var re *regexp.Regexp
	switch goos {
	case "darwin":
		files, _ = filepath.Glob(filepath.Join(home, "Library", "LaunchAgents", listenServicePrefix+"*.plist"))
		re = plistConfigRE
	case "linux":
		files, _ = filepath.Glob(filepath.Join(home, ".config", "systemd", "user", "tincan-listen-*.service"))
		re = systemdConfigRE
	default:
		return nil
	}
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		m := re.FindSubmatch(b)
		if m == nil {
			continue
		}
		v := string(m[1])
		if goos == "darwin" {
			v = html.UnescapeString(v)
		} else {
			v = strings.NewReplacer(`%%`, `%`, `\"`, `"`, `\\`, `\`).Replace(v)
		}
		out = append(out, v)
	}
	return out
}

// refreshWakeScripts brings every installed listener's scripts up to this
// release, replacing only files whose bytes still match what tincan wrote.
// An edited file is left alone and named; a missing one is named, not
// recreated, since the owner may have removed it on purpose.
func refreshWakeScripts(out io.Writer, home, goos string) error {
	var errs []error
	for _, cfg := range installedListenerConfigs(home, goos) {
		dir := listenDir(cfg)
		st, err := readWakeStamp(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		w, err := examples.Wake(st.Wake)
		if err != nil {
			fmt.Fprintf(out, "%s: %v\n", dir, err)
			continue
		}
		files := wakeFiles(w)
		changed := false
		for name, stamped := range st.Files {
			body, ok := files[name]
			if !ok {
				continue
			}
			path := filepath.Join(dir, name)
			cur, err := os.ReadFile(path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				fmt.Fprintf(out, "missing %s: not recreated; run tincan listen install --wake %s again to write it\n", path, st.Wake)
				continue
			case err != nil:
				errs = append(errs, err)
				continue
			case sha256Hex(cur) != stamped:
				fmt.Fprintf(out, "left %s: you edited it, so this release's version was not written (it is in the repo at examples/%s)\n", path, examplePath(w, name))
				continue
			case stamped == sha256Hex(body):
				continue
			}
			if err := writeFileReplacing(path, body, 0o755); err != nil {
				errs = append(errs, err)
				continue
			}
			st.Files[name] = sha256Hex(body)
			changed = true
			fmt.Fprintf(out, "updated %s\n", path)
		}
		if changed {
			if err := writeWakeStamp(dir, st); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// listenerCheck reports the listener installed for the agent whose config
// is cfg: its service file, whether launchd has it loaded, whether its
// scripts are current, and for Codex the agent-tincan entry. It returns a
// zero check, which doctor skips, when no listener is installed.
func listenerCheck(goos, home, cfg, codexCfg, def string, loaded func(label string) bool) check {
	const name = "listener"
	dir := listenDir(cfg)
	st, err := readWakeStamp(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return check{}
	}
	if err != nil {
		return check{name, "warn", err.Error(), "Run tincan listen install again."}
	}
	reinstall := fmt.Sprintf("Run tincan listen install --wake %s again.", st.Wake)
	status := "ok"
	detail := []string{fmt.Sprintf("%s wake for %s in %s", st.Wake, st.Agent, dir)}
	var fix []string
	warn := func(d, f string) {
		status = "warn"
		detail = append(detail, d)
		if f != "" && !slices.Contains(fix, f) {
			fix = append(fix, f)
		}
	}

	switch goos {
	case "darwin":
		plist := filepath.Join(home, "Library", "LaunchAgents", listenLabel(st.Agent)+".plist")
		if _, err := os.Stat(plist); err != nil {
			warn("no service file at "+plist, reinstall)
		} else if !loaded(listenLabel(st.Agent)) {
			warn("its service is not loaded", fmt.Sprintf("Start it with launchctl bootstrap gui/%d %s", os.Getuid(), plist))
		}
	case "linux":
		unit := filepath.Join(home, ".config", "systemd", "user", listenUnit(st.Agent))
		if _, err := os.Stat(unit); err != nil {
			warn("no service file at "+unit, reinstall)
		}
	}

	w, werr := examples.Wake(st.Wake)
	for file, stamped := range st.Files {
		path := filepath.Join(dir, file)
		cur, err := os.ReadFile(path)
		switch {
		case err != nil:
			warn("missing "+path, reinstall)
		case sha256Hex(cur) != stamped:
			detail = append(detail, path+" edited by you; tincan upgrade leaves it alone")
		case werr == nil && stamped != sha256Hex(wakeFiles(w)[file]) && wakeFiles(w)[file] != nil:
			warn(path+" is from an older release", fmt.Sprintf("Run tincan upgrade, or tincan listen install --wake %s again, to update it.", st.Wake))
		}
	}

	if st.Wake == "codex" {
		raw, err := os.ReadFile(codexCfg)
		if err != nil {
			warn("no Codex config at "+codexCfg, reinstall)
		} else if e, ok := findCodexEntry(strings.Split(string(raw), "\n")); !ok {
			warn("no agent-tincan entry in "+codexCfg, reinstall)
		} else {
			if !sameConfig(e.config, cfg, def) {
				pinned := e.config
				if pinned == "" {
					pinned = def
				}
				warn("the agent-tincan entry in "+codexCfg+" runs tincan as "+pinned+", so wakes act as another agent", "Point its TINCAN_CONFIG at "+cfg+".")
			}
			if !e.approve {
				warn("the agent-tincan entry in "+codexCfg+" does not set default_tools_approval_mode = \"approve\", so unattended wakes cannot call the tincan tools", reinstall)
			}
		}
	}
	return check{name, status, strings.Join(detail, "; "), strings.Join(fix, " ")}
}

func listenInstallCmd() *cobra.Command {
	var wake, binary string
	var refresh bool
	cmd := &cobra.Command{
		Use:   "install --wake <" + strings.Join(examples.WakeNames(), "|") + ">",
		Short: "Write a wake script and a login service that keeps tincan listen running for this agent; does not start it",
		Long: `Writes the wake script for --wake from this release beside this agent's
config, a launchd agent (` + listenServicePrefix + `<agent>) on macOS or a systemd user unit
(tincan-listen-<agent>.service) on Linux that runs tincan listen --exec with
it, and for Codex sets default_tools_approval_mode = "approve" on the
agent-tincan entry in Codex's config. Set TINCAN_CONFIG as for this agent's
other tincan commands.

It prints what a woken agent may do with nobody watching before it writes
anything, prints the wake.json step for the relay host, and never starts the
service. It never writes a secret into the service. A script you edited is
kept, here and by tincan upgrade, which updates the scripts you did not edit.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if refresh {
				return refreshWakeScripts(cmd.OutOrStdout(), home, runtime.GOOS)
			}
			w, err := examples.Wake(wake)
			if err != nil {
				return err
			}
			cfgPath, err := filepath.Abs(client.ConfigPath())
			if err != nil {
				return err
			}
			cfg, err := client.LoadConfigFrom(cfgPath)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if binary == "" {
				if binary, err = os.Executable(); err != nil {
					return err
				}
			}
			return installListener(listenInstallOptions{
				GOOS: runtime.GOOS, Home: home, Binary: binary, UID: os.Getuid(),
				ConfigPath: cfgPath, Agent: cfg.Agent, DefaultConfig: defaultConfigPath(),
				CodexConfig: codexConfigPath(home), Out: cmd.OutOrStdout(),
			}, w)
		},
	}
	cmd.Flags().StringVar(&wake, "wake", "", "the wake to install: "+strings.Join(examples.WakeNames(), ", "))
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "update installed wake scripts that were not edited (run by tincan upgrade)")
	_ = cmd.Flags().MarkHidden("refresh")
	return cmd
}
