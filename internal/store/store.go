// Package store persists the relay's state in SQLite: the agent directory,
// invite codes, and the request queue. It is pure Go (modernc.org/sqlite), so
// release binaries build with CGO_ENABLED=0.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

var (
	// ErrNotFound means no request with that id is visible to the caller.
	ErrNotFound = errors.New("request not found")
	// ErrForbidden means the caller is not the agent allowed to do this.
	ErrForbidden = errors.New("not allowed for this agent")
	// ErrWrongState means the request is not in a state that allows this.
	ErrWrongState = errors.New("request is not in a state that allows this")
)

const schema = `
CREATE TABLE IF NOT EXISTS agents (
  name      TEXT PRIMARY KEY,
  node_id   TEXT NOT NULL,
  node_name TEXT NOT NULL,
  joined_at INTEGER NOT NULL,
  kind      TEXT,
  node_user TEXT,
  last_seen_at INTEGER
);
CREATE TABLE IF NOT EXISTS invites (
  code    TEXT PRIMARY KEY,
  name    TEXT NOT NULL,
  expires INTEGER NOT NULL,
  kind    TEXT
);
CREATE TABLE IF NOT EXISTS requests (
  id          TEXT PRIMARY KEY,
  from_agent  TEXT NOT NULL,
  to_agent    TEXT NOT NULL,
  parent_id   TEXT NOT NULL DEFAULT '',
  trace_id    TEXT NOT NULL,
  hop         INTEGER NOT NULL,
  chain       TEXT NOT NULL,
  kind        TEXT NOT NULL,
  body        TEXT NOT NULL,
  status      TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  lease_until INTEGER NOT NULL DEFAULT 0,
  reply_seen_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS requests_to_status ON requests(to_agent, status);
CREATE TABLE IF NOT EXISTS replies (
  request_id TEXT PRIMARY KEY REFERENCES requests(id),
  from_agent TEXT NOT NULL,
  status     TEXT NOT NULL,
  body       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
`

// Store is the relay's SQLite store.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the database at path. Use ":memory:" in
// tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		// One shared in-memory database per Store.
		dsn = "file:" + randomID() + "?mode=memory&cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialize in-process
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrateAgents(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agents: %w", err)
	}
	if err := s.migrateAgentLogin(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agent login: %w", err)
	}
	// After migrateAgents: its rebuild copies only the older columns, so the
	// column is added to the rebuilt table here.
	if err := s.migrateAgentLastSeen(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agent last seen: %w", err)
	}
	if err := s.migrateInvites(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate invites: %w", err)
	}
	if err := s.migrateReplySeen(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate reply seen: %w", err)
	}
	if err := s.ensureAudit(); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit schema: %w", err)
	}
	return s, nil
}

// SetClock overrides the clock in tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for packages that add their own tables (audit).
func (s *Store) DB() *sql.DB { return s.db }

// migrateAgents upgrades an agents table created before several agents could
// share one node: that table declared node_id UNIQUE and had no kind column.
// SQLite cannot drop a constraint in place, so the table is rebuilt in one
// transaction. It is a no-op on a current table.
func (s *Store) migrateAgents() error {
	hasKind, uniqueNode, err := s.agentsShape()
	if err != nil {
		return err
	}
	if hasKind && !uniqueNode {
		_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS agents_node_id ON agents(node_id)`)
		return err
	}
	kindCol := "NULL"
	if hasKind {
		kindCol = "kind"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE agents_new (
		  name      TEXT PRIMARY KEY,
		  node_id   TEXT NOT NULL,
		  node_name TEXT NOT NULL,
		  joined_at INTEGER NOT NULL,
		  kind      TEXT
		)`,
		`INSERT INTO agents_new(name, node_id, node_name, joined_at, kind)
		  SELECT name, node_id, node_name, joined_at, ` + kindCol + ` FROM agents`,
		`DROP TABLE agents`,
		`ALTER TABLE agents_new RENAME TO agents`,
		`CREATE INDEX agents_node_id ON agents(node_id)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateAgentLogin adds the node_user column to an agents table created
// before the owning login was recorded at join. Existing agents keep NULL,
// which re-admission treats as "no login recorded". It is a no-op on a
// current table.
func (s *Store) migrateAgentLogin() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('agents') WHERE name = 'node_user')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE agents ADD COLUMN node_user TEXT`)
	return err
}

// migrateAgentLastSeen adds the last_seen_at column (unix millis, NULL until
// the agent first calls the relay) to an agents table created before activity
// was persisted. It is a no-op on a current table.
func (s *Store) migrateAgentLastSeen() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('agents') WHERE name = 'last_seen_at')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE agents ADD COLUMN last_seen_at INTEGER`)
	return err
}

// migrateInvites adds the kind column to an invites table created before
// invites could carry the agent's kind. It is a no-op on a current table.
func (s *Store) migrateInvites() error {
	var hasKind bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('invites') WHERE name = 'kind')`).Scan(&hasKind); err != nil {
		return err
	}
	if hasKind {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE invites ADD COLUMN kind TEXT`)
	return err
}

// agentsShape reports whether the agents table has a kind column and whether
// node_id carries a UNIQUE constraint.
func (s *Store) agentsShape() (hasKind, uniqueNode bool, err error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('agents')`)
	if err != nil {
		return false, false, err
	}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			rows.Close()
			return false, false, err
		}
		hasKind = hasKind || col == "kind"
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	// A UNIQUE column constraint shows up as an index with origin 'u'.
	err = s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_index_list('agents') WHERE origin = 'u')`).Scan(&uniqueNode)
	return hasKind, uniqueNode, err
}

// --- identity.Store ---

func (s *Store) PutAgent(ctx context.Context, a identity.Agent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A name moving to a new machine replaces its old binding. Other agents
	// on either machine are untouched: a node may carry several names. The
	// name's last activity carries over.
	var lastSeen sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT last_seen_at FROM agents WHERE name = ?`, a.Name).Scan(&lastSeen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, a.Name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(name, node_id, node_name, joined_at, kind, node_user, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.Name, a.NodeID, a.NodeName, a.JoinedAt.UnixMilli(), nullable(a.Kind), nullable(a.NodeUser), lastSeen); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteAgent(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetAgentKind records an agent's runtime kind; "" stores NULL.
func (s *Store) SetAgentKind(ctx context.Context, name, kind string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET kind = ? WHERE name = ?`, nullable(kind), name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// TouchAgent records that name called the relay at t. It only moves the
// stored time forward and ignores names not in the directory.
func (s *Store) TouchAgent(ctx context.Context, name string, t time.Time) error {
	ms := t.UnixMilli()
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = ? WHERE name = ? AND (last_seen_at IS NULL OR last_seen_at < ?)`, ms, name, ms)
	return err
}

// AgentsLastSeen returns the persisted last activity of every agent that has
// called the relay at least once.
func (s *Store) AgentsLastSeen(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, last_seen_at FROM agents WHERE last_seen_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var ms int64
		if err := rows.Scan(&name, &ms); err != nil {
			return nil, err
		}
		out[name] = time.UnixMilli(ms)
	}
	return out, rows.Err()
}

func (s *Store) Agents(ctx context.Context) ([]identity.Agent, error) {
	return s.agentsWhere(ctx, "1 = 1")
}

func (s *Store) AgentsByNode(ctx context.Context, nodeID string) ([]identity.Agent, error) {
	return s.agentsWhere(ctx, "node_id = ?", nodeID)
}

func (s *Store) AgentByName(ctx context.Context, name string) (identity.Agent, bool, error) {
	agents, err := s.agentsWhere(ctx, "name = ?", name)
	if err != nil || len(agents) == 0 {
		return identity.Agent{}, false, err
	}
	return agents[0], true, nil
}

func (s *Store) agentsWhere(ctx context.Context, where string, args ...any) ([]identity.Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, node_id, node_name, joined_at, COALESCE(kind, ''), COALESCE(node_user, '') FROM agents WHERE `+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identity.Agent
	for rows.Next() {
		var a identity.Agent
		var joined int64
		if err := rows.Scan(&a.Name, &a.NodeID, &a.NodeName, &joined, &a.Kind, &a.NodeUser); err != nil {
			return nil, err
		}
		a.JoinedAt = time.UnixMilli(joined)
		out = append(out, a)
	}
	return out, rows.Err()
}

// nullable maps "" to SQL NULL.
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func (s *Store) PutInvite(ctx context.Context, inv identity.Invite) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO invites(code, name, expires, kind) VALUES (?, ?, ?, ?)`,
		inv.Code, inv.Name, inv.Expires.UnixMilli(), nullable(inv.Kind))
	return err
}

func (s *Store) TakeInvite(ctx context.Context, code string) (identity.Invite, bool, error) {
	var inv identity.Invite
	var exp int64
	err := s.db.QueryRowContext(ctx, `DELETE FROM invites WHERE code = ? RETURNING code, name, expires, COALESCE(kind, '')`, code).Scan(&inv.Code, &inv.Name, &exp, &inv.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Invite{}, false, nil
	}
	if err != nil {
		return identity.Invite{}, false, err
	}
	inv.Expires = time.UnixMilli(exp)
	return inv, true, nil
}

// --- request queue ---

// Enqueue stores a new request. The caller has already set From, To, Kind,
// Body, TraceID, Hop, Chain, and ParentID. Enqueue assigns ID and CreatedAt.
func (s *Store) Enqueue(ctx context.Context, req envelope.Request, ttl time.Duration) (envelope.Request, error) {
	now := s.now()
	req.ID = randomID()
	req.CreatedAt = now.UTC().Truncate(time.Millisecond)
	if req.TraceID == "" {
		req.TraceID = req.ID
	}
	chain, err := json.Marshal(req.Chain)
	if err != nil {
		return envelope.Request{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO requests
		(id, from_agent, to_agent, parent_id, trace_id, hop, chain, kind, body, status, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.From, req.To, req.ParentID, req.TraceID, req.Hop, string(chain), string(req.Kind), req.Body,
		string(envelope.StatusQueued), now.UnixMilli(), now.UnixMilli(), now.Add(ttl).UnixMilli())
	if err != nil {
		return envelope.Request{}, err
	}
	return req, nil
}

// Deliver hands up to limit queued requests for agent to its poller, marking
// them delivered under a lease. A request delivered but never claimed returns
// to the queue when the lease runs out, so a crash after polling strands
// nothing.
func (s *Store) Deliver(ctx context.Context, agent string, limit int, lease time.Duration) ([]envelope.Request, error) {
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `UPDATE requests SET status = ?, lease_until = ?, updated_at = ?
		WHERE id IN (SELECT id FROM requests WHERE to_agent = ? AND status = ? AND expires_at > ? ORDER BY created_at LIMIT ?)
		RETURNING `+requestCols,
		string(envelope.StatusDelivered), now.Add(lease).UnixMilli(), now.UnixMilli(),
		agent, string(envelope.StatusQueued), now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanRequests(rows)
	if err != nil {
		return nil, err
	}
	sortByCreated(out)
	return out, nil
}

// CountQueued returns how many requests are waiting for agent without
// delivering them.
func (s *Store) CountQueued(ctx context.Context, agent string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE to_agent = ? AND status = ? AND expires_at > ?`,
		agent, string(envelope.StatusQueued), s.now().UnixMilli()).Scan(&n)
	return n, err
}

// PendingRequests names up to limit of agent's queued requests, oldest
// first, without delivering them or reading their bodies.
func (s *Store) PendingRequests(ctx context.Context, agent string, limit int) ([]envelope.Pending, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, from_agent FROM requests
		WHERE to_agent = ? AND status = ? AND expires_at > ? ORDER BY created_at, rowid LIMIT ?`,
		agent, string(envelope.StatusQueued), s.now().UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envelope.Pending
	for rows.Next() {
		var p envelope.Pending
		if err := rows.Scan(&p.ID, &p.From); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Claim marks a request as being worked on by its target, under a lease. A
// notify gets no lease: no reply will ever close it, so a lease would requeue
// and redeliver it every ClaimLease. A claimed notify simply stays claimed.
func (s *Store) Claim(ctx context.Context, id, agent string, lease time.Duration) (envelope.Request, error) {
	req, _, err := s.lookup(ctx, id)
	if err != nil {
		return envelope.Request{}, err
	}
	if req.To != agent {
		return envelope.Request{}, ErrForbidden
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ?, lease_until = CASE WHEN kind = ? THEN 0 ELSE ? END, updated_at = ?
		WHERE id = ? AND status IN (?, ?, ?) AND expires_at > ?`,
		string(envelope.StatusClaimed), string(envelope.KindNotify), now.Add(lease).UnixMilli(), now.UnixMilli(), id,
		string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed), now.UnixMilli())
	if err != nil {
		return envelope.Request{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Request{}, ErrWrongState
	}
	req, _, err = s.lookup(ctx, id)
	return req, err
}

// Reply stores the target's answer and closes the request. The reply starts
// unseen by the asker.
func (s *Store) Reply(ctx context.Context, id, agent string, rep envelope.Reply) (envelope.Reply, error) {
	req, _, err := s.lookup(ctx, id)
	if err != nil {
		return envelope.Reply{}, err
	}
	if req.To != agent {
		return envelope.Reply{}, ErrForbidden
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Reply{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, lease_until = 0, updated_at = ?, reply_seen_at = 0
		WHERE id = ? AND status IN (?, ?, ?)`,
		string(rep.Status), now.UnixMilli(), id,
		string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed))
	if err != nil {
		return envelope.Reply{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Reply{}, ErrWrongState
	}
	rep.RequestID, rep.From, rep.CreatedAt = id, agent, now.UTC().Truncate(time.Millisecond)
	if _, err := tx.ExecContext(ctx, `INSERT INTO replies(request_id, from_agent, status, body, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, agent, string(rep.Status), rep.Body, now.UnixMilli()); err != nil {
		return envelope.Reply{}, err
	}
	return rep, tx.Commit()
}

// Result is a request with its current status and reply, if any.
type Result = envelope.Result

// Get returns a request for its sender or its target. When it hands the
// sender a reply, that reply counts as seen.
func (s *Store) Get(ctx context.Context, id, agent string) (Result, error) {
	req, status, err := s.lookup(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if req.From != agent && req.To != agent {
		return Result{}, ErrNotFound
	}
	rep, err := s.replyFor(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if rep != nil && req.From == agent {
		if err := s.MarkRepliesSeen(ctx, agent, []string{id}); err != nil {
			return Result{}, err
		}
	}
	return Result{Request: req, Status: status, Reply: rep}, nil
}

// replyFor returns the stored reply for a request, or nil if none yet.
func (s *Store) replyFor(ctx context.Context, id string) (*envelope.Reply, error) {
	var rep envelope.Reply
	var created int64
	var st string
	err := s.db.QueryRowContext(ctx, `SELECT from_agent, status, body, created_at FROM replies WHERE request_id = ?`, id).
		Scan(&rep.From, &st, &rep.Body, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rep.RequestID, rep.Status, rep.CreatedAt = id, envelope.Status(st), time.UnixMilli(created).UTC()
	return &rep, nil
}

// Cancel lets the sender withdraw a request the target has not claimed.
func (s *Store) Cancel(ctx context.Context, id, agent string) error {
	req, _, err := s.lookup(ctx, id)
	if err != nil {
		return err
	}
	if req.From != agent {
		return ErrForbidden
	}
	res, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ?, updated_at = ? WHERE id = ? AND status IN (?, ?)`,
		string(envelope.StatusCancelled), s.now().UnixMilli(), id, string(envelope.StatusQueued), string(envelope.StatusDelivered))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrWrongState
	}
	return nil
}

// CancelAllFor cancels every open request sent by or addressed to agent (used
// when an agent is removed, so nothing it sent is still delivered). It returns
// the cancelled ids.
func (s *Store) CancelAllFor(ctx context.Context, agent string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE requests SET status = ?, lease_until = 0, updated_at = ?
		WHERE (from_agent = ? OR to_agent = ?) AND status IN (?, ?, ?) RETURNING id`,
		string(envelope.StatusCancelled), s.now().UnixMilli(), agent, agent,
		string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Transition is one state change made by Sweep.
type Transition struct {
	ID      string
	TraceID string
	From    string
	To      string
	Status  envelope.Status
}

// Sweep expires requests past their TTL and returns requests whose delivery
// or claim lease ran out to the queue.
func (s *Store) Sweep(ctx context.Context) ([]Transition, error) {
	now := s.now().UnixMilli()
	var out []Transition
	collect := func(query string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Transition
			var st string
			if err := rows.Scan(&t.ID, &t.TraceID, &t.From, &t.To, &st); err != nil {
				return err
			}
			t.Status = envelope.Status(st)
			out = append(out, t)
		}
		return rows.Err()
	}
	// A claim past its TTL expires once its lease runs out rather than going
	// back to the queue, where it would only wake the agent for a request
	// Deliver rejects.
	if err := collect(`UPDATE requests SET status = ?, lease_until = 0, updated_at = ?
		WHERE (status IN (?, ?) OR (status = ? AND lease_until > 0 AND lease_until <= ?)) AND expires_at <= ?
		RETURNING id, trace_id, from_agent, to_agent, status`,
		string(envelope.StatusExpired), now, string(envelope.StatusQueued), string(envelope.StatusDelivered),
		string(envelope.StatusClaimed), now, now); err != nil {
		return nil, err
	}
	if err := collect(`UPDATE requests SET status = ?, lease_until = 0, updated_at = ?
		WHERE status IN (?, ?) AND lease_until > 0 AND lease_until <= ? RETURNING id, trace_id, from_agent, to_agent, status`,
		string(envelope.StatusQueued), now, string(envelope.StatusDelivered), string(envelope.StatusClaimed), now); err != nil {
		return nil, err
	}
	return out, nil
}

// OpenClaim returns the ask agent has claimed and not yet answered, when it
// has exactly one. The relay uses it to continue a chain when a sender forgets
// to name a parent. With several open claims it cannot tell which one a new
// request continues, so it reports none and the request starts a new chain.
// Claimed notifies never count: nothing closes them.
func (s *Store) OpenClaim(ctx context.Context, agent string) (envelope.Request, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestCols+` FROM requests WHERE to_agent = ? AND status = ? AND kind = ?
		ORDER BY updated_at DESC LIMIT 2`, agent, string(envelope.StatusClaimed), string(envelope.KindAsk))
	if err != nil {
		return envelope.Request{}, false, err
	}
	defer rows.Close()
	reqs, err := scanRequests(rows)
	if err != nil || len(reqs) != 1 {
		return envelope.Request{}, false, err
	}
	return reqs[0], true, nil
}

// Request returns a stored request regardless of caller (relay-internal).
func (s *Store) Request(ctx context.Context, id string) (envelope.Request, envelope.Status, error) {
	return s.lookup(ctx, id)
}

const requestCols = `id, from_agent, to_agent, parent_id, trace_id, hop, chain, kind, body, status, created_at`

type scanner interface{ Scan(dest ...any) error }

func scanRequest(sc scanner) (envelope.Request, envelope.Status, error) {
	var r envelope.Request
	var chain, kind, status string
	var created int64
	if err := sc.Scan(&r.ID, &r.From, &r.To, &r.ParentID, &r.TraceID, &r.Hop, &chain, &kind, &r.Body, &status, &created); err != nil {
		return envelope.Request{}, "", err
	}
	if err := json.Unmarshal([]byte(chain), &r.Chain); err != nil {
		return envelope.Request{}, "", err
	}
	r.Kind, r.CreatedAt = envelope.Kind(kind), time.UnixMilli(created).UTC()
	return r, envelope.Status(status), nil
}

func scanRequests(rows *sql.Rows) ([]envelope.Request, error) {
	var out []envelope.Request
	for rows.Next() {
		r, _, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) lookup(ctx context.Context, id string) (envelope.Request, envelope.Status, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id = ?`, id)
	r, st, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return envelope.Request{}, "", ErrNotFound
	}
	return r, st, err
}

func sortByCreated(rs []envelope.Request) {
	slices.SortStableFunc(rs, func(a, b envelope.Request) int { return a.CreatedAt.Compare(b.CreatedAt) })
}

func randomID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
