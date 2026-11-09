package notes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Backend is a notes app the service reads and writes: the Agent Notes
// helper, Bear, or Apple Notes. Every backend returns notes in any state
// (active, trashed, archived); the service decides what an asker may see.
type Backend interface {
	// Name is the app's name as the owner knows it, used in replies,
	// progress notes and logs.
	Name() string
	// Create adds a note, or returns the note an earlier create with the
	// same idempotency key made (Existed set).
	Create(ctx context.Context, title, body string, tags []string, props map[string]string, key string) (CreateResult, error)
	// Search returns the notes whose title or text holds every word of
	// query, in any state.
	Search(ctx context.Context, query string) ([]NoteSummary, error)
	// Read returns one note by id, in any state: ErrNoteNotFound when there
	// is none, ErrNoteLocked when it is locked or encrypted.
	Read(ctx context.Context, id string) (Note, error)
	// Probe checks that the backend can be read from this process and
	// returns how many notes it holds. It never returns note text.
	Probe(ctx context.Context) (int, error)
}

// ErrNoteLocked is a read of a note the owner has locked or encrypted. Its
// text is never read.
var ErrNoteLocked = errors.New("note is locked")

// Backend names for --backend.
const (
	BackendAgentNotes = "agent-notes"
	BackendBear       = "bear"
	BackendAppleNotes = "apple-notes"
)

// BackendNames lists the accepted --backend values, the default first.
var BackendNames = []string{BackendAgentNotes, BackendBear, BackendAppleNotes}

// CheckBackendName reports an unknown --backend value.
func CheckBackendName(name string) error {
	if slices.Contains(BackendNames, name) {
		return nil
	}
	return fmt.Errorf("unknown notes backend %q: use one of %s", name, strings.Join(BackendNames, ", "))
}

// codePermissionDenied is a backend macOS will not let this process use
// until the owner grants it access. It is transient: the add is kept and
// retried once access is granted.
const codePermissionDenied = "permission_denied"

// codeUnavailable is a backend that is not installed or cannot be reached
// right now. It is transient.
const codeUnavailable = "helper_unavailable"
