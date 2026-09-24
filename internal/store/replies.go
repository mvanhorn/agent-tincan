package store

import (
	"context"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// MaxUnseenReplies bounds how many unseen replies one call returns.
const MaxUnseenReplies = 50

// replyStatuses are the terminal statuses a reply sets; only these requests
// carry a reply the asker can see.
var replyStatuses = []any{string(envelope.StatusAnswered), string(envelope.StatusFailed), string(envelope.StatusDeclined)}

const replyStatusIn = `status IN (?, ?, ?)`

// migrateReplySeen adds reply_seen_at to a requests table created before
// replies were tracked as seen by the asker. Replies already stored are
// marked seen at their last update, so an upgrade does not hand every asker
// its whole history as new. It is a no-op on a current table.
func (s *Store) migrateReplySeen() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'reply_seen_at')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`ALTER TABLE requests ADD COLUMN reply_seen_at INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE requests SET reply_seen_at = updated_at WHERE `+replyStatusIn, replyStatuses...); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS requests_from_seen ON requests(from_agent, reply_seen_at)`)
	return err
}

// ParentPreviewChars bounds the parent body an unseen reply carries; the
// client shows a shorter preview still.
const ParentPreviewChars = 1000

// UnseenReplies returns up to MaxUnseenReplies of agent's own requests that
// have a reply agent has not seen yet, oldest reply first. A request asked
// while agent was handling a request addressed to it carries that parent's
// id, sender, body preview, and current status.
func (s *Store) UnseenReplies(ctx context.Context, agent string) ([]envelope.Result, error) {
	args := append([]any{ParentPreviewChars, agent}, replyStatuses...)
	args = append(args, MaxUnseenReplies)
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixed("q.", requestCols)+`, p.from_agent, p.status, p.body, p.created_at, p.attachments,
		COALESCE(par.id, ''), COALESCE(par.from_agent, ''), COALESCE(substr(par.body, 1, ?), ''), COALESCE(par.status, '')
		FROM requests q JOIN replies p ON p.request_id = q.id
		LEFT JOIN requests par ON q.parent_id != '' AND par.id = q.parent_id AND par.to_agent = q.from_agent
		WHERE q.from_agent = ? AND q.reply_seen_at = 0 AND q.`+replyStatusIn+`
		ORDER BY p.created_at, q.id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envelope.Result
	for rows.Next() {
		var rep envelope.Reply
		var repStatus string
		var repCreated int64
		var par envelope.Parent
		var parStatus, repAtts string
		req, st, err := scanRequest(extraCols{rows, []any{&rep.From, &repStatus, &rep.Body, &repCreated, &repAtts, &par.ID, &par.From, &par.Body, &parStatus}})
		if err != nil {
			return nil, err
		}
		if rep.Attachments, err = decodeAttachments(repAtts); err != nil {
			return nil, err
		}
		rep.RequestID, rep.Status, rep.CreatedAt = req.ID, envelope.Status(repStatus), time.UnixMilli(repCreated).UTC()
		res := envelope.Result{Request: req, Status: st, Reply: &rep}
		if par.ID != "" {
			par.Status = envelope.Status(parStatus)
			res.Parent = &par
		}
		out = append(out, res)
	}
	return out, rows.Err()
}

// CountUnseenReplies returns how many replies to agent's requests it has not
// seen, without the MaxUnseenReplies bound.
func (s *Store) CountUnseenReplies(ctx context.Context, agent string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE from_agent = ? AND reply_seen_at = 0 AND `+replyStatusIn,
		append([]any{agent}, replyStatuses...)...).Scan(&n)
	return n, err
}

// AgentsWithUnseenReplies returns every agent that has at least one unseen
// reply to its own requests, in name order. A restarted relay uses it to
// reschedule the reply wakes its old process held only in memory.
func (s *Store) AgentsWithUnseenReplies(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT from_agent FROM requests WHERE reply_seen_at = 0 AND `+replyStatusIn+` ORDER BY from_agent`, replyStatuses...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkRepliesSeen records that agent has seen the replies to the requests in
// ids. Ids agent did not send, or that have no reply yet, are left alone.
func (s *Store) MarkRepliesSeen(ctx context.Context, agent string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{s.now().UnixMilli(), agent}
	args = append(args, replyStatuses...)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE requests SET reply_seen_at = ?
		WHERE from_agent = ? AND reply_seen_at = 0 AND `+replyStatusIn+` AND id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	return err
}

// prefixed qualifies each column in a comma-separated list with p.
func prefixed(p, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, c := range parts {
		parts[i] = p + c
	}
	return strings.Join(parts, ", ")
}

// extraCols scans a request row that carries more columns after
// requestCols, so scanRequest still decodes the request part.
type extraCols struct {
	sc    scanner
	extra []any
}

func (e extraCols) Scan(dest ...any) error { return e.sc.Scan(append(dest, e.extra...)...) }
