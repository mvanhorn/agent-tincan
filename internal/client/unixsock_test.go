package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenUnixPathTooLong(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120), "admin.sock")
	_, err := ListenUnix(long)
	if err == nil || !strings.Contains(err.Error(), "socket path too long") || !strings.Contains(err.Error(), long) {
		t.Fatalf("err = %v", err)
	}
	dir, err := os.MkdirTemp("", "tc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := ListenUnix(filepath.Join(dir, "a.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}
