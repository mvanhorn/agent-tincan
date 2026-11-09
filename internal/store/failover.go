package store

// Failover: moving an ask nothing has typed into a site yet off a web
// agent that is out of messages, to another web agent. A move changes a
// request's target, so every write keyed by request id also checks the
// target (to_agent), and an agent the request moved from no longer finds
// it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// migrateFailover adds the request columns failover needs: moved_from, the
// JSON array of the targets a request moved off (” when none, like
// chain), deliver_after, before which a held-back request is not
// delivered (Unix ms, 0 for none), and claim_lapsed, 1 once any claim on
// the request ran out of lease (that claimant may have typed it into a
// site, so it never moves). claim_lapsed is never cleared; when it is
// added, requests whose claim already lapsed get it. It is a no-op on a
// current table.
func (s *Store) migrateFailover() error {
	for _, col := range []struct{ name, definition, backfill string }{
		{"moved_from", "TEXT NOT NULL DEFAULT ''", ""},
		{"deliver_after", "INTEGER NOT NULL DEFAULT 0", ""},
		{"claim_lapsed", "INTEGER NOT NULL DEFAULT 0", `UPDATE requests SET claim_lapsed = 1 WHERE lease_lapsed_at IS NOT NULL OR (status = 'queued' AND claimed_at > 0)`},
	} {
		var has bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = ?)`, col.name).Scan(&has); err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN ` + col.name + ` ` + col.definition); err != nil {
			return err
		}
		if col.backfill != "" {
			if _, err := s.db.Exec(col.backfill); err != nil {
				return err
			}
		}
	}
	return nil
}

// notTarget is the error for agent acting on req as its target when it is
// not: not found for a target the request moved off (it has lost sight of
// it), forbidden otherwise.
func notTarget(req envelope.Request, agent string) error {
	if slices.Contains(req.MovedFrom, agent) {
		return ErrNotFound
	}
	return ErrForbidden
}

// wrongState is the error for a conditional write by agent that changed no
// row: not found when the request has moved off agent, ErrWrongState
// otherwise.
func (s *Store) wrongState(ctx context.Context, id, agent string) error {
	if req, _, err := s.lookup(ctx, id); err == nil && req.To != agent && slices.Contains(req.MovedFrom, agent) {
		return ErrNotFound
	}
	return ErrWrongState
}

// requeueClaim is the SET list that returns a request to the queue with
// its claim, lease and progress cleared, shared by Move and HoldBack.
const requeueClaim = `status = 'queued', lease_until = 0, claimed_at = 0, claim_session = '', progress_note = '', progress_at = 0, lease_lapsed_at = NULL`

// Move makes to the target of request id in place of from, keeping its id,
// sender, chain and body. A queued request moves (claimed false); a claimed
// one moves only from its claimant under a live claim (claimed true, a
// handback). Either way it ends queued for to, with the claim, lease,
// progress and hold-back cleared as a lease requeue clears them, from added
// to moved_from, and its pending wake note deleted so to's silence is
// reported in turn. Its project is dropped: the label named a session of
// from, and waiting on it would only delay the new target. A request never
// moves to an agent it moved from, nor once a claim on it lapsed
// (ClaimLapsed). It returns ErrWrongState when the request is no longer in that state for
// from (a Deliver or Claim won), and the moved request otherwise.
func (s *Store) Move(ctx context.Context, id, from, to string, claimed bool) (envelope.Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, err
	}
	defer tx.Rollback()
	req, _, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id = ?`, id))
	if err != nil {
		return envelope.Request{}, ErrNotFound
	}
	if to == from || slices.Contains(req.MovedFrom, to) {
		return envelope.Request{}, ErrWrongState
	}
	movedFrom, err := json.Marshal(append(slices.Clone(req.MovedFrom), from))
	if err != nil {
		return envelope.Request{}, err
	}
	now := s.now().UnixMilli()
	cond := `status = 'queued'`
	if claimed {
		cond = `status = 'claimed' AND lease_until > ?`
	}
	args := []any{to, string(movedFrom), now, id, from, now}
	if claimed {
		args = append(args, now)
	}
	res, err := tx.ExecContext(ctx, `UPDATE requests SET to_agent = ?, moved_from = ?, to_project = '', `+requeueClaim+`, deliver_after = 0, updated_at = ?
		WHERE id = ? AND to_agent = ? AND expires_at > ? AND claim_lapsed = 0 AND `+cond, args...)
	if err != nil {
		return envelope.Request{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Request{}, ErrWrongState
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM relay_notes WHERE request_id = ? AND kind = ?`, id, NoteWake); err != nil {
		return envelope.Request{}, err
	}
	if err := tx.Commit(); err != nil {
		return envelope.Request{}, err
	}
	req, _, err = s.lookup(ctx, id)
	return req, err
}

// HoldBack keeps request id with its target, agent, but holds its delivery
// back until until: a queued request (claimed false) stays queued, and a
// claim agent holds live (claimed true, a handback with nowhere to move)
// is requeued with its claim cleared. It returns ErrWrongState when the
// request is not in that state for agent.
func (s *Store) HoldBack(ctx context.Context, id, agent string, until time.Time, claimed bool) (envelope.Request, error) {
	now := s.now().UnixMilli()
	query := `UPDATE requests SET deliver_after = ? WHERE id = ? AND to_agent = ? AND status = 'queued' AND expires_at > ?`
	args := []any{until.UnixMilli(), id, agent, now}
	if claimed {
		query = `UPDATE requests SET ` + requeueClaim + `, deliver_after = ?, updated_at = ?
			WHERE id = ? AND to_agent = ? AND status = 'claimed' AND lease_until > ? AND expires_at > ?`
		args = []any{until.UnixMilli(), now, id, agent, now, now}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return envelope.Request{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Request{}, ErrWrongState
	}
	req, _, err := s.lookup(ctx, id)
	return req, err
}

// FailHeld closes request id, which agent's limit would hold back past its
// expiry, with agent's terminal reply rep, but only while it is in the
// state HoldBack would hold it from: queued (claimed false), or claimed by
// agent under a live claim (claimed true, a handback). It returns
// ErrWrongState when the request left that state, such as an ask its
// target claimed after the caller read it as queued.
func (s *Store) FailHeld(ctx context.Context, id, agent string, claimed bool, rep envelope.Reply) (envelope.Reply, error) {
	if !rep.Status.Terminal() {
		return envelope.Reply{}, ErrWrongState
	}
	now := s.now().UnixMilli()
	guard, args := `status = 'queued' AND expires_at > ?`, []any{now}
	if claimed {
		guard, args = `status = 'claimed' AND lease_until > ? AND expires_at > ?`, []any{now, now}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Reply{}, err
	}
	defer tx.Rollback()
	if rep, err = s.replyTx(ctx, tx, id, agent, rep, guard, args...); err != nil {
		return envelope.Reply{}, err
	}
	return rep, tx.Commit()
}

// RequestExpiry is when request id expires.
func (s *Store) RequestExpiry(ctx context.Context, id string) (time.Time, error) {
	var ms int64
	if err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM requests WHERE id = ?`, id).Scan(&ms); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, ErrNotFound
		}
		return time.Time{}, err
	}
	return time.UnixMilli(ms), nil
}

// ClaimLapsed reports whether any claim on request id ever ran out of
// lease. Its claimant may have typed the ask into a site before it went
// quiet, so a later claimant handing it back cannot vouch that it is
// unsent.
func (s *Store) ClaimLapsed(ctx context.Context, id string) (bool, error) {
	var lapsed bool
	if err := s.db.QueryRowContext(ctx, `SELECT claim_lapsed = 1 FROM requests WHERE id = ?`, id).Scan(&lapsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	return lapsed, nil
}

// MoveCandidate is a queued request, whether it was ever claimed (a web
// agent that claimed it may already have typed it into its site), and when
// HoldBack holds it back to (zero for never), so a sweep need not hold it
// back again.
type MoveCandidate struct {
	Request      envelope.Request
	Claimed      bool
	DeliverAfter time.Time
}

// MoveCandidates returns agent's live queued requests, oldest first, each
// with whether it was ever claimed and its hold-back time.
func (s *Store) MoveCandidates(ctx context.Context, agent string) ([]MoveCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestCols+`, claimed_at > 0 OR lease_lapsed_at IS NOT NULL OR claim_lapsed = 1, deliver_after FROM requests
		WHERE to_agent = ? AND status = ? AND expires_at > ? ORDER BY created_at, rowid`,
		agent, string(envelope.StatusQueued), s.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MoveCandidate
	for rows.Next() {
		var m MoveCandidate
		var after int64
		var err error
		if m.Request, _, err = scanRequest(extraCols{rows, []any{&m.Claimed, &after}}); err != nil {
			return nil, err
		}
		if after > 0 {
			m.DeliverAfter = time.UnixMilli(after)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReleaseHolds brings held-back requests in line with their targets'
// current limits, limited (each limited agent's reset time, while it is
// ahead): a queued request held back past now whose target is not in
// limited is released at once, and one held back past its target's reset
// time is held only until then. It returns the agents with a request
// released now, for their inboxes to be told.
func (s *Store) ReleaseHolds(ctx context.Context, limited map[string]time.Time) ([]string, error) {
	now := s.now().UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT to_agent FROM requests WHERE status = ? AND expires_at > ? AND deliver_after > ?`,
		string(envelope.StatusQueued), now, now)
	if err != nil {
		return nil, err
	}
	var agents []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return nil, err
		}
		agents = append(agents, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var released []string
	for _, a := range agents {
		until := int64(0)
		if t, ok := limited[a]; ok && t.UnixMilli() > now {
			until = t.UnixMilli()
		}
		res, err := s.db.ExecContext(ctx, `UPDATE requests SET deliver_after = ? WHERE to_agent = ? AND status = ? AND expires_at > ? AND deliver_after > ?`,
			until, a, string(envelope.StatusQueued), now, max(until, now))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 && until == 0 {
			released = append(released, a)
		}
	}
	return released, nil
}
