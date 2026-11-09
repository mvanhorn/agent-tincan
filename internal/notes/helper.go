package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// DefaultHelperPath is the agent-notes helper inside the installed app.
const DefaultHelperPath = "/Applications/Agent Notes.app/Contents/Helpers/agent-notes"

// Environment the helper reads (KTD8).
const (
	envLibraryRoot    = "AGENT_NOTES_LIBRARY_ROOT"
	envAppSupportRoot = "AGENT_NOTES_APPLICATION_SUPPORT_ROOT"
)

// helperTimeout bounds one helper call.
const helperTimeout = 60 * time.Second

// maxHelperOutput bounds what is read from the helper's stdout.
const maxHelperOutput = 32 << 20

// NoteSummary is a note as the helper lists it.
type NoteSummary struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	State   string   `json:"state"`
	Pinned  bool     `json:"pinned"`
	Tags    []string `json:"tags"`
	Hash    string   `json:"hash"`
	Snippet string   `json:"snippet,omitempty"`
}

// Note is a note with its body.
type Note struct {
	Summary NoteSummary `json:"summary"`
	Body    string      `json:"body"`
}

// StateActive is the only state search and read expose.
const StateActive = "active"

// helperResponse is the helper's CLIResponse, only the fields used here.
type helperResponse struct {
	OK      bool          `json:"ok"`
	Command string        `json:"command"`
	Notes   []NoteSummary `json:"notes"`
	Note    *Note         `json:"note"`
	// Existed is absent (nil) from a helper that predates idempotency
	// keys, which is how such a helper is told apart.
	Existed *bool `json:"existed"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// HelperError is a helper call that did not succeed. Code is the helper's
// error code, or one of the service's own (helper_unavailable,
// helper_bad_output) when the helper could not be run or understood.
type HelperError struct {
	Code    string
	Message string
}

func (e *HelperError) Error() string {
	if e.Message == "" {
		return "agent-notes helper: " + e.Code
	}
	return fmt.Sprintf("agent-notes helper: %s: %s", e.Code, e.Message)
}

// permanentCodes are the helper's validation-class error codes (from
// AgentNotesCommandEngine): the same request would fail the same way on
// every retry. Every other code, including ones this build does not know,
// is transient: the add is kept and retried rather than lost (KTD9).
var permanentCodes = map[string]bool{
	"invalid_metadata":   true,
	"missing_option":     true,
	"unsupported_option": true,
	"invalid_identifier": true,
	"invalid_path":       true,
	"input_too_large":    true,
	"invalid_utf8":       true,
	"unknown_command":    true,
	"invalid_request":    true,
	"missing_input":      true,
	"unsafe_path":        true,
	// The app may have saved the note but it cannot be identified, so
	// creating again could save it twice.
	codeCreateUnconfirmed: true,
}

// IsPermanent reports whether err is a helper failure that retrying cannot
// fix.
func IsPermanent(err error) bool {
	var he *HelperError
	return errors.As(err, &he) && permanentCodes[he.Code]
}

// errorCode is err's helper code, or "" when err is not a helper failure.
func errorCode(err error) string {
	if he, ok := errors.AsType[*HelperError](err); ok {
		return he.Code
	}
	return ""
}

// Helper runs the agent-notes helper against one library with the
// service's own Application Support root.
type Helper struct {
	Path           string
	LibraryRoot    string
	AppSupportRoot string
}

// env is the helper's environment: this process's, minus every
// AGENT_NOTES_ variable (an in-app agent session's among them), plus the
// service's own library and Application Support roots. The helper then
// never reads a session context meant for an in-app agent (KTD8).
func (h *Helper) env() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AGENT_NOTES_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, envLibraryRoot+"="+h.LibraryRoot, envAppSupportRoot+"="+h.AppSupportRoot)
}

// run invokes the helper with args and decodes its one-line response.
func (h *Helper) run(ctx context.Context, args ...string) (helperResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.Path, args...)
	cmd.Env = h.env()
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{b: &stdout, n: maxHelperOutput}
	cmd.Stderr = &limitedBuffer{b: &stderr, n: 4 << 10}
	runErr := cmd.Run()
	var resp helperResponse
	line := bytes.TrimSpace(stdout.Bytes())
	if len(line) == 0 || json.Unmarshal(line, &resp) != nil {
		var ee *exec.ExitError
		if runErr != nil && !errors.As(runErr, &ee) {
			return helperResponse{}, &HelperError{Code: startFailureCode(ctx, runErr), Message: runErr.Error()}
		}
		msg := strings.TrimSpace(stderr.String())
		if runErr != nil {
			msg = strings.TrimSpace(runErr.Error() + " " + msg)
		}
		return helperResponse{}, &HelperError{Code: "helper_bad_output", Message: msg}
	}
	if !resp.OK {
		he := &HelperError{Code: "operation_failed"}
		if resp.Error != nil {
			he.Code, he.Message = resp.Error.Code, resp.Error.Message
		}
		return resp, he
	}
	return resp, nil
}

// startFailureCode classifies a helper that could not be started. A
// missing or unexecutable helper, or a run cut off by its deadline, is
// helper_unavailable (transient: installing Agent Notes or a later retry
// fixes it). Any other start failure, such as EINVAL from a NUL in argv or
// E2BIG from oversized arguments, comes from the arguments themselves and
// would fail the same way on every retry, so it is permanent.
func startFailureCode(ctx context.Context, err error) string {
	if ctx.Err() != nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, exec.ErrNotFound) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "helper_unavailable"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && (errno == syscall.EINVAL || errno == syscall.E2BIG) {
		return "invalid_request"
	}
	return "helper_unavailable"
}

// Name is the app the helper belongs to.
func (h *Helper) Name() string { return "Agent Notes" }

// Probe checks that the helper is installed and executable. The helper has
// no count command, so the count is -1 (unknown).
func (h *Helper) Probe(context.Context) (int, error) {
	st, err := os.Stat(h.Path)
	if err != nil {
		return -1, &HelperError{Code: codeUnavailable, Message: err.Error()}
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return -1, &HelperError{Code: codeUnavailable, Message: h.Path + " is not an executable"}
	}
	return -1, nil
}

var _ Backend = (*Helper)(nil)

// CreateResult is the note an add made or found.
type CreateResult struct {
	Note    Note
	Existed bool // the idempotency key matched an existing note
	// NoIdempotency is set when a key was sent but the response has no
	// "existed" field: the helper predates idempotency keys and ignored it.
	NoIdempotency bool
}

// codeHelperTooOld is the health code for a helper that ignores
// idempotency keys.
const codeHelperTooOld = "helper_too_old"

// helperTooOldMessage tells the owner what to do about codeHelperTooOld.
const helperTooOldMessage = "This Agent Notes helper does not support idempotency keys, so a redelivered add could create a duplicate note. Please update Agent Notes to the latest version."

// Create adds a note in the background with an idempotency key and
// provenance properties. A note in any state already carrying key is
// returned instead of a new one.
func (h *Helper) Create(ctx context.Context, title, body string, tags []string, props map[string]string, key string) (CreateResult, error) {
	pj, err := json.Marshal(props)
	if err != nil {
		return CreateResult{}, err
	}
	args := []string{"create", "--title", title, "--body", body}
	if len(tags) > 0 {
		args = append(args, "--tags", strings.Join(tags, ","))
	}
	args = append(args, "--background", "--properties", string(pj), "--idempotency-key", key)
	resp, err := h.run(ctx, args...)
	if err != nil {
		return CreateResult{}, err
	}
	if resp.Note == nil || resp.Note.Summary.ID == "" {
		return CreateResult{}, &HelperError{Code: "helper_bad_output", Message: "create returned no note id"}
	}
	res := CreateResult{Note: *resp.Note}
	if resp.Existed != nil {
		res.Existed = *resp.Existed
	} else if key != "" {
		res.NoIdempotency = true
	}
	return res, nil
}

// Search returns the notes matching query, in every state.
func (h *Helper) Search(ctx context.Context, query string) ([]NoteSummary, error) {
	resp, err := h.run(ctx, "search", "--query", query)
	if err != nil {
		return nil, err
	}
	return resp.Notes, nil
}

// ErrNoteNotFound is a read of an id the library does not hold.
var ErrNoteNotFound = errors.New("note not found")

// Read returns the note with id, in any state.
func (h *Helper) Read(ctx context.Context, id string) (Note, error) {
	resp, err := h.run(ctx, "read", "--id", id)
	var he *HelperError
	if errors.As(err, &he) && isNotFound(he) {
		return Note{}, ErrNoteNotFound
	}
	if err != nil {
		return Note{}, err
	}
	if resp.Note == nil {
		return Note{}, &HelperError{Code: "helper_bad_output", Message: "read returned no note"}
	}
	return *resp.Note, nil
}

// isNotFound recognizes the helper's answer for an unknown id: the engine
// reports LibraryActorError.noteNotFound as operation_failed with this
// message; a not_found code is accepted too in case a later helper adds
// one.
func isNotFound(he *HelperError) bool {
	return he.Code == "not_found" || he.Code == "note_not_found" ||
		(he.Code == "operation_failed" && strings.Contains(he.Message, "no longer exists"))
}

// limitedBuffer keeps at most n bytes and discards the rest, so a runaway
// helper cannot exhaust memory.
type limitedBuffer struct {
	b *bytes.Buffer
	n int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

var _ io.Writer = (*limitedBuffer)(nil)
