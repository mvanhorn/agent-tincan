package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func trackTaskNotice(ctx context.Context, tx *sql.Tx, req envelope.Request, attempt int) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO task_notice_delivery (request_id, attempt, reported_status) VALUES (?, ?, ?)`, req.ID, attempt, req.TaskNoticeStatus)
	return err
}

// TaskDelivery identifies one attempt to deliver a task notice to the owner.
type TaskDelivery struct {
	Request  envelope.Request
	Attempt  int
	CanRetry bool
}

// cancelTaskNoticesFor marks pending notices for fallback in the same
// transaction that takes away a host's mark or removes its identity.
func (s *Store) cancelTaskNoticesFor(ctx context.Context, tx *sql.Tx, host string) error {
	_, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'cancelled', lease_until = 0, updated_at = ?
		WHERE to_agent = ? AND status IN ('queued', 'delivered', 'claimed', 'expired')
		AND id IN (SELECT request_id FROM task_notice_delivery WHERE outcome = '')`, s.now().UnixMilli(), host)
	return err
}

// TaskNoticeClaimSession gives the session that first claimed this notice.
func (s *Store) TaskNoticeClaimSession(ctx context.Context, id string) (string, error) {
	var session string
	err := s.db.QueryRowContext(ctx, `SELECT claim_session FROM requests WHERE id = ?`, id).Scan(&session)
	return session, err
}

// TaskNoticesDue finds claims past the receipt deadline, expired or cancelled
// notices, and notices whose host is no longer eligible. Attempts and
// outcomes survive relay restarts.
func (s *Store) TaskNoticesDue(ctx context.Context, after time.Duration, limit int, eligible []string) ([]TaskDelivery, error) {
	hosts, err := json.Marshal(eligible)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH hosts AS (SELECT value FROM json_each(?) WHERE type = 'text')
		SELECT r.id, r.trace_id, r.to_agent, r.notice_for, r.notice_kind, r.body, d.attempt, d.reported_status,
			h.value IS NOT NULL AND r.status != 'cancelled'
		FROM task_notice_delivery d JOIN requests r ON r.id = d.request_id
		LEFT JOIN hosts h ON h.value = r.to_agent
		WHERE d.outcome = '' AND (h.value IS NULL OR r.status IN ('expired', 'cancelled')
			OR (r.status = 'claimed' AND r.claimed_at <= ?))
		ORDER BY r.created_at, r.id LIMIT ?`, string(hosts), s.now().Add(-after).UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var due []TaskDelivery
	for rows.Next() {
		var d TaskDelivery
		if err := rows.Scan(&d.Request.ID, &d.Request.TraceID, &d.Request.To, &d.Request.NoticeFor, &d.Request.NoticeKind, &d.Request.Body, &d.Attempt, &d.Request.TaskNoticeStatus, &d.CanRetry); err != nil {
			return nil, err
		}
		due = append(due, d)
	}
	return due, rows.Err()
}

// TellTaskNotices acknowledges the entire batch or none of it. A repeat
// after success is safe. Receipts from other agents, before claim, or for
// an attempt already replaced by a sweep cannot settle a notice.
func (s *Store) TellTaskNotices(ctx context.Context, agent string, ids []string) ([]TaskDelivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var told []TaskDelivery
	for _, id := range ids {
		d := TaskDelivery{Request: envelope.Request{ID: id}}
		var status, outcome string
		err := tx.QueryRowContext(ctx, `SELECT r.trace_id, r.notice_for, r.status, d.attempt, d.outcome
			FROM task_notice_delivery d JOIN requests r ON r.id = d.request_id
			WHERE r.id = ? AND r.to_agent = ?`, id, agent).Scan(&d.Request.TraceID, &d.Request.NoticeFor, &status, &d.Attempt, &outcome)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("task notice %s: %w", id, ErrNotFound)
		case err != nil:
			return nil, err
		case outcome == "told":
			continue
		case status != string(envelope.StatusClaimed) || outcome != "":
			return nil, fmt.Errorf("task notice %s is not awaiting a receipt: %w", id, ErrWrongState)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_notice_delivery SET outcome = 'told' WHERE request_id = ?`, id); err != nil {
			return nil, err
		}
		told = append(told, d)
	}
	return told, tx.Commit()
}

// AdvanceTaskNotice settles the old attempt and queues its retry or fallback
// in one transaction. If a receipt or another sweep won the race, it does
// nothing. A retry also yields to a concurrent cancellation. Fallback closes
// any still-active request, even when no fallback recipient is configured.
func (s *Store) AdvanceTaskNotice(ctx context.Context, d TaskDelivery, next envelope.Request, ttl time.Duration) (envelope.Request, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, false, err
	}
	defer tx.Rollback()
	now := s.now()
	outcome := "exhausted"
	if d.Request.NoticeKind == envelope.TaskNoticeReminder {
		var id, dueAt int64
		var dropped bool
		// Read the deadline only while this attempt is still actionable.
		// A due change cancels it in the same transaction as the change.
		err := tx.QueryRowContext(ctx, `SELECT t.id, t.due_at, t.reminder_dropped_at IS NOT NULL
			FROM tasks t JOIN requests r ON r.notice_for = CAST(t.id AS TEXT)
			JOIN task_notice_delivery d ON d.request_id = r.id WHERE r.id = ? AND d.outcome = ''`,
			d.Request.ID).Scan(&id, &dueAt, &dropped)
		if errors.Is(err, sql.ErrNoRows) {
			return envelope.Request{}, false, nil
		}
		if err != nil {
			return envelope.Request{}, false, err
		}
		deadline := time.Unix(dueAt, 0).Add(reminderLateness)
		ttl = min(ttl, deadline.Sub(now))
		if !now.Before(deadline) {
			next = envelope.Request{}
			if !dropped {
				if _, err := tx.ExecContext(ctx, `UPDATE tasks SET reminder_dropped_at = ? WHERE id = ?`, now.Unix(), id); err != nil {
					return envelope.Request{}, false, err
				}
				if err := noteLateReminder(ctx, tx, id, now); err != nil {
					return envelope.Request{}, false, err
				}
			}
		}
	}
	retry := IsTaskNotice(next.NoticeKind)
	if retry {
		outcome = "retried"
	}
	res, err := tx.ExecContext(ctx, `UPDATE task_notice_delivery SET outcome = ? WHERE request_id = ? AND outcome = ''
		AND (NOT ? OR EXISTS (SELECT 1 FROM requests WHERE id = ? AND status IN ('claimed', 'expired')))`,
		outcome, d.Request.ID, retry, d.Request.ID)
	if err != nil {
		return envelope.Request{}, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return envelope.Request{}, false, err
	}
	if !retry {
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'cancelled', lease_until = 0, updated_at = ?
			WHERE id = ? AND status IN ('queued', 'delivered', 'claimed')`, s.now().UnixMilli(), d.Request.ID); err != nil {
			return envelope.Request{}, false, err
		}
	}
	if next.To != "" {
		if next, err = s.enqueueTx(ctx, tx, next, ttl); err != nil {
			return envelope.Request{}, false, err
		}
		if retry {
			if err := trackTaskNotice(ctx, tx, next, d.Attempt+1); err != nil {
				return envelope.Request{}, false, err
			}
		}
	}
	return next, true, tx.Commit()
}
