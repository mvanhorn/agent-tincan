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
var distBinary = regexp.MustCompile(`^tincan_(linux|darwin)_(amd64|arm64)$`)

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
// tincan_<os>_<arch> binaries and checksums.txt, to joined agents and
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
	if raw, err := os.ReadFile(filepath.Join(d.dir, "VERSION")); err == nil {
		m.Version = strings.TrimSpace(string(raw))
	}
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
