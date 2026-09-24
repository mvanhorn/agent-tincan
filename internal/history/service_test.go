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
