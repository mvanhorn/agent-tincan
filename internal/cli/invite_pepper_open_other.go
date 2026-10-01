//go:build !darwin && !linux

package cli

import (
	"errors"
	"fmt"
	"os"
)

func openInvitePepperFile(path string) (*os.File, error) {
	// Release targets are Linux and macOS. Do not silently fall back to a
	// path-check-then-open sequence on platforms without this implementation.
	return nil, fmt.Errorf("secure invite-pepper file opening: %w", errors.ErrUnsupported)
}
