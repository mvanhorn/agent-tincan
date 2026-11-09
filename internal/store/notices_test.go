package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// relayNotice queues a relay notice to the asker of req, linked to req
// when kind is set.
func relayNotice(t *testing.T, s *Store, req envelope.Request, kind string) envelope.Request {
	t.Helper()
	n := envelope.Request{From: "relay", To: req.From, Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: "about " + req.ID}
	if kind != "" {
		n.NoticeFor, n.NoticeKind = req.ID, kind
	}
	n, err := s.Enqueue(context.Background(), n, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func statusIs(t *testing.T, s *Store, id string, want envelope.Status) {
	t.Helper()
	if _, st, err := s.Request(context.Background(), id); err != nil || st != want {
		t.Fatalf("%s: status %s %v, want %s", id, st, err, want)
	}
}

// A terminal reply closes the queued stale-claim notice about its request
// in the same write, and names it. Other notices, and a needs_input reply,
// leave notices alone.
func TestReplyClosesQueuedStaleClaimNotice(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "instinct", "grokbot", "find it")
	stale := relayNotice(t, s, req, NoteStaleClaim)
	other := relayNotice(t, s, req, "")
	if _, err := s.Claim(ctx, req.ID, "grokbot", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "grokbot", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which one?"}); err != nil {
		t.Fatal(err)
	}
	statusIs(t, s, stale.ID, envelope.StatusQueued)
	if _, err := s.Answer(ctx, req.ID, "instinct", "the blue one"); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Reply(ctx, req.ID, "grokbot", envelope.Reply{Status: envelope.StatusAnswered, Body: "found"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Superseded, []string{stale.ID}) {
		t.Fatalf("superseded = %v, want [%s]", rep.Superseded, stale.ID)
	}
	statusIs(t, s, stale.ID, envelope.StatusCancelled)
	got, err := s.Deliver(ctx, "instinct", 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != other.ID {
		t.Fatalf("delivered %+v %v, want only the unlinked notice", got, err)
	}
}

// Every terminal status closes the notice; one already delivered to the
// asker stays as it is.
func TestTerminalRepliesCloseOnlyQueuedNotices(t *testing.T) {
	for _, st := range []envelope.Status{envelope.StatusDeclined, envelope.StatusFailed} {
		s, _ := open(t, ":memory:")
		ctx := context.Background()
		req := ask(t, s, "instinct", "grokbot", "find it")
		n := relayNotice(t, s, req, NoteStaleClaim)
		rep, err := s.Reply(ctx, req.ID, "grokbot", envelope.Reply{Status: st, Body: "no"})
		if err != nil || len(rep.Superseded) != 1 {
			t.Fatalf("%s: superseded %v %v", st, rep.Superseded, err)
		}
		statusIs(t, s, n.ID, envelope.StatusCancelled)
	}
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "instinct", "grokbot", "find it")
	n := relayNotice(t, s, req, NoteStaleClaim)
	if got, err := s.Deliver(ctx, "instinct", 10, time.Minute); err != nil || len(got) != 1 {
		t.Fatalf("deliver %+v %v", got, err)
	}
	rep, err := s.Reply(ctx, req.ID, "grokbot", envelope.Reply{Status: envelope.StatusAnswered, Body: "found"})
	if err != nil || len(rep.Superseded) != 0 {
		t.Fatalf("superseded %v %v", rep.Superseded, err)
	}
	statusIs(t, s, n.ID, envelope.StatusDelivered)
}

// Cancelling a request closes its queued stale-claim notice too.
func TestCancelClosesQueuedStaleClaimNotice(t *testing.T) {
	s, _ := open(t, ":memory:")
	req := ask(t, s, "instinct", "grokbot", "find it")
	n := relayNotice(t, s, req, NoteStaleClaim)
	closed, err := s.Cancel(context.Background(), req.ID, "instinct")
	if err != nil || !slices.Equal(closed, []string{n.ID}) {
		t.Fatalf("closed %v %v, want [%s]", closed, err, n.ID)
	}
	statusIs(t, s, n.ID, envelope.StatusCancelled)
}

// A stale-claim notice for a request that has already closed, by a
// terminal reply or a cancel, is not queued: the sweep that decided to send
// it read the request before the reply landed.
func TestStaleClaimNoticeAfterCloseIsNotQueued(t *testing.T) {
	ctx := context.Background()
	for _, close := range []string{"reply", "cancel"} {
		t.Run(close, func(t *testing.T) {
			s, _ := open(t, ":memory:")
			req := ask(t, s, "instinct", "grokbot", "find it")
			if close == "reply" {
				if _, err := s.Claim(ctx, req.ID, "grokbot", time.Minute); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Reply(ctx, req.ID, "grokbot", envelope.Reply{Status: envelope.StatusAnswered, Body: "found"}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.Cancel(ctx, req.ID, "instinct"); err != nil {
				t.Fatal(err)
			}
			n := envelope.Request{From: "relay", To: req.From, Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: "about " + req.ID, NoticeFor: req.ID, NoticeKind: NoteStaleClaim}
			if _, err := s.Enqueue(ctx, n, time.Hour); !errors.Is(err, ErrNoticeSuperseded) {
				t.Fatalf("enqueue err = %v, want ErrNoticeSuperseded", err)
			}
			relayNotice(t, s, req, "") // an unlinked notice still goes out
			got, err := s.Deliver(ctx, "instinct", 10, time.Minute)
			if err != nil || len(got) != 1 || got[0].NoticeFor != "" {
				t.Fatalf("delivered %+v %v, want only the unlinked notice", got, err)
			}
		})
	}
}

// Removing an agent cancels the requests addressed to it and closes the
// stale-claim notices queued to their askers.
func TestRemovalClosesQueuedStaleClaimNotice(t *testing.T) {
	s, _ := open(t, ":memory:")
	req := ask(t, s, "instinct", "grokbot", "find it")
	n := relayNotice(t, s, req, NoteStaleClaim)
	ids, err := s.CancelAllFor(context.Background(), "grokbot")
	if err != nil || !slices.Contains(ids, req.ID) || !slices.Contains(ids, n.ID) {
		t.Fatalf("cancelled %v %v, want the request and its notice", ids, err)
	}
	statusIs(t, s, n.ID, envelope.StatusCancelled)
}

// The notice link columns migrate in place: a database from before them
// opens, opening it again is a no-op, and a notice queued before the
// upgrade, which has no link, is still delivered after a reply.
func TestNoticeLinkMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	req := ask(t, s, "instinct", "grokbot", "before the upgrade")
	old := relayNotice(t, s, req, "")
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS requests_notice_for`); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"notice_for", "notice_kind"} {
		if _, err := s.db.Exec(`ALTER TABLE requests DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if s, err = Open(path); err != nil {
			t.Fatal(err)
		}
		got, st, err := s.Request(context.Background(), old.ID)
		if err != nil || st != envelope.StatusQueued || got.NoticeFor != "" {
			t.Fatalf("%+v %s %v", got, st, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	rep, err := s.Reply(ctx, req.ID, "grokbot", envelope.Reply{Status: envelope.StatusAnswered, Body: "found"})
	if err != nil || len(rep.Superseded) != 0 {
		t.Fatalf("superseded %v %v", rep.Superseded, err)
	}
	if got, err := s.Deliver(ctx, "instinct", 10, time.Minute); err != nil || len(got) != 1 || got[0].ID != old.ID {
		t.Fatalf("delivered %+v %v", got, err)
	}
	other := ask(t, s, "instinct", "grokbot", "find another")
	linked := relayNotice(t, s, other, NoteStaleClaim)
	if got, _, err := s.Request(ctx, linked.ID); err != nil || got.NoticeFor != other.ID || got.NoticeKind != NoteStaleClaim {
		t.Fatalf("link not stored: %+v %v", got, err)
	}
}
