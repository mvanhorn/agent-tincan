package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Email replies.
//
// The relay records an opted-in agent's reply to a request email as its
// answer (relay/emailreplies.go). email_replies keeps one row per tagged
// message the relay decided on, keyed by AgentMail's message id, so each
// message is decided once across polls and restarts, and so the response
// email the relay owes for it is retried until it is sent. A row holds no
// mail text, tag or address: only the ids, the agent whose inbox it came
// from (so a retried response goes out through that agent's inbox), the
// outcome code and whether the response was sent.

// ErrEmailDecided means the message already has an email_replies row.
var ErrEmailDecided = errors.New("email message already decided")

// ErrLiveClaim means the request is claimed under a live lease.
var ErrLiveClaim = errors.New("request is claimed under a live lease")

// EmailOutcomeRecorded is the outcome of a message whose reply was stored.
const EmailOutcomeRecorded = "recorded"

// EmailReply is one decided message.
type EmailReply struct {
	MessageID string
	Agent     string
	RequestID string
	Outcome   string
	CreatedAt time.Time
}

// migrateEmailReplies creates the table of decided email messages. It is a
// no-op when the table exists.
func (s *Store) migrateEmailReplies() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS email_replies (message_id TEXT PRIMARY KEY, agent TEXT NOT NULL, request_id TEXT NOT NULL,
		outcome TEXT NOT NULL, response_sent INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
		CREATE INDEX IF NOT EXISTS email_replies_unsent ON email_replies(response_sent) WHERE response_sent = 0`)
	return err
}

// EmailDecided reports whether message messageID already has a row.
func (s *Store) EmailDecided(ctx context.Context, messageID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM email_replies WHERE message_id = ?`, messageID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// DecideEmail records outcome for message messageID from agent's inbox,
// naming request requestID (which may not exist: it is what the subject
// claimed). respond says whether a response email is owed; a row without
// one is stored as already sent. It returns ErrEmailDecided when the message
// already has a row.
func (s *Store) DecideEmail(ctx context.Context, messageID, agent, requestID, outcome string, respond bool) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO email_replies (message_id, agent, request_id, outcome, response_sent, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id) DO NOTHING`, messageID, agent, requestID, outcome, !respond, s.now().UnixMilli())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrEmailDecided
	}
	return nil
}

// ReplyByEmail stores agent's reply rep to request id from email message
// messageID, and the message's email_replies row (outcome recorded,
// response owed), in one transaction: either both are kept or neither.
// Unlike Reply it takes only a terminal reply, refuses a request claimed
// under a live lease (ErrLiveClaim) and one held and never approved
// (ErrWrongState), and needs no claim. It returns ErrEmailDecided when the
// message already has a row, ErrForbidden when agent is not the target and
// ErrWrongState when the request is no longer open.
func (s *Store) ReplyByEmail(ctx context.Context, messageID, id, agent string, rep envelope.Reply) (envelope.Reply, error) {
	if !rep.Status.Terminal() || len(rep.Attachments) > 0 {
		return envelope.Reply{}, ErrWrongState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Reply{}, err
	}
	defer tx.Rollback()
	var to string
	var claimLive bool
	err = tx.QueryRowContext(ctx, `SELECT to_agent, status = ? AND lease_until > ? FROM requests WHERE id = ?`,
		string(envelope.StatusClaimed), s.now().UnixMilli(), id).Scan(&to, &claimLive)
	if errors.Is(err, sql.ErrNoRows) {
		return envelope.Reply{}, ErrNotFound
	}
	if err != nil {
		return envelope.Reply{}, err
	}
	if to != agent {
		return envelope.Reply{}, ErrForbidden
	}
	if claimLive {
		return envelope.Reply{}, ErrLiveClaim
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO email_replies (message_id, agent, request_id, outcome, response_sent, created_at) VALUES (?, ?, ?, ?, 0, ?)
		ON CONFLICT(message_id) DO NOTHING`, messageID, agent, id, EmailOutcomeRecorded, s.now().UnixMilli())
	if err != nil {
		return envelope.Reply{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Reply{}, ErrEmailDecided
	}
	if rep, err = s.replyTx(ctx, tx, id, agent, rep, `NOT (status = 'claimed' AND lease_until > ?) AND NOT (was_held = 1 AND approved = 0)`); err != nil {
		return envelope.Reply{}, err
	}
	return rep, tx.Commit()
}

// ClaimLive reports whether request id is claimed under a live lease.
func (s *Store) ClaimLive(ctx context.Context, id string) (bool, error) {
	var live bool
	err := s.db.QueryRowContext(ctx, `SELECT status = ? AND lease_until > ? FROM requests WHERE id = ?`,
		string(envelope.StatusClaimed), s.now().UnixMilli(), id).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return live, err
}

// UnsentEmailResponses returns the decided messages from agent's inbox
// whose response email is still owed, oldest first.
func (s *Store) UnsentEmailResponses(ctx context.Context, agent string) ([]EmailReply, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, agent, request_id, outcome, created_at FROM email_replies
		WHERE response_sent = 0 AND agent = ? ORDER BY created_at, rowid`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmailReply
	for rows.Next() {
		var e EmailReply
		var ms int64
		if err := rows.Scan(&e.MessageID, &e.Agent, &e.RequestID, &e.Outcome, &ms); err != nil {
			return nil, err
		}
		e.CreatedAt = time.UnixMilli(ms).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkEmailResponseSent records that message messageID's response email
// was sent.
func (s *Store) MarkEmailResponseSent(ctx context.Context, messageID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE email_replies SET response_sent = 1 WHERE message_id = ?`, messageID)
	return err
}

// LastAskClosed is when agent's most recently closed ask (answered,
// failed, declined, expired or cancelled) closed, zero when it has none.
func (s *Store) LastAskClosed(ctx context.Context, agent string) (time.Time, error) {
	var ms sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(updated_at) FROM requests WHERE to_agent = ? AND kind = ? AND status IN (?, ?, ?, ?, ?)`,
		agent, string(envelope.KindAsk), string(envelope.StatusAnswered), string(envelope.StatusFailed), string(envelope.StatusDeclined),
		string(envelope.StatusExpired), string(envelope.StatusCancelled)).Scan(&ms)
	if err != nil || !ms.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(ms.Int64).UTC(), nil
}
