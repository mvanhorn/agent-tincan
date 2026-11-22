package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const DefaultTaskReturnedCloseAfter = time.Hour

// returnedHostResult joins the latest return and the ask it answered. An
// old return has no snapshot, so use the current host mark for that row.
const returnedHostResult = `JOIN requests result ON result.id = t.result_request AND result.task_id = t.id
	JOIN task_events returned ON returned.id = (
		SELECT MAX(e.id) FROM task_events e WHERE e.task_id = t.id AND e.kind = 'status' AND e.status = 'returned'
	)`

const returnedHostActivity = `LEFT JOIN task_events activity ON activity.id = (
	SELECT MAX(e.id) FROM task_events e WHERE e.task_id = t.id AND e.id > returned.id
	AND e.actor = result.from_agent AND e.kind IN ('asked', 'comment')
)`

const returnedHostTask = `t.status = 'returned' AND t.current_request = '' AND t.quiet = 0
	AND t.filer != '@owner' AND result.from_agent NOT IN ('', '@owner', 'relay', returned.actor)
	AND result.from_guest = 0 AND result.status = 'answered' AND result.held_ack = ''
	AND returned.prior IS NULL AND COALESCE(returned.returned_by_host,
		EXISTS (SELECT 1 FROM agents WHERE name = returned.actor AND task_host = 1))
	AND t.room_post_id IS NULL AND t.participant_only = 0 AND t.source IS NULL
	AND NOT ` + taskNeverApproved + ` AND NOT ` + taskOriginalAskUnapproved + `
	AND NOT EXISTS (SELECT 1 FROM schedules WHERE task_id = t.id)`

// SweepReturnedHostTasks asks hosts to review each result after a quiet
// window. With no host available it closes the task, preserving the result
// and leaving a history entry. The history read and the writes share a
// transaction so a follow-up or new assignment cannot race that closure.
func (s *Store) SweepReturnedHostTasks(ctx context.Context, after time.Duration, hostAvailable bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	rows, err := tx.QueryContext(ctx, `SELECT t.id, COALESCE(activity.id, returned.id), returned.actor, result.from_agent
		FROM tasks t `+returnedHostResult+` `+returnedHostActivity+`
		WHERE `+returnedHostTask+`
		AND COALESCE(activity.created_at, returned.created_at) <= ?
		AND (NOT ? OR t.returned_close_notified < COALESCE(activity.id, returned.id))
		AND NOT EXISTS (
			SELECT 1 FROM task_followups f JOIN requests r ON r.id = f.request_id WHERE f.task_id = t.id
			AND r.status IN ('queued', 'delivered', 'claimed', 'held', 'needs_input')
		)
		ORDER BY COALESCE(activity.created_at, returned.created_at), t.id LIMIT 20`, now.Add(-after).UnixMilli(), hostAvailable)
	if err != nil {
		return err
	}
	defer rows.Close()
	type candidate struct {
		task, activity int64
		actor, asker   string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.task, &c.activity, &c.actor, &c.asker); err != nil {
			return err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range candidates {
		// Replace any notice issued before the latest follow-up, or settle
		// the prior notice if the host has become unavailable.
		if err := s.cancelTaskNoticesTx(ctx, tx, c.task, envelope.TaskNoticeReturnedClose, ""); err != nil {
			return err
		}
		if !hostAvailable {
			if err := s.closeReturnedHostTaskTx(ctx, tx, c.task, now.UnixMilli()); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_notices (task_id, status, actor, asker, created_at, kind)
			VALUES (?, 'returned', ?, ?, ?, ?)`, c.task, c.actor, c.asker, now.UnixMilli(), envelope.TaskNoticeReturnedClose); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET returned_close_notified = ? WHERE id = ?`, c.activity, c.task); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) closeReturnedHostTaskTx(ctx context.Context, tx *sql.Tx, id, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'done' WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{
		TaskID: id, Actor: TaskRelay, Kind: envelope.TaskEventStatus, Status: envelope.TaskDone,
		Body: "returned, asker did not follow up",
	}, now); err != nil {
		return err
	}
	return s.closeTaskReminderTx(ctx, tx, id)
}
