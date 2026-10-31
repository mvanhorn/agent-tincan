package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// GitHubReleaseURL is the value to pass to tincan relay --release-url for
// this project's GitHub releases. The relay makes no outbound download
// unless its operator sets --release-url; callers can never choose it.
const GitHubReleaseURL = "https://github.com/mvanhorn/agent-tincan/releases/download"

// exitRestart is the relay's exit status after an upgrade with
// --upgrade-exit: EX_TEMPFAIL, nonzero so systemd's Restart=on-failure
// starts the new build as well as a keep-alive loop does.
const exitRestart = 75

// releaseTag is what --from-github accepts: a release version with an
// optional leading v and an optional prerelease suffix.
var releaseTag = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// backupSafe keeps a version usable in the backup file's name.
var backupSafe = regexp.MustCompile(`[^A-Za-z0-9._+-]`)

// maxChecksumsBytes caps a downloaded checksums.txt.
const maxChecksumsBytes = 1 << 20

func relayUpgradeCmd() *cobra.Command {
	var relayURL, socket, fromGitHub string
	var force bool
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "relay-upgrade",
		Short: "Upgrade the relay to the release in its --dist directory (admin devices only)",
		Long: `Ask the relay to install the tincan release in its --dist directory over its
own binary, then restart. Run it from an admin device (or on the relay host,
where the local admin socket is used); agents are refused.

The relay picks tincan_<os>_<arch> for its own platform, checks its sha256
against the dist checksums.txt, writes it next to the running binary, keeps
the old one as <binary>.<old version>, and renames the new one into place. It
replies, then restarts: it re-executes itself by default, or, when started
with --upgrade-exit, exits with status 75 for its supervisor (systemd or a
keep-alive loop) to start the new build.

With --from-github vX.Y.Z the relay first downloads that release's binaries
and checksums.txt into its dist directory, checking each binary against the
release's checksums.txt, so no one needs a shell on the relay host. The relay
must have been started with --release-url (for this project,
https://github.com/mvanhorn/agent-tincan/releases/download); it never downloads otherwise.

A release that is not newer than the relay's build is refused unless --force.
The relay user must be able to replace its own binary.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fromGitHub != "" && !releaseTag.MatchString(fromGitHub) {
				return fmt.Errorf("--from-github wants a release tag such as v0.8.0, got %q", fromGitHub)
			}
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			return relayUpgradeRun(cmd.Context(), r, client.RelayUpgradeRequest{Force: force, FromGitHub: fromGitHub}, wait, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config or TINCAN_RELAY)")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().BoolVar(&force, "force", false, "install the release even when it is not newer than the relay's build")
	cmd.Flags().StringVar(&fromGitHub, "from-github", "", "have the relay download this release tag (v0.8.0) from GitHub into its dist directory first")
	cmd.Flags().DurationVar(&wait, "wait", time.Minute, "how long to wait for the restarted relay to answer with the new build (0 to not wait)")
	return cmd
}

// relayUpgradePoll is how often relay-upgrade asks whether the relay is back.
var relayUpgradePoll = 2 * time.Second

// relayUpgradeRun asks the relay to upgrade and, with wait, watches for it to
// come back on the new build.
func relayUpgradeRun(ctx context.Context, r *client.Relay, in client.RelayUpgradeRequest, wait time.Duration, out io.Writer) error {
	res, err := r.RelayUpgrade(ctx, in)
	if err != nil {
		if apiErr, ok := errors.AsType[*client.APIError](err); ok && apiErr.Code == http.StatusNotFound && strings.Contains(apiErr.Message, "page not found") {
			return fmt.Errorf("%w. The relay predates tincan relay-upgrade: upgrade it by hand once, and later releases can use this command", err)
		}
		if client.IsStatus(err, http.StatusConflict) && strings.Contains(err.Error(), "not newer") {
			return fmt.Errorf("%w. Pass --force to reinstall it anyway", err)
		}
		if _, ok := errors.AsType[*client.APIError](err); !ok {
			return fmt.Errorf("%w. The relay may still have finished the upgrade: check its build with tincan agents (the first line) before trying again", err)
		}
		return err
	}
	how := "re-executing itself"
	if res.Restart == "exit" {
		how = "exiting for its supervisor to start it again"
	}
	fmt.Fprintf(out, "The relay installed tincan %s (was %s) and is restarting, %s.\n", res.To, res.From, how)
	if wait <= 0 {
		fmt.Fprintln(out, "Check it with tincan agents: the first line names the relay's build.")
		return nil
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(relayUpgradePoll):
		}
		ro, err := r.Roster(ctx)
		if err == nil && strings.TrimPrefix(ro.RelayVersion, "v") == strings.TrimPrefix(res.To, "v") {
			fmt.Fprintf(out, "The relay is back on tincan %s.\n", res.To)
			return nil
		}
	}
	return fmt.Errorf("the relay did not answer on tincan %s within %s; check it with tincan agents (the first line names its build) or look at the relay host's log", res.To, wait)
}

// relayUpgrader is the running relay's relay.SelfUpgrader: it installs a
// release from the dist directory (downloading it first on request) over
// the relay's own binary and, once the reply is out, stops the relay so
// its caller can re-exec or exit.
type relayUpgrader struct {
	exe        string // resolved path of the running binary
	current    string // the running build
	platform   string // tincan_<goos>_<goarch>
	releaseURL string // base for --from-github downloads; "" turns them off
	exitMode   bool   // --upgrade-exit: exit for a supervisor instead of re-exec
	http       *http.Client
	exec       func(argv0 string, argv, envv []string) error
	stop       context.CancelFunc // stops the relay (the root shutdown)

	mu        sync.Mutex
	installed string // the version installed, awaiting restart
}

// errRelayRestart is what runRelay returns after an upgrade, once the
// servers are drained and the store is closed.
type errRelayRestart struct{ u *relayUpgrader }

func (e *errRelayRestart) Error() string { return "relay upgraded to " + e.u.installed + "; restart" }

// withRelayUpgrader lets admins upgrade a relay that serves a dist
// directory. The returned context ends when an upgrade asks for a restart;
// the upgrader is nil when self-upgrade is off.
func withRelayUpgrader(ctx context.Context, srv *relay.Server, f relayFlags) (context.Context, *relayUpgrader) {
	if f.dist == "" {
		return ctx, nil
	}
	upCtx, stop := context.WithCancel(ctx)
	u, err := newRelayUpgrader(f, stop)
	if err != nil {
		stop()
		log.Printf("relay self-upgrade is off: %v", err)
		return ctx, nil
	}
	srv.SetSelfUpgrader(u)
	return upCtx, u
}

// newRelayUpgrader resolves the running binary for f. stop is the relay's
// root shutdown.
func newRelayUpgrader(f relayFlags, stop context.CancelFunc) (*relayUpgrader, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find the running tincan binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, fmt.Errorf("resolve the running tincan binary: %w", err)
	}
	return &relayUpgrader{
		exe: exe, current: Version, platform: "tincan_" + runtime.GOOS + "_" + runtime.GOARCH,
		releaseURL: strings.TrimRight(f.releaseURL, "/"), exitMode: f.upgradeExit,
		http: &http.Client{Timeout: client.DistDownloadTimeout}, exec: syscall.Exec, stop: stop,
	}, nil
}

// Restart stops the relay; runRelay then drains, closes the store, and
// returns an errRelayRestart for its caller to act on.
func (u *relayUpgrader) Restart() { u.stop() }

// restartErr is runRelay's result once it has shut down: errRelayRestart
// after an upgrade, else nil.
func (u *relayUpgrader) restartErr() error {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.installed == "" {
		return nil
	}
	return &errRelayRestart{u: u}
}

// restart runs the upgraded build: it replaces this process with the new
// binary, same arguments and environment, or with --upgrade-exit exits 75
// for the supervisor.
func (u *relayUpgrader) restart() error {
	if u.exitMode {
		return &ExitError{Code: exitRestart, Err: fmt.Errorf("relay upgraded to %s; exiting with status %d so its supervisor starts the new build", u.installed, exitRestart)}
	}
	log.Printf("re-executing %s (tincan %s)", u.exe, u.installed)
	if err := u.exec(u.exe, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("relay upgraded to %s, but re-executing %s failed: %w; start the relay again", u.installed, u.exe, err)
	}
	return nil
}

// Upgrade installs the release over the running binary. On any error the
// binary is unchanged.
func (u *relayUpgrader) Upgrade(ctx context.Context, dist string, in client.RelayUpgradeRequest) (client.RelayUpgradeResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var res client.RelayUpgradeResult
	if u.installed != "" {
		return res, fmt.Errorf("%w (tincan %s)", relay.ErrUpgradePending, u.installed)
	}
	if err := writable(u.exe); err != nil {
		return res, err
	}
	if in.FromGitHub != "" {
		if u.releaseURL == "" {
			return res, fmt.Errorf("%w: this relay does not download releases; its operator must start it with --release-url %s (or upgrade from what is already in --dist)", relay.ErrUpgradeUnavailable, GitHubReleaseURL)
		}
		if !releaseTag.MatchString(in.FromGitHub) {
			return res, fmt.Errorf("%w: %q is not a release tag", relay.ErrUpgradeUnavailable, in.FromGitHub)
		}
		if err := u.newer(in.FromGitHub, in.Force); err != nil {
			return res, err
		}
		if err := u.fetchRelease(ctx, dist, in.FromGitHub); err != nil {
			return res, err
		}
	}
	raw, err := os.ReadFile(filepath.Join(dist, "VERSION"))
	version := strings.TrimSpace(string(raw))
	if err != nil || !releaseTag.MatchString(version) {
		return res, fmt.Errorf("%w: the dist VERSION file is missing or does not name a release", relay.ErrUpgradeUnavailable)
	}
	if err := u.newer(version, in.Force); err != nil {
		return res, err
	}
	sums, err := readChecksums(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		return res, fmt.Errorf("%w: %v", relay.ErrUpgradeUnavailable, err)
	}
	want := sums[u.platform]
	if want == "" {
		return res, fmt.Errorf("%w: the dist checksums.txt has no %s", relay.ErrUpgradeUnavailable, u.platform)
	}
	if err := u.install(filepath.Join(dist, u.platform), want, version); err != nil {
		return res, err
	}
	u.installed = strings.TrimPrefix(version, "v")
	restart := "re-exec"
	if u.exitMode {
		restart = "exit"
	}
	return client.RelayUpgradeResult{From: strings.TrimPrefix(u.current, "v"), To: u.installed, Restart: restart}, nil
}

// newer refuses a version that is not newer than the running build, unless
// forced.
func (u *relayUpgrader) newer(version string, force bool) error {
	if force || client.Ahead(version, u.current) {
		return nil
	}
	return fmt.Errorf("%w: the release is %s and the relay runs %s", relay.ErrUpgradeNotNewer, strings.TrimPrefix(version, "v"), strings.TrimPrefix(u.current, "v"))
}

// install copies src next to the running binary, checks it against want,
// keeps the old binary as <exe>.<old version>, and renames the copy into
// place.
func (u *relayUpgrader) install(src, want, version string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("%w: the dist has no %s", relay.ErrUpgradeUnavailable, u.platform)
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: the dist has no %s", relay.ErrUpgradeUnavailable, u.platform)
	}
	mode := os.FileMode(0o755)
	if fi, err := os.Stat(u.exe); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.exe), ".tincan-relay-upgrade-*")
	if err != nil {
		return fmt.Errorf("%w: write next to %s: %v", relay.ErrUpgradeNotWritable, u.exe, err)
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%w for %s: sha256 %s, checksums.txt says %s; %s was not changed", relay.ErrUpgradeChecksum, u.platform, got, want, u.exe)
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	backup := u.exe + "." + backupSafe.ReplaceAllString(strings.TrimPrefix(u.current, "v"), "_")
	if err := keepBackup(u.exe, backup); err != nil {
		return fmt.Errorf("keep the old binary as %s: %w; %s was not changed", backup, err, u.exe)
	}
	// Rename gives the path a new inode: the running process keeps its
	// file, and macOS does not kill a signed binary rewritten in place.
	if err := os.Rename(tmp.Name(), u.exe); err != nil {
		os.Remove(backup)
		return fmt.Errorf("replace %s: %w", u.exe, err)
	}
	done = true
	log.Printf("installed tincan %s at %s; the old build is %s", strings.TrimPrefix(version, "v"), u.exe, backup)
	return nil
}

// keepBackup makes backup a copy of exe, replacing an older backup of the
// same name: a hard link when the filesystem allows it, else a copy.
func keepBackup(exe, backup string) error {
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if os.Link(exe, backup) == nil {
		return nil
	}
	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(backup)
		return err
	}
	return dst.Close()
}

// readChecksums parses a sha256sum-style checksums.txt: "<hex>  <name>" per
// line, the name optionally prefixed with "*".
func readChecksums(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the dist has no checksums.txt")
	}
	return parseChecksums(raw), nil
}

func parseChecksums(raw []byte) map[string]string {
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			continue
		}
		sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return sums
}

// fetchRelease downloads tag's checksums.txt and every release binary it
// lists into dist, each checked against it. Files land in a temporary
// directory inside dist first and are renamed into place only when all of
// them match, binaries and checksums.txt first and VERSION last, so the
// relay never announces a release whose files are not all there.
func (u *relayUpgrader) fetchRelease(ctx context.Context, dist, tag string) error {
	tag = "v" + strings.TrimPrefix(tag, "v")
	base := u.releaseURL + "/" + url.PathEscape(tag) + "/"
	var sumsRaw bytes.Buffer
	if err := u.download(ctx, base+"checksums.txt", &sumsRaw, maxChecksumsBytes); err != nil {
		return err
	}
	sums := parseChecksums(sumsRaw.Bytes())
	var names []string
	for name := range sums {
		if relay.IsDistBinary(name) {
			names = append(names, name)
		}
	}
	if sums[u.platform] == "" {
		return fmt.Errorf("%w: release %s has no %s in its checksums.txt", relay.ErrUpgradeUnavailable, tag, u.platform)
	}
	tmpDir, err := os.MkdirTemp(dist, ".tincan-release-*")
	if err != nil {
		return fmt.Errorf("%w: write to the dist directory %s: %v", relay.ErrUpgradeNotWritable, dist, err)
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(tmpDir)
		}
	}()
	for _, name := range names {
		f, err := os.OpenFile(filepath.Join(tmpDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
		if err != nil {
			return err
		}
		h := sha256.New()
		err = u.download(ctx, base+name, io.MultiWriter(f, h), client.MaxDistBytes)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != sums[name] {
			return fmt.Errorf("%w for %s in release %s: sha256 %s, its checksums.txt says %s; the dist was not changed", relay.ErrUpgradeChecksum, name, tag, got, sums[name])
		}
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "checksums.txt"), sumsRaw.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "VERSION"), []byte(strings.TrimPrefix(tag, "v")+"\n"), 0o644); err != nil {
		return err
	}
	intact, err := publishDist(dist, tmpDir, append(names, "checksums.txt", "VERSION"))
	if err != nil {
		keep = !intact
		return err
	}
	log.Printf("downloaded tincan release %s into %s", tag, dist)
	return nil
}

// publishDist moves the staged files in tmpDir into dist in order (VERSION
// last). Each rename replaces its file atomically, so a download sees the
// whole old or the whole new file and never a missing one. The old files
// are kept (hard-linked, or copied) in tmpDir first; if a move fails they
// are renamed back. intact is false only when that restore also failed:
// the old files are then still in tmpDir, and the caller must keep it.
func publishDist(dist, tmpDir string, names []string) (intact bool, err error) {
	old := filepath.Join(tmpDir, ".old")
	if err := os.Mkdir(old, 0o700); err != nil {
		return true, err
	}
	type moved struct {
		name    string
		hadPrev bool
	}
	var done []moved
	restore := func(cause error) (bool, error) {
		var failed []string
		for _, m := range slices.Backward(done) {
			dst := filepath.Join(dist, m.name)
			var err error
			if m.hadPrev {
				err = os.Rename(filepath.Join(old, m.name), dst)
			} else if err = os.Remove(dst); errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			if err != nil {
				failed = append(failed, m.name)
			}
		}
		if len(failed) > 0 {
			return false, fmt.Errorf("%w: %v; restoring %s in the dist directory also failed, the old files are in %s", relay.ErrUpgradeNotWritable, cause, strings.Join(failed, ", "), old)
		}
		return true, fmt.Errorf("%w: %v; the dist was not changed", relay.ErrUpgradeNotWritable, cause)
	}
	for _, name := range names {
		dst := filepath.Join(dist, name)
		hadPrev := false
		if _, err := os.Lstat(dst); err == nil {
			if err := keepBackup(dst, filepath.Join(old, name)); err != nil {
				return restore(fmt.Errorf("keep the old %s: %w", name, err))
			}
			hadPrev = true
		}
		if err := publishRename(filepath.Join(tmpDir, name), dst); err != nil {
			return restore(fmt.Errorf("move %s into the dist directory: %w", name, err))
		}
		done = append(done, moved{name, hadPrev})
	}
	return true, nil
}

// publishRename moves one staged file into the dist. A var so tests can make
// a move fail.
var publishRename = os.Rename

// download streams url into w, at most limit bytes.
func (u *relayUpgrader) download(ctx context.Context, rawURL string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", relay.ErrUpgradeFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s: not found (is the release tag right?)", relay.ErrUpgradeUnavailable, rawURL)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s: HTTP %d", relay.ErrUpgradeFetch, rawURL, resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("%w: %s: %v", relay.ErrUpgradeFetch, rawURL, err)
	}
	if n > limit {
		return fmt.Errorf("%w: %s is larger than %d bytes", relay.ErrUpgradeFetch, rawURL, limit)
	}
	return nil
}
