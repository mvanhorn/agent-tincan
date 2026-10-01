//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
)

func openInvitePepperFile(path string) (*os.File, error) {
	// O_NOFOLLOW binds refusal to the actual open, not a prior path check.
	// O_NONBLOCK prevents a non-regular file from blocking before f.Stat
	// can reject it. It has no effect on regular-file reads.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
