package cli

import (
	"bytes"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/examples"
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
		CodexConfig: filepath.Join(home, ".codex", "config.toml"),
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

// Agents on one machine keep their scripts apart, and a script whose path
// has a space and an apostrophe still runs as one path through sh -c.
func TestListenInstallQuotesTheScript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Ben's Configs")
	cfg := filepath.Join(dir, "codex.json")
	o, _ := installForTest(t, "linux", cfg, codexWake(t))
	script := filepath.Join(dir, "codex.listen", "codex-wake.sh")
	got := listenExecArg(script)
	if _, err := exec.LookPath("sh"); err == nil {
		out, err := exec.Command("sh", "-c", "printf %s "+got).Output()
		if err != nil || string(out) != script {
			t.Fatalf("sh -c saw %q (%v), want %q", out, err, script)
		}
	}
	if _, err := os.Stat(filepath.Join(o.Home, ".config", "systemd", "user", "tincan-listen-codex.service")); err != nil {
		t.Fatal(err)
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
	o := listenInstallOptions{GOOS: "linux", Home: t.TempDir(), Binary: "/opt/tincan", ConfigPath: "/x/c.json", Out: &bytes.Buffer{}}
	if err := installListener(o, codexWake(t)); err == nil || !strings.Contains(err.Error(), "join") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.Home, ".config")); err == nil {
		t.Fatal("wrote files before refusing")
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
			if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
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
	run := func() check { return listenerCheck(o.GOOS, o.Home, cfg, o.CodexConfig, o.DefaultConfig, loaded) }

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

	if err := os.WriteFile(o.CodexConfig, []byte("[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nenv = { TINCAN_CONFIG = \""+cfg+"\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := run(); c.Status != "warn" || !strings.Contains(c.Detail, "approval") {
		t.Fatalf("no approval mode: %+v", c)
	}
	if err := os.WriteFile(o.CodexConfig, []byte("[mcp_servers.agent-tincan]\ncommand = \"tincan\"\ndefault_tools_approval_mode = \"approve\"\nenv = { TINCAN_CONFIG = \"/elsewhere/other.json\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := run(); c.Status != "warn" || !strings.Contains(c.Detail, "other.json") {
		t.Fatalf("other agent's config: %+v", c)
	}

	if c := listenerCheck(o.GOOS, o.Home, filepath.Join(t.TempDir(), "none.json"), o.CodexConfig, o.DefaultConfig, loaded); c.Name != "" {
		t.Fatalf("no listener installed should add no line: %+v", c)
	}
}
