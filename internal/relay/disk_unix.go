//go:build unix

package relay

import "syscall"

// freeDiskBytes reports the bytes available to the relay under dir.
func freeDiskBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
