package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

var (
	// ErrAttachmentNotFound means no finished upload has that id.
	ErrAttachmentNotFound = errors.New("no such attachment")
	// ErrAttachmentNotYours means a message names an attachment another
	// agent uploaded. An agent attaches only its own uploads.
	ErrAttachmentNotYours = errors.New("attachment was uploaded by another agent")
	// ErrAttachmentInUse means the attachment is already on a message. Each
	// upload rides on one message; upload the file again to send it again.
	ErrAttachmentInUse = errors.New("attachment is already on another message")
	// ErrAttachmentDeleted means retention has removed the file.
	ErrAttachmentDeleted = errors.New("attachment has been deleted")
	// ErrAgentQuota means the uploader's stored attachments would pass its
	// quota.
	ErrAgentQuota = errors.New("agent attachment quota exceeded")
	// ErrRelayQuota means the relay's stored attachments would pass its
	// quota.
	ErrRelayQuota = errors.New("relay attachment storage is full")
)

// attachmentSchema is the attachments table. size is the reservation while
// an upload is in flight (ready = 0) and the real size once it is committed.
// request_id is set when a request or its reply first carries the
// attachment. deleted_at is set once retention has removed the file; the
// row stays for the record.
const attachmentSchema = `
CREATE TABLE IF NOT EXISTS attachments (
  id          TEXT PRIMARY KEY,
  uploader    TEXT NOT NULL,
  name        TEXT NOT NULL DEFAULT '',
  mime        TEXT NOT NULL DEFAULT '',
  size        INTEGER NOT NULL,
  sha256      TEXT NOT NULL DEFAULT '',
  ready       INTEGER NOT NULL DEFAULT 0,
  request_id  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  deleted_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS attachments_uploader ON attachments(uploader, deleted_at);
CREATE INDEX IF NOT EXISTS attachments_request ON attachments(request_id);
CREATE INDEX IF NOT EXISTS attachments_live ON attachments(deleted_at, size);
`

// AttachmentQuota caps the bytes of attachments the relay keeps, per
// uploader and in total. Deleted attachments do not count.
type AttachmentQuota struct {
	PerAgent int64
	Total    int64
}

// AttachmentRecord is an attachment's stored metadata.
type AttachmentRecord struct {
	envelope.Attachment
	SHA256    string
	Uploader  string
	RequestID string // the request that carries it, on the request or its reply; "" until sent
	Ready     bool   // the upload finished and the file is in place
	CreatedAt time.Time
	DeletedAt time.Time // zero while the file is kept
}

// migrateAttachments adds the attachments table, and an attachments column
// to requests and replies tables created before attachments. It is a no-op
// on a current database.
func (s *Store) migrateAttachments() error {
	if _, err := s.db.Exec(attachmentSchema); err != nil {
		return err
	}
	for _, table := range []string{"requests", "replies"} {
		var has bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('` + table + `') WHERE name = 'attachments')`).Scan(&has); err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN attachments TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// ReserveAttachment records an upload about to be written, counting reserve
// bytes against the quotas before any byte reaches disk. It returns the new
// attachment's id. Finish with CommitAttachment, or DropAttachment on
// failure; a reservation never committed is swept as an orphan.
func (s *Store) ReserveAttachment(ctx context.Context, uploader, name string, reserve int64, q AttachmentQuota) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var mine, total int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN uploader = ? THEN size ELSE 0 END), 0), COALESCE(SUM(size), 0)
		FROM attachments WHERE deleted_at = 0`, uploader).Scan(&mine, &total); err != nil {
		return "", err
	}
	if mine+reserve > q.PerAgent {
		return "", fmt.Errorf("%w: %s holds %d of %d bytes", ErrAgentQuota, uploader, mine, q.PerAgent)
	}
	if total+reserve > q.Total {
		return "", fmt.Errorf("%w: %d of %d bytes in use", ErrRelayQuota, total, q.Total)
	}
	id := randomID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id, uploader, name, size, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, uploader, name, reserve, s.now().UnixMilli()); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// CommitAttachment marks a reserved upload finished, with its real size,
// media type, and sha256.
func (s *Store) CommitAttachment(ctx context.Context, id string, size int64, mime, sha string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE attachments SET size = ?, mime = ?, sha256 = ?, ready = 1 WHERE id = ? AND ready = 0 AND deleted_at = 0`,
		size, mime, sha, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAttachmentNotFound
	}
	return nil
}

// DropAttachment forgets an upload that failed before it was committed.
func (s *Store) DropAttachment(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE id = ? AND request_id = ''`, id)
	return err
}

// Attachment returns an attachment's metadata, finished or not.
func (s *Store) Attachment(ctx context.Context, id string) (AttachmentRecord, error) {
	var a AttachmentRecord
	var ready int
	var created, deleted int64
	err := s.db.QueryRowContext(ctx, `SELECT id, uploader, name, mime, size, sha256, ready, request_id, created_at, deleted_at FROM attachments WHERE id = ?`, id).
		Scan(&a.ID, &a.Uploader, &a.Name, &a.MIME, &a.Size, &a.SHA256, &ready, &a.RequestID, &created, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return AttachmentRecord{}, ErrAttachmentNotFound
	}
	if err != nil {
		return AttachmentRecord{}, err
	}
	a.Ready = ready == 1
	a.CreatedAt = time.UnixMilli(created).UTC()
	if deleted != 0 {
		a.DeletedAt = time.UnixMilli(deleted).UTC()
	}
	return a, nil
}

// bindAttachments checks that sender uploaded every attachment in atts,
// that each upload finished and is on no other message, and ties them to
// requestID inside tx. It returns the attachments filled in from the
// stored metadata, so the message carries the relay's name, media type,
// and size rather than the client's.
func bindAttachments(ctx context.Context, tx *sql.Tx, atts []envelope.Attachment, sender, requestID string) ([]envelope.Attachment, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	out := make([]envelope.Attachment, 0, len(atts))
	for _, in := range atts {
		var a envelope.Attachment
		var uploader, bound string
		var ready int
		var deleted int64
		err := tx.QueryRowContext(ctx, `SELECT id, name, mime, size, uploader, ready, request_id, deleted_at FROM attachments WHERE id = ?`, in.ID).
			Scan(&a.ID, &a.Name, &a.MIME, &a.Size, &uploader, &ready, &bound, &deleted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("%w: %s", ErrAttachmentNotFound, in.ID)
		case err != nil:
			return nil, err
		case uploader != sender:
			return nil, fmt.Errorf("%w: %s", ErrAttachmentNotYours, in.ID)
		case deleted != 0:
			return nil, fmt.Errorf("%w: %s", ErrAttachmentDeleted, in.ID)
		case ready == 0:
			return nil, fmt.Errorf("%w: %s (upload not finished)", ErrAttachmentNotFound, in.ID)
		case bound != "":
			return nil, fmt.Errorf("%w: %s", ErrAttachmentInUse, in.ID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attachments SET request_id = ? WHERE id = ?`, requestID, in.ID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// bindAndEncodeAttachments binds atts to requestID inside tx, then returns
// them filled in along with their stored form.
func bindAndEncodeAttachments(ctx context.Context, tx *sql.Tx, atts []envelope.Attachment, sender, requestID string) ([]envelope.Attachment, string, error) {
	bound, err := bindAttachments(ctx, tx, atts, sender, requestID)
	if err != nil {
		return nil, "", err
	}
	enc, err := encodeAttachments(bound)
	if err != nil {
		return nil, "", err
	}
	return bound, enc, nil
}

// encodeAttachments is the stored form of a message's attachments: "" for
// none, so rows without attachments look as they did before.
func encodeAttachments(atts []envelope.Attachment) (string, error) {
	if len(atts) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(atts)
	return string(raw), err
}

func decodeAttachments(raw string) ([]envelope.Attachment, error) {
	if raw == "" {
		return nil, nil
	}
	var out []envelope.Attachment
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode attachments: %w", err)
	}
	return out, nil
}

// SweepAttachments applies retention and returns the ids whose files the
// caller should now remove. An upload on no message is an orphan: after
// orphanAge its row is deleted outright, finished or not. An attachment on
// a request is kept until keepAfterDone after the request reached a final
// state (answered, failed, declined, expired, cancelled, or claimed for a
// notify, which has no lease and never leaves claimed unless the target
// chooses to reply); then its row is marked deleted and kept, so the message
// still lists what it carried.
func (s *Store) SweepAttachments(ctx context.Context, orphanAge, keepAfterDone time.Duration) ([]string, error) {
	now := s.now()
	var ids []string
	collect := func(query string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}
	if err := collect(`DELETE FROM attachments WHERE request_id = '' AND created_at <= ? RETURNING id`,
		now.Add(-orphanAge).UnixMilli()); err != nil {
		return nil, err
	}
	if err := collect(`UPDATE attachments SET deleted_at = ?
		WHERE deleted_at = 0 AND request_id IN (
		  SELECT id FROM requests WHERE (status IN (?, ?, ?, ?, ?) OR (kind = ? AND status = ?)) AND updated_at <= ?)
		RETURNING id`,
		now.UnixMilli(),
		string(envelope.StatusAnswered), string(envelope.StatusFailed), string(envelope.StatusDeclined),
		string(envelope.StatusExpired), string(envelope.StatusCancelled),
		string(envelope.KindNotify), string(envelope.StatusClaimed),
		now.Add(-keepAfterDone).UnixMilli()); err != nil {
		return nil, err
	}
	return ids, nil
}
