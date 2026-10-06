package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// A reply by email and its email_replies row are kept together: a second
// attempt for the same message changes nothing, and a reply refused because
// the request is closed leaves no row behind.
func TestReplyByEmailIsAtomic(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "muse", "instinct", "find the quote")
	rep, err := s.ReplyByEmail(ctx, "<m1>", req.ID, "instinct", 0, envelope.Reply{Status: envelope.StatusAnswered, Body: "$4,200"})
	if err != nil || rep.From != "instinct" || rep.Body != "$4,200" {
		t.Fatalf("ReplyByEmail = %+v, %v", rep, err)
	}
	if _, err := s.ReplyByEmail(ctx, "<m1>", req.ID, "instinct", 0, envelope.Reply{Status: envelope.StatusAnswered, Body: "again"}); !errors.Is(err, ErrEmailDecided) {
		t.Fatalf("same message again: %v, want ErrEmailDecided", err)
	}
	if _, err := s.ReplyByEmail(ctx, "<m2>", req.ID, "instinct", 0, envelope.Reply{Status: envelope.StatusAnswered, Body: "late"}); !errors.Is(err, ErrWrongState) {
		t.Fatalf("closed request: %v, want ErrWrongState", err)
	}
	if ok, _ := s.EmailDecided(ctx, "<m2>"); ok {
		t.Fatal("a refused reply left an email_replies row")
	}
	res, err := s.Get(ctx, req.ID, "muse")
	if err != nil || res.Reply == nil || res.Reply.Body != "$4,200" {
		t.Fatalf("stored = %+v, %v", res, err)
	}
	owed, err := s.UnsentEmailResponses(ctx, "instinct")
	if err != nil || len(owed) != 1 || owed[0].MessageID != "<m1>" || owed[0].Outcome != EmailOutcomeRecorded || owed[0].RequestID != req.ID {
		t.Fatalf("owed = %+v, %v", owed, err)
	}
	if err := s.MarkEmailResponseSent(ctx, "<m1>"); err != nil {
		t.Fatal(err)
	}
	if owed, _ := s.UnsentEmailResponses(ctx, "instinct"); len(owed) != 0 {
		t.Fatalf("owed after sent = %+v", owed)
	}
}

// A claim under a live lease refuses an email reply; once the lease has
// run out the reply is taken, with no claim of its own.
func TestReplyByEmailRefusesLiveClaim(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "muse", "instinct", "call the plumber")
	if _, err := s.Deliver(ctx, "instinct", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, req.ID, "instinct", time.Minute); err != nil {
		t.Fatal(err)
	}
	if live, err := s.ClaimLive(ctx, req.ID); err != nil || !live {
		t.Fatalf("ClaimLive = %v, %v", live, err)
	}
	if _, err := s.ReplyByEmail(ctx, "<m1>", req.ID, "instinct", 0, envelope.Reply{Status: envelope.StatusAnswered, Body: "done"}); !errors.Is(err, ErrLiveClaim) {
		t.Fatalf("live claim: %v, want ErrLiveClaim", err)
	}
	if _, err := s.ReplyByEmail(ctx, "<m1>", req.ID, "grokbot", 0, envelope.Reply{Status: envelope.StatusAnswered, Body: "done"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other agent: %v, want ErrForbidden", err)
	}
	c.advance(2 * time.Minute)
	if _, err := s.ReplyByEmail(ctx, "<m1>", req.ID, "instinct", 0, envelope.Reply{Status: envelope.StatusFailed, Body: "no plumber"}); err != nil {
		t.Fatalf("lapsed claim: %v", err)
	}
	if _, st, _ := s.Request(ctx, req.ID); st != envelope.StatusFailed {
		t.Fatalf("status = %s, want failed", st)
	}
}

// DecideEmail keeps one row per message, and a row that owes no response
// (a wrong sender's) is never retried. Rows survive reopening the store.
func TestDecideEmailOncePerMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := context.Background()
	if err := s.DecideEmail(ctx, "<m1>", "instinct", "r1", "wrong_sender", false); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideEmail(ctx, "<m2>", "instinct", "r1", "closed", true); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideEmail(ctx, "<m2>", "instinct", "r1", "recorded", true); !errors.Is(err, ErrEmailDecided) {
		t.Fatalf("second decision: %v, want ErrEmailDecided", err)
	}
	s.Close()
	s, _ = open(t, path)
	if ok, err := s.EmailDecided(ctx, "<m1>"); err != nil || !ok {
		t.Fatalf("EmailDecided after reopen = %v, %v", ok, err)
	}
	owed, err := s.UnsentEmailResponses(ctx, "instinct")
	if err != nil || len(owed) != 1 || owed[0].MessageID != "<m2>" || owed[0].Outcome != "closed" {
		t.Fatalf("owed = %+v, %v", owed, err)
	}
	if owed, _ := s.UnsentEmailResponses(ctx, "muse"); len(owed) != 0 {
		t.Fatalf("another agent's owed = %+v", owed)
	}
}

// An email reply is taken only for the clarification round its tag was
// checked against: if the agent asked and the asker answered meanwhile, the
// request has moved on and the reply is refused as out of date.
func TestReplyByEmailRefusesStaleRound(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "muse", "instinct", "find the quote")
	if _, err := s.Deliver(ctx, "instinct", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, req.ID, "instinct", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "instinct", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which house?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(ctx, req.ID, "muse", "the Seattle one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplyByEmail(ctx, "<m1>", req.ID, "instinct", 0, envelope.Reply{Status: envelope.StatusAnswered, Body: "old answer"}); !errors.Is(err, ErrStaleRound) {
		t.Fatalf("round-0 reply after round 1: %v, want ErrStaleRound", err)
	}
	if ok, _ := s.EmailDecided(ctx, "<m1>"); ok {
		t.Fatal("a refused reply left an email_replies row")
	}
	if _, err := s.ReplyByEmail(ctx, "<m2>", req.ID, "instinct", 1, envelope.Reply{Status: envelope.StatusAnswered, Body: "$4,200"}); err != nil {
		t.Fatalf("current round: %v", err)
	}
}

// A request held for approval and never approved cannot be answered by
// email, even though its target is the agent.
func TestReplyByEmailRefusesHeldUnapproved(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "muse", "instinct", "wire the money")
	if _, err := s.db.ExecContext(ctx, `UPDATE requests SET was_held = 1, approved = 0 WHERE id = ?`, req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplyByEmail(ctx, "<m1>", req.ID, "instinct", 0, envelope.Reply{Status: envelope.StatusAnswered, Body: "sent"}); !errors.Is(err, ErrWrongState) {
		t.Fatalf("held, unapproved: %v, want ErrWrongState", err)
	}
}
