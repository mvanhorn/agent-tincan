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

var platformFile = releaseAsset(runtime.GOOS, runtime.GOARCH)

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
	// Windows loads a FileInfo's file id from its path on first use, so
	// before would read the new binary; the moved-aside count below covers it.
	if runtime.GOOS != "windows" && os.SameFile(before, after) {
		t.Fatal("binary was overwritten in place; want a new inode renamed over it")
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != 0o755 { // Windows has no mode bits
		t.Fatalf("mode = %v, want 0755", after.Mode().Perm())
	}
	want := 1
	if runtime.GOOS == "windows" {
		want = 2 // the running exe, moved aside
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != want {
		t.Fatalf("leftover temp files: %v", entries)
	}
	for _, want := range []string{"0.4.0", "Restart any long-running tincan processes"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestReleaseAsset(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, want string }{
		{"windows", "amd64", "tincan_windows_amd64.exe"},
		{"windows", "arm64", "tincan_windows_arm64.exe"},
		{"linux", "amd64", "tincan_linux_amd64"},
		{"darwin", "arm64", "tincan_darwin_arm64"},
	} {
		if got := releaseAsset(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("releaseAsset(%s, %s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// oldBinaries lists the moved-aside binaries in dir, oldest first.
func oldBinaries(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "tincan.old-*"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// installBuild runs replaceBinary as an upgrade would, from a new file next
// to exe.
func installBuild(t *testing.T, goos, exe, body string) {
	t.Helper()
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(goos, tmp, exe); err != nil {
		t.Fatal(err)
	}
}

// Windows will not replace a running exe but will rename it. A tincan mcp
// can keep an old file open for days, so a second upgrade must not need the
// first aside file gone, and that file goes once it is free. The held file
// only stays put on Windows, where an open file cannot be deleted.
func TestReplaceBinaryWithFirstAsideFileHeldOpen(t *testing.T) {
	exe, _ := fakeExe(t)
	dir := filepath.Dir(exe)

	installBuild(t, "windows", exe, "second build")
	first := oldBinaries(t, dir)
	if len(first) != 1 {
		t.Fatalf("moved aside: %v, want one file", first)
	}
	if raw, _ := os.ReadFile(first[0]); string(raw) != "old binary" {
		t.Fatalf("aside file holds %q", raw)
	}
	held, err := os.Open(first[0])
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	installBuild(t, "windows", exe, "third build")
	if raw, _ := os.ReadFile(exe); string(raw) != "third build" {
		t.Fatalf("exe holds %q", raw)
	}
	if got := oldBinaries(t, dir); len(got) != 2 {
		t.Fatalf("moved aside: %v, want both builds", got)
	}

	removeOldBinaries(dir)
	want := 0
	if runtime.GOOS == "windows" {
		want = 1 // still held open
	}
	if got := oldBinaries(t, dir); len(got) != want {
		t.Fatalf("after a run with one held open: %v, want %d left", got, want)
	}
	held.Close()
	removeOldBinaries(dir)
	if got := oldBinaries(t, dir); len(got) != 0 {
		t.Fatalf("after the held file was freed: %v", got)
	}
	if raw, _ := os.ReadFile(exe); string(raw) != "third build" {
		t.Fatalf("cleanup touched the exe: %q", raw)
	}
}

// Moving the new file in can fail after the old exe has moved aside. The old
// exe goes back, so the machine is never left without a tincan.
func TestReplaceBinaryRestoresExeWhenMoveFails(t *testing.T) {
	exe, _ := fakeExe(t)
	err := replaceBinary("windows", filepath.Join(filepath.Dir(exe), "no-such-file"), exe)
	if err == nil {
		t.Fatal("want an error")
	}
	if raw, rerr := os.ReadFile(exe); rerr != nil || string(raw) != "old binary" {
		t.Fatalf("exe = %q, %v; want the old binary back", raw, rerr)
	}
	if got := oldBinaries(t, filepath.Dir(exe)); len(got) != 0 {
		t.Fatalf("left aside: %v", got)
	}
}

// Other platforms rename straight over the running binary and leave nothing.
func TestReplaceBinaryLeavesNoAsideFileOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a rename over a file only models unix here; Windows always moves the exe aside")
	}
	exe, _ := fakeExe(t)
	installBuild(t, "linux", exe, "second build")
	if raw, _ := os.ReadFile(exe); string(raw) != "second build" {
		t.Fatalf("exe holds %q", raw)
	}
	if got := oldBinaries(t, filepath.Dir(exe)); len(got) != 0 {
		t.Fatalf("left aside: %v", got)
	}
}

// Only moved-aside binaries go; anything else near the exe stays.
func TestRemoveOldBinariesKeepsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"tincan.exe", "tincan.older", "notes.old-1", "tincan.old-1"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "tincan.old-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	removeOldBinaries(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := "notes.old-1 tincan.exe tincan.old-dir tincan.older"; strings.Join(left, " ") != want {
		t.Fatalf("left %v, want %s", left, want)
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
