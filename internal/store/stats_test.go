package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// finish answers one ask to agent that took d.
func finish(t *testing.T, s *Store, c *clock, agent string, status envelope.Status, d time.Duration) {
	t.Helper()
	req := ask(t, s, "grokbot", agent, "dummy question")
	c.advance(d)
	if _, err := s.Reply(context.Background(), req.ID, agent, envelope.Reply{Status: status, Body: "dummy reply"}); err != nil {
		t.Fatal(err)
	}
}

// Only asks with an outcome the teammate owns count: pings, notifies, open
// requests, declines, cancels and holds the owner never approved do not.
func TestAgentStatsCountsFinishedAsks(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	start := c.t
	finish(t, s, c, "muse", envelope.StatusAnswered, time.Minute)
	finish(t, s, c, "muse", envelope.StatusAnswered, 3*time.Minute)
	finish(t, s, c, "muse", envelope.StatusAnswered, 2*time.Minute)
	// A fast failure says nothing about reply time.
	finish(t, s, c, "muse", envelope.StatusFailed, time.Second)
	finish(t, s, c, "instinct", envelope.StatusFailed, time.Minute)

	// Excluded kinds, even when finished.
	for _, kind := range []envelope.Kind{envelope.KindPing, envelope.KindNotify} {
		req, err := s.Enqueue(ctx, envelope.Request{From: "grokbot", To: "muse", Kind: kind, Body: "dummy", Hop: 1, Chain: []string{}}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered}); err != nil {
			t.Fatal(err)
		}
	}
	// A declined ask is the teammate's choice, not a failure to answer.
	finish(t, s, c, "muse", envelope.StatusDeclined, time.Minute)
	// Cancelled by its sender.
	cancelled := ask(t, s, "grokbot", "muse", "dummy question")
	if err := s.Cancel(ctx, cancelled.ID, "grokbot"); err != nil {
		t.Fatal(err)
	}
	// Denied by the owner, and left to lapse in the hold.
	denied := holdFor(t, s, "muse", time.Hour)
	if _, err := s.DenyHeld(ctx, denied.ID, "no"); err != nil {
		t.Fatal(err)
	}
	holdFor(t, s, "muse", time.Minute)
	// Expires from the queue, which is the teammate's silence.
	ask(t, s, "grokbot", "muse", "dummy question")
	c.advance(2 * time.Hour)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	// Still open.
	ask(t, s, "grokbot", "muse", "dummy question")
	holdFor(t, s, "muse", time.Hour)

	got, err := s.AgentStats(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if want := (AgentStat{Answered: 3, Failed: 1, Expired: 1, MedianReply: 2 * time.Minute, HasMedian: true}); got["muse"] != want {
		t.Fatalf("muse = %+v, want %+v", got["muse"], want)
	}
	if want := (AgentStat{Failed: 1}); got["instinct"] != want {
		t.Fatalf("instinct = %+v, want %+v", got["instinct"], want)
	}
	if len(got) != 2 {
		t.Fatalf("stats = %+v, want only muse and instinct", got)
	}
}

// An ask that expires on an unanswered clarification was waiting on its asker,
// so no window counts it, on either side of the question. One answered in time
// and a plain expiry from the queue still count.
func TestAgentStatsLeavesOutExpiryWaitingOnAsker(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	start := c.t
	question := func() envelope.Request {
		t.Helper()
		req := ask(t, s, "grokbot", "muse", "dummy question")
		if _, err := s.Claim(ctx, req.ID, "muse", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "dummy question back"}); err != nil {
			t.Fatal(err)
		}
		return req
	}
	abandoned := question()
	c.advance(time.Minute)
	answered := question()
	if _, err := s.Answer(ctx, answered.ID, "grokbot", "dummy answer"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, answered.ID, "muse", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, answered.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "dummy reply"}); err != nil {
		t.Fatal(err)
	}
	afterReply := c.t.Add(time.Second)
	c.advance(2 * time.Hour)
	// A plain expiry from the queue beside it stays counted.
	silent := ask(t, s, "grokbot", "muse", "dummy question")
	c.advance(2 * time.Hour)
	transitions, err := s.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expired := map[string]bool{}
	for _, tr := range transitions {
		expired[tr.ID] = tr.Status == envelope.StatusExpired
	}
	if !expired[abandoned.ID] || !expired[silent.ID] {
		t.Fatalf("sweep = %+v, want both asks expired", transitions)
	}
	for since, want := range map[time.Time]AgentStat{
		start:      {Answered: 1, Expired: 1},
		afterReply: {Expired: 1},
	} {
		got, err := s.AgentStats(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		st := got["muse"]
		st.MedianReply, st.HasMedian = 0, false
		if st != want {
			t.Fatalf("since %v: muse = %+v, want %+v", since, st, want)
		}
	}
}

func holdFor(t *testing.T, s *Store, agent string, ttl time.Duration) envelope.Request {
	t.Helper()
	req, err := s.Enqueue(context.Background(), envelope.Request{From: "grokbot", To: agent, Kind: envelope.KindAsk, Body: "dummy question", Hop: 1, Chain: []string{}, Status: envelope.StatusHeld, HoldTTL: ttl}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// The owner's delay is not the teammate's: a request held for an hour and
// answered a minute after approval has a one-minute reply time.
func TestAgentStatsHeldMeasuresFromApproval(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	start := c.t
	req := holdFor(t, s, "muse", 2*time.Hour)
	c.advance(time.Hour)
	if _, err := s.Release(ctx, req.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, AuditEvent{Event: "approved", RequestID: req.ID, Actor: "macbook"}); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentStats(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if want := (AgentStat{Answered: 1, MedianReply: time.Minute, HasMedian: true}); got["muse"] != want {
		t.Fatalf("muse = %+v, want %+v", got["muse"], want)
	}
}

// With no approval event to measure from (the audit write can fail after
// the release), the ask counts but its time is unknown, never the hold's.
func TestAgentStatsApprovedWithoutEventHasNoReplyTime(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	start := c.t
	req := holdFor(t, s, "muse", 2*time.Hour)
	c.advance(time.Hour)
	if _, err := s.Release(ctx, req.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentStats(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if want := (AgentStat{Answered: 1}); got["muse"] != want {
		t.Fatalf("muse = %+v, want %+v", got["muse"], want)
	}
}

// The median of an even count is the midpoint of the middle two, whatever
// order the replies came in.
func TestAgentStatsEvenMedianIsStable(t *testing.T) {
	for _, order := range [][]time.Duration{
		{time.Minute, 4 * time.Minute, 10 * time.Minute, time.Second},
		{10 * time.Minute, time.Second, 4 * time.Minute, time.Minute},
	} {
		s, c := open(t, ":memory:")
		start := c.t
		for _, d := range order {
			finish(t, s, c, "muse", envelope.StatusAnswered, d)
		}
		got, err := s.AgentStats(context.Background(), start)
		if err != nil {
			t.Fatal(err)
		}
		if want := 2*time.Minute + 30*time.Second; got["muse"].MedianReply != want {
			t.Fatalf("order %v: median = %v, want %v", order, got["muse"].MedianReply, want)
		}
	}
}

// The window is about when an ask finished, so one sent before it and
// finished inside it counts, and one finished before it does not.
func TestAgentStatsWindowUsesFinishTime(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	finish(t, s, c, "muse", envelope.StatusAnswered, time.Minute)
	ask(t, s, "grokbot", "muse", "dummy question")
	c.advance(2 * time.Hour)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	straddles := ask(t, s, "grokbot", "muse", "dummy question")
	c.advance(time.Minute)
	since := c.t
	c.advance(time.Minute)
	if _, err := s.Reply(ctx, straddles.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentStats(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if want := (AgentStat{Answered: 1, MedianReply: 2 * time.Minute, HasMedian: true}); got["muse"] != want {
		t.Fatalf("muse = %+v, want %+v", got["muse"], want)
	}
}

// A relay that was down expires old asks when it restarts, and that restart
// is not when they finished: the deadline dates them.
func TestAgentStatsDatesExpiryByDeadline(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	ask(t, s, "grokbot", "muse", "dummy question")
	c.advance(time.Hour)
	deadline := c.t
	c.advance(72 * time.Hour)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for since, want := range map[time.Time]AgentStat{
		deadline:                       {Expired: 1},
		deadline.Add(time.Millisecond): {},
		c.t.Add(-time.Minute):          {},
	} {
		got, err := s.AgentStats(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		if got["muse"] != want {
			t.Fatalf("since %v: muse = %+v, want %+v", since, got["muse"], want)
		}
	}
}

// Progress renews a claim past its deadline, and the ask was still live then,
// so its expiry is no earlier than the last progress.
func TestAgentStatsExpiryCountsProgressPastDeadline(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "muse", "dummy question")
	if _, err := s.Claim(ctx, req.ID, "muse", 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Hour)
	if err := s.SetProgress(ctx, req.ID, "muse", "dummy progress", time.Hour); err != nil {
		t.Fatal(err)
	}
	progress := c.t
	c.advance(2 * time.Hour)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for since, want := range map[time.Time]AgentStat{
		progress:                       {Expired: 1},
		progress.Add(time.Millisecond): {},
	} {
		got, err := s.AgentStats(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		if got["muse"] != want {
			t.Fatalf("since %v: muse = %+v, want %+v", since, got["muse"], want)
		}
	}
}

// A reply inside the millisecond is a known median of 0, unlike a record with
// no reply time at all.
func TestAgentStatsKnownZeroMedian(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	start := c.t
	finish(t, s, c, "muse", envelope.StatusAnswered, 0)
	finish(t, s, c, "instinct", envelope.StatusFailed, time.Minute)
	got, err := s.AgentStats(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if want := (AgentStat{Answered: 1, HasMedian: true}); got["muse"] != want {
		t.Fatalf("muse = %+v, want %+v", got["muse"], want)
	}
	if want := (AgentStat{Failed: 1}); got["instinct"] != want {
		t.Fatalf("instinct = %+v, want %+v", got["instinct"], want)
	}
}

// History is kept for good, so the read finds replies by time and never walks
// every answered ask: the cache limits how often it runs, not how long it
// holds the store's one connection.
func TestAgentStatsFindsRepliesByTime(t *testing.T) {
	s, _ := open(t, ":memory:")
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+agentStatsQuery, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(plan, "\n")
	if !strings.Contains(got, "replies_created") || strings.Contains(got, "SCAN") {
		t.Fatalf("plan does not find replies by time:\n%s", got)
	}
}
