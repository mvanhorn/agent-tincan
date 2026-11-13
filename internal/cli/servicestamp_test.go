package cli

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func TestServiceStampIsTheServiceLogLayout(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	s := serviceStamp()
	if !strings.HasSuffix(s, " ") {
		t.Fatalf("stamp %q must end with a space", s)
	}
	if _, err := time.Parse(client.LogStamp, strings.TrimSuffix(s, " ")); err != nil {
		t.Fatalf("stamp %q does not parse as client.LogStamp: %v", s, err)
	}
}

// stdLogDate is the date Go's std log writes with its default flags.
var stdLogDate = regexp.MustCompile(`\d{4}/\d\d/\d\d`)

// assertStampedLines fails unless every line of out starts with one
// client.LogStamp time and a space, carries no second time, and the lines
// after their stamps are want.
func assertStampedLines(t *testing.T, out string, want ...string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("log = %q, want %d lines", out, len(want))
	}
	for i, l := range lines {
		stamp, rest, _ := strings.Cut(l, " ")
		if _, err := time.Parse(client.LogStamp, stamp); err != nil {
			t.Fatalf("line %q does not start with a stamp: %v", l, err)
		}
		first, _, _ := strings.Cut(rest, " ")
		if _, err := time.Parse(client.LogStamp, first); err == nil || stdLogDate.MatchString(rest) {
			t.Fatalf("line %q carries two times", l)
		}
		if rest != want[i] {
			t.Fatalf("line %q, want %q after the stamp", l, want[i])
		}
	}
}

// swapStd points os.Stdout and os.Stderr at files for the rest of the test
// and returns their paths.
func swapStd(t *testing.T) (stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	stdout, stderr = filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
	out, err := os.Create(stdout)
	if err != nil {
		t.Fatal(err)
	}
	errf, err := os.Create(stderr)
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, errf
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		out.Close()
		errf.Close()
	})
	return stdout, stderr
}

// A long-running command's std log lines go to stderr with one service
// stamp each, never to stdout, which is a protocol channel for tincan mcp
// and the native host. The previous log setup comes back afterwards.
func TestServiceLogStampsStdLogOnStderr(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	stdout, stderr := swapStd(t)
	oldFlags := log.Flags()
	restore := useServiceLog()
	log.Printf("tincan relay serving on %s", "100.64.0.1:8080")
	log.Printf("rebind: muse moved\nfrom a to b")
	restore()
	if log.Flags() != oldFlags {
		t.Fatalf("flags = %d after restore, want %d", log.Flags(), oldFlags)
	}
	got, err := os.ReadFile(stderr)
	if err != nil {
		t.Fatal(err)
	}
	assertStampedLines(t, string(got), "tincan relay serving on 100.64.0.1:8080", "rebind: muse moved", "from a to b")
	if b, _ := os.ReadFile(stdout); len(b) != 0 {
		t.Fatalf("log wrote to stdout: %q", b)
	}
}
