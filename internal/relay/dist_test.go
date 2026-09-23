package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// distDir holds two platform binaries, checksums.txt, VERSION, and files the
// relay must never serve.
func distDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"tincan_linux_amd64":   "linux binary",
		"tincan_darwin_arm64":  "darwin binary",
		"checksums.txt":        "abc  tincan_linux_amd64.tar.gz\n",
		"VERSION":              "0.4.0\n",
		"wake.json":            `{"secret":true}`,
		"tincan_windows_amd64": "not a supported platform",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDistServesOnlyAllowedFilesToJoinedAgents(t *testing.T) {
	h := newHarness(t, Config{})
	h.srv.SetDist(distDir(t))

	rec := h.do(museAddr, "GET", "/v1/dist/tincan_linux_amd64", "", http.StatusOK, nil)
	if rec.Body.String() != "linux binary" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	h.do(macAddr, "GET", "/v1/dist/tincan_darwin_arm64", "", http.StatusOK, nil) // admin device
	h.do(grokAddr, "GET", "/v1/dist/checksums.txt", "", http.StatusOK, nil)
	for _, name := range []string{"wake.json", "VERSION", "tincan_windows_amd64", "tincan_linux_arm64", "..%2Fwake.json"} {
		h.do(grokAddr, "GET", "/v1/dist/"+name, "", http.StatusNotFound, nil)
	}
	h.do(strangerAddr, "GET", "/v1/dist/tincan_linux_amd64", "", http.StatusForbidden, nil)
	h.do(strangerAddr, "GET", "/v1/dist", "", http.StatusForbidden, nil)
}

func TestDistManifest(t *testing.T) {
	h := newHarness(t, Config{})
	h.srv.SetDist(distDir(t))
	var m client.DistManifest
	h.do(instinctAddr, "GET", "/v1/dist", "", http.StatusOK, &m)
	if m.Version != "0.4.0" {
		t.Fatalf("version = %q", m.Version)
	}
	want := map[string]string{"tincan_darwin_arm64": sum([]byte("darwin binary")), "tincan_linux_amd64": sum([]byte("linux binary"))}
	if len(m.Files) != len(want) {
		t.Fatalf("files = %+v, want %v", m.Files, want)
	}
	for _, f := range m.Files {
		if want[f.Name] != f.SHA256 {
			t.Fatalf("file %s sha256 = %s, want %s", f.Name, f.SHA256, want[f.Name])
		}
	}
}

// Without VERSION the manifest still lists files, with an empty version.
func TestDistManifestWithoutVersion(t *testing.T) {
	h := newHarness(t, Config{})
	dir := distDir(t)
	os.Remove(filepath.Join(dir, "VERSION"))
	h.srv.SetDist(dir)
	var m client.DistManifest
	h.do(instinctAddr, "GET", "/v1/dist", "", http.StatusOK, &m)
	if m.Version != "" || len(m.Files) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestDistOffWithoutFlag(t *testing.T) {
	h := newHarness(t, Config{})
	h.do(museAddr, "GET", "/v1/dist", "", http.StatusNotFound, nil)
	h.do(museAddr, "GET", "/v1/dist/tincan_linux_amd64", "", http.StatusNotFound, nil)
}

// A binary symlinked into the dist directory (say, to a build output) is
// listed in the manifest and downloads, since both paths follow symlinks. A
// symlink to a directory is neither.
func TestDistFollowsSymlinkedBinary(t *testing.T) {
	h := newHarness(t, Config{})
	dir := distDir(t)
	build := t.TempDir()
	if err := os.WriteFile(filepath.Join(build, "tincan"), []byte("arm linux binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(build, "tincan"), filepath.Join(dir, "tincan_linux_arm64")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(build, filepath.Join(dir, "tincan_darwin_amd64")); err != nil {
		t.Fatal(err)
	}
	h.srv.SetDist(dir)
	var m client.DistManifest
	h.do(instinctAddr, "GET", "/v1/dist", "", http.StatusOK, &m)
	if got := m.SHA256Of("tincan_linux_arm64"); got != sum([]byte("arm linux binary")) {
		t.Fatalf("symlinked binary sha256 = %q, manifest = %+v", got, m)
	}
	if m.SHA256Of("tincan_darwin_amd64") != "" || len(m.Files) != 3 {
		t.Fatalf("manifest = %+v, want the two regular binaries and the symlinked one", m)
	}
	rec := h.do(museAddr, "GET", "/v1/dist/tincan_linux_arm64", "", http.StatusOK, nil)
	if rec.Body.String() != "arm linux binary" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	h.do(museAddr, "GET", "/v1/dist/tincan_darwin_amd64", "", http.StatusNotFound, nil)
}
