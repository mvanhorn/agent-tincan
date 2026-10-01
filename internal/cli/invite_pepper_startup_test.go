package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// Only disposable test state is used; never print the fixture contents.
func TestInvitePepperConcurrentFirstStart(t *testing.T) {
	old := runtime.GOMAXPROCS(8)
	defer runtime.GOMAXPROCS(old)
	const workers = 32
	failures, mismatches := 0, 0
	for range 12 {
		dir := t.TempDir()
		start := make(chan struct{})
		values := make([][]byte, workers)
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				values[i], errs[i] = loadOrCreateInvitePepper(dir)
			}(i)
		}
		close(start)
		wg.Wait()
		stored, err := os.ReadFile(filepath.Join(dir, "invite-pepper"))
		if err != nil {
			t.Fatal(err)
		}
		for i := range workers {
			if errs[i] != nil {
				failures++
				continue
			}
			if !bytes.Equal(values[i], stored) {
				mismatches++
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "invite-pepper" {
			t.Fatal("initialization left temporary files behind")
		}
	}
	if failures != 0 || mismatches != 0 {
		t.Fatalf("simultaneous startup: initialization errors=%d, mismatched results=%d", failures, mismatches)
	}
}

func TestInvitePepperRestartPreservesFile(t *testing.T) {
	dir := t.TempDir()
	a, err := loadOrCreateInvitePepper(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "invite-pepper")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadOrCreateInvitePepper(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 || !bytes.Equal(a, b) || !os.SameFile(before, after) {
		t.Fatal("restart changed the existing file or value")
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != 0600 {
		t.Fatal("unexpected file permissions")
	}
}

func TestInvitePepperIncompleteFileRemainsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invite-pepper")
	fixture := []byte("incomplete-test-fixture")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateInvitePepper(dir); err == nil {
		t.Fatal("incomplete pre-existing file accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, fixture) {
		t.Fatal("invalid pre-existing file overwritten")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("failed validation created temporary files")
	}
}
