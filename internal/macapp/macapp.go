// Package macapp installs Agent Tincan.app, the small signed app every tincan
// LaunchAgent on macOS starts through (cmd/agent-tincan-app), so macOS lists
// tincan's services in Login Items as "Agent Tincan" with its icon instead of
// under the signing certificate's personal name.
//
// Release builds of tincan for macOS embed the signed, notarized and stapled
// app as a zip (build tag macapp, see `make mac-app`). Ensure installs it into
// ~/Applications, which needs no admin password, and replaces it only when the
// installed copy is missing, fails its signature check, or differs from the
// embedded one. Development builds and other systems embed nothing, and
// Ensure reports ErrUnavailable so callers write service files as before.
package macapp

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// BundleID identifies Agent Tincan.app. LaunchAgents name it in
	// AssociatedBundleIdentifiers.
	BundleID = "com.agenttincan.app"
	// AppName is the bundle's directory name in ~/Applications.
	AppName = "Agent Tincan.app"
	// LauncherPath is the launcher executable inside the bundle.
	LauncherPath = "Contents/MacOS/agent-tincan"

	developerID = `anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "NM8VT393AR"`

	// TincanRequirement is what the launcher demands of the binary it runs:
	// Apple's Developer ID chain, the Agent Tincan team, and a tincan
	// signing identifier. The Team ID alone is not enough, since a
	// self-signed certificate can carry any OU.
	TincanRequirement = developerID + ` and (identifier "tincan_darwin_arm64" or identifier "tincan_darwin_amd64" or identifier "com.agenttincan.tincan")`

	// AppRequirement is what an installed Agent Tincan.app must satisfy.
	AppRequirement = developerID + ` and identifier "` + BundleID + `"`

	lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
)

// ErrUnavailable means this tincan cannot install the app: it is not running
// on macOS, or it is a build without the embedded app.
var ErrUnavailable = errors.New("this build does not include Agent Tincan.app")

// Embedded reports whether this build carries the app.
func Embedded() bool { return len(embeddedZip) > 0 }

// Options configures Ensure. Zero values mean the running system: the user's
// home, runtime.GOOS, the embedded zip, codesign validation and lsregister.
type Options struct {
	Home     string
	GOOS     string
	Zip      []byte
	Validate func(app string) error
	Register func(app string) error
}

func (o Options) withDefaults() (Options, error) {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Zip == nil {
		o.Zip = embeddedZip
	}
	if o.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return o, err
		}
		o.Home = h
	}
	if o.Validate == nil {
		o.Validate = Validate
	}
	if o.Register == nil {
		o.Register = register
	}
	return o, nil
}

// Path is the installed app's location for home.
func Path(home string) string { return filepath.Join(home, "Applications", AppName) }

// Launcher is the installed launcher's path for home.
func Launcher(home string) string { return filepath.Join(Path(home), LauncherPath) }

// Ensure makes sure ~/Applications/Agent Tincan.app is the embedded app and
// returns its launcher path. It returns ErrUnavailable off macOS or without
// an embedded app, and leaves any installed app untouched on error.
func Ensure(o Options) (string, error) {
	o, err := o.withDefaults()
	if err != nil {
		return "", err
	}
	if o.GOOS != "darwin" || len(o.Zip) == 0 {
		return "", ErrUnavailable
	}
	zr, err := zip.NewReader(bytes.NewReader(o.Zip), int64(len(o.Zip)))
	if err != nil {
		return "", fmt.Errorf("embedded Agent Tincan.app: %w", err)
	}
	dst := Path(o.Home)
	if current(zr, dst) && o.Validate(dst) == nil {
		return Launcher(o.Home), nil
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".agent-tincan-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extract(zr, tmp); err != nil {
		return "", fmt.Errorf("unpack Agent Tincan.app: %w", err)
	}
	staged := filepath.Join(tmp, AppName)
	if err := o.Validate(staged); err != nil {
		return "", fmt.Errorf("unpacked Agent Tincan.app: %w", err)
	}
	if _, err := os.Lstat(dst); err == nil {
		// Swap in one step so the launcher path never goes missing while
		// launchd may be starting a service; the old copy lands in tmp.
		if err := swap(staged, dst); err != nil {
			return "", fmt.Errorf("replace %s: %w", dst, err)
		}
	} else if err := os.Rename(staged, dst); err != nil {
		return "", err
	}
	if err := o.Register(dst); err != nil {
		return "", fmt.Errorf("register %s: %w", dst, err)
	}
	return Launcher(o.Home), nil
}

// current reports whether every file in the zip is already at dst with the
// same content and executable bit.
func current(zr *zip.Reader, dst string) bool {
	for _, f := range zr.File {
		rel, ok := entryPath(f.Name)
		if !ok {
			return false
		}
		if f.FileInfo().IsDir() {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != int64(f.UncompressedSize64) ||
			fi.Mode().Perm()&0o111 != f.Mode().Perm()&0o111 {
			return false
		}
		want, err := readEntry(f)
		if err != nil {
			return false
		}
		have, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil || !bytes.Equal(have, want) {
			return false
		}
	}
	return true
}

// entryPath maps a zip entry to its path inside the bundle.
func entryPath(name string) (string, bool) {
	rel, ok := strings.CutPrefix(name, AppName+"/")
	return filepath.FromSlash(rel), ok
}

// extract writes the zip's app into dir. The zip is build-time content
// inside the signed tincan binary; the signature check on the result is
// what guards the install.
func extract(zr *zip.Reader, dir string) error {
	for _, f := range zr.File {
		rel, ok := entryPath(f.Name)
		if !ok {
			return fmt.Errorf("unexpected entry %q", f.Name)
		}
		p := filepath.Join(dir, AppName, rel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		b, err := readEntry(f)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, f.Mode().Perm()|0o400); err != nil {
			return err
		}
	}
	return nil
}

func readEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Validate checks app's code signature against AppRequirement.
func Validate(app string) error {
	out, err := exec.Command("/usr/bin/codesign", "--verify", "--strict", "--deep", "-R="+AppRequirement, app).CombinedOutput()
	if err != nil {
		return fmt.Errorf("signature check failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func register(app string) error {
	if out, err := exec.Command(lsregister, "-f", app).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
