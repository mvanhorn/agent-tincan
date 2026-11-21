package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func (s *Store) migrateClarification() error {
	for _, col := range []struct{ name, definition string }{
		{"reply_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"exchanges", "TEXT NOT NULL DEFAULT '[]'"},
		{"lease_paused", "INTEGER NOT NULL DEFAULT 0"},
		{"resumed", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var has bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = ?)`, col.name).Scan(&has); err != nil {
			return err
		}
		if !has {
			if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN ` + col.name + ` ` + col.definition); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) needsInput(ctx context.Context, id, agent string, rep envelope.Reply) (envelope.Reply, error) {
	if err := envelope.ValidateInput(rep.Body); err != nil {
		return envelope.Reply{}, err
	}
	if len(rep.Attachments) != 0 {
		return envelope.Reply{}, ErrWrongState
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Reply{}, err
	}
	defer tx.Rollback()
	req, status, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id = ?`, id))
	if err != nil {
		return envelope.Reply{}, err
	}
	if req.To != agent {
		return envelope.Reply{}, notTarget(req, agent)
	}
	if status != envelope.StatusClaimed || req.Kind != envelope.KindAsk || len(req.Exchanges) >= envelope.MaxExchanges {
		return envelope.Reply{}, ErrWrongState
	}
	req.Exchanges = append(req.Exchanges, envelope.Exchange{Question: rep.Body, At: now.UTC().Truncate(time.Millisecond)})
	exchanges, err := json.Marshal(req.Exchanges)
	if err != nil {
		return envelope.Reply{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, exchanges = ?, lease_paused = 1, lease_until = 0, updated_at = ?, reply_seen_at = 0, reply_generation = reply_generation + 1, held_ack = ''
		WHERE id = ? AND to_agent = ? AND status = ? AND expires_at > ? AND lease_until > ?`, envelope.StatusNeedsInput, string(exchanges), now.UnixMilli(), id, agent, envelope.StatusClaimed, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return envelope.Reply{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Reply{}, ErrWrongState
	}
	if err := tx.QueryRowContext(ctx, `SELECT reply_generation FROM requests WHERE id = ?`, id).Scan(&rep.Generation); err != nil {
		return envelope.Reply{}, err
	}
	rep.RequestID, rep.From, rep.CreatedAt = id, agent, now.UTC().Truncate(time.Millisecond)
	if _, err := tx.ExecContext(ctx, `INSERT INTO replies(request_id, from_agent, status, body, created_at) VALUES (?, ?, ?, ?, ?)`, id, agent, rep.Status, rep.Body, now.UnixMilli()); err != nil {
		return envelope.Reply{}, err
	}
	if req.RoomPostID != 0 {
		if err := s.mirrorReplyTx(ctx, tx, rep); err != nil {
			return envelope.Reply{}, err
		}
	}
	if err := s.followTaskTx(ctx, tx, id, `status = ?`, []any{string(envelope.TaskQuestion)},
		envelope.TaskEvent{Actor: agent, Kind: envelope.TaskEventStatus, Body: rep.Body, Status: envelope.TaskQuestion}); err != nil {
		return envelope.Reply{}, err
	}
	return rep, tx.Commit()
}

// Answer records input from the original sender and queues the same request again.
func (s *Store) Answer(ctx context.Context, id, agent, body string) (envelope.Request, error) {
	return s.answer(ctx, id, agent, body, false, AnswerCheck{})
}

// AnswerCheck is what the owner's approval rules made of a teammate's answer
// (relay.AnswerChecker): the request's contributors with the names the
// answer may carry, and whether the answered request waits for the owner
// again, for HoldTTL from now.
type AnswerCheck struct {
	Contributors []string
	Held         bool
	HoldTTL      time.Duration
}

// AnswerChecked is Answer with the owner's approval rules applied to the
// answer: the request keeps check's contributors, and when check holds it,
// it goes back to held, unapproved, rather than to its target. The owner
// approved the request it was, not the text the asker adds to it.
func (s *Store) AnswerChecked(ctx context.Context, id, agent, body string, check AnswerCheck) (envelope.Request, error) {
	return s.answer(ctx, id, agent, body, false, check)
}

// AnswerAsGuest is Answer for a guest. The owner approved the ask, not this
// answer, so the answer goes in framed as the guest's text and the request
// stops being approved: what the teammate does next on it is held again.
func (s *Store) AnswerAsGuest(ctx context.Context, id, agent, body string) (envelope.Request, error) {
	return s.answer(ctx, id, agent, body, true, AnswerCheck{})
}

func (s *Store) answer(ctx context.Context, id, agent, body string, guest bool, check AnswerCheck) (envelope.Request, error) {
	if err := envelope.ValidateInput(body); err != nil {
		return envelope.Request{}, err
	}
	if guest {
		body = envelope.GuestText(body)
	}
	// Resolve missing ids and authorization before starting the transaction.
	req, _, err := s.lookup(ctx, id)
	if err != nil {
		return envelope.Request{}, err
	}
	if req.From != agent {
		return envelope.Request{}, ErrForbidden
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, err
	}
	defer tx.Rollback()
	req, status, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id = ?`, id))
	if err != nil {
		return envelope.Request{}, err
	}
	if status != envelope.StatusNeedsInput || len(req.Exchanges) == 0 {
		return envelope.Request{}, ErrWrongState
	}
	req.Exchanges[len(req.Exchanges)-1].Answer = body
	exchanges, err := json.Marshal(req.Exchanges)
	if err != nil {
		return envelope.Request{}, err
	}
	if check.Contributors != nil {
		req.Contributors = check.Contributors
	}
	contributors := ""
	if len(req.Contributors) > 0 {
		b, err := json.Marshal(req.Contributors)
		if err != nil {
			return envelope.Request{}, err
		}
		contributors = string(b)
	}
	now := s.now().UnixMilli()
	status, expires := envelope.StatusQueued, int64(0)
	if check.Held {
		status, expires = envelope.StatusHeld, s.now().Add(check.HoldTTL).UnixMilli()
	}
	// The asker's answer queues the request afresh, so its project gets the
	// first look again rather than whichever window polls next. One the
	// owner's rules hold waits for the owner as a fresh held request does.
	res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, exchanges = ?, lease_paused = 0, lease_until = 0, resumed = 1, held_ack = '', updated_at = ?, reply_seen_at = ?, queued_at = ?, fallback_woken = 0,
		progress_note = '', progress_at = 0, guest_approved = guest_approved AND ?, contributors = ?,
		was_held = was_held OR ?, approved = approved AND NOT ?, expires_at = CASE WHEN ? THEN ? ELSE expires_at END
		WHERE id = ? AND status = ? AND expires_at > ?`, status, string(exchanges), now, now, now, !guest, contributors,
		check.Held, check.Held, check.Held, expires, id, envelope.StatusNeedsInput, now)
	if err != nil {
		return envelope.Request{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Request{}, ErrWrongState
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM replies WHERE request_id = ?`, id); err != nil {
		return envelope.Request{}, err
	}
	if err := s.followTaskTx(ctx, tx, id, `status = ?`, []any{string(envelope.TaskWorking)},
		envelope.TaskEvent{Actor: agent, Kind: envelope.TaskEventStatus, Body: "answered the question", Status: envelope.TaskWorking}); err != nil {
		return envelope.Request{}, err
	}
	req.Resumed = true
	req.GuestApproved = req.GuestApproved && !guest
	req.Status = status
	if check.Held {
		req.WasHeld, req.Approved = true, false
	}
	return req, tx.Commit()
}
