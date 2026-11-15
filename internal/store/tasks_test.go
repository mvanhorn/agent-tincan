package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

func addTask(t *testing.T, s *Store, task envelope.Task) envelope.Task {
	t.Helper()
	out, created, err := s.AddTask(context.Background(), task)
	if err != nil || !created {
		t.Fatalf("add %+v = %+v, %v, %v", task, out, created, err)
	}
	return out
}

// attempt sends an ask under task id, the way an assign does.
func attempt(s *Store, id int64, from, to string, held bool) (envelope.Request, error) {
	req := envelope.Request{From: from, To: to, Kind: envelope.KindAsk, Body: "do it", Hop: 1, Chain: []string{}, TaskID: id}
	if held {
		req.Status, req.HoldTTL = envelope.StatusHeld, time.Hour
	}
	return s.Enqueue(context.Background(), req, time.Hour)
}

func taskIDs(tasks []envelope.Task) []int64 {
	ids := []int64{}
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

// The task tables and the request link migrate onto an existing relay
// database, keep its requests, and opening again changes nothing.
func TestTasksMigrateOnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := context.Background()
	req := ask(t, s, "codex", "muse", "hi")
	// A database from before tasks: drop what this version added.
	if _, err := s.DB().Exec(`DROP TABLE task_claimants; DROP TABLE task_events; DROP TABLE tasks;
		DROP INDEX requests_task; ALTER TABLE requests DROP COLUMN task_id;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var first envelope.Task
	for i := range 2 {
		s, _ = open(t, path)
		if got, _, err := s.Request(ctx, req.ID); err != nil || got.Body != "hi" || got.From != "codex" || got.To != "muse" || got.TaskID != 0 {
			t.Fatalf("request = %+v %v", got, err)
		}
		if i == 0 {
			first = addTask(t, s, envelope.Task{Filer: "codex", Title: "ship it"})
		}
		if got, err := s.Task(ctx, first.ID, ""); err != nil || got.Title != "ship it" {
			t.Fatalf("task after open %d = %+v %v", i, got, err)
		}
		s.Close()
	}
}

// The owner marker is not a name an agent can join under.
func TestTaskOwnerIsNotAnAgentName(t *testing.T) {
	if identity.ValidName(envelope.TaskOwner) {
		t.Fatalf("%q is a legal agent name", envelope.TaskOwner)
	}
}

// Adding a task returns its id and starts its history with one "added"
// entry by the filer.
func TestAddTaskWritesAddedEvent(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	task := addTask(t, s, envelope.Task{Filer: "codex", Title: "ship it", Detail: "tag and release"})
	if task.ID == 0 || task.Status != envelope.TaskOpen || task.Filer != "codex" || task.Title != "ship it" || task.Detail != "tag and release" || !task.CreatedAt.Equal(c.t) {
		t.Fatalf("task = %+v", task)
	}
	got, err := s.Task(ctx, task.ID, "")
	if err != nil || got.ID != task.ID || got.Status != envelope.TaskOpen || got.Detail != "tag and release" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	events, err := s.TaskEvents(ctx, task.ID, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if e := events[0]; e.Kind != envelope.TaskEventAdded || e.Actor != "codex" || e.TaskID != task.ID || e.Status != envelope.TaskOpen || !e.At.Equal(c.t) {
		t.Fatalf("event = %+v", e)
	}
	if _, err := s.Task(ctx, task.ID+1, ""); !errors.Is(err, ErrNoTask) {
		t.Fatalf("missing task: %v", err)
	}
	// The owner's own task, and one held for the owner's approval.
	own := addTask(t, s, envelope.Task{Filer: envelope.TaskOwner, Assignee: envelope.TaskOwner, Title: "call the bank"})
	if own.Filer != envelope.TaskOwner || own.Assignee != envelope.TaskOwner {
		t.Fatalf("owner task = %+v", own)
	}
	held := addTask(t, s, envelope.Task{Filer: "codex", Title: "from mail", Status: envelope.TaskProposed})
	if held.Status != envelope.TaskProposed {
		t.Fatalf("proposed task = %+v", held)
	}
}

// One filer holds at most MaxOpenTasks tasks that are not done or dropped;
// closing one frees a slot, and the owner is not capped.
func TestAddTaskLimit(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	var first envelope.Task
	for i := range MaxOpenTasks {
		task := addTask(t, s, envelope.Task{Filer: "codex", Title: fmt.Sprint("task ", i)})
		if i == 0 {
			first = task
		}
	}
	if _, _, err := s.AddTask(ctx, envelope.Task{Filer: "codex", Title: "one too many"}); !errors.Is(err, ErrTaskLimit) {
		t.Fatalf("over the limit: %v", err)
	}
	if all, err := s.Tasks(ctx, "", 0, 1000); err != nil || len(all) != MaxOpenTasks {
		t.Fatalf("after refusal: %d tasks, %v", len(all), err)
	}
	// Another filer has its own count.
	addTask(t, s, envelope.Task{Filer: "muse", Title: "mine"})
	if _, err := s.SetTaskStatus(ctx, first.ID, envelope.TaskDone, envelope.TaskOwner, "", envelope.TaskOpen); err != nil {
		t.Fatal(err)
	}
	addTask(t, s, envelope.Task{Filer: "codex", Title: "fits again"})
	for i := range MaxOpenTasks + 1 {
		addTask(t, s, envelope.Task{Filer: envelope.TaskOwner, Title: fmt.Sprint("owner ", i)})
	}
}

// A key names one live task per filer: adding with it again returns that
// task and creates nothing, until the task is done or dropped.
func TestAddTaskDedupesOnKey(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	first := addTask(t, s, envelope.Task{Filer: "codex", Title: "reply to Ana", Key: "mail-17"})
	again, created, err := s.AddTask(ctx, envelope.Task{Filer: "codex", Title: "reply to Ana, again", Key: "mail-17"})
	if err != nil || created || again.ID != first.ID || again.Title != "reply to Ana" {
		t.Fatalf("same key = %+v, %v, %v", again, created, err)
	}
	if events, err := s.TaskEvents(ctx, first.ID, 0, 10); err != nil || len(events) != 1 {
		t.Fatalf("events after a duplicate = %+v, %v", events, err)
	}
	other := addTask(t, s, envelope.Task{Filer: "muse", Title: "reply to Ana", Key: "mail-17"})
	if other.ID == first.ID {
		t.Fatal("another filer's key matched")
	}
	// No key never matches.
	a := addTask(t, s, envelope.Task{Filer: "codex", Title: "a"})
	if b := addTask(t, s, envelope.Task{Filer: "codex", Title: "b"}); a.ID == b.ID {
		t.Fatal("keyless tasks matched")
	}
	if _, err := s.SetTaskStatus(ctx, first.ID, envelope.TaskDone, envelope.TaskOwner, "", envelope.TaskOpen); err != nil {
		t.Fatal(err)
	}
	fresh := addTask(t, s, envelope.Task{Filer: "codex", Title: "reply to Ana", Key: "mail-17"})
	if fresh.ID == first.ID {
		t.Fatal("a done task's key matched")
	}
	if all, err := s.Tasks(ctx, "", 0, 100); err != nil || len(all) != 5 {
		t.Fatalf("tasks = %v, %v", taskIDs(all), err)
	}
}

// An ask under a task takes the task only while no other ask holds it and
// its status allows an attempt; a refused ask is not stored.
func TestTaskClaimIsConditional(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	task := addTask(t, s, envelope.Task{Filer: "codex", Title: "ship it"})
	req, err := attempt(s, task.ID, "codex", "muse", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.Request(ctx, req.ID); err != nil || got.TaskID != task.ID {
		t.Fatalf("request = %+v, %v", got, err)
	}
	got, err := s.Task(ctx, task.ID, "")
	if err != nil || got.Status != envelope.TaskWorking || got.CurrentRequest != req.ID || got.Assignee != "muse" || got.AskStatus != envelope.StatusQueued {
		t.Fatalf("claimed task = %+v, %v", got, err)
	}
	events, err := s.TaskEvents(ctx, task.ID, 0, 10)
	if err != nil || len(events) != 2 || events[1].Kind != envelope.TaskEventAssigned || events[1].Actor != "codex" || events[1].Body != "muse" || events[1].Status != envelope.TaskWorking {
		t.Fatalf("events = %+v, %v", events, err)
	}

	// A working task refuses a second ask and stores nothing for it.
	if _, err := attempt(s, task.ID, "codex", "hermes", false); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("second ask: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM requests WHERE task_id = ?`, task.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("requests under the task = %d, %v", n, err)
	}
	if got, err := s.Task(ctx, task.ID, ""); err != nil || got.CurrentRequest != req.ID || got.Assignee != "muse" {
		t.Fatalf("task after refusal = %+v, %v", got, err)
	}

	proposed := addTask(t, s, envelope.Task{Filer: "codex", Title: "from mail", Status: envelope.TaskProposed})
	if _, err := attempt(s, proposed.ID, "codex", "muse", false); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("ask under a proposed task: %v", err)
	}
	done := addTask(t, s, envelope.Task{Filer: "codex", Title: "old"})
	if _, err := s.SetTaskStatus(ctx, done.ID, envelope.TaskDone, envelope.TaskOwner, "", envelope.TaskOpen); err != nil {
		t.Fatal(err)
	}
	if _, err := attempt(s, done.ID, "codex", "muse", false); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("ask under a done task: %v", err)
	}
	if _, err := attempt(s, 9999, "codex", "muse", false); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("ask under no task: %v", err)
	}
	// An ordinary ask is untouched.
	if plain := ask(t, s, "codex", "muse", "hi"); plain.TaskID != 0 {
		t.Fatalf("plain ask = %+v", plain)
	}
}

// A status change applies only from the statuses the caller allows, and
// each change is one history entry; history also takes comments.
func TestSetTaskStatusAndHistory(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	task := addTask(t, s, envelope.Task{Filer: "codex", Title: "ship it"})
	if _, err := s.SetTaskStatus(ctx, task.ID, envelope.TaskOpen, envelope.TaskOwner, "", envelope.TaskDone, envelope.TaskDropped); !errors.Is(err, ErrTaskState) {
		t.Fatalf("reopen of an open task: %v", err)
	}
	if _, err := s.SetTaskStatus(ctx, task.ID+1, envelope.TaskDone, envelope.TaskOwner, "", envelope.TaskOpen); !errors.Is(err, ErrNoTask) {
		t.Fatalf("missing task: %v", err)
	}
	c.advance(time.Minute)
	ev, err := s.AppendTaskEvent(ctx, envelope.TaskEvent{TaskID: task.ID, Actor: "muse", Kind: envelope.TaskEventComment, Body: "waiting on the tag"})
	if err != nil || ev.ID == 0 || !ev.At.Equal(c.t) {
		t.Fatalf("comment = %+v, %v", ev, err)
	}
	if _, err := s.AppendTaskEvent(ctx, envelope.TaskEvent{TaskID: task.ID + 1, Actor: "muse", Kind: envelope.TaskEventComment, Body: "x"}); !errors.Is(err, ErrNoTask) {
		t.Fatalf("comment on no task: %v", err)
	}
	got, err := s.SetTaskStatus(ctx, task.ID, envelope.TaskDone, envelope.TaskOwner, "shipped", envelope.TaskOpen, envelope.TaskReturned)
	if err != nil || got.Status != envelope.TaskDone {
		t.Fatalf("done = %+v, %v", got, err)
	}
	events, err := s.TaskEvents(ctx, task.ID, 0, 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if e := events[1]; e.Kind != envelope.TaskEventComment || e.Actor != "muse" || e.Body != "waiting on the tag" || e.Status != "" {
		t.Fatalf("comment event = %+v", e)
	}
	if e := events[2]; e.Kind != envelope.TaskEventStatus || e.Actor != envelope.TaskOwner || e.Body != "shipped" || e.Status != envelope.TaskDone {
		t.Fatalf("status event = %+v", e)
	}
	if after, err := s.TaskEvents(ctx, task.ID, events[1].ID, 10); err != nil || len(after) != 1 || after[0].ID != events[2].ID {
		t.Fatalf("events after a cursor = %+v, %v", after, err)
	}
	// A task with an ask open under it is not open or returned, so a done
	// that allows only those is refused.
	busy := addTask(t, s, envelope.Task{Filer: "codex", Title: "busy"})
	if _, err := attempt(s, busy.ID, "codex", "muse", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTaskStatus(ctx, busy.ID, envelope.TaskDone, envelope.TaskOwner, "", envelope.TaskOpen, envelope.TaskReturned); !errors.Is(err, ErrTaskState) {
		t.Fatalf("done under an open ask: %v", err)
	}
}

// An agent sees a task it filed, one whose ask is addressed to it and not
// held, one it claimed an ask under, and any open, unassigned task with no
// result. It does not see another agent's proposed task. An admin sees all.
func TestTasksVisibility(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	filed := addTask(t, s, envelope.Task{Filer: "muse", Title: "muse filed, codex works"})
	if _, err := attempt(s, filed.ID, "muse", "codex", false); err != nil {
		t.Fatal(err)
	}
	assigned := addTask(t, s, envelope.Task{Filer: "codex", Title: "codex filed, muse works"})
	if _, err := attempt(s, assigned.ID, "codex", "muse", false); err != nil {
		t.Fatal(err)
	}
	pool := addTask(t, s, envelope.Task{Filer: "codex", Title: "anyone"})
	heldTask := addTask(t, s, envelope.Task{Filer: "codex", Title: "held for the owner"})
	heldReq, err := attempt(s, heldTask.ID, "codex", "muse", true)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := addTask(t, s, envelope.Task{Filer: "codex", Title: "hermes works"})
	if _, err := attempt(s, elsewhere.ID, "codex", "hermes", false); err != nil {
		t.Fatal(err)
	}
	proposed := addTask(t, s, envelope.Task{Filer: "codex", Title: "from mail", Status: envelope.TaskProposed})
	mine := addTask(t, s, envelope.Task{Filer: "muse", Title: "muse's from mail", Status: envelope.TaskProposed})
	owners := addTask(t, s, envelope.Task{Filer: envelope.TaskOwner, Assignee: envelope.TaskOwner, Title: "owner's own"})
	// A returned task is no longer in the pool: it carries a result.
	returned := addTask(t, s, envelope.Task{Filer: "codex", Title: "came back"})
	if _, err := s.DB().Exec(`UPDATE tasks SET result = 'the answer', result_request = 'r1' WHERE id = ?`, returned.ID); err != nil {
		t.Fatal(err)
	}

	list := func(viewer string) []int64 {
		t.Helper()
		tasks, err := s.Tasks(ctx, viewer, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return taskIDs(tasks)
	}
	same := func(got []int64, want ...int64) bool {
		return fmt.Sprint(got) == fmt.Sprint(want)
	}
	if got := list("muse"); !same(got, filed.ID, assigned.ID, pool.ID, mine.ID) {
		t.Fatalf("muse sees %v", got)
	}
	if got := list(""); len(got) != 9 {
		t.Fatalf("admin sees %v", got)
	}
	if got := list("hermes"); !same(got, pool.ID, elsewhere.ID) {
		t.Fatalf("hermes sees %v", got)
	}
	// Get applies the same filter, and hides a task as if it were missing.
	for id, want := range map[int64]bool{assigned.ID: true, pool.ID: true, heldTask.ID: false, proposed.ID: false, owners.ID: false, returned.ID: false} {
		_, err := s.Task(ctx, id, "muse")
		if want && err != nil || !want && !errors.Is(err, ErrNoTask) {
			t.Fatalf("muse get %d: %v, want visible=%v", id, err, want)
		}
	}
	// Once the owner approves the held ask, its target sees the task.
	if _, err := s.DB().Exec(`UPDATE requests SET status = 'queued' WHERE id = ?`, heldReq.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, heldTask.ID, "muse"); err != nil {
		t.Fatalf("muse get after approval: %v", err)
	}
	// A claimant keeps seeing the task after its ask is gone, and recording
	// the same claim twice is harmless.
	for range 2 {
		if err := s.RecordTaskClaimant(ctx, returned.ID, "hermes"); err != nil {
			t.Fatal(err)
		}
	}
	if got := list("hermes"); !same(got, pool.ID, elsewhere.ID, returned.ID) {
		t.Fatalf("hermes sees %v after claiming", got)
	}
	// A claim does not open a proposed task to anyone but its filer.
	if err := s.RecordTaskClaimant(ctx, proposed.ID, "hermes"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, proposed.ID, "hermes"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("hermes get of a proposed task: %v", err)
	}
	// Paging: after a cursor, up to a limit.
	if tasks, err := s.Tasks(ctx, "", filed.ID, 2); err != nil || !same(taskIDs(tasks), assigned.ID, pool.ID) {
		t.Fatalf("page = %v, %v", taskIDs(tasks), err)
	}
}
