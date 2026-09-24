package history

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The example plist in the repo is the template install writes.
func TestExamplePlistIsTheInstallTemplate(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "history", ServiceLabel+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != launchdTemplate {
		t.Fatal("examples/history/com.agenttincan.history.plist differs from launchdTemplate; keep them identical")
	}
}

func TestInstallServiceDarwin(t *testing.T) {
	home := t.TempDir()
	res, err := InstallService(ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/it's <here>/tincan", CodexDir: "/opt/homebrew/bin", UID: 501})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist")
	if res.Path != want {
		t.Fatalf("path = %s, want %s", res.Path, want)
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "__") {
		t.Fatalf("placeholder left in plist:\n%s", s)
	}
	// Well-formed XML, with the binary escaped rather than breaking out.
	dec := xml.NewDecoder(strings.NewReader(s))
	var strs []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if cd, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(cd)) != "" {
			strs = append(strs, string(cd))
		}
	}
	joined := strings.Join(strs, "|")
	for _, want := range []string{
		"/opt/it's <here>/tincan|history|serve",
		home + "/.config/tincan/history.json",
		home + "/Library/Logs/tincan-history.log",
		"/opt/homebrew/bin:",
		"KeepAlive",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plist missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(res.Next, "launchctl bootstrap gui/501 "+res.Path) {
		t.Fatalf("next step = %q", res.Next)
	}
	if st, _ := os.Stat(res.Path); st.Mode().Perm() != 0o644 {
		t.Fatalf("plist mode %v", st.Mode())
	}
}

func TestInstallServiceLinux(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no codex or claude found on this machine
	home := t.TempDir()
	res, err := InstallService(ServiceOptions{GOOS: "linux", Home: home, Binary: "/opt/tin can/tincan"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != filepath.Join(home, ".config", "systemd", "user", "tincan-history.service") {
		t.Fatalf("path = %s", res.Path)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	for _, want := range []string{`ExecStart="/opt/tin can/tincan" history serve`, "Environment=TINCAN_CONFIG=%h/.config/tincan/history.json", "Restart=always", "WantedBy=default.target"} {
		if !strings.Contains(s, want) {
			t.Errorf("unit missing %q:\n%s", want, s)
		}
	}
	if !strings.Contains(res.Next, "systemctl --user enable --now tincan-history.service") {
		t.Fatalf("next = %q", res.Next)
	}
	if strings.Contains(s, "homebrew") || !strings.Contains(s, home+"/.local/bin") {
		t.Errorf("unit PATH should carry user bins and no macOS paths:\n%s", s)
	}
}

func TestInstallServiceRejectsUnsafeBinary(t *testing.T) {
	for _, bin := range []string{"relative/tincan", "/opt/bad\nline", `/opt/"quoted"/tincan`} {
		if _, err := InstallService(ServiceOptions{GOOS: "linux", Home: t.TempDir(), Binary: bin}); err == nil {
			t.Errorf("binary %q accepted", bin)
		}
	}
	if _, err := InstallService(ServiceOptions{GOOS: "windows", Home: t.TempDir(), Binary: `C:\tincan.exe`}); err == nil {
		t.Error("windows accepted; there is no service definition for it")
	}
}

func TestInstallWebServiceDarwinWritesPlistNotLoaded(t *testing.T) {
	for _, site := range WebSites {
		home := t.TempDir()
		res, err := InstallWebService(site, ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tin <can>/tincan", UID: 501})
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.web."+string(site)+".plist")
		if res.Path != want {
			t.Fatalf("path = %s, want %s", res.Path, want)
		}
		b, err := os.ReadFile(res.Path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, "__") {
			t.Fatalf("placeholder left:\n%s", s)
		}
		var strs []string
		dec := xml.NewDecoder(strings.NewReader(s))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if cd, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(cd)) != "" {
				strs = append(strs, string(cd))
			}
		}
		joined := strings.Join(strs, "|")
		agent := WebAgentName(site)
		for _, want := range []string{
			"com.agenttincan.web." + string(site),
			"/opt/tin <can>/tincan|web|serve|--site|" + string(site),
			home + "/.config/tincan/" + agent + ".json",
			home + "/Library/Logs/tincan-" + agent + ".log",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("plist missing %q:\n%s", want, joined)
			}
		}
		// Written, never loaded: the caller gets the command to run.
		if res.Next != "launchctl bootstrap gui/501 "+res.Path {
			t.Fatalf("next = %q", res.Next)
		}
		if st, _ := os.Stat(res.Path); st.Mode().Perm() != 0o644 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
	if WebAgentName(SourceChatGPT) != "chatgpt-web" || WebAgentName(SourceClaudeAI) != "claude-web" {
		t.Fatal("default agent names changed")
	}
}

func TestInstallWebServiceLinuxAndBadInput(t *testing.T) {
	home := t.TempDir()
	res, err := InstallWebService(SourceClaudeAI, ServiceOptions{GOOS: "linux", Home: home, Binary: "/opt/tincan"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	for _, want := range []string{`ExecStart="/opt/tincan" web serve --site claude-ai`, "TINCAN_CONFIG=%h/.config/tincan/claude-web.json"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("unit missing %q:\n%s", want, b)
		}
	}
	if !strings.HasSuffix(res.Next, "tincan-claude-web.service") {
		t.Fatalf("next = %q", res.Next)
	}
	if _, err := InstallWebService("codex", ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tincan"}); err == nil {
		t.Fatal("site codex accepted")
	}
	if _, err := InstallWebService(SourceChatGPT, ServiceOptions{GOOS: "darwin", Home: home, Binary: "rel/tincan"}); err == nil {
		t.Fatal("relative binary accepted")
	}
}

// Tool directories with spaces stay on the service PATH; a repeat is
// dropped, and so is a directory that cannot be written into a PATH or a
// systemd line at all (a colon, a newline, a NUL, a double quote).
func TestServicePathKeepsSpacesDropsUnsafe(t *testing.T) {
	got := servicePath("linux", "", "/opt/my tools/bin", "/opt/my tools/bin", "/usr/bin", "/a:b", "/bad\ndir", "/bad\x00dir", `/q"d`, "relative/bin", "", "/opt/100%/bin", `/opt/back\slash`, "/opt/$HOME/bin")
	want := "/opt/my tools/bin:/usr/bin:/opt/100%/bin:/opt/back\\slash:/opt/$HOME/bin:/usr/local/bin:/bin"
	if got != want {
		t.Fatalf("servicePath = %q\nwant %q", got, want)
	}
}

func TestInstallServiceToolDirWithSpace(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	res, err := InstallService(ServiceOptions{GOOS: "linux", Home: home, Binary: "/opt/tincan", CodexDir: "/opt/my tools/bin", ClaudeDir: "/opt/50% & more"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	// The whole assignment is quoted so the space stays in the value, and
	// % is doubled so systemd does not read it as a specifier.
	if want := `Environment="PATH=/opt/my tools/bin:/opt/50%% & more:` + home + `/.local/bin:`; !strings.Contains(string(b), want) {
		t.Fatalf("unit missing %q:\n%s", want, b)
	}
	res, err = InstallService(ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tincan", CodexDir: "/opt/my tools/bin", ClaudeDir: "/opt/50% & more", UID: 501})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(res.Path)
	if want := "<string>/opt/my tools/bin:/opt/50% &amp; more:" + home + "/.local/bin:"; !strings.Contains(string(b), want) {
		t.Fatalf("plist missing %q:\n%s", want, b)
	}
}

// systemdEscape makes a value safe inside a double-quoted systemd
// assignment.
func TestSystemdEscape(t *testing.T) {
	if got := systemdEscape(`/a b\c%d`); got != `/a b\\c%%d` {
		t.Fatalf("systemdEscape = %q", got)
	}
}
