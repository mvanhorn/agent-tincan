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

// One snapshot is shared by all hosts, so it uses the withheld view even
// when an individual host happens to be the filer.
const briefingDeadlineVisible = `(t.status != 'proposed' AND NOT ` + taskNeverApproved + `)`
const briefingTaskCols = `t.id, t.status, COALESCE(t.due_day, ''), t.due_at, t.assignee, ` + briefingDeadlineVisible

func readTaskBriefing(ctx context.Context, tx *sql.Tx, today string, now time.Time, since, afterEvent int64) (b TaskBriefing, latestDone int64, err error) {
	b.Due, err = briefingTasks(ctx, tx, `SELECT `+briefingTaskCols+` FROM tasks t
		WHERE t.status NOT IN ('done', 'dropped') AND t.due_day <= ? AND `+briefingDeadlineVisible+`
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
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT t.id FROM tasks t
		WHERE t.status NOT IN ('done', 'dropped') AND t.due_day > ? AND `+briefingDeadlineVisible+`
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
		var visible bool
		if err := rows.Scan(&task.ID, &task.Status, &task.Due.DueDay, &task.Due.DueAt, &task.Assignee, &visible); err != nil {
			return nil, err
		}
		if !visible {
			task.Due = envelope.TaskDue{}
			task.Assignee = ""
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func missedBriefingNotices(ctx context.Context, tx *sql.Tx, since int64) ([]BriefingNotice, error) {
	// A receipt confirms the original notice across hosts and retries. Old
	// deliveries lack an outbox ID, so match their fixed text and metadata
	// within the briefing window.
	rows, err := tx.QueryContext(ctx, `SELECT CAST(r.notice_for AS INTEGER), r.notice_kind, d.reported_status
		FROM task_notice_delivery d JOIN requests r ON r.id = d.request_id
		JOIN agents h ON h.name = r.to_agent AND h.task_host = 1
		LEFT JOIN tasks t ON t.id = CAST(r.notice_for AS INTEGER)
		WHERE d.outcome IN ('', 'exhausted') AND r.created_at >= ?
		AND (r.notice_kind = ? OR t.`+taskOpenStatuses+`)
		AND NOT EXISTS (
			SELECT 1 FROM task_notice_delivery ack
			WHERE ack.outcome = 'told' AND ack.notice_id = d.notice_id
		)
		AND (d.notice_id IS NOT NULL OR NOT EXISTS (
			SELECT 1 FROM task_notice_delivery ack JOIN requests receipt ON receipt.id = ack.request_id
			WHERE ack.outcome = 'told' AND ack.notice_id IS NULL AND receipt.created_at >= ?
			AND receipt.notice_kind = r.notice_kind AND receipt.notice_for = r.notice_for
			AND ack.reported_status = d.reported_status AND receipt.body = r.body
		))
		GROUP BY r.notice_for, r.notice_kind, d.reported_status
		ORDER BY MIN(r.created_at), MIN(r.rowid)`, since, envelope.TaskNoticeBriefing, since)
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
