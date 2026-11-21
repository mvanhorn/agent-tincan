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
// result, and result_authors the JSON list of the agents behind that ask:
// who replied, its sender, its chain and its contributors, NULL for a task
// with no result or one stored before the column. task_events is the task's history, read in id order, which is
// also time order. task_claimants holds every agent that claimed, answered
// or assigned an ask under the task, which is what lets it keep reading
// the task afterwards; the target of an ask that ended while held for the
// owner is taken out again. source is the outside source the filer declared for
// the task's text, and suggested_assignee whom the filer asked to have a
// proposed task go to; NULL for none. named_for is whom the filer named for
// the task as it added it, a teammate or the owner, NULL for none: a task
// with one was addressed to that one and is never in the shared pool,
// whatever became of its assign. tasks_live is what a listing without
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
  suggested_assignee TEXT,
  named_for          TEXT,
  result_authors     TEXT
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
	// A tasks table made before named_for gets the column here.
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name = 'named_for')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE tasks ADD COLUMN named_for TEXT`); err != nil {
			return err
		}
	}
	// A tasks table made before result_authors gets the column here.
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name = 'result_authors')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE tasks ADD COLUMN result_authors TEXT`); err != nil {
			return err
		}
	}
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
	// entry's actor held a claim on as it wrote the entry's text, and of
	// that request's contributors; NULL for none. Only an added entry and a
	// comment carry one.
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('task_events') WHERE name = 'chain')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE task_events ADD COLUMN chain TEXT`); err != nil {
			return err
		}
	}
	// for_owner marks a status entry a task host made on the owner's word,
	// and prior is the status the task had before the entry, NULL for none:
	// it is kept on a host's done or drop, so a reopen can put the task back
	// where the host found it, and on every reopen.
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('task_events') WHERE name = 'for_owner')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE task_events ADD COLUMN for_owner INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('task_events') WHERE name = 'prior')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE task_events ADD COLUMN prior TEXT`); err != nil {
			return err
		}
	}
	for _, col := range []struct{ name, kind string }{
		{"due_day", "TEXT"}, {"due_at", "INTEGER"}, {"reminded_at", "INTEGER"}, {"reminder_dropped_at", "INTEGER"},
	} {
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name = ?)`, col.name).Scan(&has); err != nil {
			return err
		}
		if !has {
			if _, err := s.db.Exec(`ALTER TABLE tasks ADD COLUMN ` + col.name + " " + col.kind); err != nil {
				return err
			}
		}
	}
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('task_events') WHERE name = 'due_change')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE task_events ADD COLUMN due_change TEXT`); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS tasks_reminder_due ON tasks(due_at)
		WHERE reminded_at IS NULL AND reminder_dropped_at IS NULL AND status NOT IN ('done', 'dropped')`)
	return err
}

// taskSelect reads tasks with the status of the ask open under each and
// the schedule each repeats on, neither of which is copied onto the task.
const taskSelect = `SELECT ` + taskCols + taskFrom

const taskCols = `t.id, t.title, t.detail, t.status, t.filer, t.assignee, t.dedupe_key, t.result, t.result_request, t.current_request, t.agent_assigns, COALESCE(t.source, ''), COALESCE(t.suggested_assignee, ''), t.created_at, COALESCE(t.due_day, ''), t.due_at, COALESCE(q.status, ''),
		COALESCE(r.id, 0), COALESCE(r.every_s, 0), COALESCE(r.to_agent, '')`

const taskFrom = `
	FROM tasks t LEFT JOIN requests q ON q.id = t.current_request LEFT JOIN schedules r ON r.task_id = t.id`

// taskVisible is true for the tasks viewer ?1 may read; an empty
// viewer is an admin's, every task. An agent reads a task it filed. It reads any other task only
// once it is past proposed, and then when the ask open under it is
// addressed to the agent, when the agent claimed an ask under it, or when
// it is open, unassigned, carries no result, has never been assigned and
// was added with nobody named for it (named_for). That last test does not
// wait for an assign: a task added for one teammate is out of the shared
// pool from the insert, through the moment before its ask is written and
// for good when that ask is refused.
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
		OR t.status = 'open' AND t.assignee = '' AND t.result = '' AND t.result_request = '' AND t.named_for IS NULL
			AND NOT EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.kind = 'assigned')))`

// taskNeverApproved is true for a task t that was filed as proposed and
// that the owner never approved, whatever its status now. The owner's
// approval is the one entry by the owner that opens a proposed task, so its
// absence is what marks one. ReopenTask and the host view both read it, so
// they cannot come to disagree about which tasks wait for the owner.
const taskNeverApproved = `(EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.kind = '` + envelope.TaskEventAdded + `' AND e.status = '` + string(envelope.TaskProposed) + `')
		AND NOT EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.kind = '` + envelope.TaskEventStatus + `' AND e.status = '` + string(envelope.TaskOpen) + `' AND e.actor = '` + envelope.TaskOwner + `'))`

// taskHostVisible is true for the tasks task host ?1 is shown: every task
// but one whose open ask is held for the owner with the host as its target,
// which the host reads only if it filed it, as in taskVisible. Being shown
// a task is not reading it: taskHostWithheld says which of these the host
// gets as a row without text.
const taskHostVisible = `(t.filer = ?1 OR NOT (COALESCE(q.to_agent, '') = ?1 AND COALESCE(q.status, '') = 'held'))`

// taskHostWithheld is true for a task whose text task host ?1 may not read:
// one the owner never approved (taskNeverApproved) that the host did not
// file.
const taskHostWithheld = `(t.filer != ?1 AND ` + taskNeverApproved + `)`

const taskOpenStatuses = `status NOT IN ('done', 'dropped')`

// taskResultEvent is the id of the history entry that last stored a result
// on a row of tasks, 0 for a task with none. Every write of a result adds
// one in its own transaction, so the id names one version of the result. A
// reopen that puts a task back to returned stores no result and records
// the status it left (prior), which is what keeps it out.
const taskResultEvent = `COALESCE((SELECT MAX(e.id) FROM task_events e WHERE e.task_id = tasks.id AND e.kind = 'status' AND e.status = 'returned' AND e.prior IS NULL), 0)`

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
// proposed, and starts its history; t.Chain is kept on the first entry, and
// t.NamedFor on the task, which keeps it out of the shared pool. A task with a key the same filer
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
	res, err := tx.ExecContext(ctx, `INSERT INTO tasks (title, detail, status, filer, assignee, dedupe_key, source, suggested_assignee, named_for, created_at, due_day, due_at) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, ''), ?)`,
		t.Title, t.Detail, string(t.Status), t.Filer, t.Assignee, t.Key, t.Source, t.SuggestedAssignee, t.NamedFor, now, t.DueDay, t.DueAt)
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
	if t.Status == envelope.TaskProposed {
		// A task that waits for the owner is news for the task hosts.
		if err := taskNoticeTx(ctx, tx, id, t.Status, t.Filer, "", now); err != nil {
			return envelope.Task{}, false, err
		}
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
	if viewer == "" {
		// Every task: the count is read from the status index alone.
		return s.closedTasks(ctx, `SELECT status, COUNT(*) FROM tasks WHERE status IN ('done', 'dropped') GROUP BY status`)
	}
	return s.closedTasks(ctx, closedTasksWhere+taskVisible+closedTasksBy, viewer)
}

const (
	closedTasksWhere = `SELECT t.status, COUNT(*) FROM tasks t LEFT JOIN requests q ON q.id = t.current_request WHERE `
	closedTasksBy    = ` AND t.status IN ('done', 'dropped') GROUP BY t.status`
)

// HostTask returns task id as task host host is shown it: whole, or as a
// withheld row (withheldTask) when the owner never approved it and host did
// not file it. It returns ErrNoTask for a task that is not shown to the
// host at all. Whether host is a task host now is the caller's to have
// checked.
func (s *Store) HostTask(ctx context.Context, id int64, host string) (envelope.Task, error) {
	t, err := scanHostTask(s.db.QueryRowContext(ctx, hostTaskSelect+` AND t.id = ?2`, host, id))
	if errors.Is(err, sql.ErrNoRows) {
		return envelope.Task{}, ErrNoTask
	}
	return t, err
}

// HostTasks is Tasks for task host host: up to limit of the tasks it is
// shown with an id above after, oldest first, each as HostTask returns it.
func (s *Store) HostTasks(ctx context.Context, host string, after int64, limit int, closed bool) ([]envelope.Task, error) {
	live := ""
	if !closed {
		live = ` AND t.` + taskOpenStatuses
	}
	rows, err := s.db.QueryContext(ctx, hostTaskSelect+live+` AND t.id > ?2 ORDER BY t.id LIMIT ?3`, host, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []envelope.Task{}
	for rows.Next() {
		t, err := scanHostTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClosedHostTasks counts the done and the dropped tasks task host host is
// shown, withheld rows among them.
func (s *Store) ClosedHostTasks(ctx context.Context, host string) (done, dropped int, err error) {
	return s.closedTasks(ctx, closedTasksWhere+taskHostVisible+closedTasksBy, host)
}

// hostTaskSelect reads the tasks task host ?1 is shown, each with whether
// its text is withheld from the host. Every read for a host goes through it
// and scanHostTask.
const hostTaskSelect = `SELECT ` + taskCols + `, ` + taskHostWithheld + taskFrom + ` WHERE ` + taskHostVisible

// scanHostTask reads one row of hostTaskSelect, and hands on a task whose
// text is withheld as withheldTask makes it.
func scanHostTask(sc scanner) (envelope.Task, error) {
	var withheld bool
	t, err := scanTaskAnd(sc, &withheld)
	if err != nil || !withheld {
		return t, err
	}
	return withheldTask(t), nil
}

// withheldTask is the row a task host is shown of t, a task whose text it
// may not read. It is built from the fields named here and no others, so
// the title, detail, result, key and suggested assignee are left out, and
// so is any field a task gains later.
func withheldTask(t envelope.Task) envelope.Task {
	return envelope.Task{ID: t.ID, Status: t.Status, Filer: t.Filer, Source: t.Source, CreatedAt: t.CreatedAt, Withheld: true}
}

// closedTasks runs query, which counts tasks by status, and returns the
// counts of the done and the dropped.
func (s *Store) closedTasks(ctx context.Context, query string, args ...any) (done, dropped int, err error) {
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
// missing or in any other status, get ErrTaskBusy. It is also conditional
// on the task's result still being the one req's body was built from
// (req.TaskResultEvent, as TaskCarry gave it): an ask written from a result
// that has been replaced since gets ErrTaskBusy too, and its sender builds
// it again. A task that repeats
// repeats for req's target from now on, unless req is stored as held for
// the owner: then the repeat follows only when the owner approves the ask
// (Release), so one that is denied, expires or is cancelled leaves the
// repeat where it was. A move to a fallback leaves it too.
func (s *Store) claimTaskTx(ctx context.Context, tx *sql.Tx, req envelope.Request) error {
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET current_request = ?, assignee = ?, status = ?
		WHERE id = ? AND current_request = '' AND status IN (?, ?) AND `+taskResultEvent+` = ?`,
		req.ID, req.To, string(envelope.TaskWorking), req.TaskID, string(envelope.TaskOpen), string(envelope.TaskReturned), req.TaskResultEvent)
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
	ev := envelope.TaskEvent{TaskID: req.TaskID, Actor: actor, Kind: envelope.TaskEventAssigned, Body: req.To, Status: envelope.TaskWorking}
	if req.TaskOwnerWords != "" {
		ev.Actor, ev.Body, ev.ForOwner = req.From, req.TaskOwnerWords, true
	}
	_, err = addTaskEvent(ctx, tx, ev, s.now().UnixMilli())
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
// ends, pauses or moves a request calls it, or a wrapper of it. A status
// entry that is news for the task hosts gets its outbox row here too
// (taskAskNoticeTx), so no such write can leave the row out.
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
	now := s.now().UnixMilli()
	if _, err = addTaskEvent(ctx, tx, ev, now); err != nil {
		return err
	}
	// A task that came back, asks a question or is open again is news for
	// the task hosts; one that is working again is not.
	if ev.Kind == envelope.TaskEventStatus && ev.Status != envelope.TaskWorking {
		return taskAskNoticeTx(ctx, tx, reqID, ev.Status, ev.Actor, now)
	}
	return nil
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
// request, so a task assigned again since keeps what it has. Either write
// of a result stores the agents behind it beside it (taskResultAuthorsTx).
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
	var authors string
	var err error
	// The ask's target can read the task after its reply even when it
	// replied without claiming first.
	if _, err = tx.ExecContext(ctx, `INSERT INTO task_claimants (task_id, agent)
		SELECT task_id, to_agent FROM requests WHERE id = ? AND task_id IS NOT NULL AND to_agent = ?
		ON CONFLICT DO NOTHING`, rep.RequestID, rep.From); err != nil {
		return err
	}
	if rep.Replaced || rep.Status == envelope.StatusAnswered {
		if authors, err = taskResultAuthorsTx(ctx, tx, rep); err != nil {
			return err
		}
	}
	if rep.Replaced {
		res, err := tx.ExecContext(ctx, `UPDATE tasks SET result = ?, result_authors = ? WHERE result_request = ? AND status = ? AND current_request = ''
			AND id = (SELECT task_id FROM requests WHERE id = ?)`, taskResult(rep), authors, rep.RequestID, string(envelope.TaskReturned), rep.RequestID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		now := s.now().UnixMilli()
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_events (task_id, actor, kind, body, status, created_at)
			SELECT task_id, ?, ?, ?, ?, ? FROM requests WHERE id = ?`,
			rep.From, envelope.TaskEventStatus, taskNote(string(rep.Status), "replaced the earlier acknowledgement"), string(envelope.TaskReturned), now, rep.RequestID); err != nil {
			return err
		}
		return taskAskNoticeTx(ctx, tx, rep.RequestID, envelope.TaskReturned, rep.From, now)
	}
	if rep.Status != envelope.StatusAnswered {
		return s.taskReleasedTx(ctx, tx, rep.RequestID, rep.From, taskNote(string(rep.Status), rep.Body))
	}
	return s.followTaskTx(ctx, tx, rep.RequestID, `status = ?, result = ?, result_request = ?, result_authors = ?, current_request = ''`,
		[]any{string(envelope.TaskReturned), taskResult(rep), rep.RequestID, authors},
		envelope.TaskEvent{Actor: rep.From, Kind: envelope.TaskEventStatus, Body: "answered on request " + rep.RequestID, Status: envelope.TaskReturned})
}

// taskResultAuthorsTx is the JSON list a task keeps with the result rep
// stores: the agents whose text the answer may hold. rep.From wrote it, and
// wrote it while working the request it answers, so the request's sender,
// chain and contributors are behind it as well. Never the owner, whose text
// needs no approval of the owner's.
func taskResultAuthorsTx(ctx context.Context, tx *sql.Tx, rep envelope.Reply) (string, error) {
	var from, chain, contributors string
	if err := tx.QueryRowContext(ctx, `SELECT from_agent, chain, contributors FROM requests WHERE id = ?`, rep.RequestID).Scan(&from, &chain, &contributors); err != nil {
		return "", err
	}
	names := []string{rep.From}
	for _, list := range []string{chain, contributors} {
		if list == "" {
			continue
		}
		var more []string
		if err := json.Unmarshal([]byte(list), &more); err != nil {
			return "", err
		}
		names = append(names, more...)
	}
	names = append(names, from)
	authors := []string{}
	for _, name := range names {
		if name != "" && name != envelope.TaskOwner && !slices.Contains(authors, name) {
			authors = append(authors, name)
		}
	}
	b, err := json.Marshal(authors)
	return string(b), err
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
	if err := s.closeTaskReminderTx(ctx, tx, id); err != nil {
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
	return s.markTaskDone(ctx, id, taskChange{actor: actor, note: note})
}

// MarkTaskDoneForOwner is MarkTaskDone for task host host acting on the
// owner's words: the history entry names the host, is marked as done for
// the owner (envelope.TaskEvent.ForOwner), quotes the words and records the
// status the task had, which a later reopen returns it to.
func (s *Store) MarkTaskDoneForOwner(ctx context.Context, id int64, host, words string) (envelope.Task, error) {
	return s.markTaskDone(ctx, id, taskChange{actor: host, note: words, forOwner: true})
}

func (s *Store) markTaskDone(ctx context.Context, id int64, by taskChange) (envelope.Task, error) {
	out, err := s.setTaskStatus(ctx, id, envelope.TaskDone, by, envelope.TaskOpen, envelope.TaskReturned)
	if errors.Is(err, ErrTaskState) {
		if t, terr := s.Task(ctx, id, ""); terr == nil && (t.Status == envelope.TaskWorking || t.Status == envelope.TaskQuestion) {
			err = ErrTaskAskOpen
		}
	}
	return out, err
}

// TaskCarried is what the next ask under a task is built from, read as one
// snapshot.
type TaskCarried struct {
	// Task is the task, whole.
	Task envelope.Task
	// ResultBy is who wrote Task.Result, "" for a task with none.
	ResultBy string
	// ResultAuthors are the agents behind Task.Result: ResultBy, and the
	// sender, chain and contributors of the ask it answered. Nil for a task
	// with no result.
	ResultAuthors []string
	// ResultEvent is the id of the history entry that last stored
	// Task.Result, 0 for none: what envelope.Request.TaskResultEvent takes.
	ResultEvent int64
	// FiledUnder is the chain the filer held a claim under as it filed the
	// task, with that request's contributors; nil for none.
	FiledUnder []string
	// Comments are the comments no attempt has been handed with a result
	// yet, oldest first, each with what its author held the same way.
	Comments []envelope.TaskEvent
}

// TaskCarry returns what the next ask under task id carries, all of it read
// in one transaction, so the result, who is behind it and the comments
// beside it belong to the same moment. It returns ErrNoTask when there is
// no such task.
//
// The comments are those made since the attempt that produced the result
// handed the task back. A reply that replaces a held acknowledgement is the
// same attempt's: it writes a second returned entry, and the comments made
// between the two were in no ask, so the first entry stays the cutoff. That
// is the first returned entry after the last assign before the latest one.
func (s *Store) TaskCarry(ctx context.Context, id int64) (TaskCarried, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskCarried{}, err
	}
	defer tx.Rollback()
	var c TaskCarried
	if c.Task, err = getTask(ctx, tx, id, ""); err != nil {
		return TaskCarried{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT id, actor FROM task_events WHERE task_id = ? AND kind = ? AND status = ? AND prior IS NULL ORDER BY id DESC LIMIT 1`,
		id, envelope.TaskEventStatus, string(envelope.TaskReturned)).Scan(&c.ResultEvent, &c.ResultBy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TaskCarried{}, err
	}
	since := c.ResultEvent
	if since != 0 {
		err = tx.QueryRowContext(ctx, `SELECT MIN(id) FROM task_events WHERE task_id = ?1 AND kind = ?2 AND status = ?3 AND prior IS NULL
			AND id > COALESCE((SELECT MAX(id) FROM task_events WHERE task_id = ?1 AND kind = ?4 AND id < ?5), 0)`,
			id, envelope.TaskEventStatus, string(envelope.TaskReturned), envelope.TaskEventAssigned, c.ResultEvent).Scan(&since)
		if err != nil {
			return TaskCarried{}, err
		}
	}
	var authors, chain string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(t.result_authors, ''), COALESCE((SELECT e.chain FROM task_events e WHERE e.task_id = t.id AND e.kind = ? ORDER BY e.id LIMIT 1), '')
		FROM tasks t WHERE t.id = ?`, envelope.TaskEventAdded, id).Scan(&authors, &chain)
	if err != nil {
		return TaskCarried{}, err
	}
	if authors != "" && c.Task.Result != "" {
		if err := json.Unmarshal([]byte(authors), &c.ResultAuthors); err != nil {
			return TaskCarried{}, err
		}
	}
	if chain != "" {
		if err := json.Unmarshal([]byte(chain), &c.FiledUnder); err != nil {
			return TaskCarried{}, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+taskEventCols+` FROM task_events WHERE task_id = ? AND kind = ? AND id > ? ORDER BY id`,
		id, envelope.TaskEventComment, since)
	if err != nil {
		return TaskCarried{}, err
	}
	defer rows.Close()
	for rows.Next() {
		ev, err := scanTaskEvent(rows)
		if err != nil {
			return TaskCarried{}, err
		}
		c.Comments = append(c.Comments, ev)
	}
	return c, rows.Err()
}

// ReopenTask puts a done or dropped task back as actor's change, with note
// in its history, and returns ErrTaskState for a task in any other status.
// It goes back to open, with two exceptions. A task that was filed as
// proposed and that the owner never approved (taskNeverApproved) was
// dropped before the owner's decision, and returns to proposed to wait for
// it, whoever closed it. A task a task host closed for the owner
// (taskHostClosedPrior) returns to the status the host found it in, open or
// returned, so a result the host closed on comes back to the owner's
// attention with the result still on it. The check and the change are one
// statement, and the entry records the status the task left.
func (s *Store) ReopenTask(ctx context.Context, id int64, actor, note string) (envelope.Task, error) {
	return s.reopenTask(ctx, id, taskChange{actor: actor, note: note})
}

// ReopenTaskForOwner is ReopenTask for task host host acting on the owner's
// words, with the history entry marked as MarkTaskDoneForOwner marks it.
func (s *Store) ReopenTaskForOwner(ctx context.Context, id int64, host, words string) (envelope.Task, error) {
	return s.reopenTask(ctx, id, taskChange{actor: host, note: words, forOwner: true})
}

// taskHostClosedPrior is the status a task host found task t in when it
// closed t for the owner, open or returned, and NULL when the entry that
// closed t is not a host's: the owner's own done and drop, and an agent's
// drop, reopen to open as before.
const taskHostClosedPrior = `(SELECT e.prior FROM task_events e WHERE e.id = (SELECT MAX(c.id) FROM task_events c WHERE c.task_id = t.id AND c.kind = '` + envelope.TaskEventStatus + `' AND c.status IN ('done', 'dropped'))
		AND e.for_owner = 1 AND e.prior IN ('open', 'returned'))`

func (s *Store) reopenTask(ctx context.Context, id int64, by taskChange) (envelope.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Task{}, err
	}
	defer tx.Rollback()
	var prior string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&prior); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNoTask
		}
		return envelope.Task{}, err
	}
	var to string
	err = tx.QueryRowContext(ctx, `UPDATE tasks AS t SET status = CASE WHEN `+taskNeverApproved+` THEN ?1 ELSE COALESCE(`+taskHostClosedPrior+`, ?2) END
		WHERE id = ?3 AND status IN (?4, ?5) RETURNING status`,
		string(envelope.TaskProposed), string(envelope.TaskOpen), id, string(envelope.TaskDone), string(envelope.TaskDropped)).Scan(&to)
	if errors.Is(err, sql.ErrNoRows) {
		return envelope.Task{}, ErrTaskState
	}
	if err != nil {
		return envelope.Task{}, err
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: id, Actor: by.actor, Kind: envelope.TaskEventStatus, Body: by.note, Status: envelope.TaskStatus(to), ForOwner: by.forOwner, Prior: envelope.TaskStatus(prior)}, s.now().UnixMilli()); err != nil {
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
	return s.setTaskStatus(ctx, id, to, taskChange{actor: actor, note: note}, from...)
}

// DropTaskForOwner drops task id for task host host acting on the owner's
// words, from a status with no ask open under it (proposed, open or
// returned), so it never cancels a teammate's ask: a task with one gets
// ErrTaskState, as an agent's drop of its own task does. The history entry
// is marked as MarkTaskDoneForOwner marks it.
func (s *Store) DropTaskForOwner(ctx context.Context, id int64, host, words string) (envelope.Task, error) {
	return s.setTaskStatus(ctx, id, envelope.TaskDropped, taskChange{actor: host, note: words, forOwner: true}, envelope.TaskProposed, envelope.TaskOpen, envelope.TaskReturned)
}

// taskChange is who changes a task's status and the note its history
// entry keeps. forOwner marks a task host acting on the owner's words: the
// note is those words, and the entry records the status the task had.
type taskChange struct {
	actor, note string
	forOwner    bool
}

func (s *Store) setTaskStatus(ctx context.Context, id int64, to envelope.TaskStatus, by taskChange, from ...envelope.TaskStatus) (envelope.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Task{}, err
	}
	defer tx.Rollback()
	// The store has one connection, so nothing changes the task between
	// this read and the update below.
	var prior string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&prior); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNoTask
		}
		return envelope.Task{}, err
	}
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
		return envelope.Task{}, ErrTaskState
	}
	ev := envelope.TaskEvent{TaskID: id, Actor: by.actor, Kind: envelope.TaskEventStatus, Body: by.note, Status: to}
	if by.forOwner {
		ev.ForOwner, ev.Prior = true, envelope.TaskStatus(prior)
	}
	if _, err := addTaskEvent(ctx, tx, ev, s.now().UnixMilli()); err != nil {
		return envelope.Task{}, err
	}
	if to == envelope.TaskDone || to == envelope.TaskDropped {
		if err := endTaskRepeatTx(ctx, tx, id); err != nil {
			return envelope.Task{}, err
		}
		if err := s.closeTaskReminderTx(ctx, tx, id); err != nil {
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
	var dueChange sql.NullString
	if ev.DueChange != nil {
		b, err := json.Marshal(ev.DueChange)
		if err != nil {
			return envelope.TaskEvent{}, err
		}
		dueChange = sql.NullString{String: string(b), Valid: true}
	}
	res, err := x.ExecContext(ctx, `INSERT INTO task_events (task_id, actor, kind, body, status, created_at, chain, for_owner, prior, due_change)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ? WHERE EXISTS (SELECT 1 FROM tasks WHERE id = ?)`,
		ev.TaskID, ev.Actor, ev.Kind, ev.Body, string(ev.Status), at, chain, ev.ForOwner, string(ev.Prior), dueChange, ev.TaskID)
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

func scanTask(sc scanner) (envelope.Task, error) { return scanTaskAnd(sc) }

// scanTaskAnd reads the columns of taskCols into a task, and the columns a
// query selects after them into more.
func scanTaskAnd(sc scanner, more ...any) (envelope.Task, error) {
	var t envelope.Task
	var status, ask string
	var created int64
	if err := sc.Scan(append([]any{&t.ID, &t.Title, &t.Detail, &status, &t.Filer, &t.Assignee, &t.Key, &t.Result, &t.ResultRequest, &t.CurrentRequest, &t.AgentAssigns, &t.Source, &t.SuggestedAssignee, &created, &t.DueDay, &t.DueAt, &ask, &t.RepeatSchedule, &t.RepeatEverySeconds, &t.RepeatTo}, more...)...); err != nil {
		return envelope.Task{}, err
	}
	t.Status, t.AskStatus, t.CreatedAt = envelope.TaskStatus(status), envelope.Status(ask), time.UnixMilli(created).UTC()
	return t, nil
}

const taskEventCols = `id, task_id, actor, kind, body, status, created_at, COALESCE(chain, ''), for_owner, COALESCE(prior, ''), COALESCE(due_change, '')`

func scanTaskEvent(sc scanner) (envelope.TaskEvent, error) {
	var ev envelope.TaskEvent
	var status, chain, prior, dueChange string
	var at int64
	if err := sc.Scan(&ev.ID, &ev.TaskID, &ev.Actor, &ev.Kind, &ev.Body, &status, &at, &chain, &ev.ForOwner, &prior, &dueChange); err != nil {
		return envelope.TaskEvent{}, err
	}
	if chain != "" {
		if err := json.Unmarshal([]byte(chain), &ev.Chain); err != nil {
			return envelope.TaskEvent{}, err
		}
	}
	if dueChange != "" {
		if err := json.Unmarshal([]byte(dueChange), &ev.DueChange); err != nil {
			return envelope.TaskEvent{}, err
		}
	}
	ev.Status, ev.Prior, ev.At = envelope.TaskStatus(status), envelope.TaskStatus(prior), time.UnixMilli(at).UTC()
	return ev, nil
}

// TaskRelay is the actor a task's history names for a change the relay
// made itself: an expiry, a removal, a move.
const TaskRelay = "relay"
