package store

import (
	"context"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// HoldAck keeps claimer's answered reply ack, which only acknowledges the
// work, on its claim of ask id instead of closing the request: the ack
// becomes the progress note (cut to envelope.MaxProgressNote) and is kept
// whole in held_ack, and the lease renews as SetProgressLeases renews it.
// The next reply clears it. If the claim lapses or the request expires
// first, CloseHeldAcks closes the request with the ack, so a reply misread as
// an acknowledgment is never handed back to its agent to do again. Only asks
// qualify; a notify has no lease to run out. It returns ErrWrongState (or
// another lookup error) when claimer holds no live claim on an ask id.
func (s *Store) HoldAck(ctx context.Context, id, claimer, ack string, lease, urgentLease time.Duration) error {
	note := ack
	if len(note) > envelope.MaxProgressNote {
		note = note[:envelope.MaxProgressNote]
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, `UPDATE requests SET held_ack = ?, progress_note = ?, progress_at = ?, updated_at = ?, lease_lapsed_at = NULL,
		lease_until = CASE WHEN urgent = 1 THEN ? ELSE ? END
		WHERE id = ? AND to_agent = ? AND status = ? AND kind = ? AND lease_until > ?`,
		ack, note, now.UnixMilli(), now.UnixMilli(), now.Add(urgentLease).UnixMilli(), now.Add(lease).UnixMilli(),
		id, claimer, string(envelope.StatusClaimed), string(envelope.KindAsk), now.UnixMilli())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrWrongState
	}
	return nil
}

// CloseHeldAcks answers, with its held ack, every claimed ask whose claim
// lease has run out or whose expiry has come while an ack was held
// (HoldAck). It runs before Sweep, which would otherwise requeue or expire
// those requests. It returns the replies it stored, for the relay to tell
// the askers as it tells them of any reply.
func (s *Store) CloseHeldAcks(ctx context.Context) ([]envelope.Reply, error) {
	now := s.now().UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT id, to_agent, held_ack FROM requests
		WHERE status = ? AND held_ack != '' AND lease_paused = 0 AND ((lease_until > 0 AND lease_until <= ?) OR expires_at <= ?)`,
		string(envelope.StatusClaimed), now, now)
	if err != nil {
		return nil, err
	}
	type held struct{ id, agent, ack string }
	var due []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.agent, &h.ack); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []envelope.Reply
	for _, h := range due {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return out, err
		}
		rep, err := s.replyTx(ctx, tx, h.id, h.agent, envelope.Reply{Status: envelope.StatusAnswered, Body: h.ack}, "held_ack != ''")
		if err != nil {
			_ = tx.Rollback()
			if err == ErrWrongState {
				continue // answered or moved since the select
			}
			return out, err
		}
		if err := tx.Commit(); err != nil {
			return out, err
		}
		out = append(out, rep)
	}
	return out, nil
}
