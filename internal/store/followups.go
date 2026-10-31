package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// ErrFollowOnGone means a follow-on already fired or was dropped.
var ErrFollowOnGone = errors.New("follow-on already fired or dropped")

// A send with then leaves one followups row, written with the request. The
// reply that answers the request marks the row due in its own transaction,
// so a restart after the reply still finds it. The row goes in the
// transaction that enqueues the follow-on or the notice that it was refused,
// and whichever deletes it is the one fire.
func (s *Store) migrateFollowups() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS followups (
		request_id TEXT PRIMARY KEY REFERENCES requests(id),
		to_agent   TEXT NOT NULL,
		body       TEXT NOT NULL,
		due_at     INTEGER NOT NULL DEFAULT 0 -- when the reply came, 0 until then
	);
	CREATE INDEX IF NOT EXISTS followups_due ON followups(due_at)`); err != nil {
		return err
	}
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'follow_of')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN follow_of TEXT NOT NULL DEFAULT ''`)
	return err
}

// followOnTx writes what enqueueTx owes a request's follow-on: the row for a
// send with then, and for a follow-on itself the delete that makes its fire
// the only one. It returns ErrFollowOnGone when that row is gone.
func (s *Store) followOnTx(ctx context.Context, tx *sql.Tx, req envelope.Request) error {
	if req.Then != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO followups (request_id, to_agent, body) VALUES (?, ?, ?)`, req.ID, req.Then.To, req.Then.Body); err != nil {
			return err
		}
	}
	if req.FollowOf == "" {
		return nil
	}
	return takeFollowOnTx(ctx, tx, req.FollowOf)
}

func takeFollowOnTx(ctx context.Context, tx *sql.Tx, id string) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM followups WHERE request_id = ? AND due_at > 0`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = ErrFollowOnGone
		}
		return err
	}
	return nil
}

// markFollowOnDueTx makes rep's request's follow-on due if rep answers it.
// Any other reply leaves the row for PruneFollowOns.
func (s *Store) markFollowOnDueTx(ctx context.Context, tx *sql.Tx, rep envelope.Reply) error {
	if rep.Status != envelope.StatusAnswered {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE followups SET due_at = ? WHERE request_id = ?`, s.now().UnixMilli(), rep.RequestID)
	return err
}

// FollowOnOf returns the follow-on request id asked for, whether or not it
// is due.
func (s *Store) FollowOnOf(ctx context.Context, id string) (envelope.Then, bool, error) {
	var then envelope.Then
	err := s.db.QueryRowContext(ctx, `SELECT to_agent, body FROM followups WHERE request_id = ?`, id).Scan(&then.To, &then.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return envelope.Then{}, false, nil
	}
	return then, err == nil, err
}

// DueFollowOn is a follow-on ready to fire: what it asks, the reply it
// carries, and the session the first request came from.
type DueFollowOn struct {
	Then        envelope.Then
	Reply       string
	FromSession string
}

// DueFollowOn returns request id's follow-on if it is due.
func (s *Store) DueFollowOn(ctx context.Context, id string) (DueFollowOn, bool, error) {
	var d DueFollowOn
	err := s.db.QueryRowContext(ctx, `SELECT f.to_agent, f.body, r.body, q.from_session FROM followups f
		JOIN replies r ON r.request_id = f.request_id JOIN requests q ON q.id = f.request_id
		WHERE f.request_id = ? AND f.due_at > 0`, id).Scan(&d.Then.To, &d.Then.Body, &d.Reply, &d.FromSession)
	if errors.Is(err, sql.ErrNoRows) {
		return DueFollowOn{}, false, nil
	}
	return d, err == nil, err
}

// DueFollowOns lists the requests whose follow-on is due, oldest first.
func (s *Store) DueFollowOns(ctx context.Context) ([]string, error) {
	return s.ids(ctx, `SELECT request_id FROM followups WHERE due_at > 0 ORDER BY due_at`)
}

// DropFollowOn ends request id's due follow-on without sending it, enqueuing
// notice, when there is one, in the same transaction, so the asker is told
// exactly once. It returns ErrFollowOnGone when the row is gone.
func (s *Store) DropFollowOn(ctx context.Context, id string, notice *envelope.Request, ttl time.Duration) (envelope.Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, err
	}
	defer tx.Rollback()
	if err := takeFollowOnTx(ctx, tx, id); err != nil {
		return envelope.Request{}, err
	}
	var n envelope.Request
	if notice != nil {
		if n, err = s.enqueueTx(ctx, tx, *notice, ttl); err != nil {
			return envelope.Request{}, err
		}
	}
	return n, tx.Commit()
}

// PruneFollowOns deletes the follow-ons of requests that closed without an
// answer (failed, declined, cancelled or expired) and returns those
// requests' ids.
func (s *Store) PruneFollowOns(ctx context.Context) ([]string, error) {
	return s.ids(ctx, `DELETE FROM followups WHERE due_at = 0 AND request_id IN
		(SELECT id FROM requests WHERE status IN (?, ?, ?, ?)) RETURNING request_id`,
		string(envelope.StatusFailed), string(envelope.StatusDeclined), string(envelope.StatusCancelled), string(envelope.StatusExpired))
}
