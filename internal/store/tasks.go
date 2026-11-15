package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// MaxOpenTasks caps the tasks one agent filed that are not done or dropped.
const MaxOpenTasks = 50

// MaxAgentAssigns caps how many times agents may assign one task before the
// owner resets its count.
const MaxAgentAssigns = 5

var (
	// ErrNoTask means there is no such task, or none the caller may see.
	ErrNoTask = errors.New("no such task")
	// ErrTaskLimit is AddTask's answer when the filer already has
	// MaxOpenTasks tasks that are not done or dropped.
	ErrTaskLimit = errors.New("too many open tasks; finish or drop one first")
	// ErrTaskBusy means an ask could not be sent under a task: another ask
	// is open under it, or its status does not allow an attempt.
	ErrTaskBusy = errors.New("the task has an ask open under it or is not open to one")
	// ErrTaskState means a task was not in a status the change allows.
	ErrTaskState = errors.New("the task is not in a status that allows this")
	// ErrTaskAskOpen refuses marking a task done while an ask is open under
	// it.
	ErrTaskAskOpen = errors.New("an ask is still open under the task; drop the task or wait for the ask to end")
	// ErrTaskAssignLimit refuses an agent's assign of a task agents have
	// already assigned MaxAgentAssigns times since the owner's last reset.
	ErrTaskAssignLimit = fmt.Errorf("agents have assigned this task %d times; the owner must assign it or reset the count", MaxAgentAssigns)
)

// migrateTasks creates the tasks, task_events and task_claimants tables and
// the requests.task_id link. Tasks name agents with no foreign key to
// agents, since a rebind re-inserts the agent row. A task is never deleted,
// so its id only ever names one task. current_request is the one ask open
// under the task, empty for none; result_request is the ask that produced
// result. task_events is the task's history, read in id order, which is
// also time order. task_claimants holds every agent that claimed, answered
// or assigned an ask under the task, which is what lets it keep reading
// the task afterwards; the target of an ask that ended while held for the
// owner is taken out again. source is the outside source the filer declared for
// the task's text, and suggested_assignee whom the filer asked to have a
// proposed task go to; NULL for none. tasks_live is what a listing without
// the closed tasks walks, so it costs what the live tasks do however many
// are done or dropped, and tasks_status is what counts those.
func (s *Store) migrateTasks() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
  id                 INTEGER PRIMARY KEY,
  title              TEXT NOT NULL,
  detail             TEXT NOT NULL DEFAULT '',
  status             TEXT NOT NULL,
  filer              TEXT NOT NULL,
  assignee           TEXT NOT NULL DEFAULT '',
  dedupe_key         TEXT NOT NULL DEFAULT '',
  result             TEXT NOT NULL DEFAULT '',
  result_request     TEXT NOT NULL DEFAULT '',
  current_request    TEXT NOT NULL DEFAULT '',
  agent_assigns      INTEGER NOT NULL DEFAULT 0,
  created_at         INTEGER NOT NULL,
  source             TEXT,
  suggested_assignee TEXT
);
CREATE INDEX IF NOT EXISTS tasks_filer ON tasks(filer, dedupe_key);
CREATE TABLE IF NOT EXISTS task_events (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  actor      TEXT NOT NULL,
  kind       TEXT NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS task_events_task ON task_events(task_id, id);
CREATE TABLE IF NOT EXISTS task_claimants (
  task_id INTEGER NOT NULL REFERENCES tasks(id),
  agent   TEXT NOT NULL,
  PRIMARY KEY (task_id, agent)
);
CREATE INDEX IF NOT EXISTS tasks_live ON tasks(id) WHERE status NOT IN ('done', 'dropped');
CREATE INDEX IF NOT EXISTS tasks_status ON tasks(status);`)
	if err != nil {
		return err
	}
	var has bool
	// task_id marks an ask that is an attempt at a task: the task it is
	// for, NULL for every other request.
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'task_id')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN task_id INTEGER`); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS requests_task ON requests(task_id) WHERE task_id IS NOT NULL`); err != nil {
		return err
	}
	// contributors is the JSON list of agents, beside the sender and the
	// chain, whose text the request's body carries; '' for none. The owner's
	// approval rules read it again when the request moves to a new target.
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'contributors')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN contributors TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	// chain is the JSON list of agents in the chain of the request the
	// entry's actor held a claim on as it wrote the entry's text; NULL for
	// none. Only an added entry and a comment carry one.
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('task_events') WHERE name = 'chain')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE task_events ADD COLUMN chain TEXT`); err != nil {
			return err
		}
	}
	return nil
}

// taskSelect reads tasks with the status of the ask open under each and
// the schedule each repeats on, neither of which is copied onto the task.
const taskSelect = `SELECT t.id, t.title, t.detail, t.status, t.filer, t.assignee, t.dedupe_key, t.result, t.result_request, t.current_request, t.agent_assigns, COALESCE(t.source, ''), COALESCE(t.suggested_assignee, ''), t.created_at, COALESCE(q.status, ''),
		COALESCE(r.id, 0), COALESCE(r.every_s, 0), COALESCE(r.to_agent, '')
	FROM tasks t LEFT JOIN requests q ON q.id = t.current_request LEFT JOIN schedules r ON r.task_id = t.id`

// taskVisible is true for the tasks viewer ?1 may read; an empty
// viewer is an admin's, every task. An agent reads a task it filed. It reads any other task only
// once it is past proposed, and then when the ask open under it is
// addressed to the agent, when the agent claimed an ask under it, or when
// it is open, unassigned, carries no result and has never been assigned.
// While the ask open under a task is held for the owner, its target reads
// the task only if it filed it, whatever part it took in the task before:
// the COALESCEs keep a task with no open ask, whose q columns are NULL, out
// of that test. A task that was assigned once leaves the shared pool for
// good: its ask may have been held and denied, and the target of a denied
// ask must not read the text afterwards.
const taskVisible = `(?1 = '' OR t.filer = ?1 OR t.status != 'proposed'
		AND NOT (COALESCE(q.to_agent, '') = ?1 AND COALESCE(q.status, '') = 'held') AND (
		q.to_agent = ?1
		OR EXISTS (SELECT 1 FROM task_claimants c WHERE c.task_id = t.id AND c.agent = ?1)
		OR t.status = 'open' AND t.assignee = '' AND t.result = '' AND t.result_request = ''
			AND NOT EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.kind = 'assigned')))`

const taskOpenStatuses = `status NOT IN ('done', 'dropped')`

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func getTask(ctx context.Context, q queryer, id int64, viewer string) (envelope.Task, error) {
	t, err := scanTask(q.QueryRowContext(ctx, taskSelect+` WHERE `+taskVisible+` AND t.id = ?2`, viewer, id))
	if errors.Is(err, sql.ErrNoRows) {
		return envelope.Task{}, ErrNoTask
	}
	return t, err
}

// AddTask stores a new task filed by t.Filer, open unless t.Status says
// proposed, and starts its history; t.Chain is kept on the first entry. A task with a key the same filer
// already has on a task that is not done or dropped is not stored: that
// task is returned, with false. An agent with MaxOpenTasks such tasks gets
// ErrTaskLimit; the owner has no limit. Both checks run in the insert's
// transaction, after its first write, so no other AddTask lands between
// them.
func (s *Store) AddTask(ctx context.Context, t envelope.Task) (envelope.Task, bool, error) {
	return s.addTask(ctx, t, nil)
}

// AddRepeatingTask is AddTask for a task that repeats on repeat, a schedule
// stored in the same transaction as AddSchedule stores one, so a task whose
// schedule is refused (ErrNoAgent, ErrScheduleLimit) is not stored either.
// A task returned for its key gets no schedule.
func (s *Store) AddRepeatingTask(ctx context.Context, t envelope.Task, repeat envelope.Schedule) (envelope.Task, bool, error) {
	return s.addTask(ctx, t, &repeat)
}

func (s *Store) addTask(ctx context.Context, t envelope.Task, repeat *envelope.Schedule) (envelope.Task, bool, error) {
	if t.Status == "" {
		t.Status = envelope.TaskOpen
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Task{}, false, err
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()
	res, err := tx.ExecContext(ctx, `INSERT INTO tasks (title, detail, status, filer, assignee, dedupe_key, source, suggested_assignee, created_at) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
		t.Title, t.Detail, string(t.Status), t.Filer, t.Assignee, t.Key, t.Source, t.SuggestedAssignee, now)
	if err != nil {
		return envelope.Task{}, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return envelope.Task{}, false, err
	}
	if t.Key != "" {
		var earlier int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE filer = ? AND dedupe_key = ? AND id != ? AND `+taskOpenStatuses+` ORDER BY id LIMIT 1`, t.Filer, t.Key, id).Scan(&earlier)
		if err == nil {
			// The deferred rollback takes the new row back out.
			out, err := getTask(ctx, tx, earlier, "")
			return out, false, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return envelope.Task{}, false, err
		}
	}
	if t.Filer != envelope.TaskOwner {
		var live int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE filer = ? AND `+taskOpenStatuses, t.Filer).Scan(&live); err != nil {
			return envelope.Task{}, false, err
		}
		if live > MaxOpenTasks {
			return envelope.Task{}, false, ErrTaskLimit
		}
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: id, Actor: t.Filer, Kind: envelope.TaskEventAdded, Status: t.Status, Chain: t.Chain}, now); err != nil {
		return envelope.Task{}, false, err
	}
	if repeat != nil {
		repeat.TaskID = id
		if _, err := s.addScheduleTx(ctx, tx, *repeat); err != nil {
			return envelope.Task{}, false, err
		}
	}
	out, err := getTask(ctx, tx, id, "")
	if err != nil {
		return envelope.Task{}, false, err
	}
	return out, true, tx.Commit()
}

// Task returns task id as viewer may read it; viewer "" is an admin's, any
// task. It returns ErrNoTask for a task viewer may not read, so a caller
// learns nothing about it.
func (s *Store) Task(ctx context.Context, id int64, viewer string) (envelope.Task, error) {
	return getTask(ctx, s.db, id, viewer)
}

// Tasks lists up to limit of the tasks viewer may read with an id above
// after, oldest first; viewer "" is an admin's, every task. Done and dropped
// tasks are listed only with closed.
func (s *Store) Tasks(ctx context.Context, viewer string, after int64, limit int, closed bool) ([]envelope.Task, error) {
	live := ""
	if !closed {
		live = ` AND t.` + taskOpenStatuses
	}
	rows, err := s.db.QueryContext(ctx, taskSelect+` WHERE `+taskVisible+live+` AND t.id > ?2 ORDER BY t.id LIMIT ?3`, viewer, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []envelope.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClosedTasks counts the done and the dropped tasks viewer may read; viewer
// "" is an admin's, every one.
func (s *Store) ClosedTasks(ctx context.Context, viewer string) (done, dropped int, err error) {
	query, args := `SELECT t.status, COUNT(*) FROM tasks t LEFT JOIN requests q ON q.id = t.current_request
		WHERE `+taskVisible+` AND t.status IN ('done', 'dropped') GROUP BY t.status`, []any{viewer}
	if viewer == "" {
		// Every task: the count is read from the status index alone.
		query, args = `SELECT status, COUNT(*) FROM tasks WHERE status IN ('done', 'dropped') GROUP BY status`, nil
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return 0, 0, err
		}
		if envelope.TaskStatus(status) == envelope.TaskDone {
			done = n
		} else {
			dropped = n
		}
	}
	return done, dropped, rows.Err()
}

// claimTaskTx gives the task to req, an ask being written in tx as an
// attempt at it: the task's one open ask becomes req, its assignee req's
// target, and it is working. The update is conditional on the task having
// no open ask and being open or returned, so of any number of asks racing
// for a task one is stored; the others, and an ask under a task that is
// missing or in any other status, get ErrTaskBusy. A task that repeats
// repeats for req's target from now on, unless req is stored as held for
// the owner: then the repeat follows only when the owner approves the ask
// (Release), so one that is denied, expires or is cancelled leaves the
// repeat where it was. A move to a fallback leaves it too.
func (s *Store) claimTaskTx(ctx context.Context, tx *sql.Tx, req envelope.Request) error {
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET current_request = ?, assignee = ?, status = ?
		WHERE id = ? AND current_request = '' AND status IN (?, ?)`,
		req.ID, req.To, string(envelope.TaskWorking), req.TaskID, string(envelope.TaskOpen), string(envelope.TaskReturned))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = ErrTaskBusy
		}
		return err
	}
	if req.Status != envelope.StatusHeld {
		if err := moveTaskRepeatTx(ctx, tx, req.TaskID, req.To); err != nil {
			return err
		}
	}
	actor := req.From
	if req.TaskActor != "" {
		actor = req.TaskActor
	}
	_, err = addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: req.TaskID, Actor: actor, Kind: envelope.TaskEventAssigned, Body: req.To, Status: envelope.TaskWorking}, s.now().UnixMilli())
	return err
}

// moveTaskRepeatTx makes the schedule task id repeats on, if it has one,
// send to to from now on. A schedule never targets its own sender, a pair
// no schedule may be made with, so an assign to that agent leaves the
// repeat where it was.
func moveTaskRepeatTx(ctx context.Context, tx *sql.Tx, id int64, to string) error {
	_, err := tx.ExecContext(ctx, `UPDATE schedules SET to_agent = ?1 WHERE task_id = ?2 AND to_agent != ?1 AND from_agent != ?1`, to, id)
	return err
}

// taskAskApprovedTx is for reqID, a held request the owner just approved in
// tx: when it is the ask open under a task that repeats, the repeat follows
// it to its target, as it would have at once had the ask not been held.
func taskAskApprovedTx(ctx context.Context, tx *sql.Tx, reqID string) error {
	var task int64
	var to string
	err := tx.QueryRowContext(ctx, `SELECT t.id, q.to_agent FROM requests q JOIN tasks t ON t.id = q.task_id AND t.current_request = q.id WHERE q.id = ?`, reqID).Scan(&task, &to)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return moveTaskRepeatTx(ctx, tx, task, to)
}

// forgetHeldTargetTx is for reqID, a request that just ended in tx with no
// result. When it was an attempt at a task that was held for the owner and
// never approved, its target is no longer one of the task's claimants,
// whatever part it took in the task before: the owner let none of what the
// ask carried reach it, so it does not read the task afterwards. The agent
// that filed the task still reads its own. One statement, which removes
// nothing for any other request.
func forgetHeldTargetTx(ctx context.Context, tx *sql.Tx, reqID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM task_claimants WHERE (task_id, agent) IN
		(SELECT task_id, to_agent FROM requests WHERE id = ? AND task_id IS NOT NULL AND was_held = 1 AND approved = 0)`, reqID)
	return err
}

// followTaskTx keeps a task in step with request reqID, the ask open under
// it, inside tx, the transaction that changes the request: it applies set
// (with args) to the task and writes ev to its history. It returns at once
// for a request that is no attempt at a task, and changes nothing unless
// the request is still the task's open ask, so a write about an ask the
// task has moved on from leaves the task alone. Every store write that
// ends, pauses or moves a request calls it, or a wrapper of it.
func (s *Store) followTaskTx(ctx context.Context, tx *sql.Tx, reqID, set string, args []any, ev envelope.TaskEvent) error {
	var task sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM requests WHERE id = ?`, reqID).Scan(&task); err != nil || !task.Valid {
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET `+set+` WHERE id = ? AND current_request = ?`, append(slices.Clone(args), task.Int64, reqID)...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	ev.TaskID = task.Int64
	_, err = addTaskEvent(ctx, tx, ev, s.now().UnixMilli())
	return err
}

// taskReleasedTx is followTaskTx for an ask that ended with no result: the
// task is open again with no assignee, and its history says why, as
// actor's change. The target of an ask that ended while still held is
// forgotten (forgetHeldTargetTx).
func (s *Store) taskReleasedTx(ctx context.Context, tx *sql.Tx, reqID, actor, note string) error {
	if err := forgetHeldTargetTx(ctx, tx, reqID); err != nil {
		return err
	}
	return s.followTaskTx(ctx, tx, reqID, `status = ?, assignee = '', current_request = ''`, []any{string(envelope.TaskOpen)},
		envelope.TaskEvent{Actor: actor, Kind: envelope.TaskEventStatus, Body: note, Status: envelope.TaskOpen})
}

// taskRepliedTx is followTaskTx for rep, a terminal reply just written in
// tx. An answer hands the task back: it is returned, and the answer is its
// result. A failure or a decline releases it, with the reason in its
// history. A reply that replaces a held acknowledgement finds its request
// closed and the task returned with the acknowledgement: it becomes the
// result instead, but only while the task is still returned on that
// request, so a task assigned again since keeps what it has.
func (s *Store) taskRepliedTx(ctx context.Context, tx *sql.Tx, rep envelope.Reply) error {
	// One read ends it for a reply to a request that is no attempt at a
	// task.
	var task sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM requests WHERE id = ?`, rep.RequestID).Scan(&task); err != nil || !task.Valid {
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		return err
	}
	// The ask's target can read the task after its reply even when it
	// replied without claiming first.
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_claimants (task_id, agent)
		SELECT task_id, to_agent FROM requests WHERE id = ? AND task_id IS NOT NULL AND to_agent = ?
		ON CONFLICT DO NOTHING`, rep.RequestID, rep.From); err != nil {
		return err
	}
	if rep.Replaced {
		res, err := tx.ExecContext(ctx, `UPDATE tasks SET result = ? WHERE result_request = ? AND status = ? AND current_request = ''
			AND id = (SELECT task_id FROM requests WHERE id = ?)`, taskResult(rep), rep.RequestID, string(envelope.TaskReturned), rep.RequestID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO task_events (task_id, actor, kind, body, status, created_at)
			SELECT task_id, ?, ?, ?, ?, ? FROM requests WHERE id = ?`,
			rep.From, envelope.TaskEventStatus, taskNote(string(rep.Status), "replaced the earlier acknowledgement"), string(envelope.TaskReturned), s.now().UnixMilli(), rep.RequestID)
		return err
	}
	if rep.Status != envelope.StatusAnswered {
		return s.taskReleasedTx(ctx, tx, rep.RequestID, rep.From, taskNote(string(rep.Status), rep.Body))
	}
	return s.followTaskTx(ctx, tx, rep.RequestID, `status = ?, result = ?, result_request = ?, current_request = ''`,
		[]any{string(envelope.TaskReturned), taskResult(rep), rep.RequestID},
		envelope.TaskEvent{Actor: rep.From, Kind: envelope.TaskEventStatus, Body: "answered on request " + rep.RequestID, Status: envelope.TaskReturned})
}

// taskNote is a history note: what happened, and the reason given, if any.
func taskNote(what, reason string) string {
	if reason == "" {
		return what
	}
	return what + ": " + reason
}

// taskResult is the result a task keeps of rep: its body, and for a reply
// with files a line naming them and the request that keeps them. The files
// stay on the request.
func taskResult(rep envelope.Reply) string {
	if len(rep.Attachments) == 0 {
		return rep.Body
	}
	names := make([]string, len(rep.Attachments))
	for i, a := range rep.Attachments {
		names[i] = a.Name
	}
	return strings.TrimPrefix(rep.Body+fmt.Sprintf("\n[%d attachment(s) on request %s: %s]", len(names), rep.RequestID, strings.Join(names, ", ")), "\n")
}

// taskProgressTx copies a progress note claimer just set on request reqID
// into the history of the task the request is an attempt at. One statement,
// which writes nothing for any other request.
func taskProgressTx(ctx context.Context, tx *sql.Tx, reqID, claimer, note string, at int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO task_events (task_id, actor, kind, body, created_at)
		SELECT q.task_id, ?, ?, ?, ? FROM requests q JOIN tasks t ON t.id = q.task_id AND t.current_request = q.id WHERE q.id = ?`,
		claimer, envelope.TaskEventProgress, note, at, reqID)
	return err
}

// cancelTaskAskTx cancels the ask open under task id inside tx, whatever
// open state it is in, and closes the relay's queued notices about it. The
// target of an ask cancelled while held is forgotten (forgetHeldTargetTx). It
// returns the ids it cancelled, none for a task with no open ask. The task
// row is the caller's to change in the same transaction.
func (s *Store) cancelTaskAskTx(ctx context.Context, tx *sql.Tx, id int64) ([]string, error) {
	var ask string
	err := tx.QueryRowContext(ctx, `UPDATE requests SET status = ?, lease_until = 0, updated_at = ?
		WHERE id = (SELECT current_request FROM tasks WHERE id = ?) AND status IN (?, ?, ?, ?, ?) RETURNING id`,
		string(envelope.StatusCancelled), s.now().UnixMilli(), id,
		string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed), string(envelope.StatusNeedsInput), string(envelope.StatusHeld)).Scan(&ask)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := forgetHeldTargetTx(ctx, tx, ask); err != nil {
		return nil, err
	}
	closed, err := s.closeNoticesTx(ctx, tx, ask)
	return append([]string{ask}, closed...), err
}

// endTaskRepeatTx deletes the schedule task id repeats on, if it has one,
// inside tx, the transaction that closes the task.
func endTaskRepeatTx(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE task_id = ?`, id)
	return err
}

// DropTask gives task id up as actor's change, with note in its history,
// from any status but done or dropped, and ends its repeat. An ask open under it is cancelled
// in the same transaction, claimed or not, so its target's later reply is
// refused as a reply to any cancelled request is; the task then has no
// assignee. It returns the dropped task and the ids of the requests it
// cancelled. Dropping with an ask open is the owner's to do: a caller
// acting for an agent uses SetTaskStatus from the statuses with no ask.
func (s *Store) DropTask(ctx context.Context, id int64, actor, note string) (envelope.Task, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Task{}, nil, err
	}
	defer tx.Rollback()
	cancelled, err := s.cancelTaskAskTx(ctx, tx, id)
	if err != nil {
		return envelope.Task{}, nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, assignee = CASE WHEN current_request = '' THEN assignee ELSE '' END, current_request = ''
		WHERE id = ? AND `+taskOpenStatuses, string(envelope.TaskDropped), id)
	if err != nil {
		return envelope.Task{}, nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			if _, err = getTask(ctx, tx, id, ""); err == nil {
				err = ErrTaskState
			}
		}
		return envelope.Task{}, nil, err
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: id, Actor: actor, Kind: envelope.TaskEventStatus, Body: note, Status: envelope.TaskDropped}, s.now().UnixMilli()); err != nil {
		return envelope.Task{}, nil, err
	}
	if err := endTaskRepeatTx(ctx, tx, id); err != nil {
		return envelope.Task{}, nil, err
	}
	out, err := getTask(ctx, tx, id, "")
	if err != nil {
		return envelope.Task{}, nil, err
	}
	return out, cancelled, tx.Commit()
}

// ReleaseTask takes task id back from its assignee as actor's change, with
// note in its history: the ask open under it is cancelled in the same
// transaction, claimed or not, and the task is open with no assignee, ready
// to be assigned to someone else. It returns the ids of the requests it
// cancelled. A task with no open ask is left as it is, with nothing
// cancelled; ErrNoTask when there is no such task.
func (s *Store) ReleaseTask(ctx context.Context, id int64, actor, note string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cancelled, err := s.cancelTaskAskTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if len(cancelled) == 0 {
		_, err := getTask(ctx, tx, id, "")
		return nil, err
	}
	openAsk := cancelled[0]
	if err := s.taskReleasedTx(ctx, tx, openAsk, actor, note); err != nil {
		return nil, err
	}
	return cancelled, tx.Commit()
}

// MarkTaskDone closes task id as actor's change, with note in its history.
// Only an open or returned task can be done: one with an ask open under it
// gets ErrTaskAskOpen, and any other status ErrTaskState.
func (s *Store) MarkTaskDone(ctx context.Context, id int64, actor, note string) (envelope.Task, error) {
	out, err := s.SetTaskStatus(ctx, id, envelope.TaskDone, actor, note, envelope.TaskOpen, envelope.TaskReturned)
	if errors.Is(err, ErrTaskState) {
		if t, terr := s.Task(ctx, id, ""); terr == nil && (t.Status == envelope.TaskWorking || t.Status == envelope.TaskQuestion) {
			err = ErrTaskAskOpen
		}
	}
	return out, err
}

// TaskCarry returns what the next ask under task id carries beside the
// task's own text: who wrote its stored result ("" when it has none), the
// chain its filer held a claim under as it filed the task (filedUnder, nil
// for none), and the comments made since that result, oldest first, each
// with the chain its author held.
func (s *Store) TaskCarry(ctx context.Context, id int64) (resultBy string, filedUnder []string, comments []envelope.TaskEvent, err error) {
	var last int64
	err = s.db.QueryRowContext(ctx, `SELECT id, actor FROM task_events WHERE task_id = ? AND kind = ? AND status = ? ORDER BY id DESC LIMIT 1`,
		id, envelope.TaskEventStatus, string(envelope.TaskReturned)).Scan(&last, &resultBy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil, err
	}
	var chain string
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(chain, '') FROM task_events WHERE task_id = ? AND kind = ? ORDER BY id LIMIT 1`, id, envelope.TaskEventAdded).Scan(&chain)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil, err
	}
	if chain != "" {
		if err := json.Unmarshal([]byte(chain), &filedUnder); err != nil {
			return "", nil, nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskEventCols+` FROM task_events WHERE task_id = ? AND kind = ? AND id > ? ORDER BY id`,
		id, envelope.TaskEventComment, last)
	if err != nil {
		return "", nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ev, err := scanTaskEvent(rows)
		if err != nil {
			return "", nil, nil, err
		}
		comments = append(comments, ev)
	}
	return resultBy, filedUnder, comments, rows.Err()
}

// ReopenTask puts a done or dropped task back as actor's change, with note
// in its history, and returns ErrTaskState for a task in any other status.
// It goes back to open, except a task that was filed as proposed and that
// the owner never approved: that one was dropped before the owner's
// decision, and returns to proposed to wait for it. The owner's approval is
// the one entry by the owner that opens a proposed task, so its absence is
// what marks one. The check and the change are one statement.
func (s *Store) ReopenTask(ctx context.Context, id int64, actor, note string) (envelope.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Task{}, err
	}
	defer tx.Rollback()
	var to string
	err = tx.QueryRowContext(ctx, `UPDATE tasks SET status = CASE
			WHEN EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = tasks.id AND e.kind = ?1 AND e.status = ?2)
				AND NOT EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = tasks.id AND e.kind = ?3 AND e.status = ?4 AND e.actor = ?5)
			THEN ?2 ELSE ?4 END
		WHERE id = ?6 AND status IN (?7, ?8) RETURNING status`,
		envelope.TaskEventAdded, string(envelope.TaskProposed), envelope.TaskEventStatus, string(envelope.TaskOpen), envelope.TaskOwner,
		id, string(envelope.TaskDone), string(envelope.TaskDropped)).Scan(&to)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := getTask(ctx, tx, id, ""); err != nil {
			return envelope.Task{}, err
		}
		return envelope.Task{}, ErrTaskState
	}
	if err != nil {
		return envelope.Task{}, err
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: id, Actor: actor, Kind: envelope.TaskEventStatus, Body: note, Status: envelope.TaskStatus(to)}, s.now().UnixMilli()); err != nil {
		return envelope.Task{}, err
	}
	out, err := getTask(ctx, tx, id, "")
	if err != nil {
		return envelope.Task{}, err
	}
	return out, tx.Commit()
}

// SetTaskStatus moves task id to status to and records it in the task's
// history as actor's change, with note. With from, it does so only while
// the task is in one of those statuses, and returns ErrTaskState otherwise:
// the check and the change are one statement, so an ask sent under the task
// meanwhile either lands first and refuses the change, or finds the new
// status. A task moved to done or dropped stops repeating: its schedule is
// deleted in the same transaction.
func (s *Store) SetTaskStatus(ctx context.Context, id int64, to envelope.TaskStatus, actor, note string, from ...envelope.TaskStatus) (envelope.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Task{}, err
	}
	defer tx.Rollback()
	cond, args := ``, []any{string(to), id}
	if len(from) > 0 {
		cond = ` AND status IN (?` + strings.Repeat(`, ?`, len(from)-1) + `)`
		for _, st := range from {
			args = append(args, string(st))
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ?`+cond, args...)
	if err != nil {
		return envelope.Task{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return envelope.Task{}, err
	}
	if n == 0 {
		if _, err := getTask(ctx, tx, id, ""); err != nil {
			return envelope.Task{}, err
		}
		return envelope.Task{}, ErrTaskState
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: id, Actor: actor, Kind: envelope.TaskEventStatus, Body: note, Status: to}, s.now().UnixMilli()); err != nil {
		return envelope.Task{}, err
	}
	if to == envelope.TaskDone || to == envelope.TaskDropped {
		if err := endTaskRepeatTx(ctx, tx, id); err != nil {
			return envelope.Task{}, err
		}
	}
	out, err := getTask(ctx, tx, id, "")
	if err != nil {
		return envelope.Task{}, err
	}
	return out, tx.Commit()
}

// CountTaskAssign takes one of task id's MaxAgentAssigns agent assigns, for
// an assign an agent is about to make. The check and the count are one
// statement, so racing assigns never take more than there are. A task with
// none left gets ErrTaskAssignLimit.
func (s *Store) CountTaskAssign(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET agent_assigns = agent_assigns + 1 WHERE id = ? AND agent_assigns < ?`, id, MaxAgentAssigns)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	if _, err := s.Task(ctx, id, ""); err != nil {
		return err
	}
	return ErrTaskAssignLimit
}

// UncountTaskAssign gives back what CountTaskAssign took, for an assign
// that sent nothing.
func (s *Store) UncountTaskAssign(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET agent_assigns = agent_assigns - 1 WHERE id = ? AND agent_assigns > 0`, id)
	return err
}

// ResetTaskAssigns sets task id's count of agent assigns back to zero as
// actor's change, and says so in its history.
func (s *Store) ResetTaskAssigns(ctx context.Context, id int64, actor string) (envelope.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Task{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET agent_assigns = 0 WHERE id = ?`, id); err != nil {
		return envelope.Task{}, err
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: id, Actor: actor, Kind: envelope.TaskEventAssignsReset}, s.now().UnixMilli()); err != nil {
		return envelope.Task{}, err
	}
	out, err := getTask(ctx, tx, id, "")
	if err != nil {
		return envelope.Task{}, err
	}
	return out, tx.Commit()
}

// AppendTaskEvent adds ev to the history of task ev.TaskID, stamped now. It
// returns ErrNoTask when there is no such task.
func (s *Store) AppendTaskEvent(ctx context.Context, ev envelope.TaskEvent) (envelope.TaskEvent, error) {
	return addTaskEvent(ctx, s.db, ev, s.now().UnixMilli())
}

// addTaskEvent writes one history entry, with ev.Chain when it has one. The
// task check and the insert are one statement.
func addTaskEvent(ctx context.Context, x execer, ev envelope.TaskEvent, at int64) (envelope.TaskEvent, error) {
	var chain sql.NullString
	if len(ev.Chain) > 0 {
		b, err := json.Marshal(ev.Chain)
		if err != nil {
			return envelope.TaskEvent{}, err
		}
		chain = sql.NullString{String: string(b), Valid: true}
	}
	res, err := x.ExecContext(ctx, `INSERT INTO task_events (task_id, actor, kind, body, status, created_at, chain)
		SELECT ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM tasks WHERE id = ?)`,
		ev.TaskID, ev.Actor, ev.Kind, ev.Body, string(ev.Status), at, chain, ev.TaskID)
	if err != nil {
		return envelope.TaskEvent{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.TaskEvent{}, ErrNoTask
	}
	if ev.ID, err = res.LastInsertId(); err != nil {
		return envelope.TaskEvent{}, err
	}
	ev.At = time.UnixMilli(at).UTC()
	return ev, nil
}

// TaskEvents returns up to limit entries of task id's history after the
// entry with id after, oldest first.
func (s *Store) TaskEvents(ctx context.Context, id, after int64, limit int) ([]envelope.TaskEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskEventCols+` FROM task_events WHERE task_id = ? AND id > ? ORDER BY id LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []envelope.TaskEvent{}
	for rows.Next() {
		ev, err := scanTaskEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// RecordTaskClaimant notes that agent took part in task id, by claiming or
// assigning an ask under it, so the agent can read the task from then on. Recording it again changes nothing.
func (s *Store) RecordTaskClaimant(ctx context.Context, id int64, agent string) error {
	return recordTaskClaimant(ctx, s.db, id, agent)
}

// recordTaskClaimant is RecordTaskClaimant on x, which may be the
// transaction of the claim itself.
func recordTaskClaimant(ctx context.Context, x execer, id int64, agent string) error {
	_, err := x.ExecContext(ctx, `INSERT INTO task_claimants (task_id, agent) VALUES (?, ?) ON CONFLICT DO NOTHING`, id, agent)
	return err
}

func scanTask(sc scanner) (envelope.Task, error) {
	var t envelope.Task
	var status, ask string
	var created int64
	if err := sc.Scan(&t.ID, &t.Title, &t.Detail, &status, &t.Filer, &t.Assignee, &t.Key, &t.Result, &t.ResultRequest, &t.CurrentRequest, &t.AgentAssigns, &t.Source, &t.SuggestedAssignee, &created, &ask, &t.RepeatSchedule, &t.RepeatEverySeconds, &t.RepeatTo); err != nil {
		return envelope.Task{}, err
	}
	t.Status, t.AskStatus, t.CreatedAt = envelope.TaskStatus(status), envelope.Status(ask), time.UnixMilli(created).UTC()
	return t, nil
}

const taskEventCols = `id, task_id, actor, kind, body, status, created_at, COALESCE(chain, '')`

func scanTaskEvent(sc scanner) (envelope.TaskEvent, error) {
	var ev envelope.TaskEvent
	var status, chain string
	var at int64
	if err := sc.Scan(&ev.ID, &ev.TaskID, &ev.Actor, &ev.Kind, &ev.Body, &status, &at, &chain); err != nil {
		return envelope.TaskEvent{}, err
	}
	if chain != "" {
		if err := json.Unmarshal([]byte(chain), &ev.Chain); err != nil {
			return envelope.TaskEvent{}, err
		}
	}
	ev.Status, ev.At = envelope.TaskStatus(status), time.UnixMilli(at).UTC()
	return ev, nil
}

// TaskRelay is the actor a task's history names for a change the relay
// made itself: an expiry, a removal, a move.
const TaskRelay = "relay"
