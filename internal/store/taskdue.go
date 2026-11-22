package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

var ErrTaskDueFiler = errors.New("only the owner or the agent that filed this task can change its due date")

// SetTaskDue changes the due fields and writes the history entry in the
// same transaction. A changed time makes the new deadline eligible to ring.
func (s *Store) SetTaskDue(ctx context.Context, id int64, due envelope.TaskDue, actor, words string, forOwner bool) (envelope.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Task{}, err
	}
	defer tx.Rollback()
	t, err := getTask(ctx, tx, id, "")
	if err != nil {
		return envelope.Task{}, err
	}
	if actor != envelope.TaskOwner && !forOwner && t.Filer != actor {
		return envelope.Task{}, ErrTaskDueFiler
	}
	var prior sql.NullInt64
	if t.DueAt != nil {
		prior = sql.NullInt64{Int64: *t.DueAt, Valid: true}
	}
	var next sql.NullInt64
	if due.DueAt != nil {
		next = sql.NullInt64{Int64: *due.DueAt, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET due_day = NULLIF(?, ''), due_at = ?,
		reminded_at = CASE WHEN due_at IS ? THEN reminded_at END,
		reminder_dropped_at = CASE WHEN due_at IS ? THEN reminder_dropped_at END WHERE id = ?`,
		due.DueDay, next, next, next, id); err != nil {
		return envelope.Task{}, err
	}
	if prior != next {
		if err := s.cancelTaskReminderTx(ctx, tx, id); err != nil {
			return envelope.Task{}, err
		}
	}
	ev := envelope.TaskEvent{TaskID: id, Actor: actor, Kind: envelope.TaskEventDue, Body: words, ForOwner: forOwner,
		DueChange: &envelope.TaskDueChange{Before: t.TaskDue, After: due}}
	if _, err := addTaskEvent(ctx, tx, ev, s.now().UnixMilli()); err != nil {
		return envelope.Task{}, err
	}
	t.TaskDue = due
	return t, tx.Commit()
}

const reminderLateness = 6 * time.Hour

// QueueTaskReminders stages due reminders and records deadlines missed by
// more than six hours. EnqueueTaskNotice stamps reminded_at only when the
// first host's request is durable.
func (s *Store) QueueTaskReminders(ctx context.Context, haveHosts bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	cutoff := now.Add(-reminderLateness).Unix()
	rows, err := tx.QueryContext(ctx, `UPDATE tasks SET reminder_dropped_at = ?
		WHERE due_at < ? AND reminded_at IS NULL AND reminder_dropped_at IS NULL
		AND `+taskOpenStatuses+` RETURNING id`, now.Unix(), cutoff)
	if err != nil {
		return err
	}
	var late []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		late = append(late, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range late {
		if err := s.cancelTaskReminderTx(ctx, tx, id); err != nil {
			return err
		}
		if err := noteLateReminder(ctx, tx, id, now); err != nil {
			return err
		}
	}
	if haveHosts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_notices (task_id, kind, status, actor, created_at)
			SELECT id, ?, '', ?, ? FROM tasks
			WHERE due_at BETWEEN ? AND ? AND reminded_at IS NULL AND reminder_dropped_at IS NULL
			AND `+taskOpenStatuses+` AND NOT EXISTS (
				SELECT 1 FROM task_notices n WHERE n.task_id = tasks.id AND n.kind = ?
			) ORDER BY due_at, id`, envelope.TaskNoticeReminder, TaskRelay, now.UnixMilli(), cutoff, now.Unix(), envelope.TaskNoticeReminder); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func noteLateReminder(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	ev := envelope.TaskEvent{TaskID: id, Actor: TaskRelay, Kind: envelope.TaskEventReminderDropped, Body: "Reminder dropped because its due time passed more than six hours ago."}
	_, err := addTaskEvent(ctx, tx, ev, now.UnixMilli())
	return err
}

func (s *Store) closeTaskReminderTx(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET reminder_dropped_at = ? WHERE id = ?
		AND due_at IS NOT NULL AND reminded_at IS NULL AND reminder_dropped_at IS NULL`, s.now().Unix(), id); err != nil {
		return err
	}
	return s.cancelTaskReminderTx(ctx, tx, id)
}

// cancelTaskReminderTx removes staged work and settles outstanding host
// attempts so that changing or closing the task cannot generate a retry.
func (s *Store) cancelTaskReminderTx(ctx context.Context, tx *sql.Tx, id int64) error {
	return s.cancelTaskNoticesTx(ctx, tx, id, envelope.TaskNoticeReminder, NoteTaskReminderFallback)
}

func (s *Store) cancelTaskNoticesTx(ctx context.Context, tx *sql.Tx, id int64, kind, fallback string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_notice_told WHERE notice_id IN (
		SELECT id FROM task_notices WHERE kind = ? AND task_id = ?)`, kind, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_notices WHERE kind = ? AND task_id = ?`, kind, id); err != nil {
		return err
	}
	const attempts = `SELECT r.id FROM requests r JOIN task_notice_delivery d ON r.id = d.request_id
		WHERE r.notice_kind = ? AND r.notice_for = ? AND d.outcome = ''`
	task := strconv.FormatInt(id, 10)
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'cancelled', lease_until = 0, updated_at = ?
		WHERE (id IN (`+attempts+`) OR (notice_kind = NULLIF(?, '') AND notice_for = ?))
		AND status IN ('queued', 'delivered', 'claimed')`, s.now().UnixMilli(), kind, task, fallback, task); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE task_notice_delivery SET outcome = 'cancelled' WHERE request_id IN (`+attempts+`)`, kind, task)
	return err
}
