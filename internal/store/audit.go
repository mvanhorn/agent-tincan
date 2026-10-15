package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const auditSchema = `
CREATE TABLE IF NOT EXISTS audit (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  at         INTEGER NOT NULL,
  event      TEXT NOT NULL,
  request_id TEXT NOT NULL DEFAULT '',
  trace_id   TEXT NOT NULL DEFAULT '',
  actor      TEXT NOT NULL DEFAULT '',
  detail     TEXT NOT NULL DEFAULT '',
  prev_hash  TEXT NOT NULL,
  hash       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_trace ON audit(trace_id);
CREATE INDEX IF NOT EXISTS audit_actor_at ON audit(actor, at);
`

// AuditEvent is one entry in the append-only audit log.
type AuditEvent struct {
	Seq       int64     `json:"seq"`
	At        time.Time `json:"at"`
	Event     string    `json:"event"`
	RequestID string    `json:"request_id,omitempty"`
	TraceID   string    `json:"trace_id,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	PrevHash  string    `json:"prev_hash"`
	Hash      string    `json:"hash"`
}

func (s *Store) ensureAudit() error {
	_, err := s.db.Exec(auditSchema)
	return err
}

func auditHash(prev string, e AuditEvent) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%d\n%s\n%s\n%s\n%s\n%s", prev, e.At.UnixMilli(), e.Event, e.RequestID, e.TraceID, e.Actor, e.Detail)
	return hex.EncodeToString(h.Sum(nil))
}

// Audit appends an event, chaining its hash to the previous entry.
func (s *Store) Audit(ctx context.Context, e AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev string
	err = tx.QueryRowContext(ctx, `SELECT hash FROM audit ORDER BY seq DESC LIMIT 1`).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	e.At = s.now().UTC().Truncate(time.Millisecond)
	e.PrevHash = prev
	e.Hash = auditHash(prev, e)
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(at, event, request_id, trace_id, actor, detail, prev_hash, hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.At.UnixMilli(), e.Event, e.RequestID, e.TraceID, e.Actor, e.Detail, e.PrevHash, e.Hash); err != nil {
		return err
	}
	return tx.Commit()
}

// VerifyAudit walks the log and reports the first entry whose hash does not
// match its contents or its predecessor.
func (s *Store) VerifyAudit(ctx context.Context) (checked int, err error) {
	events, err := s.auditWhere(ctx, "")
	if err != nil {
		return 0, err
	}
	prev := ""
	for _, e := range events {
		if e.PrevHash != prev || auditHash(prev, e) != e.Hash {
			return checked, fmt.Errorf("audit log broken at entry %d (%s %s)", e.Seq, e.Event, e.RequestID)
		}
		prev = e.Hash
		checked++
	}
	return checked, nil
}

// AuditEvents returns the whole audit log, oldest first.
func (s *Store) AuditEvents(ctx context.Context) ([]AuditEvent, error) {
	return s.auditWhere(ctx, "")
}

// AuditForTrace returns every audit entry for a trace, oldest first.
func (s *Store) AuditForTrace(ctx context.Context, traceID string) ([]AuditEvent, error) {
	return s.auditWhere(ctx, "WHERE trace_id = ?", traceID)
}

// AuditForActor returns actor's audit entries of the given events from
// since to until, oldest first, at most limit of them.
func (s *Store) AuditForActor(ctx context.Context, actor string, since, until time.Time, limit int, events ...string) ([]AuditEvent, error) {
	if len(events) == 0 || limit <= 0 {
		return nil, nil
	}
	args := []any{actor, since.UnixMilli(), until.UnixMilli()}
	for _, e := range events {
		args = append(args, e)
	}
	args = append(args, limit)
	return s.auditQuery(ctx, "WHERE actor = ? AND at >= ? AND at <= ? AND event IN (?"+strings.Repeat(", ?", len(events)-1)+")", "LIMIT ?", args...)
}

// FirstAuditAfter returns actor's first audit entry of the given events with
// a seq above after and, when before is not 0, below before.
func (s *Store) FirstAuditAfter(ctx context.Context, actor string, after, before int64, events ...string) (AuditEvent, bool, error) {
	if len(events) == 0 {
		return AuditEvent{}, false, nil
	}
	if before == 0 {
		before = math.MaxInt64
	}
	args := []any{actor, after, before}
	for _, e := range events {
		args = append(args, e)
	}
	rows, err := s.auditQuery(ctx, "WHERE actor = ? AND seq > ? AND seq < ? AND event IN (?"+strings.Repeat(", ?", len(events)-1)+")", "LIMIT 1", args...)
	if err != nil || len(rows) == 0 {
		return AuditEvent{}, false, err
	}
	return rows[0], true, nil
}

// LastAuditFor returns actor's latest audit entry of the given events.
func (s *Store) LastAuditFor(ctx context.Context, actor string, events ...string) (AuditEvent, bool, error) {
	if len(events) == 0 {
		return AuditEvent{}, false, nil
	}
	args := []any{actor}
	for _, e := range events {
		args = append(args, e)
	}
	rows, err := s.auditQuery(ctx, "WHERE seq = (SELECT MAX(seq) FROM audit WHERE actor = ? AND event IN (?"+strings.Repeat(", ?", len(events)-1)+"))", "", args...)
	if err != nil || len(rows) == 0 {
		return AuditEvent{}, false, err
	}
	return rows[0], true, nil
}

func (s *Store) auditWhere(ctx context.Context, where string, args ...any) ([]AuditEvent, error) {
	return s.auditQuery(ctx, where, "", args...)
}

// auditQuery reads audit rows matching where, in seq order, with tail (a
// LIMIT clause, or "") after the ordering.
func (s *Store) auditQuery(ctx context.Context, where, tail string, args ...any) ([]AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, event, request_id, trace_id, actor, detail, prev_hash, hash FROM audit `+where+` ORDER BY seq `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var at int64
		if err := rows.Scan(&e.Seq, &at, &e.Event, &e.RequestID, &e.TraceID, &e.Actor, &e.Detail, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// TraceStep is one request in a chain with its current state.
type TraceStep = envelope.Result

// Trace returns every request in a chain, in the order they were sent.
func (s *Store) Trace(ctx context.Context, traceID string) ([]TraceStep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM requests WHERE trace_id = ? ORDER BY created_at, hop`, traceID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]TraceStep, 0, len(ids))
	for _, id := range ids {
		req, st, err := s.lookup(ctx, id)
		if err != nil {
			return nil, err
		}
		rep, err := s.replyFor(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, TraceStep{Request: req, Status: st, Reply: rep, Progress: req.Progress, Exchanges: req.Exchanges})
	}
	return out, nil
}

// RecentTraces returns the newest chain ids with their first request. A
// trace starts at hop 1, or with no parent: an ask a session files under a
// trace of its own still carries the hop of the chain it is checked
// against (see policy.Prepare).
func (s *Store) RecentTraces(ctx context.Context, limit int, excludePings bool) ([]TraceStep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestCols+` FROM requests WHERE (hop = 1 OR parent_id = '') AND (? = 0 OR kind != 'ping') ORDER BY created_at DESC LIMIT ?`, excludePings, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TraceStep
	for rows.Next() {
		req, st, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, TraceStep{Request: req, Status: st, Progress: req.Progress})
	}
	return out, rows.Err()
}

// Participants lists every agent that sent or received a request in a chain.
func Participants(steps []TraceStep) []string {
	seen := map[string]bool{}
	var out []string
	for _, st := range steps {
		for _, a := range append([]string{st.Request.From, st.Request.To}, st.Request.Chain...) {
			if a != "" && !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// DetailJSON encodes a small detail map for an audit entry.
func DetailJSON(kv map[string]any) string {
	raw, err := json.Marshal(kv)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
