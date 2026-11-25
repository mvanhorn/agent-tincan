package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// migrateJoinRequests creates the join request table. A request keeps its
// row while open and a slim row (no agents, no code) for an hour after it
// reaches a final state, so its outcome stays readable and the hourly cap
// survives a restart.
func (s *Store) migrateJoinRequests() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS join_requests (
  id         TEXT PRIMARY KEY,
  node_id    TEXT NOT NULL,
  node_key   TEXT NOT NULL,
  node_name  TEXT NOT NULL,
  node_user  TEXT NOT NULL DEFAULT '',
  tags       TEXT NOT NULL DEFAULT '[]',
  addrs      TEXT NOT NULL DEFAULT '[]',
  agents     TEXT NOT NULL DEFAULT '[]', -- requested names and kinds, cleared once final
  code       TEXT NOT NULL DEFAULT '',   -- the matching code, cleared once final; not a credential
  status     TEXT NOT NULL,
  filed_at   INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  decided_at INTEGER NOT NULL DEFAULT 0,
  decided_by TEXT NOT NULL DEFAULT '',
  closed_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS join_requests_node ON join_requests(node_id, filed_at);
CREATE INDEX IF NOT EXISTS join_requests_status ON join_requests(status, expires_at);`)
	return err
}

const joinCols = `id, node_id, node_key, node_name, node_user, tags, addrs, agents, code, status, filed_at, expires_at, decided_at, decided_by, closed_at`

func scanJoin(sc scanner) (identity.JoinRequest, error) {
	var r identity.JoinRequest
	var tags, addrs, agents string
	var filed, expires, decided, closed int64
	if err := sc.Scan(&r.ID, &r.NodeID, &r.NodeKey, &r.NodeName, &r.NodeUser, &tags, &addrs, &agents, &r.Code, &r.Status, &filed, &expires, &decided, &r.DecidedBy, &closed); err != nil {
		return identity.JoinRequest{}, err
	}
	for _, f := range []struct {
		raw string
		v   any
	}{{tags, &r.Tags}, {addrs, &r.Addrs}, {agents, &r.Agents}} {
		if err := json.Unmarshal([]byte(f.raw), f.v); err != nil {
			return identity.JoinRequest{}, fmt.Errorf("join request %s: %w", r.ID, err)
		}
	}
	r.Filed, r.Expires = time.UnixMilli(filed), time.UnixMilli(expires)
	r.Decided, r.Closed = unixMilliOrZero(decided), unixMilliOrZero(closed)
	return r, nil
}

func unixMilliOrZero(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func zeroOrUnixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func jsonList[T any](v []T) (string, error) {
	if v == nil {
		v = []T{}
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// InsertJoinRequest stores r, checking the caps in the same transaction: it
// is refused with identity.ErrJoinCap when r's node already has an open
// request, has filed identity.MaxJoinRequestsPerHour in the hour before now
// (whatever their outcome), or the relay holds
// identity.MaxPendingJoinRequests pending. The relay calls it through
// identity.Directory.FileJoinRequest, under the directory's join lock and
// after the name checks.
func (s *Store) InsertJoinRequest(ctx context.Context, r identity.JoinRequest, now time.Time) error {
	tags, err := jsonList(r.Tags)
	if err != nil {
		return err
	}
	addrs, err := jsonList(r.Addrs)
	if err != nil {
		return err
	}
	agents, err := jsonList(r.Agents)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	nowMS := now.UnixMilli()
	var open, hour, pending int
	if err := tx.QueryRowContext(ctx, `SELECT
  (SELECT COUNT(*) FROM join_requests WHERE node_id = ? AND status IN (?, ?) AND expires_at > ?),
  (SELECT COUNT(*) FROM join_requests WHERE node_id = ? AND filed_at > ?),
  (SELECT COUNT(*) FROM join_requests WHERE status = ? AND expires_at > ?)`,
		r.NodeID, identity.JoinPending, identity.JoinApproved, nowMS,
		r.NodeID, now.Add(-time.Hour).UnixMilli(),
		identity.JoinPending, nowMS).Scan(&open, &hour, &pending); err != nil {
		return err
	}
	if err := identity.JoinCapRefusal(open, hour, pending); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO join_requests (`+joinCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.NodeID, r.NodeKey, r.NodeName, r.NodeUser, tags, addrs, agents, r.Code, r.Status,
		r.Filed.UnixMilli(), r.Expires.UnixMilli(), zeroOrUnixMilli(r.Decided), r.DecidedBy, zeroOrUnixMilli(r.Closed)); err != nil {
		return err
	}
	return tx.Commit()
}

func getJoin(ctx context.Context, q queryer, id string, now time.Time) (identity.JoinRequest, bool, error) {
	r, err := scanJoin(q.QueryRowContext(ctx, `SELECT `+joinCols+` FROM join_requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return identity.JoinRequest{}, false, nil
	}
	if err != nil {
		return identity.JoinRequest{}, false, err
	}
	return r.AsOf(now), true, nil
}

// JoinRequest returns the request with id as it stands at now: an open
// request past its expiry reads as expired even before the sweep.
func (s *Store) JoinRequest(ctx context.Context, id string, now time.Time) (identity.JoinRequest, bool, error) {
	return getJoin(ctx, s.db, id, now)
}

// PendingJoinRequests lists the pending requests not yet expired at now,
// oldest first.
func (s *Store) PendingJoinRequests(ctx context.Context, now time.Time) ([]identity.JoinRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+joinCols+` FROM join_requests WHERE status = ? AND expires_at > ? ORDER BY filed_at, id`, identity.JoinPending, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identity.JoinRequest
	for rows.Next() {
		r, err := scanJoin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// moveJoin moves request id to status to inside tx when
// identity.JoinRequest.CanMoveTo allows it, with a conditional update on the
// status it was read in. by, when set, is recorded as where it was decided.
// A final status clears the stored agents and code. It returns the request
// as decided, names and code included; on identity.ErrJoinState, the request
// as it stands.
func moveJoin(ctx context.Context, tx *sql.Tx, id, to, by string, now time.Time) (identity.JoinRequest, error) {
	r, ok, err := getJoin(ctx, tx, id, now)
	if err != nil {
		return identity.JoinRequest{}, err
	}
	if !ok {
		return identity.JoinRequest{}, identity.ErrJoinNotFound
	}
	if !r.CanMoveTo(to) {
		return r, identity.ErrJoinState
	}
	from := r.Status
	r.Status = to
	if by != "" {
		r.Decided, r.DecidedBy = now, by
	}
	keep := r.Open()
	if !keep {
		r.Closed = now
	}
	// An open request keeps its agents and code; a final one loses both.
	res, err := tx.ExecContext(ctx, `UPDATE join_requests SET status = ?, decided_at = ?, decided_by = ?, closed_at = ?,
  agents = CASE WHEN ? THEN agents ELSE '[]' END, code = CASE WHEN ? THEN code ELSE '' END
WHERE id = ? AND status = ? AND expires_at > ?`,
		to, zeroOrUnixMilli(r.Decided), r.DecidedBy, zeroOrUnixMilli(r.Closed), keep, keep, id, from, now.UnixMilli())
	if err != nil {
		return identity.JoinRequest{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return identity.JoinRequest{}, err
	} else if n != 1 {
		return identity.JoinRequest{}, fmt.Errorf("join request %s changed while it was moved to %s", id, to)
	}
	return r, nil
}

// moveJoinTx runs moveJoin in a transaction of its own.
func (s *Store) moveJoinTx(ctx context.Context, id, to, by string, now time.Time) (identity.JoinRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity.JoinRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := moveJoin(ctx, tx, id, to, by, now)
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}

// ApproveJoinRequest moves a pending, unexpired request to approved and
// records by as where it was decided. It binds nothing; the requesting node
// claims the agents.
func (s *Store) ApproveJoinRequest(ctx context.Context, id, by string, now time.Time) (identity.JoinRequest, error) {
	return s.moveJoinTx(ctx, id, identity.JoinApproved, by, now)
}

// DenyJoinRequest moves a pending, unexpired request to denied, which is
// final: the stored row loses its names and code.
func (s *Store) DenyJoinRequest(ctx context.Context, id, by string, now time.Time) (identity.JoinRequest, error) {
	return s.moveJoinTx(ctx, id, identity.JoinDenied, by, now)
}

// VoidJoinRequest moves an open, unexpired request to voided, which is
// final.
func (s *Store) VoidJoinRequest(ctx context.Context, id string, now time.Time) (identity.JoinRequest, error) {
	return s.moveJoinTx(ctx, id, identity.JoinVoided, "", now)
}

// FailJoinRequest moves an approved, unexpired request to failed, which is
// final.
func (s *Store) FailJoinRequest(ctx context.Context, id string, now time.Time) (identity.JoinRequest, error) {
	return s.moveJoinTx(ctx, id, identity.JoinFailed, "", now)
}

// ClaimJoinRequest delivers an approved, unexpired request to n in one
// transaction: n must be the recorded node with the recorded key
// (identity.ErrJoinWrongNode otherwise, nothing changed); it inserts an agent
// row on n for every requested name and moves the request to delivered, so
// it binds every name or none. A name that already has an agent row fails
// the insert and rolls the whole claim back. The relay claims through
// identity.Directory.ClaimJoinRequest, which re-checks the names under the
// join lock first.
func (s *Store) ClaimJoinRequest(ctx context.Context, id string, n identity.Node, now time.Time) (identity.JoinRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity.JoinRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	r, ok, err := getJoin(ctx, tx, id, now)
	if err != nil {
		return identity.JoinRequest{}, err
	}
	if !ok {
		return identity.JoinRequest{}, identity.ErrJoinNotFound
	}
	if r.NodeID != n.ID || r.NodeKey != n.Key {
		return identity.JoinRequest{}, identity.ErrJoinWrongNode
	}
	if !r.CanMoveTo(identity.JoinDelivered) {
		return r, identity.ErrJoinState
	}
	for _, a := range r.Agents {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agents(name, node_id, node_name, joined_at, kind, node_user) VALUES (?, ?, ?, ?, ?, ?)`,
			a.Name, n.ID, n.Name, now.UnixMilli(), nullable(a.Kind), nullable(n.User)); err != nil {
			return identity.JoinRequest{}, fmt.Errorf("bind %s: %w", a.Name, err)
		}
	}
	r, err = moveJoin(ctx, tx, id, identity.JoinDelivered, "", now)
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}

// JoinNameHeld reports whether a request other than exceptID that is open
// and unexpired at now asks for name.
func (s *Store) JoinNameHeld(ctx context.Context, name, exceptID string, now time.Time) (bool, error) {
	var held bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM join_requests r, json_each(r.agents) j
WHERE r.status IN (?, ?) AND r.expires_at > ? AND r.id != ? AND json_extract(j.value, '$.name') = ?)`,
		identity.JoinPending, identity.JoinApproved, now.UnixMilli(), exceptID, name).Scan(&held)
	return held, err
}

// SweepJoinRequests moves every open request whose expiry is at or before
// now to expired, clearing its names and code, and returns those requests
// as they were, names included and oldest first, for the audit log. It
// deletes final rows closed identity.JoinFinalKeep or more before now.
func (s *Store) SweepJoinRequests(ctx context.Context, now time.Time) ([]identity.JoinRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	nowMS := now.UnixMilli()
	rows, err := tx.QueryContext(ctx, `SELECT `+joinCols+` FROM join_requests WHERE status IN (?, ?) AND expires_at <= ? ORDER BY filed_at, id`,
		identity.JoinPending, identity.JoinApproved, nowMS)
	if err != nil {
		return nil, err
	}
	var expired []identity.JoinRequest
	for rows.Next() {
		r, err := scanJoin(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		r.Status, r.Closed = identity.JoinExpired, now
		expired = append(expired, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(expired) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE join_requests SET status = ?, closed_at = ?, agents = '[]', code = '' WHERE status IN (?, ?) AND expires_at <= ?`,
			identity.JoinExpired, nowMS, identity.JoinPending, identity.JoinApproved, nowMS); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM join_requests WHERE status NOT IN (?, ?) AND closed_at <= ?`,
		identity.JoinPending, identity.JoinApproved, now.Add(-identity.JoinFinalKeep).UnixMilli()); err != nil {
		return nil, err
	}
	return expired, tx.Commit()
}

// LiveInviteNamed reports whether an unredeemed invite for name has not
// expired at now.
func (s *Store) LiveInviteNamed(ctx context.Context, name string, now time.Time) (bool, error) {
	var live bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM invites WHERE name = ? AND expires >= ?)`, name, now.UnixMilli()).Scan(&live)
	return live, err
}
