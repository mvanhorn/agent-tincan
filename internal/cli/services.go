package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/macapp"
)

const plutilBin = "/usr/bin/plutil"

func servicesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "services",
		Short: "Manage tincan's macOS background services",
	}
	var revert bool
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
restarted to pick up the change, except the one this command runs under.

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
				home:    home,
				uid:     os.Getuid(),
				exe:     exe,
				revert:  revert,
				ensure:  func() (string, error) { return macapp.Ensure(macapp.Options{Home: home}) },
				launchd: launchctl{uid: os.Getuid()},
				self:    os.Getenv("XPC_SERVICE_NAME"),
				sleep:   func() { time.Sleep(time.Second) },
				out:     cmd.OutOrStdout(),
			}
			return r.run()
		},
	}
	refresh.Flags().BoolVar(&revert, "revert", false, "make services run the tincan binary directly again")
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
	home    string
	uid     int
	exe     string // the running tincan binary, symlinks resolved
	revert  bool
	ensure  func() (string, error)
	launchd launchdOps
	self    string // the launchd job this process runs under, if any
	sleep   func()
	out     io.Writer
}

func (r *serviceRefresher) run() error {
	launcher := ""
	if !r.revert {
		l, err := r.ensure()
		if errors.Is(err, macapp.ErrUnavailable) {
			fmt.Fprintln(r.out, "This tincan build does not include Agent Tincan.app (a development build), so its services keep showing the signer's name in Login Items. To undo an earlier refresh, run tincan services refresh --revert.")
			return nil
		}
		if err != nil {
			return fmt.Errorf("install Agent Tincan.app: %w", err)
		}
		launcher = l
	}
	paths, err := filepath.Glob(filepath.Join(r.home, "Library", "LaunchAgents", "com.agenttincan.*.plist"))
	if err != nil {
		return err
	}
	var failed []string
	for _, p := range paths {
		label := strings.TrimSuffix(filepath.Base(p), ".plist")
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
	args, ok := stringList(m["ProgramArguments"])
	prog, hasProgram := m["Program"].(string)
	if !hasProgram {
		if !ok || len(args) == 0 {
			return "skipped (no program)", nil
		}
		prog, args = args[0], args[1:]
	} else if len(args) > 0 {
		args = args[1:] // with Program, ProgramArguments[0] is only argv[0]
	}
	target, targs := prog, args
	if isLauncher(prog) && len(args) > 0 {
		target, targs = args[0], args[1:]
	}
	if resolved, err := filepath.EvalSymlinks(target); err != nil || resolved != r.exe {
		return fmt.Sprintf("skipped (runs %s, not this tincan %s)", target, r.exe), nil
	}
	if label == "com.agenttincan.notes" && !r.revert {
		return "skipped (the notes service keeps running tincan directly so its Files and Folders grant stays its own)", nil
	}
	ids, _ := stringList(m["AssociatedBundleIdentifiers"])
	wantArgs := append([]string{target}, targs...)
	wantIDs := slices.DeleteFunc(slices.Clone(ids), func(s string) bool { return s == macapp.BundleID })
	if !r.revert {
		wantArgs = append([]string{launcher}, wantArgs...)
		wantIDs = append(wantIDs, macapp.BundleID)
	}
	curArgs, _ := stringList(m["ProgramArguments"])
	if !hasProgram && slices.Equal(curArgs, wantArgs) && slices.Equal(ids, wantIDs) {
		return "already current", nil
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := writePlistEdit(path, fi.Mode().Perm(), wantArgs, hasProgram, wantIDs); err != nil {
		return "", fmt.Errorf("update failed: %w", err)
	}
	if !r.launchd.Loaded(label) {
		return "updated", nil
	}
	if label == r.self {
		return "updated; takes effect at next restart (refresh is running under this service)", nil
	}
	if err := r.restart(label, path); err != nil {
		// Put the original back so the service runs as before.
		werr := writeFileReplacing(path, orig, fi.Mode().Perm())
		var berr error
		if werr == nil && !r.launchd.Loaded(label) {
			berr = r.launchd.Bootstrap(path)
		}
		return "", fmt.Errorf("restart failed (%v); restored the original plist%s", err, restoreNote(werr, berr))
	}
	return "updated, restarted", nil
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
func writePlistEdit(path string, mode os.FileMode, args []string, dropProgram bool, ids []string) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tincan-refresh-*.plist")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(orig); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
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
		out, err := exec.Command(plutilBin, append(e, tmp.Name())...).CombinedOutput()
		if err != nil && !(e[0] == "-remove" && strings.Contains(string(out), "No value to remove")) {
			return fmt.Errorf("plutil %s: %v: %s", strings.Join(e[:2], " "), err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.Command(plutilBin, "-lint", tmp.Name()).CombinedOutput(); err != nil {
		return fmt.Errorf("edited plist is invalid: %s", strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// writeFileReplacing writes data to path through a temp file and rename.
func writeFileReplacing(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tincan-refresh-*.plist")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
