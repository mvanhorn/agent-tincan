package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// distBinary matches the release binaries the relay may serve for
// tincan upgrade. Nothing else in the dist directory is ever served, apart
// from checksums.txt.
var distBinary = regexp.MustCompile(`^tincan_((linux|darwin)_(amd64|arm64)|windows_(amd64|arm64)\.exe)$`)

// distChecksums is the release checksums file, served as-is.
const distChecksums = "checksums.txt"

// distDownloadTimeout bounds one binary download, well past the API write
// timeout, so a slow proxy (Muse's) can still fetch tens of megabytes.
const distDownloadTimeout = 10 * time.Minute

// dist serves release files from an operator-managed directory.
type dist struct {
	dir string

	mu   sync.Mutex
	sums map[string]distSum // cached sha256 per file, keyed by name
}

type distSum struct {
	size int64
	mod  time.Time
	sum  string
}

// SetDist serves the release files in dir at /v1/dist for tincan upgrade:
// tincan_<os>_<arch> binaries (.exe on Windows) and checksums.txt, to joined agents and
// admins only. The optional VERSION file in dir names the release. An empty
// dir turns it off.
func (s *Server) SetDist(dir string) {
	if dir == "" {
		s.dist = nil
		return
	}
	s.dist = &dist{dir: dir, sums: map[string]distSum{}}
}

// distCaller attributes a dist request like any agent call (admins too) and
// reports whether it may proceed.
func (s *Server) distCaller(w http.ResponseWriter, r *http.Request) bool {
	if !s.isAdmin(r) && s.agent(w, r) == "" {
		return false
	}
	if s.dist == nil {
		writeErr(w, http.StatusNotFound, errors.New("this relay does not serve upgrades (start it with --dist <dir>)"))
		return false
	}
	return true
}

func (s *Server) handleDistManifest(w http.ResponseWriter, r *http.Request) {
	if !s.distCaller(w, r) {
		return
	}
	d := s.dist
	m := client.DistManifest{Files: []client.DistFile{}}
	m.Version = d.release()
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, e := range entries {
		if !distBinary.MatchString(e.Name()) {
			continue
		}
		// Follow symlinks and require a regular file, as handleDistFile
		// does, so the manifest lists exactly what downloads.
		if fi, err := os.Stat(filepath.Join(d.dir, e.Name())); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		sum, err := d.sum(e.Name())
		if err != nil {
			log.Printf("dist %s: %v", e.Name(), err)
			continue
		}
		m.Files = append(m.Files, client.DistFile{Name: e.Name(), SHA256: sum})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleDistFile(w http.ResponseWriter, r *http.Request) {
	if !s.distCaller(w, r) {
		return
	}
	name := r.PathValue("name")
	if name != distChecksums && !distBinary.MatchString(name) {
		writeErr(w, http.StatusNotFound, errors.New("no such release file"))
		return
	}
	f, err := os.Open(filepath.Join(s.dist.dir, name))
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("no such release file"))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, errors.New("no such release file"))
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(distDownloadTimeout))
	w.Header().Set("Content-Type", "application/octet-stream")
	if name == distChecksums {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// sum returns the sha256 of the named file, recomputing it only when the
// file's size or modification time changed.
func (d *dist) sum(name string) (string, error) {
	path := filepath.Join(d.dir, name)
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	c, ok := d.sums[name]
	d.mu.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c.sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	c = distSum{size: fi.Size(), mod: fi.ModTime(), sum: hex.EncodeToString(h.Sum(nil))}
	d.mu.Lock()
	d.sums[name] = c
	d.mu.Unlock()
	return c.sum, nil
}

// release is the dist VERSION, read on every call: the file is a few bytes,
// and reading it each time means an in-place edit is never missed.
func (d *dist) release() string {
	raw, err := os.ReadFile(filepath.Join(d.dir, "VERSION"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// installable reports whether the dist holds the release binary for
// platform (os_arch), so a notice never sends an agent to a failing
// tincan upgrade.
func (d *dist) installable(platform string) bool {
	name := "tincan_" + platform
	if strings.HasPrefix(platform, "windows_") {
		name += ".exe"
	}
	if !distBinary.MatchString(name) {
		return false
	}
	fi, err := os.Stat(filepath.Join(d.dir, name))
	return err == nil && fi.Mode().IsRegular()
}

// upgradeFor returns the newer release the caller should install, or "".
// It needs the caller's build and platform headers, and the platform's
// binary in the dist; clients that send no platform get no notice.
func (s *Server) upgradeFor(r *http.Request) string {
	if _, guest := guestOf(r.Context()); guest {
		return "" // a guest's machine is not the owner's to upgrade from here
	}
	if s.dist != nil {
		available := s.dist.release()
		if client.Newer(available, r.Header.Get(client.VersionHeader)) && s.dist.installable(r.Header.Get(client.PlatformHeader)) {
			return strings.TrimPrefix(available, "v")
		}
	}
	return ""
}
