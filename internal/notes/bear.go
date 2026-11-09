package notes

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"modernc.org/sqlite"
)

// bearDBRel is Bear's database under the home directory.
const bearDBRel = "Library/Group Containers/9K33E3U3T4.net.shinyfrog.bear/Application Data/database.sqlite"

// BearDBPath is Bear's database for the current user.
func BearDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, bearDBRel)
}

// bearCreatePoll bounds how long a create waits for Bear to write the note,
// and bearPollEvery how often it looks.
var (
	bearCreatePoll = 15 * time.Second
	bearPollEvery  = 300 * time.Millisecond
)

// bearSearchLimit caps the rows a search reads; the service shows at most
// MaxSearchCount of them.
const bearSearchLimit = 200

// codeCreateUnconfirmed is an add the app may have saved but whose note the
// service cannot identify. It is permanent: creating again could save the
// note twice.
const codeCreateUnconfirmed = "create_unconfirmed"

// Bear reads a private copy of Bear's database and creates notes through
// Bear's URL scheme, so the service never opens or writes Bear's files.
type Bear struct {
	// DB is Bear's database (BearDBPath when empty).
	DB string
	// TempDir holds each read's copy of the database (os.TempDir() when
	// empty).
	TempDir string
	// copied runs after each copy is taken, before it is checked; tests
	// use it to write to the database mid-copy.
	copied func()
	// Open opens a bear:// URL without bringing Bear forward. It is
	// open -g on macOS; tests replace it.
	Open   func(ctx context.Context, u string) error
	ledger *ledger
	now    func() time.Time
}

// NewBear builds the Bear backend, keeping its add ledger in appSupport.
func NewBear(appSupport string, logf func(string, ...any)) *Bear {
	return &Bear{ledger: newLedger(ledgerPath(appSupport, BackendBear), logf)}
}

var _ Backend = (*Bear)(nil)

func (b *Bear) Name() string { return BearName }

func (b *Bear) dbPath() string {
	if b.DB != "" {
		return b.DB
	}
	return BearDBPath()
}

func (b *Bear) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// foldFunc is the SQL function search matches through: Unicode case
// folding, which SQLite's own lower() and LIKE do only for ASCII.
const foldFunc = "tincan_fold"

var registerFold = sync.OnceValue(func() error {
	return sqlite.RegisterDeterministicScalarFunction(foldFunc, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		switch v := args[0].(type) {
		case string:
			return fold(v), nil
		case []byte:
			return fold(string(v)), nil
		}
		return "", nil
	})
})

// fold is the case-insensitive form search compares.
func fold(s string) string {
	return strings.Map(unicode.ToLower, s)
}

// bearCopyTries bounds how often open copies again when Bear wrote its
// database while it was being copied.
const bearCopyTries = 3

type bearSnapKey struct{}

// bearSnap is one request's copy of Bear's database, shared by every read
// in that request and removed when the request ends.
type bearSnap struct {
	mu   sync.Mutex
	db   *sql.DB
	done func()
}

// Snapshot makes every read under the returned context share one copy of
// Bear's database, made on first use. release removes it.
func (b *Bear) Snapshot(ctx context.Context) (context.Context, func()) {
	s := &bearSnap{}
	return context.WithValue(ctx, bearSnapKey{}, s), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.done != nil {
			s.done()
			s.db, s.done = nil, nil
		}
	}
}

// open returns a copy of Bear's database: the request's snapshot when ctx
// carries one, else a copy for this read alone. done releases the latter.
func (b *Bear) open(ctx context.Context) (db *sql.DB, done func(), err error) {
	s, _ := ctx.Value(bearSnapKey{}).(*bearSnap)
	if s == nil {
		return b.copyDB(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		if s.db, s.done, err = b.copyDB(ctx); err != nil {
			return nil, nil, err
		}
	}
	return s.db, func() {}, nil
}

// copyDB copies Bear's database, with its -wal file when present,
// into a fresh 0700 directory and opens the copy; Bear's own files are
// never opened by SQLite. A copy taken while Bear wrote (the database or
// WAL changed size or time meanwhile) is discarded and taken again, so the
// database and WAL always match. done closes the copy and removes it. A
// missing database is "Bear is not installed", and one macOS will not let
// this process read is permission_denied.
func (b *Bear) copyDB(ctx context.Context) (db *sql.DB, done func(), err error) {
	if err := registerFold(); err != nil {
		return nil, nil, &HelperError{Code: codeUnavailable, Message: "sqlite: " + err.Error()}
	}
	path := b.dbPath()
	for range bearCopyTries {
		before := b.dbStamp()
		dir, err := os.MkdirTemp(b.TempDir, "tincan-bear-")
		if err != nil {
			return nil, nil, &HelperError{Code: codeUnavailable, Message: err.Error()}
		}
		cleanup := func() { _ = os.RemoveAll(dir) }
		cp := filepath.Join(dir, "database.sqlite")
		// Not -shm: it is only an index into the WAL that Bear rewrites
		// freely, and a stale one can show a page from the wrong moment.
		// Without it SQLite rebuilds the index from the copied WAL.
		for _, suffix := range []string{"", "-wal"} {
			err := copyPrivate(ctx, path+suffix, cp+suffix)
			switch {
			case err == nil:
				continue
			case suffix != "" && errors.Is(err, fs.ErrNotExist):
				continue
			case errors.Is(err, fs.ErrNotExist):
				err = &HelperError{Code: codeUnavailable, Message: "no Bear database at " + path + "; is Bear installed and opened once?"}
			case errors.Is(err, fs.ErrPermission):
				err = &HelperError{Code: CodePermissionDenied, Message: "macOS does not let this process read Bear's database at " + path}
			default:
				err = &HelperError{Code: codeUnavailable, Message: err.Error()}
			}
			cleanup()
			return nil, nil, err
		}
		if b.copied != nil {
			b.copied()
		}
		if b.dbStamp() != before {
			cleanup() // Bear wrote meanwhile: the copies may not match
			continue
		}
		u := url.URL{Scheme: "file", Path: cp, RawQuery: "_pragma=query_only(1)&_pragma=busy_timeout(5000)"}
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			cleanup()
			return nil, nil, &HelperError{Code: codeUnavailable, Message: err.Error()}
		}
		db.SetMaxOpenConns(1)
		return db, func() { _ = db.Close(); cleanup() }, nil
	}
	return nil, nil, &HelperError{Code: codeUnavailable, Message: "Bear kept writing its database while it was being copied; try again"}
}

// copyPrivate copies src to a new 0600 file dst, stopping when ctx ends.
func copyPrivate(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, ctxReader{ctx, in}); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ctxReader fails its reads once ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// dbStamp changes when Bear writes its database or WAL, so a create poll
// copies the database again only when there may be a new note.
func (b *Bear) dbStamp() string {
	var s strings.Builder
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(b.dbPath() + suffix); err == nil {
			fmt.Fprintf(&s, "%d/%d;", fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return s.String()
}

// dbError classifies a query failure: a busy or briefly unreadable
// database (Bear writing, or a journal left by a crash) is transient.
func dbError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &HelperError{Code: codeUnavailable, Message: err.Error()}
	}
	msg := err.Error()
	if strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column") {
		return &HelperError{Code: codeUnavailable, Message: "this Bear version is not supported: " + msg}
	}
	return &HelperError{Code: codeUnavailable, Message: msg}
}

// bearRow is one ZSFNOTE row.
type bearRow struct {
	pk                        int64
	id, title, text           string
	trashed, archived, pinned bool
	locked                    bool
}

const bearCols = `Z_PK, ZUNIQUEIDENTIFIER, COALESCE(ZTITLE, ''), COALESCE(ZTEXT, ''), COALESCE(ZTRASHED, 0), COALESCE(ZARCHIVED, 0), COALESCE(ZPINNED, 0), COALESCE(ZENCRYPTED, 0) OR COALESCE(ZLOCKED, 0)`

func scanBear(rows *sql.Rows) (bearRow, error) {
	var r bearRow
	err := rows.Scan(&r.pk, &r.id, &r.title, &r.text, &r.trashed, &r.archived, &r.pinned, &r.locked)
	if r.locked {
		r.text = "" // a locked note's text is never used
	}
	return r, err
}

func (r bearRow) state() string {
	switch {
	case r.trashed:
		return StateTrashed
	case r.archived:
		return StateArchived
	}
	return StateActive
}

func (r bearRow) summary(tags []string, snippet string) NoteSummary {
	s := NoteSummary{ID: r.id, Title: r.title, State: r.state(), Pinned: r.pinned, Tags: tags, Snippet: snippet}
	if !r.locked {
		s.Hash = textHash(r.text)
	}
	return s
}

// Search returns the notes holding every word of query in the title or,
// for notes that are not locked, the text.
func (b *Bear) Search(ctx context.Context, query string) ([]NoteSummary, error) {
	words := strings.Fields(fold(query))
	if len(words) == 0 {
		return nil, nil
	}
	db, done, err := b.open(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	where := []string{"COALESCE(ZPERMANENTLYDELETED, 0) = 0"}
	var args []any
	for _, w := range words {
		where = append(where, fmt.Sprintf(`(instr(%[1]s(ZTITLE), ?) > 0 OR (COALESCE(ZENCRYPTED, 0) = 0 AND COALESCE(ZLOCKED, 0) = 0 AND instr(%[1]s(ZTEXT), ?) > 0))`, foldFunc))
		args = append(args, w, w)
	}
	q := `SELECT ` + bearCols + ` FROM ZSFNOTE WHERE ` + strings.Join(where, " AND ") +
		// Active notes first, so trashed and archived matches never push
		// them past the limit.
		fmt.Sprintf(` ORDER BY (COALESCE(ZTRASHED, 0) = 0 AND COALESCE(ZARCHIVED, 0) = 0) DESC, COALESCE(ZPINNED, 0) DESC, ZMODIFICATIONDATE DESC LIMIT %d`, bearSearchLimit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, dbError(err)
	}
	var found []bearRow
	for rows.Next() {
		r, err := scanBear(rows)
		if err != nil {
			rows.Close()
			return nil, dbError(err)
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, dbError(err)
	}
	rows.Close()
	tags, err := bearTags(ctx, db, found)
	if err != nil {
		return nil, err
	}
	out := make([]NoteSummary, 0, len(found))
	for _, r := range found {
		out = append(out, r.summary(tags[r.pk], snippetAround(r.text, words)))
	}
	return out, nil
}

// Read returns one note by its Bear id.
func (b *Bear) Read(ctx context.Context, id string) (Note, error) {
	db, done, err := b.open(ctx)
	if err != nil {
		return Note{}, err
	}
	defer done()
	r, ok, err := bearByID(ctx, db, id)
	if err != nil {
		return Note{}, err
	}
	if !ok {
		return Note{}, ErrNoteNotFound
	}
	tags, err := bearTags(ctx, db, []bearRow{r})
	if err != nil {
		return Note{}, err
	}
	n := Note{Summary: r.summary(tags[r.pk], "")}
	if r.locked {
		return n, ErrNoteLocked
	}
	n.Body = r.text
	return n, nil
}

func bearByID(ctx context.Context, db *sql.DB, id string) (bearRow, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+bearCols+` FROM ZSFNOTE WHERE ZUNIQUEIDENTIFIER = ? AND COALESCE(ZPERMANENTLYDELETED, 0) = 0 LIMIT 1`, id)
	if err != nil {
		return bearRow{}, false, dbError(err)
	}
	defer rows.Close()
	if !rows.Next() {
		return bearRow{}, false, dbError(rows.Err())
	}
	r, err := scanBear(rows)
	return r, err == nil, dbError(err)
}

// Probe counts Bear's notes, so doctor can tell the database is readable.
func (b *Bear) Probe(ctx context.Context) (int, error) {
	db, done, err := b.open(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ZSFNOTE WHERE COALESCE(ZPERMANENTLYDELETED, 0) = 0`).Scan(&n); err != nil {
		return 0, dbError(err)
	}
	return n, nil
}

// tagJoinRE is Bear's note-to-tag join table, Z_<n>TAGS. Bear also has
// Z_<n>PINNEDINTAGS, which is not the tag list.
var tagJoinRE = regexp.MustCompile(`^Z_[0-9]+TAGS$`)

// bearTags returns the tags of each row, by Z_PK. The join table's name
// and columns differ between Bear versions, so they are looked up.
func bearTags(ctx context.Context, db *sql.DB, rows []bearRow) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(rows) == 0 {
		return out, nil
	}
	table, noteCol, tagCol, err := bearTagJoin(ctx, db)
	if err != nil || table == "" {
		return out, err
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rows)), ",")
	args := make([]any, len(rows))
	for i, r := range rows {
		args[i] = r.pk
	}
	q := fmt.Sprintf(`SELECT j.%s, COALESCE(t.ZTITLE, '') FROM %s j JOIN ZSFNOTETAG t ON t.Z_PK = j.%s WHERE j.%s IN (%s) ORDER BY t.ZTITLE`, noteCol, table, tagCol, noteCol, ph)
	res, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, dbError(err)
	}
	defer res.Close()
	for res.Next() {
		var pk int64
		var tag string
		if err := res.Scan(&pk, &tag); err != nil {
			return nil, dbError(err)
		}
		if tag != "" {
			out[pk] = append(out[pk], tag)
		}
	}
	return out, dbError(res.Err())
}

// identRE limits what a discovered table or column name may be before it
// is put into a query.
var identRE = regexp.MustCompile(`^Z_?[0-9A-Z_]+$`)

func bearTagJoin(ctx context.Context, db *sql.DB) (table, noteCol, tagCol string, err error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'Z\_%TAGS' ESCAPE '\'`)
	if err != nil {
		return "", "", "", dbError(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil && tagJoinRE.MatchString(n) {
			names = append(names, n)
		}
	}
	rows.Close()
	if len(names) != 1 {
		return "", "", "", nil // no tag table, or one this build cannot tell apart: no tags
	}
	cols, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, names[0])
	if err != nil {
		return "", "", "", dbError(err)
	}
	defer cols.Close()
	for cols.Next() {
		var c string
		if cols.Scan(&c) != nil || !identRE.MatchString(c) {
			continue
		}
		switch {
		case strings.HasSuffix(c, "NOTES"):
			noteCol = c
		case strings.HasSuffix(c, "TAGS"):
			tagCol = c
		}
	}
	if noteCol == "" || tagCol == "" || !identRE.MatchString(names[0]) {
		return "", "", "", nil
	}
	return names[0], noteCol, tagCol, nil
}

// snippetAround is one line of text around the first folded word found,
// or the start of the text.
func snippetAround(text string, words []string) string {
	if text == "" {
		return ""
	}
	rs := []rune(text)
	folded := []rune(fold(text))
	at := 0
	if len(folded) == len(rs) {
		for _, w := range words {
			if i := runeIndex(folded, []rune(w)); i >= 0 {
				at = i
				break
			}
		}
	}
	from := max(0, at-60)
	to := min(len(rs), at+100)
	s := oneLine(string(rs[from:to]))
	if from > 0 {
		s = "..." + s
	}
	if to < len(rs) {
		s += "..."
	}
	return s
}

func runeIndex(s, sub []rune) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := range sub {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// coreDataEpoch is where Bear's timestamps start.
var coreDataEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// Create saves a note through bear://x-callback-url/create in the
// background. The add is recorded as pending before Bear is asked, so a
// retry after a crash or a slow Bear looks for that attempt's note and
// never asks Bear twice.
func (b *Bear) Create(ctx context.Context, title, body string, tags []string, props map[string]string, key string) (CreateResult, error) {
	e, ok, err := b.ledger.Lookup(key)
	if err != nil {
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: err.Error()}
	}
	if ok && e.State == ledgerDone {
		return b.existing(ctx, e.NoteID)
	}
	if ok && e.State == ledgerPending {
		// An earlier attempt asked Bear and its note was not found in time.
		id, err := b.awaitNote(ctx, e.Title, e.Start)
		if err != nil {
			return CreateResult{}, err
		}
		if id == "" {
			return CreateResult{}, &HelperError{Code: codeCreateUnconfirmed, Message: "Bear was asked to save the note but it has not appeared; it may still be in Bear under that title"}
		}
		if err := b.ledger.Done(key, id); err != nil {
			return CreateResult{}, &HelperError{Code: codeUnavailable, Message: err.Error()}
		}
		return b.found(ctx, id)
	}
	if runtime.GOOS != "darwin" && b.Open == nil {
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: "the Bear backend runs only on macOS"}
	}
	// Bear's own clock has whole-second precision on some versions.
	start := b.clock().Add(-2 * time.Second)
	if err := b.ledger.Begin(key, title, start, props); err != nil {
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: err.Error()}
	}
	if err := b.openURL(ctx, bearCreateURL(title, body, tags)); err != nil {
		// Bear was never asked, so a retry may ask again.
		_ = b.ledger.Forget(key)
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: "could not ask Bear to save the note: " + err.Error()}
	}
	id, err := b.awaitNote(ctx, title, start)
	if err != nil {
		return CreateResult{}, err
	}
	if id == "" {
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: "Bear has not saved the note yet"}
	}
	if err := b.ledger.Done(key, id); err != nil {
		return CreateResult{}, &HelperError{Code: codeUnavailable, Message: err.Error()}
	}
	res, err := b.found(ctx, id)
	res.Existed = false
	return res, err
}

func (b *Bear) openURL(ctx context.Context, u string) error {
	if b.Open != nil {
		return b.Open(ctx, u)
	}
	out, err := exec.CommandContext(ctx, "open", "-g", u).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// existing is a done add: the note it made, in whatever state it is in now.
func (b *Bear) existing(ctx context.Context, id string) (CreateResult, error) {
	res, err := b.found(ctx, id)
	if errors.Is(err, ErrNoteNotFound) {
		// The owner deleted it for good; the add still happened once.
		return CreateResult{Note: Note{Summary: NoteSummary{ID: id, State: StateDeleted}}, Existed: true}, nil
	}
	return res, err
}

func (b *Bear) found(ctx context.Context, id string) (CreateResult, error) {
	n, err := b.Read(ctx, id)
	if errors.Is(err, ErrNoteLocked) {
		err = nil
	}
	if err != nil {
		return CreateResult{}, err
	}
	n.Body = ""
	return CreateResult{Note: n, Existed: true}, nil
}

// bearCreateURL is the create URL. Spaces are %20: Bear reads + as a plus.
func bearCreateURL(title, body string, tags []string) string {
	esc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	var b strings.Builder
	b.WriteString("bear://x-callback-url/create?title=")
	b.WriteString(esc(title))
	b.WriteString("&text=")
	b.WriteString(esc(body))
	if len(tags) > 0 {
		b.WriteString("&tags=")
		b.WriteString(esc(strings.Join(tags, ",")))
	}
	b.WriteString("&open_note=no&show_window=no&new_window=no&edit=no")
	return b.String()
}

// awaitNote polls for the note an attempt started at start made: exactly
// one note created since then whose title matches. It returns "" when none
// appears in time, and fails when more than one could be it.
func (b *Bear) awaitNote(ctx context.Context, title string, start time.Time) (string, error) {
	want := normTitle(title)
	since := start.Sub(coreDataEpoch).Seconds()
	deadline := b.clock().Add(bearCreatePoll)
	seen := ""
	for {
		var ids []string
		if stamp := b.dbStamp(); stamp == "" || stamp != seen {
			db, done, err := b.open(ctx)
			if err != nil {
				return "", err
			}
			ids, err = createdSince(ctx, db, since, want)
			done()
			if err != nil {
				return "", err
			}
			seen = stamp
		}
		switch {
		case len(ids) == 1:
			return ids[0], nil
		case len(ids) > 1:
			return "", &HelperError{Code: codeCreateUnconfirmed, Message: fmt.Sprintf("%d Bear notes with this title were created at the same time, so the notes agent cannot tell which is this add's", len(ids))}
		}
		if !b.clock().Before(deadline) {
			return "", nil
		}
		select {
		case <-ctx.Done():
			return "", &HelperError{Code: codeUnavailable, Message: ctx.Err().Error()}
		case <-time.After(bearPollEvery):
		}
	}
}

func createdSince(ctx context.Context, db *sql.DB, since float64, title string) ([]string, error) {
	// A trashed note is never the one just made, and Bear rewrites a note's
	// creation date when it trashes it, so trashed notes are left out.
	rows, err := db.QueryContext(ctx, `SELECT ZUNIQUEIDENTIFIER, COALESCE(ZTITLE, '') FROM ZSFNOTE WHERE ZCREATIONDATE >= ? AND COALESCE(ZPERMANENTLYDELETED, 0) = 0 AND COALESCE(ZTRASHED, 0) = 0`, since)
	if err != nil {
		return nil, dbError(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, dbError(err)
		}
		if normTitle(t) == title {
			ids = append(ids, id)
		}
	}
	return ids, dbError(rows.Err())
}

// normTitle is a title as compared when looking for a created note: Bear
// may keep it as a "# " heading, so leading #s, outer space and runs of
// space do not count, nor does case.
func normTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "#")
	return fold(strings.Join(strings.Fields(s), " "))
}
