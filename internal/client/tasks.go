package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// MaxTaskNoticeReceipts bounds one batch of task notice acknowledgments.
const MaxTaskNoticeReceipts = 100

// TaskNoticesTold records a commitment to post the claimed task notices.
func TaskNoticesTold(ctx context.Context, c Rawer, ids []string) error {
	return c.Raw(ctx, http.MethodPost, "/v1/tasks/told", struct {
		NoticeIDs []string `json:"notice_ids"`
	}{NoticeIDs: ids}, nil)
}

// ErrTasksUnsupported identifies a relay that predates the shared task
// list. Nothing was sent.
var ErrTasksUnsupported = errors.New("this relay has no task list; upgrade the relay")

// requireTasks fails with ErrTasksUnsupported unless the relay advertises
// the task list. It runs before every task call, so an older relay answers
// with one clear error instead of its plain 404.
func requireTasks(ctx context.Context, c Rawer) error {
	_, err := taskCapabilities(ctx, c)
	return err
}

// taskCapabilities is requireTasks for a call that also needs to know what
// else the relay advertises.
func taskCapabilities(ctx context.Context, c Rawer) (Capabilities, error) {
	caps, err := capabilities(ctx, c)
	if err != nil {
		return Capabilities{}, fmt.Errorf("check relay capabilities: %w", err)
	}
	if !caps.Tasks {
		return Capabilities{}, ErrTasksUnsupported
	}
	return caps, nil
}

// TaskAdd is a task to file. Assignee is a teammate, envelope.TaskOwner for
// the owner's own, or empty. Key makes a second add with the same key from
// the same filer return the first task. Source names where the text came
// from when that is outside the team ("mail", "meeting"); only an agent
// declares one, and the task then waits for the owner as proposed.
// EverySeconds makes the task repeat: the relay assigns it to Assignee, a
// teammate, again that often, within the bounds of a scheduled ask's repeat.
type TaskAdd struct {
	envelope.TaskDue
	Due          string `json:"-"` // resolved in the relay's task zone before sending
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

// The owner's decisions on a task, for DecideTask. A task host makes
// TaskDone, TaskReopen and TaskDrop on the owner's word, with
// HostDecideTask.
const (
	TaskApprove = "approve"
	TaskDone    = "done"
	TaskReopen  = "reopen"
	TaskReset   = "reset"
	TaskDrop    = "drop"
	TaskDue     = "due"
)

// ErrTaskHostDecisionsUnsupported identifies a relay that predates a task
// host's decisions. Nothing was sent.
var ErrTaskHostDecisionsUnsupported = errors.New("this relay does not take a task host's decisions; upgrade the relay")

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
	caps, err := taskCapabilities(ctx, c)
	if err != nil {
		return TaskResult{}, err
	}
	if a.Due != "" {
		a.TaskDue, err = caps.parseTaskDue(a.Due)
		if err != nil {
			return TaskResult{}, err
		}
	} else if caps.TaskZone == "" && (a.DueDay != "" || a.DueAt != nil) {
		return TaskResult{}, errTaskDueUnsupported
	}
	var out TaskResult
	err = c.Raw(ctx, "POST", tasksPath(owner), a, &out)
	out.Task.DueZone = caps.TaskZone
	return out, err
}

// TaskCounts is how many done and dropped tasks a listing covers.
type TaskCounts struct {
	Done    int `json:"done"`
	Dropped int `json:"dropped"`
}

// Tasks lists the tasks this agent may read, or with owner every task
// (admin devices only), with overdue and due-today items first. Done and dropped tasks are listed
// only with closed; either way the counts say how many of each there are.
// Each detail and result is cut to its start; Task reads one whole.
func Tasks(ctx context.Context, c Rawer, owner, closed bool) ([]envelope.Task, TaskCounts, error) {
	caps, err := taskCapabilities(ctx, c)
	if err != nil {
		return nil, TaskCounts{}, err
	}
	list, counts, _, err := readTasks(ctx, c, tasksPath(owner)+"?", closed, caps.TaskZone)
	return list, counts, err
}

// TaskHostView is what the relay said of a read that asked for the task
// host view.
type TaskHostView struct {
	// Served is set when the relay answered with the host view: every task,
	// with the text of the ones the owner never approved withheld.
	Served bool
	// Refused is why this agent, a task host the owner named, got its
	// ordinary view instead; "" for an agent that is no host.
	Refused string
}

// hostParam asks a task read for the host view.
const hostParam = "host=1&"

// HostTasks is Tasks for an agent, asking for the task host view. The relay
// serves it only to a task host the owner named, and the returned view says
// whether it did; any other agent gets what Tasks returns. A relay that
// does not advertise task hosts is not asked.
func HostTasks(ctx context.Context, c Rawer, closed bool) ([]envelope.Task, TaskCounts, TaskHostView, error) {
	caps, err := taskCapabilities(ctx, c)
	if err != nil {
		return nil, TaskCounts{}, TaskHostView{}, err
	}
	path := tasksPath(false) + "?"
	if caps.TaskHosts {
		path += hostParam
	}
	return readTasks(ctx, c, path, closed, caps.TaskZone)
}

// readTasks reads every page of the listing at path, which ends with the
// parameters before after. The host view is reported served only when every
// page was.
func readTasks(ctx context.Context, c Rawer, path string, closed bool, zone string) ([]envelope.Task, TaskCounts, TaskHostView, error) {
	var list []envelope.Task
	var counts TaskCounts
	var after int64
	view := TaskHostView{Served: true}
	if closed {
		path = strings.Replace(path, "?", "?closed=1&", 1)
	}
	for {
		var out struct {
			Tasks       []envelope.Task `json:"tasks"`
			NextAfter   int64           `json:"next_after"`
			Closed      *TaskCounts     `json:"closed"`
			Host        bool            `json:"host,omitempty"`
			HostRefused string          `json:"host_refused,omitempty"`
		}
		if err := c.Raw(ctx, "GET", path+"after="+strconv.FormatInt(after, 10), nil, &out); err != nil {
			return nil, TaskCounts{}, TaskHostView{}, err
		}
		list = append(list, out.Tasks...)
		if out.Closed != nil {
			counts = *out.Closed
		}
		view.Served = view.Served && out.Host
		if out.HostRefused != "" {
			view.Refused = out.HostRefused
		}
		if out.NextAfter == 0 {
			break
		}
		if out.NextAfter <= after {
			return nil, TaskCounts{}, TaskHostView{}, errors.New("relay repeated a task page")
		}
		after = out.NextAfter
	}
	if closed {
		// The relay counts only what it leaves out.
		counts = TaskCounts{}
		for _, t := range list {
			switch t.Status {
			case envelope.TaskDone:
				counts.Done++
			case envelope.TaskDropped:
				counts.Dropped++
			}
		}
	}
	for i := range list {
		if list[i].DueDay != "" {
			list[i].DueZone = zone
		}
	}
	orderTasksByDue(list, time.Now())
	return list, counts, view, nil
}

// Task reads task id whole with its history, oldest first: one this agent
// may read, or with owner any (admin devices only).
func Task(ctx context.Context, c Rawer, owner bool, id int64) (envelope.Task, []envelope.TaskEvent, error) {
	caps, err := taskCapabilities(ctx, c)
	if err != nil {
		return envelope.Task{}, nil, err
	}
	return readTask(ctx, c, taskPath(owner, id, "")+"?", caps.TaskZone)
}

// HostTask is Task for an agent, asking for the task host view as HostTasks
// does. A task host is shown a task the owner never approved as a withheld
// row, with no history.
func HostTask(ctx context.Context, c Rawer, id int64) (envelope.Task, []envelope.TaskEvent, error) {
	caps, err := taskCapabilities(ctx, c)
	if err != nil {
		return envelope.Task{}, nil, err
	}
	path := taskPath(false, id, "") + "?"
	if caps.TaskHosts {
		path += hostParam
	}
	return readTask(ctx, c, path, caps.TaskZone)
}

// readTask reads the task at path, which ends with the parameters before
// after, and every page of its history.
func readTask(ctx context.Context, c Rawer, path, zone string) (envelope.Task, []envelope.TaskEvent, error) {
	var task envelope.Task
	var events []envelope.TaskEvent
	var after int64
	for {
		var out struct {
			Task      envelope.Task        `json:"task"`
			Events    []envelope.TaskEvent `json:"events"`
			NextAfter int64                `json:"next_after"`
		}
		if err := c.Raw(ctx, "GET", path+"after="+strconv.FormatInt(after, 10), nil, &out); err != nil {
			return envelope.Task{}, nil, err
		}
		task, events = out.Task, append(events, out.Events...)
		if out.NextAfter == 0 {
			if task.DueDay != "" {
				task.DueZone = zone
			}
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
// returned one, TaskReopen puts a done or dropped one back to open (or to
// proposed when it was never approved), and TaskReset sets its count of
// agent assigns back to zero.
func DecideTask(ctx context.Context, c Rawer, decision string, id int64, note string) (envelope.Task, error) {
	return taskCall(ctx, c, taskPath(true, id, "/"+decision), map[string]string{"note": note})
}

// HostDecideTask is a task host's decision on task id on the owner's word:
// TaskDone, TaskReopen or TaskDrop, with words, the owner's own words that
// asked for it, which the task's history keeps under this agent's name. The
// relay refuses an agent the owner has not named a task host, and refuses a
// host while it holds a claimed request or a guest's ask is open with it. A
// relay that does not advertise task host decisions is not called.
func HostDecideTask(ctx context.Context, c Rawer, id int64, decision, words string) (envelope.Task, error) {
	return hostDecideTask(ctx, c, id, decision, words, "")
}

// HostSetTaskDue changes a due date on the owner's words, which the relay records.
func HostSetTaskDue(ctx context.Context, c Rawer, id int64, when, words string) (envelope.Task, error) {
	return hostDecideTask(ctx, c, id, TaskDue, words, when)
}

func hostDecideTask(ctx context.Context, c Rawer, id int64, decision, words, when string) (envelope.Task, error) {
	caps, err := taskCapabilities(ctx, c)
	if err != nil {
		return envelope.Task{}, err
	}
	if !caps.TaskHostDecisions {
		return envelope.Task{}, ErrTaskHostDecisionsUnsupported
	}
	in := struct {
		Decision string `json:"decision"`
		Words    string `json:"owner_words"`
		envelope.TaskDue
	}{Decision: decision, Words: words}
	if decision == TaskDue {
		in.TaskDue, err = caps.parseTaskDue(when)
		if err != nil {
			return envelope.Task{}, err
		}
	}
	var out TaskResult
	err = c.Raw(ctx, "POST", taskPath(false, id, "/decide"), in, &out)
	out.Task.DueZone = caps.TaskZone
	return out.Task, err
}

// FormatHostDecision is the answer to a task host's decision: where the
// task stands now and how to undo it, for the host to tell the owner.
func FormatHostDecision(t envelope.Task, decision string) string {
	undo := "reopen it the same way"
	if decision == TaskDue {
		return FormatTaskDueChange(t)
	}
	if decision == TaskReopen {
		undo = "mark it done or drop it the same way"
	}
	return fmt.Sprintf("Task #%d (%s) is now %s, done for the owner under your name with the owner's words in its history. Tell the owner what changed; if they did not mean it, %s.", t.ID, taskTitle(t), t.Status, undo)
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

// FormatTasks renders a task list for tincan tasks, grouped by status.
// Done and dropped tasks are counted, from counts, and listed only when
// list holds them.
func FormatTasks(list []envelope.Task, counts TaskCounts) string {
	if len(list) == 0 && counts == (TaskCounts{}) {
		return "No tasks.\n"
	}
	now := time.Now()
	// Put time-sensitive tasks above the usual status groups.
	var b strings.Builder
	var priority [2][]string
	remaining := make([]envelope.Task, 0, len(list))
	for _, t := range list {
		if rank := taskDueRank(t, now); rank < len(priority) {
			priority[rank] = append(priority[rank], fmt.Sprintf("  %s [%s]\n", taskLine(t), t.Status))
		} else {
			remaining = append(remaining, t)
		}
	}
	for i, label := range []string{"Overdue", "Due today"} {
		if len(priority[i]) > 0 {
			fmt.Fprintf(&b, "%s (%d)\n", label, len(priority[i]))
			b.WriteString(strings.Join(priority[i], ""))
		}
	}
	for _, g := range taskGroups {
		var lines []string
		for _, t := range remaining {
			if t.Status == g.status {
				lines = append(lines, "  "+taskLine(t)+"\n")
			}
		}
		n := len(lines)
		switch g.status {
		case envelope.TaskDone:
			n = counts.Done
		case envelope.TaskDropped:
			n = counts.Dropped
		}
		if n == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s (%d)\n", g.title, n)
		b.WriteString(strings.Join(lines, ""))
	}
	return b.String()
}

// FormatHostTasks renders the answer to HostTasks. A list served with the
// host view opens by saying the reader is the owner's task host; its groups
// are FormatTasks's, which lead with what waits on the owner. A task host
// that got its ordinary view is told why. Any other list is FormatTasks's.
func FormatHostTasks(list []envelope.Task, counts TaskCounts, view TaskHostView) string {
	head := ""
	switch {
	case view.Served:
		head = "You are the owner's task host: this is every task on the list, with what waits on the owner first.\n"
	case view.Refused != "":
		head = "This is your ordinary view, not the task host view: " + ScheduleText(view.Refused) + ".\n"
	}
	return head + FormatTasks(list, counts)
}

// taskWithheld stands where the title of a withheld task would be.
const taskWithheld = "text withheld until the owner approves it"

// taskTitle is a task's title on one line, or for a withheld row the words
// that say why it has none.
func taskTitle(t envelope.Task) string {
	if t.Withheld {
		return taskWithheld
	}
	return ScheduleText(t.Title)
}

// taskLine is one task on one line of a listing.
func taskLine(t envelope.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s (filed by %s", t.ID, taskTitle(t), envelope.TaskWho(t.Filer))
	switch {
	case t.Assignee == envelope.TaskOwner:
		b.WriteString(", the owner's own")
	case t.Assignee != "":
		fmt.Fprintf(&b, ", with %s", t.Assignee)
	case t.SuggestedAssignee != "":
		fmt.Fprintf(&b, ", suggested for %s", envelope.TaskWho(t.SuggestedAssignee))
	}
	if t.AskStatus != "" {
		fmt.Fprintf(&b, ", ask %s", t.AskStatus)
	}
	if t.Source != "" {
		fmt.Fprintf(&b, ", source %s", t.Source)
	}
	if t.RepeatEverySeconds != 0 {
		fmt.Fprintf(&b, ", repeats %s", everyText(t.RepeatEverySeconds))
	}
	if due := FormatTaskDue(t); due != "" {
		fmt.Fprintf(&b, ", due %s", due)
	}
	b.WriteString(")")
	return b.String()
}

// everyText is how a listing says a repeat of seconds.
func everyText(seconds int64) string {
	return fmt.Sprintf("every %v", time.Duration(seconds)*time.Second)
}

// TaskEverySeconds reads every, the repeat an add of a task names, as the
// add's EverySeconds. A repeating task goes to a teammate: with no to it
// returns noTo, the caller's own wording.
func TaskEverySeconds(every, to string, now time.Time, noTo error) (int64, error) {
	if to == "" {
		return 0, noTo
	}
	_, interval, err := ParseWhen("", every, now)
	if err != nil {
		return 0, err
	}
	return int64(interval / time.Second), nil
}

// FormatTaskHeld is the sentence that follows the news of an assignment
// whose ask the relay holds for the owner, "" for any other: the task
// reads as working, and its assignee has not been sent it yet.
func FormatTaskHeld(t envelope.Task) string {
	if t.AskStatus != envelope.StatusHeld {
		return ""
	}
	return fmt.Sprintf(" The ask is held for the owner's approval: %s gets the task only once the owner approves it.", t.Assignee)
}

// FormatTaskRepeat is the sentence tincan tasks add and the add_task tool
// end with for a task that repeats, and "" for one that does not.
func FormatTaskRepeat(t envelope.Task) string {
	if t.RepeatEverySeconds == 0 {
		return ""
	}
	return fmt.Sprintf(" It repeats %s for %s until it is done or dropped; stop only the repeat with tincan unschedule %d or cancel_schedule.", everyText(t.RepeatEverySeconds), t.RepeatTo, t.RepeatSchedule)
}

// FormatTask renders one task whole with its history for tincan tasks show.
// The title is put on one line; the detail, result and comments keep their
// own lines.
func FormatTask(t envelope.Task, events []envelope.TaskEvent, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task #%d: %s\nStatus: %s\nFiled by: %s at %s\n", t.ID, taskTitle(t), t.Status, envelope.TaskWho(t.Filer), clockTime(t.CreatedAt, now))
	if due := FormatTaskDue(t); due != "" {
		label := "Due"
		if taskDueRank(t, now) == 0 {
			label = "Overdue"
		}
		fmt.Fprintf(&b, "%s: %s\n", label, due)
	}
	if t.Assignee != "" {
		fmt.Fprintf(&b, "Assignee: %s\n", envelope.TaskWho(t.Assignee))
	}
	if t.SuggestedAssignee != "" {
		fmt.Fprintf(&b, "Suggested assignee: %s\n", envelope.TaskWho(t.SuggestedAssignee))
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
		fmt.Fprintf(&b, "Repeats: %s for %s (schedule #%d)\n", everyText(t.RepeatEverySeconds), t.RepeatTo, t.RepeatSchedule)
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
		fmt.Fprintf(&b, "%s %s %s", clockTime(ev.At, now), envelope.TaskWho(ev.Actor), ev.Kind)
		if ev.Status != "" {
			fmt.Fprintf(&b, " (%s)", ev.Status)
		}
		if ev.ForOwner {
			b.WriteString(" for the owner, who said")
		}
		if ev.Body != "" {
			fmt.Fprintf(&b, ": %s", ev.Body)
		}
		b.WriteString("\n")
	}
	return b.String()
}
