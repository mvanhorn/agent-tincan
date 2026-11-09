//go:build !unix

package cli

import (
	"errors"
	"os/exec"
)

// checkWorkerPlatform refuses per-request workers where there is no sh to
// run them and no process group to stop them.
func checkWorkerPlatform() error {
	return errors.New("tincan listen --per-request needs a unix shell and process groups, so it does not run on this platform")
}

func setWorkerProcessGroup(*exec.Cmd) {}

func killWorkerGroup(*exec.Cmd) {}
