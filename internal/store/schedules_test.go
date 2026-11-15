package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

// repeatingTask files a task from grokbot that repeats hourly for muse.
func repeatingTask(t *testing.T, s *Store, c *clock) envelope.Task {
	t.Helper()
	task, created, err := s.AddRepeatingTask(context.Background(), envelope.Task{Filer: "grokbot", Title: "check the backups"},
		envelope.Schedule{From: "grokbot", To: "muse", EverySeconds: 3600, NextAt: c.t.Add(time.Hour)})
	if err != nil || !created || task.RepeatSchedule == 0 || task.RepeatEverySeconds != 3600 || task.RepeatTo != "muse" {
		t.Fatalf("repeating task = %+v, %v, %v", task, created, err)
	}
	return task
}

// A database from before repeating tasks gains the schedule's task link,
// and opening it again changes nothing.
func TestSchedulesMigrateOnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, c := open(t, path)
	ctx := context.Background()
	joinAll(t, s, c, "grokbot", "muse")
	old, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "weekly report", EverySeconds: 3600, NextAt: c.t.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP INDEX schedules_task; ALTER TABLE schedules DROP COLUMN task_id;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := range 2 {
		s, c = open(t, path)
		list, err := s.Schedules(ctx, "grokbot", 0, MaxSchedules)
		if err != nil || len(list) == 0 || list[0].ID != old.ID || list[0].Body != "weekly report" || list[0].TaskID != 0 {
			t.Fatalf("schedules after open %d = %+v, %v", i, list, err)
		}
		if i == 0 {
			repeatingTask(t, s, c)
		} else if len(list) != 2 || list[1].TaskID == 0 {
			t.Fatalf("schedules after the second open = %+v", list)
		}
		s.Close()
	}
}

// A repeating task is stored with its schedule or not at all, and a task
// returned for its key gets no second schedule.
func TestAddRepeatingTaskIsOneWrite(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "grokbot", "muse")
	repeat := envelope.Schedule{From: "grokbot", To: "muse", EverySeconds: 3600, NextAt: c.t.Add(time.Hour)}
	first, _, err := s.AddRepeatingTask(ctx, envelope.Task{Filer: "grokbot", Title: "check the backups", Key: "backups"}, repeat)
	if err != nil {
		t.Fatal(err)
	}
	again, created, err := s.AddRepeatingTask(ctx, envelope.Task{Filer: "grokbot", Title: "check them again", Key: "backups"}, repeat)
	if err != nil || created || again.ID != first.ID || again.RepeatSchedule != first.RepeatSchedule {
		t.Fatalf("second add with the key = %+v, %v, %v", again, created, err)
	}
	list, err := s.Schedules(ctx, "grokbot", 0, MaxSchedules)
	if err != nil || len(list) != 1 || list[0].TaskID != first.ID || list[0].Body != "" {
		t.Fatalf("schedules = %+v, %v", list, err)
	}

	gone := repeat
	gone.To = "nobody"
	if _, _, err := s.AddRepeatingTask(ctx, envelope.Task{Filer: "grokbot", Title: "for nobody"}, gone); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("repeat for an unknown agent: %v", err)
	}
	for range MaxSchedules - 1 {
		if _, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "x", NextAt: c.t.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.AddRepeatingTask(ctx, envelope.Task{Filer: "grokbot", Title: "one too many"}, repeat); !errors.Is(err, ErrScheduleLimit) {
		t.Fatalf("repeat over the schedule limit: %v", err)
	}
	if tasks, err := s.Tasks(ctx, "", 0, 10); err != nil || len(tasks) != 1 {
		t.Fatalf("tasks after the refused adds = %+v, %v", tasks, err)
	}
}

// Closing a task deletes its schedule in the same write, and a task opened
// again does not repeat. Cancelling the schedule, or removing an agent it
// names, leaves the task with no repeat.
func TestTaskRepeatEnds(t *testing.T) {
	ctx := context.Background()
	ends := map[string]func(s *Store, id, schedule int64) error{
		"done": func(s *Store, id, _ int64) error {
			_, err := s.MarkTaskDone(ctx, id, envelope.TaskOwner, "")
			return err
		},
		"dropped by its filer": func(s *Store, id, _ int64) error {
			_, err := s.SetTaskStatus(ctx, id, envelope.TaskDropped, "grokbot", "", envelope.TaskOpen)
			return err
		},
		"dropped by the owner": func(s *Store, id, _ int64) error {
			_, _, err := s.DropTask(ctx, id, envelope.TaskOwner, "")
			return err
		},
		"schedule cancelled": func(s *Store, _, schedule int64) error {
			_, err := s.CancelSchedule(ctx, schedule, "grokbot")
			return err
		},
		"sender removed": func(s *Store, _, _ int64) error {
			_, err := s.DeleteAgent(ctx, "grokbot")
			return err
		},
		"target removed": func(s *Store, _, _ int64) error {
			_, err := s.DeleteAgent(ctx, "muse")
			return err
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			s, c := open(t, ":memory:")
			joinAll(t, s, c, "grokbot", "muse")
			task := repeatingTask(t, s, c)
			if err := end(s, task.ID, task.RepeatSchedule); err != nil {
				t.Fatal(err)
			}
			if list, err := s.Schedules(ctx, "", 0, MaxSchedules); err != nil || len(list) != 0 {
				t.Fatalf("schedules = %+v, %v", list, err)
			}
			got, err := s.Task(ctx, task.ID, "")
			if err != nil || got.RepeatSchedule != 0 || got.RepeatEverySeconds != 0 || got.RepeatTo != "" {
				t.Fatalf("task = %+v, %v", got, err)
			}
			if closed := got.Status == envelope.TaskDone || got.Status == envelope.TaskDropped; closed != (name == "done" || strings.HasPrefix(name, "dropped")) {
				t.Fatalf("task is %s", got.Status)
			}
			if got.Status != envelope.TaskOpen {
				if got, err = s.SetTaskStatus(ctx, task.ID, envelope.TaskOpen, envelope.TaskOwner, "", envelope.TaskDone, envelope.TaskDropped); err != nil || got.RepeatSchedule != 0 {
					t.Fatalf("task opened again = %+v, %v", got, err)
				}
			}
		})
	}
}

// Assigning a repeating task to another teammate moves its schedule's
// target in the write that stores the ask. A move of that ask to a fallback
// does not.
func TestTaskRepeatFollowsAssignNotMove(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "grokbot", "muse", "instinct", "scout")
	task := repeatingTask(t, s, c)
	plain, err := s.AddSchedule(ctx, envelope.Schedule{From: "grokbot", To: "muse", Body: "x", NextAt: c.t.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	req, err := attempt(s, task.ID, "grokbot", "instinct", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Task(ctx, task.ID, ""); err != nil || got.RepeatTo != "instinct" || got.Assignee != "instinct" || got.RepeatSchedule != task.RepeatSchedule {
		t.Fatalf("task after the assign = %+v, %v", got, err)
	}
	if _, err := s.RerouteUntaken(ctx, req.ID, "instinct", "scout"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Task(ctx, task.ID, ""); err != nil || got.RepeatTo != "instinct" || got.Assignee != "scout" {
		t.Fatalf("task after the move = %+v, %v", got, err)
	}
	list, err := s.Schedules(ctx, "grokbot", 0, MaxSchedules)
	if err != nil || len(list) != 2 || list[1].ID != plain.ID || list[1].To != "muse" || list[1].TaskID != 0 {
		t.Fatalf("schedules = %+v, %v", list, err)
	}
}
