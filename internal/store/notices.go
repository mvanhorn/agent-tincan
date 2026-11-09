package store

import (
	"context"
	"database/sql"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// A notice the relay sends an asker about a stale claim names the request
// it is about (notice_for) and the note it carries (notice_kind). When that
// request closes, by a terminal reply or a cancel, a notice still queued
// for the asker is closed in the same transaction, so the asker never reads
// it after the answer. Requests that are not such notices have both empty,
// as do notices queued by a relay that predates them, which still deliver.

// migrateNoticeLinks adds notice_for and notice_kind to requests. It is a
// no-op on a current table.
func (s *Store) migrateNoticeLinks() error {
	for _, col := range []string{"notice_for", "notice_kind"} {
		var has bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = ?)`, col).Scan(&has); err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS requests_notice_for ON requests(notice_for) WHERE notice_for != ''`)
	return err
}

// closeNoticesTx closes, without delivery, the queued stale-claim notices
// about request id inside tx and returns their ids. A notice already handed
// to the asker's poller is left alone.
func (s *Store) closeNoticesTx(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `UPDATE requests SET status = ?, lease_until = 0, updated_at = ?
		WHERE notice_for = ? AND notice_kind = ? AND status = ? RETURNING id`,
		string(envelope.StatusCancelled), s.now().UnixMilli(), id, NoteStaleClaim, string(envelope.StatusQueued))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		ids = append(ids, n)
	}
	return ids, rows.Err()
}
