package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Hello is what the extension sends when it connects to the native host:
// its manifest version, whether it is unpacked (only an unpacked extension
// re-reads files from disk on reload), and the sha256 of each of its files
// as Chrome loaded them.
type Hello struct {
	Version  string            `json:"version"`
	Unpacked bool              `json:"unpacked"`
	Files    map[string]string `json:"files"`
}

// ExtensionDirEnv names the unpacked extension directory for the native
// host; the wrapper `tincan history install --extension-dir` writes sets
// it.
const ExtensionDirEnv = "TINCAN_EXTENSION_DIR"

// reloadCooldown is how long the host waits before asking again for a
// reload toward the same files on disk, so a reload that does not help
// (the loaded copy lives somewhere else) never loops.
const reloadCooldown = 10 * time.Minute

// extFileName is the shape of a file name a hello may report: a plain
// top-level name, so a report can never make the host read outside the
// extension directory.
var extFileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.(js|json)$`)

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, 16<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ExtensionDrift compares what the loaded extension reported with the
// files in dir. reason is empty when they match. fingerprint identifies
// the files on disk, for the reload loop guard.
func ExtensionDrift(dir string, h Hello) (reason, fingerprint string, err error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "", "", fmt.Errorf("extension dir: %w", err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Version == "" {
		return "", "", errors.New("extension dir: manifest.json has no version")
	}
	names := []string{"manifest.json"}
	for n := range h.Files {
		if extFileName.MatchString(n) && n != "manifest.json" {
			names = append(names, n)
		}
	}
	sort.Strings(names[1:])
	fp := sha256.New()
	fmt.Fprintf(fp, "version %s\n", m.Version)
	if m.Version != h.Version {
		reason = fmt.Sprintf("version %s is loaded, %s is on disk", h.Version, m.Version)
	}
	for _, n := range names {
		sum, err := fileSHA256(filepath.Join(dir, n))
		if err != nil {
			sum = "missing"
		}
		fmt.Fprintf(fp, "%s %s\n", n, sum)
		if reason == "" && sum != h.Files[n] {
			reason = n + " changed on disk"
		}
	}
	return reason, hex.EncodeToString(fp.Sum(nil)), nil
}

type reloadState struct {
	Fingerprint string    `json:"fingerprint"`
	At          time.Time `json:"at"`
}

func (h *NativeHost) logf(format string, args ...any) {
	if h.Log != nil {
		_, _ = fmt.Fprintf(h.Log, "tincan native-host: "+format+"\n", args...)
	}
}

// onHello asks the extension to reload when the unpacked files on disk
// differ from the ones it loaded, at most once per cooldown for the same
// files.
func (h *NativeHost) onHello(hello Hello) {
	if h.ExtensionDir == "" {
		return
	}
	if !hello.Unpacked {
		h.logf("extension %s is not unpacked; it updates through its store", hello.Version)
		return
	}
	reason, fp, err := ExtensionDrift(h.ExtensionDir, hello)
	if err != nil {
		h.logf("cannot compare with %s: %v", h.ExtensionDir, err)
		return
	}
	if reason == "" {
		return
	}
	statePath := h.ReloadStatePath
	if statePath == "" {
		statePath = filepath.Join(filepath.Dir(h.SocketPath), "reload-state.json")
	}
	var st reloadState
	prev, readErr := os.ReadFile(statePath)
	if readErr == nil {
		_ = json.Unmarshal(prev, &st)
	}
	if st.Fingerprint == fp && time.Since(st.At) < reloadCooldown {
		h.logf("%s, but a reload toward these files was already asked for at %s; not asking again", reason, st.At.Format(time.RFC3339))
		return
	}
	// The cooldown is recorded before asking, so a reload that cannot be
	// recorded is never asked for (no loop), and rolled back when the ask
	// cannot be written, so the next hello asks again.
	if b, err := json.Marshal(reloadState{Fingerprint: fp, At: time.Now().UTC()}); err == nil {
		if err := writeFileAtomic(statePath, b, 0o600); err != nil {
			h.logf("cannot record the reload (%v); not asking, to avoid a loop", err)
			return
		}
	}
	h.mu.Lock()
	h.seq++
	id := h.seq
	h.mu.Unlock()
	h.outMu.Lock()
	err = WriteMessage(h.Out, NativeRequest{ID: id, Op: OpExtensionReload, Args: OpArgs{}}, MaxHostMessage)
	h.outMu.Unlock()
	if err != nil {
		h.logf("reload request failed: %v", err)
		var rerr error
		if readErr == nil {
			rerr = writeFileAtomic(statePath, prev, 0o600)
		} else {
			rerr = os.Remove(statePath)
		}
		if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			h.logf("cannot roll back the reload record: %v", rerr)
		}
		return
	}
	h.logf("%s: asked the extension to reload", reason)
}
