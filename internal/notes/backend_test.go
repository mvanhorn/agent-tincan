package notes

import (
	"context"
	"strings"
	"testing"
	"time"

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
	creates   int
}

func (b *stubBackend) Name() string { return b.name }

func (b *stubBackend) Create(context.Context, string, string, []string, map[string]string, string) (CreateResult, error) {
	b.creates++
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
		"0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C":                       true,
		"0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C-4242-00000ABCDEF01234": true,
		"SFNote2Intro3":                          true,
		"0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C-x": false,
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
	denied := &HelperError{Code: CodePermissionDenied, Message: "macOS refused access"}
	b.searchErr, b.probeErr = denied, denied
	r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	h := r.svc.readHealth()
	if h.Probe == nil || h.Probe.Code != CodePermissionDenied || h.Probe.Count != 0 {
		t.Fatalf("probe after a refused search = %+v", h.Probe)
	}
	if h.Command != "search" || h.Code != CodePermissionDenied {
		t.Fatalf("the access check overwrote the last result: %+v", h)
	}
}

// An add waiting in the spool when the owner switched apps is not saved to
// the new app: the old app may already hold it.
func TestWaitingAddIsNotMovedToAnotherApp(t *testing.T) {
	b := &stubBackend{name: AppleNotesName}
	r := rigWith(t, b)
	req := r.send(t, "grokbot", addBody("Tent", "stakes"))
	claimed, err := r.cfg.Relay.Claim(t.Context(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := SpoolEntry{Request: claimed, Backend: BearName, Note: Request{Op: OpAdd, Title: "Tent", Body: "stakes"}, SpooledAt: time.Now().UTC()}
	if err := r.svc.spool.Put(e); err != nil {
		t.Fatal(err)
	}
	if n := r.svc.RetrySpooled(t.Context()); n != 1 {
		t.Fatalf("RetrySpooled tried %d", n)
	}
	res := r.get(t, "grokbot", req.ID)
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "saved to Bear") || !strings.Contains(res.Reply.Body, `check there for a note titled "Tent"`) {
		t.Fatalf("reply = %s %q", res.Status, res.Reply.Body)
	}
	if b.creates != 0 {
		t.Fatalf("the add was saved to the new app %d times", b.creates)
	}
	r.assertSpoolEmpty(t)
}

// A failed access check is replaced once the app answers again, so doctor
// does not keep reporting a problem that is gone.
func TestRecoveredAccessIsRecorded(t *testing.T) {
	denied := &HelperError{Code: CodePermissionDenied, Message: "macOS refused access"}
	b := &stubBackend{name: BearName, probeErr: denied}
	r := rigWith(t, b)
	r.svc.recordProbe(t.Context())
	h := r.svc.readHealth()
	if h.Probe == nil || h.Probe.Code != CodePermissionDenied {
		t.Fatalf("probe = %+v", h.Probe)
	}
	b.probeErr, b.probeN = nil, 5
	r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	if p := r.svc.readHealth().Probe; p.Code != CodePermissionDenied {
		t.Fatalf("a failed check was retried within the minute: %+v", p)
	}
	h = r.svc.readHealth()
	h.Probe.At = h.Probe.At.Add(-2 * time.Minute)
	r.svc.writeHealth(h)
	r.ask(t, "grokbot", `note: {"op":"search","query":"lamp"}`)
	if p := r.svc.readHealth().Probe; p.Code != "" || p.Count != 5 {
		t.Fatalf("probe after a working search = %+v, want the recovery recorded", p)
	}
}
