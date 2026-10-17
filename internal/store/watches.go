package store

import (
	"context"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Watch is one agent's request to be told once when target next becomes
// idle. It lapses at Expires if it has not fired by then.
type Watch struct {
	ID      int64
	Watcher string
	Target  string
	Created time.Time
	Expires time.Time
}

// migrateWatches creates the watches table. There is no foreign key to
// agents, since a rebind re-inserts the agent row; DeleteAgent drops an
// agent's watches instead. The partial unique index keeps one pending watch
// per watcher and target, while fired ones stay until they expire so a
// second fire can see it already happened.
func (s *Store) migrateWatches() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS watches (
  id         INTEGER PRIMARY KEY,
  watcher    TEXT NOT NULL,
  target     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  fired      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS watches_pending ON watches(watcher, target) WHERE fired = 0;`)
	return err
}

// AddWatch records that watcher wants to be told when target is next idle,
// for ttl. A pending watch for the same pair is returned as it is, with
// created false, so asking twice still sends one notice.
func (s *Store) AddWatch(ctx context.Context, watcher, target string, ttl time.Duration) (Watch, bool, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Watch{}, false, err
	}
	defer tx.Rollback()
	// A pending watch past its expiry would hold the pair's slot.
	if _, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE watcher = ? AND target = ? AND expires_at <= ?`, watcher, target, now.UnixMilli()); err != nil {
		return Watch{}, false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO watches (watcher, target, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		watcher, target, now.UnixMilli(), now.Add(ttl).UnixMilli())
	if err != nil {
		return Watch{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Watch{}, false, err
	}
	w, err := scanWatch(tx.QueryRowContext(ctx, `SELECT id, watcher, target, created_at, expires_at FROM watches WHERE watcher = ? AND target = ? AND fired = 0`, watcher, target))
	if err != nil {
		return Watch{}, false, err
	}
	return w, n > 0, tx.Commit()
}

// PendingWatches lists the watches that have neither fired nor expired,
// oldest first, and deletes every watch past its expiry, fired or not.
func (s *Store) PendingWatches(ctx context.Context) ([]Watch, error) {
	now := s.now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM watches WHERE expires_at <= ?`, now); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, watcher, target, created_at, expires_at FROM watches WHERE fired = 0 AND expires_at > ? ORDER BY id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// FireWatch marks w fired and queues notice for its watcher in one
// transaction, so a crash leaves neither and a racing or repeated fire
// (the sweep and the post-reply check, or a restarted relay) queues
// nothing. It reports false when w had already fired or expired.
func (s *Store) FireWatch(ctx context.Context, w Watch, notice envelope.Request, ttl time.Duration) (envelope.Request, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE watches SET fired = 1 WHERE id = ? AND fired = 0 AND expires_at > ?`, w.ID, s.now().UnixMilli())
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

func scanWatch(sc scanner) (Watch, error) {
	var w Watch
	var created, expires int64
	if err := sc.Scan(&w.ID, &w.Watcher, &w.Target, &created, &expires); err != nil {
		return Watch{}, err
	}
	w.Created, w.Expires = time.UnixMilli(created), time.UnixMilli(expires)
	return w, nil
}
