package cli

import (
	"io"
	"testing"
	"unicode"
	"unicode/utf8"
)

// fakeTerminal makes the writers for which isTerm returns true count as a
// terminal for the rest of the test.
func fakeTerminal(t *testing.T, isTerm func(io.Writer) bool) {
	t.Helper()
	old := writerIsTerminal
	writerIsTerminal = isTerm
	t.Cleanup(func() { writerIsTerminal = old })
}

// everyWriterIsTerminal makes every writer count as a terminal.
func everyWriterIsTerminal(io.Writer) bool { return true }

// noWriterIsTerminal makes no writer count as a terminal.
func noWriterIsTerminal(io.Writer) bool { return false }

// assertTerminalSafe fails when s holds a control rune other than a newline
// or tab, a bidi control, or invalid UTF-8.
func assertTerminalSafe(t *testing.T, what, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatalf("%s split a character: %q", what, s)
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			t.Fatalf("%s kept %U: %q", what, r, s)
		}
	}
}

func TestForTerminalOnlyChangesTerminalOutput(t *testing.T) {
	const s = "a\x1b]0;pwned\x07\u202eb\n\tc"
	fakeTerminal(t, everyWriterIsTerminal)
	if got := forTerminal(io.Discard, s); got != "a ]0;pwned  b\n\tc" {
		t.Fatalf("terminal = %q", got)
	}
	fakeTerminal(t, noWriterIsTerminal)
	if got := forTerminal(io.Discard, s); got != s {
		t.Fatalf("not a terminal = %q", got)
	}
}
