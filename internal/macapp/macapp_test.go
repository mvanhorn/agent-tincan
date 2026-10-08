package macapp

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testZip builds an app zip shaped like `ditto -c -k --keepParent` output:
// every entry under "Agent Tincan.app/", with the launcher executable.
func testZip(t *testing.T, launcher, bundleID string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, mode os.FileMode, body string) {
		h := &zip.FileHeader{Name: AppName + "/" + name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("Contents/Info.plist", 0o644, "<plist><dict><key>CFBundleIdentifier</key><string>"+bundleID+"</string></dict></plist>")
	add("Contents/MacOS/agent-tincan", 0o755, launcher)
	add("Contents/Resources/AppIcon.icns", 0o644, "icon")
	add("Contents/_CodeSignature/CodeResources", 0o644, "seal")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type recorder struct {
	validated  []string
	registered []string
	reject     func(path string) bool
}

func (r *recorder) opts(home string, z []byte) Options {
	return Options{
		Home: home,
		GOOS: "darwin",
		Zip:  z,
		Validate: func(app string) error {
			r.validated = append(r.validated, app)
			if r.reject != nil && r.reject(app) {
				return errors.New("signature invalid")
			}
			return nil
		},
		Register: func(app string) error {
			r.registered = append(r.registered, app)
			return nil
		},
	}
}

func appPath(home string) string { return filepath.Join(home, "Applications", AppName) }

func launcherBody(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(appPath(home), LauncherPath))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func noTemps(t *testing.T, home string) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(home, "Applications"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != AppName {
			t.Errorf("left behind %s in ~/Applications", e.Name())
		}
	}
}

func TestEnsureInstallsFreshApp(t *testing.T) {
	home := t.TempDir()
	var r recorder
	got, err := Ensure(r.opts(home, testZip(t, "v1", BundleID)))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(appPath(home), LauncherPath)
	if got != want {
		t.Fatalf("launcher = %q, want %q", got, want)
	}
	fi, err := os.Stat(want)
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("launcher not an executable file: %v %v", fi, err)
	}
	if launcherBody(t, home) != "v1" {
		t.Fatal("launcher content not from the zip")
	}
	if len(r.registered) != 1 || r.registered[0] != appPath(home) {
		t.Fatalf("registered %v, want the installed app", r.registered)
	}
	noTemps(t, home)
}

func TestEnsureLeavesIdenticalAppAlone(t *testing.T) {
	home := t.TempDir()
	z := testZip(t, "v1", BundleID)
	var r recorder
	if _, err := Ensure(r.opts(home, z)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	l := filepath.Join(appPath(home), LauncherPath)
	if err := os.Chtimes(l, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(r.opts(home, z)); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(l)
	if !fi.ModTime().Equal(old) {
		t.Fatal("an identical, valid app was rewritten")
	}
}

func TestEnsureReplacesChangedApp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the atomic directory swap is macOS only")
	}
	home := t.TempDir()
	var r recorder
	if _, err := Ensure(r.opts(home, testZip(t, "v1", BundleID))); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(r.opts(home, testZip(t, "v2", BundleID))); err != nil {
		t.Fatal(err)
	}
	if launcherBody(t, home) != "v2" {
		t.Fatal("changed app was not replaced")
	}
	noTemps(t, home)
}

func TestEnsureReplacesTamperedAppWithSameContentName(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the atomic directory swap is macOS only")
	}
	home := t.TempDir()
	z := testZip(t, "v1", BundleID)
	var r recorder
	if _, err := Ensure(r.opts(home, z)); err != nil {
		t.Fatal(err)
	}
	// Same bytes on disk, but the installed copy no longer validates (its
	// signature was broken by a modified file elsewhere in the bundle).
	installed := appPath(home)
	if err := os.WriteFile(filepath.Join(installed, "Contents/Resources/extra"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.reject = func(p string) bool {
		_, err := os.Stat(filepath.Join(p, "Contents/Resources/extra"))
		return err == nil
	}
	if _, err := Ensure(r.opts(home, z)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(installed, "Contents/Resources/extra")); !os.IsNotExist(err) {
		t.Fatal("tampered app was not replaced")
	}
	noTemps(t, home)
}

func TestEnsureKeepsExistingAppWhenNewCopyFailsValidation(t *testing.T) {
	home := t.TempDir()
	var r recorder
	if _, err := Ensure(r.opts(home, testZip(t, "v1", BundleID))); err != nil {
		t.Fatal(err)
	}
	installed := appPath(home)
	r.reject = func(p string) bool { return p != installed }
	if _, err := Ensure(r.opts(home, testZip(t, "v2", "com.example.other"))); err == nil {
		t.Fatal("a copy that fails validation was accepted")
	}
	if launcherBody(t, home) != "v1" {
		t.Fatal("existing app was disturbed")
	}
	noTemps(t, home)
}

func TestEnsureRejectsCorruptZip(t *testing.T) {
	home := t.TempDir()
	var r recorder
	if _, err := Ensure(r.opts(home, testZip(t, "v1", BundleID))); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(r.opts(home, []byte("not a zip"))); err == nil {
		t.Fatal("corrupt zip accepted")
	}
	if launcherBody(t, home) != "v1" {
		t.Fatal("existing app was disturbed")
	}
	noTemps(t, home)
}

func TestEnsureUnavailableWithoutZipOrOffMac(t *testing.T) {
	home := t.TempDir()
	var r recorder
	o := r.opts(home, nil)
	if _, err := Ensure(o); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no zip: err = %v, want ErrUnavailable", err)
	}
	o = r.opts(home, testZip(t, "v1", BundleID))
	o.GOOS = "linux"
	if _, err := Ensure(o); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("linux: err = %v, want ErrUnavailable", err)
	}
	if _, err := os.Stat(filepath.Join(home, "Applications")); !os.IsNotExist(err) {
		t.Fatal("wrote ~/Applications while unavailable")
	}
}

func TestRequirementsPinAppleDeveloperID(t *testing.T) {
	for _, req := range []string{TincanRequirement, AppRequirement} {
		for _, part := range []string{"anchor apple generic", "certificate 1[field.1.2.840.113635.100.6.2.6] exists", "certificate leaf[field.1.2.840.113635.100.6.1.13] exists", `certificate leaf[subject.OU] = "NM8VT393AR"`} {
			if !strings.Contains(req, part) {
				t.Errorf("%q lacks %q", req, part)
			}
		}
	}
	if !strings.Contains(AppRequirement, `identifier "`+BundleID+`"`) {
		t.Error("AppRequirement does not pin the app's bundle identifier")
	}
}
