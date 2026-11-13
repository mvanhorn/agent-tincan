package cli

import (
	"log"
	"os"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// serviceStamp starts a line a serve command writes to its log with the
// local time, in the same layout the services use (client.LogStamp), so a
// restart shows when it happened. Under the systemd journal it is empty:
// the journal stamps every line itself.
func serviceStamp() string {
	return client.LogPrefix(time.Now())
}

// useServiceLog sends Go's std log to stderr with one service stamp per
// line, in place of its own date and time, for a long-running command, and
// returns the func that puts the previous setup back. It never writes to
// stdout, which is a protocol channel for tincan mcp and the native host.
func useServiceLog() (restore func()) {
	w, flags, prefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(client.NewLogWriter(os.Stderr, time.Now))
	log.SetFlags(0)
	log.SetPrefix("")
	return func() {
		log.SetOutput(w)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	}
}
