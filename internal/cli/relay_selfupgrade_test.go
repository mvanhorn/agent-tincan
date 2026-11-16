package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// --self-upgrade fills in the two upgrade flags, keeping any the operator
// gave, and creates the dist folder.
func TestApplySelfUpgrade(t *testing.T) {
	state := t.TempDir()
	cmd := relayInstallCmd()
	if err := applySelfUpgrade(cmd.Flags(), state); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("self-upgrade", "true"); err != nil {
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
	if fi, err := os.Stat(dist); err != nil || !fi.IsDir() {
		t.Fatalf("dist folder not created: %v", err)
	}
	if fi, _ := os.Stat(dist); runtime.GOOS != "windows" && fi.Mode().Perm()&0o022 != 0 {
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
}

func TestSelfUpgradeOffer(t *testing.T) {
	if got := selfUpgradeOffer(""); !strings.Contains(got, "--self-upgrade") || !strings.Contains(got, "relay-upgrade") {
		t.Fatalf("offer = %q", got)
	}
	if got := selfUpgradeOffer("/srv/dist"); got != "" {
		t.Fatalf("offer with --dist set = %q", got)
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
	if got := selfUpgradeWarning(exe, dist); !strings.Contains(got, bin) || !strings.Contains(got, "tincan relay-upgrade") {
		t.Fatalf("read-only binary folder: warning %q", got)
	}
	os.Chmod(bin, 0o755)

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

func TestRelayUpgradeLatestFlag(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"tag_name":"v0.9.0; bad"}`))
	}))
	t.Cleanup(ts.Close)
	orig := latestReleaseURL
	latestReleaseURL = ts.URL
	t.Cleanup(func() { latestReleaseURL = orig })

	run := func(args ...string) (string, error) {
		cmd := relayUpgradeCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append(args, "--relay", "http://127.0.0.1:1", "--wait", "0"))
		err := cmd.Execute()
		return out.String(), err
	}
	if _, err := run("--latest", "--from-github", "v0.9.0"); err == nil || !strings.Contains(err.Error(), "--from-github") {
		t.Fatalf("both flags: err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("asked GitHub before refusing the flag pair")
	}
	if _, err := run("--latest"); err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatalf("malformed tag: err = %v", err)
	}
}
