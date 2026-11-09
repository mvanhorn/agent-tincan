package notes

import (
	"context"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// stubBackend is a Backend whose answers a test sets directly.
type stubBackend struct {
	name      string
	readNote  Note
	readErr   error
	searchErr error
	createErr error
	probeN    int
	probeErr  error
}

func (b *stubBackend) Name() string { return b.name }

func (b *stubBackend) Create(context.Context, string, string, []string, map[string]string, string) (CreateResult, error) {
	return CreateResult{}, b.createErr
}

func (b *stubBackend) Search(context.Context, string) ([]NoteSummary, error) {
	return nil, b.searchErr
}

func (b *stubBackend) Read(context.Context, string) (Note, error) { return b.readNote, b.readErr }

func (b *stubBackend) Probe(context.Context) (int, error) { return b.probeN, b.probeErr }

// rigWith builds the usual test rig with backend in place of the helper.
func rigWith(t *testing.T, backend Backend) *rig {
	t.Helper()
	r := newRig(t, relay.Config{})
	r.cfg.Backend = backend
	r.restart(t)
	return r
}

func TestLockedNoteReadSaysLockedAndKeepsHealthOK(t *testing.T) {
	r := rigWith(t, &stubBackend{name: "Bear", readNote: Note{Summary: NoteSummary{ID: "0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C", State: StateActive}}, readErr: ErrNoteLocked})
	res := r.ask(t, "grokbot", `note: {"op":"read","id":"0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C"}`)
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "is locked") {
		t.Fatalf("reply = %s %q, want the locked reply", res.Status, res.Reply.Body)
	}
	if h := r.svc.readHealth(); !h.OK {
		t.Fatalf("health = %+v, want OK: a locked note is a working backend", h)
	}
}

func TestLockedNoteInTrashReadsAsNotFound(t *testing.T) {
	r := rigWith(t, &stubBackend{name: "Bear", readNote: Note{Summary: NoteSummary{ID: "0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C", State: "trashed"}}, readErr: ErrNoteLocked})
	res := r.ask(t, "grokbot", `note: {"op":"read","id":"0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C"}`)
	if !strings.Contains(res.Reply.Body, "No note with id 0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C was found") {
		t.Fatalf("reply = %q, want not found", res.Reply.Body)
	}
}

func TestRepliesNameTheChosenBackend(t *testing.T) {
	fail := &HelperError{Code: codeUnavailable, Message: "Bear is not installed"}
	r := rigWith(t, &stubBackend{name: "Bear", searchErr: fail, createErr: &HelperError{Code: "invalid_request", Message: "bad"}})
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	if !strings.Contains(res.Reply.Body, "could not reach Bear") || strings.Contains(res.Reply.Body, "Agent Notes") {
		t.Fatalf("search reply = %q, want it to name Bear only", res.Reply.Body)
	}
	res = r.ask(t, "grokbot", addBody("Tent", "stakes"))
	if !strings.Contains(res.Reply.Body, "Bear rejected it") || strings.Contains(res.Reply.Body, "Agent Notes") {
		t.Fatalf("add reply = %q, want it to name Bear only", res.Reply.Body)
	}
}

func TestBackendNamesAreChecked(t *testing.T) {
	for _, n := range BackendNames {
		if err := CheckBackendName(n); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	err := CheckBackendName("nope")
	if err == nil || !strings.Contains(err.Error(), "agent-notes, bear, apple-notes") {
		t.Fatalf("CheckBackendName(nope) = %v, want the three names listed", err)
	}
}

func TestReadAcceptsEachAppsNoteIDs(t *testing.T) {
	for id, ok := range map[string]bool{
		"0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C":                             true,
		"x-coredata://5A6B7C8D-1E2F-4A5B-8C9D-0E1F2A3B4C5D/ICNote/p1234":   true,
		"x-coredata://5A6B7C8D-1E2F-4A5B-8C9D-0E1F2A3B4C5D/ICFolder/p1234": false,
		"x-coredata://nope/ICNote/p1":                                      false,
		"../../etc/passwd":                                                 false,
	} {
		if err := Validate(Request{Op: OpRead, ID: id}); (err == nil) != ok {
			t.Errorf("Validate(read %q) = %v, want ok=%v", id, err, ok)
		}
	}
}

func TestUnconfirmedAddTellsTheAskerToCheckFirst(t *testing.T) {
	r := rigWith(t, &stubBackend{name: "Bear", createErr: &HelperError{Code: codeCreateUnconfirmed, Message: "two candidates"}})
	res := r.ask(t, "grokbot", addBody("Tent", "stakes"))
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, `Check Bear for a note titled "Tent"`) || strings.Contains(res.Reply.Body, "Nothing was written") {
		t.Fatalf("reply = %s %q", res.Status, res.Reply.Body)
	}
	r.assertSpoolEmpty(t)
}

// The service records its own access check for doctor, and checks again
// when a request is refused for lack of permission.
func TestServiceRecordsItsAccessCheck(t *testing.T) {
	b := &stubBackend{name: "Bear", probeN: 3}
	r := rigWith(t, b)
	r.svc.recordProbe(t.Context())
	if h := r.svc.readHealth(); h.Backend != "Bear" || h.Probe == nil || h.Probe.Count != 3 || h.Probe.Code != "" {
		t.Fatalf("health after a good check = %+v %+v", h, h.Probe)
	}
	denied := &HelperError{Code: codePermissionDenied, Message: "macOS refused access"}
	b.searchErr, b.probeErr = denied, denied
	r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	h := r.svc.readHealth()
	if h.Probe == nil || h.Probe.Code != codePermissionDenied || h.Probe.Count != 0 {
		t.Fatalf("probe after a refused search = %+v", h.Probe)
	}
	if h.Command != "search" || h.Code != codePermissionDenied {
		t.Fatalf("the access check overwrote the last result: %+v", h)
	}
}
