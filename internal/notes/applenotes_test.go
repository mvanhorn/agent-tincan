package notes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNotesApp stands in for Notes behind osascript: it answers each fixed
// script from an in-memory library, as the real scripts would.
type fakeNotesApp struct {
	t       *testing.T
	mu      sync.Mutex
	notes   []fakeAppleNote
	next    int
	inputs  [][]byte
	scripts []string
	fail    error // returned by every run when set
	// createErr fails a create after the note is made, as a timeout
	// after Notes saved it would.
	createErr error
}

type fakeAppleNote struct {
	id, name, text, folder string
	locked                 bool
	created                time.Time
}

func (a *fakeNotesApp) put(name, text, folder string, locked bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	id := fmt.Sprintf("x-coredata://5A6B7C8D-1E2F-4A5B-8C9D-0E1F2A3B4C5D/ICNote/p%d", a.next)
	a.notes = append(a.notes, fakeAppleNote{id: id, name: name, text: text, folder: folder, locked: locked, created: time.Now()})
	return id
}

func (a *fakeNotesApp) run(_ context.Context, script string, input []byte) ([]byte, error) {
	a.mu.Lock()
	a.scripts = append(a.scripts, script)
	a.inputs = append(a.inputs, input)
	fail := a.fail
	a.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	var in map[string]any
	if err := json.Unmarshal(input, &in); err != nil {
		a.t.Fatalf("script input is not JSON: %q", input)
	}
	switch script {
	case jxaList:
		byFolder := map[string]map[string]any{}
		var order []string
		a.mu.Lock()
		for _, n := range a.notes {
			f := byFolder[n.folder]
			if f == nil {
				f = map[string]any{"folder": n.folder, "ids": []string{}, "names": []string{}, "texts": []string{}, "locked": []bool{}}
				byFolder[n.folder] = f
				order = append(order, n.folder)
			}
			text := n.text
			if n.locked {
				text = "" // Notes does not give a locked note's text
			}
			f["ids"] = append(f["ids"].([]string), n.id)
			f["names"] = append(f["names"].([]string), n.name)
			f["texts"] = append(f["texts"].([]string), text)
			f["locked"] = append(f["locked"].([]bool), n.locked)
		}
		a.mu.Unlock()
		var fs []any
		for _, k := range order {
			fs = append(fs, byFolder[k])
		}
		return json.Marshal(map[string]any{"folders": fs})
	case jxaRead:
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, n := range a.notes {
			if n.id == in["id"] {
				text := n.text
				if n.locked {
					text = ""
				}
				return json.Marshal(map[string]any{"found": true, "id": n.id, "name": n.name, "locked": n.locked, "folder": n.folder, "text": text})
			}
		}
		return []byte(`{"found":false}`), nil
	case jxaCreate:
		h := in["html"].(string)
		name := strings.TrimSuffix(strings.TrimPrefix(h[:strings.Index(h, "</h1>")+5], "<div><h1>"), "</h1>")
		id := a.put(name, h, "Notes", false)
		if a.createErr != nil {
			return nil, a.createErr
		}
		return json.Marshal(map[string]string{"id": id})
	case jxaRecent:
		since := time.UnixMilli(int64(in["since"].(float64)))
		a.mu.Lock()
		defer a.mu.Unlock()
		ids, names := []string{}, []string{}
		for _, n := range a.notes {
			if !n.created.Before(since) {
				ids, names = append(ids, n.id), append(names, n.name)
			}
		}
		return json.Marshal(map[string]any{"ids": ids, "names": names})
	case jxaCount:
		a.mu.Lock()
		defer a.mu.Unlock()
		return json.Marshal(map[string]int{"count": len(a.notes)})
	}
	a.t.Fatalf("unknown script")
	return nil, nil
}

func (a *fakeNotesApp) count(script string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, s := range a.scripts {
		if s == script {
			n++
		}
	}
	return n
}

func newAppleNotes(t *testing.T) (*AppleNotes, *fakeNotesApp) {
	app := &fakeNotesApp{t: t}
	a := NewAppleNotes(t.TempDir(), t.Logf)
	a.Run = app.run
	return a, app
}

func titles(ns []NoteSummary) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Title+"/"+n.State)
	}
	slices.Sort(out)
	return out
}

func TestAppleNotesSearchNeedsEveryWordAndMarksDeleted(t *testing.T) {
	a, app := newAppleNotes(t)
	app.put("Lake trip", "Packing list for the cabin", "Notes", false)
	app.put("Cabin", "directions", "Notes", false)
	app.put("Old lake trip", "packing cabin", "Recently Deleted", false)
	app.put("Alte Seereise", "packing cabin lake", "Zuletzt gelöscht", false)
	got, err := a.Search(t.Context(), "LAKE cabin packing")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Alte Seereise/trashed", "Lake trip/active", "Old lake trip/trashed"}
	if !slices.Equal(titles(got), want) {
		t.Fatalf("search = %v, want %v", titles(got), want)
	}
}

func TestAppleNotesLockedNotesMatchByNameOnly(t *testing.T) {
	a, app := newAppleNotes(t)
	id := app.put("Bank", "pin zebra", "Notes", true)
	if got, _ := a.Search(t.Context(), "zebra"); len(got) != 0 {
		t.Fatalf("a locked note was found by its text: %v", titles(got))
	}
	got, err := a.Search(t.Context(), "bank")
	if err != nil || len(got) != 1 || got[0].Snippet != "" {
		t.Fatalf("by name: %+v, %v", got, err)
	}
	n, err := a.Read(t.Context(), id)
	if !errors.Is(err, ErrNoteLocked) || n.Body != "" {
		t.Fatalf("read locked = %+v, %v", n, err)
	}
}

func TestAppleNotesReadMissingNote(t *testing.T) {
	a, _ := newAppleNotes(t)
	if _, err := a.Read(t.Context(), "x-coredata://5A6B7C8D-1E2F-4A5B-8C9D-0E1F2A3B4C5D/ICNote/p99"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("read missing = %v", err)
	}
}

func TestAppleNotesInputsTravelOnlyOnStdin(t *testing.T) {
	a, app := newAppleNotes(t)
	evil := `"); Application('Finder').delete(); ("`
	if _, err := a.Create(t.Context(), evil, "x</div><script>"+evil, []string{"tag"}, nil, "tincan:r1"); err != nil {
		t.Fatal(err)
	}
	for _, s := range app.scripts {
		if strings.Contains(s, "Finder") {
			t.Fatal("note text reached a script's source")
		}
		if !slices.Contains([]string{jxaList, jxaRead, jxaCreate, jxaRecent, jxaCount}, s) {
			t.Fatal("a script other than the fixed ones ran")
		}
	}
	var in map[string]string
	_ = json.Unmarshal(app.inputs[0], &in)
	if !strings.Contains(in["html"], "&lt;script&gt;") || strings.Contains(in["html"], "<script>") {
		t.Fatalf("body HTML = %q, want it escaped", in["html"])
	}
}

func TestAppleNoteHTMLKeepsLinesAndTags(t *testing.T) {
	got := appleNoteHTML("A & B", "one < two\n\nthree", []string{"camping trip", "from-agent"})
	want := "<div><h1>A &amp; B</h1></div><div>one &lt; two</div><div><br></div><div>three</div><div>#camping-trip #from-agent</div>"
	if got != want {
		t.Fatalf("html =\n%s\nwant\n%s", got, want)
	}
}

func TestAppleNotesCreateIsIdempotent(t *testing.T) {
	a, app := newAppleNotes(t)
	res, err := a.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1")
	if err != nil || res.Existed || res.Note.Summary.ID == "" {
		t.Fatalf("create = %+v, %v", res, err)
	}
	again, err := a.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1")
	if err != nil || !again.Existed || again.Note.Summary.ID != res.Note.Summary.ID {
		t.Fatalf("redelivered = %+v, %v", again, err)
	}
	if app.count(jxaCreate) != 1 {
		t.Fatalf("Notes was asked to create %d times, want once", app.count(jxaCreate))
	}
	if _, err := a.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r2"); err != nil || app.count(jxaCreate) != 2 {
		t.Fatalf("a different add with the same content was not saved: %v", err)
	}
}

func TestAppleNotesCreateThatTimedOutIsAdoptedNotRepeated(t *testing.T) {
	a, app := newAppleNotes(t)
	app.createErr = errors.New("signal: killed")
	if _, err := a.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1"); err == nil || IsPermanent(err) {
		t.Fatalf("first attempt = %v, want transient", err)
	}
	app.createErr = nil
	res, err := a.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1")
	if err != nil || res.Note.Summary.ID == "" {
		t.Fatalf("retry = %+v, %v; want the earlier note adopted", res, err)
	}
	if app.count(jxaCreate) != 1 {
		t.Fatalf("Notes was asked to create %d times, want once", app.count(jxaCreate))
	}
}

func TestAppleNotesPermissionDenied(t *testing.T) {
	a, app := newAppleNotes(t)
	app.fail = &osascriptError{err: errors.New("exit status 1"), stderr: "execution error: Not authorized to send Apple events to Notes. (-1743)"}
	_, err := a.Probe(t.Context())
	if errorCode(err) != CodePermissionDenied || IsPermanent(err) {
		t.Fatalf("probe = %v, want a transient permission_denied", err)
	}
	if _, err := a.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1"); errorCode(err) != CodePermissionDenied {
		t.Fatalf("create = %v", err)
	}
	// Notes was never asked, so once access is granted the add goes ahead.
	app.fail = nil
	if res, err := a.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1"); err != nil || res.Existed {
		t.Fatalf("after access = %+v, %v", res, err)
	}
}

func TestAppleNotesProbeCounts(t *testing.T) {
	a, app := newAppleNotes(t)
	app.put("A", "a", "Notes", false)
	app.put("B", "b", "Notes", false)
	if n, err := a.Probe(t.Context()); err != nil || n != 2 {
		t.Fatalf("probe = %d, %v", n, err)
	}
}

func TestDeletedFolderNames(t *testing.T) {
	for _, n := range []string{"Recently Deleted", "recently deleted ", "Zuletzt gelöscht", "最近删除"} {
		if !IsDeletedFolder(n) {
			t.Errorf("%q not recognized", n)
		}
	}
	if IsDeletedFolder("Notes") {
		t.Error("Notes recognized as deleted")
	}
}
