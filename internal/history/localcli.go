package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // the database/sql "sqlite" driver
)

// cliSession is one session of a local coding CLI's store, before its
// messages are read.
type cliSession struct {
	id, title, cwd string
	updated        time.Time
	// automated is set when the store itself marks the session as not
	// typed by the owner.
	automated bool
	// load reads the session's turns, and a title when the store keeps
	// one with the messages rather than in the session list.
	load func() ([]turn, string, error)
}

// cliStore answers List and Read for a local CLI from its sessions, with
// the Grok CLI reader's exclusions: the history service's scratch dir is
// never reported, and wake runs (an id in a <name>.wake-sessions list, or
// a cwd in a wake's workdir or home) only with Options.All.
type cliStore struct {
	source     Source
	wakeDir    string
	scratchDir string
	window     Window
	now        func() time.Time
	// sessions runs fn on the CLI's sessions, newest first.
	sessions func(ctx context.Context, fn func(cands []cliSession) error) error
}

// fromCandidates adapts a function that returns the sessions to
// cliStore.sessions.
func fromCandidates(cands func(context.Context) ([]cliSession, error)) func(context.Context, func([]cliSession) error) error {
	return func(ctx context.Context, fn func([]cliSession) error) error {
		c, err := cands(ctx)
		if err != nil {
			return err
		}
		return fn(c)
	}
}

// withSQLiteSessions runs fn on the sessions load reads from a copy of the
// SQLite database at path (withSQLiteCopy), newest first. A missing
// database, or one with no sessions, is no history for label.
func withSQLiteSessions(ctx context.Context, label, path, tempDir string, load func(context.Context, *sql.DB) ([]cliSession, error), fn func(cands []cliSession) error) error {
	err := withSQLiteCopy(path, tempDir, func(db *sql.DB) error {
		cands, err := load(ctx, db)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return noHistory(label, path)
		}
		return fn(cands)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return noHistory(label, path)
	}
	return err
}

// List implements Reader.List for a local CLI.
func (s cliStore) List(ctx context.Context, count int, opts Options) (page Page, err error) {
	if err := checkListCount(count); err != nil {
		return Page{}, err
	}
	err = s.sessions(ctx, func(cands []cliSession) error {
		page, err = s.list(ctx, cands, count, opts)
		return err
	})
	return page, err
}

// Read implements Reader.Read for a local CLI.
func (s cliStore) Read(ctx context.Context, q Query, opts Options) (page Page, err error) {
	if err := readCLI(s.source, q); err != nil {
		return Page{}, err
	}
	err = s.sessions(ctx, func(cands []cliSession) error {
		page, err = s.read(ctx, cands, q, opts)
		return err
	})
	return page, err
}

func (s cliStore) thread(e cliSession, w wakeLists, all bool) (thread, bool, error) {
	if inDirResolved(e.cwd, s.scratchDir) {
		return thread{}, false, nil
	}
	conv := Conversation{Source: s.source, ID: e.id, Title: e.title, UpdatedAt: e.updated, Cwd: e.cwd}
	conv.Automated = e.automated || w.ids[e.id] || w.inWakeDir(e.cwd)
	if conv.Automated && !all {
		return thread{}, false, nil
	}
	turns, title, err := e.load()
	if err != nil {
		return thread{}, false, fmt.Errorf("%s: read session %s: %w", s.source, e.id, err)
	}
	if conv.Title == "" {
		conv.Title = title
	}
	if conv.Title == "" && len(turns) > 0 {
		conv.Title = titleFrom(turns[0].prompt.Text)
	}
	return thread{conv: conv, turns: turns}, true, nil
}

// list implements Reader.List over cands, which are newest first.
func (s cliStore) list(ctx context.Context, cands []cliSession, count int, opts Options) (Page, error) {
	wk := readWakes(s.wakeDir)
	w, now := effectiveWindow(s.window, opts), orNow(s.now)
	page := Page{Window: w}
	for _, e := range cands {
		if len(page.Conversations) >= count {
			break
		}
		if !w.fresh(e.updated, now) {
			page.Limited = LimitAge
			break
		}
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		th, ok, err := s.thread(e, wk, opts.All)
		if err != nil {
			return Page{}, err
		}
		if ok && len(th.turns) > 0 {
			page.Conversations = append(page.Conversations, th.conv)
		}
	}
	return page, nil
}

// read implements Reader.Read over cands, which are newest first.
func (s cliStore) read(ctx context.Context, cands []cliSession, q Query, opts Options) (Page, error) {
	wk := readWakes(s.wakeDir)
	if q.Mode == ModeConversation {
		for _, e := range cands {
			if e.id != q.ConversationID {
				continue
			}
			th, ok, err := s.thread(e, wk, opts.All)
			if err != nil {
				return Page{}, err
			}
			if ok {
				return Page{Conversations: []Conversation{conversationMessages(th, false)}, Window: effectiveWindow(s.window, opts)}, nil
			}
		}
		return Page{}, fmt.Errorf("%s: %w: %s", s.source, ErrNotFound, q.ConversationID)
	}
	return pick(q, effectiveWindow(s.window, opts), orNow(s.now), len(cands), false,
		func(i int) time.Time { return cands[i].updated },
		func(i int) (thread, bool, error) {
			if err := ctx.Err(); err != nil {
				return thread{}, false, err
			}
			th, ok, err := s.thread(cands[i], wk, opts.All)
			// A session with no typed prompt does not use up a window slot.
			return th, ok && len(th.turns) > 0, err
		})
}

// readCLI is the Reader.Read preamble shared by the local CLI readers.
func readCLI(want Source, q Query) error {
	if err := q.Validate(); err != nil {
		return err
	}
	return checkSource(want, q)
}

// withSQLiteCopy copies the SQLite database at path, with its -wal and
// -shm files when present, into a fresh 0700 directory under tempDir
// (os.TempDir() when empty), opens the copy and runs fn on it. The app
// that owns the store may hold it open with recent writes only in the
// WAL, which a read-only or immutable open of the original would miss;
// the original is never opened. The copy is removed when fn returns,
// whatever happened. A missing database is fs.ErrNotExist.
func withSQLiteCopy(path, tempDir string, fn func(db *sql.DB) error) (err error) {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(tempDir, "tincan-history-")
	if err != nil {
		return err
	}
	defer func() {
		if rerr := os.RemoveAll(dir); err == nil && rerr != nil {
			err = rerr
		}
	}()
	copyPath := filepath.Join(dir, "store.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFile(path+suffix, copyPath+suffix); err != nil {
			if suffix != "" && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
	}
	db, err := sql.Open("sqlite", copyPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA query_only=1"); err != nil {
		return err
	}
	return fn(db)
}

// copyFile copies src to a new 0600 file dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// msTime is a Unix millisecond time; zero stays zero.
func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
