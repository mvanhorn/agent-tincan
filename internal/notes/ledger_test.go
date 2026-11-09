package notes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLedgerRecordsPendingThenDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adds.json")
	l := newLedger(path, nil)
	start := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if err := l.Begin("tincan:r1", "Tent", start, map[string]string{"remote.agent": "grokbot"}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := l.Lookup("tincan:r1")
	if err != nil || !ok || e.State != ledgerPending || !e.Start.Equal(start) || e.Title != "Tent" {
		t.Fatalf("after Begin: %+v %v %v", e, ok, err)
	}
	if err := l.Done("tincan:r1", "N-1"); err != nil {
		t.Fatal(err)
	}
	// A new ledger over the same file, as after a restart, sees it.
	e, ok, err = newLedger(path, nil).Lookup("tincan:r1")
	if err != nil || !ok || e.State != ledgerDone || e.NoteID != "N-1" || e.Props["remote.agent"] != "grokbot" {
		t.Fatalf("after Done and reopen: %+v %v %v", e, ok, err)
	}
	if _, ok, _ := l.Lookup("tincan:other"); ok {
		t.Fatal("an unknown key must not be found")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}
}

func TestCorruptLedgerIsMovedAsideAndStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adds.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	l := newLedger(path, func(f string, a ...any) { logged.WriteString(f) })
	if _, ok, err := l.Lookup("k"); err != nil || ok {
		t.Fatalf("Lookup on a corrupt ledger = %v, %v; want empty", ok, err)
	}
	if b, err := os.ReadFile(path + ".corrupt"); err != nil || string(b) != "{not json" {
		t.Fatalf("corrupt copy = %q, %v", b, err)
	}
	if !strings.Contains(logged.String(), "did not parse") {
		t.Fatalf("log = %q, want the corruption logged", logged.String())
	}
	if err := l.Begin("k", "T", time.Now(), nil); err != nil {
		t.Fatalf("Begin after recovery: %v", err)
	}
}

// A pending add is never pruned, however old: its add may still be in the
// spool after a long outage, and the entry is what stops a second note. Only
// done entries past ledgerKeep go.
func TestLedgerNeverPrunesAPendingAdd(t *testing.T) {
	l := newLedger(filepath.Join(t.TempDir(), "adds.json"), nil)
	old := time.Now().Add(-2 * ledgerKeep)
	if err := l.Begin("tincan:waiting", "Tent", old, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Begin("tincan:saved", "Lamp", old, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Done("tincan:saved", "N-1"); err != nil {
		t.Fatal(err)
	}
	if err := l.Begin("tincan:new", "Map", time.Now(), nil); err != nil { // any write prunes
		t.Fatal(err)
	}
	if e, ok, err := l.Lookup("tincan:waiting"); err != nil || !ok || e.State != ledgerPending {
		t.Fatalf("an old pending add was pruned: %+v %v %v", e, ok, err)
	}
	if _, ok, _ := l.Lookup("tincan:saved"); ok {
		t.Fatal("a done entry past ledgerKeep was kept")
	}
}
