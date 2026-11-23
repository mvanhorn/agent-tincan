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
	// hosts makes it advertise task hosts as well, and decisions their
	// decisions.
	hosts, decisions bool
}

func (f *taskRelay) Raw(_ context.Context, method, path string, in, out any) error {
	if path == "/v1/capabilities" {
		*(out.(*Capabilities)) = Capabilities{Tasks: true, TaskHosts: f.hosts, TaskHostDecisions: f.decisions}
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
		"host":    func() error { _, err := HostDecideTask(ctx, old, 1, TaskDone, "x"); return err },
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

func TestFormatTaskHeld(t *testing.T) {
	held := envelope.Task{ID: 3, Assignee: "muse", AskStatus: envelope.StatusHeld}
	if got := FormatTaskHeld(held); !strings.Contains(got, "held for the owner's approval") || !strings.Contains(got, "muse gets the task only once") {
		t.Errorf("held = %q", got)
	}
	held.AskStatus = envelope.StatusQueued
	if got := FormatTaskHeld(held); got != "" {
		t.Errorf("queued = %q, want nothing", got)
	}
}

// HostTasks and HostTask ask for the host view only of a relay that
// advertises task hosts, on every page, and report what the relay says it
// served. Against a relay that does not, they make the calls Tasks and Task
// make and report no host view.
func TestHostReadsAskOnlyARelayWithTaskHosts(t *testing.T) {
	ctx := t.Context()
	f := &taskRelay{hosts: true, answers: map[string]string{
		"GET /v1/tasks?host=1&after=0":          `{"host":true,"tasks":[{"id":1},{"id":2,"withheld":true}],"next_after":2,"closed":{"done":1,"dropped":0}}`,
		"GET /v1/tasks?host=1&after=2":          `{"host":true,"tasks":[{"id":3}],"closed":{"done":1,"dropped":0}}`,
		"GET /v1/tasks?closed=1&host=1&after=0": `{"host":false,"host_refused":"a guest's ask is open with you","tasks":[{"id":1}]}`,
		"GET /v1/tasks/9?host=1&after=0":        `{"host":true,"task":{"id":9,"status":"dropped","withheld":true},"events":[]}`,
	}}
	list, counts, view, err := HostTasks(ctx, f, false)
	if err != nil || len(list) != 3 || !list[1].Withheld || counts != (TaskCounts{Done: 1}) || view != (TaskHostView{Served: true}) {
		t.Fatalf("host list = %+v, %+v, %+v, %v", list, counts, view, err)
	}
	list, _, view, err = HostTasks(ctx, f, true)
	if err != nil || len(list) != 1 || view != (TaskHostView{Refused: "a guest's ask is open with you"}) {
		t.Fatalf("host list while refused = %+v, %+v, %v", list, view, err)
	}
	task, events, err := HostTask(ctx, f, 9)
	if err != nil || !task.Withheld || len(events) != 0 {
		t.Fatalf("host read = %+v, %+v, %v", task, events, err)
	}
	if want := []string{"GET /v1/tasks?host=1&after=0", "GET /v1/tasks?host=1&after=2", "GET /v1/tasks?closed=1&host=1&after=0", "GET /v1/tasks/9?host=1&after=0"}; strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
	// Tasks and Task never ask, whatever the relay advertises.
	f.calls = nil
	f.answers["GET /v1/tasks?after=0"] = `{"tasks":[]}`
	if _, _, err := Tasks(ctx, f, false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Task(ctx, f, false, 7); err != nil {
		t.Fatal(err)
	}
	if want := "GET /v1/tasks?after=0\nGET /v1/tasks/7?after=0"; strings.Join(f.calls, "\n") != want {
		t.Fatalf("calls of the plain reads = %q", f.calls)
	}

	old := &taskRelay{answers: map[string]string{
		"GET /v1/tasks?after=0":   `{"tasks":[{"id":1}],"closed":{"done":2,"dropped":0}}`,
		"GET /v1/tasks/9?after=0": `{"task":{"id":9,"status":"open"},"events":[{"id":4}]}`,
	}}
	list, counts, view, err = HostTasks(ctx, old, false)
	if err != nil || len(list) != 1 || counts != (TaskCounts{Done: 2}) || view != (TaskHostView{}) {
		t.Fatalf("list from a relay without task hosts = %+v, %+v, %+v, %v", list, counts, view, err)
	}
	if task, events, err := HostTask(ctx, old, 9); err != nil || task.ID != 9 || len(events) != 1 {
		t.Fatalf("read from a relay without task hosts = %+v, %+v, %v", task, events, err)
	}
	if want := "GET /v1/tasks?after=0\nGET /v1/tasks/9?after=0"; strings.Join(old.calls, "\n") != want {
		t.Fatalf("calls to a relay without task hosts = %q", old.calls)
	}
	if _, _, _, err := HostTasks(ctx, &oldRelay{}, false); !errors.Is(err, ErrTasksUnsupported) {
		t.Fatalf("host list from a relay with no task list: %v", err)
	}
	if _, _, err := HostTask(ctx, &oldRelay{}, 1); !errors.Is(err, ErrTasksUnsupported) {
		t.Fatalf("host read from a relay with no task list: %v", err)
	}
}

// A list served with the host view opens by saying so, leads with what
// waits on the owner (proposed, then question, then returned) and prints a
// withheld row with its id, filer and source in place of text. A named
// host that did not get the host view is told why; any other list reads as
// FormatTasks prints it.
func TestFormatHostTasks(t *testing.T) {
	list := []envelope.Task{
		{ID: 1, Title: "book the venue", Status: envelope.TaskOpen, Filer: envelope.TaskOwner},
		{ID: 2, Title: "draft the invite", Status: envelope.TaskWorking, Filer: "grokbot", Assignee: "muse"},
		{ID: 3, Title: "check the backups", Status: envelope.TaskReturned, Filer: "grokbot", Assignee: "muse"},
		{ID: 4, Title: "which venue?", Status: envelope.TaskQuestion, Filer: "grokbot", Assignee: "instinct"},
		{ID: 5, Status: envelope.TaskProposed, Filer: "grokbot", Source: "mail", Withheld: true},
		{ID: 6, Title: "the host's own", Status: envelope.TaskProposed, Filer: "muse", Source: "meeting"},
		{ID: 7, Status: envelope.TaskDropped, Filer: "instinct", Source: "mail", Withheld: true},
	}
	out := FormatHostTasks(list, TaskCounts{Dropped: 1}, TaskHostView{Served: true})
	if !strings.HasPrefix(out, "You are the owner's task host") {
		t.Errorf("host list does not open by saying so: %q", out)
	}
	order := []string{"You are the owner's task host", "Proposed, waiting for the owner's approval (2)", "Question, waiting for an answer (1)", "Returned (1)", "Working (1)", "Open (1)", "Dropped (1)"}
	at := -1
	for _, part := range order {
		i := strings.Index(out, part)
		if i <= at {
			t.Fatalf("%q is missing or out of order in %q", part, out)
		}
		at = i
	}
	for _, want := range []string{
		"  #5 text withheld until the owner approves it (filed by grokbot, source mail)\n",
		"  #6 the host's own (filed by muse, source meeting)\n",
		"Dropped (1)\n  #7 text withheld until the owner approves it (filed by instinct, source mail)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q lacks %q", out, want)
		}
	}

	plain := FormatTasks(list[:4], TaskCounts{})
	if got := FormatHostTasks(list[:4], TaskCounts{}, TaskHostView{}); got != plain {
		t.Errorf("a list not served with the host view = %q, want %q", got, plain)
	}
	refused := FormatHostTasks(list[:4], TaskCounts{}, TaskHostView{Refused: "a guest's ask is open with you"})
	if !strings.HasSuffix(refused, plain) || !strings.Contains(strings.TrimSuffix(refused, plain), "a guest's ask is open with you") || strings.Contains(refused, "You are the owner's task host") {
		t.Errorf("a refused host's list = %q", refused)
	}
	if got := FormatHostTasks(nil, TaskCounts{}, TaskHostView{Served: true}); !strings.HasPrefix(got, "You are the owner's task host") || !strings.HasSuffix(got, "No tasks.\n") {
		t.Errorf("an empty host list = %q", got)
	}
}

// A withheld task read alone says its text is withheld, with its filer and
// source.
func TestFormatTaskWithheld(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	out := FormatTask(envelope.Task{ID: 5, Status: envelope.TaskDropped, Filer: "grokbot", Source: "mail", Withheld: true, CreatedAt: now}, nil, now)
	for _, want := range []string{"Task #5: text withheld until the owner approves it\n", "Status: dropped\n", "Filed by: grokbot at ", "Source: mail\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q lacks %q", out, want)
		}
	}
}

// A host decision posts the decision and the owner's words to the decide
// route, and only to a relay that advertises host decisions: an older one
// is answered with the upgrade message and sent nothing.
func TestHostDecideTask(t *testing.T) {
	ctx := t.Context()
	older := &taskRelay{hosts: true}
	if _, err := HostDecideTask(ctx, older, 7, TaskDone, "done"); !errors.Is(err, ErrTaskHostDecisionsUnsupported) || len(older.calls) != 0 {
		t.Fatalf("against an older relay: %v, calls %v", err, older.calls)
	}
	f := &taskRelay{hosts: true, decisions: true, answers: map[string]string{"POST /v1/tasks/7/decide": `{"task":{"id":7,"title":"sweep","status":"done"}}`}}
	task, err := HostDecideTask(ctx, f, 7, TaskDrop, "drop the sweep")
	if err != nil || task.Status != envelope.TaskDone {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if want := `POST /v1/tasks/7/decide {"decision":"drop","owner_words":"drop the sweep"}`; len(f.calls) != 1 || f.calls[0] != want {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
}

// The answer to a host decision says where the task is and how to undo it,
// and a history entry made for the owner says so before the words.
func TestFormatHostDecision(t *testing.T) {
	task := envelope.Task{ID: 3, Title: "sweep\x07 the porch", Status: envelope.TaskDone}
	if got := FormatHostDecision(task, TaskDone); !strings.HasPrefix(got, "Task #3 (sweep the porch) is now done, done for the owner") || !strings.HasSuffix(got, "reopen it the same way.") {
		t.Errorf("done = %q", got)
	}
	task.Status = envelope.TaskReturned
	if got := FormatHostDecision(task, TaskReopen); !strings.Contains(got, "is now returned") || !strings.HasSuffix(got, "mark it done or drop it the same way.") {
		t.Errorf("reopen = %q", got)
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	out := FormatTask(task, []envelope.TaskEvent{{ID: 1, Actor: "grokbot", Kind: envelope.TaskEventStatus, Status: envelope.TaskDone, ForOwner: true, Prior: envelope.TaskReturned, Body: "the porch is swept", At: now}}, now)
	if !strings.Contains(out, " grokbot status (done) for the owner, who said: the porch is swept\n") {
		t.Errorf("history = %q", out)
	}
}

func TestParseTaskDueInOwnerZone(t *testing.T) {
	t.Setenv("TZ", "UTC")
	now := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC) // Friday evening in Pacific time.
	tests := []struct {
		phrase, day string
		at          string
	}{
		{"friday", "2026-11-20", ""},
		{"friday 3pm", "2026-11-27", "2026-11-27T23:00:00Z"},
		{"tomorrow", "2026-11-21", ""},
		{"in 2h", "2026-11-20", "2026-11-21T04:00:00Z"},
		{"2026-11-21T01:30:00Z", "2026-11-20", "2026-11-21T01:30:00Z"},
		{"clear", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.phrase, func(t *testing.T) {
			due, err := ParseTaskDue(tc.phrase, "America/Los_Angeles", now)
			if err != nil || due.DueDay != tc.day {
				t.Fatalf("due = %+v, %v", due, err)
			}
			got := ""
			if due.DueAt != nil {
				got = time.Unix(*due.DueAt, 0).UTC().Format(time.RFC3339)
			}
			if got != tc.at {
				t.Fatalf("due time = %q, want %q", got, tc.at)
			}
		})
	}
}
