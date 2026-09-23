package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func reply(t *testing.T, s *Store, req envelope.Request, status envelope.Status, body string) {
	t.Helper()
	if _, err := s.Reply(context.Background(), req.ID, req.To, envelope.Reply{Status: status, Body: body}); err != nil {
		t.Fatal(err)
	}
}

func unseenIDs(t *testing.T, s *Store, agent string) []string {
	t.Helper()
	got, err := s.UnseenReplies(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.Request.ID)
	}
	return ids
}

// A reply starts unseen by the asker, comes back with its body and status
// oldest first, and stays unseen until the asker marks it.
func TestUnseenRepliesAndMarkSeen(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	first := ask(t, s, "hermes", "muse", "call the garage")
	second := ask(t, s, "hermes", "instinct", "check the calendar")
	pending := ask(t, s, "hermes", "grokbot", "still thinking")
	other := ask(t, s, "grokbot", "muse", "not hermes's ask")
	if ids := unseenIDs(t, s, "hermes"); len(ids) != 0 {
		t.Fatalf("unseen before any reply = %v", ids)
	}
	reply(t, s, second, envelope.StatusDeclined, "busy")
	c.advance(time.Second)
	reply(t, s, first, envelope.StatusAnswered, "Tue 3pm")
	reply(t, s, other, envelope.StatusAnswered, "done")

	got, err := s.UnseenReplies(ctx, "hermes")
	if err != nil || len(got) != 2 {
		t.Fatalf("unseen = %+v, %v", got, err)
	}
	if got[0].Request.ID != second.ID || got[0].Status != envelope.StatusDeclined || got[0].Reply == nil || got[0].Reply.Body != "busy" || got[0].Reply.From != "instinct" {
		t.Fatalf("first unseen = %+v", got[0])
	}
	if got[1].Request.ID != first.ID || got[1].Reply.Body != "Tue 3pm" {
		t.Fatalf("second unseen = %+v", got[1])
	}
	if n, err := s.CountUnseenReplies(ctx, "hermes"); err != nil || n != 2 {
		t.Fatalf("count = %d, %v", n, err)
	}
	// Only the asker can mark its replies; ids it did not send are ignored.
	if err := s.MarkRepliesSeen(ctx, "grokbot", []string{first.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRepliesSeen(ctx, "hermes", []string{second.ID, other.ID, pending.ID}); err != nil {
		t.Fatal(err)
	}
	if ids := unseenIDs(t, s, "hermes"); len(ids) != 1 || ids[0] != first.ID {
		t.Fatalf("unseen after mark = %v", ids)
	}
	if ids := unseenIDs(t, s, "grokbot"); len(ids) != 1 || ids[0] != other.ID {
		t.Fatalf("grokbot unseen = %v", ids)
	}
	if err := s.MarkRepliesSeen(ctx, "hermes", nil); err != nil {
		t.Fatalf("empty mark: %v", err)
	}
	// A reply to a request marked before it was answered still starts unseen.
	c.advance(time.Second) // order after first's reply, not by random id
	reply(t, s, pending, envelope.StatusFailed, "gave up")
	if ids := unseenIDs(t, s, "hermes"); len(ids) != 2 || ids[1] != pending.ID {
		t.Fatalf("unseen after late reply = %v", ids)
	}
}

// Get by the asker marks a returned reply seen; Get by the target, or before
// a reply exists, does not.
func TestGetByAskerMarksReplySeen(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "hermes", "muse", "call the garage")
	if _, err := s.Get(ctx, req.ID, "hermes"); err != nil {
		t.Fatal(err)
	}
	reply(t, s, req, envelope.StatusAnswered, "Tue 3pm")
	if _, err := s.Get(ctx, req.ID, "muse"); err != nil {
		t.Fatal(err)
	}
	if ids := unseenIDs(t, s, "hermes"); len(ids) != 1 {
		t.Fatalf("target's get must not mark: unseen = %v", ids)
	}
	res, err := s.Get(ctx, req.ID, "hermes")
	if err != nil || res.Reply == nil {
		t.Fatalf("get = %+v, %v", res, err)
	}
	if ids := unseenIDs(t, s, "hermes"); len(ids) != 0 {
		t.Fatalf("asker's get should mark the reply seen: unseen = %v", ids)
	}
}

// Cancelled and expired requests have no reply, so they are never unseen.
func TestUnseenRepliesSkipsCancelledAndExpired(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	cancelled := ask(t, s, "hermes", "muse", "never mind")
	if err := s.Cancel(ctx, cancelled.ID, "hermes"); err != nil {
		t.Fatal(err)
	}
	ask(t, s, "hermes", "instinct", "too slow")
	c.advance(2 * time.Hour)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountUnseenReplies(ctx, "hermes"); n != 0 {
		t.Fatalf("unseen = %d, want 0", n)
	}
}

func TestUnseenRepliesIsBounded(t *testing.T) {
	s, _ := open(t, ":memory:")
	for range MaxUnseenReplies + 5 {
		reply(t, s, ask(t, s, "hermes", "muse", "x"), envelope.StatusAnswered, "y")
	}
	if ids := unseenIDs(t, s, "hermes"); len(ids) != MaxUnseenReplies {
		t.Fatalf("unseen = %d, want %d", len(ids), MaxUnseenReplies)
	}
	if n, _ := s.CountUnseenReplies(context.Background(), "hermes"); n != MaxUnseenReplies+5 {
		t.Fatalf("count = %d, want all %d", n, MaxUnseenReplies+5)
	}
}

// A requests table from before reply tracking gains reply_seen_at on open.
// Replies already stored count as seen, so an upgrade does not flood every
// asker's inbox with old answers; requests answered afterwards start unseen.
func TestOldRequestsTableMigratesReplySeen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE requests (id TEXT PRIMARY KEY, from_agent TEXT NOT NULL, to_agent TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
		  trace_id TEXT NOT NULL, hop INTEGER NOT NULL, chain TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL, status TEXT NOT NULL,
		  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, lease_until INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE replies (request_id TEXT PRIMARY KEY REFERENCES requests(id), from_agent TEXT NOT NULL, status TEXT NOT NULL,
		  body TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`INSERT INTO requests VALUES ('old1', 'hermes', 'muse', '', 'old1', 1, '["hermes"]', 'ask', 'q', 'answered', 1790000000000, 1790000001000, 1790086400000, 0)`,
		`INSERT INTO replies VALUES ('old1', 'muse', 'answered', 'a', 1790000001000)`,
		`INSERT INTO requests VALUES ('old2', 'hermes', 'muse', '', 'old2', 1, '["hermes"]', 'ask', 'q2', 'claimed', 1790000000000, 1790000001000, 1890086400000, 0)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, _ := open(t, path)
	ctx := context.Background()
	if ids := unseenIDs(t, s, "hermes"); len(ids) != 0 {
		t.Fatalf("old replies should migrate as seen: unseen = %v", ids)
	}
	if _, err := s.Reply(ctx, "old2", "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "late"}); err != nil {
		t.Fatal(err)
	}
	if ids := unseenIDs(t, s, "hermes"); len(ids) != 1 || ids[0] != "old2" {
		t.Fatalf("unseen after a new reply = %v", ids)
	}
	s.Close()
	// Reopening an already-migrated database is a no-op and keeps the state.
	s2, _ := open(t, path)
	if ids := unseenIDs(t, s2, "hermes"); len(ids) != 1 || ids[0] != "old2" {
		t.Fatalf("unseen after reopen = %v", ids)
	}
}

// AgentsWithUnseenReplies names each asker holding an unseen reply once,
// so a restarted relay can reschedule their reply wakes.
func TestAgentsWithUnseenReplies(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	a := ask(t, s, "hermes", "muse", "one")
	b := ask(t, s, "hermes", "muse", "two")
	c := ask(t, s, "grokbot", "muse", "three")
	ask(t, s, "instinct", "muse", "no reply yet")
	for _, r := range []envelope.Request{a, b, c} {
		reply(t, s, r, envelope.StatusAnswered, "ok")
	}
	if err := s.MarkRepliesSeen(ctx, "grokbot", []string{c.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentsWithUnseenReplies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "hermes" {
		t.Fatalf("agents = %v, want [hermes]", got)
	}
}
