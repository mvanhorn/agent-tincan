package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

var platformFile = "tincan_" + runtime.GOOS + "_" + runtime.GOARCH

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeExe writes a stand-in for the running tincan binary and returns its
// path and file info.
func fakeExe(t *testing.T) (string, os.FileInfo) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tincan")
	if err := os.WriteFile(path, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, fi
}

func meshWithDist(t *testing.T, files map[string]string) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.Server.SetDist(dir)
	return m
}

func assertUntouched(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "old binary" {
		t.Fatalf("executable = %q, %v; want the old binary untouched", raw, err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("executable was replaced")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files next to the executable: %v", entries)
	}
}

// An agent without GitHub access updates from the relay: the new binary goes
// to a new inode renamed over the old path, never written in place.
func TestUpgradeReplacesExecutable(t *testing.T) {
	m := meshWithDist(t, map[string]string{platformFile: "new binary", "VERSION": "0.4.0"})
	exe, before := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, false, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil || string(raw) != "new binary" {
		t.Fatalf("executable = %q, %v", raw, err)
	}
	after, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("binary was overwritten in place; want a new inode renamed over it")
	}
	if after.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", after.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
	for _, want := range []string{"0.4.0", "Restart any long-running tincan processes"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

// An executable that already matches the relay's build is left alone.
func TestUpgradeAlreadyCurrent(t *testing.T) {
	m := meshWithDist(t, map[string]string{platformFile: "old binary", "VERSION": "0.4.0"})
	exe, before := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, false, &out); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, exe, before)
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestUpgradeCheckDoesNotWrite(t *testing.T) {
	m := meshWithDist(t, map[string]string{platformFile: "new binary", "VERSION": "0.4.0"})
	exe, before := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, true, false, &out); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, exe, before)
	for _, want := range []string{Version, "0.4.0", "tincan upgrade"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("check output missing %q:\n%s", want, out.String())
		}
	}
}

func TestUpgradeRefusesMissingPlatform(t *testing.T) {
	other := "tincan_linux_arm64"
	if platformFile == other {
		other = "tincan_darwin_arm64"
	}
	m := meshWithDist(t, map[string]string{other: "someone else's binary", "VERSION": "0.4.0"})
	exe, before := fakeExe(t)
	err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), platformFile) {
		t.Fatalf("err = %v, want one naming %s", err, platformFile)
	}
	assertUntouched(t, exe, before)
}

// A download that does not match the manifest checksum never replaces the
// binary.
func TestUpgradeRefusesChecksumMismatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dist":
			json.NewEncoder(w).Encode(client.DistManifest{Version: "0.4.0", Files: []client.DistFile{{Name: platformFile, SHA256: sha([]byte("the real build"))}}})
		case "/v1/dist/" + platformFile:
			w.Write([]byte("tampered build"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	r, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	exe, before := fakeExe(t)
	err = upgrade(t.Context(), r, exe, false, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	assertUntouched(t, exe, before)
}

// A relay without --dist says so instead of failing obscurely.
func TestUpgradeWithoutDist(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	exe, before := fakeExe(t)
	err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--dist") {
		t.Fatalf("err = %v", err)
	}
	assertUntouched(t, exe, before)
}

// After an upgrade, tincan names each tincan mcp still on the old build and
// how its app reloads it; with none running it lists the step per app.
func TestReloadAdvice(t *testing.T) {
	alive := func(mcpserver.Launch) bool { return true }
	ls := []mcpserver.Launch{
		{PID: 10, Client: "claude-code 2.0.1", Version: "0.3.0"},
		{PID: 11, Client: "cursor-vscode 1", Version: "0.4.0"},
		{PID: 12, Client: "codex-mcp-client 1", Version: "0.3.0", Ended: time.Now()},
	}
	got := reloadAdvice(ls, "0.4.0", alive)
	for _, want := range []string{"pid 10", "Claude Code", "Restart any long-running tincan processes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("advice lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"pid 11", "pid 12"} {
		if strings.Contains(got, not) {
			t.Fatalf("advice names %s:\n%s", not, got)
		}
	}
	none := reloadAdvice(nil, "0.4.0", alive)
	for _, want := range []string{"Claude Code", "Codex", "Cursor", "other apps"} {
		if !strings.Contains(none, want) {
			t.Fatalf("advice with no launches lacks %q:\n%s", want, none)
		}
	}
}

// A client newer than the relay's release is not downgraded unless forced;
// the output names both versions and how to bring the relay up instead.
func TestUpgradeRefusesDowngrade(t *testing.T) {
	setVersion(t, "0.8.0")
	m := meshWithDist(t, map[string]string{platformFile: "relay build", "VERSION": "0.7.0"})
	for _, check := range []bool{false, true} {
		exe, before := fakeExe(t)
		var out bytes.Buffer
		if err := upgrade(t.Context(), m.Client(t, "muse"), exe, check, false, &out); err != nil {
			t.Fatal(err)
		}
		assertUntouched(t, exe, before)
		for _, want := range []string{"0.8.0", "0.7.0", "relay-upgrade --from-github v0.8.0"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("check=%v output missing %q:\n%s", check, want, out.String())
			}
		}
		for _, bad := range []string{"Run tincan upgrade to install", "Upgraded", "is available"} {
			if strings.Contains(out.String(), bad) {
				t.Fatalf("check=%v output has %q:\n%s", check, bad, out.String())
			}
		}
	}
}

// A stable client is not replaced by the relay's prerelease of the same
// version without --force, and doctor warns rather than fails.
func TestUpgradeRefusesStableToPrerelease(t *testing.T) {
	setVersion(t, "0.8.0")
	m := meshWithDist(t, map[string]string{platformFile: "relay build", "VERSION": "0.8.0-rc1"})
	exe, before := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, false, &out); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, exe, before)
	if !strings.Contains(out.String(), "not replacing it") {
		t.Fatalf("output = %q", out.String())
	}
	if c := versionCheck(t.Context(), m.Client(t, "muse"), exe); c.Status != "warn" {
		t.Fatalf("doctor = %+v, want warn", c)
	}
}

// A check with --force on a newer client still reports a downgrade instead
// of telling it to install, and changes nothing.
func TestUpgradeCheckForceNewerClient(t *testing.T) {
	setVersion(t, "0.8.0")
	m := meshWithDist(t, map[string]string{platformFile: "relay build", "VERSION": "0.7.0"})
	exe, before := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, true, true, &out); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, exe, before)
	if !strings.Contains(out.String(), "would downgrade") || strings.Contains(out.String(), "Run tincan upgrade to install") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestUpgradeForceDowngrades(t *testing.T) {
	setVersion(t, "0.8.0")
	m := meshWithDist(t, map[string]string{platformFile: "relay build", "VERSION": "0.7.0"})
	exe, _ := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, true, &out); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(exe); string(raw) != "relay build" {
		t.Fatalf("executable = %q, want the relay's build", raw)
	}
	if !strings.Contains(out.String(), "from tincan 0.8.0 to 0.7.0") {
		t.Fatalf("output = %q", out.String())
	}
}

// An older client and a same-release client behave as before.
func TestUpgradeOlderClientInstalls(t *testing.T) {
	setVersion(t, "0.3.0")
	m := meshWithDist(t, map[string]string{platformFile: "new binary", "VERSION": "0.4.0"})
	exe, _ := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, true, false, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"A newer tincan release is available: 0.4.0", "Run tincan upgrade to install it"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("check output missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, false, &out); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(exe); string(raw) != "new binary" {
		t.Fatalf("executable = %q, want the relay's build", raw)
	}

	setVersion(t, "0.4.0")
	out.Reset()
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("same release output = %q", out.String())
	}
}

// After the swap, upgrade runs the new binary's services refresh so the
// macOS services move onto the new build's Agent Tincan.app. A refresh
// failure is reported but does not fail the upgrade: the binary is replaced.
func TestUpgradeRunsServicesRefresh(t *testing.T) {
	m := meshWithDist(t, map[string]string{platformFile: "new binary", "VERSION": "0.4.0"})
	exe, _ := fakeExe(t)
	var ran string
	orig := postUpgradeRefresh
	t.Cleanup(func() { postUpgradeRefresh = orig })
	postUpgradeRefresh = func(newExe string) (string, error) {
		ran = newExe
		return "com.agenttincan.council: updated, restarted\n", errors.New("exit status 1")
	}
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, false, &out); err != nil {
		t.Fatalf("upgrade failed because refresh failed: %v", err)
	}
	if ran != exe {
		t.Fatalf("refresh ran %q, want the new binary %q", ran, exe)
	}
	for _, want := range []string{"com.agenttincan.council: updated, restarted", "tincan services refresh"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}
