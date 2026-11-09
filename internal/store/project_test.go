package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const projectGrace = 10 * time.Minute

func scopedAsk(t *testing.T, s *Store, project string) envelope.Request {
	t.Helper()
	req, err := s.Enqueue(context.Background(), envelope.Request{From: "asker", To: "handler", Kind: envelope.KindAsk, Body: "work", Hop: 1, Chain: []string{}, Project: project}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func deliverTo(t *testing.T, s *Store, project string) []envelope.Request {
	t.Helper()
	reqs, err := s.DeliverFor(context.Background(), "handler", QueueScope{Project: project, Grace: projectGrace}, 20, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return reqs
}

// A stored request keeps its label, and the zero scope takes everything, as
// a caller that predates projects did.
func TestZeroScopeTakesEveryRequest(t *testing.T) {
	s, _ := open(t, ":memory:")
	req := scopedAsk(t, s, "alpha")
	reqs, err := s.Deliver(context.Background(), "handler", 20, time.Minute)
	if err != nil || len(reqs) != 1 || reqs[0].ID != req.ID || reqs[0].Project != "alpha" {
		t.Fatalf("deliver = %+v %v", reqs, err)
	}
}

// Fallback is exact at the grace, and the counts agree with delivery.
func TestQueueScopeFallsBackByAge(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := scopedAsk(t, s, "alpha")
	other := QueueScope{Project: "beta", Grace: projectGrace}
	c.advance(projectGrace - time.Millisecond)
	if n, _, err := s.CountQueuedWithPingsFor(ctx, "handler", other); n != 0 || err != nil {
		t.Fatalf("count before grace = %d %v", n, err)
	}
	if pending, err := s.PendingRequestsFor(ctx, "handler", other, 10); len(pending) != 0 || err != nil {
		t.Fatalf("pending before grace = %+v %v", pending, err)
	}
	c.advance(time.Millisecond)
	if n, _, err := s.CountQueuedWithPingsFor(ctx, "handler", other); n != 1 || err != nil {
		t.Fatalf("count at grace = %d %v", n, err)
	}
	if reqs := deliverTo(t, s, "beta"); len(reqs) != 1 || reqs[0].ID != req.ID {
		t.Fatalf("deliver at grace = %+v", reqs)
	}
}

// An asker's answer queues the request afresh, so a late resume goes back to
// its project first rather than to whichever window polls next.
func TestAnswerRestartsProjectClock(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := scopedAsk(t, s, "alpha")
	if reqs := deliverTo(t, s, "alpha"); len(reqs) != 1 {
		t.Fatalf("deliver = %+v", reqs)
	}
	if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "handler", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"}); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * projectGrace)
	if _, err := s.Answer(ctx, req.ID, "asker", "main"); err != nil {
		t.Fatal(err)
	}
	if reqs := deliverTo(t, s, "beta"); len(reqs) != 0 {
		t.Fatalf("resumed request reached another project: %+v", reqs)
	}
	if reqs := deliverTo(t, s, "alpha"); len(reqs) != 1 || !reqs[0].Resumed {
		t.Fatalf("resume = %+v", reqs)
	}
}

// A lease that lapses does not restart the clock: the matching window had
// its turn, and restarting could keep a request from falling back at all.
func TestLapsedLeaseKeepsProjectClock(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := scopedAsk(t, s, "alpha")
	if reqs := deliverTo(t, s, "alpha"); len(reqs) != 1 {
		t.Fatalf("deliver = %+v", reqs)
	}
	c.advance(projectGrace)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if reqs := deliverTo(t, s, "beta"); len(reqs) != 1 || reqs[0].ID != req.ID {
		t.Fatalf("after a lapsed lease = %+v", reqs)
	}
}

// rev changes on each return to the queue, so a wake can tell a return from
// the delivery it already announced, whichever way the request came back.
func TestPendingRevChangesOnReturn(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	rev := func() int64 {
		t.Helper()
		list, err := s.PendingRequestsFor(ctx, "handler", QueueScope{}, 10)
		if err != nil || len(list) != 1 || list[0].Rev == 0 {
			t.Fatalf("pending = %+v, %v", list, err)
		}
		return list[0].Rev
	}
	req := scopedAsk(t, s, "")
	first := rev()
	if again := rev(); again != first {
		t.Fatalf("rev moved without a return: %d, %d", first, again)
	}
	if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second)
	if _, err := s.Reply(ctx, req.ID, "handler", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"}); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second)
	if _, err := s.Answer(ctx, req.ID, "asker", "main"); err != nil {
		t.Fatal(err)
	}
	answered := rev()
	if answered == first {
		t.Fatalf("rev unchanged after an answer: %d", answered)
	}
	if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Minute)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if lapsed := rev(); lapsed == answered {
		t.Fatalf("rev unchanged after a lapsed lease: %d", lapsed)
	}
}

// A move drops the label: it named a session of the old target.
func TestMoveDropsProject(t *testing.T) {
	s, _ := open(t, ":memory:")
	req := scopedAsk(t, s, "alpha")
	moved, err := s.Move(context.Background(), req.ID, "handler", "other", false)
	if err != nil || moved.Project != "" {
		t.Fatalf("moved = %+v %v", moved, err)
	}
}

// An old database gains the columns, its queued requests stay open to every
// session, and reopening changes nothing.
func TestProjectColumnsMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := context.Background()
	req := ask(t, s, "asker", "handler", "from before projects")
	for _, col := range []string{"to_project", "queued_at", "fallback_woken"} {
		if _, err := s.db.Exec(`ALTER TABLE requests DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, _ = open(t, path)
	if reqs := deliverTo(t, s, "beta"); len(reqs) != 1 || reqs[0].ID != req.ID || reqs[0].Project != "" {
		t.Fatalf("deliver after migration = %+v", reqs)
	}
	if err := s.migrateProject(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); err != nil {
		t.Fatal(err)
	}
}

// A request is reported once when it opens to any session, and again only
// if its fallback clock restarts.
func TestFallenBackReportsOncePerClock(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	fallen := func() []envelope.Request {
		t.Helper()
		reqs, err := s.FallenBack(ctx, projectGrace)
		if err != nil {
			t.Fatal(err)
		}
		return reqs
	}
	ask(t, s, "asker", "handler", "names no project")
	req := scopedAsk(t, s, "alpha")
	if reqs := fallen(); len(reqs) != 0 {
		t.Fatalf("before the grace = %+v", reqs)
	}
	c.advance(projectGrace)
	if reqs := fallen(); len(reqs) != 1 || reqs[0].ID != req.ID || reqs[0].To != "handler" {
		t.Fatalf("at the grace = %+v", reqs)
	}
	if reqs := fallen(); len(reqs) != 0 {
		t.Fatalf("second sweep = %+v", reqs)
	}
	if _, err := s.Claim(ctx, req.ID, "handler", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "handler", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(ctx, req.ID, "asker", "main"); err != nil {
		t.Fatal(err)
	}
	if reqs := fallen(); len(reqs) != 0 {
		t.Fatalf("right after the answer = %+v", reqs)
	}
	c.advance(projectGrace)
	if reqs := fallen(); len(reqs) != 1 || reqs[0].ID != req.ID {
		t.Fatalf("after the answer's grace = %+v", reqs)
	}
}

// HiddenIDsFor is the complement of what a scope may take: it names another
// project's request during the grace and nothing a scope can take.
func TestHiddenIDsFollowScope(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	theirs := scopedAsk(t, s, "alpha")
	scopedAsk(t, s, "beta")
	scopedAsk(t, s, "")
	scope := QueueScope{Project: "beta", Grace: projectGrace}
	if ids, err := s.HiddenIDsFor(ctx, "handler", scope, 10); err != nil || len(ids) != 1 || ids[0] != theirs.ID {
		t.Fatalf("hidden = %v %v", ids, err)
	}
	if ids, err := s.HiddenIDsFor(ctx, "handler", QueueScope{}, 10); err != nil || len(ids) != 0 {
		t.Fatalf("hidden from the zero scope = %v %v", ids, err)
	}
	c.advance(projectGrace)
	if ids, err := s.HiddenIDsFor(ctx, "handler", scope, 10); err != nil || len(ids) != 0 {
		t.Fatalf("hidden after grace = %v %v", ids, err)
	}
}
