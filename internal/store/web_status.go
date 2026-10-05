package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// WebStatus retains clear observations as ordering tombstones.
type WebStatus struct {
	NodeID     string
	JoinedAt   time.Time
	Site       string
	ObservedAt time.Time
	Since      time.Time
	Pending    bool
	RetryAt    time.Time
}

func (s *Store) migrateWebStatus() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS web_status (
 agent TEXT PRIMARY KEY REFERENCES agents(name) ON DELETE CASCADE,
 node_id TEXT NOT NULL, joined_at INTEGER NOT NULL, site TEXT NOT NULL,
 observed_at INTEGER NOT NULL, since INTEGER, notice_pending INTEGER NOT NULL DEFAULT 0, notice_retry_at INTEGER NOT NULL DEFAULT 0)`)
	return err
}

// SetWebStatus accepts only a newer observation for the current binding.
func (s *Store) SetWebStatus(ctx context.Context, a identity.Agent, in envelope.WebStatus, now time.Time) error {
	var since any
	if in.State == "signed_out" {
		since = now.UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_status(agent,node_id,joined_at,site,observed_at,since,notice_pending)
 SELECT name,node_id,joined_at,?,?,?,? FROM agents WHERE name=? AND node_id=? AND joined_at=?
 ON CONFLICT(agent) DO UPDATE SET site=excluded.site, observed_at=excluded.observed_at,
 since=CASE WHEN excluded.since IS NULL THEN NULL ELSE COALESCE(web_status.since,excluded.since) END,
 notice_pending=CASE WHEN excluded.since IS NULL THEN 0 WHEN web_status.since IS NULL THEN 1 ELSE web_status.notice_pending END,
 notice_retry_at=CASE WHEN excluded.since IS NULL OR web_status.since IS NULL THEN 0 ELSE web_status.notice_retry_at END
 WHERE excluded.observed_at > web_status.observed_at`, in.Site, in.ObservedAt.UnixMilli(), since, in.State == "signed_out", a.Name, a.NodeID, a.JoinedAt.UnixMilli())
	return err
}

// WebStatuses reads the roster's facts in one snapshot, outside relay locks.
func (s *Store) WebStatuses(ctx context.Context) (map[string]WebStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent,node_id,joined_at,site,observed_at,since,notice_pending,notice_retry_at FROM web_status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WebStatus{}
	for rows.Next() {
		var name string
		var v WebStatus
		var joined, observed, retry int64
		var since sql.NullInt64
		if err := rows.Scan(&name, &v.NodeID, &joined, &v.Site, &observed, &since, &v.Pending, &retry); err != nil {
			return nil, err
		}
		v.RetryAt = time.UnixMilli(retry)
		v.JoinedAt = time.UnixMilli(joined)
		v.ObservedAt = time.UnixMilli(observed)
		if since.Valid {
			v.Since = time.UnixMilli(since.Int64)
		}
		out[name] = v
	}
	return out, rows.Err()
}

// Apply adds facts only for the directory identity that made the report.
func (v WebStatus) Apply(a identity.Agent, target *envelope.Target) {
	if v.NodeID != a.NodeID || v.JoinedAt.UnixMilli() != a.JoinedAt.UnixMilli() || v.ObservedAt.IsZero() {
		return
	}
	target.WebStatusObservedAt = v.ObservedAt
	target.WebHost = a.NodeName
	if !v.Since.IsZero() {
		target.SignedOutSite = v.Site
		target.SignedOutSince = v.Since
	}
}

// EnqueueWebNotice queues metadata and marks its episode notified atomically.
// Recovery or rebinding while a notice is prepared makes this a no-op.
func (s *Store) EnqueueWebNotice(ctx context.Context, agent string, status WebStatus, to, body string, ttl time.Duration) (envelope.Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, err
	}
	defer tx.Rollback()
	var eligible bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM web_status WHERE agent=? AND node_id=? AND joined_at=? AND since=? AND notice_pending=1)`, agent, status.NodeID, status.JoinedAt.UnixMilli(), status.Since.UnixMilli()).Scan(&eligible)
	if err != nil || !eligible {
		return envelope.Request{}, err
	}
	var recipient bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE name=?)`, to).Scan(&recipient); err != nil {
		return envelope.Request{}, err
	}
	if !recipient {
		return envelope.Request{}, fmt.Errorf("operator recipient is not joined")
	}
	now := s.now()
	id := randomID()
	req := envelope.Request{ID: id, TraceID: id, From: "relay", To: to, Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: body, CreatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO requests(id,from_agent,to_agent,trace_id,hop,chain,kind,body,status,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, "relay", to, id, 1, `["relay"]`, string(req.Kind), body, string(envelope.StatusQueued), now.UnixMilli(), now.UnixMilli(), now.Add(ttl).UnixMilli())
	if err != nil {
		return envelope.Request{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE web_status SET notice_pending=0 WHERE agent=?`, agent); err != nil {
		return envelope.Request{}, err
	}
	return req, tx.Commit()
}

// RetryWebNotice bounds failed queue attempts across relay restarts.
func (s *Store) RetryWebNotice(ctx context.Context, agent string, status WebStatus, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE web_status SET notice_retry_at=? WHERE agent=? AND node_id=? AND joined_at=? AND since=?`, at.UnixMilli(), agent, status.NodeID, status.JoinedAt.UnixMilli(), status.Since.UnixMilli())
	return err
}
