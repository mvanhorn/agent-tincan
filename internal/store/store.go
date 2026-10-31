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
	"log"
	"slices"
	"sync"
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
	// ErrGroupFull means a sender has filled this group.
	ErrGroupFull = errors.New("group already has 8 requests from this sender")
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
  last_seen_at INTEGER,
  version   TEXT,
  good_at   TEXT
);
CREATE TABLE IF NOT EXISTS invites (
  code    TEXT PRIMARY KEY, -- hex HMAC digest of the one-time code, never the raw code
  name    TEXT NOT NULL,
  expires INTEGER NOT NULL,
  kind    TEXT,
  new_only  INTEGER NOT NULL DEFAULT 0, -- 1 for a code minted by InviteNew
  mint_node TEXT -- the minting device's node id for a new_only code
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
  urgent INTEGER NOT NULL DEFAULT 0,
  reply_seen_at INTEGER NOT NULL DEFAULT 0,
  attachments TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS requests_to_status ON requests(to_agent, status);
-- QueueStats groups open requests across every agent; leading with status
-- keeps it to the open rows instead of the whole request history.
CREATE INDEX IF NOT EXISTS requests_status_to ON requests(status, to_agent);
CREATE TABLE IF NOT EXISTS replies (
  request_id TEXT PRIMARY KEY REFERENCES requests(id),
  from_agent TEXT NOT NULL,
  status     TEXT NOT NULL,
  body       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  attachments TEXT NOT NULL DEFAULT ''
);
-- AgentStats reads the replies written in a window; finding them by time
-- keeps it off the rest of the request history.
CREATE INDEX IF NOT EXISTS replies_created ON replies(created_at);
`

// Store is the relay's SQLite store.
type Store struct {
	db   *sql.DB
	now  func() time.Time
	path string // the database file, "" for an in-memory store

	pollMu sync.Mutex     // orders poll audit rows against wake calls
	waking map[string]int // agents with a wake call in flight, guarded by pollMu
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
	if path != ":memory:" {
		s.path = path
	}
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
	if err := s.migrateAgentVersion(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agent version: %w", err)
	}
	if err := s.migrateAgentGoodAt(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agent good-at: %w", err)
	}
	if err := s.migrateAgentFeatures(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agent features: %w", err)
	}
	if err := s.migrateAgentLastPoll(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agent last poll: %w", err)
	}
	if err := s.migrateInvites(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate invites: %w", err)
	}
	if err := s.migrateInviteCodeHash(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate invite code hash: %w", err)
	}
	if err := s.migrateReplySeen(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate reply seen: %w", err)
	}
	if err := s.migrateGroups(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.migrateAttachments(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate attachments: %w", err)
	}
	if err := s.migrateUrgent(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate urgent: %w", err)
	}
	for _, col := range []struct{ name, definition string }{{"progress_note", "TEXT NOT NULL DEFAULT ''"}, {"progress_at", "INTEGER NOT NULL DEFAULT 0"}, {"claimed_at", "INTEGER NOT NULL DEFAULT 0"}, {"from_session", "TEXT NOT NULL DEFAULT ''"}, {"claim_session", "TEXT NOT NULL DEFAULT ''"}} {
		var exists bool
		if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = ?)", col.name).Scan(&exists); err != nil {
			db.Close()
			return nil, err
		}
		if !exists {
			if _, err := s.db.Exec("ALTER TABLE requests ADD COLUMN " + col.name + " " + col.definition); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	if err := s.migrateLeaseLapsed(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate lease lapsed: %w", err)
	}
	if err := s.migrateSearch(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate search: %w", err)
	}
	if err := s.migrateClarification(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate clarification: %w", err)
	}
	if err := s.ensureAudit(); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit schema: %w", err)
	}
	if err := s.migrateApproval(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate approval: %w", err)
	}
	if err := s.migrateWebStatus(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate web status: %w", err)
	}
	if err := s.migrateWakes(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate wakes: %w", err)
	}
	if err := s.migrateRelayNotes(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate relay notes: %w", err)
	}
	if err := s.migrateOwnerNotices(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate owner notices: %w", err)
	}
	if err := s.migrateEmailReplies(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate email replies: %w", err)
	}
	if err := s.migrateWatches(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate watches: %w", err)
	}
	if err := s.migrateFailover(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate failover: %w", err)
	}
	if err := s.migratePush(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate push subscriptions: %w", err)
	}
	if err := s.migrateRooms(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate rooms: %w", err)
	}
	if err := s.migrateProject(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate project: %w", err)
	}
	if err := s.migrateLeases(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate leases: %w", err)
	}
	if err := s.migrateSchedules(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schedules: %w", err)
	}
	if err := s.migrateFollowups(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate followups: %w", err)
	}
	if err := s.migrateGuests(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate guests: %w", err)
	}
	for {
		more, err := s.backfillSearchBatch()
		if err != nil {
			log.Printf("search backfill incomplete; will resume on reopen: %v", err)
			break
		}
		if !more {
			break
		}
	}
	return s, nil
}

func (s *Store) migrateUrgent() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'urgent')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN urgent INTEGER NOT NULL DEFAULT 0`)
	return err
}

// migrateLeaseLapsed adds lease_lapsed_at, when Sweep last put the request
// back in the queue because its claim lease ran out (unix millis, NULL when
// it has not lapsed since its last claim or reply). The roster reads it to
// say a claim lapsed; a relay that predates it never sets or reads it.
func (s *Store) migrateLeaseLapsed() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'lease_lapsed_at')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN lease_lapsed_at INTEGER`)
	return err
}

// Path is the database file the store was opened on, or "" for an
// in-memory store. The relay keeps attachment files beside it.
func (s *Store) Path() string { return s.path }

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

// migrateAgentVersion adds the version column (the tincan build the agent
// last called with, NULL until it has reported one) to an agents table
// created before versions were recorded. It is a no-op on a current table.
func (s *Store) migrateAgentVersion() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('agents') WHERE name = 'version')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE agents ADD COLUMN version TEXT`)
	return err
}

// migrateAgentGoodAt adds the good_at column (the owner's line saying what
// the agent is good at, NULL until set) to an agents table created before
// lines were stored. It is a no-op on a current table.
func (s *Store) migrateAgentGoodAt() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('agents') WHERE name = 'good_at')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE agents ADD COLUMN good_at TEXT`)
	return err
}

// migrateProject adds to_project (the project of the session a request is
// for, "" for any session), queued_at (when its fallback clock started) and
// fallback_woken (whether FallenBack reported it since that clock started).
// Old rows name no project, so a zero queued_at never counts. It is a no-op
// on a current table.
func (s *Store) migrateProject() error {
	for _, col := range []struct{ name, definition string }{
		{"to_project", "TEXT NOT NULL DEFAULT ''"},
		{"queued_at", "INTEGER NOT NULL DEFAULT 0"},
		{"fallback_woken", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var has bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = ?)`, col.name).Scan(&has); err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN ` + col.name + ` ` + col.definition); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateAgentFeatures() error {
	var pollColumn bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('agents') WHERE name = 'poll_features')`).Scan(&pollColumn); err != nil {
		return err
	}
	if !pollColumn {
		if _, err := s.db.Exec(`ALTER TABLE agents ADD COLUMN poll_features TEXT`); err != nil {
			return err
		}
	}

	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('agents') WHERE name = 'features')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE agents ADD COLUMN features TEXT`)
	return err
}

// migrateInvites adds the columns an invites table created before invites
// carried them lacks: kind, then new_only and mint_node (the redeem-time
// check of a new-only code). It is a no-op on a current table.
func (s *Store) migrateInvites() error {
	for _, col := range []struct{ name, definition string }{
		{"kind", "TEXT"},
		{"new_only", "INTEGER NOT NULL DEFAULT 0"},
		{"mint_node", "TEXT"},
	} {
		var has bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('invites') WHERE name = ?)`, col.name).Scan(&has); err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE invites ADD COLUMN " + col.name + " " + col.definition); err != nil {
			return err
		}
	}
	return nil
}

// migrateInviteCodeHash invalidates invites minted before codes were stored
// as digests. Those rows hold raw one-time codes, so they cannot be carried
// forward; every invite is at most ten minutes old (see identity.InviteTTL),
// so an admin simply mints a fresh one. Only legacy-shaped rows are deleted:
// a valid digest is always 64 lowercase hex chars, so rows already in that
// shape (mixed tables, restored backups) are preserved. The single DELETE
// keeps detection and removal atomic — no concurrent insert can slip between
// them. The invites.code column keeps its name but now holds the hex HMAC
// digest — see identity.hashInviteCode.
func (s *Store) migrateInviteCodeHash() error {
	_, err := s.db.Exec(`DELETE FROM invites WHERE length(code) != 64 OR code GLOB '*[^0-9a-f]*'`)
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
	// name's last activity and poll, the build it last reported and the
	// owner's good-at line carry over; only SetAgentGoodAt writes the line.
	var lastSeen, lastPoll sql.NullInt64
	var version, features, pollFeatures, goodAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT last_seen_at, last_poll_at, version, features, poll_features, good_at FROM agents WHERE name = ?`, a.Name).Scan(&lastSeen, &lastPoll, &version, &features, &pollFeatures, &goodAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, a.Name); err != nil {
		return err
	}
	until, asks, err := encodeGuest(a.Guest)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(name, node_id, node_name, joined_at, kind, node_user, last_seen_at, last_poll_at, version, features, poll_features, good_at, guest_until, guest_asks, guest_no_hold, guest_ended) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Name, a.NodeID, a.NodeName, a.JoinedAt.UnixMilli(), nullable(a.Kind), nullable(a.NodeUser), lastSeen, lastPoll, version, features, pollFeatures, goodAt, until, asks, a.Guest.NoHold, a.Guest.Ended); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAgent removes name and, in the same transaction, the last wake the
// relay sent it, every idle watch it set or that watches it, every scheduled
// ask from or to it, and its room memberships, so an agent joined again under
// the name starts with none and in no rooms. Its room posts stay, shown under
// its name.
func (s *Store) DeleteAgent(ctx context.Context, name string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM wakes WHERE agent = ?`, name); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE watcher = ? OR target = ?`, name, name); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE from_agent = ? OR to_agent = ?`, name, name); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE agent = ?`, name); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM work_leases WHERE holder = ?`, name); err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
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

// SetAgentGoodAt records the owner's good-at line for an agent; "" stores
// NULL.
func (s *Store) SetAgentGoodAt(ctx context.Context, name, line string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET good_at = ? WHERE name = ?`, nullable(line), name)
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

// SetAgentVersion records the tincan build name last called with; ""
// stores NULL. It ignores names not in the directory.
func (s *Store) SetAgentVersion(ctx context.Context, name, version string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET version = ? WHERE name = ?`, nullable(version), name)
	return err
}

// AgentVersions returns the tincan build each agent last called with, for
// every agent that has reported one.
func (s *Store) AgentVersions(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, version FROM agents WHERE version IS NOT NULL AND version != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, v string
		if err := rows.Scan(&name, &v); err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, rows.Err()
}

// SetAgentFeatures records the client capabilities last called with; ""
// stores NULL. It ignores names not in the directory.
func (s *Store) SetAgentFeatures(ctx context.Context, name, features string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET features = ? WHERE name = ?`, nullable(features), name)
	return err
}

// AgentFeatures returns the client capabilities each agent last called with, for
// every agent that has reported one.
func (s *Store) AgentFeatures(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, features FROM agents WHERE features IS NOT NULL AND features != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, v string
		if err := rows.Scan(&name, &v); err != nil {
			return nil, err
		}
		out[name] = v
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
	rows, err := s.db.QueryContext(ctx, `SELECT name, node_id, node_name, joined_at, COALESCE(kind, ''), COALESCE(node_user, ''), COALESCE(good_at, ''), guest_until, guest_asks, guest_no_hold, guest_ended FROM agents WHERE `+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identity.Agent
	for rows.Next() {
		var a identity.Agent
		var joined, until int64
		var asks string
		if err := rows.Scan(&a.Name, &a.NodeID, &a.NodeName, &joined, &a.Kind, &a.NodeUser, &a.GoodAt, &until, &asks, &a.Guest.NoHold, &a.Guest.Ended); err != nil {
			return nil, err
		}
		a.JoinedAt = time.UnixMilli(joined)
		if a.Guest.Until, a.Guest.Asks, err = decodeGuest(until, asks); err != nil {
			return nil, err
		}
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

// PutInvite stores inv and retires any earlier unredeemed code for the same
// name, so only the newest invite for a name works. Callers pass the digest
// from identity.hashInviteCode in inv.Code; the raw code is never persisted.
func (s *Store) PutInvite(ctx context.Context, inv identity.Invite) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE name = ? AND code != ?`, inv.Name, inv.Code); err != nil {
		return err
	}
	until, asks, err := encodeGuest(inv.Guest)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO invites(code, name, expires, kind, new_only, mint_node, guest_until, guest_asks, guest_no_hold) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.Code, inv.Name, inv.Expires.UnixMilli(), nullable(inv.Kind), inv.NewOnly, nullable(inv.MintNode), until, asks, inv.Guest.NoHold); err != nil {
		return err
	}
	return tx.Commit()
}

// TakeInvite removes and returns the invite for a code digest, so a code
// works once. Callers pass the digest from identity.hashInviteCode; the raw
// code never reaches the database.
func (s *Store) TakeInvite(ctx context.Context, code string) (identity.Invite, bool, error) {
	var inv identity.Invite
	var exp, until int64
	var asks string
	err := s.db.QueryRowContext(ctx, `DELETE FROM invites WHERE code = ? RETURNING code, name, expires, COALESCE(kind, ''), new_only, COALESCE(mint_node, ''), guest_until, guest_asks, guest_no_hold`, code).Scan(&inv.Code, &inv.Name, &exp, &inv.Kind, &inv.NewOnly, &inv.MintNode, &until, &asks, &inv.Guest.NoHold)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Invite{}, false, nil
	}
	if err != nil {
		return identity.Invite{}, false, err
	}
	inv.Expires = time.UnixMilli(exp)
	if inv.Guest.Until, inv.Guest.Asks, err = decodeGuest(until, asks); err != nil {
		return identity.Invite{}, false, err
	}
	return inv, true, nil
}

// --- request queue ---

// Enqueue stores a new request. The caller has already set From, To, Kind,
// Body, TraceID, Hop, Chain, and ParentID. Enqueue assigns ID and CreatedAt.
// Attachments name uploads by id: each must be the sender's own finished
// upload on no other message, and comes back filled in from its metadata.
// A bad reference stores nothing.
func (s *Store) Enqueue(ctx context.Context, req envelope.Request, ttl time.Duration) (envelope.Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, err
	}
	defer tx.Rollback()
	if req, err = s.enqueueTx(ctx, tx, req, ttl); err != nil {
		return envelope.Request{}, err
	}
	return req, tx.Commit()
}

// enqueueTx is Enqueue inside tx, which the caller commits.
func (s *Store) enqueueTx(ctx context.Context, tx *sql.Tx, req envelope.Request, ttl time.Duration) (envelope.Request, error) {
	now := s.now()
	status := envelope.StatusQueued
	if req.Status == envelope.StatusHeld {
		status = envelope.StatusHeld
		ttl = req.HoldTTL
	}
	req.WasHeld, req.Approved = status == envelope.StatusHeld, false
	req.ID = randomID()
	req.CreatedAt = now.UTC().Truncate(time.Millisecond)
	if req.TraceID == "" {
		req.TraceID = req.ID
	}
	chain, err := json.Marshal(req.Chain)
	if err != nil {
		return envelope.Request{}, err
	}
	if req.Group != "" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM requests WHERE from_agent = ? AND group_id = ? LIMIT ?)`, req.From, req.Group, MaxGroupRequests).Scan(&count); err != nil {
			return envelope.Request{}, err
		}
		if count >= MaxGroupRequests {
			return envelope.Request{}, ErrGroupFull
		}
	}
	if req.RoomPostID != 0 {
		// A mention's poster and target must both be members of the post's
		// room as the ask is written, so a removal that lands after the
		// relay's own check still leaves nothing for a former member.
		var members int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT m.agent) FROM room_posts p JOIN room_members m ON m.room_id = p.room_id
			WHERE p.id = ? AND m.agent IN (?, ?)`, req.RoomPostID, req.From, req.To).Scan(&members); err != nil {
			return envelope.Request{}, err
		}
		if members != 2 {
			return envelope.Request{}, ErrNotRoomMember
		}
	}
	var atts string
	if req.Attachments, atts, err = bindAndEncodeAttachments(ctx, tx, req.Attachments, req.From, req.ID); err != nil {
		return envelope.Request{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO requests
		(id, from_agent, to_agent, parent_id, trace_id, hop, chain, kind, body, status, created_at, updated_at, expires_at, attachments, was_held, approved, group_id, urgent, from_session, room_post_id, to_project, queued_at, follow_of, from_guest, guest_approved)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.From, req.To, req.ParentID, req.TraceID, req.Hop, string(chain), string(req.Kind), req.Body,
		string(status), now.UnixMilli(), now.UnixMilli(), now.Add(ttl).UnixMilli(), atts, req.WasHeld, req.Approved, req.Group, req.Urgent, req.FromSession, sql.NullInt64{Int64: req.RoomPostID, Valid: req.RoomPostID != 0}, req.Project, now.UnixMilli(), req.FollowOf, req.FromGuest, req.GuestApproved)
	if err != nil {
		return envelope.Request{}, err
	}
	if err := s.followOnTx(ctx, tx, req); err != nil {
		return envelope.Request{}, err
	}
	return req, nil
}

// maxDeliverBytes bounds the request text one poll delivers, so a batch of
// large or resumed requests (which carry their clarification exchanges) stays
// well inside a client's response limit. The oldest request always goes out,
// whatever its size; the rest wait for the next poll.
const maxDeliverBytes = 1 << 20

// QueueScope is which of an agent's queued requests one caller may take. The
// zero scope takes them all, as before projects existed.
type QueueScope struct {
	// Project is the caller's project label, "" for none.
	Project string
	// Grace is how long a request for another project stays closed to the
	// caller after it was queued. Age decides, not whether a session of
	// that project looks live, because an open idle window keeps calling
	// the relay and would otherwise strand the request.
	Grace time.Duration
}

// scopeOpen is the test for a request a scope may take, with the arguments
// QueueScope.filter gives it.
const scopeOpen = `(to_project = '' OR to_project = ? OR queued_at <= ?)`

// filter narrows a query on requests to what the scope may take at now. It
// is the one test every delivery path shares.
func (scope QueueScope) filter(now time.Time) (string, []any) {
	if scope.Grace <= 0 {
		return "", nil
	}
	return ` AND ` + scopeOpen, []any{scope.Project, now.Add(-scope.Grace).UnixMilli()}
}

// Deliver hands up to limit queued requests for agent to its poller, marking
// them delivered under a lease. A request delivered but never claimed returns
// to the queue when the lease runs out, so a crash after polling strands
// nothing. A request held back (HoldBack) waits until its time.
func (s *Store) Deliver(ctx context.Context, agent string, limit int, lease time.Duration) ([]envelope.Request, error) {
	return s.DeliverFor(ctx, agent, QueueScope{}, limit, lease)
}

// DeliverFor is Deliver of what scope may take.
func (s *Store) DeliverFor(ctx context.Context, agent string, scope QueueScope, limit int, lease time.Duration) ([]envelope.Request, error) {
	now := s.now()
	filter, filterArgs := scope.filter(now)
	args := []any{string(envelope.StatusDelivered), now.Add(lease).UnixMilli(), now.UnixMilli(),
		agent, string(envelope.StatusQueued), now.UnixMilli(), now.UnixMilli()}
	args = append(append(args, filterArgs...), limit, maxDeliverBytes)
	rows, err := s.db.QueryContext(ctx, `UPDATE requests SET status = ?, lease_until = ?, updated_at = ?
		WHERE id IN (SELECT id FROM (
			SELECT id, SUM(length(body) + length(exchanges)) OVER (ORDER BY urgent DESC, created_at, rowid) AS running,
				ROW_NUMBER() OVER (ORDER BY urgent DESC, created_at, rowid) AS n
			FROM requests WHERE to_agent = ? AND status = ? AND expires_at > ? AND deliver_after <= ?`+filter+`
			ORDER BY urgent DESC, created_at, rowid LIMIT ?
		) WHERE n = 1 OR running <= ?)
		RETURNING `+requestCols, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanRequests(rows)
	if err != nil {
		return nil, err
	}
	sortByPriority(out)
	return out, nil
}

// FallenBack returns the queued requests that named a project and have now
// been queued for grace, so any session may take them, each once per fallback
// clock. A caller that found nothing during the grace has no other reason to
// look again, so the relay wakes the target for these. A request held back
// (HoldBack) waits until its time, as Deliver skips it.
func (s *Store) FallenBack(ctx context.Context, grace time.Duration) ([]envelope.Request, error) {
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `UPDATE requests SET fallback_woken = 1
		WHERE status = ? AND to_project != '' AND fallback_woken = 0 AND queued_at <= ? AND expires_at > ? AND deliver_after <= ?
		RETURNING `+requestCols,
		string(envelope.StatusQueued), now.Add(-grace).UnixMilli(), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequests(rows)
}

// CountQueued returns how many requests are waiting for agent without
// delivering them. A request held back (HoldBack) is not waiting until its
// time, as Deliver skips it.
func (s *Store) CountQueued(ctx context.Context, agent string) (int, error) {
	var n int
	now := s.now().UnixMilli()
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE to_agent = ? AND status = ? AND expires_at > ? AND deliver_after <= ?`,
		agent, string(envelope.StatusQueued), now, now).Scan(&n)
	return n, err
}

// QueuedIDs lists the ids of agent's live requests still waiting to be
// delivered, oldest first: the requests CountQueued counts.
func (s *Store) QueuedIDs(ctx context.Context, agent string) ([]string, error) {
	now := s.now().UnixMilli()
	return s.ids(ctx, `SELECT id FROM requests WHERE to_agent = ? AND status = ? AND expires_at > ? AND deliver_after <= ? ORDER BY created_at, rowid`,
		agent, string(envelope.StatusQueued), now, now)
}

// HiddenIDsFor lists up to limit ids of agent's queued requests that scope
// may not take yet, oldest first.
func (s *Store) HiddenIDsFor(ctx context.Context, agent string, scope QueueScope, limit int) ([]string, error) {
	at := s.now()
	filter, filterArgs := scope.filter(at)
	if filter == "" {
		return nil, nil
	}
	now := at.UnixMilli()
	args := append(append([]any{agent, string(envelope.StatusQueued), now, now}, filterArgs...), limit)
	return s.ids(ctx, `SELECT id FROM requests WHERE to_agent = ? AND status = ? AND expires_at > ? AND deliver_after <= ? AND NOT `+scopeOpen+` ORDER BY created_at, rowid LIMIT ?`, args...)
}

// ids runs a query whose rows are single ids.
func (s *Store) ids(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AgentsWithQueuedRequests returns every agent that has a live request
// still waiting to be delivered.
func (s *Store) AgentsWithQueuedRequests(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT to_agent FROM requests WHERE status = ? AND expires_at > ? ORDER BY to_agent`,
		string(envelope.StatusQueued), s.now().UnixMilli())
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

// CountUrgentQueued returns how many of agent's queued requests are urgent.
func (s *Store) CountUrgentQueued(ctx context.Context, agent string) (int, error) {
	return s.CountUrgentQueuedFor(ctx, agent, QueueScope{})
}

// CountUrgentQueuedFor is CountUrgentQueued of what scope may take.
func (s *Store) CountUrgentQueuedFor(ctx context.Context, agent string, scope QueueScope) (int, error) {
	var n int
	at := s.now()
	now := at.UnixMilli()
	filter, filterArgs := scope.filter(at)
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE to_agent = ? AND status = ? AND urgent = 1 AND expires_at > ? AND deliver_after <= ?`+filter,
		append([]any{agent, string(envelope.StatusQueued), now, now}, filterArgs...)...).Scan(&n)
	return n, err
}

// OpenAsks returns agent's open asks with their bodies and clarification
// exchanges, urgent first and then oldest, each with its current status in
// Status. Open means queued, delivered or claimed and not expired. It is the
// only query a request email reads, so it never returns a held request (or
// one that was held and never approved), a ping, a notify, an ask built
// from a room mention (its text is the room's, so its wake carries counts
// only), a request marked from_guest (an email cannot carry its framing, and
// a mail reader holds no claim for the mark to follow, so it is taken through
// the inbox), or a request paused for input or already finished: none of
// those may reach an email.
func (s *Store) OpenAsks(ctx context.Context, agent string) ([]envelope.Request, error) {
	return s.OpenAsksFor(ctx, agent, QueueScope{})
}

// OpenAsksFor is OpenAsks of what scope may take.
func (s *Store) OpenAsksFor(ctx context.Context, agent string, scope QueueScope) ([]envelope.Request, error) {
	now := s.now()
	filter, filterArgs := scope.filter(now)
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestCols+` FROM requests
		WHERE to_agent = ? AND kind = ? AND status IN (?, ?, ?) AND expires_at > ? AND NOT (was_held = 1 AND approved = 0) AND room_post_id IS NULL AND from_guest = 0`+filter+`
		ORDER BY urgent DESC, created_at, rowid`,
		append([]any{agent, string(envelope.KindAsk), string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed), now.UnixMilli()}, filterArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envelope.Request
	for rows.Next() {
		r, status, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		r.Status = status
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueueStat is one agent's backlog: requests waiting to be claimed, the
// creation time of the oldest of them, and claims whose lease is still live.
// OldestClaim is when the oldest live claim began and ClaimAsker who sent
// that request, so the roster can say whose work a busy agent is on without
// reading any request text. Lapsed counts waiting requests whose last claim
// ran out of lease with no claim, progress note or reply since.
type QueueStat struct {
	Queued       int
	OldestQueued time.Time
	Claimed      int
	OldestClaim  time.Time
	ClaimAsker   string
	Lapsed       int
}

// QueueStats returns every agent's backlog in one grouped query. Expired and
// finished requests, claims whose lease ran out, and pings (answered
// automatically, never work) are not counted.
func (s *Store) QueueStats(ctx context.Context) (map[string]QueueStat, error) {
	now := s.now().UnixMilli()
	// Each group has exactly one MIN, so SQLite takes the bare from_agent
	// from the row holding it: for claims, the asker of the oldest claim.
	rows, err := s.db.QueryContext(ctx, `SELECT to_agent, status, COUNT(*),
		MIN(CASE WHEN status = 'claimed' THEN claimed_at ELSE created_at END), from_agent, SUM(lease_lapsed_at IS NOT NULL)
		FROM requests WHERE status IN ('queued', 'delivered', 'claimed') AND expires_at > ?
		AND (status != 'claimed' OR lease_until > ?) AND kind != 'ping' GROUP BY to_agent, status`, now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]QueueStat)
	for rows.Next() {
		var agent, status, asker string
		var count, lapsed int
		var oldest int64
		if err := rows.Scan(&agent, &status, &count, &oldest, &asker, &lapsed); err != nil {
			return nil, err
		}
		stat := out[agent]
		if status == string(envelope.StatusClaimed) {
			stat.Claimed += count
			if oldest > 0 {
				stat.OldestClaim, stat.ClaimAsker = time.UnixMilli(oldest), asker
			}
		} else {
			stat.Lapsed += lapsed
			stat.Queued += count
			at := time.UnixMilli(oldest)
			if stat.OldestQueued.IsZero() || at.Before(stat.OldestQueued) {
				stat.OldestQueued = at
			}
		}
		out[agent] = stat
	}
	return out, rows.Err()
}

// PendingRequests names up to limit of agent's queued requests, urgent
// first and then oldest, without delivering them or reading their bodies.
// A request held back (HoldBack) is left out until its time.
// Queued pings come first and have their own limit, so a backlog of other
// requests never hides a ping from a peek-based responder.
func (s *Store) PendingRequests(ctx context.Context, agent string, limit int) ([]envelope.Pending, error) {
	return s.PendingRequestsFor(ctx, agent, QueueScope{}, limit)
}

// PendingRequestsFor is PendingRequests of what scope may take.
func (s *Store) PendingRequestsFor(ctx context.Context, agent string, scope QueueScope, limit int) ([]envelope.Pending, error) {
	out, err := s.pendingRequests(ctx, agent, scope, "kind = ?", limit)
	if err != nil {
		return nil, err
	}
	rest, err := s.pendingRequests(ctx, agent, scope, "kind != ?", limit)
	return append(out, rest...), err
}

func (s *Store) pendingRequests(ctx context.Context, agent string, scope QueueScope, kindCond string, limit int) ([]envelope.Pending, error) {
	at := s.now()
	now := at.UnixMilli()
	filter, filterArgs := scope.filter(at)
	args := append(append([]any{agent, string(envelope.StatusQueued), now, now, string(envelope.KindPing)}, filterArgs...), limit)
	rows, err := s.db.QueryContext(ctx, `SELECT id, from_agent, kind, urgent, updated_at FROM requests
		WHERE to_agent = ? AND status = ? AND expires_at > ? AND deliver_after <= ? AND `+kindCond+filter+` ORDER BY urgent DESC, created_at, rowid LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envelope.Pending
	for rows.Next() {
		var p envelope.Pending
		if err := rows.Scan(&p.ID, &p.From, &p.Kind, &p.Urgent, &p.Rev); err != nil {
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
	return s.ClaimLeases(ctx, id, agent, lease, lease)
}

// ClaimLeases is Claim with urgentLease in place of lease for an urgent
// request, so a claimed urgent request that goes quiet is requeued sooner.
// claimed_at records when this claim began; claiming again while claimed
// keeps it.
func (s *Store) ClaimLeases(ctx context.Context, id, agent string, lease, urgentLease time.Duration) (envelope.Request, error) {
	return s.ClaimLeasesFrom(ctx, id, agent, "", QueueScope{}, lease, urgentLease)
}

// ClaimLeasesFrom is ClaimLeases from the claimer's session, which is
// recorded with the claim like claimed_at: claiming again while claimed
// keeps the first session. "" is a claim made with no session. A queued
// request held back (HoldBack) cannot be claimed before its time, and a
// request outside scope cannot be claimed at all, so a claim by id reaches
// no further than a poll.
func (s *Store) ClaimLeasesFrom(ctx context.Context, id, agent, session string, scope QueueScope, lease, urgentLease time.Duration) (envelope.Request, error) {
	req, _, err := s.lookup(ctx, id)
	if err != nil {
		return envelope.Request{}, err
	}
	if req.To != agent {
		return envelope.Request{}, notTarget(req, agent)
	}
	now := s.now()
	filter, filterArgs := scope.filter(now)
	args := []any{string(envelope.StatusClaimed), string(envelope.KindNotify), now.Add(urgentLease).UnixMilli(), now.Add(lease).UnixMilli(),
		string(envelope.StatusClaimed), now.UnixMilli(), string(envelope.StatusClaimed), session, now.UnixMilli(), id, agent,
		string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed), now.UnixMilli(),
		string(envelope.StatusQueued), now.UnixMilli()}
	res, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ?, lease_until = CASE WHEN kind = ? THEN 0 WHEN urgent = 1 THEN ? ELSE ? END,
		claimed_at = CASE WHEN status = ? THEN claimed_at ELSE ? END,
		claim_session = CASE WHEN status = ? THEN claim_session ELSE ? END, updated_at = ?, lease_lapsed_at = NULL
		WHERE id = ? AND to_agent = ? AND status IN (?, ?, ?) AND expires_at > ? AND (status != ? OR deliver_after <= ?)`+filter, append(args, filterArgs...)...)
	if err != nil {
		return envelope.Request{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Request{}, s.wrongState(ctx, id, agent)
	}
	req, _, err = s.lookup(ctx, id)
	return req, err
}

// SetProgress updates only a current claim and renews its lease.
func (s *Store) SetProgress(ctx context.Context, id, claimer, note string, lease time.Duration) error {
	return s.SetProgressLeases(ctx, id, claimer, note, lease, lease)
}

// SetProgressLeases is SetProgress renewing an urgent request's claim by
// urgentLease instead of lease, the same lease ClaimLeases gave it.
func (s *Store) SetProgressLeases(ctx context.Context, id, claimer, note string, lease, urgentLease time.Duration) error {
	if len(note) > envelope.MaxProgressNote {
		return envelope.ErrBodyTooLarge
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, `UPDATE requests SET progress_note = ?, progress_at = ?, updated_at = ?, lease_lapsed_at = NULL,
		lease_until = CASE WHEN kind = ? THEN 0 WHEN urgent = 1 THEN ? ELSE ? END
		WHERE id = ? AND to_agent = ? AND status = ? AND (lease_until = 0 OR lease_until > ?)`,
		note, now.UnixMilli(), now.UnixMilli(), string(envelope.KindNotify), now.Add(urgentLease).UnixMilli(), now.Add(lease).UnixMilli(),
		id, claimer, string(envelope.StatusClaimed), now.UnixMilli())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return s.wrongState(ctx, id, claimer)
	}
	return nil
}

// Reply stores the target's reply, closing the request or pausing its lease
// for needs_input. The reply starts unseen by the asker.
func (s *Store) Reply(ctx context.Context, id, agent string, rep envelope.Reply) (envelope.Reply, error) {
	req, _, err := s.lookup(ctx, id)
	if err != nil {
		return envelope.Reply{}, err
	}
	if req.To != agent {
		return envelope.Reply{}, notTarget(req, agent)
	}
	if rep.Status == envelope.StatusNeedsInput {
		return s.needsInput(ctx, id, agent, rep)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Reply{}, err
	}
	defer tx.Rollback()
	if rep, err = s.replyTx(ctx, tx, id, agent, rep, ""); err != nil {
		if errors.Is(err, ErrWrongState) {
			_ = tx.Rollback()
			err = s.wrongState(ctx, id, agent)
		}
		return envelope.Reply{}, err
	}
	return rep, tx.Commit()
}

// replyTx closes open request id with agent's terminal reply rep inside tx:
// the request takes rep's status and the reply row is written. guard, when
// set, is a further SQL condition with guardArgs for its placeholders, such
// as the email path's emailReplyGuard. It returns ErrWrongState when the
// request is no longer open, no longer agent's (it moved), or fails guard.
func (s *Store) replyTx(ctx context.Context, tx *sql.Tx, id, agent string, rep envelope.Reply, guard string, guardArgs ...any) (envelope.Reply, error) {
	now := s.now()
	args := []any{string(rep.Status), now.UnixMilli(), now.UnixMilli(), id, agent,
		string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed)}
	where := ""
	if guard != "" {
		where = " AND (" + guard + ")"
		args = append(args, guardArgs...)
	}
	res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, lease_until = 0, updated_at = ?, reply_seen_at = CASE WHEN kind = 'ping' THEN ? ELSE 0 END, reply_generation = reply_generation + 1, lease_lapsed_at = NULL
		WHERE id = ? AND to_agent = ? AND status IN (?, ?, ?)`+where, args...)
	if err != nil {
		return envelope.Reply{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Reply{}, ErrWrongState
	}
	if err := tx.QueryRowContext(ctx, `SELECT reply_generation FROM requests WHERE id = ?`, id).Scan(&rep.Generation); err != nil {
		return envelope.Reply{}, err
	}
	rep.RequestID, rep.From, rep.CreatedAt = id, agent, now.UTC().Truncate(time.Millisecond)
	var atts string
	if rep.Attachments, atts, err = bindAndEncodeAttachments(ctx, tx, rep.Attachments, agent, id); err != nil {
		return envelope.Reply{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO replies(request_id, from_agent, status, body, created_at, attachments) VALUES (?, ?, ?, ?, ?, ?)`,
		id, agent, string(rep.Status), rep.Body, now.UnixMilli(), atts); err != nil {
		return envelope.Reply{}, err
	}
	if err := s.mirrorReplyTx(ctx, tx, rep); err != nil {
		return envelope.Reply{}, err
	}
	if err := s.markFollowOnDueTx(ctx, tx, rep); err != nil {
		return envelope.Reply{}, err
	}
	return rep, nil
}

// Result is a request with its current status and reply, if any.
type Result = envelope.Result

// Get returns a request for its sender or its target. When it hands the
// sender a reply, that reply counts as seen. The sender also gets the
// relay's latest note on the request, if there is one.
func (s *Store) Get(ctx context.Context, id, agent string) (Result, error) {
	req, status, err := s.lookup(ctx, id)
	if err != nil {
		return Result{}, err
	}
	// A target the request moved off no longer finds it.
	if req.From != agent && req.To != agent {
		return Result{}, ErrNotFound
	}
	rep, err := s.replyFor(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if rep != nil && req.From == agent {
		if err := s.MarkRepliesSeen(ctx, agent, nil, envelope.ReplyAck{ID: id, Generation: rep.Generation}); err != nil {
			return Result{}, err
		}
	}
	var note *envelope.Progress
	if req.From == agent {
		if note, err = s.RelayNote(ctx, id); err != nil {
			return Result{}, err
		}
	}
	req.RedactFor(agent)
	res := Result{Request: req, Status: status, Reply: rep, Progress: req.Progress, Exchanges: req.Exchanges, RelayNote: note}
	res.RedactPassFor(agent)
	return res, nil
}

// replyFor returns the stored reply for a request, or nil if none yet.
func (s *Store) replyFor(ctx context.Context, id string) (*envelope.Reply, error) {
	var rep envelope.Reply
	var created int64
	var st, atts string
	err := s.db.QueryRowContext(ctx, `SELECT from_agent, status, body, created_at, attachments, (SELECT reply_generation FROM requests WHERE id = request_id) FROM replies WHERE request_id = ?
		AND (status != 'needs_input' OR EXISTS (SELECT 1 FROM requests WHERE id = request_id AND status = 'needs_input'))`, id).
		Scan(&rep.From, &st, &rep.Body, &created, &atts, &rep.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rep.Attachments, err = decodeAttachments(atts); err != nil {
		return nil, err
	}
	rep.RequestID, rep.Status, rep.CreatedAt = id, envelope.Status(st), time.UnixMilli(created).UTC()
	return &rep, nil
}

// Cancel lets the sender withdraw a request the target has not claimed.
func (s *Store) Cancel(ctx context.Context, id, agent string) error {
	return s.cancel(ctx, id, agent, "")
}

// CancelUntaken is Cancel for a gateway guest. A request back in the queue
// after a teammate claimed it, or after its question was answered, is not that
// guest's to withdraw: a claim is what marks what the teammate sends, and
// cancelling here would stop that claim from ever coming back while the
// teammate may still be acting on what it read.
func (s *Store) CancelUntaken(ctx context.Context, id, agent string) error {
	return s.cancel(ctx, id, agent, ` AND resumed = 0 AND claim_lapsed = 0`)
}

func (s *Store) cancel(ctx context.Context, id, agent, cond string) error {
	req, _, err := s.lookup(ctx, id)
	if err != nil {
		return err
	}
	if req.From != agent {
		return ErrForbidden
	}
	res, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ?, updated_at = ? WHERE id = ? AND status IN (?, ?, ?)`+cond,
		string(envelope.StatusCancelled), s.now().UnixMilli(), id, string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusHeld))
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
		WHERE (from_agent = ? OR to_agent = ?) AND status IN (?, ?, ?, ?, ?) RETURNING id`,
		string(envelope.StatusCancelled), s.now().UnixMilli(), agent, agent,
		string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed), string(envelope.StatusNeedsInput), string(envelope.StatusHeld))
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

// Transition is one state change made by Sweep. Prev is the status the
// request left, and ClaimedAt when its last claim began (zero if it was
// never claimed).
type Transition struct {
	Held      bool
	Urgent    bool
	ID        string
	TraceID   string
	From      string
	To        string
	Kind      envelope.Kind
	Status    envelope.Status
	Prev      envelope.Status
	ClaimedAt time.Time
}

// Sweep expires requests past their TTL and returns requests whose delivery
// or claim lease ran out to the queue.
func (s *Store) Sweep(ctx context.Context) ([]Transition, error) {
	now := s.now().UnixMilli()
	var out []Transition
	collect := func(prev envelope.Status, query string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, query+` RETURNING id, trace_id, from_agent, to_agent, kind, status, urgent, claimed_at`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t := Transition{Prev: prev}
			var st, kind string
			var claimed int64
			if err := rows.Scan(&t.ID, &t.TraceID, &t.From, &t.To, &kind, &st, &t.Urgent, &claimed); err != nil {
				return err
			}
			t.Status, t.Kind = envelope.Status(st), envelope.Kind(kind)
			if claimed > 0 {
				t.ClaimedAt = time.UnixMilli(claimed)
			}
			out = append(out, t)
		}
		return rows.Err()
	}
	if err := collect(envelope.StatusHeld, `UPDATE requests SET status = ?, updated_at = ? WHERE status = ? AND expires_at <= ?`,
		string(envelope.StatusExpired), now, string(envelope.StatusHeld), now); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Held = true
	}
	// A claim past its TTL expires once its lease runs out rather than going
	// back to the queue, where it would only wake the agent for a request
	// Deliver rejects. Each starting status is its own pass so the relay
	// knows what the request left.
	for _, prev := range []envelope.Status{envelope.StatusQueued, envelope.StatusDelivered, envelope.StatusNeedsInput} {
		if err := collect(prev, `UPDATE requests SET status = ?, lease_until = 0, updated_at = ? WHERE status = ? AND expires_at <= ?`,
			string(envelope.StatusExpired), now, string(prev), now); err != nil {
			return nil, err
		}
	}
	if err := collect(envelope.StatusClaimed, `UPDATE requests SET status = ?, lease_until = 0, updated_at = ?
		WHERE status = ? AND lease_until > 0 AND lease_until <= ? AND expires_at <= ?`,
		string(envelope.StatusExpired), now, string(envelope.StatusClaimed), now, now); err != nil {
		return nil, err
	}
	// A claim that ran out of lease is marked lapsed, so the roster can say
	// so until the agent claims or answers the request again, and for good
	// in claim_lapsed, which keeps failover from moving it; a delivery
	// lease running out is not the agent's lapse.
	for _, prev := range []envelope.Status{envelope.StatusDelivered, envelope.StatusClaimed} {
		if err := collect(prev, `UPDATE requests SET status = ?, lease_until = 0, updated_at = ?, progress_note = '', progress_at = 0,
		lease_lapsed_at = CASE WHEN status = ? THEN ? ELSE lease_lapsed_at END,
		claim_lapsed = CASE WHEN status = ? THEN 1 ELSE claim_lapsed END
		WHERE status = ? AND lease_paused = 0 AND lease_until > 0 AND lease_until <= ?`,
			string(envelope.StatusQueued), now, string(envelope.StatusClaimed), now, string(envelope.StatusClaimed), string(prev), now); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// OpenClaim returns the ask agent has claimed and not yet answered, when it
// has exactly one. The relay uses it to continue a chain when a sender forgets
// to name a parent. With several open claims it cannot tell which one a new
// request continues, so it reports none and the request starts a new chain.
// Claimed notifies never count: nothing closes them.
func (s *Store) OpenClaim(ctx context.Context, agent string) (envelope.Request, bool, error) {
	return s.openClaim(ctx, `SELECT `+requestCols+` FROM requests WHERE to_agent = ? AND status = ? AND kind = ?
		ORDER BY updated_at DESC LIMIT 2`, agent, string(envelope.StatusClaimed), string(envelope.KindAsk))
}

// OpenClaimFor is OpenClaim counting only the claims made from session or
// with no session, so a session's ask is filed under its own work and not
// under another session's. The relay still checks the ask against
// OpenClaim; see policy.Prepare.
func (s *Store) OpenClaimFor(ctx context.Context, agent, session string) (envelope.Request, bool, error) {
	return s.openClaim(ctx, `SELECT `+requestCols+` FROM requests WHERE to_agent = ? AND status = ? AND kind = ? AND claim_session IN (?, '')
		ORDER BY updated_at DESC LIMIT 2`, agent, string(envelope.StatusClaimed), string(envelope.KindAsk), session)
}

func (s *Store) openClaim(ctx context.Context, query string, args ...any) (envelope.Request, bool, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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

const requestCols = `id, from_agent, to_agent, parent_id, trace_id, hop, chain, kind, body, status, created_at, attachments, exchanges, resumed, was_held, approved, group_id, urgent, progress_note, progress_at, moved_from, room_post_id, to_project, follow_of, from_guest, guest_approved`

type scanner interface{ Scan(dest ...any) error }

func scanRequest(sc scanner) (envelope.Request, envelope.Status, error) {
	var r envelope.Request
	var chain, kind, status, atts, exchanges, movedFrom string
	var created, progressAt int64
	var note string
	var roomPost sql.NullInt64
	if err := sc.Scan(&r.ID, &r.From, &r.To, &r.ParentID, &r.TraceID, &r.Hop, &chain, &kind, &r.Body, &status, &created, &atts, &exchanges, &r.Resumed, &r.WasHeld, &r.Approved, &r.Group, &r.Urgent, &note, &progressAt, &movedFrom, &roomPost, &r.Project, &r.FollowOf, &r.FromGuest, &r.GuestApproved); err != nil {
		return envelope.Request{}, "", err
	}
	r.RoomPostID = roomPost.Int64
	if err := json.Unmarshal([]byte(chain), &r.Chain); err != nil {
		return envelope.Request{}, "", err
	}
	if movedFrom != "" {
		if err := json.Unmarshal([]byte(movedFrom), &r.MovedFrom); err != nil {
			return envelope.Request{}, "", err
		}
	}
	if err := json.Unmarshal([]byte(exchanges), &r.Exchanges); err != nil {
		return envelope.Request{}, "", err
	}
	var err error
	if r.Attachments, err = decodeAttachments(atts); err != nil {
		return envelope.Request{}, "", err
	}
	if progressAt != 0 && envelope.Status(status) == envelope.StatusClaimed {
		r.Progress = &envelope.Progress{Note: note, At: time.UnixMilli(progressAt).UTC(), By: r.To}
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

func sortByPriority(rs []envelope.Request) {
	slices.SortStableFunc(rs, func(a, b envelope.Request) int {
		if a.Urgent != b.Urgent {
			if a.Urgent {
				return -1
			}
			return 1
		}
		return a.CreatedAt.Compare(b.CreatedAt)
	})
}

func randomID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Held lists unexpired requests waiting for the owner.
func (s *Store) Held(ctx context.Context) ([]envelope.Request, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestCols+` FROM requests WHERE status = ? AND expires_at > ? ORDER BY created_at`, string(envelope.StatusHeld), s.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequests(rows)
}

// CountHeld counts the held requests Held would list: held and unexpired.
func (s *Store) CountHeld(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE status = ? AND expires_at > ?`, string(envelope.StatusHeld), s.now().UnixMilli()).Scan(&n)
	return n, err
}

// Release queues a held request with a fresh delivery TTL and its original
// creation time. Time held is not time queued, so its fallback clock starts
// here.
func (s *Store) Release(ctx context.Context, id string, ttl time.Duration) (envelope.Request, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ?, approved = 1, guest_approved = from_guest, updated_at = ?, queued_at = ?, fallback_woken = 0, expires_at = ? WHERE id = ? AND status = ? AND expires_at > ?`, string(envelope.StatusQueued), now.UnixMilli(), now.UnixMilli(), now.Add(ttl).UnixMilli(), id, string(envelope.StatusHeld), now.UnixMilli())
	if err != nil {
		return envelope.Request{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Request{}, ErrWrongState
	}
	req, _, err := s.lookup(ctx, id)
	return req, err
}

// DenyHeld atomically declines a held request and saves the owner's reason.
func (s *Store) DenyHeld(ctx context.Context, id, reason string) (envelope.Request, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.Request{}, err
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, updated_at = ? WHERE id = ? AND status = ? AND expires_at > ?`, string(envelope.StatusDeclined), now, id, string(envelope.StatusHeld), now)
	if err != nil {
		return envelope.Request{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Request{}, ErrWrongState
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO replies (request_id,from_agent,status,body,created_at) VALUES (?,?,?,?,?)`, id, "relay", string(envelope.StatusDeclined), reason, now); err != nil {
		return envelope.Request{}, err
	}
	if err := tx.Commit(); err != nil {
		return envelope.Request{}, err
	}
	req, _, err := s.lookup(ctx, id)
	return req, err
}

func (s *Store) migrateGroups() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'group_id')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN group_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS requests_group ON requests(from_agent, group_id)`)
	return err
}

// MaxGroupRequests bounds membership per sender and group tag.
const MaxGroupRequests = 8

// RequestsByGroup lists only ids and targets sent by sender in group.
func (s *Store) RequestsByGroup(ctx context.Context, sender, group string) ([]envelope.GroupMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, to_agent FROM requests WHERE from_agent = ? AND group_id = ? ORDER BY created_at, id LIMIT ?`, sender, group, MaxGroupRequests)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []envelope.GroupMember
	for rows.Next() {
		var member envelope.GroupMember
		if err := rows.Scan(&member.ID, &member.To); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

// PollFeatures is poll-only capability state, separate from legacy advertisements.
func (s *Store) PollFeatures(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, poll_features FROM agents WHERE poll_features IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, state string
		if err := rows.Scan(&name, &state); err != nil {
			return nil, err
		}
		out[name] = state
	}
	return out, rows.Err()
}

// SetPollFeatures persists poll-only capabilities and the last unsupported poll.
func (s *Store) SetPollFeatures(ctx context.Context, name, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET poll_features = ? WHERE name = ?`, state, name)
	return err
}

// CountQueuedWithPings counts queued requests and pings from the same
// snapshot, leaving out requests held back (HoldBack) until their time.
func (s *Store) CountQueuedWithPings(ctx context.Context, agent string) (int, int, error) {
	return s.CountQueuedWithPingsFor(ctx, agent, QueueScope{})
}

// CountQueuedWithPingsFor is CountQueuedWithPings of what scope may take.
func (s *Store) CountQueuedWithPingsFor(ctx context.Context, agent string, scope QueueScope) (int, int, error) {
	var queued, pings int
	at := s.now()
	now := at.UnixMilli()
	filter, filterArgs := scope.filter(at)
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(CASE WHEN kind = ? THEN 1 END) FROM requests WHERE to_agent = ? AND status = ? AND expires_at > ? AND deliver_after <= ?`+filter,
		append([]any{string(envelope.KindPing), agent, string(envelope.StatusQueued), now, now}, filterArgs...)...).Scan(&queued, &pings)
	return queued, pings, err
}
