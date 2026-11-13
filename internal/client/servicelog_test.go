package client

import (
	"bytes"
	"log"
	"testing"
	"time"
)

var logTestTime = time.Date(2026, 11, 8, 21, 55, 7, 137_000_000, time.FixedZone("PST", -8*3600))

const logTestStamp = "2026-11-08T21:55:07.137-08:00 "

func TestLogPrefixIsTheStampOutsideTheJournal(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	if got := LogPrefix(logTestTime); got != logTestStamp {
		t.Fatalf("prefix = %q, want %q", got, logTestStamp)
	}
	t.Setenv("JOURNAL_STREAM", "not a stream")
	if got := LogPrefix(logTestTime); got != logTestStamp {
		t.Fatalf("prefix with a garbage JOURNAL_STREAM = %q", got)
	}
}

func TestLogLinesStampsEveryLine(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	got := LogLines(logTestTime, "tincan pass: first\nsecond")
	if want := logTestStamp + "tincan pass: first\n" + logTestStamp + "second\n"; got != want {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

// Go's std log, sent through the log writer with no flags of its own,
// writes exactly one stamp per line and none of its own date layout.
func TestLogWriterStampsStdLogLines(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	var buf bytes.Buffer
	l := log.New(NewLogWriter(&buf, func() time.Time { return logTestTime }), "", 0)
	l.Printf("tincan relay serving on %s", "100.64.0.1:8080")
	l.Printf("rebind: muse moved\nfrom a to b")
	want := logTestStamp + "tincan relay serving on 100.64.0.1:8080\n" +
		logTestStamp + "rebind: muse moved\n" + logTestStamp + "from a to b\n"
	if got := buf.String(); got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

// A line that arrives in pieces is stamped once, at its start.
func TestLogWriterStampsALineWrittenInPiecesOnce(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	var buf bytes.Buffer
	w := NewLogWriter(&buf, func() time.Time { return logTestTime })
	for _, p := range []string{"one ", "line\nand", " the next\n"} {
		if n, err := w.Write([]byte(p)); err != nil || n != len(p) {
			t.Fatalf("write %q = %d, %v", p, n, err)
		}
	}
	if got, want := buf.String(), logTestStamp+"one line\n"+logTestStamp+"and the next\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}
