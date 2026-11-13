//go:build unix

package client

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// stderrIsJournal reports whether stderr is the systemd journal, the way
// systemd documents it: JOURNAL_STREAM is "<device>:<inode>" of the stream
// it connected, and only counts when that is still stderr.
func stderrIsJournal() bool {
	dev, ino, ok := strings.Cut(os.Getenv("JOURNAL_STREAM"), ":")
	if !ok {
		return false
	}
	wantDev, err := strconv.ParseUint(dev, 10, 64)
	if err != nil {
		return false
	}
	wantIno, err := strconv.ParseUint(ino, 10, 64)
	if err != nil {
		return false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(os.Stderr.Fd()), &st); err != nil {
		return false
	}
	return uint64(st.Dev) == wantDev && uint64(st.Ino) == wantIno
}
