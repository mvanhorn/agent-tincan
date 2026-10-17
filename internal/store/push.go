package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PushSubscription is one admin phone's Web Push subscription: the push
// service endpoint and the browser's keys, which are secrets, plus the
// device that subscribed. ID is the relay's own name for it, the only part
// that appears in the audit log.
type PushSubscription struct {
	ID        string
	Endpoint  string
	P256dh    string
	Auth      string
	Device    string // the subscribing device's machine name
	Node      string // its tailnet node id
	CreatedAt time.Time
	UpdatedAt time.Time
}

// migratePush creates the push subscription table, one row per endpoint.
func (s *Store) migratePush() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS push_subscriptions (
  id         TEXT PRIMARY KEY,
  endpoint   TEXT NOT NULL UNIQUE,
  p256dh     TEXT NOT NULL,
  auth       TEXT NOT NULL,
  device     TEXT NOT NULL DEFAULT '',
  node       TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`)
	return err
}

const pushCols = `id, endpoint, p256dh, auth, device, node, created_at, updated_at`

func scanPush(sc scanner) (PushSubscription, error) {
	var p PushSubscription
	var created, updated int64
	if err := sc.Scan(&p.ID, &p.Endpoint, &p.P256dh, &p.Auth, &p.Device, &p.Node, &created, &updated); err != nil {
		return PushSubscription{}, err
	}
	p.CreatedAt, p.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return p, nil
}

// UpsertPushSubscription stores sub under its endpoint: a new row, or new
// keys and device for the endpoint's existing row, which keeps its id. It
// reports whether the row is new.
func (s *Store) UpsertPushSubscription(ctx context.Context, sub PushSubscription) (PushSubscription, bool, error) {
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PushSubscription{}, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE push_subscriptions SET p256dh = ?, auth = ?, device = ?, node = ?, updated_at = ? WHERE endpoint = ?`,
		sub.P256dh, sub.Auth, sub.Device, sub.Node, now, sub.Endpoint)
	if err != nil {
		return PushSubscription{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PushSubscription{}, false, err
	}
	if n == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO push_subscriptions (`+pushCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			randomID(), sub.Endpoint, sub.P256dh, sub.Auth, sub.Device, sub.Node, now, now); err != nil {
			return PushSubscription{}, false, err
		}
	}
	out, err := scanPush(tx.QueryRowContext(ctx, `SELECT `+pushCols+` FROM push_subscriptions WHERE endpoint = ?`, sub.Endpoint))
	if err != nil {
		return PushSubscription{}, false, err
	}
	return out, n == 0, tx.Commit()
}

// DeletePushSubscription removes the subscription for endpoint and returns
// its id, or false when there was none.
func (s *Store) DeletePushSubscription(ctx context.Context, endpoint string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ? RETURNING id`, endpoint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// DeletePushSubscriptionID removes the subscription with id, reporting
// whether it was there.
func (s *Store) DeletePushSubscriptionID(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PushSubscriptions lists every subscription, oldest first.
func (s *Store) PushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pushCols+` FROM push_subscriptions ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		p, err := scanPush(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
