package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeExec records the exec the launcher asked for instead of replacing the
// test process.
type fakeExec struct {
	path string
	argv []string
}

func (f *fakeExec) exec(path string, argv []string, _ []string) error {
	f.path, f.argv = path, argv
	return nil
}

func executable(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tincan")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func accept(string) error { return nil }

func TestLauncherExecsTargetWithItsArgs(t *testing.T) {
	target := executable(t)
	var fx fakeExec
	var stderr strings.Builder
	code := run([]string{"/app/agent-tincan", target, "history", "serve"}, accept, fx.exec, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("run = %d, stderr %q", code, stderr.String())
	}
	want, _ := filepath.EvalSymlinks(target)
	if fx.path != want || strings.Join(fx.argv, "|") != want+"|history|serve" {
		t.Fatalf("exec(%q, %q), want the target with argv[1:]", fx.path, fx.argv)
	}
}

func TestLauncherWithNoArgumentsDoesNothing(t *testing.T) {
	var fx fakeExec
	var stderr strings.Builder
	if code := run([]string{"/app/agent-tincan"}, accept, fx.exec, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0 when opened without arguments", code)
	}
	if fx.path != "" {
		t.Fatalf("exec ran %q with no target", fx.path)
	}
}

func TestLauncherRefusesBadTargets(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"relative":       "tincan",
		"missing":        filepath.Join(dir, "nope"),
		"not executable": plain,
		"directory":      dir,
	} {
		t.Run(name, func(t *testing.T) {
			var fx fakeExec
			var stderr strings.Builder
			if code := run([]string{"/app/agent-tincan", target, "x"}, accept, fx.exec, &stderr); code == 0 {
				t.Fatalf("run accepted %q", target)
			}
			if fx.path != "" || !strings.Contains(stderr.String(), "agent-tincan:") {
				t.Fatalf("exec %q, stderr %q", fx.path, stderr.String())
			}
		})
	}
}

func TestLauncherRefusesTargetFailingSignature(t *testing.T) {
	target := executable(t)
	var fx fakeExec
	var stderr strings.Builder
	reject := func(string) error { return errors.New("not signed by Agent Tincan") }
	if code := run([]string{"/app/agent-tincan", target}, reject, fx.exec, &stderr); code == 0 || fx.path != "" {
		t.Fatalf("run = %d, exec %q; want a refusal", code, fx.path)
	}
	if !strings.Contains(stderr.String(), "not signed by Agent Tincan") {
		t.Fatalf("stderr %q does not name the signature problem", stderr.String())
	}
}

func TestLauncherVerifiesTheResolvedTarget(t *testing.T) {
	target := executable(t)
	link := filepath.Join(t.TempDir(), "tincan")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var checked string
	var fx fakeExec
	var stderr strings.Builder
	verify := func(p string) error { checked = p; return nil }
	if code := run([]string{"/app/agent-tincan", link}, verify, fx.exec, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr %q", code, stderr.String())
	}
	want, _ := filepath.EvalSymlinks(target)
	if checked != want || fx.path != want {
		t.Fatalf("verified %q and ran %q, want both to be the resolved %q", checked, fx.path, want)
	}
}

// The production requirement pins Apple's Developer ID chain, not just a Team
// ID string that a self-signed certificate could carry.
func TestRequirementIsAppleAnchoredDeveloperID(t *testing.T) {
	for _, part := range []string{
		"anchor apple generic",
		"certificate 1[field.1.2.840.113635.100.6.2.6] exists",
		"certificate leaf[field.1.2.840.113635.100.6.1.13] exists",
		`certificate leaf[subject.OU] = "NM8VT393AR"`,
		`identifier "com.agenttincan.tincan"`,
	} {
		if !strings.Contains(Requirement, part) {
			t.Errorf("Requirement lacks %q", part)
		}
	}
}

// The real verifier refuses an ad-hoc-signed binary.
func TestCodesignVerifierRefusesAdHocBinary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("codesign is macOS only")
	}
	p := filepath.Join(t.TempDir(), "tincan")
	b, err := os.ReadFile("/bin/echo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/codesign", "-f", "-s", "-", p).CombinedOutput(); err != nil {
		t.Skipf("cannot ad-hoc sign: %v %s", err, out)
	}
	if err := codesignVerify(p); err == nil {
		t.Fatal("an ad-hoc-signed binary satisfied the requirement")
	}
}
