//go:build unix

package client

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// stderrToFile points os.Stderr at a new file for the rest of the test and
// returns the file's device and inode.
func stderrToFile(t *testing.T) (dev, ino uint64) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = old; f.Close() })
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	return uint64(st.Dev), uint64(st.Ino)
}

// Under the systemd journal, which stamps every line itself, a service
// adds no stamp of its own.
func TestLogPrefixIsEmptyWhenStderrIsTheJournal(t *testing.T) {
	dev, ino := stderrToFile(t)
	t.Setenv("JOURNAL_STREAM", fmt.Sprintf("%d:%d", dev, ino))
	if got := LogPrefix(logTestTime); got != "" {
		t.Fatalf("prefix under the journal = %q", got)
	}
	if got := LogLines(logTestTime, "a\nb"); got != "a\nb\n" {
		t.Fatalf("lines under the journal = %q", got)
	}
}

// JOURNAL_STREAM survives into children whose stderr is somewhere else;
// it only counts when it names stderr itself.
func TestLogPrefixStampsWhenJournalStreamNamesAnotherFile(t *testing.T) {
	dev, ino := stderrToFile(t)
	t.Setenv("JOURNAL_STREAM", fmt.Sprintf("%d:%d", dev, ino+1))
	if got := LogPrefix(logTestTime); got != logTestStamp {
		t.Fatalf("prefix = %q, want %q", got, logTestStamp)
	}
}
