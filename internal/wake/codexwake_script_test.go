package wake

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests run examples/codex/codex-wake.sh with a fake codex binary that
// records its argv, so they check exactly what the wake would hand to
// "codex exec": the sandbox flags, the prompt, and which write roots become
// --add-dir.

const fakeCodex = `#!/bin/sh
for a in "$@"; do printf '%s\0' "$a"; done > "$ARGV_OUT"
`

type wakeRun struct {
	argv   []string
	stderr string
}

// flagValues returns every value that follows flag in argv.
func (r wakeRun) flagValues(flag string) []string {
	var out []string
	for i := 0; i+1 < len(r.argv); i++ {
		if r.argv[i] == flag {
			out = append(out, r.argv[i+1])
		}
	}
	return out
}

func (r wakeRun) prompt() string {
	if len(r.argv) == 0 {
		return ""
	}
	return r.argv[len(r.argv)-1]
}

func canonical(t *testing.T, p string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return c
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// runCodexWake runs the wake script with HOME at home and the given extra
// environment, and returns the argv the fake codex received.
func runCodexWake(t *testing.T, home string, env ...string) wakeRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("codex-wake.sh is a POSIX sh script")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "codex", "codex-wake.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "codex")
	if err := os.WriteFile(bin, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	argvOut := filepath.Join(tmp, "argv")

	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"CODEX_BIN=" + bin,
		"ARGV_OUT=" + argvOut,
		"TINCAN_CONFIG=" + filepath.Join(tmp, "codex.json"),
		"TINCAN_CODEX_LOCK_DIR=" + filepath.Join(tmp, "lock"),
		"TINCAN_CODEX_WORKDIR=" + filepath.Join(tmp, "work"),
		"TINCAN_WAITING=1",
	}, env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("codex-wake.sh: %v\nstderr:\n%s", err, stderr.String())
	}
	raw, err := os.ReadFile(argvOut)
	if err != nil {
		t.Fatalf("fake codex was not run: %v\nstderr:\n%s", err, stderr.String())
	}
	argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	return wakeRun{argv: argv, stderr: stderr.String()}
}

func TestCodexWakeEnablesSandboxNetwork(t *testing.T) {
	r := runCodexWake(t, t.TempDir())
	found := false
	for _, v := range r.flagValues("-c") {
		if v == "sandbox_workspace_write.network_access=true" {
			found = true
		}
	}
	if !found {
		t.Fatalf("codex exec argv lacks -c sandbox_workspace_write.network_access=true: %q", r.argv)
	}
	if got := r.flagValues("--sandbox"); len(got) != 1 || got[0] != "workspace-write" {
		t.Fatalf("--sandbox = %q, want [workspace-write]", got)
	}
	for _, a := range r.argv {
		if a == "--dangerously-bypass-approvals-and-sandbox" {
			t.Fatalf("wake must not bypass the sandbox: %q", r.argv)
		}
	}
}

func TestCodexWakePromptLocatesPriorThreads(t *testing.T) {
	p := runCodexWake(t, t.TempDir()).prompt()
	if !strings.Contains(p, "tincan history codex --list 20 --all") {
		t.Fatalf("prompt lacks the history lookup instruction:\n%s", p)
	}
	if !strings.Contains(p, "cwd") {
		t.Fatalf("prompt does not tell Codex to use the thread's cwd:\n%s", p)
	}
	if !strings.Contains(p, "check_inbox") {
		t.Fatalf("prompt lost the check_inbox instruction:\n%s", p)
	}
}

func TestCodexWakeWriteRootsNoneByDefault(t *testing.T) {
	r := runCodexWake(t, t.TempDir())
	if got := r.flagValues("--add-dir"); len(got) != 0 {
		t.Fatalf("--add-dir without TINCAN_CODEX_WRITE_ROOTS: %q", got)
	}
}

func TestCodexWakeWriteRootsDefaultAllowedRoots(t *testing.T) {
	home := t.TempDir()
	inCode := filepath.Join(home, "code", "proj")
	inDocs := filepath.Join(home, "Documents", "Codex", "thread")
	elsewhere := filepath.Join(home, "private")
	mkdirs(t, inCode, inDocs, elsewhere)

	r := runCodexWake(t, home,
		"TINCAN_CODEX_WRITE_ROOTS="+inCode+":"+elsewhere+":"+inDocs)
	got := r.flagValues("--add-dir")
	want := []string{canonical(t, inCode), canonical(t, inDocs)}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("--add-dir = %q, want %q\nstderr:\n%s", got, want, r.stderr)
	}
	if !strings.Contains(r.stderr, elsewhere) {
		t.Fatalf("refused root %s not reported on stderr:\n%s", elsewhere, r.stderr)
	}
}

func TestCodexWakeWriteRootsRefusesEscapes(t *testing.T) {
	home := t.TempDir()
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	proj := filepath.Join(allowed, "proj with space")
	outside := filepath.Join(base, "outside")
	sibling := filepath.Join(base, "allowed-evil")
	mkdirs(t, proj, outside, sibling)
	escape := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	// A symlink that stays inside the allowed root is fine: it resolves there.
	inside := filepath.Join(allowed, "inside")
	if err := os.Symlink(proj, inside); err != nil {
		t.Fatal(err)
	}

	roots := []string{
		proj,
		outside,                               // plainly outside
		escape,                                // symlink under the root pointing out
		allowed + "/../outside",               // ../ trick
		sibling,                               // shares the root's prefix but is not under it
		"relative/path",                       // not absolute
		filepath.Join(allowed, "missing"),     // does not exist
		inside,                                // symlink resolving inside
		"",                                    // empty entry is ignored
		filepath.Join(allowed, "escape", "."), // symlink escape with a trailing component
	}
	r := runCodexWake(t, home,
		"TINCAN_CODEX_ALLOWED_ROOTS="+allowed,
		"TINCAN_CODEX_WRITE_ROOTS="+strings.Join(roots, ":"))

	got := r.flagValues("--add-dir")
	want := []string{canonical(t, proj), canonical(t, proj)}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("--add-dir = %q, want %q\nstderr:\n%s", got, want, r.stderr)
	}
	for _, refused := range []string{outside, escape, allowed + "/../outside", sibling, "relative/path"} {
		if !strings.Contains(r.stderr, refused) {
			t.Errorf("refused root %q not reported on stderr:\n%s", refused, r.stderr)
		}
	}
	outsideCanon := canonical(t, outside)
	for _, a := range got {
		if strings.HasPrefix(a, outsideCanon) {
			t.Fatalf("root outside the allowed list became --add-dir: %q", got)
		}
	}
}
