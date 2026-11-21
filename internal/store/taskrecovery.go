package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const maxRecoveredTasks = 100

// Requests already closed at migration time are historical. Open asks still
// qualify when they end, including questions already waiting at upgrade time.
func (s *Store) migrateTaskRecovery() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name = 'task_recovery_requests')`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = tx.Exec(`
ALTER TABLE requests ADD COLUMN recovery_source TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN skip_task_recovery INTEGER NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN recovery_repeat INTEGER NOT NULL DEFAULT 0;
CREATE TABLE task_recovery_requests (
  request_id TEXT PRIMARY KEY REFERENCES requests(id) ON DELETE CASCADE,
  waiting_since INTEGER
);
CREATE INDEX task_recovery_due ON task_recovery_requests(waiting_since);
WITH RECURSIVE room_chain(id) AS (
  SELECT id FROM requests WHERE room_post_id IS NOT NULL
  UNION
  SELECT r.id FROM requests r JOIN room_chain c ON c.id = r.parent_id
)
UPDATE requests SET skip_task_recovery = 1
WHERE id IN (SELECT id FROM room_chain) OR (
  kind = 'ask' AND status IN ('held', 'queued', 'delivered', 'claimed', 'needs_input') AND (
    group_id GLOB 'pass-*'
    OR EXISTS (SELECT 1 FROM agents WHERE name IN (from_agent, to_agent) AND kind IN ('history', 'notes', 'pass', 'council'))
  )
);
UPDATE requests SET recovery_repeat = schedules.id FROM schedules
WHERE requests.id = schedules.last_request AND schedules.every_s > 0;
INSERT INTO task_recovery_requests
SELECT id, updated_at FROM requests WHERE status = 'needs_input' AND kind = 'ask'
  AND task_id IS NULL AND from_agent != 'relay' AND skip_task_recovery = 0;
CREATE TRIGGER requests_task_recovery AFTER UPDATE OF status ON requests
WHEN OLD.status != NEW.status AND NEW.kind = 'ask' AND NEW.task_id IS NULL AND NEW.from_agent != 'relay'
BEGIN
  DELETE FROM task_recovery_requests WHERE request_id = NEW.id;
  INSERT INTO task_recovery_requests
  SELECT NEW.id, CASE WHEN NEW.status = 'needs_input' THEN NEW.updated_at END
  WHERE NEW.skip_task_recovery = 0 AND NEW.status IN ('needs_input', 'failed', 'declined', 'expired', 'cancelled')
    -- Removing a teammate cancels its request before deleting its schedule.
    AND (NEW.status IN ('needs_input', 'cancelled') OR NOT EXISTS (SELECT 1 FROM schedules WHERE id = NEW.recovery_repeat));
END;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverTasks files a bounded batch from the durable outcome queue. The
// request state, task, audit and notice are committed together. A stale
// clarification is closed here, so an answer and a filing cannot both win.
func (s *Store) RecoverTasks(ctx context.Context, questionAfter time.Duration) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var available int
	if err := tx.QueryRowContext(ctx, `SELECT ? - COUNT(*) FROM tasks WHERE filer = ? AND `+taskOpenStatuses,
		maxRecoveredTasks, envelope.TaskRelayFiler).Scan(&available); err != nil || available <= 0 {
		return nil, err
	}
	now := s.now()
	ids, err := recoveryIDs(ctx, tx, now.Add(-questionAfter).UnixMilli(), min(available, 20))
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var expired []string
	for _, id := range ids {
		req, status, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id = ?`, id))
		if err != nil {
			return nil, err
		}
		reason := string(status)
		if status == envelope.StatusNeedsInput {
			reason = "clarification timed out"
			if _, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'expired', lease_paused = 0, updated_at = ? WHERE id = ?`, now.UnixMilli(), id); err != nil {
				return nil, err
			}
			expired = append(expired, id)
		}
		switch {
		case status == envelope.StatusDeclined && req.WasHeld && !req.Approved:
			reason = "denied by the owner"
		case status == envelope.StatusCancelled:
			reason = "cancelled by the relay"
		case status == envelope.StatusExpired && req.WasHeld && !req.Approved:
			reason = "expired while held for approval"
		}
		if err := recoverTaskTx(ctx, tx, req, reason, now.UnixMilli()); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_recovery_requests WHERE request_id = ?`, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return expired, nil
}

func recoveryIDs(ctx context.Context, tx *sql.Tx, before int64, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.request_id FROM task_recovery_requests p JOIN requests r ON r.id = p.request_id
		WHERE (p.waiting_since IS NULL OR p.waiting_since <= ?) AND NOT EXISTS (SELECT 1 FROM schedules WHERE id = r.recovery_repeat)
		ORDER BY p.waiting_since, p.rowid LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func recoverTaskTx(ctx context.Context, tx *sql.Tx, req envelope.Request, reason string, now int64) error {
	source := req.RecoverySource
	switch {
	case req.FromGuest:
		source = "guest"
	case source == "" && req.WasHeld && !req.Approved:
		source = "held-request"
	}
	status := envelope.TaskOpen
	if source != "" {
		status = envelope.TaskProposed
	}
	first := req.To
	if len(req.MovedFrom) > 0 {
		first = req.MovedFrom[0]
	}
	title := recoveredTaskTitle(req.Body, first)
	res, err := tx.ExecContext(ctx, `INSERT INTO tasks (title, detail, status, filer, dedupe_key, source, named_for, created_at)
		VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?)`, title, req.Body, status, envelope.TaskRelayFiler, req.ID, source, req.From, now)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	body := fmt.Sprintf("Request %s from %s to %s ended without a result: %s.", req.ID, req.From, first, reason)
	if first != req.To {
		body += fmt.Sprintf(" Final target: %s.", req.To)
	}
	chain := append(req.Chain, req.Contributors...)
	chain = append(chain, req.From)
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{TaskID: id, Actor: envelope.TaskRelayFiler, Kind: envelope.TaskEventAdded,
		Body: body, Status: status, Chain: chain}, now); err != nil {
		return err
	}
	if status == envelope.TaskOpen {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_claimants(task_id, agent) VALUES (?, ?)`, id, req.From); err != nil {
			return err
		}
	}
	if err := taskNoticeTx(ctx, tx, id, status, envelope.TaskRelayFiler, "", now); err != nil {
		return err
	}
	// Keep the audit row out of request traces, whose readers need not
	// have access to this task.
	return auditTx(ctx, tx, AuditEvent{At: time.UnixMilli(now).UTC(), Event: "task_auto_filed", RequestID: req.ID, Actor: TaskRelay,
		Detail: DetailJSON(map[string]any{"task": id, "status": status, "reason": reason, "from": req.From, "original_to": first, "to": req.To})})
}

func recoveredTaskTitle(body, target string) string {
	body = strings.TrimSpace(strings.TrimPrefix(body, envelope.GuestLine+"\n"))
	line, _, _ := strings.Cut(body, "\n")
	if envelope.IsThreadingLine(line) {
		_, rest, _ := strings.Cut(body, "\n")
		line, _, _ = strings.Cut(strings.TrimSpace(rest), "\n")
	}
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return "Unfinished ask to " + target
	}
	if len(line) <= 200 {
		return line
	}
	end := 197
	for !utf8.RuneStart(line[end]) {
		end--
	}
	return line[:end] + "…"
}
