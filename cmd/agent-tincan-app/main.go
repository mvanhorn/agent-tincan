// Command agent-tincan-app is the executable inside Agent Tincan.app. Every
// tincan LaunchAgent on macOS starts it with the tincan binary and that
// binary's arguments:
//
//	agent-tincan /path/to/tincan history serve ...
//
// Because the job's program lives inside the app, macOS lists the job in
// Login Items as "Agent Tincan" with the app's icon instead of under the
// signing certificate's personal name. The launcher then replaces itself
// with tincan (exec, same pid), so the service runs exactly as before.
//
// The launcher runs only a binary signed with the Agent Tincan Developer ID,
// so no other program can borrow the name. Opened from Finder with no
// arguments, it does nothing.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mvanhorn/agent-tincan/internal/macapp"
)

func main() {
	os.Exit(run(os.Args, codesignVerify, syscall.Exec, os.Stderr))
}

// run checks the target named by args[1] and execs it with args[1:]. It
// returns only when it does not exec: 0 with no target, 1 on a refusal.
func run(args []string, verify func(string) error, execve func(string, []string, []string) error, stderr io.Writer) int {
	if len(args) < 2 {
		return 0
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agent-tincan: "+format+"\n", a...)
		return 1
	}
	target := args[1]
	if !filepath.IsAbs(target) {
		return fail("%s: the tincan binary must be an absolute path", target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fail("%v", err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return fail("%v", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fail("%s is not an executable file", resolved)
	}
	if err := verify(resolved); err != nil {
		return fail("refusing to run %s: %v", resolved, err)
	}
	argv := append([]string{resolved}, args[2:]...)
	if err := execve(resolved, argv, os.Environ()); err != nil {
		return fail("exec %s: %v", resolved, err)
	}
	return 0
}

// codesignVerify checks path against macapp.TincanRequirement.
func codesignVerify(path string) error {
	if err := macapp.VerifyTincan(path); err != nil {
		return fmt.Errorf("not signed by Agent Tincan (%v)", err)
	}
	return nil
}
