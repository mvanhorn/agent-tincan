package cli

import (
	"bytes"
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
// __TINCAN_BINARY__, __HOME__, __PATH__, __AGENT__, __TINCAN_CONFIG__ and
// __TINCAN_BIN__, XML-escaped when filled, __ARGS__, the listen flags, and
// __EXTRA_ENV__, more environment entries.
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
    <key>TINCAN_BIN</key>
    <string>__TINCAN_BIN__</string>__EXTRA_ENV__
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

const listenSystemdTemplate = `[Unit]
Description=Agent Tincan listener for __AGENT__
After=network-online.target

[Service]
ExecStart="__TINCAN_BINARY__" listen__ARGS__
Environment="PATH=__PATH__"
Environment="TINCAN_CONFIG=__TINCAN_CONFIG__"
Environment="TINCAN_BIN=__TINCAN_BIN__"__EXTRA_ENV__
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

// listenService is what a listener's service runs: the agent, its wake
// script and config, the tincan binary, and for Codex the CODEX_HOME the
// install edited, so a woken Codex reads that same config.
type listenService struct {
	Agent, Script, Config, Binary, CodexHome string
}

func (s listenService) def() history.ServiceDef {
	launchdEnv, systemdEnv := "", ""
	vars := []string{"__AGENT__", s.Agent, "__TINCAN_CONFIG__", s.Config, "__TINCAN_BIN__", s.Binary}
	if s.CodexHome != "" {
		launchdEnv = "\n    <key>CODEX_HOME</key>\n    <string>__CODEX_HOME__</string>"
		systemdEnv = "\nEnvironment=\"CODEX_HOME=__CODEX_HOME__\""
		vars = append(vars, "__CODEX_HOME__", s.CodexHome)
	}
	return history.ServiceDef{
		Label:   listenLabel(s.Agent),
		Unit:    listenUnit(s.Agent),
		Launchd: strings.Replace(listenLaunchdTemplate, "__EXTRA_ENV__", launchdEnv, 1),
		Systemd: strings.Replace(listenSystemdTemplate, "__EXTRA_ENV__", systemdEnv, 1),
		Vars:    vars,
		// tincan listen runs the command through sh -c, so the path is
		// quoted to stay one word.
		Args:        []string{"--exec", shellQuote(s.Script)},
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

type wakeFileState int

const (
	wakeFileCurrent wakeFileState = iota // this release's bytes
	wakeFileStale                        // what tincan last wrote, from an older release
	wakeFileEdited                       // not what tincan last wrote: the owner's
	wakeFileMissing
)

// readWakeFile reads an installed wake file and says what it is against its
// stamp and this release's body. Only a read error other than a missing
// file is returned.
func readWakeFile(path, stamped string, body []byte) (wakeFileState, error) {
	cur, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return wakeFileMissing, nil
	case err != nil:
		return 0, err
	case body != nil && bytes.Equal(cur, body):
		return wakeFileCurrent, nil
	case stamped == "" || sha256Hex(cur) != stamped:
		return wakeFileEdited, nil
	}
	return wakeFileStale, nil
}

type listenInstallOptions struct {
	GOOS, Home, Binary string
	UID                int
	Launcher           string
	ConfigPath         string // the agent's config, absolute
	Agent              string
	DefaultConfig      string
	CodexHome          string // "" for ~/.codex
	Out                io.Writer
}

func (o listenInstallOptions) codexConfig() string { return codexConfigPath(o.Home, o.CodexHome) }

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
		return errors.New(listenService{Agent: o.Agent}.def().Unsupported)
	}
	isCodex := w.Name == "codex"
	if isCodex {
		if err := checkCodexEntry(o.codexConfig(), o.ConfigPath, o.DefaultConfig); err != nil {
			return err
		}
	}
	out := o.Out
	fmt.Fprintf(out, "Before you start it: %s\n\n", w.Notice)
	var toolDirs []string
	if p, err := exec.LookPath(w.Tool); err == nil {
		toolDirs = append(toolDirs, filepath.Dir(p))
	} else {
		fmt.Fprintf(out, "Note: %s is not on this PATH, so the service cannot find it either. Install it, or put its folder on your PATH, and run this again.\n", w.Tool)
	}
	toolDirs = append(toolDirs, filepath.Dir(o.Binary))

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
		state, err := readWakeFile(path, prior.Files[name], body)
		if err != nil {
			return err
		}
		switch state {
		case wakeFileCurrent:
			st.Files[name] = sha256Hex(body)
			fmt.Fprintf(out, "current %s\n", path)
		case wakeFileEdited:
			st.Files[name] = prior.Files[name]
			if st.Files[name] == "" {
				st.Files[name] = sha256Hex(body)
			}
			fmt.Fprintf(out, "left %s: it differs from what tincan wrote, so your version was kept. This release's version is in the repo at examples/%s.\n", path, examplePath(w, name))
		default:
			if err := writeFileReplacing(path, body, 0o755); err != nil {
				return err
			}
			st.Files[name] = sha256Hex(body)
			fmt.Fprintf(out, "wrote %s\n", path)
		}
	}
	if err := writeWakeStamp(dir, st); err != nil {
		return err
	}

	svc := listenService{Agent: o.Agent, Script: filepath.Join(dir, w.File), Config: o.ConfigPath, Binary: o.Binary}
	if isCodex {
		svc.CodexHome = o.CodexHome
	}
	res, err := history.InstallServiceDef(history.ServiceOptions{GOOS: o.GOOS, Home: o.Home, Binary: o.Binary, UID: o.UID, Launcher: o.Launcher, ToolDirs: toolDirs}, svc.def())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "listener service definition: %s (not started)\n", res.Path)

	if isCodex {
		changed, notice, err := setCodexApproval(o.codexConfig(), o.Binary, o.ConfigPath, o.DefaultConfig)
		switch {
		case err != nil:
			return fmt.Errorf("could not update %s (%w), so a woken Codex could not call the tincan tools. Fix that and run this again, or add default_tools_approval_mode = \"approve\" to its [mcp_servers.agent-tincan] table yourself", o.codexConfig(), err)
		case notice != "":
			return errors.New(notice)
		case changed:
			fmt.Fprintf(out, "set default_tools_approval_mode = \"approve\" on the agent-tincan entry in %s\n", o.codexConfig())
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

func listenServiceFiles(home, goos string) []string {
	var files []string
	switch goos {
	case "darwin":
		files, _ = filepath.Glob(filepath.Join(home, "Library", "LaunchAgents", listenServicePrefix+"*.plist"))
	case "linux":
		files, _ = filepath.Glob(filepath.Join(home, ".config", "systemd", "user", "tincan-listen-*.service"))
	}
	return files
}

func listenServiceFile(home, goos, agent string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", listenLabel(agent)+".plist")
	}
	return filepath.Join(home, ".config", "systemd", "user", listenUnit(agent))
}

// serviceEnv is the value a listener's plist or unit sets for key, or ""
// when it sets none.
func serviceEnv(b []byte, goos, key string) string {
	q := regexp.QuoteMeta(key)
	if goos == "darwin" {
		if m := regexp.MustCompile(`<key>` + q + `</key>\s*<string>([^<]*)</string>`).FindSubmatch(b); m != nil {
			return html.UnescapeString(string(m[1]))
		}
		return ""
	}
	if m := regexp.MustCompile(`(?m)^Environment="` + q + `=((?:[^"\\]|\\.)*)"`).FindSubmatch(b); m != nil {
		return strings.NewReplacer(`%%`, `%`, `\"`, `"`, `\\`, `\`).Replace(string(m[1]))
	}
	return ""
}

// refreshWakeScripts brings every installed listener's scripts up to this
// release, replacing only files whose bytes still match what tincan wrote.
// An edited file is left alone and named; a missing one is named, not
// recreated, since the owner may have removed it on purpose.
func refreshWakeScripts(out io.Writer, home, goos string) error {
	var errs []error
	for _, f := range listenServiceFiles(home, goos) {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		cfg := serviceEnv(b, goos, "TINCAN_CONFIG")
		if cfg == "" {
			continue
		}
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
			state, err := readWakeFile(path, stamped, body)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			switch state {
			case wakeFileMissing:
				fmt.Fprintf(out, "missing %s: not recreated; run tincan listen install --wake %s again to write it\n", path, st.Wake)
			case wakeFileEdited:
				fmt.Fprintf(out, "left %s: you edited it, so this release's version was not written (it is in the repo at examples/%s)\n", path, examplePath(w, name))
			case wakeFileCurrent:
				if stamped != sha256Hex(body) {
					st.Files[name], changed = sha256Hex(body), true
				}
			case wakeFileStale:
				if err := writeFileReplacing(path, body, 0o755); err != nil {
					errs = append(errs, err)
					continue
				}
				st.Files[name], changed = sha256Hex(body), true
				fmt.Fprintf(out, "updated %s\n", path)
			}
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
// scripts are current, and for Codex the agent-tincan entry in the Codex
// config the service uses. shellCodexHome is CODEX_HOME in doctor's own
// environment. ok is false when no listener is installed.
func listenerCheck(goos, home, cfg, shellCodexHome, def string, loaded func(label string) bool) (c check, ok bool) {
	const name = "listener"
	dir := listenDir(cfg)
	st, err := readWakeStamp(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return check{}, false
	}
	if err != nil {
		return check{name, "warn", err.Error(), "Run tincan listen install again."}, true
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

	svcFile := listenServiceFile(home, goos, st.Agent)
	svc, err := os.ReadFile(svcFile)
	if err != nil {
		warn("no service file at "+svcFile, reinstall)
	} else if goos == "darwin" && !loaded(listenLabel(st.Agent)) {
		warn("its service is not loaded", fmt.Sprintf("Start it with launchctl bootstrap gui/%d %s", os.Getuid(), svcFile))
	}

	var files map[string][]byte
	if w, err := examples.Wake(st.Wake); err == nil {
		files = wakeFiles(w)
	}
	for file, stamped := range st.Files {
		path := filepath.Join(dir, file)
		body, known := files[file]
		state, err := readWakeFile(path, stamped, body)
		switch {
		case err != nil || state == wakeFileMissing:
			warn("missing "+path, reinstall)
		case state == wakeFileEdited:
			detail = append(detail, path+" edited by you; tincan upgrade leaves it alone")
		case state == wakeFileStale && known:
			warn(path+" is from an older release", fmt.Sprintf("Run tincan upgrade, or tincan listen install --wake %s again, to update it.", st.Wake))
		}
	}

	if st.Wake == "codex" {
		codexHome := serviceEnv(svc, goos, "CODEX_HOME")
		codexCfg := codexConfigPath(home, codexHome)
		if shellCodexHome != codexHome {
			warn(fmt.Sprintf("CODEX_HOME here is %q but the listener runs Codex with %q, so a woken Codex reads %s", shellCodexHome, codexHome, codexCfg), "Run tincan listen install --wake codex again with the CODEX_HOME Codex should use.")
		}
		_, raw, err := readCodexConfig(codexCfg)
		if err != nil || len(raw) == 0 {
			warn("no Codex config at "+codexCfg, reinstall)
		} else if e, ok := findCodexEntry(strings.Split(string(raw), "\n")); !ok {
			warn("no agent-tincan entry in "+codexCfg, reinstall)
		} else {
			if msg := codexEntryMismatch(e, codexCfg, cfg, def); msg != "" {
				warn(msg, "")
			}
			if !e.approve {
				warn("the agent-tincan entry in "+codexCfg+" does not set default_tools_approval_mode = \"approve\", so unattended wakes cannot call the tincan tools", reinstall)
			}
		}
	}
	return check{name, status, strings.Join(detail, "; "), strings.Join(fix, " ")}, true
}

func listenInstallCmd() *cobra.Command {
	var wake, binary string
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
			w, err := examples.Wake(wake)
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
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
				ConfigPath: cfgPath, Agent: cfg.Agent, DefaultConfig: client.DefaultConfigPath(),
				CodexHome: os.Getenv("CODEX_HOME"), Out: cmd.OutOrStdout(),
			}, w)
		},
	}
	cmd.Flags().StringVar(&wake, "wake", "", "the wake to install: "+strings.Join(examples.WakeNames(), ", "))
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	return cmd
}

// listenRefreshCmd is run by tincan upgrade with the new binary, which
// carries the new release's scripts.
func listenRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "refresh",
		Short:  "Update installed wake scripts that were not edited",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			return refreshWakeScripts(cmd.OutOrStdout(), home, runtime.GOOS)
		},
	}
}
