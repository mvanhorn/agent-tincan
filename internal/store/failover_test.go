package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// A queued ask moves to its new target with its id, sender, chain and
// body; the old target joins moved_from, its pending wake note is gone,
// and it can no longer claim, reply to, post progress on or read the ask.
func TestMoveQueuedAsk(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "codex", "claude-web", "summarize this paper")
	if _, err := s.AddRelayNote(ctx, req.ID, NoteWake, 0, "woken, silent", s.now()); err != nil {
		t.Fatal(err)
	}
	moved, err := s.Move(ctx, req.ID, "claude-web", "gemini-web", false)
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != req.ID || moved.From != "codex" || moved.To != "gemini-web" || moved.Body != req.Body || !slices.Equal(moved.MovedFrom, []string{"claude-web"}) {
		t.Fatalf("moved = %+v", moved)
	}
	if asks, err := s.UnnoticedAsks(ctx, "gemini-web"); err != nil || len(asks) != 1 {
		t.Fatalf("the new target's silence would not be told: %+v %v", asks, err)
	}
	if _, err := s.Claim(ctx, req.ID, "claude-web", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old target claim: %v", err)
	}
	if _, err := s.Reply(ctx, req.ID, "claude-web", envelope.Reply{Status: envelope.StatusAnswered, Body: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old target reply: %v", err)
	}
	if err := s.SetProgress(ctx, req.ID, "claude-web", "working", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old target progress: %v", err)
	}
	if _, err := s.Get(ctx, req.ID, "claude-web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old target get: %v", err)
	}
	if _, err := s.Claim(ctx, req.ID, "gemini-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "claude-web", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which paper?"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old target needs input: %v", err)
	}
	if _, err := s.Reply(ctx, req.ID, "gemini-web", envelope.Reply{Status: envelope.StatusAnswered, Body: "done"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Get(ctx, req.ID, "codex")
	if err != nil || res.Reply == nil || res.Reply.From != "gemini-web" || res.Request.To != "gemini-web" {
		t.Fatalf("asker sees %+v %v", res, err)
	}
}

// A move is conditional: an ask already delivered, one whose target
// changed, or one going back to an agent it moved from stays put.
func TestMoveIsConditional(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "codex", "claude-web", "hi")
	if _, err := s.Move(ctx, req.ID, "grok-web", "gemini-web", false); !errors.Is(err, ErrWrongState) {
		t.Fatalf("wrong old target: %v", err)
	}
	if _, err := s.Move(ctx, req.ID, "claude-web", "gemini-web", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, req.ID, "gemini-web", "claude-web", false); !errors.Is(err, ErrWrongState) {
		t.Fatalf("moved back: %v", err)
	}
	if _, err := s.Deliver(ctx, "gemini-web", 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, req.ID, "gemini-web", "grok-web", false); !errors.Is(err, ErrWrongState) {
		t.Fatalf("delivered ask moved: %v", err)
	}
}

// A claimed ask moves only for its claimant, under a live claim, and its
// claim fields are cleared as a lease requeue clears them.
func TestMoveClaimedAsk(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "codex", "claude-web", "hi")
	if _, err := s.Move(ctx, req.ID, "claude-web", "gemini-web", true); !errors.Is(err, ErrWrongState) {
		t.Fatalf("unclaimed handback: %v", err)
	}
	if _, err := s.Claim(ctx, req.ID, "claude-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Minute)
	if _, err := s.Move(ctx, req.ID, "claude-web", "gemini-web", true); !errors.Is(err, ErrWrongState) {
		t.Fatalf("lapsed claim moved: %v", err)
	}
	if _, err := s.Claim(ctx, req.ID, "claude-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	moved, err := s.Move(ctx, req.ID, "claude-web", "gemini-web", true)
	if err != nil || moved.To != "gemini-web" {
		t.Fatalf("%+v %v", moved, err)
	}
	stats, err := s.QueueStats(ctx)
	if err != nil || stats["gemini-web"].Queued != 1 || stats["claude-web"].Claimed != 0 || stats["gemini-web"].Lapsed != 0 {
		t.Fatalf("stats = %+v %v", stats, err)
	}
}

// A held-back ask stays with its target but is not delivered before its
// time; a handed-back claim is requeued that way.
func TestHoldBack(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "codex", "claude-web", "hi")
	if _, err := s.Claim(ctx, req.ID, "claude-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	until := c.t.Add(30 * time.Minute)
	if _, err := s.HoldBack(ctx, req.ID, "gemini-web", until, true); !errors.Is(err, ErrWrongState) {
		t.Fatalf("non-claimant held back: %v", err)
	}
	if _, err := s.HoldBack(ctx, req.ID, "claude-web", until, true); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Deliver(ctx, "claude-web", 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("delivered before its time: %+v %v", got, err)
	}
	c.advance(30 * time.Minute)
	if got, err := s.Deliver(ctx, "claude-web", 10, time.Minute); err != nil || len(got) != 1 {
		t.Fatalf("not delivered after its time: %+v %v", got, err)
	}
	queued := ask(t, s, "codex", "claude-web", "later")
	if _, err := s.HoldBack(ctx, queued.ID, "claude-web", c.t.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Deliver(ctx, "claude-web", 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("queued ask delivered while held back: %+v %v", got, err)
	}
}

// FailHeld closes an ask only in the state HoldBack would hold it from:
// queued, or for a handback under its target's live claim.
func TestFailHeld(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	failed := envelope.Reply{Status: envelope.StatusFailed, Body: "limited"}
	claimed := ask(t, s, "codex", "claude-web", "claimed")
	if _, err := s.Claim(ctx, claimed.ID, "claude-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailHeld(ctx, claimed.ID, "claude-web", false, failed); !errors.Is(err, ErrWrongState) {
		t.Fatalf("claimed ask failed as queued: %v", err)
	}
	if _, err := s.FailHeld(ctx, claimed.ID, "claude-web", true, failed); err != nil {
		t.Fatalf("handback under a live claim: %v", err)
	}
	lapsed := ask(t, s, "codex", "claude-web", "lapsed")
	if _, err := s.Claim(ctx, lapsed.ID, "claude-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Minute)
	if _, err := s.FailHeld(ctx, lapsed.ID, "claude-web", true, failed); !errors.Is(err, ErrWrongState) {
		t.Fatalf("handback with a lapsed claim: %v", err)
	}
	queued := ask(t, s, "codex", "claude-web", "queued")
	if _, err := s.FailHeld(ctx, queued.ID, "gemini-web", false, failed); !errors.Is(err, ErrWrongState) {
		t.Fatalf("failed as a non-target: %v", err)
	}
	if _, err := s.FailHeld(ctx, queued.ID, "claude-web", false, failed); err != nil {
		t.Fatal(err)
	}
	if _, st, err := s.Request(ctx, queued.ID); err != nil || st != envelope.StatusFailed {
		t.Fatalf("status = %v, %v", st, err)
	}
}

// MoveCandidates lists a target's queued asks and whether each was ever
// claimed (it may then have been typed into a site already).
func TestMoveCandidates(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	fresh := ask(t, s, "codex", "claude-web", "fresh")
	lapsed := ask(t, s, "codex", "claude-web", "lapsed")
	if _, err := s.Claim(ctx, lapsed.ID, "claude-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Minute)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, envelope.Request{From: "codex", To: "claude-web", Kind: envelope.KindNotify, Body: "fyi", Hop: 1, Chain: []string{"codex"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := s.MoveCandidates(ctx, "claude-web")
	if err != nil {
		t.Fatal(err)
	}
	claimed := map[string]bool{}
	for _, m := range got {
		claimed[m.Request.ID] = m.Claimed
	}
	if len(got) != 3 || claimed[fresh.ID] || !claimed[lapsed.ID] {
		t.Fatalf("candidates = %+v", got)
	}
}

// A Deliver to the old target racing a move leaves the request with one
// target: delivered to the old one, or queued on the new one.
func TestMoveRacesDeliver(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	for range 50 {
		req := ask(t, s, "codex", "claude-web", "race")
		var wg sync.WaitGroup
		var moveErr error
		var delivered []envelope.Request
		wg.Go(func() { _, moveErr = s.Move(ctx, req.ID, "claude-web", "gemini-web", false) })
		wg.Go(func() { delivered, _ = s.Deliver(ctx, "claude-web", 10, time.Minute) })
		wg.Wait()
		got, status, err := s.Request(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case moveErr == nil && got.To == "gemini-web" && status == envelope.StatusQueued && len(delivered) == 0:
		case errors.Is(moveErr, ErrWrongState) && got.To == "claude-web" && status == envelope.StatusDelivered && len(delivered) == 1:
		default:
			t.Fatalf("move %v, delivered %d, request to %s %s", moveErr, len(delivered), got.To, status)
		}
		if _, err := s.Cancel(ctx, req.ID, "codex"); err != nil && !errors.Is(err, ErrWrongState) {
			t.Fatal(err)
		}
	}
}

// The failover columns migrate in place: a database from before them
// opens, and opening it again is a no-op.
func TestFailoverMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	req := ask(t, s, "codex", "claude-web", "before the upgrade")
	if _, err := s.Claim(context.Background(), req.ID, "claude-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"moved_from", "deliver_after", "claim_lapsed"} {
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
		got, _, err := s.Request(context.Background(), req.ID)
		if err != nil || got.To != "claude-web" || len(got.MovedFrom) != 0 {
			t.Fatalf("%+v %v", got, err)
		}
		if lapsed, err := s.ClaimLapsed(context.Background(), req.ID); err != nil || !lapsed {
			t.Fatalf("lapse not carried over: %v %v", lapsed, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A claim that Sweep finds lapsed marks the request for good: claiming it
// again does not clear the mark, and it never moves, queued or handed
// back.
func TestClaimLapsedNeverMoves(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "codex", "claude-web", "hi")
	if lapsed, err := s.ClaimLapsed(ctx, req.ID); err != nil || lapsed {
		t.Fatalf("before any claim: %v %v", lapsed, err)
	}
	if _, err := s.Claim(ctx, req.ID, "claude-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Minute)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, req.ID, "claude-web", "gemini-web", false); !errors.Is(err, ErrWrongState) {
		t.Fatalf("lapsed ask moved from the queue: %v", err)
	}
	if _, err := s.Claim(ctx, req.ID, "claude-web", time.Minute); err != nil {
		t.Fatal(err)
	}
	if lapsed, err := s.ClaimLapsed(ctx, req.ID); err != nil || !lapsed {
		t.Fatalf("after a reclaim: %v %v", lapsed, err)
	}
	if _, err := s.Move(ctx, req.ID, "claude-web", "gemini-web", true); !errors.Is(err, ErrWrongState) {
		t.Fatalf("reclaimed lapsed ask handed back and moved: %v", err)
	}
	if _, err := s.ClaimLapsed(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}
