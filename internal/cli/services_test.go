package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/macapp"
)

// fakeLaunchd stands in for launchctl: a set of loaded labels, and a log of
// bootout/bootstrap calls. failBootstrap makes the next n bootstraps fail.
type fakeLaunchd struct {
	loaded        map[string]bool
	calls         []string
	failBootstrap int
}

func (f *fakeLaunchd) Loaded(label string) bool { return f.loaded[label] }

func (f *fakeLaunchd) Bootout(label string) error {
	f.calls = append(f.calls, "bootout "+label)
	delete(f.loaded, label)
	return nil
}

func (f *fakeLaunchd) Bootstrap(path string) error {
	label := strings.TrimSuffix(filepath.Base(path), ".plist")
	f.calls = append(f.calls, "bootstrap "+label)
	if f.failBootstrap > 0 {
		f.failBootstrap--
		return errors.New("Bootstrap failed: 5: Input/output error")
	}
	f.loaded[label] = true
	return nil
}

type servicesFixture struct {
	home, agents, exe, launcher string
	ld                          *fakeLaunchd
	out                         strings.Builder
}

func newServicesFixture(t *testing.T) *servicesFixture {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("services refresh edits launchd plists with macOS plutil")
	}
	home := t.TempDir()
	home, _ = filepath.EvalSymlinks(home)
	f := &servicesFixture{home: home, agents: filepath.Join(home, "Library", "LaunchAgents"), ld: &fakeLaunchd{loaded: map[string]bool{}}}
	if err := os.MkdirAll(f.agents, 0o755); err != nil {
		t.Fatal(err)
	}
	f.exe = filepath.Join(home, "bin", "tincan")
	if err := os.MkdirAll(filepath.Dir(f.exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.exe, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.launcher = macapp.Launcher(home)
	return f
}

func (f *servicesFixture) refresher(revert bool) *serviceRefresher {
	return &serviceRefresher{
		home:    f.home,
		exe:     f.exe,
		revert:  revert,
		ensure:  func() (string, error) { return f.launcher, nil },
		verify:  func(string) error { return nil },
		launchd: f.ld,
		sleep:   func() {},
		out:     &f.out,
	}
}

// writePlist writes a launchd plist built from keys with plutil, so the
// fixture is a real plist in whatever form plutil writes.
func (f *servicesFixture) writePlist(t *testing.T, label string, keys map[string]any) string {
	t.Helper()
	p := filepath.Join(f.agents, label+".plist")
	keys["Label"] = label
	b, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-convert", "xml1", p).CombinedOutput(); err != nil {
		t.Fatalf("plutil: %v %s", err, out)
	}
	return p
}

func readPlist(t *testing.T, p string) map[string]any {
	t.Helper()
	out, err := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", p).Output()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func strs(v any) []string {
	var s []string
	for _, x := range v.([]any) {
		s = append(s, x.(string))
	}
	return s
}

func TestRefreshWrapsTincanPlistAndKeepsOtherKeys(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.history", map[string]any{
		"ProgramArguments":     []string{f.exe, "history", "serve"},
		"EnvironmentVariables": map[string]string{"PATH": "/usr/bin"},
		"KeepAlive":            true,
		"StandardOutPath":      "/tmp/h.log",
	})
	before := readPlist(t, p)
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	after := readPlist(t, p)
	if got := strs(after["ProgramArguments"]); !slices.Equal(got, []string{f.launcher, f.exe, "history", "serve"}) {
		t.Fatalf("ProgramArguments = %q", got)
	}
	if got := strs(after["AssociatedBundleIdentifiers"]); !slices.Equal(got, []string{macapp.BundleID}) {
		t.Fatalf("AssociatedBundleIdentifiers = %q", got)
	}
	delete(after, "ProgramArguments")
	delete(after, "AssociatedBundleIdentifiers")
	delete(before, "ProgramArguments")
	a, _ := json.Marshal(after)
	b, _ := json.Marshal(before)
	if string(a) != string(b) {
		t.Fatalf("other keys changed:\nbefore %s\nafter  %s", b, a)
	}
	if !strings.Contains(f.out.String(), "com.agenttincan.history: updated") {
		t.Fatalf("output: %s", f.out.String())
	}
}

func TestRefreshKeepsHandWrittenArgsInOrder(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.web.dots", map[string]any{
		"ProgramArguments": []string{f.exe, "web", "serve", "--site", "dots", "--name", "dot-web", "--thread", "t1"},
	})
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	if got := strs(readPlist(t, p)["ProgramArguments"]); !slices.Equal(got, []string{f.launcher, f.exe, "web", "serve", "--site", "dots", "--name", "dot-web", "--thread", "t1"}) {
		t.Fatalf("ProgramArguments = %q", got)
	}
}

func TestRefreshUpdatesStaleLauncherWithoutDuplicates(t *testing.T) {
	f := newServicesFixture(t)
	old := "/Applications/Agent Tincan.app/Contents/MacOS/agent-tincan"
	p := f.writePlist(t, "com.agenttincan.council", map[string]any{
		"ProgramArguments":            []string{old, f.exe, "council", "serve"},
		"AssociatedBundleIdentifiers": []string{macapp.BundleID},
	})
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	m := readPlist(t, p)
	if got := strs(m["ProgramArguments"]); !slices.Equal(got, []string{f.launcher, f.exe, "council", "serve"}) {
		t.Fatalf("ProgramArguments = %q", got)
	}
	if got := strs(m["AssociatedBundleIdentifiers"]); !slices.Equal(got, []string{macapp.BundleID}) {
		t.Fatalf("AssociatedBundleIdentifiers = %q", got)
	}
}

func TestRefreshTwiceIsANoOp(t *testing.T) {
	f := newServicesFixture(t)
	f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}})
	f.ld.loaded["com.agenttincan.council"] = true
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	f.ld.calls = nil
	f.out.Reset()
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	if len(f.ld.calls) != 0 || !strings.Contains(f.out.String(), "com.agenttincan.council: already current") {
		t.Fatalf("second run: calls %v, output %s", f.ld.calls, f.out.String())
	}
}

func TestRefreshSkipsWhatIsNotTincans(t *testing.T) {
	f := newServicesFixture(t)
	other := filepath.Join(f.home, "x", "tincan") // a tincan that is not the running binary
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"python":   f.writePlist(t, "com.agenttincan.py", map[string]any{"ProgramArguments": []string{"/usr/bin/python3", "x.py"}}),
		"other":    f.writePlist(t, "com.agenttincan.other", map[string]any{"ProgramArguments": []string{other, "listen"}}),
		"mislabel": f.writePlist(t, "com.agenttincan.mislabel", map[string]any{"ProgramArguments": []string{f.exe, "listen"}}),
		"notes":    f.writePlist(t, "com.agenttincan.notes", map[string]any{"ProgramArguments": []string{f.exe, "notes", "serve"}}),
	}
	// A Label that does not match the file name.
	if out, err := exec.Command("/usr/bin/plutil", "-replace", "Label", "-string", "com.agenttincan.history", paths["mislabel"]).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	// A symlinked plist.
	target := filepath.Join(f.home, "real.plist")
	b, _ := os.ReadFile(paths["notes"])
	if err := os.WriteFile(target, b, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.agents, "com.agenttincan.linked.plist")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	snap := map[string][]byte{}
	for _, p := range append(slices.Collect(mapValues(paths)), target) {
		snap[p], _ = os.ReadFile(p)
	}
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	for p, want := range snap {
		if got, _ := os.ReadFile(p); string(got) != string(want) {
			t.Errorf("%s was changed", p)
		}
	}
	out := f.out.String()
	for _, label := range []string{"com.agenttincan.py", "com.agenttincan.other", "com.agenttincan.mislabel", "com.agenttincan.notes", "com.agenttincan.linked"} {
		if !strings.Contains(out, label+": skipped") {
			t.Errorf("no skip line for %s:\n%s", label, out)
		}
	}
}

func mapValues(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for _, v := range m {
			if !yield(v) {
				return
			}
		}
	}
}

func TestRefreshWrapsProgramKeyPlist(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.codex-listen", map[string]any{
		"Program":          f.exe,
		"ProgramArguments": []string{"tincan", "listen", "--exec", "/x/wake.sh"},
	})
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	m := readPlist(t, p)
	if _, ok := m["Program"]; ok {
		t.Fatal("Program key kept; launchd would run it instead of the launcher")
	}
	if got := strs(m["ProgramArguments"]); !slices.Equal(got, []string{f.launcher, f.exe, "listen", "--exec", "/x/wake.sh"}) {
		t.Fatalf("ProgramArguments = %q", got)
	}
}

func TestRefreshKeepsUsersOwnBundleIDs(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.council", map[string]any{
		"ProgramArguments":            []string{f.exe, "council", "serve"},
		"AssociatedBundleIdentifiers": []string{"com.example.mine"},
	})
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	if got := strs(readPlist(t, p)["AssociatedBundleIdentifiers"]); !slices.Equal(got, []string{"com.example.mine", macapp.BundleID}) {
		t.Fatalf("after refresh = %q", got)
	}
	if err := f.refresher(true).run(); err != nil {
		t.Fatal(err)
	}
	if got := strs(readPlist(t, p)["AssociatedBundleIdentifiers"]); !slices.Equal(got, []string{"com.example.mine"}) {
		t.Fatalf("after revert = %q", got)
	}
}

func TestRefreshRestartsOnlyLoadedJobs(t *testing.T) {
	f := newServicesFixture(t)
	f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}})
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.exe, "history", "serve"}})
	f.ld.loaded["com.agenttincan.council"] = true
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"bootout com.agenttincan.council", "bootstrap com.agenttincan.council"}; !slices.Equal(f.ld.calls, want) {
		t.Fatalf("launchctl calls = %q, want %q", f.ld.calls, want)
	}
	if !strings.Contains(f.out.String(), "com.agenttincan.council: updated, restarted") {
		t.Fatalf("output: %s", f.out.String())
	}
}

func TestRefreshNeverRestartsItsOwnJob(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.codex-listen", map[string]any{"ProgramArguments": []string{f.exe, "listen"}})
	f.ld.loaded["com.agenttincan.codex-listen"] = true
	r := f.refresher(false)
	r.self = "com.agenttincan.codex-listen"
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if len(f.ld.calls) != 0 {
		t.Fatalf("restarted its own job: %q", f.ld.calls)
	}
	if got := strs(readPlist(t, p)["ProgramArguments"]); got[0] != f.launcher {
		t.Fatalf("own job's plist not rewritten: %q", got)
	}
	if !strings.Contains(f.out.String(), "takes effect at next restart") {
		t.Fatalf("output: %s", f.out.String())
	}
}

func TestRefreshRetriesBootstrap(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}})
	f.ld.loaded["com.agenttincan.council"] = true
	f.ld.failBootstrap = 1
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	if !f.ld.loaded["com.agenttincan.council"] || strs(readPlist(t, p)["ProgramArguments"])[0] != f.launcher {
		t.Fatalf("job not loaded with the new plist; calls %q", f.ld.calls)
	}
}

func TestRefreshRestoresOriginalWhenRestartKeepsFailing(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}})
	orig, _ := os.ReadFile(p)
	f.ld.loaded["com.agenttincan.council"] = true
	f.ld.failBootstrap = 3
	err := f.refresher(false).run()
	if err == nil {
		t.Fatal("a failed restart was not reported as an error")
	}
	if got, _ := os.ReadFile(p); string(got) != string(orig) {
		t.Fatal("original plist not restored")
	}
	if !f.ld.loaded["com.agenttincan.council"] {
		t.Fatalf("original job not reloaded; calls %q", f.ld.calls)
	}
	if !strings.Contains(f.out.String(), "com.agenttincan.council: restart failed") {
		t.Fatalf("output: %s", f.out.String())
	}
}

func TestRefreshRevertRestoresPlainPlist(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}, "KeepAlive": true})
	before := readPlist(t, p)
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	if err := f.refresher(true).run(); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(readPlist(t, p))
	b, _ := json.Marshal(before)
	if string(a) != string(b) {
		t.Fatalf("revert did not restore the plist:\nwant %s\ngot  %s", b, a)
	}
}

func TestRefreshWithoutEmbeddedAppChangesNothing(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}})
	orig, _ := os.ReadFile(p)
	r := f.refresher(false)
	r.ensure = func() (string, error) { return "", macapp.ErrUnavailable }
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != string(orig) {
		t.Fatal("plist changed without an app")
	}
	if !strings.Contains(f.out.String(), "does not include Agent Tincan.app") {
		t.Fatalf("output: %s", f.out.String())
	}
}

func (f *servicesFixture) loginItems() loginItemsEnv {
	return loginItemsEnv{
		home:        f.home,
		exe:         f.exe,
		embedded:    true,
		validateApp: func(string) error { return nil },
		signed:      func(string) error { return nil },
		liveProgram: func(string) string { return "" },
	}
}

func (f *servicesFixture) installLauncher(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.launcher, []byte("l"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLoginItemsCheckDevelopmentBuild(t *testing.T) {
	f := newServicesFixture(t)
	f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}})
	env := f.loginItems()
	env.embedded = false
	c := loginItemsCheck(env)
	if c.Status != "warn" || !strings.Contains(c.Detail, "development build") {
		t.Fatalf("check = %+v", c)
	}
}

func TestLoginItemsCheckNamesUnrefreshedPlist(t *testing.T) {
	f := newServicesFixture(t)
	f.installLauncher(t)
	f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}})
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "history", "serve"}})
	c := loginItemsCheck(f.loginItems())
	if c.Status != "warn" || !strings.Contains(c.Detail, "com.agenttincan.council") || strings.Contains(c.Detail, "com.agenttincan.history") || !strings.Contains(c.Fix, "tincan services refresh") {
		t.Fatalf("check = %+v", c)
	}
}

func TestLoginItemsCheckPassesWhenCurrent(t *testing.T) {
	f := newServicesFixture(t)
	f.installLauncher(t)
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "history", "serve"}})
	f.writePlist(t, "com.agenttincan.notes", map[string]any{"ProgramArguments": []string{f.exe, "notes", "serve"}})
	if c := loginItemsCheck(f.loginItems()); c.Status != "ok" {
		t.Fatalf("check = %+v", c)
	}
}

func TestLoginItemsCheckMissingAppBreaksWrappedServices(t *testing.T) {
	f := newServicesFixture(t)
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "history", "serve"}})
	c := loginItemsCheck(f.loginItems())
	if c.Status != "fail" || !strings.Contains(c.Detail, "missing") || !strings.Contains(c.Fix, "tincan services refresh") {
		t.Fatalf("check = %+v", c)
	}
}

func TestLoginItemsCheckModifiedApp(t *testing.T) {
	f := newServicesFixture(t)
	f.installLauncher(t)
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "history", "serve"}})
	env := f.loginItems()
	env.validateApp = func(string) error { return errors.New("a sealed resource is missing or invalid") }
	c := loginItemsCheck(env)
	if c.Status != "fail" || !strings.Contains(c.Detail, "signature") {
		t.Fatalf("check = %+v", c)
	}
}

func TestLoginItemsCheckUnsignedBuildBehindLauncher(t *testing.T) {
	f := newServicesFixture(t)
	f.installLauncher(t)
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "history", "serve"}})
	env := f.loginItems()
	env.signed = func(string) error { return errors.New("not signed by Agent Tincan") }
	c := loginItemsCheck(env)
	if c.Status != "fail" || !strings.Contains(c.Fix, "tincan services refresh --revert") {
		t.Fatalf("check = %+v", c)
	}
}

func TestLoginItemsCheckRestartPending(t *testing.T) {
	f := newServicesFixture(t)
	f.installLauncher(t)
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "history", "serve"}})
	env := f.loginItems()
	env.liveProgram = func(label string) string { return f.exe }
	c := loginItemsCheck(env)
	if c.Status != "warn" || !strings.Contains(c.Detail, "com.agenttincan.history") || !strings.Contains(c.Detail, "restart") {
		t.Fatalf("check = %+v", c)
	}
}

// After an upgrade, services keep running the old build until restarted, so
// --restart restarts loaded tincan jobs even when their plist is current,
// notes included, but never the job refresh runs under.
func TestRefreshRestartRestartsCurrentLoadedJobs(t *testing.T) {
	f := newServicesFixture(t)
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "history", "serve"}, "AssociatedBundleIdentifiers": []string{macapp.BundleID}})
	f.writePlist(t, "com.agenttincan.notes", map[string]any{"ProgramArguments": []string{f.exe, "notes", "serve"}})
	f.writePlist(t, "com.agenttincan.codex-listen", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "listen"}, "AssociatedBundleIdentifiers": []string{macapp.BundleID}})
	f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "council", "serve"}, "AssociatedBundleIdentifiers": []string{macapp.BundleID}})
	for _, l := range []string{"com.agenttincan.history", "com.agenttincan.notes", "com.agenttincan.codex-listen"} {
		f.ld.loaded[l] = true
	}
	r := f.refresher(false)
	r.restartCurrent = true
	r.self = "com.agenttincan.codex-listen"
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	want := []string{"bootout com.agenttincan.history", "bootstrap com.agenttincan.history", "bootout com.agenttincan.notes", "bootstrap com.agenttincan.notes"}
	if !slices.Equal(f.ld.calls, want) {
		t.Fatalf("launchctl calls = %q, want %q", f.ld.calls, want)
	}
	out := f.out.String()
	for _, w := range []string{"com.agenttincan.history: already current, restarted", "com.agenttincan.notes: skipped", "restarted", "com.agenttincan.council: already current"} {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q:\n%s", w, out)
		}
	}
}

func TestRefreshDevBuildNamesWrappedServices(t *testing.T) {
	f := newServicesFixture(t)
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "history", "serve"}})
	r := f.refresher(false)
	r.ensure = func() (string, error) { return "", macapp.ErrUnavailable }
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if out := f.out.String(); !strings.Contains(out, "com.agenttincan.history start through Agent Tincan.app") || !strings.Contains(out, "--revert") {
		t.Fatalf("output: %s", out)
	}
}

func TestLoginItemsCheckUnreadablePlist(t *testing.T) {
	f := newServicesFixture(t)
	if err := os.WriteFile(filepath.Join(f.agents, "com.agenttincan.broken.plist"), []byte("not a plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := loginItemsCheck(f.loginItems()); c.Status != "fail" || !strings.Contains(c.Detail, "com.agenttincan.broken") {
		t.Fatalf("check = %+v", c)
	}
}

// An unsigned tincan (a self-built binary that embeds the signed app) must
// not wrap its services: the launcher would refuse it and they would stop.
func TestRefreshLeavesServicesAloneForUnsignedBinary(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}})
	orig, _ := os.ReadFile(p)
	f.ld.loaded["com.agenttincan.council"] = true
	r := f.refresher(false)
	r.verify = func(string) error { return errors.New("not signed by Agent Tincan") }
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != string(orig) || len(f.ld.calls) != 0 {
		t.Fatalf("services changed for an unsigned binary; calls %q", f.ld.calls)
	}
	if !strings.Contains(f.out.String(), "not signed") {
		t.Fatalf("output: %s", f.out.String())
	}
}

// Revert restores a hand-written plist byte for byte when nothing changed
// since refresh, including a Program key, its argv[0], and the user's own
// com.agenttincan.app entry.
func TestRefreshRevertRestoresHandWrittenPlistExactly(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.codex-listen", map[string]any{
		"Program":                     f.exe,
		"ProgramArguments":            []string{"my-listener", "listen"},
		"AssociatedBundleIdentifiers": []string{macapp.BundleID, "com.example.mine"},
	})
	orig, _ := os.ReadFile(p)
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) == string(orig) {
		t.Fatal("refresh did not wrap the plist")
	}
	if err := f.refresher(true).run(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != string(orig) {
		t.Fatalf("revert did not restore the original:\n%s\nwant\n%s", got, orig)
	}
}

// Revert leaves a plist refresh never touched exactly as it is.
func TestRefreshRevertLeavesUntouchedPlistAlone(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.codex-listen", map[string]any{"Program": f.exe, "ProgramArguments": []string{"tincan", "listen"}})
	orig, _ := os.ReadFile(p)
	if err := f.refresher(true).run(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != string(orig) {
		t.Fatal("revert changed a plist refresh never wrapped")
	}
}

// When a wrapped plist was edited after refresh, revert keeps the edit and
// only unwraps it.
func TestRefreshRevertKeepsLaterEdits(t *testing.T) {
	f := newServicesFixture(t)
	p := f.writePlist(t, "com.agenttincan.council", map[string]any{"ProgramArguments": []string{f.exe, "council", "serve"}})
	if err := f.refresher(false).run(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-insert", "ProgramArguments.4", "-string", "--verbose", p).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := f.refresher(true).run(); err != nil {
		t.Fatal(err)
	}
	if got := strs(readPlist(t, p)["ProgramArguments"]); !slices.Equal(got, []string{f.exe, "council", "serve", "--verbose"}) {
		t.Fatalf("ProgramArguments = %q", got)
	}
}

// Doctor checks the launcher each plist names, not only the default app.
func TestLoginItemsCheckMissingNamedLauncher(t *testing.T) {
	f := newServicesFixture(t)
	f.installLauncher(t)
	old := filepath.Join(f.home, "Old", "Agent Tincan.app", "Contents", "MacOS", "agent-tincan")
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{old, f.exe, "history", "serve"}})
	c := loginItemsCheck(f.loginItems())
	if c.Status != "fail" || !strings.Contains(c.Detail, "com.agenttincan.history") || !strings.Contains(c.Fix, "tincan services refresh") {
		t.Fatalf("check = %+v", c)
	}
}

func TestLoginItemsCheckPendingRestartAdvice(t *testing.T) {
	f := newServicesFixture(t)
	f.installLauncher(t)
	f.writePlist(t, "com.agenttincan.history", map[string]any{"ProgramArguments": []string{f.launcher, f.exe, "history", "serve"}})
	env := f.loginItems()
	env.liveProgram = func(string) string { return f.exe }
	if c := loginItemsCheck(env); !strings.Contains(c.Fix, "tincan services refresh --restart") {
		t.Fatalf("check = %+v", c)
	}
}
