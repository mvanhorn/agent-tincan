package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// MaxSchedules caps the live schedules one agent can own.
const MaxSchedules = 32

// ErrScheduleLimit is AddSchedule's answer when the owner already has
// MaxSchedules live schedules.
var ErrScheduleLimit = errors.New("too many scheduled asks; cancel one first")

// migrateSchedules creates the schedules table. There is no foreign key to
// agents, since a rebind re-inserts the agent row; DeleteAgent drops an
// agent's schedules instead. AUTOINCREMENT keeps a cancelled schedule's id
// from coming back, so an id only ever names one schedule. blocked marks a
// recurring schedule whose last fire was rejected, so a run of rejections
// tells the asker once.
func (s *Store) migrateSchedules() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schedules (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  from_agent   TEXT NOT NULL,
  to_agent     TEXT NOT NULL,
  body         TEXT NOT NULL,
  urgent       INTEGER NOT NULL DEFAULT 0,
  every_s      INTEGER NOT NULL DEFAULT 0,
  next_at      INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  last_request TEXT NOT NULL DEFAULT '',
  blocked      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS schedules_next ON schedules(next_at);
CREATE INDEX IF NOT EXISTS schedules_owner ON schedules(from_agent);`)
	return err
}

const scheduleCols = `id, from_agent, to_agent, body, urgent, every_s, next_at, created_at, last_request, blocked`

// AddSchedule stores a new schedule. Both agents must still be in the
// directory and the owner under MaxSchedules: the checks run in the insert's
// transaction, after its first write, so a DeleteAgent or another AddSchedule
// cannot land between them. It returns ErrNoAgent or ErrScheduleLimit
// otherwise.
func (s *Store) AddSchedule(ctx context.Context, sch envelope.Schedule) (envelope.Schedule, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Schedule{}, err
	}
	defer tx.Rollback()
	out, err := scanSchedule(tx.QueryRowContext(ctx, `INSERT INTO schedules (from_agent, to_agent, body, urgent, every_s, next_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING `+scheduleCols,
		sch.From, sch.To, sch.Body, sch.Urgent, sch.EverySeconds, sch.NextAt.UnixMilli(), s.now().UnixMilli()))
	if err != nil {
		return envelope.Schedule{}, err
	}
	var known, live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE name IN (?, ?)`, sch.From, sch.To).Scan(&known); err != nil {
		return envelope.Schedule{}, err
	}
	if known != 2 {
		return envelope.Schedule{}, ErrNoAgent
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schedules WHERE from_agent = ?`, sch.From).Scan(&live); err != nil {
		return envelope.Schedule{}, err
	}
	if live > MaxSchedules {
		return envelope.Schedule{}, ErrScheduleLimit
	}
	return out, tx.Commit()
}

// Schedules lists owner's schedules, or every schedule with owner "", oldest
// first.
func (s *Store) Schedules(ctx context.Context, owner string, after int64, limit int) ([]envelope.Schedule, error) {
	return s.querySchedules(ctx, `SELECT `+scheduleCols+` FROM schedules WHERE ? IN ('', from_agent) AND id > ? ORDER BY id LIMIT ?`, owner, after, limit)
}

// DueSchedules lists the schedules whose next time has come, earliest first.
func (s *Store) DueSchedules(ctx context.Context) ([]envelope.Schedule, error) {
	return s.querySchedules(ctx, `SELECT `+scheduleCols+` FROM schedules WHERE next_at <= ? ORDER BY next_at, id`, s.now().UnixMilli())
}

func (s *Store) querySchedules(ctx context.Context, query string, args ...any) ([]envelope.Schedule, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envelope.Schedule
	for rows.Next() {
		sch, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, rows.Err()
}

// AdvanceSchedule takes one due occurrence of sch, as DueSchedules read it:
// it deletes a one-shot, or moves a recurring schedule to its first time
// after now on its own cadence, so occurrences missed while the relay was
// down collapse into this one. The update is conditional on the next time
// sch was read with, so of any number of racing sweeps one reports true, and
// only that one may send. It reports false when the schedule moved on or
// went since.
func (s *Store) AdvanceSchedule(ctx context.Context, sch envelope.Schedule) (bool, error) {
	var res sql.Result
	var err error
	at := sch.NextAt.UnixMilli()
	if sch.EverySeconds == 0 {
		res, err = s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ? AND next_at = ?`, sch.ID, at)
	} else {
		every := sch.EverySeconds * time.Second.Milliseconds()
		next := at + ((s.now().UnixMilli()-at)/every+1)*every
		res, err = s.db.ExecContext(ctx, `UPDATE schedules SET next_at = ? WHERE id = ? AND next_at = ?`, next, sch.ID, at)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ScheduleFired records requestID as the schedule's latest request, which
// the next occurrence checks is closed, and ends any run of rejections. A
// schedule gone by now (a one-shot) is left alone.
func (s *Store) ScheduleFired(ctx context.Context, id int64, requestID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET last_request = ?, blocked = 0 WHERE id = ?`, requestID, id)
	return err
}

// BlockSchedule marks recurring schedule id rejected and, for the first
// rejection in a run, queues notice for its owner in the same transaction.
// It reports whether it queued; it does not for a later rejection in the
// run, or for a schedule that has gone.
func (s *Store) BlockSchedule(ctx context.Context, id int64, notice envelope.Request, ttl time.Duration) (envelope.Request, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE schedules SET blocked = 1 WHERE id = ? AND blocked = 0`, id)
	if err != nil {
		return envelope.Request{}, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return envelope.Request{}, false, err
	}
	if notice, err = s.enqueueTx(ctx, tx, notice, ttl); err != nil {
		return envelope.Request{}, false, err
	}
	return notice, true, tx.Commit()
}

// CancelSchedule deletes schedule id and returns it. With an owner it
// touches only that owner's schedule; "" is an admin's, any schedule. It
// returns ErrNotFound when there is no such schedule, so a caller learns
// nothing about another agent's.
func (s *Store) CancelSchedule(ctx context.Context, id int64, owner string) (envelope.Schedule, error) {
	sch, err := scanSchedule(s.db.QueryRowContext(ctx, `DELETE FROM schedules WHERE id = ? AND ? IN ('', from_agent) RETURNING `+scheduleCols, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return envelope.Schedule{}, ErrNotFound
	}
	return sch, err
}

func scanSchedule(sc scanner) (envelope.Schedule, error) {
	var sch envelope.Schedule
	var next, created int64
	if err := sc.Scan(&sch.ID, &sch.From, &sch.To, &sch.Body, &sch.Urgent, &sch.EverySeconds, &next, &created, &sch.LastRequest, &sch.Blocked); err != nil {
		return envelope.Schedule{}, err
	}
	sch.NextAt, sch.CreatedAt = time.UnixMilli(next).UTC(), time.UnixMilli(created).UTC()
	return sch, nil
}
