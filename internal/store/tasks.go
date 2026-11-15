package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// MaxOpenTasks caps the tasks one agent filed that are not done or dropped.
const MaxOpenTasks = 50

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
)

// migrateTasks creates the tasks, task_events and task_claimants tables and
// the requests.task_id link. Tasks name agents with no foreign key to
// agents, since a rebind re-inserts the agent row. A task is never deleted,
// so its id only ever names one task. current_request is the one ask open
// under the task, empty for none; result_request is the ask that produced
// result. task_events is the task's history, read in id order, which is
// also time order. task_claimants holds every agent that claimed an ask
// under the task, which is what lets it keep reading the task afterwards.
func (s *Store) migrateTasks() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
  id              INTEGER PRIMARY KEY,
  title           TEXT NOT NULL,
  detail          TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL,
  filer           TEXT NOT NULL,
  assignee        TEXT NOT NULL DEFAULT '',
  dedupe_key      TEXT NOT NULL DEFAULT '',
  result          TEXT NOT NULL DEFAULT '',
  result_request  TEXT NOT NULL DEFAULT '',
  current_request TEXT NOT NULL DEFAULT '',
  agent_assigns   INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL
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
);`)
	if err != nil {
		return err
	}
	// task_id marks an ask that is an attempt at a task: the task it is
	// for, NULL for every other request.
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'task_id')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN task_id INTEGER`); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS requests_task ON requests(task_id) WHERE task_id IS NOT NULL`)
	return err
}

// taskSelect reads tasks with the status of the ask open under each, which
// is not copied onto the task.
const taskSelect = `SELECT t.id, t.title, t.detail, t.status, t.filer, t.assignee, t.dedupe_key, t.result, t.result_request, t.current_request, t.agent_assigns, t.created_at, COALESCE(q.status, '')
	FROM tasks t LEFT JOIN requests q ON q.id = t.current_request`

// taskVisible is true for the tasks viewer ?1 may read; an empty
// viewer is an admin's, every task. An agent reads a task it filed. It reads any other task only
// once it is past proposed, and then when the ask open under it is
// addressed to the agent and not held for the owner, when the agent claimed
// an ask under it, or when it is open, unassigned and carries no result.
const taskVisible = `(?1 = '' OR t.filer = ?1 OR t.status != 'proposed' AND (
		q.to_agent = ?1 AND q.status != 'held'
		OR EXISTS (SELECT 1 FROM task_claimants c WHERE c.task_id = t.id AND c.agent = ?1)
		OR t.status = 'open' AND t.assignee = '' AND t.result = '' AND t.result_request = ''))`

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
// proposed, and starts its history. A task with a key the same filer
// already has on a task that is not done or dropped is not stored: that
// task is returned, with false. An agent with MaxOpenTasks such tasks gets
// ErrTaskLimit; the owner has no limit. Both checks run in the insert's
// transaction, after its first write, so no other AddTask lands between
// them.
func (s *Store) AddTask(ctx context.Context, t envelope.Task) (envelope.Task, bool, error) {
	if t.Status == "" {
		t.Status = envelope.TaskOpen
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Task{}, false, err
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()
	res, err := tx.ExecContext(ctx, `INSERT INTO tasks (title, detail, status, filer, assignee, dedupe_key, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Title, t.Detail, string(t.Status), t.Filer, t.Assignee, t.Key, now)
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
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: id, Actor: t.Filer, Kind: envelope.TaskEventAdded, Status: t.Status}, now); err != nil {
		return envelope.Task{}, false, err
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
// after, oldest first; viewer "" is an admin's, every task.
func (s *Store) Tasks(ctx context.Context, viewer string, after int64, limit int) ([]envelope.Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelect+` WHERE `+taskVisible+` AND t.id > ?2 ORDER BY t.id LIMIT ?3`, viewer, after, limit)
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

// claimTaskTx gives the task to req, an ask being written in tx as an
// attempt at it: the task's one open ask becomes req, its assignee req's
// target, and it is working. The update is conditional on the task having
// no open ask and being open or returned, so of any number of asks racing
// for a task one is stored; the others, and an ask under a task that is
// missing or in any other status, get ErrTaskBusy.
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
	_, err = addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: req.TaskID, Actor: req.From, Kind: envelope.TaskEventAssigned, Body: req.To, Status: envelope.TaskWorking}, s.now().UnixMilli())
	return err
}

// SetTaskStatus moves task id to status to and records it in the task's
// history as actor's change, with note. With from, it does so only while
// the task is in one of those statuses, and returns ErrTaskState otherwise:
// the check and the change are one statement, so an ask sent under the task
// meanwhile either lands first and refuses the change, or finds the new
// status.
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

// addTaskEvent writes one history entry. The task check and the insert are
// one statement.
func addTaskEvent(ctx context.Context, x execer, ev envelope.TaskEvent, at int64) (envelope.TaskEvent, error) {
	res, err := x.ExecContext(ctx, `INSERT INTO task_events (task_id, actor, kind, body, status, created_at)
		SELECT ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM tasks WHERE id = ?)`,
		ev.TaskID, ev.Actor, ev.Kind, ev.Body, string(ev.Status), at, ev.TaskID)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, actor, kind, body, status, created_at FROM task_events WHERE task_id = ? AND id > ? ORDER BY id LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []envelope.TaskEvent{}
	for rows.Next() {
		var ev envelope.TaskEvent
		var status string
		var at int64
		if err := rows.Scan(&ev.ID, &ev.TaskID, &ev.Actor, &ev.Kind, &ev.Body, &status, &at); err != nil {
			return nil, err
		}
		ev.Status, ev.At = envelope.TaskStatus(status), time.UnixMilli(at).UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// RecordTaskClaimant notes that agent claimed an ask under task id, so the
// agent can read the task from then on. Recording it again changes nothing.
func (s *Store) RecordTaskClaimant(ctx context.Context, id int64, agent string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_claimants (task_id, agent) VALUES (?, ?) ON CONFLICT DO NOTHING`, id, agent)
	return err
}

func scanTask(sc scanner) (envelope.Task, error) {
	var t envelope.Task
	var status, ask string
	var created int64
	if err := sc.Scan(&t.ID, &t.Title, &t.Detail, &status, &t.Filer, &t.Assignee, &t.Key, &t.Result, &t.ResultRequest, &t.CurrentRequest, &t.AgentAssigns, &created, &ask); err != nil {
		return envelope.Task{}, err
	}
	t.Status, t.AskStatus, t.CreatedAt = envelope.TaskStatus(status), envelope.Status(ask), time.UnixMilli(created).UTC()
	return t, nil
}
