package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Winning a fire is conditional on the next time the caller read, so a stale
// read loses, and a recurring schedule lands past now on its own cadence.
func TestAdvanceScheduleIsConditional(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "grokbot", "muse")
	sch, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "x", NextAt: c.t.Add(time.Hour), EverySeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	c.advance(3*time.Hour + time.Minute)
	due, err := s.DueSchedules(ctx)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %+v, %v", due, err)
	}
	if won, err := s.AdvanceSchedule(ctx, due[0]); err != nil || !won {
		t.Fatalf("first advance = %v, %v", won, err)
	}
	if won, err := s.AdvanceSchedule(ctx, due[0]); err != nil || won {
		t.Fatalf("stale advance = %v, %v", won, err)
	}
	list, err := s.Schedules(ctx, "grokbot", 0, MaxSchedules)
	if err != nil || len(list) != 1 || !list[0].NextAt.Equal(sch.NextAt.Add(3*time.Hour)) {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if due, err := s.DueSchedules(ctx); err != nil || len(due) != 0 {
		t.Fatalf("due after = %+v, %v", due, err)
	}
}

// A one-shot's fire deletes it, and a new schedule never takes a cancelled
// one's id, so an id a caller holds can only ever name one schedule.
func TestScheduleIDsAreNotReused(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "grokbot", "muse")
	first, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "x", NextAt: c.t.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.AdvanceSchedule(ctx, first); err != nil || !won {
		t.Fatalf("advance = %v, %v", won, err)
	}
	if list, err := s.Schedules(ctx, "", 0, MaxSchedules); err != nil || len(list) != 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	second, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "x", NextAt: c.t.Add(time.Hour)})
	if err != nil || second.ID == first.ID {
		t.Fatalf("second = %+v, %v", second, err)
	}
}

func TestAddScheduleChecksAgentsAndLimit(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "grokbot", "muse")
	if _, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "gone", Body: "x", NextAt: c.t.Add(time.Hour)}); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("unknown target: %v", err)
	}
	for range MaxSchedules {
		if _, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "x", NextAt: c.t.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "x", NextAt: c.t.Add(time.Hour)}); !errors.Is(err, ErrScheduleLimit) {
		t.Fatalf("over the limit: %v", err)
	}
	if list, err := s.Schedules(ctx, "grokbot", 0, MaxSchedules); err != nil || len(list) != MaxSchedules {
		t.Fatalf("list = %d, %v", len(list), err)
	}
}

// DeleteAgent takes schedules the agent owns or is the target of.
func TestDeleteAgentDropsSchedules(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "grokbot", "muse", "instinct")
	for _, pair := range [][2]string{{"grokbot", "muse"}, {"muse", "grokbot"}, {"instinct", "muse"}} {
		if _, err := s.AddSchedule(ctx, envelope.Schedule{From: pair[0], To: pair[1], Body: "x", NextAt: c.t.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteAgent(ctx, "grokbot"); err != nil {
		t.Fatal(err)
	}
	list, err := s.Schedules(ctx, "", 0, MaxSchedules)
	if err != nil || len(list) != 1 || list[0].From != "instinct" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// CancelSchedule with an owner touches only that owner's row.
func TestCancelScheduleOwner(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "grokbot", "muse")
	sch, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "x", NextAt: c.t.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelSchedule(ctx, sch.ID, "muse"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong owner: %v", err)
	}
	got, err := s.CancelSchedule(ctx, sch.ID, "grokbot")
	if err != nil || got.ID != sch.ID || got.To != "muse" {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
}

// BlockSchedule queues its notice only for the first rejection in a run.
func TestBlockScheduleNoticeOnce(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "grokbot", "muse")
	sch, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "x", NextAt: c.t.Add(time.Hour), EverySeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	notice := envelope.Request{From: "relay", To: "grokbot", Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: "blocked"}
	if _, first, err := s.BlockSchedule(ctx, sch.ID, notice, time.Hour); err != nil || !first {
		t.Fatalf("first = %v, %v", first, err)
	}
	if _, first, err := s.BlockSchedule(ctx, sch.ID, notice, time.Hour); err != nil || first {
		t.Fatalf("second = %v, %v", first, err)
	}
	ids, err := s.QueuedIDs(ctx, "grokbot")
	if err != nil || len(ids) != 1 {
		t.Fatalf("queued = %v, %v", ids, err)
	}
	if err := s.ScheduleFired(ctx, sch.ID, "req-1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.Schedules(ctx, "grokbot", 0, MaxSchedules)
	if err != nil || len(list) != 1 || list[0].Blocked || list[0].LastRequest != "req-1" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, first, err := s.BlockSchedule(ctx, sch.ID, notice, time.Hour); err != nil || !first {
		t.Fatalf("new run = %v, %v", first, err)
	}
}
