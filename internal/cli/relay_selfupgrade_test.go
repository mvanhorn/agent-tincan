package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// --self-upgrade fills in the two upgrade flags, keeping any the operator
// gave, and creates the dist folder.
func TestApplySelfUpgrade(t *testing.T) {
	state := t.TempDir()
	cmd := relayInstallCmd()
	if err := cmd.Flags().Set("self-upgrade", "true"); err != nil {
		t.Fatal(err)
	}
	if err := applySelfUpgrade(cmd.Flags(), state); err != nil {
		t.Fatal(err)
	}
	args := relayServiceArgs(cmd.Flags())
	for _, a := range args {
		if strings.Contains(a, "self-upgrade") {
			t.Fatalf("the install-only flag reached the service: %q", args)
		}
	}
	if err := checkServiceArgs(args); err != nil {
		t.Fatal(err)
	}
	back := relayCmd()
	if err := back.Flags().Parse(args); err != nil {
		t.Fatalf("tincan relay cannot start with %q: %v", args, err)
	}
	dist := filepath.Join(state, "dist")
	for _, want := range []string{"--dist=" + dist, "--release-url=" + GitHubReleaseURL} {
		if !slices.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	fi, err := os.Stat(dist)
	if err != nil || !fi.IsDir() {
		t.Fatalf("dist folder not created: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o022 != 0 {
		t.Fatalf("dist folder is writable by group or others: %v", fi.Mode().Perm())
	}

	cmd = relayInstallCmd()
	mine := filepath.Join(t.TempDir(), "my-dist")
	if err := cmd.Flags().Set("dist", mine); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("release-url", "https://example.test/releases"); err != nil {
		t.Fatal(err)
	}
	if err := applySelfUpgrade(cmd.Flags(), state); err != nil {
		t.Fatal(err)
	}
	args = relayServiceArgs(cmd.Flags())
	if !slices.Contains(args, "--dist="+mine) || !slices.Contains(args, "--release-url=https://example.test/releases") {
		t.Fatalf("operator's flags were replaced: %q", args)
	}
}

// A relay installed before its first start gets a private state folder,
// as the relay itself would make it, before the dist folder inside it.
func TestApplySelfUpgradeKeepsStatePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	state := filepath.Join(t.TempDir(), "tincan-relay")
	cmd := relayInstallCmd()
	if err := applySelfUpgrade(cmd.Flags(), state); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(state); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state folder mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}

	state = filepath.Join(t.TempDir(), "tincan-relay")
	cmd = relayInstallCmd()
	if err := cmd.Flags().Set("dist", filepath.Join(state, "my-dist")); err != nil {
		t.Fatal(err)
	}
	if err := applySelfUpgrade(cmd.Flags(), state); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(state); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("with --dist inside it, state folder mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}
}

func TestSelfUpgradeOffer(t *testing.T) {
	if got := selfUpgradeOffer(); !strings.Contains(got, "--self-upgrade") || !strings.Contains(got, "relay-upgrade") {
		t.Fatalf("offer = %q", got)
	}
}

func TestSelfUpgradeWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the writability check is Unix-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only folder")
	}
	bin := t.TempDir()
	exe := filepath.Join(bin, "tincan")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	dist := t.TempDir()
	if got := selfUpgradeWarning(exe, dist); got != "" {
		t.Fatalf("writable: warning %q", got)
	}

	if err := os.Chmod(bin, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bin, 0o755) })
	if got := selfUpgradeWarning(exe, dist); !strings.Contains(got, bin) || !strings.Contains(got, "not writable by the relay user") {
		t.Fatalf("read-only binary folder: warning %q", got)
	}
	os.Chmod(bin, 0o755)

	// A read-only dist folder only stops downloads: a release staged there
	// by hand still installs, so the warning does not say upgrades fail.
	if err := os.Chmod(dist, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dist, 0o755) })
	if got := selfUpgradeWarning(exe, dist); !strings.Contains(got, "--from-github") || !strings.Contains(got, "staged there by hand still installs") || strings.Contains(got, "will fail") {
		t.Fatalf("read-only dist folder: warning %q", got)
	}
	os.Chmod(dist, 0o755)

	// Whoever can write a parent can swap the whole dist folder.
	parent := t.TempDir()
	nested := filepath.Join(parent, "dist")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })
	if got := selfUpgradeWarning(exe, nested); !strings.Contains(got, parent) || !strings.Contains(got, "other users") {
		t.Fatalf("dist parent writable by others: warning %q", got)
	}
	// A sticky parent such as /tmp does not let others replace what they do not own.
	if err := os.Chmod(parent, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	if got := selfUpgradeWarning(exe, nested); strings.Contains(got, parent) {
		t.Fatalf("sticky parent flagged: warning %q", got)
	}
	os.Chmod(parent, 0o755)

	// Writing into a folder also needs search permission.
	if err := os.Chmod(dist, 0o655); err != nil {
		t.Fatal(err)
	}
	if got := selfUpgradeWarning(exe, dist); !strings.Contains(got, dist) {
		t.Fatalf("dist without search permission: warning %q", got)
	}
	os.Chmod(dist, 0o755)

	if err := os.Chmod(dist, 0o777); err != nil {
		t.Fatal(err)
	}
	if got := selfUpgradeWarning(exe, dist); !strings.Contains(got, dist) || !strings.Contains(got, "other users") {
		t.Fatalf("dist writable by others: warning %q", got)
	}
	os.Chmod(dist, 0o755)

	if err := os.Chmod(dist, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dist, 0o755) })
	if got := selfUpgradeWarning(exe, dist); !strings.Contains(got, dist) {
		t.Fatalf("read-only dist: warning %q", got)
	}
}

// A --dist that links elsewhere is only as safe as the folders above the
// link's target: whoever can write /srv/shared can swap /srv/shared/releases.
func TestSelfUpgradeWarningSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the writability check is Unix-only")
	}
	exe := filepath.Join(t.TempDir(), "tincan")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	releases := filepath.Join(shared, "releases")
	if err := os.Mkdir(releases, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(shared, 0o755) })
	dist := filepath.Join(t.TempDir(), "dist")
	if err := os.Symlink(releases, dist); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(shared)
	if err != nil {
		t.Fatal(err)
	}
	if got := selfUpgradeWarning(exe, dist); !strings.Contains(got, real) || !strings.Contains(got, "other users") {
		t.Fatalf("dist links into a folder others can write: warning %q, want %s named", got, real)
	}
}

// A relative --dist is checked from the folder the relay runs in up to /,
// not only up to ".".
func TestSelfUpgradeWarningRelativeDist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the writability check is Unix-only")
	}
	exe := filepath.Join(t.TempDir(), "tincan")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	open := t.TempDir()
	work := filepath.Join(open, "relay")
	if err := os.MkdirAll(filepath.Join(work, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(open, 0o755) })
	t.Chdir(work)
	got := selfUpgradeWarning(exe, "dist")
	if !strings.Contains(got, "other users") || !strings.Contains(got, open) {
		t.Fatalf("relative dist inside a folder others can write: warning %q, want %s named", got, open)
	}
}

// --latest resolves the tag on this machine and checks its shape before
// anything reaches the relay.
func TestLatestReleaseTag(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
		wantErr    bool
	}{
		{"stable tag", `{"tag_name":"v0.9.0"}`, 200, "v0.9.0", false},
		{"malformed tag", `{"tag_name":"v0.9.0; rm -rf /"}`, 200, "", true},
		{"no tag", `{}`, 200, "", true},
		{"not found", `{"message":"Not Found"}`, 404, "", true},
		{"bad json", `not json`, 200, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			t.Cleanup(ts.Close)
			got, err := latestReleaseTag(t.Context(), ts.URL)
			if tc.wantErr != (err != nil) || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

// --latest sends the relay the tag it looked up, as from_github, and
// nothing reaches the relay when the lookup fails or the flags clash.
func TestRelayUpgradeLatestFlag(t *testing.T) {
	var hits atomic.Int32
	var tag atomic.Value
	tag.Store("v0.9.0; bad")
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"tag_name":"` + tag.Load().(string) + `"}`))
	}))
	t.Cleanup(gh.Close)
	orig := latestReleaseURL
	latestReleaseURL = gh.URL
	t.Cleanup(func() { latestReleaseURL = orig })

	// The relay's release source holds release 0.9.0 for this platform.
	rig := newSelfUpgradeRig(t, map[string]string{"VERSION": "0.7.0\n"})
	files := goodDist("0.9.0")
	var fetched []string
	var fetchMu sync.Mutex
	rel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchMu.Lock()
		fetched = append(fetched, r.URL.Path)
		fetchMu.Unlock()
		body, ok := files[path.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(rel.Close)
	rig.u.releaseURL = rel.URL

	// Record each upgrade request the relay is sent.
	var sent []client.RelayUpgradeRequest
	var sentMu sync.Mutex
	admin := rig.m.Server.AdminHandler()
	socket := serveHandlerSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == client.RelayUpgradePath {
			body, _ := io.ReadAll(r.Body)
			var in client.RelayUpgradeRequest
			if err := json.Unmarshal(body, &in); err != nil {
				t.Errorf("upgrade request %q: %v", body, err)
			}
			sentMu.Lock()
			sent = append(sent, in)
			sentMu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		admin.ServeHTTP(w, r)
	}))
	requests := func() []client.RelayUpgradeRequest {
		sentMu.Lock()
		defer sentMu.Unlock()
		return slices.Clone(sent)
	}

	run := func(args ...string) (string, error) {
		cmd := relayUpgradeCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append(args, "--socket", socket, "--wait", "0"))
		err := cmd.Execute()
		return out.String(), err
	}
	if _, err := run("--latest", "--from-github", "v0.9.0"); err == nil || !strings.Contains(err.Error(), "--from-github") {
		t.Fatalf("both flags: err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("asked GitHub before refusing the flag pair")
	}
	if _, err := run("--latest"); err == nil || !strings.Contains(err.Error(), "v0.9.0; bad") {
		t.Fatalf("malformed tag: err = %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("GitHub asked %d times, want 1", hits.Load())
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("the relay was sent %+v before a good tag was found", got)
	}

	tag.Store("v0.9.0")
	out, err := run("--latest")
	if got := requests(); len(got) != 1 || got[0].FromGitHub != "v0.9.0" {
		t.Fatalf("relay was sent %+v, want one request with from_github v0.9.0", got)
	}
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !strings.Contains(out, "latest release: v0.9.0") {
		t.Fatalf("output %q lacks the resolved tag", out)
	}
	fetchMu.Lock()
	defer fetchMu.Unlock()
	if !slices.Contains(fetched, "/v0.9.0/checksums.txt") {
		t.Fatalf("relay fetched %q, want the v0.9.0 release", fetched)
	}
}

// serveHandlerSocket serves h on a fresh unix socket, as the relay's local
// admin socket.
func serveHandlerSocket(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}
