package store

import (
	"context"
	"strconv"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Task notices: news of a task for the owner's task hosts. Four changes
// are news: the task came back with a reply (returned), its assignee asked
// a question (question), its ask ended with no result so it is open again
// (open), and it was filed to wait for the owner (proposed). Each writes a
// task_notices row in the transaction that writes the task's status entry,
// so a change the store committed always has its row and one it rolled back
// has none. The relay reads the rows afterwards, queues a notice from the
// relay for each host that should hear, and deletes the row.

// NoteTask is the notice_kind of a task notice. Its notice_for is the task's
// id in decimal, which is how the one unread notice per task and host is
// found.
const NoteTask = "task"

// TaskNotice is one task change waiting to be told to the task hosts.
type TaskNotice struct {
	ID     int64
	TaskID int64
	// Status is the status the change gave the task.
	Status envelope.TaskStatus
	// Actor is who made the change: an agent, envelope.TaskOwner or
	// TaskRelay.
	Actor string
	// Asker is the sender of the ask the change ended or paused, "" for a
	// task filed as proposed, which has no ask.
	Asker string
}

// migrateTaskNotices creates the task_notices outbox and task_notice_told,
// which names the hosts a row has been settled for: told, or left alone
// because an unread notice about the task already waits for them. Neither
// has a foreign key to agents, since a rebind re-inserts the agent row. It
// is a no-op when the tables exist.
func (s *Store) migrateTaskNotices() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS task_notices (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  status     TEXT NOT NULL,
  actor      TEXT NOT NULL,
  asker      TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS task_notice_told (
  notice_id INTEGER NOT NULL,
  host      TEXT NOT NULL,
  PRIMARY KEY (notice_id, host)
);`)
	return err
}

// taskNoticeTx writes the outbox row for a change x, the transaction that
// just wrote task id's status entry, made: the task now has status, by
// actor, and asker sent the ask the change is about.
func taskNoticeTx(ctx context.Context, x execer, id int64, status envelope.TaskStatus, actor, asker string, at int64) error {
	_, err := x.ExecContext(ctx, `INSERT INTO task_notices (task_id, status, actor, asker, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, string(status), actor, asker, at)
	return err
}

// taskAskNoticeTx is taskNoticeTx for a change that came through request
// reqID, an attempt at the task: the asker is that request's sender. One
// statement, which writes nothing for a request that is no attempt at a
// task.
func taskAskNoticeTx(ctx context.Context, x execer, reqID string, status envelope.TaskStatus, actor string, at int64) error {
	_, err := x.ExecContext(ctx, `INSERT INTO task_notices (task_id, status, actor, asker, created_at)
		SELECT task_id, ?, ?, from_agent, ? FROM requests WHERE id = ? AND task_id IS NOT NULL`,
		string(status), actor, at, reqID)
	return err
}

// TaskNotices returns up to limit task changes not yet told to the hosts,
// oldest first.
func (s *Store) TaskNotices(ctx context.Context, limit int) ([]TaskNotice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, status, actor, asker FROM task_notices ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskNotice
	for rows.Next() {
		var n TaskNotice
		var status string
		if err := rows.Scan(&n.ID, &n.TaskID, &status, &n.Actor, &n.Asker); err != nil {
			return nil, err
		}
		n.Status = envelope.TaskStatus(status)
		out = append(out, n)
	}
	return out, rows.Err()
}

// EnqueueTaskNotice queues req, the relay's notice of change n for the host
// req.To, and records that n is settled for that host, in one transaction.
// It reports false, queuing nothing, in two cases. The host is already
// settled for n, so a pass over n that failed part way and runs again never
// tells a host twice. Or an unread notice about the same task already waits
// for the host: the host will read the task as it is now, so this change
// adds none and nothing is kept for later.
func (s *Store) EnqueueTaskNotice(ctx context.Context, n TaskNotice, req envelope.Request, ttl time.Duration) (envelope.Request, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO task_notice_told (notice_id, host) VALUES (?, ?) ON CONFLICT DO NOTHING`, n.ID, req.To)
	if err != nil {
		return envelope.Request{}, false, err
	}
	if told, err := res.RowsAffected(); err != nil || told == 0 {
		return envelope.Request{}, false, err
	}
	req.NoticeFor, req.NoticeKind = strconv.FormatInt(n.TaskID, 10), NoteTask
	var unread bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requests WHERE notice_for = ? AND notice_kind = ? AND to_agent = ? AND status = ? AND expires_at > ?)`,
		req.NoticeFor, NoteTask, req.To, string(envelope.StatusQueued), s.now().UnixMilli()).Scan(&unread); err != nil {
		return envelope.Request{}, false, err
	}
	if unread {
		return envelope.Request{}, false, tx.Commit()
	}
	if req, err = s.enqueueTx(ctx, tx, req, ttl); err != nil {
		return envelope.Request{}, false, err
	}
	return req, true, tx.Commit()
}

// DeleteTaskNotice removes change id from the outbox, with the record of
// the hosts it was settled for, once every host that should hear has been
// queued a notice.
func (s *Store) DeleteTaskNotice(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_notice_told WHERE notice_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_notices WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
