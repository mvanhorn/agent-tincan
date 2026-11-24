package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func keyed(from, to, body, key string) envelope.Request {
	return envelope.Request{From: from, To: to, Kind: envelope.KindAsk, Body: body, Hop: 1, Chain: []string{from}, IdempotencyKey: key, SendHash: "hash-of-" + body}
}

// TestEnqueueIdempotencyKey: a second Enqueue under a sender's used key
// stores nothing and returns ErrDuplicateSend, and SentWithKey finds the
// first. Another sender's same key, or no key, stores a new request.
func TestEnqueueIdempotencyKey(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	first, err := s.Enqueue(ctx, keyed("grokbot", "instinct", "deploy", "k1"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, keyed("grokbot", "muse", "something else", "k1"), time.Hour); !errors.Is(err, ErrDuplicateSend) {
		t.Fatalf("second enqueue under a used key: %v, want ErrDuplicateSend", err)
	}
	got, err := s.SentWithKey(ctx, "grokbot", "k1")
	if err != nil || got.ID != first.ID || got.Body != "deploy" || got.To != "instinct" || got.Status != envelope.StatusQueued ||
		got.IdempotencyKey != "k1" || got.SendHash != "hash-of-deploy" {
		t.Fatalf("SentWithKey = %+v, %v; want %s queued with its key and digest", got, err, first.ID)
	}
	if _, err := s.SentWithKey(ctx, "muse", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SentWithKey for another sender: %v, want ErrNotFound", err)
	}
	if other, err := s.Enqueue(ctx, keyed("muse", "instinct", "deploy", "k1"), time.Hour); err != nil || other.ID == first.ID {
		t.Fatalf("another sender's same key: %+v, %v", other, err)
	}
	for range 2 {
		if _, err := s.Enqueue(ctx, keyed("grokbot", "instinct", "deploy", ""), time.Hour); err != nil {
			t.Fatalf("enqueue without a key: %v", err)
		}
	}
	if _, err := s.SentWithKey(ctx, "grokbot", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SentWithKey with no key: %v, want ErrNotFound", err)
	}
	if n, err := s.CountQueued(ctx, "instinct"); err != nil || n != 4 {
		t.Fatalf("instinct has %d queued (%v), want 4: grokbot k1, muse k1, two without a key", n, err)
	}
}

// TestIdempotencyKeyUniqueIndex: the database itself refuses a second row
// with one sender and key, and allows any number of rows without a key,
// such as the rows a relay that predates the column inserts.
func TestIdempotencyKeyUniqueIndex(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	if _, err := s.Enqueue(ctx, keyed("grokbot", "instinct", "deploy", "k1"), time.Hour); err != nil {
		t.Fatal(err)
	}
	insert := func(id, from, key string) error {
		_, err := s.DB().ExecContext(ctx, `INSERT INTO requests
			(id, from_agent, to_agent, trace_id, hop, chain, kind, body, status, created_at, updated_at, expires_at, idempotency_key)
			VALUES (?, ?, 'instinct', ?, 1, '[]', 'ask', 'x', 'queued', 0, 0, 0, ?)`, id, from, id, key)
		return err
	}
	if err := insert("raw-dup", "grokbot", "k1"); err == nil {
		t.Fatal("a second row under grokbot's key was stored; the unique index is missing")
	}
	if err := insert("raw-muse", "muse", "k1"); err != nil {
		t.Fatalf("another sender's same key: %v", err)
	}
	if err := insert("raw-a", "grokbot", ""); err != nil {
		t.Fatalf("row without a key: %v", err)
	}
	if err := insert("raw-b", "grokbot", ""); err != nil {
		t.Fatalf("second row without a key: %v", err)
	}
	// A row written without naming the column, as an older relay writes.
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO requests (id, from_agent, to_agent, trace_id, hop, chain, kind, body, status, created_at, updated_at, expires_at)
		VALUES ('old', 'grokbot', 'instinct', 'old', 1, '[]', 'ask', 'x', 'queued', 0, 0, 0)`); err != nil {
		t.Fatalf("row without the key columns: %v", err)
	}
}

// TestIdempotencyKeyMigratesOldDatabase: a database from before the key
// gains its columns and index on open, keeps its requests, and refuses a
// duplicate afterwards.
func TestIdempotencyKeyMigratesOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := context.Background()
	old := ask(t, s, "grokbot", "instinct", "before the upgrade")
	for _, stmt := range []string{`DROP INDEX requests_idempotency`, `ALTER TABLE requests DROP COLUMN idempotency_key`, `ALTER TABLE requests DROP COLUMN send_hash`} {
		if _, err := s.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s, _ = open(t, path)
	if _, err := s.Get(ctx, old.ID, "grokbot"); err != nil {
		t.Fatalf("old request after the migration: %v", err)
	}
	if _, err := s.Enqueue(ctx, keyed("grokbot", "instinct", "deploy", "k1"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, keyed("grokbot", "instinct", "deploy", "k1"), time.Hour); !errors.Is(err, ErrDuplicateSend) {
		t.Fatalf("after the migration: %v, want ErrDuplicateSend", err)
	}
}

// TestDuplicateSendFilesNoTask: an ask is filed on the task list in the
// transaction that queues it, so a duplicate under a used key rolls back the
// task it would have filed.
func TestDuplicateSendFilesNoTask(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	first, err := s.EnqueueFilingTasks(ctx, keyed("grokbot", "instinct", "deploy", "k1"), time.Hour, false)
	if err != nil || first.TaskID == 0 {
		t.Fatalf("first keyed ask = %+v, %v, want a filed task", first, err)
	}
	if _, err := s.EnqueueFilingTasks(ctx, keyed("grokbot", "instinct", "deploy", "k1"), time.Hour, false); !errors.Is(err, ErrDuplicateSend) {
		t.Fatalf("duplicate: %v, want ErrDuplicateSend", err)
	}
	var tasks int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("%d tasks (%v) after a duplicate, want 1", tasks, err)
	}
}
