package cli

import (
	"io"
	"os"

	"golang.org/x/term"
)

// writerIsTerminal reports whether w is a terminal. Tests replace it to
// answer per writer.
var writerIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// forTerminal is s as it should be written to w. On a terminal, teammate
// and model text is data: control and bidi runes become spaces, keeping the
// newlines and tabs that lay it out, so it cannot drive the terminal. Any
// other writer (a pipe, a file, an agent reading the output) gets s exactly.
func forTerminal(w io.Writer, s string) string {
	if writerIsTerminal(w) {
		return terminalSafeLines(s)
	}
	return s
}
