package notes

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// bearFixture is a database with Bear's tables and columns, holding
// invented notes.
type bearFixture struct {
	t    *testing.T
	path string
	db   *sql.DB
	pk   int64
}

const bearSchema = `
CREATE TABLE ZSFNOTE (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZARCHIVED INTEGER, ZENCRYPTED INTEGER,
  ZLOCKED INTEGER, ZPERMANENTLYDELETED INTEGER, ZPINNED INTEGER, ZTRASHED INTEGER, ZCREATIONDATE TIMESTAMP,
  ZMODIFICATIONDATE TIMESTAMP, ZTEXT VARCHAR, ZTITLE VARCHAR, ZUNIQUEIDENTIFIER VARCHAR);
CREATE TABLE ZSFNOTETAG (Z_PK INTEGER PRIMARY KEY, ZTITLE VARCHAR);
CREATE TABLE Z_5TAGS (Z_5NOTES INTEGER, Z_13TAGS INTEGER, PRIMARY KEY (Z_5NOTES, Z_13TAGS));
CREATE TABLE Z_5PINNEDINTAGS (Z_5PINNEDNOTES INTEGER, Z_13PINNEDINTAGS INTEGER, PRIMARY KEY (Z_5PINNEDNOTES, Z_13PINNEDINTAGS));
`

func newBearFixture(t *testing.T) *bearFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "database.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(bearSchema); err != nil {
		t.Fatal(err)
	}
	return &bearFixture{t: t, path: path, db: db}
}

type bearNote struct {
	ID, Title, Text                            string
	Trashed, Archived, Pinned, Locked, Deleted bool
	Created                                    time.Time
	Tags, PinnedIn                             []string
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (f *bearFixture) put(n bearNote) bearNote {
	f.t.Helper()
	f.pk++
	if n.ID == "" {
		n.ID = strings.ToUpper(strings.Replace("0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B00", "00", string(rune('A'+f.pk%26))+string(rune('A'+f.pk/26)), 1))
	}
	if n.Created.IsZero() {
		n.Created = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	ts := n.Created.Sub(coreDataEpoch).Seconds()
	_, err := f.db.Exec(`INSERT INTO ZSFNOTE (Z_PK, ZARCHIVED, ZENCRYPTED, ZLOCKED, ZPERMANENTLYDELETED, ZPINNED, ZTRASHED, ZCREATIONDATE, ZMODIFICATIONDATE, ZTEXT, ZTITLE, ZUNIQUEIDENTIFIER)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)`, f.pk, b2i(n.Archived), b2i(n.Locked), b2i(n.Deleted), b2i(n.Pinned), b2i(n.Trashed), ts, ts, n.Text, n.Title, n.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for table, tags := range map[string][]string{"Z_5TAGS": n.Tags, "Z_5PINNEDINTAGS": n.PinnedIn} {
		for _, tag := range tags {
			var tpk int64
			if err := f.db.QueryRow(`SELECT Z_PK FROM ZSFNOTETAG WHERE ZTITLE = ?`, tag).Scan(&tpk); err != nil {
				res, err := f.db.Exec(`INSERT INTO ZSFNOTETAG (ZTITLE) VALUES (?)`, tag)
				if err != nil {
					f.t.Fatal(err)
				}
				tpk, _ = res.LastInsertId()
			}
			if _, err := f.db.Exec(`INSERT INTO `+table+` VALUES (?, ?)`, f.pk, tpk); err != nil {
				f.t.Fatal(err)
			}
		}
	}
	return n
}

func (f *bearFixture) bear() *Bear {
	b := NewBear(f.t.TempDir(), f.t.Logf)
	b.DB = f.path
	b.Open = func(context.Context, string) error { f.t.Fatal("unexpected create"); return nil }
	return b
}

func ids(ns []NoteSummary) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Title)
	}
	slices.Sort(out)
	return out
}

func TestBearSearchNeedsEveryWordInTitleOrText(t *testing.T) {
	f := newBearFixture(t)
	f.put(bearNote{Title: "Family trip", Text: "Packing list for the lake cabin"})
	f.put(bearNote{Title: "Lake cabin", Text: "Directions only"})
	f.put(bearNote{Title: "Groceries", Text: "milk, eggs"})
	got, err := f.bear().Search(t.Context(), "LAKE cabin packing")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Family trip"}; !slices.Equal(ids(got), want) {
		t.Fatalf("search = %v, want %v", ids(got), want)
	}
	if !strings.Contains(got[0].Snippet, "Packing list") {
		t.Fatalf("snippet = %q, want the text around the match", got[0].Snippet)
	}
}

func TestBearSearchFoldsNonASCIICase(t *testing.T) {
	f := newBearFixture(t)
	f.put(bearNote{Title: "Reise", Text: "Über den Pass nach Süden"})
	got, err := f.bear().Search(t.Context(), "über SÜDEN")
	if err != nil || len(got) != 1 {
		t.Fatalf("search = %v, %v; want the note with Über", ids(got), err)
	}
}

func TestBearSearchMatchesPercentAndUnderscoreLiterally(t *testing.T) {
	f := newBearFixture(t)
	f.put(bearNote{Title: "Rates", Text: "fee 5% on file_name"})
	f.put(bearNote{Title: "Other", Text: "fee 50 on fileXname"})
	got, err := f.bear().Search(t.Context(), "5% file_name")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Rates"}; !slices.Equal(ids(got), want) {
		t.Fatalf("search = %v, want %v", ids(got), want)
	}
}

func TestBearReturnsEveryStateButPermanentlyDeleted(t *testing.T) {
	f := newBearFixture(t)
	f.put(bearNote{Title: "Tent active", Text: "tent"})
	tr := f.put(bearNote{Title: "Tent trashed", Text: "tent", Trashed: true})
	f.put(bearNote{Title: "Tent archived", Text: "tent", Archived: true})
	gone := f.put(bearNote{Title: "Tent gone", Text: "tent", Deleted: true})
	b := f.bear()
	got, err := b.Search(t.Context(), "tent")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, n := range got {
		states[n.Title] = n.State
	}
	want := map[string]string{"Tent active": StateActive, "Tent trashed": "trashed", "Tent archived": "archived"}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for k, v := range want {
		if states[k] != v {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
	if n, err := b.Read(t.Context(), tr.ID); err != nil || n.Summary.State != "trashed" {
		t.Fatalf("read trashed = %+v, %v", n.Summary, err)
	}
	if _, err := b.Read(t.Context(), gone.ID); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("read permanently deleted = %v, want not found", err)
	}
}

func TestBearLockedNoteTextIsNeverSearchedOrRead(t *testing.T) {
	f := newBearFixture(t)
	secret := f.put(bearNote{Title: "Bank", Text: "pin code zebra", Locked: true})
	b := f.bear()
	if got, _ := b.Search(t.Context(), "zebra"); len(got) != 0 {
		t.Fatalf("a locked note was found by its text: %v", ids(got))
	}
	got, err := b.Search(t.Context(), "bank")
	if err != nil || len(got) != 1 || got[0].Snippet != "" || got[0].Hash != "" {
		t.Fatalf("by title: %+v, %v; want found with no snippet or hash", got, err)
	}
	n, err := b.Read(t.Context(), secret.ID)
	if !errors.Is(err, ErrNoteLocked) || n.Body != "" || n.Summary.State != StateActive {
		t.Fatalf("read locked = %+v, %v; want ErrNoteLocked and no body", n, err)
	}
}

func TestBearTagsComeFromTheTagTableOnly(t *testing.T) {
	f := newBearFixture(t)
	n := f.put(bearNote{Title: "Tent", Text: "x", Tags: []string{"camping", "gear"}, PinnedIn: []string{"pinned-here"}})
	got, err := f.bear().Read(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"camping", "gear"}; !slices.Equal(got.Summary.Tags, want) {
		t.Fatalf("tags = %v, want %v", got.Summary.Tags, want)
	}
}

func TestBearNeverWritesItsDatabase(t *testing.T) {
	f := newBearFixture(t)
	n := f.put(bearNote{Title: "Tent", Text: "stakes"})
	f.db.Close()
	before := fileSum(t, f.path)
	b := f.bear()
	if _, err := b.Search(t.Context(), "tent"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(t.Context(), n.ID); err != nil {
		t.Fatal(err)
	}
	if c, err := b.Probe(t.Context()); err != nil || c != 1 {
		t.Fatalf("probe = %d, %v", c, err)
	}
	if fileSum(t, f.path) != before {
		t.Fatal("Bear's database changed")
	}
	for _, sfx := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(f.path + sfx); err == nil {
			t.Fatalf("a %s file was created beside Bear's database", sfx)
		}
	}
}

func fileSum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestBearMissingOrUnreadableDatabase(t *testing.T) {
	b := NewBear(t.TempDir(), nil)
	b.DB = filepath.Join(t.TempDir(), "nope.sqlite")
	_, err := b.Probe(t.Context())
	if he, ok := errors.AsType[*HelperError](err); !ok || he.Code != codeUnavailable || !strings.Contains(he.Message, "Bear") {
		t.Fatalf("missing database: %v", err)
	}
	f := newBearFixture(t)
	if err := os.Chmod(f.path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.path, 0o600) })
	_, err = f.bear().Probe(t.Context())
	if he, ok := errors.AsType[*HelperError](err); !ok || he.Code != CodePermissionDenied {
		t.Fatalf("unreadable database: %v, want permission_denied", err)
	}
}

// fakeBearApp stands in for Bear: asked to create, it writes the note the
// way Bear stores it, now, later, or never.
type fakeBearApp struct {
	f      *bearFixture
	mu     sync.Mutex
	urls   []string
	delay  time.Duration
	never  bool
	stored func(title, text string) (string, string) // how Bear stores what it was sent
	now    func() time.Time                          // Bear's clock (time.Now when nil)
}

func (a *fakeBearApp) open(_ context.Context, raw string) error {
	a.mu.Lock()
	a.urls = append(a.urls, raw)
	a.mu.Unlock()
	if a.never {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	q := u.Query()
	title, text := q.Get("title"), q.Get("text")
	if a.stored != nil {
		title, text = a.stored(title, text)
	}
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	write := func() { a.f.put(bearNote{Title: title, Text: text, Created: now()}) }
	if a.delay > 0 {
		time.AfterFunc(a.delay, write)
	} else {
		write()
	}
	return nil
}

func (a *fakeBearApp) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.urls)
}

func fastBearPoll(t *testing.T, window time.Duration) {
	old, oldEvery := bearCreatePoll, bearPollEvery
	bearCreatePoll, bearPollEvery = window, 10*time.Millisecond
	t.Cleanup(func() { bearCreatePoll, bearPollEvery = old, oldEvery })
}

func newBearWithApp(t *testing.T) (*Bear, *fakeBearApp) {
	f := newBearFixture(t)
	app := &fakeBearApp{f: f}
	b := f.bear()
	b.Open = app.open
	return b, app
}

func TestBearCreateURLKeepsEveryCharacter(t *testing.T) {
	title, body := "Trip & plans #1?", "line one\nline two: 50% off & more 🎉 a+b"
	raw := bearCreateURL(title, body, []string{"camping trip", "from-agent"})
	for _, p := range []string{"open_note=no", "show_window=no", "%20"} {
		if !strings.Contains(raw, p) {
			t.Fatalf("url %q lacks %q", raw, p)
		}
	}
	if strings.Contains(raw, "+") {
		t.Fatalf("url %q has a +, which Bear reads as a plus sign", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("title") != title || q.Get("text") != body || q.Get("tags") != "camping trip,from-agent" {
		t.Fatalf("round trip: %q %q %q", q.Get("title"), q.Get("text"), q.Get("tags"))
	}
}

func TestBearCreateFindsTheNoteAsBearStoresIt(t *testing.T) {
	fastBearPoll(t, 2*time.Second)
	b, app := newBearWithApp(t)
	app.stored = func(title, text string) (string, string) {
		return title, "# " + title + "\n" + text + "\n#from-agent"
	}
	res, err := b.Create(t.Context(), "Tent  list", "stakes", []string{"from-agent"}, nil, "tincan:r1")
	if err != nil || res.Existed || res.Note.Summary.ID == "" {
		t.Fatalf("create = %+v, %v", res, err)
	}
	again, err := b.Create(t.Context(), "Tent  list", "stakes", []string{"from-agent"}, nil, "tincan:r1")
	if err != nil || !again.Existed || again.Note.Summary.ID != res.Note.Summary.ID {
		t.Fatalf("redelivered create = %+v, %v; want the same note, existed", again, err)
	}
	if app.calls() != 1 {
		t.Fatalf("Bear was asked %d times, want once", app.calls())
	}
}

func TestBearLateNoteIsAdoptedByTheRetry(t *testing.T) {
	fastBearPoll(t, 100*time.Millisecond)
	b, app := newBearWithApp(t)
	app.delay = 300 * time.Millisecond
	_, err := b.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1")
	if IsPermanent(err) || err == nil {
		t.Fatalf("first attempt = %v, want a transient error", err)
	}
	time.Sleep(400 * time.Millisecond)
	res, err := b.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1")
	if err != nil || res.Note.Summary.ID == "" {
		t.Fatalf("retry = %+v, %v; want the late note adopted", res, err)
	}
	if app.calls() != 1 {
		t.Fatalf("Bear was asked %d times, want once", app.calls())
	}
}

func TestBearNoteThatNeverAppearsFailsWithoutAskingAgain(t *testing.T) {
	fastBearPoll(t, 50*time.Millisecond)
	b, app := newBearWithApp(t)
	app.never = true
	if _, err := b.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1"); err == nil || IsPermanent(err) {
		t.Fatalf("first attempt = %v, want transient", err)
	}
	_, err := b.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1")
	if errorCode(err) != codeCreateUnconfirmed || !IsPermanent(err) {
		t.Fatalf("retry = %v, want a permanent create_unconfirmed", err)
	}
	if app.calls() != 1 {
		t.Fatalf("Bear was asked %d times, want once", app.calls())
	}
}

func TestBearTwoCandidatesFailClosed(t *testing.T) {
	fastBearPoll(t, time.Second)
	b, app := newBearWithApp(t)
	app.f.put(bearNote{Title: "Tent", Text: "someone else's", Created: time.Now()})
	_, err := b.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1")
	if errorCode(err) != codeCreateUnconfirmed {
		t.Fatalf("create = %v, want create_unconfirmed rather than a guessed id", err)
	}
}

func TestBearSameContentDifferentKeyIsSaved(t *testing.T) {
	fastBearPoll(t, time.Second)
	b, app := newBearWithApp(t)
	if _, err := b.Create(t.Context(), "Weekly review", "same text", nil, nil, "tincan:r1"); err != nil {
		t.Fatal(err)
	}
	// The second add comes later, by both clocks.
	later := func() time.Time { return time.Now().Add(5 * time.Second) }
	b.now, app.now = later, later
	res, err := b.Create(t.Context(), "Weekly review", "same text", nil, nil, "tincan:r2")
	if err == nil && res.Existed {
		t.Fatal("a different add with the same content was treated as already saved")
	}
	if app.calls() != 2 {
		t.Fatalf("Bear was asked %d times, want twice", app.calls())
	}
}

func TestBearOpenFailureLetsTheRetryAskAgain(t *testing.T) {
	fastBearPoll(t, time.Second)
	b, app := newBearWithApp(t)
	fail := true
	b.Open = func(ctx context.Context, u string) error {
		if fail {
			fail = false
			return errors.New("LSOpenURLsWithRole() failed")
		}
		return app.open(ctx, u)
	}
	if _, err := b.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1"); err == nil || IsPermanent(err) {
		t.Fatalf("first attempt = %v, want transient", err)
	}
	if res, err := b.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1"); err != nil || res.Note.Summary.ID == "" {
		t.Fatalf("retry = %+v, %v", res, err)
	}
}

// The row cap must not crowd out an active note: newer trashed notes that
// match would otherwise fill the cap first.
func TestBearSearchCapKeepsActiveNotesFirst(t *testing.T) {
	f := newBearFixture(t)
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	f.put(bearNote{ID: "SFNote2Intro1", Title: "Tent active", Text: "tent", Created: old})
	for i := range bearSearchLimit + 5 {
		f.put(bearNote{ID: fmt.Sprintf("UUID-%d-AB", i), Title: "Tent trashed", Text: "tent", Trashed: true})
	}
	got, err := f.bear().Search(t.Context(), "tent")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got, func(n NoteSummary) bool { return n.State == StateActive }) {
		t.Fatalf("the active note was cut by the row cap (%d results)", len(got))
	}
}

// Bear's database is read from a private copy that is gone after each
// read, and the copy carries writes still only in Bear's WAL.
func TestBearReadsAPrivateCopyIncludingTheWAL(t *testing.T) {
	f := newBearFixture(t)
	if _, err := f.db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	n := f.put(bearNote{Title: "Tent", Text: "stakes"}) // only in the WAL while f.db is open
	b := f.bear()
	b.TempDir = t.TempDir()
	got, err := b.Search(t.Context(), "tent")
	if err != nil || len(got) != 1 {
		t.Fatalf("search = %v, %v; want the note still in the WAL", ids(got), err)
	}
	if _, err := b.Read(t.Context(), n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(b.TempDir)
	if err != nil || len(left) != 0 {
		t.Fatalf("copies left behind: %v %v", left, err)
	}
}

// Reads under one snapshot share a single copy, removed on release.
func TestBearSnapshotSharesOneCopyPerRequest(t *testing.T) {
	f := newBearFixture(t)
	f.put(bearNote{Title: "Tent", Text: "stakes"})
	b := f.bear()
	b.TempDir = t.TempDir()
	copies := 0
	b.copied = func() { copies++ }
	ctx, release := b.Snapshot(t.Context())
	for _, q := range []string{"tent", "stakes", "lamp"} {
		if _, err := b.Search(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if copies != 1 {
		t.Fatalf("three searches in one request copied the library %d times, want once", copies)
	}
	release()
	if left, _ := os.ReadDir(b.TempDir); len(left) != 0 {
		t.Fatalf("the request's copy was left behind: %v", left)
	}
	if _, err := b.Search(t.Context(), "tent"); err != nil || copies != 2 {
		t.Fatalf("a read outside the snapshot: copies=%d err=%v", copies, err)
	}
}

// A copy taken while Bear wrote is taken again, so the database and its
// WAL never come from different moments; a database that never settles
// is a transient failure.
func TestBearCopyTakenDuringAWriteIsRetaken(t *testing.T) {
	f := newBearFixture(t)
	if _, err := f.db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	f.put(bearNote{Title: "Tent", Text: "stakes"})
	b := f.bear()
	b.TempDir = t.TempDir()
	copies := 0
	b.copied = func() {
		copies++
		if copies == 1 {
			f.put(bearNote{Title: "Tent two", Text: "stakes"}) // Bear writes mid-copy
		}
	}
	got, err := b.Search(t.Context(), "tent")
	if err != nil || copies != 2 || len(got) != 2 {
		t.Fatalf("search = %v, %v after %d copies; want both notes from a second copy", ids(got), err, copies)
	}
	b.copied = func() { f.put(bearNote{Title: "Tent more", Text: "x"}) }
	if _, err := b.Search(t.Context(), "tent"); errorCode(err) != codeUnavailable || IsPermanent(err) {
		t.Fatalf("always-changing database = %v, want a transient failure", err)
	}
	if left, _ := os.ReadDir(b.TempDir); len(left) != 0 {
		t.Fatalf("discarded copies were left behind: %v", left)
	}
}

// Bear rewrites a note's creation date when it trashes it, so a
// same-titled note trashed while an add waits looks newly created. It is
// never the add's note.
func TestBearNoteTrashedDuringTheAddIsNotACandidate(t *testing.T) {
	fastBearPoll(t, time.Second)
	b, app := newBearWithApp(t)
	app.f.put(bearNote{Title: "Tent", Text: "an older note", Trashed: true, Created: time.Now()})
	res, err := b.Create(t.Context(), "Tent", "stakes", nil, nil, "tincan:r1")
	if err != nil || res.Note.Summary.ID == "" {
		t.Fatalf("create = %+v, %v; want the new note, not create_unconfirmed", res, err)
	}
}
