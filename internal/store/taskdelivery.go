package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func trackTaskNotice(ctx context.Context, tx *sql.Tx, id string, attempt int) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO task_notice_delivery (request_id, attempt) VALUES (?, ?)`, id, attempt)
	return err
}

// TaskDelivery identifies one attempt to deliver a task notice to the owner.
type TaskDelivery struct {
	Request envelope.Request
	Attempt int
}

// TaskNoticeClaimSession gives the session that first claimed this notice.
func (s *Store) TaskNoticeClaimSession(ctx context.Context, id string) (string, error) {
	var session string
	err := s.db.QueryRowContext(ctx, `SELECT claim_session FROM requests WHERE id = ?`, id).Scan(&session)
	return session, err
}

// TaskNoticesDue finds claims without a delivery receipt and notices that
// expired in the queue. Attempts and outcomes survive relay restarts.
func (s *Store) TaskNoticesDue(ctx context.Context, after time.Duration, limit int) ([]TaskDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.trace_id, r.to_agent, r.notice_for, r.body, d.attempt
		FROM task_notice_delivery d JOIN requests r ON r.id = d.request_id
		WHERE d.outcome = '' AND (r.status = 'expired' OR (r.status = 'claimed' AND r.claimed_at <= ?))
		ORDER BY r.created_at, r.id LIMIT ?`, s.now().Add(-after).UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var due []TaskDelivery
	for rows.Next() {
		var d TaskDelivery
		if err := rows.Scan(&d.Request.ID, &d.Request.TraceID, &d.Request.To, &d.Request.NoticeFor, &d.Request.Body, &d.Attempt); err != nil {
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
// nothing. An empty next recipient settles a notice with nowhere to go.
func (s *Store) AdvanceTaskNotice(ctx context.Context, d TaskDelivery, next envelope.Request, ttl time.Duration) (envelope.Request, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, false, err
	}
	defer tx.Rollback()
	outcome := "exhausted"
	if next.NoticeKind == NoteTask {
		outcome = "retried"
	}
	res, err := tx.ExecContext(ctx, `UPDATE task_notice_delivery SET outcome = ? WHERE request_id = ? AND outcome = ''`, outcome, d.Request.ID)
	if err != nil {
		return envelope.Request{}, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return envelope.Request{}, false, err
	}
	if next.To != "" {
		if next, err = s.enqueueTx(ctx, tx, next, ttl); err != nil {
			return envelope.Request{}, false, err
		}
		if next.NoticeKind == NoteTask {
			if err := trackTaskNotice(ctx, tx, next.ID, d.Attempt+1); err != nil {
				return envelope.Request{}, false, err
			}
		}
	}
	return next, true, tx.Commit()
}
