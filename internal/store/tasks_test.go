package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
	// A missing task keeps the zero marker, and is refused for being missing.
	if c, err := s.TaskCarry(context.Background(), id); err == nil {
		req.TaskResultEvent = c.ResultEvent
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
		DROP INDEX requests_task; ALTER TABLE requests DROP COLUMN task_id; ALTER TABLE requests DROP COLUMN contributors;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var first envelope.Task
	for i := range 2 {
		s, _ = open(t, path)
		if i == 1 {
			// A tasks table from before named_for and result_authors gets
			// the columns on the next open.
			if _, err := s.DB().Exec(`ALTER TABLE tasks DROP COLUMN named_for; ALTER TABLE tasks DROP COLUMN result_authors;`); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, _ = open(t, path)
		}
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
	if all, err := s.Tasks(ctx, "", 0, 1000, true); err != nil || len(all) != MaxOpenTasks {
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
	if all, err := s.Tasks(ctx, "", 0, 100, true); err != nil || len(all) != 5 {
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
		tasks, err := s.Tasks(ctx, viewer, 0, 100, true)
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
	// Removing an agent forgets what it worked on, so a later agent under
	// its name starts with the pool alone.
	if _, err := s.DeleteAgent(ctx, "hermes"); err != nil {
		t.Fatal(err)
	}
	if got := list("hermes"); !same(got, pool.ID, elsewhere.ID) {
		t.Fatalf("hermes sees %v after its removal", got)
	}
	// A claim does not open a proposed task to anyone but its filer.
	if err := s.RecordTaskClaimant(ctx, proposed.ID, "hermes"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, proposed.ID, "hermes"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("hermes get of a proposed task: %v", err)
	}
	// Paging: after a cursor, up to a limit.
	if tasks, err := s.Tasks(ctx, "", filed.ID, 2, true); err != nil || !same(taskIDs(tasks), assigned.ID, pool.ID) {
		t.Fatalf("page = %v, %v", taskIDs(tasks), err)
	}
}

// A listing leaves done and dropped tasks out unless it asks for them, the
// count of them follows what the viewer may read, and the listing without
// them is read from the index of live tasks.
func TestTasksLeaveOutClosedUnlessAsked(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	var ids []int64
	for i, filer := range []string{"muse", "muse", "codex", "codex", "codex", "muse"} {
		ids = append(ids, addTask(t, s, envelope.Task{Filer: filer, Title: fmt.Sprintf("task %d", i)}).ID)
	}
	for i, status := range map[int]envelope.TaskStatus{0: envelope.TaskDone, 2: envelope.TaskDropped, 3: envelope.TaskDone, 4: envelope.TaskDone} {
		if _, err := s.SetTaskStatus(ctx, ids[i], status, envelope.TaskOwner, "", envelope.TaskOpen); err != nil {
			t.Fatal(err)
		}
	}
	// hermes worked on one of codex's done tasks, so it still reads it.
	if err := s.RecordTaskClaimant(ctx, ids[3], "hermes"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		viewer        string
		live, all     []int64
		done, dropped int
	}{
		{"", []int64{ids[1], ids[5]}, ids, 3, 1},
		{"muse", []int64{ids[1], ids[5]}, []int64{ids[0], ids[1], ids[5]}, 1, 0},
		{"codex", []int64{ids[1], ids[5]}, []int64{ids[1], ids[2], ids[3], ids[4], ids[5]}, 2, 1},
		{"hermes", []int64{ids[1], ids[5]}, []int64{ids[1], ids[3], ids[5]}, 1, 0},
	} {
		live, err := s.Tasks(ctx, c.viewer, 0, 100, false)
		if err != nil || !slices.Equal(taskIDs(live), c.live) {
			t.Errorf("%q lists %v, %v; want %v", c.viewer, taskIDs(live), err, c.live)
		}
		all, err := s.Tasks(ctx, c.viewer, 0, 100, true)
		if err != nil || !slices.Equal(taskIDs(all), c.all) {
			t.Errorf("%q lists %v with closed, %v; want %v", c.viewer, taskIDs(all), err, c.all)
		}
		done, dropped, err := s.ClosedTasks(ctx, c.viewer)
		if err != nil || done != c.done || dropped != c.dropped {
			t.Errorf("%q counts %d done and %d dropped, %v; want %d and %d", c.viewer, done, dropped, err, c.done, c.dropped)
		}
	}
	// The cursor pages the live tasks alone.
	if page, err := s.Tasks(ctx, "", ids[1], 100, false); err != nil || !slices.Equal(taskIDs(page), []int64{ids[5]}) {
		t.Fatalf("page after the first live task = %v, %v", taskIDs(page), err)
	}
	plan := func(query string, args ...any) string {
		t.Helper()
		rows, err := s.DB().Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			out = append(out, detail)
		}
		return strings.Join(out, "; ")
	}
	if got := plan(taskSelect+` WHERE `+taskVisible+` AND t.`+taskOpenStatuses+` AND t.id > ?2 ORDER BY t.id LIMIT ?3`, "", 0, 100); !strings.Contains(got, "tasks_live") {
		t.Errorf("the listing of live tasks does not use their index: %s", got)
	}
	if got := plan(`SELECT status, COUNT(*) FROM tasks WHERE status IN ('done', 'dropped') GROUP BY status`); !strings.Contains(got, "COVERING INDEX tasks_status") {
		t.Errorf("the admin's count of closed tasks does not use the status index: %s", got)
	}
}

// taskUnderAsk files a task as codex and sends muse one ask under it.
func taskUnderAsk(t *testing.T, s *Store, held bool) (envelope.Task, envelope.Request) {
	t.Helper()
	task := addTask(t, s, envelope.Task{Filer: "codex", Title: "ship it"})
	req, err := attempt(s, task.ID, "codex", "muse", held)
	if err != nil {
		t.Fatal(err)
	}
	return task, req
}

func claimAsk(t *testing.T, s *Store, id, agent string) {
	t.Helper()
	if _, err := s.Claim(context.Background(), id, agent, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func lastTaskEvent(t *testing.T, s *Store, id int64) envelope.TaskEvent {
	t.Helper()
	events, err := s.TaskEvents(context.Background(), id, 0, 100)
	if err != nil || len(events) == 0 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	return events[len(events)-1]
}

// Every store write that ends, pauses or moves a request moves the task the
// request is an attempt at: one request per way, each checked on the task.
func TestTaskFollowsEveryRequestChange(t *testing.T) {
	ctx := context.Background()
	answered := envelope.Reply{Status: envelope.StatusAnswered, Body: "the answer"}
	type want struct {
		status   envelope.TaskStatus
		assignee string
		open     bool   // the ask is still the task's open ask
		result   string // the stored result
		actor    string // who the last history entry names
		note     string // text the last history entry carries
	}
	released := func(actor, note string) want {
		return want{status: envelope.TaskOpen, actor: actor, note: note}
	}
	cases := []struct {
		name string
		held bool
		end  func(t *testing.T, s *Store, c *clock, req envelope.Request) error
		want want
	}{
		{name: "answered", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			_, err := s.Reply(ctx, req.ID, "muse", answered)
			return err
		}, want: want{status: envelope.TaskReturned, assignee: "muse", result: "the answer", actor: "muse"}},
		{name: "answered by email", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.ReplyByEmail(ctx, "<m1>", req.ID, "muse", 0, answered)
			return err
		}, want: want{status: envelope.TaskReturned, assignee: "muse", result: "the answer", actor: "muse"}},
		{name: "failed", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			_, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusFailed, Body: "no access to the repo"})
			return err
		}, want: released("muse", "failed: no access to the repo")},
		{name: "declined", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusDeclined, Body: "not mine"})
			return err
		}, want: released("muse", "declined: not mine")},
		{name: "failed while held back", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.FailHeld(ctx, req.ID, "muse", false, envelope.Reply{Status: envelope.StatusFailed, Body: "out of messages"})
			return err
		}, want: released("muse", "failed: out of messages")},
		{name: "needs input", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			_, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"})
			return err
		}, want: want{status: envelope.TaskQuestion, assignee: "muse", open: true, actor: "muse", note: "which branch?"}},
		{name: "answered by sender", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"}); err != nil {
				return err
			}
			_, err := s.Answer(ctx, req.ID, "codex", "main")
			return err
		}, want: want{status: envelope.TaskWorking, assignee: "muse", open: true, actor: "codex", note: "answered the question"}},
		{name: "cancelled by sender", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.Cancel(ctx, req.ID, "codex")
			return err
		}, want: released("codex", "cancelled")},
		{name: "target removed", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			_, err := s.CancelAllFor(ctx, "muse")
			return err
		}, want: released(TaskRelay, "muse was removed")},
		{name: "sender removed", end: func(_ *testing.T, s *Store, _ *clock, _ envelope.Request) error {
			_, err := s.CancelAllFor(ctx, "codex")
			return err
		}, want: released(TaskRelay, "codex was removed")},
		{name: "expired in the queue", end: func(_ *testing.T, s *Store, c *clock, _ envelope.Request) error {
			c.advance(2 * time.Hour)
			_, err := s.Sweep(ctx)
			return err
		}, want: released(TaskRelay, "expired")},
		{name: "expired while claimed", end: func(t *testing.T, s *Store, c *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			c.advance(2 * time.Hour)
			_, err := s.Sweep(ctx)
			return err
		}, want: released(TaskRelay, "expired")},
		{name: "expired waiting for input", end: func(t *testing.T, s *Store, c *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"}); err != nil {
				return err
			}
			c.advance(2 * time.Hour)
			_, err := s.Sweep(ctx)
			return err
		}, want: released(TaskRelay, "expired")},
		{name: "hold timed out", held: true, end: func(_ *testing.T, s *Store, c *clock, _ envelope.Request) error {
			c.advance(2 * time.Hour)
			_, err := s.Sweep(ctx)
			return err
		}, want: released(TaskRelay, "expired while held for the owner's approval")},
		{name: "denied by owner", held: true, end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.DenyHeld(ctx, req.ID, "not this one")
			return err
		}, want: released(envelope.TaskOwner, "denied: not this one")},
		{name: "reply replaces a held acknowledgement", end: func(t *testing.T, s *Store, c *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			if err := s.HoldAck(ctx, req.ID, "muse", "on it", 30*time.Minute, 30*time.Minute); err != nil {
				return err
			}
			c.advance(31 * time.Minute)
			if reps, err := s.CloseHeldAcks(ctx, func(string, string) bool { return true }); err != nil || len(reps) != 1 {
				t.Fatalf("close held acks = %+v, %v", reps, err)
			}
			task, err := s.Task(ctx, req.TaskID, "")
			if err != nil || task.Status != envelope.TaskReturned || task.Result != "on it" || task.ResultRequest != req.ID {
				t.Fatalf("task after the acknowledgement closed its ask = %+v, %v", task, err)
			}
			_, err = s.Reply(ctx, req.ID, "muse", answered)
			return err
		}, want: want{status: envelope.TaskReturned, assignee: "muse", result: "the answer", actor: "muse"}},
		{name: "moved by the owner's fallback", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.RerouteUntaken(ctx, req.ID, "muse", "scout")
			return err
		}, want: want{status: envelope.TaskWorking, assignee: "scout", open: true, actor: TaskRelay, note: "scout"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := open(t, ":memory:")
			task, req := taskUnderAsk(t, s, tc.held)
			if err := tc.end(t, s, c, req); err != nil {
				t.Fatal(err)
			}
			got, err := s.Task(ctx, task.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			current := ""
			if tc.want.open {
				current = req.ID
			}
			if got.Status != tc.want.status || got.Assignee != tc.want.assignee || got.CurrentRequest != current || got.Result != tc.want.result {
				t.Fatalf("task = %+v, want status %s, assignee %q, open ask %q, result %q", got, tc.want.status, tc.want.assignee, current, tc.want.result)
			}
			if tc.want.result != "" && got.ResultRequest != req.ID {
				t.Fatalf("result request = %q, want %q", got.ResultRequest, req.ID)
			}
			if ev := lastTaskEvent(t, s, task.ID); ev.Actor != tc.want.actor || !strings.Contains(ev.Body, tc.want.note) || ev.Status != tc.want.status {
				t.Fatalf("last history entry = %+v, want actor %q and note %q", ev, tc.want.actor, tc.want.note)
			}
		})
	}
}

// A reply with files leaves them on its request: the task's result ends
// with a line naming the request and the files.
func TestTaskResultPointsAtAttachments(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	claimAsk(t, s, req.ID, "muse")
	a, b := upload(t, s, "muse", "report.pdf", 10), upload(t, s, "muse", "chart.png", 10)
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "see the files", Attachments: []envelope.Attachment{{ID: a}, {ID: b}}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, task.ID, "")
	want := "see the files\n[2 attachment(s) on request " + req.ID + ": report.pdf, chart.png]"
	if err != nil || got.Result != want || got.Status != envelope.TaskReturned {
		t.Fatalf("task = %+v, %v; want result %q", got, err, want)
	}
}

// Done is refused while an ask is open under the task, working or waiting
// on a question, and allowed once the task is returned.
func TestMarkTaskDoneRefusedWhileAskOpen(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	if _, err := s.MarkTaskDone(ctx, task.ID, envelope.TaskOwner, ""); !errors.Is(err, ErrTaskAskOpen) {
		t.Fatalf("done while working: %v", err)
	}
	claimAsk(t, s, req.ID, "muse")
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkTaskDone(ctx, task.ID, envelope.TaskOwner, ""); !errors.Is(err, ErrTaskAskOpen) {
		t.Fatalf("done while a question is open: %v", err)
	}
	if got, err := s.Task(ctx, task.ID, ""); err != nil || got.Status != envelope.TaskQuestion || got.CurrentRequest != req.ID {
		t.Fatalf("task after refused done = %+v, %v", got, err)
	}
	if _, err := s.Answer(ctx, req.ID, "codex", "main"); err != nil {
		t.Fatal(err)
	}
	claimAsk(t, s, req.ID, "muse")
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "shipped"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.MarkTaskDone(ctx, task.ID, envelope.TaskOwner, "good")
	if err != nil || got.Status != envelope.TaskDone || got.Result != "shipped" {
		t.Fatalf("done after return = %+v, %v", got, err)
	}
	if _, err := s.MarkTaskDone(ctx, task.ID, envelope.TaskOwner, ""); !errors.Is(err, ErrTaskState) {
		t.Fatalf("done twice: %v", err)
	}
	if _, err := s.MarkTaskDone(ctx, task.ID+9, envelope.TaskOwner, ""); !errors.Is(err, ErrNoTask) {
		t.Fatalf("done on no task: %v", err)
	}
}

// Dropping a task cancels the ask open under it in the same transaction,
// claimed or not, so the assignee's later reply is refused as a reply to a
// cancelled request is. A task with no ask keeps its assignee.
func TestDropTaskCancelsOpenAsk(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	claimAsk(t, s, req.ID, "muse")
	got, cancelled, err := s.DropTask(ctx, task.ID, envelope.TaskOwner, "not needed")
	if err != nil || got.Status != envelope.TaskDropped || got.CurrentRequest != "" || got.Assignee != "" || !slices.Equal(cancelled, []string{req.ID}) {
		t.Fatalf("drop = %+v, %v, %v", got, cancelled, err)
	}
	if _, st, err := s.Request(ctx, req.ID); err != nil || st != envelope.StatusCancelled {
		t.Fatalf("request after drop = %s, %v", st, err)
	}
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "too late"}); !errors.Is(err, ErrWrongState) {
		t.Fatalf("reply after drop: %v", err)
	}
	if after, err := s.Task(ctx, task.ID, ""); err != nil || after.Status != envelope.TaskDropped || after.Result != "" {
		t.Fatalf("task after the late reply = %+v, %v", after, err)
	}
	if ev := lastTaskEvent(t, s, task.ID); ev.Actor != envelope.TaskOwner || ev.Body != "not needed" || ev.Status != envelope.TaskDropped {
		t.Fatalf("history = %+v", ev)
	}
	if _, _, err := s.DropTask(ctx, task.ID, envelope.TaskOwner, ""); !errors.Is(err, ErrTaskState) {
		t.Fatalf("drop twice: %v", err)
	}
	if _, _, err := s.DropTask(ctx, task.ID+9, envelope.TaskOwner, ""); !errors.Is(err, ErrNoTask) {
		t.Fatalf("drop of no task: %v", err)
	}
	own := addTask(t, s, envelope.Task{Filer: envelope.TaskOwner, Assignee: envelope.TaskOwner, Title: "call the bank"})
	got, cancelled, err = s.DropTask(ctx, own.ID, envelope.TaskOwner, "")
	if err != nil || got.Status != envelope.TaskDropped || got.Assignee != envelope.TaskOwner || len(cancelled) != 0 {
		t.Fatalf("drop with no ask = %+v, %v, %v", got, cancelled, err)
	}
}

// Releasing a task cancels the ask open under it and leaves the task open
// with no assignee, ready for another ask; with no ask open it changes
// nothing.
func TestReleaseTaskCancelsOpenAsk(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	claimAsk(t, s, req.ID, "muse")
	cancelled, err := s.ReleaseTask(ctx, task.ID, envelope.TaskOwner, "reassigned to hermes")
	if err != nil || !slices.Equal(cancelled, []string{req.ID}) {
		t.Fatalf("release = %v, %v", cancelled, err)
	}
	got, err := s.Task(ctx, task.ID, "")
	if err != nil || got.Status != envelope.TaskOpen || got.Assignee != "" || got.CurrentRequest != "" {
		t.Fatalf("task = %+v, %v", got, err)
	}
	if _, st, _ := s.Request(ctx, req.ID); st != envelope.StatusCancelled {
		t.Fatalf("request = %s", st)
	}
	if ev := lastTaskEvent(t, s, task.ID); ev.Actor != envelope.TaskOwner || ev.Body != "reassigned to hermes" || ev.Status != envelope.TaskOpen {
		t.Fatalf("history = %+v", ev)
	}
	next, err := attempt(s, task.ID, "codex", "hermes", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Task(ctx, task.ID, ""); err != nil || got.Assignee != "hermes" || got.CurrentRequest != next.ID {
		t.Fatalf("task after the next ask = %+v, %v", got, err)
	}
	idle := addTask(t, s, envelope.Task{Filer: "codex", Title: "idle"})
	before, _ := s.TaskEvents(ctx, idle.ID, 0, 10)
	if cancelled, err := s.ReleaseTask(ctx, idle.ID, envelope.TaskOwner, "x"); err != nil || len(cancelled) != 0 {
		t.Fatalf("release with no ask = %v, %v", cancelled, err)
	}
	if after, _ := s.TaskEvents(ctx, idle.ID, 0, 10); len(after) != len(before) {
		t.Fatalf("release with no ask wrote history: %+v", after)
	}
	if _, err := s.ReleaseTask(ctx, idle.ID+9, envelope.TaskOwner, "x"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("release of no task: %v", err)
	}
}

// Claiming a task's ask records the claimer on the task, so it still reads
// the task once the ask is gone, and its progress notes join the task's
// history. A plain ask writes neither.
func TestTaskAskClaimAndProgress(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	plain := ask(t, s, "codex", "muse", "hi")
	claimAsk(t, s, plain.ID, "muse")
	if err := s.SetProgress(ctx, plain.ID, "muse", "on the plain one", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT (SELECT COUNT(*) FROM task_claimants) + (SELECT COUNT(*) FROM task_events WHERE kind = 'progress')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a plain ask wrote %d task rows, %v", n, err)
	}
	claimAsk(t, s, req.ID, "muse")
	if err := s.SetProgress(ctx, req.ID, "muse", "halfway", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if ev := lastTaskEvent(t, s, task.ID); ev.Kind != envelope.TaskEventProgress || ev.Actor != "muse" || ev.Body != "halfway" {
		t.Fatalf("history = %+v", ev)
	}
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusFailed, Body: "stuck"}); err != nil {
		t.Fatal(err)
	}
	// Open, unassigned, no result: anyone reads it. A result closes that
	// door, and the claim keeps muse's open.
	if _, err := s.DB().Exec(`UPDATE tasks SET result = 'x', result_request = 'r' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, task.ID, "muse"); err != nil {
		t.Fatalf("claimer reads the task: %v", err)
	}
	if _, err := s.Task(ctx, task.ID, "hermes"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("a stranger reads the task: %v", err)
	}
}

// The next ask carries the comments made since the stored result, and who
// wrote that result.
func TestTaskCarry(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	comment := func(actor, body string) {
		t.Helper()
		if _, err := s.AppendTaskEvent(ctx, envelope.TaskEvent{TaskID: task.ID, Actor: actor, Kind: envelope.TaskEventComment, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	comment("codex", "before the result")
	c, err := s.TaskCarry(ctx, task.ID)
	if by, comments := c.ResultBy, c.Comments; err != nil || by != "" || len(comments) != 1 || comments[0].Body != "before the result" || c.ResultEvent != 0 || c.ResultAuthors != nil {
		t.Fatalf("carry with no result = %+v, %v", c, err)
	}
	claimAsk(t, s, req.ID, "muse")
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "first pass"}); err != nil {
		t.Fatal(err)
	}
	comment("hermes", "needs a test")
	comment(envelope.TaskOwner, "and a changelog line")
	c, err = s.TaskCarry(ctx, task.ID)
	if by, comments := c.ResultBy, c.Comments; err != nil || by != "muse" || len(comments) != 2 || comments[0].Actor != "hermes" || comments[0].Body != "needs a test" || comments[1].Actor != envelope.TaskOwner {
		t.Fatalf("carry = %+v, %v", c, err)
	}
	if c.Task.Result != "first pass" || c.Task.ResultRequest != req.ID || c.ResultEvent == 0 {
		t.Fatalf("carried task = %+v, result entry %d", c.Task, c.ResultEvent)
	}
	if _, err := s.TaskCarry(ctx, task.ID+9); !errors.Is(err, ErrNoTask) {
		t.Fatalf("carry of a missing task: %v", err)
	}
}

// A late reply that replaces a held acknowledgement changes nothing on a
// task that was assigned again since.
func TestReplacedReplyLeavesMovedOnTask(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	claimAsk(t, s, req.ID, "muse")
	if err := s.HoldAck(ctx, req.ID, "muse", "on it", 30*time.Minute, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(31 * time.Minute)
	if reps, err := s.CloseHeldAcks(ctx, func(string, string) bool { return true }); err != nil || len(reps) != 1 {
		t.Fatalf("close held acks = %+v, %v", reps, err)
	}
	next, err := attempt(s, task.ID, "codex", "hermes", false)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.TaskEvents(ctx, task.ID, 0, 100)
	rep, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "the real answer"})
	if err != nil || !rep.Replaced {
		t.Fatalf("late reply = %+v, %v", rep, err)
	}
	got, err := s.Task(ctx, task.ID, "")
	if err != nil || got.Status != envelope.TaskWorking || got.Assignee != "hermes" || got.CurrentRequest != next.ID || got.Result != "on it" {
		t.Fatalf("task = %+v, %v", got, err)
	}
	if after, _ := s.TaskEvents(ctx, task.ID, 0, 100); len(after) != len(before) {
		t.Fatalf("the late reply wrote history: %+v", after[len(before):])
	}
}

// A relay stopped between a task's ask and its reply finds the task
// working with its ask open, and the reply still hands it back.
func TestTaskAskSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	s.Close()
	s, _ = open(t, path)
	got, err := s.Task(ctx, task.ID, "")
	if err != nil || got.Status != envelope.TaskWorking || got.CurrentRequest != req.ID || got.AskStatus != envelope.StatusQueued {
		t.Fatalf("task after restart = %+v, %v", got, err)
	}
	claimAsk(t, s, req.ID, "muse")
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "done"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Task(ctx, task.ID, ""); err != nil || got.Status != envelope.TaskReturned || got.Result != "done" {
		t.Fatalf("task after the reply = %+v, %v", got, err)
	}
}

// The agents whose text a request carries are stored with it and read back;
// a plain request has none.
func TestRequestKeepsContributors(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req, err := s.Enqueue(ctx, envelope.Request{From: "codex", To: "muse", Kind: envelope.KindAsk, Body: "x", Hop: 1, Chain: []string{}, Contributors: []string{"hermes", "scout"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.Request(ctx, req.ID); err != nil || !slices.Equal(got.Contributors, []string{"hermes", "scout"}) {
		t.Fatalf("request = %+v, %v", got, err)
	}
	plain := ask(t, s, "codex", "muse", "hi")
	if got, _, err := s.Request(ctx, plain.ID); err != nil || got.Contributors != nil {
		t.Fatalf("plain request = %+v, %v", got, err)
	}
}

// A task stored with a teammate named for it is out of the shared pool from
// the insert: before any ask is written under it, so before it has an
// assigned entry, only its filer and an admin read it. The same row with
// nobody named is in the pool.
func TestTaskNamedForATeammateIsNeverInThePool(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	pool := addTask(t, s, envelope.Task{Filer: "codex", Title: "anyone"})
	named := addTask(t, s, envelope.Task{Filer: "codex", Title: "for muse alone", NamedFor: "muse"})
	owners := addTask(t, s, envelope.Task{Filer: envelope.TaskOwner, Title: "the owner's, for muse", NamedFor: "muse"})
	if got := readTask(t, s, named.ID); got.Status != envelope.TaskOpen || got.Assignee != "" || got.CurrentRequest != "" {
		t.Fatalf("named task as stored = %+v, want open with no assignee and no ask", got)
	}
	for _, ev := range taskHistory(t, s, named.ID) {
		if ev.Kind == envelope.TaskEventAssigned {
			t.Fatalf("named task already has an assigned entry: %+v", ev)
		}
	}
	for viewer, want := range map[string][]int64{
		"":       {pool.ID, named.ID, owners.ID},
		"codex":  {pool.ID, named.ID},
		"hermes": {pool.ID},
		// The teammate it names reads it once an ask reaches it, not before.
		"muse": {pool.ID},
	} {
		tasks, err := s.Tasks(ctx, viewer, 0, 10, true)
		if err != nil || !slices.Equal(taskIDs(tasks), want) {
			t.Errorf("%q lists %v, %v; want %v", viewer, taskIDs(tasks), err, want)
		}
		for _, id := range []int64{pool.ID, named.ID, owners.ID} {
			_, err := s.Task(ctx, id, viewer)
			if reads := slices.Contains(want, id); reads != (err == nil) || err != nil && !errors.Is(err, ErrNoTask) {
				t.Errorf("%q reads task %d: %v, want readable %v", viewer, id, err, reads)
			}
		}
	}
	// Approving a proposed task named for a teammate does not put it there.
	proposed := addTask(t, s, envelope.Task{Filer: "codex", Title: "from mail", Status: envelope.TaskProposed, SuggestedAssignee: "muse", NamedFor: "muse"})
	if _, err := s.SetTaskStatus(ctx, proposed.ID, envelope.TaskOpen, envelope.TaskOwner, "", envelope.TaskProposed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, proposed.ID, "hermes"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("an unrelated agent reads an approved task named for a teammate: %v", err)
	}
	if _, err := s.Task(ctx, proposed.ID, "codex"); err != nil {
		t.Fatalf("the filer cannot read it: %v", err)
	}
	// Once its ask is written the teammate reads it, as any assignee does.
	if _, err := attempt(s, named.ID, "codex", "muse", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, named.ID, "muse"); err != nil {
		t.Fatalf("the assignee cannot read the task: %v", err)
	}
}

func readTask(t *testing.T, s *Store, id int64) envelope.Task {
	t.Helper()
	task, err := s.Task(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func taskHistory(t *testing.T, s *Store, id int64) []envelope.TaskEvent {
	t.Helper()
	events, err := s.TaskEvents(context.Background(), id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// A result is stored with the agents behind the ask it answers: who
// replied, and the ask's chain, sender and contributors, never the owner. A
// reply that replaces a held acknowledgement stores them again.
func TestTaskResultKeepsItsAuthors(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	task := addTask(t, s, envelope.Task{Filer: "codex", Title: "ship it"})
	req, err := s.Enqueue(ctx, envelope.Request{From: "codex", To: "muse", Kind: envelope.KindAsk, Body: "do it", Hop: 2, Chain: []string{"hermes", "codex"},
		Contributors: []string{"scout", envelope.TaskOwner, "hermes"}, TaskID: task.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	authors := func() []string {
		t.Helper()
		carried, err := s.TaskCarry(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return carried.ResultAuthors
	}
	if got := authors(); got != nil {
		t.Fatalf("authors of no result = %v", got)
	}
	want := []string{"muse", "hermes", "codex", "scout"}
	claimAsk(t, s, req.ID, "muse")
	if err := s.HoldAck(ctx, req.ID, "muse", "on it", 30*time.Minute, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(31 * time.Minute)
	if reps, err := s.CloseHeldAcks(ctx, func(string, string) bool { return true }); err != nil || len(reps) != 1 {
		t.Fatalf("close held acks = %+v, %v", reps, err)
	}
	if got := authors(); !slices.Equal(got, want) {
		t.Fatalf("authors of the acknowledgement = %v, want %v", got, want)
	}
	// A row from before the column has none, and the replacement writes
	// them with the result it stores.
	if _, err := s.DB().Exec(`UPDATE tasks SET result_authors = NULL WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if got := authors(); got != nil {
		t.Fatalf("authors of a result stored without them = %v", got)
	}
	if rep, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "the real answer"}); err != nil || !rep.Replaced {
		t.Fatalf("late reply = %+v, %v", rep, err)
	}
	if got := authors(); !slices.Equal(got, want) {
		t.Fatalf("authors of the replacement = %v, want %v", got, want)
	}
}

// A comment made on a task returned with a held acknowledgement was in no
// ask, so it is still carried after the real answer replaces the
// acknowledgement. The replacement is a new version of the result, and the
// comments from before the acknowledgement stay out.
func TestReplacedReplyKeepsCommentsSinceTheAcknowledgement(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	comment := func(actor, body string) {
		t.Helper()
		if _, err := s.AppendTaskEvent(ctx, envelope.TaskEvent{TaskID: task.ID, Actor: actor, Kind: envelope.TaskEventComment, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	carried := func() TaskCarried {
		t.Helper()
		out, err := s.TaskCarry(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	bodies := func(out TaskCarried) string {
		list := []string{}
		for _, ev := range out.Comments {
			list = append(list, ev.Body)
		}
		return strings.Join(list, "|")
	}
	comment("codex", "before the attempt ended")
	claimAsk(t, s, req.ID, "muse")
	if err := s.HoldAck(ctx, req.ID, "muse", "on it", 30*time.Minute, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(31 * time.Minute)
	if reps, err := s.CloseHeldAcks(ctx, func(string, string) bool { return true }); err != nil || len(reps) != 1 {
		t.Fatalf("close held acks = %+v, %v", reps, err)
	}
	comment("hermes", "on the acknowledgement")
	acked := carried()
	if acked.Task.Status != envelope.TaskReturned || acked.Task.Result != "on it" || bodies(acked) != "on the acknowledgement" {
		t.Fatalf("carry after the acknowledgement = %+v", acked)
	}
	if rep, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "the real answer"}); err != nil || !rep.Replaced {
		t.Fatalf("late reply = %+v, %v", rep, err)
	}
	comment("codex", "on the real answer")
	got := carried()
	if got.Task.Result != "the real answer" || got.ResultBy != "muse" || bodies(got) != "on the acknowledgement|on the real answer" {
		t.Fatalf("carry after the replacement = %+v, want the comment made on the acknowledgement kept", got)
	}
	if got.ResultEvent <= acked.ResultEvent {
		t.Fatalf("result entry %d after the replacement, %d before: want a later one", got.ResultEvent, acked.ResultEvent)
	}
	// The next attempt was handed both, so its result starts a new count.
	next, err := attempt(s, task.ID, "codex", "hermes", false)
	if err != nil {
		t.Fatal(err)
	}
	claimAsk(t, s, next.ID, "hermes")
	if _, err := s.Reply(ctx, next.ID, "hermes", envelope.Reply{Status: envelope.StatusAnswered, Body: "second pass"}); err != nil {
		t.Fatal(err)
	}
	comment("codex", "on the second pass")
	if got := carried(); got.ResultBy != "hermes" || bodies(got) != "on the second pass" {
		t.Fatalf("carry after the next attempt = %+v", got)
	}
}

// An ask is stored under a task only while the task's result is the one the
// ask was built from.
func TestTaskClaimNeedsTheResultItWasBuiltFrom(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	stale, err := s.TaskCarry(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimAsk(t, s, req.ID, "muse")
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "first pass"}); err != nil {
		t.Fatal(err)
	}
	ask := envelope.Request{From: "codex", To: "hermes", Kind: envelope.KindAsk, Body: "do it", Hop: 1, Chain: []string{}, TaskID: task.ID, TaskResultEvent: stale.ResultEvent}
	if _, err := s.Enqueue(ctx, ask, time.Hour); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("ask built before the result landed: %v, want ErrTaskBusy", err)
	}
	if got := readTask(t, s, task.ID); got.Status != envelope.TaskReturned || got.CurrentRequest != "" || got.Assignee != "muse" {
		t.Fatalf("task after the refused ask = %+v", got)
	}
	fresh, err := s.TaskCarry(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	ask.TaskResultEvent = fresh.ResultEvent
	if _, err := s.Enqueue(ctx, ask, time.Hour); err != nil {
		t.Fatalf("ask built from the stored result: %v", err)
	}
}

// A task host is shown every task but one whose open ask is held with the
// host as its target. Of a task filed as proposed that the owner never
// approved, a host that did not file it gets a row with the id, status,
// filer, source and creation time and every other field empty, in the list,
// read alone, open and dropped; the host that filed it reads it whole, and
// so does any host once the owner has approved it.
func TestHostTasksWithholdNeverApprovedText(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	working := addTask(t, s, envelope.Task{Filer: "codex", Title: "hermes works"})
	if _, err := attempt(s, working.ID, "codex", "hermes", false); err != nil {
		t.Fatal(err)
	}
	heldTask := addTask(t, s, envelope.Task{Filer: "codex", Title: "held for the owner"})
	heldReq, err := attempt(s, heldTask.ID, "codex", "muse", true)
	if err != nil {
		t.Fatal(err)
	}
	proposed := addTask(t, s, envelope.Task{Filer: "codex", Title: "from mail", Detail: "the body", Key: "mail-7", Source: "mail", SuggestedAssignee: "hermes", Status: envelope.TaskProposed})
	// Fields no proposed task holds today are filled in too: the row is
	// built from what it may carry, not from what happens to be empty.
	if _, err := s.DB().Exec(`UPDATE tasks SET result = 'a result', result_request = 'r1', agent_assigns = 2, assignee = 'hermes' WHERE id = ?`, proposed.ID); err != nil {
		t.Fatal(err)
	}
	mine := addTask(t, s, envelope.Task{Filer: "muse", Title: "muse's from mail", Source: "mail", Status: envelope.TaskProposed})
	done := addTask(t, s, envelope.Task{Filer: envelope.TaskOwner, Title: "owner's, done"})
	if _, err := s.MarkTaskDone(ctx, done.ID, envelope.TaskOwner, ""); err != nil {
		t.Fatal(err)
	}

	list := func(closed bool) map[int64]envelope.Task {
		t.Helper()
		tasks, err := s.HostTasks(ctx, "muse", 0, 100, closed)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]envelope.Task{}
		for _, task := range tasks {
			one, err := s.HostTask(ctx, task.ID, "muse")
			if err != nil || !reflect.DeepEqual(one, task) {
				t.Fatalf("task %d read alone = %+v, %v; listed as %+v", task.ID, one, err, task)
			}
			out[task.ID] = task
		}
		return out
	}
	withheld := func(got envelope.Task, status envelope.TaskStatus) {
		t.Helper()
		want := envelope.Task{ID: proposed.ID, Status: status, Filer: "codex", Source: "mail", CreatedAt: proposed.CreatedAt, Withheld: true}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("withheld row = %+v, want only %+v", got, want)
		}
	}

	shown := list(false)
	if got := slices.Sorted(maps.Keys(shown)); !slices.Equal(got, []int64{working.ID, proposed.ID, mine.ID}) {
		t.Fatalf("host is shown %v", got)
	}
	if got := shown[working.ID]; got.Title != "hermes works" || got.Withheld {
		t.Fatalf("a teammate's task = %+v", got)
	}
	if got := shown[mine.ID]; got.Title != "muse's from mail" || got.Withheld {
		t.Fatalf("the host's own proposed task = %+v", got)
	}
	withheld(shown[proposed.ID], envelope.TaskProposed)
	if _, err := s.HostTask(ctx, heldTask.ID, "muse"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("task whose ask is held for the host: %v", err)
	}
	// The agent that filed the held task is shown it as a host.
	if got, err := s.HostTask(ctx, heldTask.ID, "codex"); err != nil || got.Title != "held for the owner" {
		t.Fatalf("the filer's read of its held task = %+v, %v", got, err)
	}
	if got, err := s.HostTask(ctx, proposed.ID, "codex"); err != nil || got.Withheld || got.Detail != "the body" || got.Key != "mail-7" {
		t.Fatalf("the filing host's read = %+v, %v", got, err)
	}

	// Dropped unapproved, it is withheld still, and counted.
	if _, err := s.SetTaskStatus(ctx, proposed.ID, envelope.TaskDropped, "codex", "the reason", envelope.TaskProposed); err != nil {
		t.Fatal(err)
	}
	if _, has := list(false)[proposed.ID]; has {
		t.Fatal("a dropped task is listed without closed")
	}
	withheld(list(true)[proposed.ID], envelope.TaskDropped)
	if doneN, dropped, err := s.ClosedHostTasks(ctx, "muse"); err != nil || doneN != 1 || dropped != 1 {
		t.Fatalf("closed counts for a host = %d done, %d dropped, %v", doneN, dropped, err)
	}

	// The owner's reopen is not an approval. The approval is.
	if _, err := s.ReopenTask(ctx, proposed.ID, envelope.TaskOwner, ""); err != nil {
		t.Fatal(err)
	}
	withheld(list(false)[proposed.ID], envelope.TaskProposed)
	if _, err := s.SetTaskStatus(ctx, proposed.ID, envelope.TaskOpen, envelope.TaskOwner, "", envelope.TaskProposed); err != nil {
		t.Fatal(err)
	}
	if got := list(false)[proposed.ID]; got.Withheld || got.Title != "from mail" || got.Detail != "the body" {
		t.Fatalf("approved task as a host reads it = %+v", got)
	}
	if _, err := s.SetTaskStatus(ctx, proposed.ID, envelope.TaskDropped, envelope.TaskOwner, ""); err != nil {
		t.Fatal(err)
	}
	if got := list(true)[proposed.ID]; got.Withheld || got.Title != "from mail" {
		t.Fatalf("approved, then dropped task as a host reads it = %+v", got)
	}

	// Once the owner approves the held ask, the host is shown its task.
	if _, err := s.Release(ctx, heldReq.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, err := s.HostTask(ctx, heldTask.ID, "muse"); err != nil || got.Title != "held for the owner" {
		t.Fatalf("task after its held ask was approved = %+v, %v", got, err)
	}
}
