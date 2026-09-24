package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

var bigQuota = AttachmentQuota{PerAgent: 1 << 30, Total: 1 << 40}

// upload reserves and commits an attachment of size bytes for uploader.
func upload(t *testing.T, s *Store, uploader, name string, size int64) string {
	t.Helper()
	ctx := context.Background()
	id, err := s.ReserveAttachment(ctx, uploader, name, size, bigQuota)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitAttachment(ctx, id, size, "image/png", "abc123"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAttachmentReserveCommitRoundTrip(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	id, err := s.ReserveAttachment(ctx, "history", "cat.png", 10<<20, bigQuota)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Attachment(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Ready || a.Uploader != "history" || a.Size != 10<<20 {
		t.Fatalf("reserved = %+v, want a pending 10 MB reservation by history", a)
	}
	if err := s.CommitAttachment(ctx, id, 1234, "image/png", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	a, err = s.Attachment(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := AttachmentRecord{
		Attachment: envelope.Attachment{ID: id, Name: "cat.png", MIME: "image/png", Size: 1234},
		SHA256:     "deadbeef", Uploader: "history", Ready: true, CreatedAt: c.t.UTC().Truncate(time.Millisecond),
	}
	if a != want {
		t.Fatalf("committed =\n%+v\nwant\n%+v", a, want)
	}
	if _, err := s.Attachment(ctx, "nope"); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("missing: err = %v", err)
	}
	if err := s.DropAttachment(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attachment(ctx, id); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("dropped: err = %v", err)
	}
}

// Reservations count against the quota, so two concurrent uploads cannot
// both squeeze under it. Committing replaces the reservation with the real
// size.
func TestAttachmentQuotas(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	q := AttachmentQuota{PerAgent: 100, Total: 150}
	a, err := s.ReserveAttachment(ctx, "grokbot", "a", 60, q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveAttachment(ctx, "grokbot", "b", 41, q); !errors.Is(err, ErrAgentQuota) {
		t.Fatalf("over the agent quota: err = %v, want ErrAgentQuota", err)
	}
	if _, err := s.ReserveAttachment(ctx, "grokbot", "b", 40, q); err != nil {
		t.Fatalf("exactly at the agent quota: %v", err)
	}
	// grokbot holds 100 of the relay's 150.
	if _, err := s.ReserveAttachment(ctx, "muse", "c", 51, q); !errors.Is(err, ErrRelayQuota) {
		t.Fatalf("over the relay quota: err = %v, want ErrRelayQuota", err)
	}
	if err := s.CommitAttachment(ctx, a, 10, "text/plain", "x"); err != nil {
		t.Fatal(err)
	}
	// The commit shrank a from 60 to 10, freeing 50.
	if _, err := s.ReserveAttachment(ctx, "muse", "c", 50, q); err != nil {
		t.Fatalf("after the commit shrank a reservation: %v", err)
	}
	// grokbot 50 plus muse 50 leaves room for a zero-byte file.
	if _, err := s.ReserveAttachment(ctx, "muse", "d", 0, q); err != nil {
		t.Fatalf("zero-byte reservation: %v", err)
	}
}

func TestEnqueueBindsAttachments(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	a := upload(t, s, "history", "cat.png", 1234)
	b := upload(t, s, "history", "notes.txt", 10)
	req, err := s.Enqueue(ctx, envelope.Request{From: "history", To: "grokbot", Kind: envelope.KindAsk, Body: "here", Hop: 1, Chain: []string{"history"},
		Attachments: []envelope.Attachment{{ID: a}, {ID: b, Name: "forged.exe", MIME: "text/html", Size: 1}}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := []envelope.Attachment{{ID: a, Name: "cat.png", MIME: "image/png", Size: 1234}, {ID: b, Name: "notes.txt", MIME: "image/png", Size: 10}}
	if !slices.Equal(req.Attachments, want) {
		t.Fatalf("enqueued attachments = %+v, want %+v", req.Attachments, want)
	}
	rec, _ := s.Attachment(ctx, a)
	if rec.RequestID != req.ID {
		t.Fatalf("attachment request = %q, want %q", rec.RequestID, req.ID)
	}
	// Every read path returns them.
	got, _, err := s.Request(ctx, req.ID)
	if err != nil || !slices.Equal(got.Attachments, want) {
		t.Fatalf("Request attachments = %+v, %v", got.Attachments, err)
	}
	res, err := s.Get(ctx, req.ID, "grokbot")
	if err != nil || !slices.Equal(res.Request.Attachments, want) {
		t.Fatalf("Get attachments = %+v, %v", res.Request.Attachments, err)
	}
	delivered, err := s.Deliver(ctx, "grokbot", 10, time.Minute)
	if err != nil || len(delivered) != 1 || !slices.Equal(delivered[0].Attachments, want) {
		t.Fatalf("Deliver = %+v, %v", delivered, err)
	}
	steps, err := s.Trace(ctx, req.TraceID)
	if err != nil || len(steps) != 1 || !slices.Equal(steps[0].Request.Attachments, want) {
		t.Fatalf("Trace = %+v, %v", steps, err)
	}
}

// A request without attachments stores and reads back as before.
func TestEnqueueWithoutAttachmentsUnchanged(t *testing.T) {
	s, _ := open(t, ":memory:")
	req := ask(t, s, "grokbot", "muse", "hi")
	got, _, err := s.Request(context.Background(), req.ID)
	if err != nil || got.Attachments != nil {
		t.Fatalf("attachments = %+v, %v; want nil", got.Attachments, err)
	}
}

func TestEnqueueRejectsBadAttachmentReferences(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	mine := upload(t, s, "grokbot", "a.png", 5)
	theirs := upload(t, s, "muse", "b.png", 5)
	pending, err := s.ReserveAttachment(ctx, "grokbot", "c.png", 5, bigQuota)
	if err != nil {
		t.Fatal(err)
	}
	send := func(ids ...string) error {
		var atts []envelope.Attachment
		for _, id := range ids {
			atts = append(atts, envelope.Attachment{ID: id})
		}
		_, err := s.Enqueue(ctx, envelope.Request{From: "grokbot", To: "instinct", Kind: envelope.KindAsk, Body: "x", Hop: 1, Chain: []string{}, Attachments: atts}, time.Hour)
		return err
	}
	if err := send(mine, theirs); !errors.Is(err, ErrAttachmentNotYours) {
		t.Fatalf("another agent's upload: err = %v, want ErrAttachmentNotYours", err)
	}
	if err := send("nope"); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("unknown id: err = %v", err)
	}
	if err := send(pending); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("unfinished upload: err = %v", err)
	}
	// The failed sends stored nothing and left mine unbound.
	if n, _ := s.CountQueued(ctx, "instinct"); n != 0 {
		t.Fatalf("failed sends queued %d requests", n)
	}
	if rec, _ := s.Attachment(ctx, mine); rec.RequestID != "" {
		t.Fatalf("a failed send bound %s to %s", mine, rec.RequestID)
	}
	if err := send(mine); err != nil {
		t.Fatal(err)
	}
	if err := send(mine); !errors.Is(err, ErrAttachmentInUse) {
		t.Fatalf("reuse: err = %v, want ErrAttachmentInUse", err)
	}
}

func TestReplyBindsAttachments(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "history", "send the image")
	img := upload(t, s, "history", "cat.png", 99)
	theirs := upload(t, s, "grokbot", "x.png", 1)
	if _, err := s.Reply(ctx, req.ID, "history", envelope.Reply{Status: envelope.StatusAnswered, Body: "here", Attachments: []envelope.Attachment{{ID: theirs}}}); !errors.Is(err, ErrAttachmentNotYours) {
		t.Fatalf("reply with another agent's upload: err = %v", err)
	}
	if _, st, _ := s.Request(ctx, req.ID); st != envelope.StatusQueued {
		t.Fatalf("a rejected reply closed the request: %s", st)
	}
	rep, err := s.Reply(ctx, req.ID, "history", envelope.Reply{Status: envelope.StatusAnswered, Body: "here", Attachments: []envelope.Attachment{{ID: img}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []envelope.Attachment{{ID: img, Name: "cat.png", MIME: "image/png", Size: 99}}
	if !slices.Equal(rep.Attachments, want) {
		t.Fatalf("reply attachments = %+v", rep.Attachments)
	}
	unseen, err := s.UnseenReplies(ctx, "grokbot")
	if err != nil || len(unseen) != 1 || !slices.Equal(unseen[0].Reply.Attachments, want) {
		t.Fatalf("UnseenReplies = %+v, %v", unseen, err)
	}
	res, err := s.Get(ctx, req.ID, "grokbot")
	if err != nil || !slices.Equal(res.Reply.Attachments, want) {
		t.Fatalf("Get reply = %+v, %v", res.Reply, err)
	}
	if rec, _ := s.Attachment(ctx, img); rec.RequestID != req.ID {
		t.Fatalf("reply attachment bound to %q, want %q", rec.RequestID, req.ID)
	}
}

// The sweep drops uploads never attached after the orphan age, and deletes
// blobs a retention period after their request reaches a final state,
// keeping the metadata row marked deleted. Open requests keep theirs.
func TestSweepAttachments(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	orphan := upload(t, s, "history", "orphan.png", 1)
	stale, err := s.ReserveAttachment(ctx, "history", "crashed.png", 1, bigQuota) // upload that never finished
	if err != nil {
		t.Fatal(err)
	}
	answered := upload(t, s, "history", "answered.png", 1)
	open := upload(t, s, "history", "open.png", 1)
	cancelled := upload(t, s, "grokbot", "cancelled.png", 1)

	r1 := ask(t, s, "grokbot", "history", "one")
	if _, err := s.Reply(ctx, r1.ID, "history", envelope.Reply{Status: envelope.StatusAnswered, Body: "ok", Attachments: []envelope.Attachment{{ID: answered}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, envelope.Request{From: "history", To: "grokbot", Kind: envelope.KindAsk, Body: "open", Hop: 1, Chain: []string{}, Attachments: []envelope.Attachment{{ID: open}}}, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	r3, err := s.Enqueue(ctx, envelope.Request{From: "grokbot", To: "history", Kind: envelope.KindAsk, Body: "x", Hop: 1, Chain: []string{}, Attachments: []envelope.Attachment{{ID: cancelled}}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, r3.ID, "grokbot"); err != nil {
		t.Fatal(err)
	}

	sweep := func() []string {
		t.Helper()
		ids, err := s.SweepAttachments(ctx, 24*time.Hour, 7*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(ids)
		return ids
	}
	c.advance(23 * time.Hour)
	if ids := sweep(); len(ids) != 0 {
		t.Fatalf("swept %v before anything aged out", ids)
	}
	c.advance(2 * time.Hour) // 25h
	want := []string{orphan, stale}
	slices.Sort(want)
	if ids := sweep(); !slices.Equal(ids, want) {
		t.Fatalf("after 25h swept %v, want the orphans %v", ids, want)
	}
	if _, err := s.Attachment(ctx, orphan); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("orphan row should be gone: %v", err)
	}
	c.advance(6 * 24 * time.Hour) // just over 7 days since the reply and cancel
	want = []string{answered, cancelled}
	slices.Sort(want)
	if ids := sweep(); !slices.Equal(ids, want) {
		t.Fatalf("after 7 days swept %v, want %v", ids, want)
	}
	for _, id := range want {
		rec, err := s.Attachment(ctx, id)
		if err != nil || rec.DeletedAt.IsZero() {
			t.Fatalf("%s: row = %+v, %v; want kept and marked deleted", id, rec, err)
		}
	}
	if rec, _ := s.Attachment(ctx, open); !rec.DeletedAt.IsZero() {
		t.Fatalf("attachment on an open request was deleted")
	}
	if ids := sweep(); len(ids) != 0 {
		t.Fatalf("second sweep returned %v", ids)
	}
	// The request still lists its attachment for the record.
	res, err := s.Get(ctx, r1.ID, "grokbot")
	if err != nil || len(res.Reply.Attachments) != 1 {
		t.Fatalf("reply after sweep = %+v, %v", res.Reply, err)
	}
}

// A claimed notify stays claimed forever (nothing replies to close it), so
// claimed is its final state: its attachments go keepAfterDone after the
// claim. A claimed ask is still open and keeps its attachments.
func TestSweepAttachmentsClaimedNotify(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	onNotify := upload(t, s, "history", "notify.png", 1)
	onAsk := upload(t, s, "history", "ask.png", 1)
	n, err := s.Enqueue(ctx, envelope.Request{From: "history", To: "grokbot", Kind: envelope.KindNotify, Body: "fyi", Hop: 1, Chain: []string{}, Attachments: []envelope.Attachment{{ID: onNotify}}}, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Enqueue(ctx, envelope.Request{From: "history", To: "grokbot", Kind: envelope.KindAsk, Body: "q", Hop: 1, Chain: []string{}, Attachments: []envelope.Attachment{{ID: onAsk}}}, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{n.ID, a.ID} {
		if _, err := s.Claim(ctx, id, "grokbot", 30*24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	sweep := func() []string {
		t.Helper()
		ids, err := s.SweepAttachments(ctx, 24*time.Hour, 7*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	c.advance(7*24*time.Hour - time.Minute)
	if ids := sweep(); len(ids) != 0 {
		t.Fatalf("swept %v before keepAfterDone", ids)
	}
	c.advance(2 * time.Minute)
	if ids := sweep(); !slices.Equal(ids, []string{onNotify}) {
		t.Fatalf("after keepAfterDone swept %v, want only the notify attachment %v", ids, onNotify)
	}
	if rec, err := s.Attachment(ctx, onNotify); err != nil || rec.DeletedAt.IsZero() {
		t.Fatalf("notify attachment row = %+v, %v; want kept and marked deleted", rec, err)
	}
	if rec, _ := s.Attachment(ctx, onAsk); !rec.DeletedAt.IsZero() {
		t.Fatalf("attachment on a claimed ask was deleted")
	}
	if ids := sweep(); len(ids) != 0 {
		t.Fatalf("second sweep returned %v", ids)
	}
	res, err := s.Get(ctx, n.ID, "history")
	if err != nil || len(res.Request.Attachments) != 1 {
		t.Fatalf("notify after sweep = %+v, %v", res.Request, err)
	}
}

// Deleted blobs no longer count against the quota.
func TestSweptAttachmentsFreeQuota(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	q := AttachmentQuota{PerAgent: 10, Total: 10}
	if _, err := s.ReserveAttachment(ctx, "history", "a", 10, q); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveAttachment(ctx, "history", "b", 1, q); !errors.Is(err, ErrAgentQuota) {
		t.Fatalf("err = %v", err)
	}
	c.advance(25 * time.Hour)
	if _, err := s.SweepAttachments(ctx, 24*time.Hour, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveAttachment(ctx, "history", "b", 10, q); err != nil {
		t.Fatalf("after the sweep: %v", err)
	}
}

// A database from before attachments gains the table and the columns on
// open, and its old rows read back with no attachments.
func TestOldStoreMigratesAttachments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE requests (id TEXT PRIMARY KEY, from_agent TEXT NOT NULL, to_agent TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
		  trace_id TEXT NOT NULL, hop INTEGER NOT NULL, chain TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL, status TEXT NOT NULL,
		  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, lease_until INTEGER NOT NULL DEFAULT 0,
		  reply_seen_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE replies (request_id TEXT PRIMARY KEY REFERENCES requests(id), from_agent TEXT NOT NULL, status TEXT NOT NULL,
		  body TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`INSERT INTO requests VALUES ('old1', 'grokbot', 'muse', '', 'old1', 1, '["grokbot"]', 'ask', 'q', 'answered', 1790000000000, 1790000001000, 1790086400000, 0, 0)`,
		`INSERT INTO replies VALUES ('old1', 'muse', 'answered', 'a', 1790000001000)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, _ := open(t, path)
	ctx := context.Background()
	res, err := s.Get(ctx, "old1", "grokbot")
	if err != nil || res.Request.Attachments != nil || res.Reply == nil || res.Reply.Attachments != nil {
		t.Fatalf("old row = %+v, %v", res, err)
	}
	id := upload(t, s, "grokbot", "a.png", 3)
	req, err := s.Enqueue(ctx, envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindAsk, Body: "x", Hop: 1, Chain: []string{}, Attachments: []envelope.Attachment{{ID: id}}}, time.Hour)
	if err != nil || len(req.Attachments) != 1 {
		t.Fatalf("enqueue after migration: %+v, %v", req, err)
	}
	s.Close()
	s2, _ := open(t, path) // reopening a migrated database is a no-op
	if got, _, err := s2.Request(ctx, req.ID); err != nil || len(got.Attachments) != 1 {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
	if s2.Path() != path {
		t.Fatalf("Path() = %q, want %q", s2.Path(), path)
	}
}

func TestMemoryStoreHasNoPath(t *testing.T) {
	s, _ := open(t, ":memory:")
	if s.Path() != "" {
		t.Fatalf("Path() = %q, want empty for an in-memory store", s.Path())
	}
}
