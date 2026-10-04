package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Wake is the last wake the relay sent an agent: when, and "ok" or the
// error that made the send and its retry fail.
type Wake struct {
	At     time.Time
	Result string
}

// migrateWakes creates the table holding each relay-woken agent's last wake,
// one row per agent. It is a no-op when the table exists.
func (s *Store) migrateWakes() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS wakes (agent TEXT PRIMARY KEY, woken_at INTEGER NOT NULL, result TEXT NOT NULL)`)
	return err
}

// SetLastWake records w as agent's last wake. A newer timestamp replaces
// the row; the same timestamp updates the result so a follow-up can record
// a failure (or a later recovery) without moving woken_at. A slower older
// send cannot hide a newer one.
func (s *Store) SetLastWake(ctx context.Context, agent string, w Wake) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO wakes (agent, woken_at, result) VALUES (?, ?, ?)
ON CONFLICT(agent) DO UPDATE SET woken_at = excluded.woken_at, result = excluded.result
WHERE excluded.woken_at > wakes.woken_at
   OR (excluded.woken_at = wakes.woken_at AND excluded.result != wakes.result)`, agent, w.At.UnixMilli(), w.Result)
	return err
}

// LastWakes returns the last wake of every agent the relay has woken.
func (s *Store) LastWakes(ctx context.Context) (map[string]Wake, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent, woken_at, result FROM wakes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Wake{}
	for rows.Next() {
		var name, result string
		var ms int64
		if err := rows.Scan(&name, &ms, &result); err != nil {
			return nil, err
		}
		out[name] = Wake{At: time.UnixMilli(ms), Result: result}
	}
	return out, rows.Err()
}

// migrateAgentLastPoll adds the last_poll_at column (unix millis, NULL until
// the agent first polls). A poll, unlike any other call, is the proof a woken
// agent checked in, so it is kept apart from last_seen_at. It runs after
// migrateAgents, whose rebuild copies only the older columns, and is a no-op
// on a current table.
func (s *Store) migrateAgentLastPoll() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('agents') WHERE name = 'last_poll_at')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE agents ADD COLUMN last_poll_at INTEGER`)
	return err
}

// TouchAgentPoll records that name polled the relay at t. It only moves the
// stored time forward and ignores names not in the directory.
func (s *Store) TouchAgentPoll(ctx context.Context, name string, t time.Time) error {
	ms := t.UnixMilli()
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_poll_at = ? WHERE name = ? AND (last_poll_at IS NULL OR last_poll_at < ?)`, ms, name, ms)
	return err
}

// AgentLastPoll is name's persisted last poll, zero when it has never polled
// or is not in the directory.
func (s *Store) AgentLastPoll(ctx context.Context, name string) (time.Time, error) {
	var ms sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT last_poll_at FROM agents WHERE name = ?`, name).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !ms.Valid) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(ms.Int64), nil
}

// AgentsLastPoll returns the persisted last poll of every agent that has
// polled at least once.
func (s *Store) AgentsLastPoll(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, last_poll_at FROM agents WHERE last_poll_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var ms int64
		if err := rows.Scan(&name, &ms); err != nil {
			return nil, err
		}
		out[name] = time.UnixMilli(ms)
	}
	return out, rows.Err()
}
