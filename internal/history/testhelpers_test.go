package history

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// fixtureDB creates a SQLite database at path in WAL mode and runs stmts
// in it. The connection stays open until the test ends and automatic
// checkpoints are off, so everything written is only in the -wal file,
// as with a store the app still holds open.
func fixtureDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, s := range append([]string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0"}, stmts...) {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// sqlQuote is a SQL string literal for fixture statements.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// emptyDir fails the test unless dir has no entries.
func emptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatalf("%s holds %d entries after the read, want none (first %s)", dir, len(entries), entries[0].Name())
	}
}

// fixtureTurn is one prompt and reply for a fixture session.
type fixtureTurn struct {
	prompt, reply string
	at            time.Time
}
