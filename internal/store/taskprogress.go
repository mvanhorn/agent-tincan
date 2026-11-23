package store

import (
	"context"
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const (
	DefaultTaskNudgeAfter = 15 * time.Minute
	DefaultTaskStallAfter = 30 * time.Minute
	NoteTaskNudge         = "task-nudge"
)

// Keep appointment dates and durations. Ignore clock readings and full
// date-time stamps commonly prefixed to otherwise identical updates.
const clockReading = `\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2}|[ \t]+(?:am|pm|utc|gmt|[pmec][sd]t))?`

var (
	progressTimestamp = regexp.MustCompile(`(?i)\b\d{4}-\d{2}-\d{2}[T ]` + clockReading + `\b`)
	progressClock     = regexp.MustCompile(`(?i)(^|\bas of[ \t]+|\bupdated at[ \t]+|\bchecked at[ \t]+)` + clockReading + `\b`)
)

func normalizeTaskProgress(note string) string {
	note = progressTimestamp.ReplaceAllString(note, "<time>")
	note = progressClock.ReplaceAllString(note, "${1}<time>")
	return strings.Join(strings.Fields(strings.ToLower(note)), " ")
}

// QueueTaskStalls queues each stage at most once for an attempt.
func (s *Store) QueueTaskStalls(ctx context.Context, nudgeAfter, stallAfter time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const working = ` FROM tasks t JOIN requests r ON r.id = t.current_request
		WHERE t.status = 'working' AND t.quiet != 1 AND r.status IN ('claimed', 'queued', 'delivered')`
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_progress_state (request_id, normalized, changed_at)
		SELECT r.id, '', r.queued_at`+working+` ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	now := s.now()
	for _, stage := range []struct {
		kind  string
		after time.Duration
		level int
	}{
		{NoteTaskNudge, nudgeAfter, 1},
		{envelope.TaskNoticeStall, stallAfter, 2},
	} {
		_, err = tx.ExecContext(ctx, `INSERT INTO task_notices (kind, task_id, status, actor, asker, created_at)
			SELECT ?1, t.id, 'working', r.to_agent, r.from_agent, ?2`+working+`
				AND EXISTS (SELECT 1 FROM task_progress_state p WHERE p.request_id = r.id AND p.notified < ?4
					AND p.changed_at <= ?3)
			ORDER BY r.claimed_at, t.id LIMIT 20`, stage.kind, now.UnixMilli(), now.Add(-stage.after).UnixMilli(), stage.level)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE task_progress_state SET notified = ?1, notified_at = ?3 WHERE notified < ?1 AND request_id IN (
			SELECT t.current_request FROM tasks t JOIN task_notices n ON n.task_id = t.id AND n.kind = ?2 AND n.created_at = ?3
		)`, stage.level, stage.kind, now.UnixMilli())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// cancelTaskNudgesTx removes assignee nudges that have become stale because
// the task left working or its assignee reported a new fact.
func (s *Store) cancelTaskNudgesTx(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_notice_told
		WHERE notice_id IN (SELECT id FROM task_notices WHERE task_id = ? AND kind = ?)`, id, NoteTaskNudge); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_notices WHERE task_id = ? AND kind = ?`, id, NoteTaskNudge); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'cancelled', lease_until = 0, updated_at = ?
		WHERE notice_kind = ? AND notice_for = ? AND status IN ('queued', 'delivered')`,
		s.now().UnixMilli(), NoteTaskNudge, strconv.FormatInt(id, 10))
	return err
}
