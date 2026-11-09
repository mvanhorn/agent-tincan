package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// maxAppleNotesOutput bounds a script's output. A search lists every
// note's text, so it is larger than the helper's bound.
const maxAppleNotesOutput = 256 << 20

// deletedFolderNames are the Recently Deleted folder's names in the
// languages macOS ships, folded. Notes there are reported as trashed.
var deletedFolderNames = map[string]bool{}

func init() {
	for _, n := range []string{
		"Recently Deleted", "Zuletzt gelöscht", "Supprimés récemment", "Eliminados recientemente",
		"Eliminati di recente", "Apagados Recentemente", "Apagadas recentemente", "Recent verwijderd",
		"Nyligen raderade", "Nyligt slettet", "Nylig slettet", "Äskettäin poistetut", "Недавно удаленные",
		"Ostatnio usunięte", "Son Silinenler", "最近削除した項目", "最近删除", "最近刪除", "최근 삭제된 항목",
	} {
		deletedFolderNames[fold(n)] = true
	}
}

// IsDeletedFolder reports whether a folder name is Apple Notes' Recently
// Deleted folder.
func IsDeletedFolder(name string) bool { return deletedFolderNames[fold(strings.TrimSpace(name))] }

// AppleNotes drives Apple Notes through osascript.
type AppleNotes struct {
	// Run runs a JXA script with input as its stdin and returns its
	// stdout. Tests replace it.
	Run    func(ctx context.Context, script string, input []byte) ([]byte, error)
	ledger *ledger
	now    func() time.Time

	// The last listing, reused for appleListTTL so one search's per-word
	// fallback does not list the whole library once per word.
	mu        sync.Mutex
	list      *appleList
	listAt    time.Time
	probeNote string
}

// appleListTTL is how long a listing of every note is reused.
var appleListTTL = 30 * time.Second

// NewAppleNotes builds the Apple Notes backend, keeping its add ledger in
// appSupport.
func NewAppleNotes(appSupport string, logf func(string, ...any)) *AppleNotes {
	return &AppleNotes{ledger: newLedger(ledgerPath(appSupport, BackendAppleNotes), logf), now: time.Now}
}

var _ Backend = (*AppleNotes)(nil)

func (a *AppleNotes) Name() string { return "Apple Notes" }

// script runs one of the fixed scripts with in as its JSON input.
func (a *AppleNotes) script(ctx context.Context, script string, in any, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	run := a.Run
	if run == nil {
		if runtime.GOOS != "darwin" {
			return &HelperError{Code: codeUnavailable, Message: "the Apple Notes backend runs only on macOS"}
		}
		run = runOsascript
	}
	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	raw, err := run(ctx, script, data)
	if err != nil {
		return classifyOsascript(ctx, err)
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), out); err != nil {
		return &HelperError{Code: codeBadOutput, Message: "Apple Notes script output did not parse"}
	}
	return nil
}

// osascriptError is a failed osascript run, with what it wrote to stderr.
type osascriptError struct {
	err    error
	stderr string
}

func (e *osascriptError) Error() string { return strings.TrimSpace(e.err.Error() + ": " + e.stderr) }

func runOsascript(ctx context.Context, script string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{b: &stdout, n: maxAppleNotesOutput}
	cmd.Stderr = &limitedBuffer{b: &stderr, n: 4 << 10}
	if err := cmd.Run(); err != nil {
		return nil, &osascriptError{err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

// classifyOsascript maps an osascript failure to a helper code. -1743 is
// macOS refusing Automation of Notes; the owner can grant it.
func classifyOsascript(ctx context.Context, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "-1743") || strings.Contains(msg, "Not authorized to send Apple events"):
		return &HelperError{Code: CodePermissionDenied, Message: "macOS does not let this process control Notes (Automation permission)"}
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		return errAppleTimeout
	case errors.Is(err, exec.ErrNotFound):
		return &HelperError{Code: codeUnavailable, Message: "osascript not found"}
	}
	return &HelperError{Code: codeUnavailable, Message: capRunes(oneLine(msg), 300)}
}

// errAppleTimeout is a script that ran out of time: Notes may still act.
var errAppleTimeout = &HelperError{Code: codeUnavailable, Message: "Apple Notes did not answer in time"}

type appleList struct {
	Folders []struct {
		Folder string   `json:"folder"`
		IDs    []string `json:"ids"`
		Names  []string `json:"names"`
		Texts  []string `json:"texts"`
		Locked []bool   `json:"locked"`
	} `json:"folders"`
}

// Search lists every note and keeps those holding every word of query in
// the name or, for notes that are not locked, the text.
func (a *AppleNotes) Search(ctx context.Context, query string) ([]NoteSummary, error) {
	words := strings.Fields(fold(query))
	if len(words) == 0 {
		return nil, nil
	}
	l, err := a.listing(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []NoteSummary
	for _, f := range l.Folders {
		state := StateActive
		if IsDeletedFolder(f.Folder) {
			state = StateTrashed
		}
		if len(f.Names) != len(f.IDs) || len(f.Texts) != len(f.IDs) || len(f.Locked) != len(f.IDs) {
			return nil, &HelperError{Code: codeBadOutput, Message: "Apple Notes listed a folder with mismatched fields"}
		}
		for i, id := range f.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			name, text, locked := f.Names[i], f.Texts[i], f.Locked[i]
			if locked {
				text = ""
			}
			fn, ft := fold(name), fold(text)
			match := true
			for _, w := range words {
				if !strings.Contains(fn, w) && !strings.Contains(ft, w) {
					match = false
					break
				}
			}
			if match {
				out = append(out, appleSummary(id, name, text, state, locked, words))
			}
		}
	}
	return out, nil
}

func appleSummary(id, name, text, state string, locked bool, words []string) NoteSummary {
	s := NoteSummary{ID: id, Title: name, State: state}
	if !locked {
		s.Snippet = snippetAround(text, words)
		s.Hash = textHash(text)
	}
	return s
}

type appleRead struct {
	Found  bool   `json:"found"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Locked bool   `json:"locked"`
	Folder string `json:"folder"`
	// FolderUnknown is a note whose folder Notes would not say, so it may
	// be in Recently Deleted.
	FolderUnknown bool   `json:"folderUnknown"`
	Text          string `json:"text"`
}

// listing lists every note, or reuses a listing made in the last
// appleListTTL. An add drops it, so the new note is found at once.
func (a *AppleNotes) listing(ctx context.Context) (*appleList, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.list != nil && a.clock().Sub(a.listAt) < appleListTTL {
		return a.list, nil
	}
	var l appleList
	if err := a.script(ctx, jxaList, struct{}{}, &l); err != nil {
		return nil, err
	}
	a.list, a.listAt = &l, a.clock()
	return &l, nil
}

func (a *AppleNotes) dropListing() {
	a.mu.Lock()
	a.list = nil
	a.mu.Unlock()
}

func (a *AppleNotes) clock() time.Time {
	if a.now == nil {
		return time.Now()
	}
	return a.now()
}

// Read reads one note by its Apple Notes id.
func (a *AppleNotes) Read(ctx context.Context, id string) (Note, error) {
	if !appleNoteIDRE.MatchString(id) {
		return Note{}, ErrNoteNotFound
	}
	var r appleRead
	if err := a.script(ctx, jxaRead, map[string]string{"id": id}, &r); err != nil {
		return Note{}, err
	}
	if !r.Found {
		return Note{}, ErrNoteNotFound
	}
	state := StateActive
	if r.FolderUnknown || IsDeletedFolder(r.Folder) {
		state = StateTrashed // never answered to teammates
	}
	n := Note{Summary: appleSummary(r.ID, r.Name, r.Text, state, r.Locked, nil)}
	n.Summary.Snippet = ""
	if r.Locked {
		return n, ErrNoteLocked
	}
	n.Body = r.Text
	return n, nil
}

// Probe counts the notes, so doctor can tell Notes answers this process.
func (a *AppleNotes) Probe(ctx context.Context) (int, error) {
	var c struct {
		Count   int      `json:"count"`
		Folders []string `json:"folders"`
	}
	err := a.script(ctx, jxaCount, struct{}{}, &c)
	if err == nil {
		var deleted []string
		for _, f := range c.Folders {
			if IsDeletedFolder(f) {
				deleted = append(deleted, f)
			}
		}
		note := fmt.Sprintf("no folder recognized as Recently Deleted among %d top-level folders (Notes shows it only while it holds notes; if it is there under another name, its notes would be answered as active)", len(c.Folders))
		if len(deleted) > 0 {
			note = fmt.Sprintf("treated as deleted: %s", strings.Join(deleted, ", "))
		}
		a.mu.Lock()
		a.probeNote = note
		a.mu.Unlock()
	}
	return c.Count, err
}

// ProbeNote says which folder the last Probe treated as Recently Deleted.
func (a *AppleNotes) ProbeNote() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.probeNote
}

// Create makes a note in the default folder. The add is recorded as
// pending first; a retry whose earlier attempt may have made the note
// looks for it instead of making another.
func (a *AppleNotes) Create(ctx context.Context, title, body string, tags []string, props map[string]string, key string) (CreateResult, error) {
	e, ok, err := a.ledger.Lookup(key)
	if err != nil {
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: err.Error()}
	}
	if ok && e.State == ledgerDone {
		return a.existing(ctx, e.NoteID)
	}
	if ok && e.State == ledgerPending {
		id, err := a.findCreated(ctx, e.Title, e.Start)
		if err != nil {
			return CreateResult{}, err
		}
		if id == "" {
			return CreateResult{}, &HelperError{Code: codeCreateUnconfirmed, Message: "an earlier attempt may have saved the note in Apple Notes, but it was not found"}
		}
		if err := a.ledger.Done(key, id); err != nil {
			return CreateResult{}, &HelperError{Code: codeUnavailable, Message: err.Error()}
		}
		return a.existing(ctx, id)
	}
	start := time.Now().Add(-2 * time.Second)
	if err := a.ledger.Begin(key, title, start, props); err != nil {
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: err.Error()}
	}
	var made struct {
		ID string `json:"id"`
	}
	err = a.script(ctx, jxaCreate, map[string]string{"html": appleNoteHTML(title, body, tags)}, &made)
	a.dropListing()
	if err != nil {
		return a.createFailed(ctx, key, title, start, err)
	}
	if made.ID == "" {
		return CreateResult{}, &HelperError{Code: codeBadOutput, Message: "Apple Notes returned no note id"}
	}
	if err := a.ledger.Done(key, made.ID); err != nil {
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: err.Error()}
	}
	res, err := a.existing(ctx, made.ID)
	res.Existed = false
	return res, err
}

// createFailed settles the ledger after a create script failed. macOS
// refusing Automation means Notes was never asked. Otherwise the script may
// have failed before or after the note was made, so look now: a note found
// is this add's, and none found means the add can be asked again. Only a
// timeout, when Notes may still be saving, stays pending for the retry.
func (a *AppleNotes) createFailed(ctx context.Context, key, title string, start time.Time, err error) (CreateResult, error) {
	if errorCode(err) == CodePermissionDenied {
		_ = a.ledger.Forget(key)
		return CreateResult{}, err
	}
	if ctx.Err() != nil || errors.Is(err, errAppleTimeout) {
		return CreateResult{}, err
	}
	id, ferr := a.findCreated(ctx, title, start)
	switch {
	case ferr != nil:
		return CreateResult{}, err // still pending: the retry looks again
	case id == "":
		_ = a.ledger.Forget(key)
		return CreateResult{}, err
	}
	if derr := a.ledger.Done(key, id); derr != nil {
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: derr.Error()}
	}
	res, rerr := a.existing(ctx, id)
	res.Existed = false
	return res, rerr
}

func (a *AppleNotes) existing(ctx context.Context, id string) (CreateResult, error) {
	n, err := a.Read(ctx, id)
	switch {
	case errors.Is(err, ErrNoteNotFound):
		return CreateResult{Note: Note{Summary: NoteSummary{ID: id, State: StateDeleted}}, Existed: true}, nil
	case errors.Is(err, ErrNoteLocked):
	case err != nil:
		return CreateResult{}, err
	}
	n.Body = ""
	return CreateResult{Note: n, Existed: true}, nil
}

// findCreated looks for the note an attempt started at start made: one
// note created since then whose first line is the title.
func (a *AppleNotes) findCreated(ctx context.Context, title string, start time.Time) (string, error) {
	var r struct {
		IDs   []string `json:"ids"`
		Names []string `json:"names"`
	}
	if err := a.script(ctx, jxaRecent, map[string]int64{"since": start.UnixMilli()}, &r); err != nil {
		return "", err
	}
	want := normTitle(title)
	var ids []string
	for i, id := range r.IDs {
		if i < len(r.Names) && normTitle(r.Names[i]) == want {
			ids = append(ids, id)
		}
	}
	switch len(ids) {
	case 0:
		return "", nil
	case 1:
		return ids[0], nil
	}
	return "", &HelperError{Code: codeCreateUnconfirmed, Message: fmt.Sprintf("%d Apple Notes notes with this title were created at the same time, so the notes agent cannot tell which is this add's", len(ids))}
}

// appleNoteHTML is a note body as Notes stores it: the title as the first
// line, then one div per body line, then the tags as hashtags.
func appleNoteHTML(title, body string, tags []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<div><h1>%s</h1></div>", html.EscapeString(oneLine(title)))
	for line := range strings.SplitSeq(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if line == "" {
			b.WriteString("<div><br></div>")
			continue
		}
		fmt.Fprintf(&b, "<div>%s</div>", html.EscapeString(line))
	}
	if len(tags) > 0 {
		hs := make([]string, 0, len(tags))
		for _, t := range tags {
			hs = append(hs, "#"+strings.Join(strings.Fields(t), "-"))
		}
		fmt.Fprintf(&b, "<div>%s</div>", html.EscapeString(strings.Join(hs, " ")))
	}
	return b.String()
}
