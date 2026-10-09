package notes

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
func plistStrings(t *testing.T, s string) string {
	t.Helper()
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
	res, err := InstallService(ServiceOptions{
		ServiceOptions: history.ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tin & can/tincan", CodexDir: "/opt/homebrew/bin", UID: 501},
		LibraryRoot:    "/Users/me/Documents/My <Notes>",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.notes.plist")
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
	joined := plistStrings(t, s)
	for _, want := range []string{
		"Label|com.agenttincan.notes",
		"/opt/tin & can/tincan|notes|serve|--library-root|/Users/me/Documents/My <Notes>|--helper|" + DefaultHelperPath,
		"TINCAN_CONFIG|" + home + "/.config/tincan/notes.json",
		"KeepAlive",
		"RunAtLoad",
		home + "/Library/Logs/tincan-notes.log",
		"PATH|/opt/homebrew/bin:",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plist missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(s, "<key>KeepAlive</key>\n  <true/>") {
		t.Errorf("KeepAlive is not true:\n%s", s)
	}
	if res.Next != "launchctl bootstrap gui/501 "+res.Path {
		t.Fatalf("next step = %q", res.Next)
	}
}

func TestInstallServiceCustomHelper(t *testing.T) {
	home := t.TempDir()
	res, err := InstallService(ServiceOptions{
		ServiceOptions: history.ServiceOptions{GOOS: "darwin", Home: home, Binary: "/usr/local/bin/tincan", UID: 501},
		LibraryRoot:    "/lib",
		HelperPath:     "/opt/agent-notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	if j := plistStrings(t, string(b)); !strings.Contains(j, "--library-root|/lib|--helper|/opt/agent-notes") {
		t.Fatalf("custom helper not in args:\n%s", j)
	}
}

func TestInstallServiceLinux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	res, err := InstallService(ServiceOptions{
		ServiceOptions: history.ServiceOptions{GOOS: "linux", Home: home, Binary: "/opt/tin can/tincan"},
		LibraryRoot:    `/srv/my "notes"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != filepath.Join(home, ".config", "systemd", "user", "tincan-notes.service") {
		t.Fatalf("path = %s", res.Path)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	for _, want := range []string{
		`ExecStart="/opt/tin can/tincan" notes serve --library-root "/srv/my \"notes\"" --helper "` + DefaultHelperPath + `"`,
		"Environment=TINCAN_CONFIG=%h/.config/tincan/notes.json",
		"Restart=always",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("unit missing %q:\n%s", want, s)
		}
	}
	if !strings.Contains(res.Next, "systemctl --user enable --now tincan-notes.service") {
		t.Fatalf("next = %q", res.Next)
	}
}

func TestInstallServiceRejectsBadLibraryRoot(t *testing.T) {
	for _, root := range []string{"", "relative/notes", "/a\nb", "/a\x00b"} {
		_, err := InstallService(ServiceOptions{
			ServiceOptions: history.ServiceOptions{GOOS: "darwin", Home: t.TempDir(), Binary: "/usr/local/bin/tincan", UID: 501},
			LibraryRoot:    root,
		})
		if err == nil {
			t.Errorf("library root %q accepted", root)
		}
	}
	// $ is a systemd expansion; refuse it rather than write a unit that
	// runs against another folder.
	_, err := InstallService(ServiceOptions{
		ServiceOptions: history.ServiceOptions{GOOS: "linux", Home: t.TempDir(), Binary: "/usr/local/bin/tincan"},
		LibraryRoot:    "/srv/$HOME/notes",
	})
	if err == nil {
		t.Error("linux library root with $ accepted")
	}
}

func TestDefaultPaths(t *testing.T) {
	home := "/Users/me"
	if got := DefaultAppSupportRoot("darwin", home); got != "/Users/me/Library/Application Support/tincan-notes" {
		t.Fatalf("darwin app support = %s", got)
	}
	if got := DefaultAppSupportRoot("linux", home); got != "/Users/me/.local/share/tincan-notes" {
		t.Fatalf("linux app support = %s", got)
	}
	if got := HealthPathIn("/x"); got != "/x/tincan-notes-health.json" {
		t.Fatalf("health path = %s", got)
	}
}

// Notes never runs through the launcher: macOS may attribute its Files and
// Folders grant to the app, which every wrapped service would then share.
func TestInstallServiceNeverUsesLauncher(t *testing.T) {
	home := t.TempDir()
	res, err := InstallService(ServiceOptions{
		ServiceOptions: history.ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tincan", UID: 501, Launcher: "/A/agent-tincan"},
		LibraryRoot:    "/Users/me/Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	if s := string(b); strings.Contains(s, "/A/agent-tincan") || strings.Contains(s, "AssociatedBundleIdentifiers") {
		t.Fatalf("notes plist was wrapped:\n%s", s)
	}
}
