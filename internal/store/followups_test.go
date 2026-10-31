package store

import (
	"errors"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func followOnStore(t *testing.T) (*Store, envelope.Request) {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	req, err := s.Enqueue(t.Context(), envelope.Request{From: "a", To: "b", Kind: envelope.KindAsk, Body: "x", Hop: 1, Chain: []string{"a"}, Then: &envelope.Then{To: "c", Body: "y"}}, testTTL)
	if err != nil {
		t.Fatal(err)
	}
	return s, req
}

const testTTL = time.Hour

func TestFollowOnBecomesDueOnlyWhenAnswered(t *testing.T) {
	s, req := followOnStore(t)
	ctx := t.Context()
	if then, ok, err := s.FollowOnOf(ctx, req.ID); err != nil || !ok || then.To != "c" {
		t.Fatalf("row %+v %v %v", then, ok, err)
	}
	if ids, err := s.DueFollowOns(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("due before the reply: %v %v", ids, err)
	}
	if _, err := s.Reply(ctx, req.ID, "b", envelope.Reply{Status: envelope.StatusAnswered, Body: "the reply"}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.DueFollowOns(ctx)
	if err != nil || len(ids) != 1 || ids[0] != req.ID {
		t.Fatalf("due: %v %v", ids, err)
	}
	due, ok, err := s.DueFollowOn(ctx, req.ID)
	if err != nil || !ok || due.Reply != "the reply" || due.Then.To != "c" {
		t.Fatalf("due row %+v %v %v", due, ok, err)
	}
}

func TestFollowOnEnqueueIsSingle(t *testing.T) {
	s, req := followOnStore(t)
	ctx := t.Context()
	fo := envelope.Request{From: "a", To: "c", Kind: envelope.KindAsk, Body: "y", Hop: 2, Chain: []string{"a", "b"}, FollowOf: req.ID}
	if _, err := s.Enqueue(ctx, fo, testTTL); !errors.Is(err, ErrFollowOnGone) {
		t.Fatalf("before the reply: %v", err)
	}
	if _, err := s.Reply(ctx, req.ID, "b", envelope.Reply{Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, fo, testTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, fo, testTTL); !errors.Is(err, ErrFollowOnGone) {
		t.Fatalf("second enqueue: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE follow_of = ?`, req.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d follow-ons, %v", n, err)
	}
	if _, err := s.DropFollowOn(ctx, req.ID, nil, testTTL); !errors.Is(err, ErrFollowOnGone) {
		t.Fatalf("drop after fire: %v", err)
	}
}

func TestDropFollowOnSendsOneNotice(t *testing.T) {
	s, req := followOnStore(t)
	ctx := t.Context()
	if _, err := s.Reply(ctx, req.ID, "b", envelope.Reply{Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	notice := envelope.Request{From: "relay", To: "a", Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: "no"}
	if _, err := s.DropFollowOn(ctx, req.ID, &notice, testTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DropFollowOn(ctx, req.ID, &notice, testTTL); !errors.Is(err, ErrFollowOnGone) {
		t.Fatalf("second drop: %v", err)
	}
	if n, err := s.CountQueued(ctx, "a"); err != nil || n != 1 {
		t.Fatalf("%d notices, %v", n, err)
	}
}

func TestPruneKeepsOpenAndDueFollowOns(t *testing.T) {
	s, open := followOnStore(t)
	ctx := t.Context()
	answered, err := s.Enqueue(ctx, envelope.Request{From: "a", To: "b", Kind: envelope.KindAsk, Body: "x", Hop: 1, Chain: []string{"a"}, Then: &envelope.Then{To: "c", Body: "y"}}, testTTL)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.Enqueue(ctx, envelope.Request{From: "a", To: "b", Kind: envelope.KindAsk, Body: "x", Hop: 1, Chain: []string{"a"}, Then: &envelope.Then{To: "c", Body: "y"}}, testTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, answered.ID, "b", envelope.Reply{Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, failed.ID, "b", envelope.Reply{Status: envelope.StatusFailed}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.PruneFollowOns(ctx)
	if err != nil || len(ids) != 1 || ids[0] != failed.ID {
		t.Fatalf("pruned %v %v", ids, err)
	}
	for _, id := range []string{open.ID, answered.ID} {
		if _, ok, err := s.FollowOnOf(ctx, id); err != nil || !ok {
			t.Fatalf("%s lost: %v %v", id, ok, err)
		}
	}
}
