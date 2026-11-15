package client

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// ErrTasksUnsupported identifies a relay that predates the shared task
// list. Nothing was sent.
var ErrTasksUnsupported = errors.New("this relay has no task list; upgrade the relay")

// requireTasks fails with ErrTasksUnsupported unless the relay advertises
// the task list. It runs before every task call, so an older relay answers
// with one clear error instead of its plain 404.
func requireTasks(ctx context.Context, c Rawer) error {
	caps, err := capabilities(ctx, c)
	if err != nil {
		return fmt.Errorf("check relay capabilities: %w", err)
	}
	if !caps.Tasks {
		return ErrTasksUnsupported
	}
	return nil
}

// TaskAdd is a task to file. Assignee is a teammate, envelope.TaskOwner for
// the owner's own, or empty. Key makes a second add with the same key from
// the same filer return the first task. Source names where the text came
// from when that is outside the team ("mail", "meeting"); only an agent
// declares one, and the task then waits for the owner as proposed.
// EverySeconds makes the task repeat: the relay assigns it to Assignee, a
// teammate, again that often, within the bounds of a scheduled ask's repeat.
type TaskAdd struct {
	Title        string `json:"title"`
	Detail       string `json:"detail,omitempty"`
	Assignee     string `json:"assignee,omitempty"`
	Key          string `json:"key,omitempty"`
	Source       string `json:"source,omitempty"`
	EverySeconds int64  `json:"every_seconds,omitempty"`
}

// TaskResult is what the relay answers a task call with: the task after the
// call. Event is the comment a comment stored. Existing marks an add that
// stored nothing because the filer already has a live task with the key.
// AssignError is why an add that named a teammate stored the task and sent
// no ask; the task is open and unassigned.
type TaskResult struct {
	Task        envelope.Task       `json:"task"`
	Event       *envelope.TaskEvent `json:"event,omitempty"`
	Existing    bool                `json:"existing,omitempty"`
	AssignError string              `json:"assign_error,omitempty"`
}

// The owner's decisions on a task, for DecideTask.
const (
	TaskApprove = "approve"
	TaskDone    = "done"
	TaskReopen  = "reopen"
	TaskReset   = "reset"
)

// tasksPath is the task routes' root: the calling agent's, or with owner
// the admin routes, which only an admin device may call.
func tasksPath(owner bool) string {
	if owner {
		return "/v1/admin/tasks"
	}
	return "/v1/tasks"
}

// taskPath is the route of task id, with an action such as "/assign".
func taskPath(owner bool, id int64, action string) string {
	return tasksPath(owner) + "/" + strconv.FormatInt(id, 10) + action
}

// AddTask files a as this agent's task, or with owner as the owner's (admin
// devices only). The owner's task that names a teammate is sent as the
// agent c names, so c must name one.
func AddTask(ctx context.Context, c Rawer, owner bool, a TaskAdd) (TaskResult, error) {
	if err := requireTasks(ctx, c); err != nil {
		return TaskResult{}, err
	}
	var out TaskResult
	err := c.Raw(ctx, "POST", tasksPath(owner), a, &out)
	return out, err
}

// Tasks lists the tasks this agent may read, or with owner every task
// (admin devices only), oldest first. Each detail and result is cut to its
// start; Task reads one whole.
func Tasks(ctx context.Context, c Rawer, owner bool) ([]envelope.Task, error) {
	if err := requireTasks(ctx, c); err != nil {
		return nil, err
	}
	var list []envelope.Task
	var after int64
	for {
		var out struct {
			Tasks     []envelope.Task `json:"tasks"`
			NextAfter int64           `json:"next_after"`
		}
		if err := c.Raw(ctx, "GET", tasksPath(owner)+"?after="+strconv.FormatInt(after, 10), nil, &out); err != nil {
			return nil, err
		}
		list = append(list, out.Tasks...)
		if out.NextAfter == 0 {
			return list, nil
		}
		if out.NextAfter <= after {
			return nil, errors.New("relay repeated a task page")
		}
		after = out.NextAfter
	}
}

// Task reads task id whole with its history, oldest first: one this agent
// may read, or with owner any (admin devices only).
func Task(ctx context.Context, c Rawer, owner bool, id int64) (envelope.Task, []envelope.TaskEvent, error) {
	if err := requireTasks(ctx, c); err != nil {
		return envelope.Task{}, nil, err
	}
	var task envelope.Task
	var events []envelope.TaskEvent
	var after int64
	for {
		var out struct {
			Task      envelope.Task        `json:"task"`
			Events    []envelope.TaskEvent `json:"events"`
			NextAfter int64                `json:"next_after"`
		}
		if err := c.Raw(ctx, "GET", taskPath(owner, id, "")+"?after="+strconv.FormatInt(after, 10), nil, &out); err != nil {
			return envelope.Task{}, nil, err
		}
		task, events = out.Task, append(events, out.Events...)
		if out.NextAfter == 0 {
			return task, events, nil
		}
		if out.NextAfter <= after {
			return envelope.Task{}, nil, errors.New("relay repeated a task history page")
		}
		after = out.NextAfter
	}
}

// AssignTask gives task id to teammate to, as an ask from this agent. With
// owner it is the owner's assign (admin devices only): any task, whoever
// has it, sent as the agent c names.
func AssignTask(ctx context.Context, c Rawer, owner bool, id int64, to string) (envelope.Task, error) {
	return taskCall(ctx, c, taskPath(owner, id, "/assign"), map[string]string{"to": to})
}

// CommentTask adds a comment to task id as this agent, or with owner as the
// owner (admin devices only). Nobody is woken for it: the next ask under
// the task carries it.
func CommentTask(ctx context.Context, c Rawer, owner bool, id int64, body string) (TaskResult, error) {
	if err := requireTasks(ctx, c); err != nil {
		return TaskResult{}, err
	}
	var out TaskResult
	err := c.Raw(ctx, "POST", taskPath(owner, id, "/comment"), map[string]string{"body": body}, &out)
	return out, err
}

// DropTask gives task id up. An agent drops only a task it filed with no
// ask open under it; with owner (admin devices only) any task is dropped
// and an ask open under it cancelled.
func DropTask(ctx context.Context, c Rawer, owner bool, id int64, note string) (envelope.Task, error) {
	return taskCall(ctx, c, taskPath(owner, id, "/drop"), map[string]string{"note": note})
}

// DecideTask runs one of the owner's decisions on task id (admin devices
// only): TaskApprove opens a proposed task, TaskDone closes an open or
// returned one, TaskReopen puts a done or dropped one back to open, and
// TaskReset sets its count of agent assigns back to zero.
func DecideTask(ctx context.Context, c Rawer, decision string, id int64, note string) (envelope.Task, error) {
	return taskCall(ctx, c, taskPath(true, id, "/"+decision), map[string]string{"note": note})
}

// taskCall posts in to a task route that answers with the task.
func taskCall(ctx context.Context, c Rawer, path string, in any) (envelope.Task, error) {
	if err := requireTasks(ctx, c); err != nil {
		return envelope.Task{}, err
	}
	var out TaskResult
	err := c.Raw(ctx, "POST", path, in, &out)
	return out.Task, err
}

// taskGroups is the order a task listing shows the statuses in: what waits
// on the owner first, closed tasks last.
var taskGroups = []struct {
	status envelope.TaskStatus
	title  string
}{
	{envelope.TaskProposed, "Proposed, waiting for the owner's approval"},
	{envelope.TaskQuestion, "Question, waiting for an answer"},
	{envelope.TaskReturned, "Returned"},
	{envelope.TaskWorking, "Working"},
	{envelope.TaskOpen, "Open"},
	{envelope.TaskDone, "Done"},
	{envelope.TaskDropped, "Dropped"},
}

// taskWho is how a listing names a task's filer or assignee.
func taskWho(name string) string {
	if name == envelope.TaskOwner {
		return "the owner"
	}
	return name
}

// FormatTasks renders a task list for tincan tasks, grouped by status.
// Done and dropped tasks are counted, and listed only with closed.
func FormatTasks(list []envelope.Task, closed bool) string {
	if len(list) == 0 {
		return "No tasks.\n"
	}
	var b strings.Builder
	for _, g := range taskGroups {
		var lines []string
		for _, t := range list {
			if t.Status == g.status {
				lines = append(lines, "  "+taskLine(t)+"\n")
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s (%d)\n", g.title, len(lines))
		if !closed && (g.status == envelope.TaskDone || g.status == envelope.TaskDropped) {
			continue
		}
		b.WriteString(strings.Join(lines, ""))
	}
	return b.String()
}

// taskLine is one task on one line of a listing.
func taskLine(t envelope.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s (filed by %s", t.ID, ScheduleText(t.Title), taskWho(t.Filer))
	switch {
	case t.Assignee == envelope.TaskOwner:
		b.WriteString(", the owner's own")
	case t.Assignee != "":
		fmt.Fprintf(&b, ", with %s", t.Assignee)
	case t.SuggestedAssignee != "":
		fmt.Fprintf(&b, ", suggested for %s", taskWho(t.SuggestedAssignee))
	}
	if t.AskStatus != "" {
		fmt.Fprintf(&b, ", ask %s", t.AskStatus)
	}
	if t.Source != "" {
		fmt.Fprintf(&b, ", source %s", t.Source)
	}
	if t.RepeatEverySeconds != 0 {
		fmt.Fprintf(&b, ", repeats %s", taskEvery(t))
	}
	b.WriteString(")")
	return b.String()
}

// taskEvery is how often a repeating task repeats, as a schedule listing
// says it.
func taskEvery(t envelope.Task) string {
	return fmt.Sprintf("every %v", time.Duration(t.RepeatEverySeconds)*time.Second)
}

// FormatTaskRepeat is the sentence tincan tasks add and the add_task tool
// end with for a task that repeats, and "" for one that does not.
func FormatTaskRepeat(t envelope.Task) string {
	if t.RepeatEverySeconds == 0 {
		return ""
	}
	return fmt.Sprintf(" It repeats %s for %s until it is done or dropped; stop only the repeat with tincan unschedule %d or cancel_schedule.", taskEvery(t), t.RepeatTo, t.RepeatSchedule)
}

// FormatTask renders one task whole with its history for tincan tasks show.
// The title is put on one line; the detail, result and comments keep their
// own lines.
func FormatTask(t envelope.Task, events []envelope.TaskEvent, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task #%d: %s\nStatus: %s\nFiled by: %s at %s\n", t.ID, ScheduleText(t.Title), t.Status, taskWho(t.Filer), clockTime(t.CreatedAt, now))
	if t.Assignee != "" {
		fmt.Fprintf(&b, "Assignee: %s\n", taskWho(t.Assignee))
	}
	if t.SuggestedAssignee != "" {
		fmt.Fprintf(&b, "Suggested assignee: %s\n", taskWho(t.SuggestedAssignee))
	}
	if t.CurrentRequest != "" {
		fmt.Fprintf(&b, "Open ask: %s %s\n", t.CurrentRequest, t.AskStatus)
	}
	if t.Source != "" {
		fmt.Fprintf(&b, "Source: %s\n", t.Source)
	}
	if t.Key != "" {
		fmt.Fprintf(&b, "Key: %s\n", ScheduleText(t.Key))
	}
	if t.AgentAssigns > 0 {
		fmt.Fprintf(&b, "Agent assigns: %d\n", t.AgentAssigns)
	}
	if t.RepeatEverySeconds != 0 {
		fmt.Fprintf(&b, "Repeats: %s for %s (schedule #%d)\n", taskEvery(t), t.RepeatTo, t.RepeatSchedule)
	}
	if t.Detail != "" {
		fmt.Fprintf(&b, "\nWhat is wanted:\n%s\n", t.Detail)
	}
	if t.Result != "" {
		fmt.Fprintf(&b, "\nResult (request %s):\n%s\n", t.ResultRequest, t.Result)
	}
	if len(events) > 0 {
		b.WriteString("\nHistory:\n")
	}
	for _, ev := range events {
		fmt.Fprintf(&b, "%s %s %s", clockTime(ev.At, now), taskWho(ev.Actor), ev.Kind)
		if ev.Status != "" {
			fmt.Fprintf(&b, " (%s)", ev.Status)
		}
		if ev.Body != "" {
			fmt.Fprintf(&b, ": %s", ev.Body)
		}
		b.WriteString("\n")
	}
	return b.String()
}
