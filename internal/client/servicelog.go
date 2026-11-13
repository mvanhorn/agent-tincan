package client

import (
	"io"
	"strings"
	"sync"
	"time"
)

// LogPrefix starts each line a long-running process logs: the time t in
// the LogStamp layout and a space, or nothing when stderr is the systemd
// journal, which stamps every line itself.
func LogPrefix(t time.Time) string {
	if stderrIsJournal() {
		return ""
	}
	return t.Format(LogStamp) + " "
}

// LogLines is msg as log lines: LogPrefix(t) starts every line of it, and
// it ends with a newline.
func LogLines(t time.Time, msg string) string {
	prefix := LogPrefix(t)
	return prefix + strings.ReplaceAll(strings.TrimSuffix(msg, "\n"), "\n", "\n"+prefix) + "\n"
}

// NewLogWriter returns a writer to w that starts each line with
// LogPrefix(now()). Give it to Go's log package with no flags, so a line
// carries one time, in the same layout as every other service log line.
func NewLogWriter(w io.Writer, now func() time.Time) io.Writer {
	return &logWriter{w: w, now: now, lineStart: true}
}

type logWriter struct {
	mu        sync.Mutex
	w         io.Writer
	now       func() time.Time
	lineStart bool
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	prefix := ""
	for rest := string(p); rest != ""; {
		if l.lineStart {
			if prefix == "" {
				prefix = LogPrefix(l.now())
			}
			b.WriteString(prefix)
		}
		line, after, found := strings.Cut(rest, "\n")
		b.WriteString(line)
		if found {
			b.WriteByte('\n')
		}
		l.lineStart, rest = found, after
	}
	if _, err := io.WriteString(l.w, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}
