package notes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ledger remembers each add a backend without idempotency keys has made,
// by key, so a redelivered add never writes a second note. An entry is
// written as pending before the app is asked to create the note, and marked
// done with the note's id once the note is found. A retry that finds a
// pending entry looks for that attempt's note instead of creating again.
type ledger struct {
	path string
	logf func(format string, args ...any)

	mu sync.Mutex
}

// ledgerEntry is one add. Props are the provenance properties the app has
// no field for; they are kept here only.
type ledgerEntry struct {
	State  string            `json:"state"` // ledgerPending or ledgerDone
	Start  time.Time         `json:"start"`
	Title  string            `json:"title"`
	NoteID string            `json:"note_id,omitempty"`
	Props  map[string]string `json:"props,omitempty"`
}

const (
	ledgerPending = "pending"
	ledgerDone    = "done"
)

// ledgerPath is the ledger's name in the service's Application Support
// root, one per backend.
func ledgerPath(appSupport, backend string) string {
	return filepath.Join(appSupport, "tincan-notes-"+backend+"-adds.json")
}

func newLedger(path string, logf func(string, ...any)) *ledger {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &ledger{path: path, logf: logf}
}

// Lookup returns the entry for key, if any.
func (l *ledger) Lookup(key string) (ledgerEntry, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, err := l.load()
	if err != nil {
		return ledgerEntry{}, false, err
	}
	e, ok := m[key]
	return e, ok, nil
}

// Begin records a pending add for key, started at start.
func (l *ledger) Begin(key, title string, start time.Time, props map[string]string) error {
	return l.update(key, ledgerEntry{State: ledgerPending, Start: start.UTC(), Title: title, Props: props})
}

// Done records that key's add made the note id.
func (l *ledger) Done(key, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, err := l.load()
	if err != nil {
		return err
	}
	e := m[key]
	e.State, e.NoteID = ledgerDone, id
	m[key] = e
	return l.save(m)
}

// Forget drops key, for an add the app was never asked to make.
func (l *ledger) Forget(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, err := l.load()
	if err != nil {
		return err
	}
	delete(m, key)
	return l.save(m)
}

func (l *ledger) update(key string, e ledgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, err := l.load()
	if err != nil {
		return err
	}
	m[key] = e
	return l.save(m)
}

// load reads the ledger. A file that does not parse is moved aside as
// .corrupt and logged, and the ledger starts empty: losing it only risks a
// duplicate on a later redelivery, while refusing every add would lose
// them all.
func (l *ledger) load() (map[string]ledgerEntry, error) {
	b, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]ledgerEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("add ledger: %w", err)
	}
	m := map[string]ledgerEntry{}
	if err := json.Unmarshal(b, &m); err != nil {
		bad := l.path + ".corrupt"
		if rerr := os.Rename(l.path, bad); rerr != nil {
			return nil, fmt.Errorf("add ledger %s does not parse and could not be moved aside: %w", l.path, rerr)
		}
		l.logf("add ledger %s did not parse (%v); moved it to %s and started a new one", l.path, err, bad)
		return map[string]ledgerEntry{}, nil
	}
	return m, nil
}

// ledgerKeep is how long an entry is kept. A request is redelivered for
// days at most, so older entries can no longer stop a duplicate.
const ledgerKeep = 30 * 24 * time.Hour

func (l *ledger) save(m map[string]ledgerEntry) error {
	for k, e := range m {
		if time.Since(e.Start) > ledgerKeep {
			delete(m, k)
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(l.path, append(b, '\n')); err != nil {
		return fmt.Errorf("add ledger: %w", err)
	}
	return nil
}
