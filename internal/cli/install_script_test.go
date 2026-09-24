package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRelease serves a GitHub-shaped releases API and download tree for
// site/install.sh.
type fakeRelease struct {
	tag       string
	asset     string
	binary    []byte
	checksums string
	apiStatus int
	apiHits   atomic.Int32
}

func (f *fakeRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/releases":
		f.apiHits.Add(1)
		if f.apiStatus != 0 {
			http.Error(w, `{"message":"Not Found"}`, f.apiStatus)
			return
		}
		// Pretty-printed like the real API, with a prerelease first and a
		// comma inside a string to make sure the parser copes.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[\n  {\n    \"url\": \"x\",\n    \"name\": \"rc, first\",\n    \"tag_name\": \"" + f.tag + "\",\n    \"prerelease\": true\n  }\n]\n"))
	case "/download/" + f.tag + "/" + f.asset:
		_, _ = w.Write(f.binary)
	case "/download/" + f.tag + "/checksums.txt":
		_, _ = w.Write([]byte(f.checksums))
	default:
		http.NotFound(w, r)
	}
}

func installScriptAsset(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh; not supported on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if (runtime.GOOS != "darwin" && runtime.GOOS != "linux") || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skipf("install.sh has no build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return "tincan_" + runtime.GOOS + "_" + runtime.GOARCH
}

func newFakeRelease(t *testing.T, tag, version string) *fakeRelease {
	asset := installScriptAsset(t)
	bin := []byte("#!/bin/sh\necho \"tincan " + version + "\"\n")
	sum := sha256.Sum256(bin)
	return &fakeRelease{
		tag:    tag,
		asset:  asset,
		binary: bin,
		checksums: "0000000000000000000000000000000000000000000000000000000000000000  tincan_other_arch\n" +
			hex.EncodeToString(sum[:]) + "  " + asset + "\n",
	}
}

// runInstallScript runs site/install.sh against srv with extra env, returning
// the install dir, combined output and the exit error.
func runInstallScript(t *testing.T, srv *httptest.Server, env ...string) (string, string, error) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "site", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"TINCAN_VERSION=",
		"TINCAN_RELEASES_API="+srv.URL+"/api/releases",
		"TINCAN_DOWNLOAD_BASE="+srv.URL+"/download",
		"TINCAN_INSTALL_DIR="+dir,
	)
	cmd.Env = append(cmd.Env, env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return dir, out.String(), err
}

func TestInstallScriptInstallsNewestRelease(t *testing.T) {
	f := newFakeRelease(t, "v9.9.9-rc1", "9.9.9-rc1")
	srv := httptest.NewServer(f)
	defer srv.Close()

	dir, out, err := runInstallScript(t, srv)
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "tincan"))
	if err != nil {
		t.Fatalf("tincan not installed: %v\n%s", err, out)
	}
	if !bytes.Equal(got, f.binary) {
		t.Fatalf("installed binary differs from the release asset")
	}
	st, _ := os.Stat(filepath.Join(dir, "tincan"))
	if st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("tincan is not executable: %v", st.Mode())
	}
	for _, want := range []string{
		"Installing tincan v9.9.9-rc1 (" + f.asset + ")",
		"tincan 9.9.9-rc1",
		"is not on your PATH",
		"https://agenttincan.com",
		"docs/quickstart.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := f.apiHits.Load(); n != 1 {
		t.Errorf("releases API hit %d times, want 1", n)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("install dir holds %d entries, want only tincan: %v", len(entries), entries)
	}
}

func TestInstallScriptVersionOverride(t *testing.T) {
	f := newFakeRelease(t, "v1.2.3", "1.2.3")
	srv := httptest.NewServer(f)
	defer srv.Close()

	// A bare number gets the v prefix the tags carry.
	dir, out, err := runInstallScript(t, srv, "TINCAN_VERSION=1.2.3")
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "tincan")); err != nil {
		t.Fatalf("tincan not installed: %v\n%s", err, out)
	}
	if n := f.apiHits.Load(); n != 0 {
		t.Errorf("releases API hit %d times with TINCAN_VERSION set, want 0", n)
	}
}

func TestInstallScriptChecksumMismatchAborts(t *testing.T) {
	f := newFakeRelease(t, "v9.9.9", "9.9.9")
	f.binary = append(f.binary, []byte("# tampered\n")...)
	srv := httptest.NewServer(f)
	defer srv.Close()

	dir, out, err := runInstallScript(t, srv)
	if err == nil {
		t.Fatalf("install.sh succeeded with a bad checksum:\n%s", out)
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("output lacks the checksum error:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "tincan")); !os.IsNotExist(err) {
		t.Errorf("tincan was installed despite the mismatch (stat err %v)", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("install dir not empty after abort: %v", entries)
	}
}

func TestInstallScriptPrivateRepo(t *testing.T) {
	f := newFakeRelease(t, "v9.9.9", "9.9.9")
	f.apiStatus = http.StatusNotFound
	srv := httptest.NewServer(f)
	defer srv.Close()

	dir, out, err := runInstallScript(t, srv)
	if err == nil {
		t.Fatalf("install.sh succeeded against a 404:\n%s", out)
	}
	if !strings.Contains(out, "downloads open when the repository is public") {
		t.Errorf("output lacks the not-public hint:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "tincan")); !os.IsNotExist(err) {
		t.Errorf("tincan was installed after a 404 (stat err %v)", err)
	}
}
