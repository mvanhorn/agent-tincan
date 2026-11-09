package notes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// SpoolEntry is one claimed add that has not been answered yet. It holds
// the whole request and its parsed form, so a retry needs neither the
// relay nor the extractor. Once the add is decided (saved, declined, or
// failed), Outcome and Reply record that decision before the reply is
// sent, and a retry only resends it. An entry without an Outcome,
// including one written before the field existed, is undecided.
//
// A computed search or read reply is a SpoolEntry too, kept in the
// replies subdirectory with its Op set and an answered, failed or
// declined Outcome. Its Reply holds the reply text, note text included,
// until it is delivered or purged.
type SpoolEntry struct {
	Request envelope.Request `json:"request"`
	// Op is the operation a stored reply answers; empty for an add, as in
	// entries written before stored replies.
	Op        Op        `json:"op,omitempty"`
	Note      Request   `json:"note"`
	SpooledAt time.Time `json:"spooled_at"`
	Attempts  int       `json:"attempts,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Outcome   Outcome   `json:"outcome,omitempty"`
	NoteID    string    `json:"note_id,omitempty"`
	Reply     string    `json:"reply,omitempty"`
	// Refused marks a reply the relay refused as malformed or too large
	// and that was replaced with a short failed reply, so a second refusal
	// is not replaced again.
	Refused bool `json:"refused,omitempty"`
}

// Outcome is the final decision on a spooled add.
type Outcome string

const (
	OutcomeAnswered Outcome = "answered" // a stored search or read reply
	OutcomeSaved    Outcome = "saved"
	OutcomeDeclined Outcome = "declined"
	OutcomeFailed   Outcome = "failed"
)

// status is the reply status for o; false for an undecided or unknown
// outcome.
func (o Outcome) status() (envelope.Status, bool) {
	switch o {
	case OutcomeSaved, OutcomeAnswered:
		return envelope.StatusAnswered, true
	case OutcomeDeclined:
		return envelope.StatusDeclined, true
	case OutcomeFailed:
		return envelope.StatusFailed, true
	}
	return "", false
}

// Spool is a directory of SpoolEntry files, one per request id. Every
// write is fsynced (file and directory) before Put returns, so an entry
// that Put accepted survives a crash or power loss.
type Spool struct {
	Dir string
}

var spoolIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

const spoolExt = ".json"

// repliesDir is the spool subdirectory for stored search and read
// replies. A build that predates it lists only top-level files, so it
// never mistakes a stored reply for an add.
const repliesDir = "replies"

// Replies is the spool of stored search and read replies under s.
func (s *Spool) Replies() *Spool { return &Spool{Dir: filepath.Join(s.Dir, repliesDir)} }

func (s *Spool) path(id string) (string, error) {
	if !spoolIDRE.MatchString(id) {
		return "", fmt.Errorf("spool: request id %q is not safe as a file name", id)
	}
	return filepath.Join(s.Dir, id+spoolExt), nil
}

// Put writes e durably, replacing any entry for the same request.
func (s *Spool) Put(e SpoolEntry) error {
	path, err := s.path(e.Request.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(s.Dir)
}

// Get returns the entry for id, if there is one.
func (s *Spool) Get(id string) (SpoolEntry, bool, error) {
	path, err := s.path(id)
	if err != nil {
		return SpoolEntry{}, false, err
	}
	e, err := readEntry(path)
	if errors.Is(err, os.ErrNotExist) {
		return SpoolEntry{}, false, nil
	}
	if err != nil {
		return SpoolEntry{}, false, err
	}
	return e, true, nil
}

// Remove deletes the entry for id; a missing entry is not an error.
func (s *Spool) Remove(id string) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(s.Dir)
}

// List returns every readable entry, oldest first. Entries that cannot be
// read are left on disk and named in the error, which never hides the
// readable ones.
func (s *Spool) List() ([]SpoolEntry, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SpoolEntry
	var bad []error
	for _, d := range ents {
		name := d.Name()
		if d.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, spoolExt) {
			continue
		}
		e, err := readEntry(filepath.Join(s.Dir, name))
		if err != nil {
			bad = append(bad, fmt.Errorf("spool entry %s: %w", name, err))
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SpooledAt.Before(out[j].SpooledAt) })
	return out, errors.Join(bad...)
}

func readEntry(path string) (SpoolEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SpoolEntry{}, err
	}
	var e SpoolEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return SpoolEntry{}, err
	}
	if e.Request.ID+spoolExt != filepath.Base(path) {
		return SpoolEntry{}, errors.New("request id does not match the file name")
	}
	return e, nil
}

// syncDir fsyncs dir so a rename or remove in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
