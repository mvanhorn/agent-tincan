package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// TaskBriefing contains only metadata safe to send through task notices.
type TaskBriefing struct {
	Due, Waiting, Done []BriefingTask
	Missed             []BriefingNotice
	Suggested          int64
}

type BriefingTask struct {
	ID       int64
	Status   envelope.TaskStatus
	Due      envelope.TaskDue
	Assignee string
}

type BriefingNotice struct {
	TaskID int64
	Kind   string
	Status envelope.TaskStatus
}

// QueueTaskBriefing snapshots the list and queues a briefing through the
// outbox. An empty scheduledDay means an on-demand briefing, which leaves
// the automatic schedule and its event cursor alone.
func (s *Store) QueueTaskBriefing(ctx context.Context, scheduledDay, today string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var lastDay string
	var lastAt, afterEvent int64
	if err := tx.QueryRowContext(ctx, `SELECT last_day, last_at, after_event FROM task_briefing_state WHERE id = 1`).Scan(&lastDay, &lastAt, &afterEvent); err != nil {
		return false, err
	}
	if scheduledDay != "" && lastDay >= scheduledDay {
		return false, nil
	}

	now := s.now()
	var since int64
	if lastAt == 0 {
		since = now.Add(-24 * time.Hour).UnixMilli()
	}
	briefing, latestDone, err := readTaskBriefing(ctx, tx, today, now, since, afterEvent)
	if err != nil {
		return false, err
	}
	queued := len(briefing.Due)+len(briefing.Waiting)+len(briefing.Done)+len(briefing.Missed) > 0 || briefing.Suggested != 0
	if queued {
		payload, err := json.Marshal(briefing)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_notices (kind, status, actor, created_at, briefing) VALUES (?, '', ?, ?, ?)`,
			envelope.TaskNoticeBriefing, TaskRelay, now.UnixMilli(), string(payload)); err != nil {
			return false, err
		}
		lastAt, afterEvent = now.UnixMilli(), latestDone
	}
	if scheduledDay != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE task_briefing_state SET last_day = ?, last_at = ?, after_event = ? WHERE id = 1`,
			scheduledDay, lastAt, afterEvent); err != nil {
			return false, err
		}
	}
	return queued, tx.Commit()
}

const briefingTaskCols = `t.id, t.status, COALESCE(t.due_day, ''), t.due_at, t.assignee`

func readTaskBriefing(ctx context.Context, tx *sql.Tx, today string, now time.Time, since, afterEvent int64) (b TaskBriefing, latestDone int64, err error) {
	b.Due, err = briefingTasks(ctx, tx, `SELECT `+briefingTaskCols+` FROM tasks t
		WHERE t.status NOT IN ('done', 'dropped') AND t.due_day <= ?
		ORDER BY t.due_day, COALESCE(t.due_at, 9223372036854775807), t.id LIMIT 3`, today)
	if err != nil {
		return b, 0, err
	}
	b.Waiting, err = briefingTasks(ctx, tx, `SELECT `+briefingTaskCols+` FROM tasks t
		WHERE t.status IN ('returned', 'question', 'proposed')
		ORDER BY COALESCE((SELECT MAX(e.id) FROM task_events e WHERE e.task_id = t.id AND e.status = t.status AND e.kind IN ('added', 'status')), t.id), t.id`)
	if err != nil {
		return b, 0, err
	}
	b.Done, err = briefingTasks(ctx, tx, `SELECT `+briefingTaskCols+` FROM tasks t
		WHERE t.status = 'done' AND EXISTS (
			SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.kind = 'status' AND e.status = 'done'
			AND e.id > ? AND e.created_at >= ?
		) ORDER BY t.id`, afterEvent, since)
	if err != nil {
		return b, 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM task_events WHERE kind = 'status' AND status = 'done'`).Scan(&latestDone); err != nil {
		return b, 0, err
	}
	b.Missed, err = missedBriefingNotices(ctx, tx, now.Add(-24*time.Hour).UnixMilli())
	if err != nil {
		return b, 0, err
	}
	if len(b.Due) > 0 {
		b.Suggested = b.Due[0].ID
		return b, latestDone, nil
	}
	var earliest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT id FROM tasks
		WHERE status NOT IN ('done', 'dropped') AND due_day > ?
		ORDER BY due_day, COALESCE(due_at, 9223372036854775807), id LIMIT 1)`, today).Scan(&earliest); err != nil {
		return b, 0, err
	}
	b.Suggested = earliest.Int64
	if !earliest.Valid {
		for _, task := range b.Waiting {
			if task.Status == envelope.TaskReturned {
				b.Suggested = task.ID
				break
			}
		}
	}
	return b, latestDone, nil
}

func briefingTasks(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]BriefingTask, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []BriefingTask
	for rows.Next() {
		var task BriefingTask
		if err := rows.Scan(&task.ID, &task.Status, &task.Due.DueDay, &task.Due.DueAt, &task.Assignee); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func missedBriefingNotices(ctx context.Context, tx *sql.Tx, since int64) ([]BriefingNotice, error) {
	rows, err := tx.QueryContext(ctx, `SELECT CAST(r.notice_for AS INTEGER), r.notice_kind, d.reported_status
		FROM task_notice_delivery d JOIN requests r ON r.id = d.request_id
		WHERE d.outcome IN ('', 'exhausted') AND r.created_at >= ?
		GROUP BY r.notice_for, r.notice_kind, d.reported_status
		ORDER BY MIN(r.created_at), MIN(r.rowid)`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notices []BriefingNotice
	for rows.Next() {
		var n BriefingNotice
		if err := rows.Scan(&n.TaskID, &n.Kind, &n.Status); err != nil {
			return nil, err
		}
		notices = append(notices, n)
	}
	return notices, rows.Err()
}
