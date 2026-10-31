package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// selfUpgradeRig is a relay mesh whose relay "runs" a fake executable at
// version 0.7.0, with an injected exec and a root shutdown the test sees.
type selfUpgradeRig struct {
	m       *testrelay.Mesh
	u       *relayUpgrader
	exe     string
	before  os.FileInfo
	dist    string
	ctx     context.Context // the relay's root context, ended by Restart
	mu      sync.Mutex
	execs   [][]string
	stopped bool
}

func newSelfUpgradeRig(t *testing.T, files map[string]string) *selfUpgradeRig {
	t.Helper()
	skipOnWindows(t, skipRelayHost)
	rig := &selfUpgradeRig{m: testrelay.New(t, relay.Config{Version: "0.7.0"}), dist: t.TempDir()}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(rig.dist, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rig.m.Server.SetDist(rig.dist)
	rig.exe, rig.before = fakeExe(t)
	ctx, cancel := context.WithCancel(t.Context())
	rig.ctx = ctx
	rig.u = &relayUpgrader{
		exe: rig.exe, current: "0.7.0", platform: platformFile, http: http.DefaultClient,
		exec: func(argv0 string, argv, envv []string) error {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.execs = append(rig.execs, append([]string{argv0}, argv...))
			return nil
		},
		stop: func() {
			rig.mu.Lock()
			rig.stopped = true
			rig.mu.Unlock()
			cancel()
		},
	}
	rig.m.Server.SetSelfUpgrader(rig.u)
	return rig
}

// goodDist is a dist holding release 0.8.0 for this platform.
func goodDist(version string) map[string]string {
	return map[string]string{
		platformFile:    "new binary",
		"VERSION":       version + "\n",
		"checksums.txt": sha([]byte("new binary")) + "  " + platformFile + "\n" + sha([]byte("other")) + "  tincan_other\n",
	}
}

func (rig *selfUpgradeRig) upgrade(t *testing.T, who string, in client.RelayUpgradeRequest) (client.RelayUpgradeResult, error) {
	t.Helper()
	return rig.m.Client(t, who).RelayUpgrade(t.Context(), in)
}

func (rig *selfUpgradeRig) assertNoRestart(t *testing.T) {
	t.Helper()
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if rig.stopped || len(rig.execs) != 0 {
		t.Fatalf("relay restarted (stopped=%v, execs=%v) after a refused upgrade", rig.stopped, rig.execs)
	}
	if rig.u.restartErr() != nil {
		t.Fatal("a refused upgrade left a restart pending")
	}
}

func relayUpgradedEvents(t *testing.T, m *testrelay.Mesh) []string {
	t.Helper()
	events, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, e := range events {
		if e.Event == "relay_upgraded" {
			details = append(details, e.Detail)
		}
	}
	return details
}

// An admin upgrade swaps the binary, keeps the old one, audits the versions,
// and replies before the relay restarts; the restart re-executes the new
// binary with the relay's own arguments only after the relay has stopped.
func TestRelayUpgradeSwapsBinaryAndReExecs(t *testing.T) {
	rig := newSelfUpgradeRig(t, goodDist("0.8.0"))
	replied := make(chan struct{})
	stop := rig.u.stop
	rig.u.stop = func() {
		// The handler signals the restart only once the full reply is on
		// the wire: the admin's call must be able to finish first.
		select {
		case <-replied:
		case <-time.After(5 * time.Second):
			t.Error("the relay asked to restart before the admin had its reply")
		}
		stop()
	}
	var order []string
	var orderMu sync.Mutex
	note := func(s string) { orderMu.Lock(); order = append(order, s); orderMu.Unlock() }
	exec := rig.u.exec
	rig.u.exec = func(argv0 string, argv, envv []string) error { note("exec"); return exec(argv0, argv, envv) }

	// What runRelay and the relay command do: wait for the root shutdown,
	// then act on the restart.
	done := make(chan error, 1)
	go func() {
		<-rig.ctx.Done()
		err := rig.u.restartErr()
		rs, ok := errors.AsType[*errRelayRestart](err)
		if !ok {
			done <- err
			return
		}
		done <- rs.u.restart()
	}()

	res, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	note("reply")
	close(replied)
	if res.From != "0.7.0" || res.To != "0.8.0" || res.Restart != "re-exec" {
		t.Fatalf("result = %+v", res)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never restarted")
	}
	if !slices.Equal(order, []string{"reply", "exec"}) {
		t.Fatalf("order = %v, want the reply before the exec", order)
	}
	if len(rig.execs) != 1 || rig.execs[0][0] != rig.exe || !slices.Equal(rig.execs[0][1:], os.Args) {
		t.Fatalf("execs = %v, want %s with the relay's own arguments", rig.execs, rig.exe)
	}

	if raw, _ := os.ReadFile(rig.exe); string(raw) != "new binary" {
		t.Fatalf("executable = %q", raw)
	}
	after, _ := os.Stat(rig.exe)
	if os.SameFile(rig.before, after) || after.Mode().Perm() != 0o755 {
		t.Fatalf("want a new 0755 inode renamed over the binary, got mode %v", after.Mode().Perm())
	}
	if raw, _ := os.ReadFile(rig.exe + ".0.7.0"); string(raw) != "old binary" {
		t.Fatalf("backup = %q, want the old binary", raw)
	}
	if entries, _ := os.ReadDir(filepath.Dir(rig.exe)); len(entries) != 2 {
		t.Fatalf("files next to the executable: %v, want it and its backup", entries)
	}
	details := relayUpgradedEvents(t, rig.m)
	if len(details) != 1 || !strings.Contains(details[0], `"from":"0.7.0"`) || !strings.Contains(details[0], `"to":"0.8.0"`) {
		t.Fatalf("relay_upgraded audit details = %v", details)
	}

	// A second upgrade while the restart is pending is refused.
	if _, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{Force: true}); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("second upgrade err = %v, want 409", err)
	}
}

// With --upgrade-exit the relay exits 75 for its supervisor instead.
func TestRelayUpgradeExitMode(t *testing.T) {
	rig := newSelfUpgradeRig(t, goodDist("0.8.0"))
	rig.u.exitMode = true
	res, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{})
	if err != nil || res.Restart != "exit" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	rs, ok := errors.AsType[*errRelayRestart](rig.u.restartErr())
	if !ok {
		t.Fatal("no restart pending after the upgrade")
	}
	err = rs.u.restart()
	if code, _ := ExitStatus(err); code != 75 {
		t.Fatalf("exit status = %d (%v), want 75", code, err)
	}
	if len(rig.execs) != 0 {
		t.Fatalf("exit mode re-executed: %v", rig.execs)
	}
}

// Agents and strangers cannot upgrade the relay.
func TestRelayUpgradeAdminOnly(t *testing.T) {
	rig := newSelfUpgradeRig(t, goodDist("0.8.0"))
	for _, who := range []string{"muse", "stranger"} {
		if _, err := rig.upgrade(t, who, client.RelayUpgradeRequest{Force: true}); !client.IsStatus(err, http.StatusForbidden) {
			t.Fatalf("%s: err = %v, want 403", who, err)
		}
	}
	assertUntouched(t, rig.exe, rig.before)
	rig.assertNoRestart(t)
}

// Refused upgrades change nothing and do not restart the relay.
func TestRelayUpgradeRefusals(t *testing.T) {
	other := "tincan_linux_arm64"
	if platformFile == other {
		other = "tincan_darwin_arm64"
	}
	cases := []struct {
		name  string
		files map[string]string
		force bool
		code  int
		want  string
	}{
		{"same version", goodDist("0.7.0"), false, http.StatusConflict, "not newer"},
		{"older version", goodDist("0.6.0"), false, http.StatusConflict, "not newer"},
		{"checksum mismatch", map[string]string{
			platformFile: "tampered build", "VERSION": "0.8.0",
			"checksums.txt": sha([]byte("the real build")) + "  " + platformFile + "\n",
		}, false, http.StatusUnprocessableEntity, "checksum mismatch"},
		{"missing platform binary", map[string]string{
			other: "someone else's", "VERSION": "0.8.0",
			"checksums.txt": sha([]byte("someone else's")) + "  " + other + "\n",
		}, true, http.StatusUnprocessableEntity, platformFile},
		{"binary not in checksums", map[string]string{
			platformFile: "new binary", "VERSION": "0.8.0", "checksums.txt": "",
		}, true, http.StatusUnprocessableEntity, platformFile},
		{"no checksums", map[string]string{platformFile: "new binary", "VERSION": "0.8.0"}, true, http.StatusUnprocessableEntity, "checksums.txt"},
		{"no VERSION", map[string]string{platformFile: "new binary"}, true, http.StatusUnprocessableEntity, "VERSION"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSelfUpgradeRig(t, tc.files)
			_, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{Force: tc.force})
			if !client.IsStatus(err, tc.code) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want HTTP %d naming %q", err, tc.code, tc.want)
			}
			assertUntouched(t, rig.exe, rig.before)
			rig.assertNoRestart(t)
			if d := relayUpgradedEvents(t, rig.m); len(d) != 0 {
				t.Fatalf("refused upgrade was audited as done: %v", d)
			}
		})
	}
}

// A relay on a prerelease accepts its own stable release as an upgrade
// without --force, which is what doctor tells the owner to run.
func TestRelayUpgradePrereleaseToStable(t *testing.T) {
	rig := newSelfUpgradeRig(t, goodDist("0.8.0"))
	rig.u.current = "0.8.0-rc1"
	res, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{})
	if err != nil || res.To != "0.8.0" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

// --force reinstalls a release that is not newer.
func TestRelayUpgradeForce(t *testing.T) {
	rig := newSelfUpgradeRig(t, goodDist("0.7.0"))
	res, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{Force: true})
	if err != nil || res.To != "0.7.0" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if raw, _ := os.ReadFile(rig.exe); string(raw) != "new binary" {
		t.Fatalf("executable = %q", raw)
	}
}

// A relay user that cannot replace its binary gets a clear refusal.
func TestRelayUpgradeRefusesUnwritableBinary(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anything")
	}
	rig := newSelfUpgradeRig(t, goodDist("0.8.0"))
	dir := filepath.Dir(rig.exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	_, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{})
	if !client.IsStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "own its binary") {
		t.Fatalf("err = %v, want a 409 saying the relay user must own its binary", err)
	}
	assertUntouched(t, rig.exe, rig.before)
	rig.assertNoRestart(t)
}

// A relay without --dist says so.
func TestRelayUpgradeWithoutDist(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	_, err := m.Client(t, "admin").RelayUpgrade(t.Context(), client.RelayUpgradeRequest{})
	if !client.IsStatus(err, http.StatusNotFound) || !strings.Contains(err.Error(), "--dist") {
		t.Fatalf("err = %v", err)
	}
}

// releaseServer serves a GitHub-style release download tree.
func releaseServer(t *testing.T, tag string, files map[string]string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, "/"+tag+"/")
		body, found := files[name]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// --from-github downloads every release binary into the dist, checked
// against the release's checksums.txt, then installs from there.
func TestRelayUpgradeFromGitHub(t *testing.T) {
	other := "tincan_linux_arm64"
	if platformFile == other {
		other = "tincan_darwin_arm64"
	}
	rel := map[string]string{
		platformFile: "release binary",
		other:        "other platform",
		"checksums.txt": sha([]byte("release binary")) + "  " + platformFile + "\n" +
			sha([]byte("other platform")) + "  " + other + "\n",
	}
	rig := newSelfUpgradeRig(t, map[string]string{"VERSION": "0.7.0\n"})
	rig.u.releaseURL = releaseServer(t, "v0.8.0", rel).URL
	res, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{FromGitHub: "v0.8.0"})
	if err != nil || res.To != "0.8.0" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	for name, want := range map[string]string{platformFile: "release binary", other: "other platform", "checksums.txt": rel["checksums.txt"], "VERSION": "0.8.0\n"} {
		if raw, _ := os.ReadFile(filepath.Join(rig.dist, name)); string(raw) != want {
			t.Fatalf("dist %s = %q, want %q", name, raw, want)
		}
	}
	if entries, _ := os.ReadDir(rig.dist); len(entries) != 4 {
		t.Fatalf("dist holds %v, want the release files and no temp dir", entries)
	}
	if raw, _ := os.ReadFile(rig.exe); string(raw) != "release binary" {
		t.Fatalf("executable = %q", raw)
	}
	if d := relayUpgradedEvents(t, rig.m); len(d) != 1 || !strings.Contains(d[0], `"source":"github"`) {
		t.Fatalf("audit = %v", d)
	}
}

// A release binary that does not match its checksums.txt leaves the dist
// and the relay binary as they were.
func TestRelayUpgradeFromGitHubChecksumMismatch(t *testing.T) {
	rel := map[string]string{
		platformFile:    "tampered",
		"checksums.txt": sha([]byte("release binary")) + "  " + platformFile + "\n",
	}
	rig := newSelfUpgradeRig(t, map[string]string{"VERSION": "0.7.0\n"})
	rig.u.releaseURL = releaseServer(t, "v0.8.0", rel).URL
	_, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{FromGitHub: "0.8.0"})
	if !client.IsStatus(err, http.StatusUnprocessableEntity) || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(rig.dist); len(entries) != 1 {
		t.Fatalf("dist holds %v, want only the old VERSION", entries)
	}
	if raw, _ := os.ReadFile(filepath.Join(rig.dist, "VERSION")); string(raw) != "0.7.0\n" {
		t.Fatalf("VERSION = %q", raw)
	}
	assertUntouched(t, rig.exe, rig.before)
	rig.assertNoRestart(t)
}

// Downloads can be turned off, an unknown tag says so, and a tag that is
// not newer is refused before anything is downloaded.
func TestRelayUpgradeFromGitHubRefusals(t *testing.T) {
	rig := newSelfUpgradeRig(t, map[string]string{"VERSION": "0.7.0\n"})
	if _, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{FromGitHub: "v0.8.0"}); !client.IsStatus(err, http.StatusUnprocessableEntity) || !strings.Contains(err.Error(), "--release-url") {
		t.Fatalf("downloads off: err = %v", err)
	}
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.NotFound(w, r) }))
	t.Cleanup(ts.Close)
	rig.u.releaseURL = ts.URL
	if _, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{FromGitHub: "v0.9.0"}); !client.IsStatus(err, http.StatusUnprocessableEntity) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown tag: err = %v", err)
	}
	hits.Store(0)
	if _, err := rig.upgrade(t, "admin", client.RelayUpgradeRequest{FromGitHub: "v0.7.0"}); !client.IsStatus(err, http.StatusConflict) || hits.Load() != 0 {
		t.Fatalf("same tag: err = %v, downloads = %d", err, hits.Load())
	}
	assertUntouched(t, rig.exe, rig.before)
	rig.assertNoRestart(t)
}

// The command prints the versions and, with --wait, sees the relay back.
func TestRelayUpgradeCommandOutput(t *testing.T) {
	rig := newSelfUpgradeRig(t, goodDist("0.8.0"))
	var out bytes.Buffer
	if err := relayUpgradeRun(t.Context(), rig.m.Client(t, "admin"), client.RelayUpgradeRequest{}, 0, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"0.8.0", "0.7.0", "re-executing", "tincan agents"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

// Refusals the admin can act on say how.
func TestRelayUpgradeCommandHints(t *testing.T) {
	rig := newSelfUpgradeRig(t, goodDist("0.7.0"))
	err := relayUpgradeRun(t.Context(), rig.m.Client(t, "admin"), client.RelayUpgradeRequest{}, 0, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v, want a --force hint", err)
	}
	old := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(old.Close)
	r, err := client.NewRelay(old.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	err = relayUpgradeRun(t.Context(), r, client.RelayUpgradeRequest{}, 0, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("err = %v, want an older-relay hint", err)
	}
}

func TestParseChecksums(t *testing.T) {
	h := sha([]byte("x"))
	got := parseChecksums([]byte(h + "  tincan_linux_amd64\n" + strings.ToUpper(h) + " *tincan_darwin_arm64\nnot a line\nabc  short\n"))
	if len(got) != 2 || got["tincan_linux_amd64"] != h || got["tincan_darwin_arm64"] != h {
		t.Fatalf("parsed = %v", got)
	}
}

// A publish that fails part way puts every dist file back as it was.
func TestPublishDistRollsBack(t *testing.T) {
	arm := "tincan_linux_arm64"
	if platformFile == arm {
		arm = "tincan_darwin_arm64"
	}
	dist := t.TempDir()
	for name, body := range map[string]string{platformFile: "old build", "checksums.txt": "old sums", "VERSION": "0.7.0\n"} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stage := t.TempDir()
	for name, body := range map[string]string{platformFile: "new build", arm: "new other build", "checksums.txt": "new sums", "VERSION": "0.8.0\n"} {
		if err := os.WriteFile(filepath.Join(stage, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := publishRename
	t.Cleanup(func() { publishRename = orig })
	publishRename = func(from, to string) error {
		// A file the dist already has stays in place until its
		// replacement lands, so downloads never see it missing.
		for _, name := range []string{platformFile, "checksums.txt", "VERSION"} {
			if _, err := os.Stat(filepath.Join(dist, name)); err != nil {
				t.Errorf("%s missing from the dist while publishing %s", name, filepath.Base(to))
			}
		}
		if filepath.Base(to) == "VERSION" {
			return errors.New("disk full")
		}
		return orig(from, to)
	}
	intact, err := publishDist(dist, stage, []string{platformFile, arm, "checksums.txt", "VERSION"})
	if err == nil || !intact || !strings.Contains(err.Error(), "not changed") {
		t.Fatalf("intact = %v, err = %v", intact, err)
	}
	if entries, _ := os.ReadDir(dist); len(entries) != 3 {
		t.Fatalf("dist holds %v, want only the old files", entries)
	}
	for name, want := range map[string]string{platformFile: "old build", "checksums.txt": "old sums", "VERSION": "0.7.0\n"} {
		if raw, _ := os.ReadFile(filepath.Join(dist, name)); string(raw) != want {
			t.Fatalf("%s = %q, want %q", name, raw, want)
		}
	}
}
