package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// taskRelay advertises the task list, records every other call with its
// body, and answers each with the JSON kept for it, or an empty object.
type taskRelay struct {
	calls   []string
	answers map[string]string
}

func (f *taskRelay) Raw(_ context.Context, method, path string, in, out any) error {
	if path == "/v1/capabilities" {
		*(out.(*Capabilities)) = Capabilities{Tasks: true}
		return nil
	}
	call := method + " " + path
	if in != nil {
		body, err := json.Marshal(in)
		if err != nil {
			return err
		}
		call += " " + string(body)
	}
	f.calls = append(f.calls, call)
	answer, ok := f.answers[method+" "+path]
	if !ok {
		answer = `{"task":{"id":7,"status":"open"}}`
	}
	return json.Unmarshal([]byte(answer), out)
}

// A relay that does not advertise the task list is refused before anything
// is sent, with the one error every task call shares.
func TestTaskCallsRefuseOldRelay(t *testing.T) {
	old := &oldRelay{}
	ctx := t.Context()
	calls := map[string]func() error{
		"add":     func() error { _, err := AddTask(ctx, old, false, TaskAdd{Title: "x"}); return err },
		"list":    func() error { _, _, err := Tasks(ctx, old, true, false); return err },
		"show":    func() error { _, _, err := Task(ctx, old, false, 1); return err },
		"assign":  func() error { _, err := AssignTask(ctx, old, true, 1, "muse"); return err },
		"comment": func() error { _, err := CommentTask(ctx, old, false, 1, "x"); return err },
		"drop":    func() error { _, err := DropTask(ctx, old, false, 1, ""); return err },
		"decide":  func() error { _, err := DecideTask(ctx, old, TaskDone, 1, ""); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrTasksUnsupported) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(old.calls) != 0 {
		t.Fatalf("calls reached the relay: %v", old.calls)
	}
}

func TestTaskCallsUseTheirRoutes(t *testing.T) {
	ctx := t.Context()
	f := &taskRelay{}
	steps := []struct {
		call func() error
		want string
	}{
		{func() error {
			_, err := AddTask(ctx, f, false, TaskAdd{Title: "book", Assignee: "muse", Source: "mail"})
			return err
		}, `POST /v1/tasks {"title":"book","assignee":"muse","source":"mail"}`},
		{func() error { _, err := AddTask(ctx, f, true, TaskAdd{Title: "book", Key: "k"}); return err }, `POST /v1/admin/tasks {"title":"book","key":"k"}`},
		{func() error { _, _, err := Tasks(ctx, f, false, false); return err }, `GET /v1/tasks?after=0`},
		{func() error { _, _, err := Tasks(ctx, f, true, false); return err }, `GET /v1/admin/tasks?after=0`},
		{func() error { _, _, err := Tasks(ctx, f, false, true); return err }, `GET /v1/tasks?closed=1&after=0`},
		{func() error { _, _, err := Tasks(ctx, f, true, true); return err }, `GET /v1/admin/tasks?closed=1&after=0`},
		{func() error { _, _, err := Task(ctx, f, false, 7); return err }, `GET /v1/tasks/7?after=0`},
		{func() error { _, _, err := Task(ctx, f, true, 7); return err }, `GET /v1/admin/tasks/7?after=0`},
		{func() error { _, err := AssignTask(ctx, f, false, 7, "muse"); return err }, `POST /v1/tasks/7/assign {"to":"muse"}`},
		{func() error { _, err := AssignTask(ctx, f, true, 7, "muse"); return err }, `POST /v1/admin/tasks/7/assign {"to":"muse"}`},
		{func() error { _, err := CommentTask(ctx, f, false, 7, "hi"); return err }, `POST /v1/tasks/7/comment {"body":"hi"}`},
		{func() error { _, err := CommentTask(ctx, f, true, 7, "hi"); return err }, `POST /v1/admin/tasks/7/comment {"body":"hi"}`},
		{func() error { _, err := DropTask(ctx, f, false, 7, "late"); return err }, `POST /v1/tasks/7/drop {"note":"late"}`},
		{func() error { _, err := DropTask(ctx, f, true, 7, ""); return err }, `POST /v1/admin/tasks/7/drop {"note":""}`},
		{func() error { _, err := DecideTask(ctx, f, TaskApprove, 7, ""); return err }, `POST /v1/admin/tasks/7/approve {"note":""}`},
		{func() error { _, err := DecideTask(ctx, f, TaskDone, 7, "good"); return err }, `POST /v1/admin/tasks/7/done {"note":"good"}`},
		{func() error { _, err := DecideTask(ctx, f, TaskReopen, 7, ""); return err }, `POST /v1/admin/tasks/7/reopen {"note":""}`},
		{func() error { _, err := DecideTask(ctx, f, TaskReset, 7, ""); return err }, `POST /v1/admin/tasks/7/reset {"note":""}`},
	}
	for _, s := range steps {
		f.calls = nil
		if err := s.call(); err != nil {
			t.Fatalf("%s: %v", s.want, err)
		}
		if len(f.calls) != 1 || f.calls[0] != s.want {
			t.Errorf("calls = %q, want %q", f.calls, s.want)
		}
	}
	task, err := DecideTask(ctx, f, TaskDone, 7, "")
	if err != nil || task.ID != 7 || task.Status != envelope.TaskOpen {
		t.Fatalf("task = %+v, %v", task, err)
	}
}

func TestAddTaskResult(t *testing.T) {
	f := &taskRelay{answers: map[string]string{
		"POST /v1/tasks": `{"task":{"id":3,"status":"open"},"existing":true,"assign_error":"muse is a guest"}`,
	}}
	res, err := AddTask(t.Context(), f, false, TaskAdd{Title: "x"})
	if err != nil || res.Task.ID != 3 || !res.Existing || res.AssignError != "muse is a guest" {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

// A listing and a task's history are read to the end, a page at a time, and
// a relay that hands back the same page is not followed forever. A listing
// without the closed tasks returns the relay's counts of them, and one with
// them counts the ones it read.
func TestTasksReadEveryPage(t *testing.T) {
	ctx := t.Context()
	f := &taskRelay{answers: map[string]string{
		"GET /v1/tasks?after=0":          `{"tasks":[{"id":1},{"id":2}],"next_after":2,"closed":{"done":4,"dropped":1}}`,
		"GET /v1/tasks?after=2":          `{"tasks":[{"id":3}],"closed":{"done":5,"dropped":1}}`,
		"GET /v1/tasks?closed=1&after=0": `{"tasks":[{"id":1},{"id":2,"status":"done"}],"next_after":2}`,
		"GET /v1/tasks?closed=1&after=2": `{"tasks":[{"id":3,"status":"dropped"},{"id":4,"status":"done"}]}`,
		"GET /v1/tasks/9?after=0":        `{"task":{"id":9,"status":"working"},"events":[{"id":4},{"id":5}],"next_after":5}`,
		"GET /v1/tasks/9?after=5":        `{"task":{"id":9,"status":"returned"},"events":[{"id":6}]}`,
	}}
	list, counts, err := Tasks(ctx, f, false, false)
	if err != nil || len(list) != 3 || list[2].ID != 3 || counts != (TaskCounts{Done: 5, Dropped: 1}) {
		t.Fatalf("list = %+v, %+v, %v", list, counts, err)
	}
	list, counts, err = Tasks(ctx, f, false, true)
	if err != nil || len(list) != 4 || list[3].ID != 4 || counts != (TaskCounts{Done: 2, Dropped: 1}) {
		t.Fatalf("list with closed = %+v, %+v, %v", list, counts, err)
	}
	task, events, err := Task(ctx, f, false, 9)
	if err != nil || task.Status != envelope.TaskReturned || len(events) != 3 || events[2].ID != 6 {
		t.Fatalf("task = %+v, events = %+v, %v", task, events, err)
	}

	stuck := &taskRelay{answers: map[string]string{
		"GET /v1/tasks?after=0":   `{"tasks":[{"id":1}],"next_after":1}`,
		"GET /v1/tasks?after=1":   `{"tasks":[{"id":1}],"next_after":1}`,
		"GET /v1/tasks/9?after=0": `{"task":{"id":9},"events":[{"id":4}],"next_after":4}`,
		"GET /v1/tasks/9?after=4": `{"task":{"id":9},"events":[{"id":4}],"next_after":4}`,
	}}
	if _, _, err := Tasks(ctx, stuck, false, false); err == nil {
		t.Fatal("a repeated task page was followed")
	}
	if _, _, err := Task(ctx, stuck, false, 9); err == nil {
		t.Fatal("a repeated history page was followed")
	}
}

func TestFormatTasks(t *testing.T) {
	list := []envelope.Task{
		{ID: 1, Title: "book the venue", Status: envelope.TaskOpen, Filer: envelope.TaskOwner},
		{ID: 2, Title: "draft\nthe \x1b[2Kinvite", Status: envelope.TaskWorking, Filer: "grokbot", Assignee: "muse", AskStatus: envelope.StatusDelivered},
		{ID: 3, Title: "from mail", Status: envelope.TaskProposed, Filer: "grokbot", SuggestedAssignee: "muse", Source: "mail"},
		{ID: 5, Title: "call the bank", Status: envelope.TaskOpen, Filer: "grokbot", Assignee: envelope.TaskOwner},
		{ID: 6, Title: "check the backups", Status: envelope.TaskReturned, Filer: "grokbot", Assignee: "muse", RepeatSchedule: 3, RepeatEverySeconds: 86400, RepeatTo: "muse"},
	}
	closed := []envelope.Task{
		{ID: 4, Title: "old one", Status: envelope.TaskDone, Filer: "grokbot"},
		{ID: 7, Title: "gave up", Status: envelope.TaskDropped, Filer: "grokbot"},
	}
	out := FormatTasks(list, TaskCounts{Done: 1})
	for _, want := range []string{
		"Open (2)\n  #1 book the venue (filed by the owner)\n  #5 call the bank (filed by grokbot, the owner's own)\n",
		"Working (1)\n  #2 draft the [2Kinvite (filed by grokbot, with muse, ask delivered)\n",
		"approval (1)\n  #3 from mail (filed by grokbot, suggested for muse, source mail)\n",
		"Done (1)\n",
		"Returned (1)\n  #6 check the backups (filed by grokbot, with muse, repeats every 24h0m0s)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "old one") || strings.Contains(out, "\x1b") {
		t.Errorf("listing shows a closed task or a control rune: %q", out)
	}
	if strings.Index(out, "Proposed") > strings.Index(out, "Working") || strings.Index(out, "Working") > strings.Index(out, "Open") {
		t.Errorf("groups out of order: %q", out)
	}
	if strings.Contains(out, "Dropped") {
		t.Errorf("listing names a group with no task: %q", out)
	}
	if !strings.HasSuffix(out, "Open (2)\n  #1 book the venue (filed by the owner)\n  #5 call the bank (filed by grokbot, the owner's own)\nDone (1)\n") {
		t.Errorf("the count of done tasks is not the last line: %q", out)
	}
	if out := FormatTasks(append(list, closed...), TaskCounts{Done: 1, Dropped: 1}); !strings.HasSuffix(out, "Done (1)\n  #4 old one (filed by grokbot)\nDropped (1)\n  #7 gave up (filed by grokbot)\n") {
		t.Errorf("closed listing lacks the closed tasks: %q", out)
	}
	if got := FormatTasks(nil, TaskCounts{Dropped: 2}); got != "Dropped (2)\n" {
		t.Errorf("only closed tasks = %q", got)
	}
	if got := FormatTasks(nil, TaskCounts{}); got != "No tasks.\n" {
		t.Fatalf("empty = %q", got)
	}
}

func TestFormatTask(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	task := envelope.Task{ID: 2, Title: "draft\x07 the invite", Detail: "two lines\nof detail", Status: envelope.TaskReturned, Filer: envelope.TaskOwner,
		Assignee: "muse", Result: "here it is", ResultRequest: "req-1", AgentAssigns: 2, Key: "invite", CreatedAt: now}
	out := FormatTask(task, []envelope.TaskEvent{
		{ID: 1, Actor: envelope.TaskOwner, Kind: envelope.TaskEventAdded, Status: envelope.TaskOpen, At: now},
		{ID: 2, Actor: "grokbot", Kind: envelope.TaskEventComment, Body: "use the long list", At: now},
	}, now)
	for _, want := range []string{
		"Task #2: draft the invite\nStatus: returned\nFiled by: the owner at ", "Assignee: muse\n", "Key: invite\n", "Agent assigns: 2\n",
		"\nWhat is wanted:\ntwo lines\nof detail\n", "\nResult (request req-1):\nhere it is\n",
		" the owner added (open)\n", " grokbot comment: use the long list\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q lacks %q", out, want)
		}
	}
}

// A repeating task says how often it repeats, for whom, and on which
// schedule; a task that does not repeat says nothing of it.
func TestFormatTaskRepeat(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	task := envelope.Task{ID: 6, Title: "check the backups", Status: envelope.TaskOpen, Filer: "grokbot", CreatedAt: now, RepeatSchedule: 3, RepeatEverySeconds: 5400, RepeatTo: "muse"}
	if out := FormatTask(task, nil, now); !strings.Contains(out, "Repeats: every 1h30m0s for muse (schedule #3)\n") {
		t.Errorf("%q lacks the repeat", out)
	}
	if got, want := FormatTaskRepeat(task), " It repeats every 1h30m0s for muse until it is done or dropped; stop only the repeat with tincan unschedule 3 or cancel_schedule."; got != want {
		t.Errorf("FormatTaskRepeat = %q, want %q", got, want)
	}
	task.RepeatSchedule, task.RepeatEverySeconds, task.RepeatTo = 0, 0, ""
	if out := FormatTask(task, nil, now); strings.Contains(out, "Repeats") || FormatTaskRepeat(task) != "" {
		t.Errorf("a task with no repeat shows one: %q", out)
	}
}
