package cli

import (
	"bytes"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/examples"
	"github.com/mvanhorn/agent-tincan/internal/history"
)

func codexWake(t *testing.T) examples.WakeScript {
	t.Helper()
	w, err := examples.Wake("codex")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func installForTest(t *testing.T, goos, cfgPath string, w examples.WakeScript) (listenInstallOptions, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("listen install writes POSIX services")
	}
	home := t.TempDir()
	o := listenInstallOptions{
		GOOS: goos, Home: home, Binary: "/opt/tin can/tincan", UID: 501, Launcher: "/A/agent-tincan",
		ConfigPath: cfgPath, Agent: "codex", DefaultConfig: filepath.Join(home, "default", "client.json"),
	}
	var out bytes.Buffer
	o.Out = &out
	if err := installListener(o, w); err != nil {
		t.Fatal(err)
	}
	return o, out.String()
}

// The service runs tincan listen with the wake script, as the agent whose
// config it names, under a label of its own.
func TestListenInstallDarwin(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "codex.json")
	o, out := installForTest(t, "darwin", cfg, codexWake(t))
	plist := filepath.Join(o.Home, "Library", "LaunchAgents", "com.agenttincan.listen.codex.plist")
	b, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if strings.Contains(body, "__") {
		t.Fatalf("placeholder left:\n%s", body)
	}
	script := filepath.Join(filepath.Dir(cfg), "codex.listen", "codex-wake.sh")
	for _, want := range []string{
		"<string>com.agenttincan.listen.codex</string>",
		"<string>/opt/tin can/tincan</string>",
		"<string>listen</string>",
		"<string>--exec</string>",
		"<string>" + html.EscapeString(shellQuote(script)) + "</string>",
		"<key>TINCAN_CONFIG</key>",
		"<string>" + cfg + "</string>",
		"tincan-listen-codex.log",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist lacks %q:\n%s", want, body)
		}
	}
	for _, secret := range []string{"API_KEY", "TOKEN", "ALLOW_UNCONFINED"} {
		if strings.Contains(body, secret) {
			t.Errorf("plist mentions %s", secret)
		}
	}
	fi, err := os.Stat(script)
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("script not written executable: %v %v", fi, err)
	}
	for _, want := range []string{"approvals off", "network on", "wake.json", `"codex": {"method": "command"}`, "launchctl bootstrap gui/501 " + plist} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "approvals off") > strings.Index(out, "wrote") && strings.Contains(out, "wrote") {
		t.Errorf("the notice must come before anything is written:\n%s", out)
	}
}

func TestListenInstallLinux(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "codex.json")
	o, out := installForTest(t, "linux", cfg, codexWake(t))
	unit := filepath.Join(o.Home, ".config", "systemd", "user", "tincan-listen-codex.service")
	b, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, want := range []string{`ExecStart="/opt/tin can/tincan" listen "--exec"`, `Environment="TINCAN_CONFIG=` + cfg + `"`, "Restart=always"} {
		if !strings.Contains(body, want) {
			t.Errorf("unit lacks %q:\n%s", want, body)
		}
	}
	if !strings.Contains(out, "systemctl --user") {
		t.Errorf("no start command:\n%s", out)
	}
}

// A script whose path has a space and an apostrophe still reaches sh -c,
// which tincan listen runs it through, as one path.
func TestListenInstallQuotesTheScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := filepath.Join(t.TempDir(), "Ben's Configs")
	cfg := filepath.Join(dir, "codex.json")
	o, _ := installForTest(t, "linux", cfg, codexWake(t))
	script := filepath.Join(dir, "codex.listen", "codex-wake.sh")
	b, err := os.ReadFile(filepath.Join(o.Home, ".config", "systemd", "user", "tincan-listen-codex.service"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ExecStart=.* "--exec" "((?:[^"\\]|\\.)*)"$`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no --exec in unit:\n%s", b)
	}
	exec_ := strings.NewReplacer(`%%`, `%`, `\"`, `"`, `\\`, `\`).Replace(string(m[1]))
	out, err := exec.Command("sh", "-c", "printf %s "+exec_).Output()
	if err != nil || string(out) != script {
		t.Fatalf("sh -c saw %q (%v), want %q", out, err, script)
	}
}

func TestListenInstallLibAndStamp(t *testing.T) {
	w, err := examples.Wake("copilot-cli")
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "copilot-cli.json")
	installForTest(t, "linux", cfg, w)
	dir := filepath.Join(filepath.Dir(cfg), "copilot-cli.listen")
	for _, f := range []string{"copilot-wake.sh", examples.WakeLibName} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	st, err := readWakeStamp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Wake != "copilot-cli" || st.Files["copilot-wake.sh"] != sha256Hex(w.Script) || st.Files[examples.WakeLibName] != sha256Hex(examples.WakeLib()) {
		t.Fatalf("stamp = %+v", st)
	}
}

// Installing again never throws away the owner's edit.
func TestListenInstallKeepsEditedScript(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "codex.json")
	o, _ := installForTest(t, "linux", cfg, codexWake(t))
	script := filepath.Join(filepath.Dir(cfg), "codex.listen", "codex-wake.sh")
	edited := []byte("#!/bin/sh\n# my tighter wake\n")
	if err := os.WriteFile(script, edited, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	o.Out = &out
	if err := installListener(o, codexWake(t)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(script); !bytes.Equal(b, edited) {
		t.Fatal("edited script was overwritten")
	}
	if !strings.Contains(out.String(), "left "+script) {
		t.Fatalf("output does not name the edited script:\n%s", out.String())
	}
}

func TestListenInstallRefusesUnjoined(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "c.json")
	o := listenInstallOptions{GOOS: "linux", Home: t.TempDir(), Binary: "/opt/tincan", ConfigPath: cfg, Out: &bytes.Buffer{}}
	if err := installListener(o, codexWake(t)); err == nil || !strings.Contains(err.Error(), "join") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(listenDir(cfg)); err == nil {
		t.Fatal("wrote files before refusing")
	}
	o.Agent = "bad name!"
	if err := installListener(o, codexWake(t)); err == nil || !strings.Contains(err.Error(), "cannot name a service") {
		t.Fatalf("invalid name: err = %v", err)
	}
	o.Agent = "codex"
	o.GOOS = "windows"
	if err := installListener(o, codexWake(t)); err == nil {
		t.Fatal("want a refusal on windows")
	}
}

func TestSetCodexApproval(t *testing.T) {
	cfg := "/home/o/.config/tincan/codex.json"
	def := "/home/o/.config/tincan/client.json"
	other := `[profiles.fast]
model = "o4"   # keep me

`
	for _, tc := range []struct {
		name, in   string
		cfg        string
		changed    bool
		want       []string
		wantExact  bool
		wantNotice string
	}{
		{"adds the key after the header", other + "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nargs = [\"mcp\"]\nenv = { TINCAN_CONFIG = \"" + cfg + "\" }\n", cfg, true,
			[]string{"[mcp_servers.agent-tincan]\ndefault_tools_approval_mode = \"approve\"\ncommand", `model = "o4"   # keep me`}, false, ""},
		{"sets another value", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\ndefault_tools_approval_mode = \"prompt\"\n[mcp_servers.agent-tincan.env]\nTINCAN_CONFIG = \"" + cfg + "\"\n", cfg, true,
			[]string{"default_tools_approval_mode = \"approve\"\n[mcp_servers.agent-tincan.env]"}, false, ""},
		{"already set", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\ndefault_tools_approval_mode = \"approve\"\nenv = { TINCAN_CONFIG = \"" + cfg + "\" }\n", cfg, false, nil, true, ""},
		{"no entry adds one", other, cfg, true,
			[]string{other, "[mcp_servers.agent-tincan]\ncommand = \"/opt/tincan\"\nargs = [\"mcp\"]\nenv = { TINCAN_CONFIG = \"" + cfg + "\" }\ndefault_tools_approval_mode = \"approve\"\n"}, false, ""},
		{"entry under another name", "[mcp_servers.tincan]\ncommand = \"tincan\"\nargs = [\"mcp\"]\n", cfg, false, nil, true, "named tincan"},
		{"entry for another agent", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = \"/home/o/.config/tincan/other.json\" }\n", cfg, false, nil, true, "other.json"},
		{"entry with no env is the default config", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\n", cfg, false, nil, true, "default config"},
		{"no env and installing the default config", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\n", def, true, []string{"default_tools_approval_mode = \"approve\""}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.in), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, notice, err := setCodexApproval(path, "/opt/tincan", tc.cfg, def)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(path)
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v\n%s", changed, tc.changed, b)
			}
			if tc.wantExact && string(b) != tc.in {
				t.Fatalf("file changed:\n%s", b)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(b), w) {
					t.Errorf("file lacks %q:\n%s", w, b)
				}
			}
			if tc.wantNotice != "" && !strings.Contains(notice, tc.wantNotice) {
				t.Errorf("notice %q lacks %q", notice, tc.wantNotice)
			}
			// Windows reports every writable file as 0666.
			if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
				t.Errorf("mode = %v", fi.Mode().Perm())
			}
			if tc.changed {
				if es := tomlServers(string(b)); len(es) == 0 {
					t.Error("edited file no longer reads as an MCP config")
				}
			}
		})
	}
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	if changed, _, err := setCodexApproval(path, "/opt/tincan", cfg, def); err != nil || !changed {
		t.Fatalf("missing file: changed=%v err=%v", changed, err)
	}
}

// Upgrade replaces only the scripts the owner did not change, and says which
// it left.
func TestRefreshWakeScripts(t *testing.T) {
	w := codexWake(t)
	cfgA := filepath.Join(t.TempDir(), "codex.json")
	o, _ := installForTest(t, "linux", cfgA, w)
	cfgB := filepath.Join(t.TempDir(), "codex2.json")
	o2 := o
	o2.ConfigPath, o2.Agent, o2.Out = cfgB, "codex2", &bytes.Buffer{}
	o2.CodexHome = t.TempDir() // a second Codex agent needs its own Codex config
	if err := installListener(o2, w); err != nil {
		t.Fatal(err)
	}
	scriptA := filepath.Join(filepath.Dir(cfgA), "codex.listen", "codex-wake.sh")
	scriptB := filepath.Join(filepath.Dir(cfgB), "codex2.listen", "codex-wake.sh")

	// Both were written by an older release: their stamps match their
	// bytes, which differ from this release's.
	old := []byte("#!/bin/sh\n# older release\n")
	for _, s := range []string{scriptA, scriptB} {
		if err := os.WriteFile(s, old, 0o755); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(s)
		st, _ := readWakeStamp(dir)
		st.Files["codex-wake.sh"] = sha256Hex(old)
		if err := writeWakeStamp(dir, st); err != nil {
			t.Fatal(err)
		}
	}
	// Then the owner edits B.
	edited := []byte("#!/bin/sh\n# owner edit\n")
	if err := os.WriteFile(scriptB, edited, 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := refreshWakeScripts(&out, o.Home, "linux"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(scriptA); !bytes.Equal(b, w.Script) {
		t.Error("unedited script was not refreshed")
	}
	if b, _ := os.ReadFile(scriptB); !bytes.Equal(b, edited) {
		t.Error("edited script was overwritten")
	}
	if !strings.Contains(out.String(), "left "+scriptB) || !strings.Contains(out.String(), "updated "+scriptA) {
		t.Fatalf("output:\n%s", out.String())
	}

	// A second run finds nothing to do for A, and a missing script is
	// reported, not recreated.
	if err := os.Remove(scriptA); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := refreshWakeScripts(&out, o.Home, "linux"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scriptA); err == nil {
		t.Error("missing script was recreated")
	}
	if !strings.Contains(out.String(), "missing "+scriptA) {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestListenerCheck(t *testing.T) {
	w := codexWake(t)
	cfg := filepath.Join(t.TempDir(), "codex.json")
	o, _ := installForTest(t, "darwin", cfg, w)
	loaded := func(string) bool { return true }
	run := func() check {
		c, _ := listenerCheck(o.GOOS, o.Home, cfg, "", o.DefaultConfig, loaded)
		return c
	}

	if c := run(); c.Status != "ok" || !strings.Contains(c.Detail, "codex") {
		t.Fatalf("installed: %+v", c)
	}
	script := filepath.Join(filepath.Dir(cfg), "codex.listen", "codex-wake.sh")

	if err := os.WriteFile(script, []byte("#!/bin/sh\n# mine\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := run(); c.Status != "ok" || !strings.Contains(c.Detail, "edited") {
		t.Fatalf("edited: %+v", c)
	}

	old := []byte("#!/bin/sh\n# older release\n")
	if err := os.WriteFile(script, old, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := readWakeStamp(filepath.Dir(script))
	st.Files["codex-wake.sh"] = sha256Hex(old)
	if err := writeWakeStamp(filepath.Dir(script), st); err != nil {
		t.Fatal(err)
	}
	if c := run(); c.Status != "warn" || !strings.Contains(c.Fix, "tincan upgrade") {
		t.Fatalf("outdated: %+v", c)
	}

	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	if c := run(); c.Status != "warn" || !strings.Contains(c.Fix, "listen install") {
		t.Fatalf("missing script: %+v", c)
	}
	o.Out = &bytes.Buffer{}
	if err := installListener(o, w); err != nil {
		t.Fatal(err)
	}

	loaded = func(string) bool { return false }
	if c := run(); c.Status != "warn" || !strings.Contains(c.Fix, "launchctl bootstrap") {
		t.Fatalf("not loaded: %+v", c)
	}
	loaded = func(string) bool { return true }

	if err := os.WriteFile(o.codexConfig(), []byte("[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = \""+cfg+"\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := run(); c.Status != "warn" || !strings.Contains(c.Detail, "approval") {
		t.Fatalf("no approval mode: %+v", c)
	}
	if err := os.WriteFile(o.codexConfig(), []byte("[mcp_servers.agent-tincan]\ncommand = \"tincan\"\ndefault_tools_approval_mode = \"approve\"\nenv = { TINCAN_CONFIG = \"/elsewhere/other.json\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := run(); c.Status != "warn" || !strings.Contains(c.Detail, "other.json") {
		t.Fatalf("other agent's config: %+v", c)
	}

	if _, ok := listenerCheck(o.GOOS, o.Home, filepath.Join(t.TempDir(), "none.json"), "", o.DefaultConfig, loaded); ok {
		t.Fatalf("no listener installed should add no line")
	}
}

func listenOptions(t *testing.T, goos, cfg string) listenInstallOptions {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("listen install writes POSIX services")
	}
	home := t.TempDir()
	return listenInstallOptions{
		GOOS: goos, Home: home, Binary: "/opt/tin can/tincan", UID: 501, Launcher: "/A/agent-tincan",
		ConfigPath: cfg, Agent: "codex", DefaultConfig: filepath.Join(home, "default", "client.json"),
		Out: &bytes.Buffer{},
	}
}

// A Codex entry that runs tincan as another agent stops the install
// before anything is written, so no listener can wake Codex as that agent.
func TestListenInstallRefusesAnotherAgentsCodexEntry(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "codex.json")
	o := listenOptions(t, "linux", cfg)
	if err := os.MkdirAll(filepath.Dir(o.codexConfig()), 0o700); err != nil {
		t.Fatal(err)
	}
	pinned := "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\ndefault_tools_approval_mode = \"approve\"\nenv = { TINCAN_CONFIG = \"/elsewhere/claude.json\" }\n"
	if err := os.WriteFile(o.codexConfig(), []byte(pinned), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installListener(o, codexWake(t))
	if err == nil || !strings.Contains(err.Error(), "another agent") || !strings.Contains(err.Error(), "Nothing was installed") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(listenDir(cfg)); err == nil {
		t.Error("wake scripts written for a refused install")
	}
	if _, err := os.Stat(listenServiceFile(o.Home, "linux", "codex")); err == nil {
		t.Error("service written for a refused install")
	}
	if b, _ := os.ReadFile(o.codexConfig()); string(b) != pinned {
		t.Errorf("Codex config changed:\n%s", b)
	}
}

// The pin is read the way TOML means it: comments do not count, and
// literal strings and quoted keys do.
func TestCodexEntryReadsThePin(t *testing.T) {
	cfg := "/home/o/.config/tincan/codex.json"
	def := "/home/o/.config/tincan/client.json"
	for _, tc := range []struct {
		name, in, cfg string
		changed       bool
		notice        string
	}{
		{"commented pin does not count", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = \"/o/other.json\" }\n# env = { TINCAN_CONFIG = \"" + cfg + "\" }\n", cfg, false, "other.json"},
		{"trailing comment does not count", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = \"/o/other.json\" } # was " + cfg + "\n", cfg, false, "other.json"},
		{"literal string pin", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = '/o/other.json' }\n", def, false, "other.json"},
		{"quoted key pin", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { \"TINCAN_CONFIG\" = \"/o/other.json\" }\n", def, false, "other.json"},
		{"two different pins", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = \"" + cfg + "\" }\n[mcp_servers.agent-tincan.env]\nTINCAN_CONFIG = \"/o/other.json\"\n", cfg, false, "cannot read"},
		{"literal string naming this agent", "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = '" + cfg + "' }\n", cfg, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.in), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, notice, err := setCodexApproval(path, "/opt/tincan", tc.cfg, def)
			if err != nil {
				t.Fatal(err)
			}
			if changed != tc.changed || !strings.Contains(notice, tc.notice) {
				t.Fatalf("changed = %v, notice = %q", changed, notice)
			}
			if b, _ := os.ReadFile(path); !tc.changed && string(b) != tc.in {
				t.Fatalf("file changed:\n%s", b)
			}
		})
	}
}

// A header with a comment is still the entry, so no second table is added.
func TestSetCodexApprovalCommentedHeader(t *testing.T) {
	cfg := "/home/o/.config/tincan/codex.json"
	path := filepath.Join(t.TempDir(), "config.toml")
	in := "[mcp_servers.agent-tincan] # tincan\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = \"" + cfg + "\" }\n"
	if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, notice, err := setCodexApproval(path, "/opt/tincan", cfg, "/def.json"); err != nil || !changed || notice != "" {
		t.Fatalf("changed=%v notice=%q err=%v", changed, notice, err)
	}
	b, _ := os.ReadFile(path)
	if n := strings.Count(string(b), "[mcp_servers.agent-tincan]"); n != 1 || !strings.Contains(string(b), "# tincan\ndefault_tools_approval_mode = \"approve\"") {
		t.Fatalf("%d tables:\n%s", n, b)
	}
}

// A dotfile-managed config.toml stays a link, and the file it points at
// gets the approval mode.
func TestSetCodexApprovalFollowsSymlink(t *testing.T) {
	cfg := "/home/o/.config/tincan/codex.json"
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "codex.toml")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = \""+cfg+"\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if changed, _, err := setCodexApproval(link, "/opt/tincan", cfg, "/def.json"); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("config.toml is no longer a symlink")
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), `default_tools_approval_mode = "approve"`) {
		t.Fatalf("linked file not updated:\n%s", b)
	}
}

// With CODEX_HOME set, install edits that Codex config and the service
// runs Codex with the same CODEX_HOME; doctor notices when they differ.
func TestListenInstallCodexHome(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "codex.json")
			o := listenOptions(t, goos, cfg)
			o.CodexHome = filepath.Join(t.TempDir(), "codex home")
			if err := installListener(o, codexWake(t)); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(o.CodexHome, "config.toml")); err != nil || !strings.Contains(string(b), "approve") {
				t.Fatalf("Codex config under CODEX_HOME: %s %v", b, err)
			}
			svc, _ := os.ReadFile(listenServiceFile(o.Home, goos, "codex"))
			if got := serviceEnv(svc, goos, "CODEX_HOME"); got != o.CodexHome {
				t.Fatalf("service CODEX_HOME = %q, want %q:\n%s", got, o.CodexHome, svc)
			}
			loaded := func(string) bool { return true }
			if c, _ := listenerCheck(goos, o.Home, cfg, o.CodexHome, o.DefaultConfig, loaded); c.Status != "ok" {
				t.Fatalf("matching CODEX_HOME: %+v", c)
			}
			if c, _ := listenerCheck(goos, o.Home, cfg, "", o.DefaultConfig, loaded); c.Status != "warn" || !strings.Contains(c.Detail, "CODEX_HOME") {
				t.Fatalf("different CODEX_HOME: %+v", c)
			}
		})
	}
}

// The service can find the wake's tool and this tincan wherever they live.
func TestListenInstallServicePath(t *testing.T) {
	tools := filepath.Join(t.TempDir(), "fnm", "bin")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "copilot"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	w, err := examples.Wake("copilot-cli")
	if err != nil {
		t.Fatal(err)
	}
	o := listenOptions(t, "linux", filepath.Join(t.TempDir(), "copilot-cli.json"))
	o.Agent = "copilot-cli"
	if err := installListener(o, w); err != nil {
		t.Fatal(err)
	}
	svc, _ := os.ReadFile(listenServiceFile(o.Home, "linux", "copilot-cli"))
	path := serviceEnv(svc, "linux", "PATH")
	for _, dir := range []string{tools, "/opt/tin can"} {
		if !slices.Contains(filepath.SplitList(path), dir) {
			t.Errorf("service PATH %q lacks %s", path, dir)
		}
	}
	if got := serviceEnv(svc, "linux", "TINCAN_BIN"); got != o.Binary {
		t.Errorf("TINCAN_BIN = %q, want %q", got, o.Binary)
	}
}

// Install replaces a script from an older release that nobody edited, and
// keeps a script it has no record of writing.
func TestListenInstallStaleAndUnstamped(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "codex.json")
	o, _ := installForTest(t, "linux", cfg, codexWake(t))
	dir := listenDir(cfg)
	script := filepath.Join(dir, "codex-wake.sh")
	old := []byte("#!/bin/sh\n# older release\n")
	if err := os.WriteFile(script, old, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := readWakeStamp(dir)
	st.Files["codex-wake.sh"] = sha256Hex(old)
	if err := writeWakeStamp(dir, st); err != nil {
		t.Fatal(err)
	}
	o.Out = &bytes.Buffer{}
	if err := installListener(o, codexWake(t)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(script); !bytes.Equal(b, codexWake(t).Script) {
		t.Fatal("stale unedited script was not replaced")
	}
	if st, _ := readWakeStamp(dir); st.Files["codex-wake.sh"] != sha256Hex(codexWake(t).Script) {
		t.Fatal("stamp not updated")
	}

	if err := os.WriteFile(script, old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, listenStampFile)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	o.Out = &out
	if err := installListener(o, codexWake(t)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(script); !bytes.Equal(b, old) || !strings.Contains(out.String(), "left "+script) {
		t.Fatalf("unstamped script not kept and named:\n%s", out.String())
	}
}

// When Codex's config cannot be written, install fails rather than leave a
// listener whose wakes cannot call the tincan tools.
func TestListenInstallFailsWhenCodexConfigUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only folder")
	}
	o := listenOptions(t, "linux", filepath.Join(t.TempDir(), "codex.json"))
	o.CodexHome = t.TempDir()
	if err := os.Chmod(o.CodexHome, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(o.CodexHome, 0o755) })
	var out bytes.Buffer
	o.Out = &out
	if err := installListener(o, codexWake(t)); err == nil || !strings.Contains(err.Error(), "could not update") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(out.String(), "start it with") {
		t.Fatalf("printed a start command:\n%s", out.String())
	}
	if _, err := os.Stat(listenServiceFile(o.Home, "linux", "codex")); err == nil {
		t.Error("a failed install left a listener service behind")
	}
}

// tincan upgrade runs the hidden tincan listen refresh.
func TestListenRefreshCommand(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no listener services here")
	}
	cfg := filepath.Join(t.TempDir(), "codex.json")
	o, _ := installForTest(t, runtime.GOOS, cfg, codexWake(t))
	t.Setenv("HOME", o.Home)
	script := filepath.Join(listenDir(cfg), "codex-wake.sh")
	old := []byte("#!/bin/sh\n# older release\n")
	if err := os.WriteFile(script, old, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := readWakeStamp(listenDir(cfg))
	st.Files["codex-wake.sh"] = sha256Hex(old)
	if err := writeWakeStamp(listenDir(cfg), st); err != nil {
		t.Fatal(err)
	}
	cmd := listenCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"refresh"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "updated "+script) {
		t.Fatalf("output:\n%s", out.String())
	}
}

// A quoted approval key, basic or literal, is the same key: it is
// rewritten in place, never joined by a second bare one.
func TestSetCodexApprovalQuotedKey(t *testing.T) {
	cfg := "/home/o/.config/tincan/codex.json"
	for _, key := range []string{`"default_tools_approval_mode"`, `'default_tools_approval_mode'`} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			in := "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\n" + key + " = \"prompt\"\nenv = { TINCAN_CONFIG = \"" + cfg + "\" }\n"
			if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, notice, err := setCodexApproval(path, "/opt/tincan", cfg, "/home/o/.config/tincan/client.json")
			if err != nil || notice != "" || !changed {
				t.Fatalf("changed=%v notice=%q err=%v", changed, notice, err)
			}
			b, _ := os.ReadFile(path)
			if n := strings.Count(string(b), "default_tools_approval_mode"); n != 1 {
				t.Fatalf("%d approval keys:\n%s", n, b)
			}
			if !strings.Contains(string(b), `default_tools_approval_mode = "approve"`) || strings.Contains(string(b), "prompt") {
				t.Fatalf("not rewritten to approve:\n%s", b)
			}
			if e, _ := findCodexEntry(strings.Split(string(b), "\n")); !e.approve {
				t.Errorf("edited entry does not read as approve:\n%s", b)
			}
		})
	}
}

// Reinstalling keeps the environment the owner added to the service, on
// both service managers, and names it without printing its value.
func TestListenInstallKeepsOwnerEnv(t *testing.T) {
	const secret = "ghp_s3cret&<value>"
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "codex.json")
			o, _ := installForTest(t, goos, cfg, codexWake(t))
			f := listenServiceFile(o.Home, goos, "codex")
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var edited string
			if goos == "darwin" {
				edited = strings.Replace(string(b), "    <key>TINCAN_CONFIG</key>", "    <key>COPILOT_GITHUB_TOKEN</key>\n    <string>"+html.EscapeString(secret)+"</string>\n    <key>TINCAN_CONFIG</key>", 1)
			} else {
				edited = strings.Replace(string(b), "Restart=always", "Environment=\"COPILOT_GITHUB_TOKEN="+secret+"\"\nEnvironment=TINCAN_WAKE_TIMEOUT=900\nRestart=always", 1)
			}
			if edited == string(b) {
				t.Fatal("test could not add the variable")
			}
			if err := os.WriteFile(f, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f, 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			o.Out = &out
			if err := installListener(o, codexWake(t)); err != nil {
				t.Fatal(err)
			}
			b, _ = os.ReadFile(f)
			if got := serviceEnv(b, goos, "COPILOT_GITHUB_TOKEN"); got != secret {
				t.Fatalf("COPILOT_GITHUB_TOKEN = %q after reinstall:\n%s", got, b)
			}
			if serviceEnv(b, goos, "TINCAN_CONFIG") != cfg {
				t.Errorf("TINCAN_CONFIG lost:\n%s", b)
			}
			if n := strings.Count(string(b), "TINCAN_CONFIG"); n != 1 {
				t.Errorf("TINCAN_CONFIG written %d times:\n%s", n, b)
			}
			if !strings.Contains(out.String(), "COPILOT_GITHUB_TOKEN") {
				t.Errorf("output does not name the kept variable:\n%s", out.String())
			}
			if strings.Contains(out.String(), "s3cret") {
				t.Errorf("output prints the value:\n%s", out.String())
			}
			if goos == "linux" {
				if !strings.Contains(string(b), `Environment="TINCAN_WAKE_TIMEOUT=900"`) || !strings.Contains(out.String(), "TINCAN_WAKE_TIMEOUT") {
					t.Errorf("unquoted assignment not kept:\n%s\n%s", b, out.String())
				}
			}
			if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
				t.Errorf("mode = %v, want the owner's 0600", fi.Mode().Perm())
			}
		})
	}
}

// Reinstalling over a 0600 definition that holds the owner's token asks
// the service writer for 0600, so the file is never written with wider
// bits and chmodded after, and the published file is 0600.
func TestListenInstallWritesKeptTokenOwnerOnly(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "codex.json")
			o, _ := installForTest(t, goos, cfg, codexWake(t))
			f := listenServiceFile(o.Home, goos, "codex")
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var edited string
			if goos == "darwin" {
				edited = strings.Replace(string(b), "    <key>TINCAN_CONFIG</key>", "    <key>COPILOT_GITHUB_TOKEN</key>\n    <string>ghp_x</string>\n    <key>TINCAN_CONFIG</key>", 1)
			} else {
				edited = strings.Replace(string(b), "Restart=always", "Environment=\"COPILOT_GITHUB_TOKEN=ghp_x\"\nRestart=always", 1)
			}
			if edited == string(b) {
				t.Fatal("test could not add the variable")
			}
			if err := os.WriteFile(f, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f, 0o600); err != nil {
				t.Fatal(err)
			}
			var modes []os.FileMode
			orig := installServiceDef
			t.Cleanup(func() { installServiceDef = orig })
			installServiceDef = func(so history.ServiceOptions, d history.ServiceDef) (history.ServiceResult, error) {
				modes = append(modes, d.Mode)
				return orig(so, d)
			}
			o.Out = &bytes.Buffer{}
			if err := installListener(o, codexWake(t)); err != nil {
				t.Fatal(err)
			}
			if len(modes) != 1 || modes[0] != 0o600 {
				t.Fatalf("service writer asked for modes %v, want [0600]", modes)
			}
			fi, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Fatalf("published mode = %v, want 0600", fi.Mode().Perm())
			}
			if b, _ := os.ReadFile(f); serviceEnv(b, goos, "COPILOT_GITHUB_TOKEN") != "ghp_x" {
				t.Fatalf("token not kept:\n%s", b)
			}
		})
	}
}

// A header with spaces around its dot, padding inside its brackets or a
// quoted name is still the agent-tincan entry, as are its sub-tables, so
// the approval mode is set in place and no second table is added.
func TestSetCodexApprovalSpacedHeader(t *testing.T) {
	cfg := "/home/o/.config/tincan/codex.json"
	for _, tc := range []struct{ name, header, sub string }{
		{"spaced dot", "[mcp_servers . agent-tincan]", "[mcp_servers . agent-tincan . env]"},
		{"padded brackets", "[ mcp_servers.agent-tincan ]", "[ mcp_servers.agent-tincan.env ]"},
		{"quoted name", `[mcp_servers."agent-tincan"]`, `[mcp_servers."agent-tincan".env]`},
		{"literal quoted, tabs", "[\tmcp_servers\t.\t'agent-tincan'\t]", "[mcp_servers.'agent-tincan' . env]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			in := tc.header + "\ncommand = \"tincan\"\ndefault_tools_approval_mode = \"prompt\"\n" + tc.sub + "\nTINCAN_CONFIG = \"" + cfg + "\"\n\n[profiles.fast]\nmodel = \"o4\"\n"
			if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, notice, err := setCodexApproval(path, "/opt/tincan", cfg, "/def.json")
			if err != nil || notice != "" || !changed {
				t.Fatalf("changed=%v notice=%q err=%v", changed, notice, err)
			}
			b, _ := os.ReadFile(path)
			want := strings.Replace(in, `default_tools_approval_mode = "prompt"`, `default_tools_approval_mode = "approve"`, 1)
			if string(b) != want {
				t.Fatalf("not edited in place:\n%s\nwant:\n%s", b, want)
			}
			if es := tomlServers(string(b)); len(es) != 1 || es[0].Name != "agent-tincan" {
				t.Errorf("tomlServers = %+v", es)
			}
		})
	}
}

// When the entry may be defined in a form the reader cannot take, setting
// the approval mode fails and leaves the file alone instead of adding a
// second agent-tincan table.
func TestSetCodexApprovalRefusesUnreadEntry(t *testing.T) {
	cfg := "/home/o/.config/tincan/codex.json"
	for _, in := range []string{
		"[[mcp_servers.agent-tincan]]\ncommand = \"tincan\"\n",
		"[mcp_servers.\"agent-tincan\\q\"]\ncommand = \"tincan\"\n",
		"[mcp_servers]\nagent-tincan = { command = \"tincan\" }\n",
		"mcp_servers.agent-tincan.command = \"tincan\"\n",
	} {
		t.Run(strings.SplitN(in, "\n", 2)[0], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, notice, err := setCodexApproval(path, "/opt/tincan", cfg, "/def.json")
			if err == nil || changed || notice != "" {
				t.Fatalf("changed=%v notice=%q err=%v, want an error", changed, notice, err)
			}
			if b, _ := os.ReadFile(path); string(b) != in {
				t.Fatalf("file changed:\n%s", b)
			}
		})
	}
}

func TestTomlTableHeader(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"[mcp_servers.agent-tincan]", []string{"mcp_servers", "agent-tincan"}},
		{` [ mcp_servers . "agent.tincan" ] # c`, []string{"mcp_servers", "agent.tincan"}},
		{"[a.'b c'.d]", []string{"a", "b c", "d"}},
		{"[[a.b]]", nil},
		{"[a..b]", nil},
		{"[a b]", nil},
		{"[]", nil},
		{"[a.b", nil},
		{"x = [1]", nil},
	} {
		got, ok := tomlTableHeader(tc.in)
		if ok != (tc.want != nil) || !slices.Equal(got, tc.want) {
			t.Errorf("tomlTableHeader(%q) = %q, %v; want %q", tc.in, got, ok, tc.want)
		}
	}
}

// laterSubTable is a Codex config whose agent-tincan env sub-table comes
// after another server's table, which TOML allows. That server's own
// TINCAN_CONFIG and approval mode belong to it, not to agent-tincan.
func laterSubTable(mainKeys, pin string) string {
	return "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nargs = [\"mcp\"]\n" + mainKeys +
		"\n[mcp_servers.other]\ncommand = \"other\"\ndefault_tools_approval_mode = \"prompt\"\nenv = { TINCAN_CONFIG = \"/elsewhere/other.json\" }\n" +
		"\n[mcp_servers.agent-tincan.env]\nTINCAN_CONFIG = \"" + pin + "\"\n"
}

// A pin in a sub-table after another server's table still names this
// agent, so install goes ahead.
func TestListenInstallReadsLaterCodexSubTable(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "codex.json")
	o := listenOptions(t, "linux", cfg)
	if err := os.MkdirAll(filepath.Dir(o.codexConfig()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.codexConfig(), []byte(laterSubTable("", cfg)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installListener(o, codexWake(t)); err != nil {
		t.Fatalf("install refused a matching later pin: %v", err)
	}
	b, _ := os.ReadFile(o.codexConfig())
	if e, _ := findCodexEntry(strings.Split(string(b), "\n")); !e.approve || e.config != cfg {
		t.Fatalf("entry = %+v\n%s", e, b)
	}
}

// A later sub-table that pins another agent refuses an install for the
// default config, the same as an adjacent pin would.
func TestListenInstallRefusesLaterCodexSubTablePin(t *testing.T) {
	o := listenOptions(t, "linux", "")
	o.ConfigPath = o.DefaultConfig
	if err := os.MkdirAll(filepath.Dir(o.codexConfig()), 0o700); err != nil {
		t.Fatal(err)
	}
	in := laterSubTable("", "/elsewhere/claude.json")
	if err := os.WriteFile(o.codexConfig(), []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installListener(o, codexWake(t))
	if err == nil || !strings.Contains(err.Error(), "/elsewhere/claude.json") || !strings.Contains(err.Error(), "Nothing was installed") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(listenServiceFile(o.Home, "linux", "codex")); err == nil {
		t.Error("service written for a refused install")
	}
	if b, _ := os.ReadFile(o.codexConfig()); string(b) != in {
		t.Errorf("Codex config changed:\n%s", b)
	}
}

// The approval mode is added or replaced in the entry's own table only;
// the other server's table and the later sub-table stay as they were.
func TestSetCodexApprovalLaterSubTable(t *testing.T) {
	cfg := "/home/o/.config/tincan/codex.json"
	key := "default_tools_approval_mode = \"approve\"\n"
	for _, tc := range []struct{ name, in, want string }{
		{"adds", laterSubTable("", cfg), strings.Replace(laterSubTable("", cfg), "[mcp_servers.agent-tincan]\n", "[mcp_servers.agent-tincan]\n"+key, 1)},
		{"replaces", laterSubTable("default_tools_approval_mode = \"prompt\"\n", cfg), laterSubTable(key, cfg)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.in), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, notice, err := setCodexApproval(path, "/opt/tincan", cfg, "/def.json")
			if err != nil || notice != "" || !changed {
				t.Fatalf("changed=%v notice=%q err=%v", changed, notice, err)
			}
			b, _ := os.ReadFile(path)
			if string(b) != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", b, tc.want)
			}
		})
	}
}

// The claude-code wake drives cmux, which is macOS only, so a Linux install
// refuses it before writing the script or the service.
func TestListenInstallRefusesCmuxWakeOnLinux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("listen install writes POSIX services")
	}
	w, err := examples.Wake("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "claude-code.json")
	home := t.TempDir()
	o := listenInstallOptions{
		GOOS: "linux", Home: home, Binary: "/opt/tincan", UID: 501, Launcher: "/A/agent-tincan",
		ConfigPath: cfg, Agent: "claude-code", DefaultConfig: filepath.Join(home, "default", "client.json"), Out: &bytes.Buffer{},
	}
	err = installListener(o, w)
	if err == nil {
		t.Fatal("want a refusal on linux")
	}
	for _, want := range []string{"cmux", "macOS only", "codex, copilot-cli, cursor-cli"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, err := os.Stat(listenServiceFile(home, "linux", "claude-code")); err == nil {
		t.Error("wrote the service file")
	}
	if _, err := os.Stat(filepath.Join(listenDir(cfg), w.File)); err == nil {
		t.Error("wrote the wake script")
	}
	if _, err := os.Stat(listenDir(cfg)); err == nil {
		t.Error("made the wake folder")
	}
}

// An approval key holding a multi-line string is not read as approve, and
// is refused rather than half replaced, leaving the file as it was.
func TestSetCodexApprovalRefusesMultilineValue(t *testing.T) {
	cfg := "/home/o/.config/tincan/codex.json"
	for name, val := range map[string]string{
		"basic":   "\"\"\"\nprompt\"\"\"",
		"literal": "'''\nprompt'''",
		"approve": "\"\"\"\napprove\"\"\"",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			in := "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = \"" + cfg + "\" }\ndefault_tools_approval_mode = " + val + "\n\n[other]\nx = 1\n"
			if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
				t.Fatal(err)
			}
			if e, ok := findCodexEntry(strings.Split(in, "\n")); !ok || e.approve || !e.multiline {
				t.Errorf("entry = %+v ok=%v", e, ok)
			}
			if err := checkCodexEntry(path, cfg, "/def.json"); err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("check err = %v", err)
			}
			changed, notice, err := setCodexApproval(path, "/opt/tincan", cfg, "/def.json")
			if err == nil || changed || notice != "" {
				t.Errorf("changed=%v notice=%q err=%v", changed, notice, err)
			} else {
				for _, want := range []string{path, `"approve" by hand`} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error lacks %q: %v", want, err)
					}
				}
			}
			if b, _ := os.ReadFile(path); string(b) != in {
				t.Errorf("file changed:\n%s", b)
			}
		})
	}
}
