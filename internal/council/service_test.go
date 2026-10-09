package council

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// plistStrings is every non-blank text node of a plist, in order, joined
// with "|", so a test can check adjacent ProgramArguments entries.
func plistStrings(s string) string {
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
	return strings.Join(strs, "|")
}

func TestInstallServiceDarwin(t *testing.T) {
	home := t.TempDir()
	res, err := InstallService(history.ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tin & can/tincan", CodexDir: "/opt/homebrew/bin", UID: 501})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.council.plist")
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
	joined := plistStrings(s)
	for _, want := range []string{
		"Label|com.agenttincan.council",
		"/opt/tin & can/tincan|council|serve|EnvironmentVariables",
		"TINCAN_CONFIG|" + home + "/.config/tincan/council.json",
		"KeepAlive",
		home + "/Library/Logs/tincan-council.log",
		"PATH|/opt/homebrew/bin:",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plist missing %q:\n%s", want, joined)
		}
	}
	if res.Next != "launchctl bootstrap gui/501 "+res.Path {
		t.Fatalf("next step = %q", res.Next)
	}
}

func TestInstallServiceLinux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	res, err := InstallService(history.ServiceOptions{GOOS: "linux", Home: home, Binary: "/opt/tin can/tincan"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != filepath.Join(home, ".config", "systemd", "user", "tincan-council.service") {
		t.Fatalf("path = %s", res.Path)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	for _, want := range []string{
		`ExecStart="/opt/tin can/tincan" council serve` + "\n",
		"Environment=TINCAN_CONFIG=%h/.config/tincan/council.json",
		"Restart=always",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("unit missing %q:\n%s", want, s)
		}
	}
	if !strings.Contains(res.Next, "systemctl --user enable --now tincan-council.service") {
		t.Fatalf("next = %q", res.Next)
	}
}

// Through Agent Tincan.app, the launcher comes first and the original
// arguments keep their order.
func TestInstallServiceDarwinUsesLauncher(t *testing.T) {
	home := t.TempDir()
	res, err := InstallService(history.ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tin & can/tincan", UID: 501, Launcher: "/A/Agent Tincan.app/Contents/MacOS/agent-tincan"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	joined := plistStrings(string(b))
	for _, want := range []string{
		"ProgramArguments|/A/Agent Tincan.app/Contents/MacOS/agent-tincan|/opt/tin & can/tincan|council|serve|EnvironmentVariables",
		"AssociatedBundleIdentifiers|com.agenttincan.app",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plist missing %q:\n%s", want, joined)
		}
	}
}
