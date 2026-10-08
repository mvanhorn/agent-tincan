package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/macapp"
	"github.com/mvanhorn/agent-tincan/internal/notes"
)

const plutilBin = "/usr/bin/plutil"

func servicesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "services",
		Short: "Manage tincan's macOS background services",
	}
	var revert, restart bool
	refresh := &cobra.Command{
		Use:   "refresh",
		Short: "Make tincan's LaunchAgents show as Agent Tincan in Login Items",
		Long: `Install Agent Tincan.app into ~/Applications and make every tincan
LaunchAgent in ~/Library/LaunchAgents start through it, so macOS lists them in
System Settings > Login Items as "Agent Tincan" instead of under the signing
certificate's name. tincan upgrade runs this after replacing the binary.

Each plist is edited in place: the launcher goes first in ProgramArguments,
ahead of this tincan binary, and AssociatedBundleIdentifiers names the app.
Every other key, argument and log path is kept. Plists whose program is not
this tincan binary are skipped, and so is the notes service, whose Files and
Folders grant must not be shared with other services. Loaded services are
restarted to pick up the change, except the one this command runs under;
--restart restarts the others too, so they run this binary.

--revert undoes it: services run the tincan binary directly again.

If Login Items still shows the old name after a refresh, macOS kept a stale
record. The last resort is sfltool resetbtm and a restart, which resets the
Login Items approvals of every app, not just tincan.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "darwin" {
				fmt.Fprintf(cmd.OutOrStdout(), "Nothing to refresh on %s: only macOS lists services in Login Items.\n", runtime.GOOS)
				return nil
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			r := &serviceRefresher{
				home:           home,
				exe:            exe,
				revert:         revert,
				restartCurrent: restart,
				ensure:         func() (string, error) { return macapp.Ensure(macapp.Options{Home: home}) },
				verify:         macapp.VerifyTincan,
				launchd:        launchctl{uid: os.Getuid()},
				self:           os.Getenv("XPC_SERVICE_NAME"),
				sleep:          func() { time.Sleep(time.Second) },
				out:            cmd.OutOrStdout(),
			}
			return r.run()
		},
	}
	refresh.Flags().BoolVar(&revert, "revert", false, "make services run the tincan binary directly again")
	refresh.Flags().BoolVar(&restart, "restart", false, "also restart loaded services whose plist is already current, so they run this binary (tincan upgrade passes it)")
	cmd.AddCommand(refresh)
	return cmd
}

// launchdOps is the part of launchctl refresh uses.
type launchdOps interface {
	Loaded(label string) bool
	Bootout(label string) error
	Bootstrap(path string) error
}

type launchctl struct{ uid int }

func (l launchctl) target(label string) string { return fmt.Sprintf("gui/%d/%s", l.uid, label) }

func (l launchctl) Loaded(label string) bool {
	return exec.Command("/bin/launchctl", "print", l.target(label)).Run() == nil
}

// Program is the program launchd runs for a loaded job, "" if not loaded.
func (l launchctl) Program(label string) string {
	out, err := exec.Command("/bin/launchctl", "print", l.target(label)).Output()
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(out)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "program = "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (l launchctl) Bootout(label string) error {
	if out, err := exec.Command("/bin/launchctl", "bootout", l.target(label)).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (l launchctl) Bootstrap(path string) error {
	if out, err := exec.Command("/bin/launchctl", "bootstrap", fmt.Sprintf("gui/%d", l.uid), path).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// serviceRefresher moves tincan's LaunchAgents onto (or, with revert, off)
// the Agent Tincan.app launcher.
type serviceRefresher struct {
	home   string
	exe    string // the running tincan binary, symlinks resolved
	revert bool
	// restartCurrent also restarts loaded jobs whose plist is already
	// current; tincan upgrade sets it so services move to the new build.
	restartCurrent bool
	ensure         func() (string, error)
	verify         func(exe string) error // the launcher's signature check
	launchd        launchdOps
	self           string // the launchd job this process runs under, if any
	sleep          func()
	out            io.Writer
}

func (r *serviceRefresher) run() error {
	launcher := ""
	if !r.revert {
		l, err := r.ensure()
		if errors.Is(err, macapp.ErrUnavailable) {
			fmt.Fprintln(r.out, "This tincan build does not include Agent Tincan.app (a development build), so its services keep showing the signer's name in Login Items.")
			if wrapped := r.wrappedPlists(); len(wrapped) > 0 {
				fmt.Fprintf(r.out, "%s start through Agent Tincan.app, whose launcher only runs a release-signed tincan; run tincan services refresh --revert so they run this build.\n", strings.Join(wrapped, ", "))
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("install Agent Tincan.app: %w", err)
		}
		if err := r.verify(r.exe); err != nil {
			fmt.Fprintf(r.out, "%s is not signed by Agent Tincan (%v), and the launcher only runs a release-signed tincan, so its services are left as they are.\n", r.exe, err)
			if wrapped := r.wrappedPlists(); len(wrapped) > 0 {
				fmt.Fprintf(r.out, "%s start through Agent Tincan.app and cannot run this build; run tincan services refresh --revert.\n", strings.Join(wrapped, ", "))
			}
			return nil
		}
		launcher = l
	}
	var failed []string
	for _, p := range launchAgentPlists(r.home) {
		label := plistLabel(p)
		msg, err := r.refreshOne(p, label, launcher)
		if err != nil {
			failed = append(failed, label)
			msg = err.Error()
		}
		fmt.Fprintf(r.out, "%s: %s\n", label, msg)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not refresh %s", strings.Join(failed, ", "))
	}
	return nil
}

// wrappedPlists names the plists that start this tincan through the launcher.
func (r *serviceRefresher) wrappedPlists() []string {
	var out []string
	for _, p := range launchAgentPlists(r.home) {
		if m, err := readPlistJSON(p); err == nil {
			if j, ok := parseJob(m); ok && j.launcher != "" && j.runs(r.exe) {
				out = append(out, plistLabel(p))
			}
		}
	}
	return out
}

// refreshOne brings one plist to the wanted form and returns a short status.
// Skipped plists are not errors.
func (r *serviceRefresher) refreshOne(path, label, launcher string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "skipped (not a regular file)", nil
	}
	m, err := readPlistJSON(path)
	if err != nil {
		return "", fmt.Errorf("read failed: %w", err)
	}
	if l, _ := m["Label"].(string); l != label {
		return fmt.Sprintf("skipped (its Label %q does not match the file name)", l), nil
	}
	if _, ok := m["BundleProgram"]; ok {
		return "skipped (uses BundleProgram)", nil
	}
	j, ok := parseJob(m)
	if !ok {
		return "skipped (no program)", nil
	}
	if !j.runs(r.exe) {
		return fmt.Sprintf("skipped (runs %s, not this tincan %s)", j.target, r.exe), nil
	}
	if label == notes.ServiceLabel && !r.revert {
		return r.restartUnchanged(label, path, "skipped (the notes service keeps running tincan directly so its Files and Folders grant stays its own)")
	}
	if r.revert && j.launcher == "" {
		return "already current", nil // refresh never wrapped it
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if r.revert {
		if b, ok := r.unchangedOriginal(label, m, j.launcher); ok {
			return r.apply(path, label, fi.Mode().Perm(), orig, "restored", func() error {
				if err := writeFileReplacing(path, b, fi.Mode().Perm()); err != nil {
					return err
				}
				return os.Remove(r.originalPath(label))
			})
		}
	}
	ids, _ := stringList(m["AssociatedBundleIdentifiers"])
	wantArgs := append([]string{j.target}, j.args...)
	wantIDs := slices.DeleteFunc(slices.Clone(ids), func(s string) bool { return s == macapp.BundleID })
	if !r.revert {
		wantArgs = append([]string{launcher}, wantArgs...)
		wantIDs = append(wantIDs, macapp.BundleID)
	}
	curArgs, _ := stringList(m["ProgramArguments"])
	if !j.hasProgram && slices.Equal(curArgs, wantArgs) && slices.Equal(ids, wantIDs) {
		return r.restartUnchanged(label, path, "already current")
	}
	if !r.revert && j.launcher == "" {
		// Keep the hand-written original so --revert can put it back exactly.
		if err := writeOriginal(r.originalPath(label), orig); err != nil {
			return "", fmt.Errorf("save the original plist: %w", err)
		}
	}
	return r.apply(path, label, fi.Mode().Perm(), orig, "updated", func() error {
		if err := writePlistEdit(path, orig, fi.Mode().Perm(), wantArgs, j.hasProgram, wantIDs); err != nil {
			return fmt.Errorf("update failed: %w", err)
		}
		if r.revert {
			os.Remove(r.originalPath(label))
		}
		return nil
	})
}

// apply writes a plist change and restarts the job when it is loaded,
// putting orig back if the restart keeps failing.
func (r *serviceRefresher) apply(path, label string, mode os.FileMode, orig []byte, status string, write func() error) (string, error) {
	if err := write(); err != nil {
		return "", err
	}
	if !r.launchd.Loaded(label) {
		return status, nil
	}
	if label == r.self {
		return status + "; takes effect at next restart (refresh is running under this service)", nil
	}
	if err := r.restart(label, path); err != nil {
		// Put the original back so the service runs as before.
		werr := writeFileReplacing(path, orig, mode)
		var berr error
		if werr == nil && !r.launchd.Loaded(label) {
			berr = r.launchd.Bootstrap(path)
		}
		return "", fmt.Errorf("restart failed (%v); restored the original plist%s", err, restoreNote(werr, berr))
	}
	return status + ", restarted", nil
}

// originalPath is where refresh keeps a plist as it was before wrapping.
func (r *serviceRefresher) originalPath(label string) string {
	return filepath.Join(r.home, "Library", "Application Support", "tincan", "launchagent-originals", label+".plist")
}

func writeOriginal(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileReplacing(path, data, 0o600)
}

// unchangedOriginal returns the saved original of label when the current
// plist m is exactly what refresh made from it, so restoring it loses
// nothing; a plist edited since keeps its edits and is only unwrapped.
func (r *serviceRefresher) unchangedOriginal(label string, m map[string]any, launcher string) ([]byte, bool) {
	b, err := os.ReadFile(r.originalPath(label))
	if err != nil {
		return nil, false
	}
	om, err := readPlistJSON(r.originalPath(label))
	if err != nil {
		return nil, false
	}
	j, ok := parseJob(om)
	if !ok {
		return nil, false
	}
	want := maps.Clone(om)
	delete(want, "Program")
	args := []any{launcher, j.target}
	for _, a := range j.args {
		args = append(args, a)
	}
	want["ProgramArguments"] = args
	ids, _ := stringList(om["AssociatedBundleIdentifiers"])
	var wantIDs []any
	for _, id := range ids {
		if id != macapp.BundleID {
			wantIDs = append(wantIDs, id)
		}
	}
	want["AssociatedBundleIdentifiers"] = append(wantIDs, macapp.BundleID)
	return b, reflect.DeepEqual(want, m)
}

// restartUnchanged restarts a loaded job whose plist needs no change when
// restartCurrent asks for it (after an upgrade the job still runs the old
// build), except the job refresh runs under.
func (r *serviceRefresher) restartUnchanged(label, path, status string) (string, error) {
	if !r.restartCurrent || label == r.self || !r.launchd.Loaded(label) {
		return status, nil
	}
	if err := r.restart(label, path); err != nil {
		return "", fmt.Errorf("%s, but the restart failed: %v", status, err)
	}
	return status + ", restarted", nil
}

func restoreNote(werr, berr error) string {
	switch {
	case werr != nil:
		return fmt.Sprintf(", but writing it back failed: %v", werr)
	case berr != nil:
		return fmt.Sprintf(", but loading it failed: %v", berr)
	}
	return " and reloaded it"
}

// restart boots the job out, waits for launchd to let it go, and bootstraps
// it again, retrying a few times: a bootstrap right after bootout can fail
// while the old instance is still being torn down.
func (r *serviceRefresher) restart(label, path string) error {
	if err := r.launchd.Bootout(label); err != nil {
		return err
	}
	for i := 0; i < 10 && r.launchd.Loaded(label); i++ {
		r.sleep()
	}
	var err error
	for range 3 {
		if err = r.launchd.Bootstrap(path); err == nil {
			return nil
		}
		r.sleep()
	}
	return err
}

// launchJob is what a LaunchAgent plist runs: its program, and when that
// program is the Agent Tincan.app launcher, the binary the launcher runs.
type launchJob struct {
	target     string   // the binary that ends up running
	args       []string // its arguments
	launcher   string   // the launcher in front of it, if any
	hasProgram bool     // the plist names its program with the Program key
}

func parseJob(m map[string]any) (launchJob, bool) {
	args, ok := stringList(m["ProgramArguments"])
	prog, hasProgram := m["Program"].(string)
	if !hasProgram {
		if !ok || len(args) == 0 {
			return launchJob{}, false
		}
		prog, args = args[0], args[1:]
	} else if len(args) > 0 {
		args = args[1:] // with Program, ProgramArguments[0] is only argv[0]
	}
	j := launchJob{target: prog, args: args, hasProgram: hasProgram}
	if isLauncher(prog) && len(args) > 0 {
		j.launcher, j.target, j.args = prog, args[0], args[1:]
	}
	return j, true
}

// runs reports whether the job's binary is exe (symlinks resolved).
func (j launchJob) runs(exe string) bool {
	resolved, err := filepath.EvalSymlinks(j.target)
	return err == nil && resolved == exe
}

// isLauncher reports whether p is an Agent Tincan.app launcher.
func isLauncher(p string) bool {
	return filepath.Base(p) == filepath.Base(macapp.LauncherPath) &&
		strings.HasSuffix(filepath.Dir(p), macapp.AppName+"/Contents/MacOS")
}

func readPlistJSON(path string) (map[string]any, error) {
	out, err := exec.Command(plutilBin, "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func stringList(v any) ([]string, bool) {
	xs, ok := v.([]any)
	if !ok {
		return nil, false
	}
	var s []string
	for _, x := range xs {
		str, ok := x.(string)
		if !ok {
			return nil, false
		}
		s = append(s, str)
	}
	return s, true
}

// writePlistEdit edits a copy of path next to it with plutil, checks it, and
// renames it over path, so launchd never reads a half-written plist.
func writePlistEdit(path string, orig []byte, mode os.FileMode, args []string, dropProgram bool, ids []string) error {
	tmp, err := stageTemp(path, orig, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	argsJSON, _ := json.Marshal(args)
	edits := [][]string{{"-replace", "ProgramArguments", "-json", string(argsJSON)}}
	if dropProgram {
		edits = append(edits, []string{"-remove", "Program"})
	}
	if len(ids) == 0 {
		edits = append(edits, []string{"-remove", "AssociatedBundleIdentifiers"})
	} else {
		idsJSON, _ := json.Marshal(ids)
		edits = append(edits, []string{"-replace", "AssociatedBundleIdentifiers", "-json", string(idsJSON)})
	}
	for _, e := range edits {
		out, err := exec.Command(plutilBin, append(e, tmp)...).CombinedOutput()
		if err != nil && (e[0] != "-remove" || !strings.Contains(string(out), "No value to remove")) {
			return fmt.Errorf("plutil %s: %v: %s", strings.Join(e[:2], " "), err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.Command(plutilBin, "-lint", tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("edited plist is invalid: %s", strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, path)
}

// writeFileReplacing writes data to path through a temp file and rename.
func writeFileReplacing(path string, data []byte, mode os.FileMode) error {
	tmp, err := stageTemp(path, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

// stageTemp writes data with mode to a new temp file next to path and
// returns its name.
func stageTemp(path string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".tincan-refresh-*.plist")
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(f.Name(), mode)); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// launchAgentPlists lists tincan's LaunchAgent plists under home.
func launchAgentPlists(home string) []string {
	paths, _ := filepath.Glob(filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.*.plist"))
	return paths
}

func plistLabel(path string) string { return strings.TrimSuffix(filepath.Base(path), ".plist") }

// loginItemsEnv is what the doctor's login items check reads.
type loginItemsEnv struct {
	home, exe   string
	embedded    bool                      // this build carries Agent Tincan.app
	validateApp func(app string) error    // the installed app's signature
	signed      func(exe string) error    // exe satisfies the launcher's requirement
	liveProgram func(label string) string // the program a loaded job runs, "" if not loaded
}

func defaultLoginItemsEnv(exe string) (loginItemsEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return loginItemsEnv{}, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return loginItemsEnv{
		home:        home,
		exe:         exe,
		embedded:    macapp.Embedded(),
		validateApp: macapp.Validate,
		signed:      macapp.VerifyTincan,
		liveProgram: launchctl{uid: os.Getuid()}.Program,
	}, nil
}

// loginItemsCheck reports whether this tincan's LaunchAgents show as Agent
// Tincan in Login Items, and anything that would stop them from starting.
func loginItemsCheck(e loginItemsEnv) check {
	const name = "login items"
	var wrapped, plain, pending, unreadable, missing []string
	for _, p := range launchAgentPlists(e.home) {
		label := plistLabel(p)
		m, err := readPlistJSON(p)
		if err != nil {
			unreadable = append(unreadable, label)
			continue
		}
		j, ok := parseJob(m)
		if !ok || !j.runs(e.exe) {
			continue
		}
		switch {
		case j.launcher != "":
			wrapped = append(wrapped, label)
			if _, err := os.Stat(j.launcher); err != nil {
				missing = append(missing, label)
			}
			if live := e.liveProgram(label); live != "" && live != j.launcher {
				pending = append(pending, label)
			}
		case label != notes.ServiceLabel:
			plain = append(plain, label)
		}
	}
	app := macapp.Path(e.home)
	if len(unreadable) > 0 {
		return check{name, "fail", fmt.Sprintf("cannot read %s in ~/Library/LaunchAgents, so launchd cannot start them", strings.Join(unreadable, ", ")),
			"Fix or reinstall those services (tincan <service> install), then run tincan services refresh."}
	}
	if len(wrapped) > 0 {
		if len(missing) > 0 {
			return check{name, "fail", fmt.Sprintf("the Agent Tincan launcher %s names is missing, so they cannot start", strings.Join(missing, ", ")),
				"Run tincan services refresh to reinstall " + app + " and point them at it, or tincan services refresh --revert to run them without it."}
		}
		if err := e.validateApp(app); err != nil {
			return check{name, "fail", fmt.Sprintf("%s fails its signature check: %v", app, err), "Run tincan services refresh to reinstall it."}
		}
		if err := e.signed(e.exe); err != nil {
			return check{name, "fail", fmt.Sprintf("the Agent Tincan launcher refuses %s (%v), so %s cannot start", e.exe, err, strings.Join(wrapped, ", ")),
				"Run tincan services refresh --revert, or install a release build of tincan."}
		}
	}
	if len(plain) > 0 && !e.embedded {
		return check{name, "warn", fmt.Sprintf("development build: %s show the signer's name in Login Items instead of Agent Tincan", strings.Join(plain, ", ")),
			"Install a release build of tincan, then run tincan services refresh."}
	}
	if len(plain) > 0 {
		return check{name, "warn", fmt.Sprintf("%s still show the signer's name in Login Items", strings.Join(plain, ", ")), "Run tincan services refresh."}
	}
	if len(pending) > 0 {
		return check{name, "warn", fmt.Sprintf("%s still run without the launcher until they restart", strings.Join(pending, ", ")), "Run tincan services refresh --restart from a terminal, outside those services."}
	}
	if len(wrapped) == 0 {
		return check{name, "ok", "no tincan LaunchAgents for this binary", ""}
	}
	return check{name, "ok", fmt.Sprintf("%s show as Agent Tincan in Login Items", strings.Join(wrapped, ", ")), ""}
}
