package client

import (
	"fmt"
	"net"
	"runtime"
)

// MaxUnixSocketPath is the longest unix socket path this OS accepts: the
// sun_path field holds 104 bytes on macOS and the BSDs and 108 on Linux,
// including the trailing NUL.
func MaxUnixSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// ListenUnix listens on the unix socket at path. A path longer than the OS
// allows fails with a clear "socket path too long" error instead of the
// kernel's "bind: invalid argument".
func ListenUnix(path string) (net.Listener, error) {
	if max := MaxUnixSocketPath(); len(path) > max {
		return nil, fmt.Errorf("socket path too long (%d bytes, max %d on %s): %s; use a shorter directory", len(path), max, runtime.GOOS, path)
	}
	return net.Listen("unix", path)
}
