package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func open(t *testing.T, path string) (*Store, *clock) {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	s.SetClock(c.now)
	return s, c
}

func ask(t *testing.T, s *Store, from, to, body string) envelope.Request {
	t.Helper()
	req, err := s.Enqueue(context.Background(), envelope.Request{From: from, To: to, Kind: envelope.KindAsk, Body: body, Hop: 1, Chain: []string{}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestIdentityStoreRoundTrip(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	if err := s.PutAgent(ctx, identity.Agent{Name: "muse", NodeID: "nMUSE", NodeName: "muse", JoinedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	// Moving muse to a new node replaces the old binding.
	s.PutAgent(ctx, identity.Agent{Name: "muse", NodeID: "nMUSE2", NodeName: "muse2", JoinedAt: c.t})
	agents, _ := s.Agents(ctx)
	if len(agents) != 1 || agents[0].NodeID != "nMUSE2" {
		t.Fatalf("agents = %+v", agents)
	}
	if ok, _ := s.DeleteAgent(ctx, "muse"); !ok {
		t.Fatal("delete should report a removed agent")
	}
	if ok, _ := s.DeleteAgent(ctx, "muse"); ok {
		t.Fatal("second delete should report nothing removed")
	}
	s.PutInvite(ctx, identity.Invite{Code: "AAAA-BBBB", Name: "muse", Expires: c.t})
	if _, ok, _ := s.TakeInvite(ctx, "AAAA-BBBB"); !ok {
		t.Fatal("first take should find the invite")
	}
	if _, ok, _ := s.TakeInvite(ctx, "AAAA-BBBB"); ok {
		t.Fatal("invite must be single use")
	}
}

// The SQLite store backs the identity directory end to end.
func TestDirectoryOnSQLite(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	who := fakeWho{"100.0.0.1:1": {ID: "nMAC", Name: "macbook-pro-44"}, "100.0.0.4:1": {ID: "nMUSE", Name: "muse"}}
	dir := identity.NewDirectory(s, who, identity.Config{Admins: []string{"macbook-pro-44"}})
	code, err := dir.Invite(ctx, "100.0.0.1:1", "muse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Join(ctx, "100.0.0.4:1", code); err != nil {
		t.Fatal(err)
	}
	if got, err := dir.Attribute(ctx, "100.0.0.4:1"); err != nil || got != "muse" {
		t.Fatalf("attribute = %q, %v", got, err)
	}
}

// Several agents on one machine, end to end on SQLite: a second join leaves
// the first agent intact, a re-invite moves only that name, and removing one
// leaves the other working.
func TestMultipleAgentsPerNodeOnSQLite(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	who := fakeWho{
		"100.0.0.1:1": {ID: "nMAC", Name: "macbook-pro-44"},
		"100.0.0.5:1": {ID: "nMINI", Name: "matts-mac-mini"},
	}
	dir := identity.NewDirectory(s, who, identity.Config{Admins: []string{"macbook-pro-44"}})
	join := func(addr, name string) {
		t.Helper()
		code, err := dir.Invite(ctx, identity.LocalAdmin, name)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := dir.Join(ctx, addr, code); err != nil || got != name {
			t.Fatalf("join %s from %s: %q, %v", name, addr, got, err)
		}
	}
	join("100.0.0.1:1", "claude-code")
	join("100.0.0.1:1", "codex")
	for _, name := range []string{"claude-code", "codex"} {
		if got, err := dir.Resolve(ctx, "100.0.0.1:1", name); err != nil || got != name {
			t.Fatalf("resolve %s: %q, %v", name, got, err)
		}
	}
	if _, err := dir.Resolve(ctx, "100.0.0.1:1", ""); !errors.Is(err, identity.ErrAgentAmbiguous) {
		t.Fatalf("no claim with two agents: want ErrAgentAmbiguous, got %v", err)
	}
	byNode, err := s.AgentsByNode(ctx, "nMAC")
	if err != nil || len(byNode) != 2 || byNode[0].Name != "claude-code" || byNode[1].Name != "codex" {
		t.Fatalf("agents by node = %+v, %v", byNode, err)
	}
	// Moving codex to the mini leaves claude-code on the laptop.
	join("100.0.0.5:1", "codex")
	if got, err := dir.Attribute(ctx, "100.0.0.1:1"); err != nil || got != "claude-code" {
		t.Fatalf("laptop after move: %q, %v", got, err)
	}
	if got, err := dir.Attribute(ctx, "100.0.0.5:1"); err != nil || got != "codex" {
		t.Fatalf("mini after move: %q, %v", got, err)
	}
	join("100.0.0.5:1", "hermes")
	if err := dir.Remove(ctx, identity.LocalAdmin, "codex"); err != nil {
		t.Fatal(err)
	}
	if got, err := dir.Attribute(ctx, "100.0.0.5:1"); err != nil || got != "hermes" {
		t.Fatalf("mini after removing codex: %q, %v", got, err)
	}
}

func TestAgentKindOnSQLite(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	s.PutAgent(ctx, identity.Agent{Name: "hermes", NodeID: "nMINI", NodeName: "mini", JoinedAt: c.t})
	if a, _, _ := s.AgentByName(ctx, "hermes"); a.Kind != "" {
		t.Fatalf("kind should start empty, got %q", a.Kind)
	}
	if ok, err := s.SetAgentKind(ctx, "hermes", "hermes"); err != nil || !ok {
		t.Fatalf("set kind: %v, %v", ok, err)
	}
	if ok, err := s.SetAgentKind(ctx, "nobody", "codex"); err != nil || ok {
		t.Fatalf("set kind on unknown agent: %v, %v", ok, err)
	}
	agents, _ := s.Agents(ctx)
	if len(agents) != 1 || agents[0].Kind != "hermes" {
		t.Fatalf("agents = %+v", agents)
	}
	// Clearing stores NULL and reads back empty.
	s.SetAgentKind(ctx, "hermes", "")
	if a, _, _ := s.AgentByName(ctx, "hermes"); a.Kind != "" {
		t.Fatalf("cleared kind = %q", a.Kind)
	}
	s.PutAgent(ctx, identity.Agent{Name: "codex", NodeID: "nMAC", NodeName: "mac", JoinedAt: c.t, Kind: "codex"})
	if a, _, _ := s.AgentByName(ctx, "codex"); a.Kind != "codex" {
		t.Fatalf("PutAgent kind = %q", a.Kind)
	}
}

// A relay database created before multi-agent support (node_id UNIQUE, no
// kind column) migrates on startup, keeps its agents, and then accepts a
// second name on one node.
func TestOldSchemaMigratesOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE agents (name TEXT PRIMARY KEY, node_id TEXT NOT NULL UNIQUE, node_name TEXT NOT NULL, joined_at INTEGER NOT NULL)`,
		`INSERT INTO agents VALUES ('grokbot', 'nGROK', 'grok-bot', 1790000000000)`,
		`INSERT INTO agents VALUES ('claude-code', 'nMAC', 'macbook-pro-44', 1790000000001)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, c := open(t, path)
	ctx := context.Background()
	agents, err := s.Agents(ctx)
	if err != nil || len(agents) != 2 || agents[0].Name != "claude-code" || agents[1].Name != "grokbot" || agents[1].NodeID != "nGROK" {
		t.Fatalf("agents after migration = %+v, %v", agents, err)
	}
	if err := s.PutAgent(ctx, identity.Agent{Name: "codex", NodeID: "nMAC", NodeName: "macbook-pro-44", JoinedAt: c.t, Kind: "codex"}); err != nil {
		t.Fatalf("second agent on node after migration: %v", err)
	}
	byNode, _ := s.AgentsByNode(ctx, "nMAC")
	if len(byNode) != 2 {
		t.Fatalf("nMAC agents = %+v", byNode)
	}
	var idx int
	s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'agents' AND name = 'agents_node_id'`).Scan(&idx)
	if idx != 1 {
		t.Fatal("migration should add the non-unique node_id index")
	}
	s.Close()
	// Reopening an already-migrated database is a no-op.
	s2, _ := open(t, path)
	if agents, _ := s2.Agents(ctx); len(agents) != 3 {
		t.Fatalf("agents after reopen = %+v", agents)
	}
}

type fakeWho map[string]identity.Node

func (f fakeWho) WhoIs(_ context.Context, addr string) (identity.Node, error) {
	n, ok := f[addr]
	if !ok {
		return identity.Node{}, errors.New("unknown")
	}
	return n, nil
}

func TestAskClaimReplyGet(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "instinct", "what's in the report")
	if req.ID == "" || req.TraceID != req.ID || req.CreatedAt.IsZero() {
		t.Fatalf("enqueue did not assign id/trace/time: %+v", req)
	}
	got, err := s.Deliver(ctx, "instinct", 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != req.ID {
		t.Fatalf("deliver = %+v, %v", got, err)
	}
	if again, _ := s.Deliver(ctx, "instinct", 10, time.Minute); len(again) != 0 {
		t.Fatalf("a delivered request must not be delivered twice: %+v", again)
	}
	if _, err := s.Claim(ctx, req.ID, "instinct", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "instinct", envelope.Reply{Status: envelope.StatusAnswered, Body: "3 rows"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Get(ctx, req.ID, "grokbot")
	if err != nil || res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.Body != "3 rows" || res.Reply.From != "instinct" {
		t.Fatalf("get = %+v, %v", res, err)
	}
	if _, err := s.Reply(ctx, req.ID, "instinct", envelope.Reply{Status: envelope.StatusAnswered, Body: "again"}); !errors.Is(err, ErrWrongState) {
		t.Fatalf("second reply: want ErrWrongState, got %v", err)
	}
}

func TestOnlyTheRightAgentMayAct(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "muse", "call Joe's Garage")
	if _, err := s.Claim(ctx, req.ID, "instinct", time.Minute); !errors.Is(err, ErrForbidden) {
		t.Errorf("instinct claim: want ErrForbidden, got %v", err)
	}
	if _, err := s.Reply(ctx, req.ID, "instinct", envelope.Reply{Status: envelope.StatusAnswered, Body: "x"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("instinct reply: want ErrForbidden, got %v", err)
	}
	if _, err := s.Get(ctx, req.ID, "instinct"); !errors.Is(err, ErrNotFound) {
		t.Errorf("instinct get: want ErrNotFound, got %v", err)
	}
	if err := s.Cancel(ctx, req.ID, "instinct"); !errors.Is(err, ErrForbidden) {
		t.Errorf("instinct cancel: want ErrForbidden, got %v", err)
	}
	if err := s.Cancel(ctx, req.ID, "muse"); !errors.Is(err, ErrForbidden) {
		t.Errorf("target cancel: want ErrForbidden, got %v", err)
	}
	if err := s.Cancel(ctx, req.ID, "grokbot"); err != nil {
		t.Errorf("sender cancel: %v", err)
	}
	if _, err := s.Claim(ctx, req.ID, "muse", time.Minute); !errors.Is(err, ErrWrongState) {
		t.Errorf("claim after cancel: want ErrWrongState, got %v", err)
	}
}

func TestCannotCancelClaimed(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "muse", "x")
	s.Claim(ctx, req.ID, "muse", time.Minute)
	if err := s.Cancel(ctx, req.ID, "grokbot"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("want ErrWrongState, got %v", err)
	}
}

// AE2: a request past its TTL expires and the sender sees that.
func TestTTLExpiry(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "muse", "x")
	c.advance(time.Hour + time.Second)
	if got, _ := s.Deliver(ctx, "muse", 10, time.Minute); len(got) != 0 {
		t.Fatalf("expired request was delivered: %+v", got)
	}
	tr, err := s.Sweep(ctx)
	if err != nil || len(tr) != 1 || tr[0].Status != envelope.StatusExpired {
		t.Fatalf("sweep = %+v, %v", tr, err)
	}
	if res, _ := s.Get(ctx, req.ID, "grokbot"); res.Status != envelope.StatusExpired {
		t.Fatalf("status = %s, want expired", res.Status)
	}
}

// A request delivered but never claimed (poller crashed) must return to the
// queue, not strand.
func TestDeliveryLeaseReturnsToQueue(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "muse", "x")
	s.Deliver(ctx, "muse", 10, time.Minute)
	c.advance(2 * time.Minute)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Deliver(ctx, "muse", 10, time.Minute)
	if len(got) != 1 || got[0].ID != req.ID {
		t.Fatalf("lease-expired request not redelivered: %+v", got)
	}
}

func TestClaimLeaseReturnsToQueue(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "muse", "x")
	s.Claim(ctx, req.ID, "muse", time.Minute)
	c.advance(2 * time.Minute)
	s.Sweep(ctx)
	if res, _ := s.Get(ctx, req.ID, "grokbot"); res.Status != envelope.StatusQueued {
		t.Fatalf("status = %s, want queued", res.Status)
	}
}

func TestOnlyOneClaimWins(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	ask(t, s, "grokbot", "muse", "x")
	a, _ := s.Deliver(ctx, "muse", 10, time.Minute)
	b, _ := s.Deliver(ctx, "muse", 10, time.Minute)
	if len(a)+len(b) != 1 {
		t.Fatalf("two pollers both received the request: %d + %d", len(a), len(b))
	}
}

func TestSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	req := ask(t, s, "grokbot", "muse", "x")
	s.Close()
	s2, _ := open(t, path)
	got, err := s2.Deliver(context.Background(), "muse", 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != req.ID {
		t.Fatalf("after restart deliver = %+v, %v", got, err)
	}
}

func TestCancelAllFor(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	ask(t, s, "grokbot", "muse", "a")
	ask(t, s, "instinct", "muse", "b")
	sent := ask(t, s, "muse", "grokbot", "c")
	other := ask(t, s, "instinct", "grokbot", "d")
	ids, err := s.CancelAllFor(ctx, "muse")
	if err != nil || len(ids) != 3 {
		t.Fatalf("cancelled %v, %v", ids, err)
	}
	if _, st, _ := s.Request(ctx, sent.ID); st != envelope.StatusCancelled {
		t.Fatalf("request sent by muse: status %s, want cancelled", st)
	}
	if got, _ := s.Deliver(ctx, "grokbot", 10, time.Minute); len(got) != 1 || got[0].ID != other.ID {
		t.Fatalf("only the request from instinct should reach grokbot: %+v", got)
	}
}

// A claimed notify has no reply coming, so its claim lease must not requeue
// it every ClaimLease forever.
func TestClaimedNotifyNotRequeued(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req, err := s.Enqueue(ctx, envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindNotify, Body: "fyi", Hop: 1, Chain: []string{}}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Deliver(ctx, "muse", 10, time.Minute); len(got) != 1 {
		t.Fatalf("deliver = %+v", got)
	}
	if _, err := s.Claim(ctx, req.ID, "muse", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(31 * time.Minute)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Deliver(ctx, "muse", 10, time.Minute); len(got) != 0 {
		t.Fatalf("claimed notify redelivered: %+v", got)
	}
}

func TestOpenClaim(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	if _, ok, _ := s.OpenClaim(ctx, "muse"); ok {
		t.Fatal("no claim yet")
	}
	req := ask(t, s, "instinct", "muse", "call the dentist")
	s.Claim(ctx, req.ID, "muse", time.Minute)
	got, ok, err := s.OpenClaim(ctx, "muse")
	if err != nil || !ok || got.ID != req.ID {
		t.Fatalf("open claim = %+v %v %v", got, ok, err)
	}
}

// With more than one open claim the relay cannot tell which one a new ask
// continues, and a claimed notify is never a parent.
func TestOpenClaimOnlyWhenUnambiguous(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	n, _ := s.Enqueue(ctx, envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindNotify, Body: "fyi", Hop: 1, Chain: []string{}}, time.Hour)
	s.Claim(ctx, n.ID, "muse", time.Minute)
	if got, ok, _ := s.OpenClaim(ctx, "muse"); ok {
		t.Fatalf("claimed notify became a parent: %+v", got)
	}
	a := ask(t, s, "instinct", "muse", "a")
	s.Claim(ctx, a.ID, "muse", time.Minute)
	if got, ok, err := s.OpenClaim(ctx, "muse"); err != nil || !ok || got.ID != a.ID {
		t.Fatalf("single ask claim = %+v %v %v", got, ok, err)
	}
	b := ask(t, s, "grokbot", "muse", "b")
	s.Claim(ctx, b.ID, "muse", time.Minute)
	if got, ok, err := s.OpenClaim(ctx, "muse"); err != nil || ok {
		t.Fatalf("two open claims: want none, got %+v %v %v", got, ok, err)
	}
}

// An invite keeps its kind, and a kindless invite reads back empty.
func TestInviteKindOnSQLite(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	s.PutInvite(ctx, identity.Invite{Code: "AAAA-BBBB", Name: "hermes", Kind: "hermes", Expires: c.t})
	s.PutInvite(ctx, identity.Invite{Code: "CCCC-DDDD", Name: "muse", Expires: c.t})
	if inv, ok, err := s.TakeInvite(ctx, "AAAA-BBBB"); err != nil || !ok || inv.Kind != "hermes" || inv.Name != "hermes" {
		t.Fatalf("take = %+v, %v, %v", inv, ok, err)
	}
	if inv, ok, err := s.TakeInvite(ctx, "CCCC-DDDD"); err != nil || !ok || inv.Kind != "" {
		t.Fatalf("take kindless = %+v, %v, %v", inv, ok, err)
	}
}

// An invites table from before invite kinds gains the column on open; its
// legacy raw codes are invalidated by the digest migration (a declared
// compatibility break — see TestInvitesTableGainsKindColumn), while a new
// digest row keeps its kind across reopening.
func TestOldInvitesTableMigratesOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE invites (code TEXT PRIMARY KEY, name TEXT NOT NULL, expires INTEGER NOT NULL)`,
		`INSERT INTO invites VALUES ('AAAA-BBBB', 'muse', 1790000600000)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, c := open(t, path)
	ctx := context.Background()
	if inv, ok, err := s.TakeInvite(ctx, "AAAA-BBBB"); err != nil || ok {
		t.Fatalf("legacy raw invite after migration = %+v, %v, %v; want it invalidated", inv, ok, err)
	}
	digest := strings.Repeat("c4", 32) // stand-in for a real 64-hex-char digest
	if err := s.PutInvite(ctx, identity.Invite{Code: digest, Name: "codex", Kind: "codex", Expires: c.t}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, _ := open(t, path)
	if inv, ok, _ := s2.TakeInvite(ctx, digest); !ok || inv.Kind != "codex" {
		t.Fatalf("kind after reopen = %+v", inv)
	}
}

// A claim whose lease runs out after the request's TTL expires it directly;
// requeueing it would only wake the agent for a request Deliver then rejects.
func TestSweepExpiresClaimPastTTLInsteadOfRequeueing(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "hermes", "x") // one-hour TTL
	if _, err := s.Deliver(ctx, "hermes", 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, req.ID, "hermes", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	c.advance(2*time.Hour + time.Second) // past both the TTL and the claim lease
	tr, err := s.Sweep(ctx)
	if err != nil || len(tr) != 1 || tr[0].Status != envelope.StatusExpired {
		t.Fatalf("sweep = %+v, %v; want one expiry and no requeue", tr, err)
	}
}

// An agents table from before last-seen tracking gains last_seen_at on open,
// with NULL for existing agents, and then records activity. This covers both
// the rebuild path (UNIQUE node_id) and a current-shape table missing only
// the new column.
func TestAgentsTableGainsLastSeenOnOpen(t *testing.T) {
	for name, stmts := range map[string][]string{
		"rebuild": {
			`CREATE TABLE agents (name TEXT PRIMARY KEY, node_id TEXT NOT NULL UNIQUE, node_name TEXT NOT NULL, joined_at INTEGER NOT NULL)`,
			`INSERT INTO agents VALUES ('grokbot', 'nGROK', 'grok-bot', 1790000000000)`,
		},
		"alter": {
			`CREATE TABLE agents (name TEXT PRIMARY KEY, node_id TEXT NOT NULL, node_name TEXT NOT NULL, joined_at INTEGER NOT NULL, kind TEXT, node_user TEXT)`,
			`INSERT INTO agents VALUES ('grokbot', 'nGROK', 'grok-bot', 1790000000000, 'webhook', NULL)`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			old, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range stmts {
				if _, err := old.Exec(stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			old.Close()

			s, c := open(t, path)
			ctx := context.Background()
			seen, err := s.AgentsLastSeen(ctx)
			if err != nil || len(seen) != 0 {
				t.Fatalf("last seen after migration = %v, %v", seen, err)
			}
			if err := s.TouchAgent(ctx, "grokbot", c.t); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s2, _ := open(t, path)
			seen, err = s2.AgentsLastSeen(ctx)
			if err != nil || !seen["grokbot"].Equal(c.t) {
				t.Fatalf("last seen after reopen = %v, %v", seen, err)
			}
		})
	}
}

// TouchAgent only moves last_seen_at forward, ignores unknown names, and
// survives a rejoin that rewrites the agent row.
func TestTouchAgentNeverMovesBackwards(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	if err := s.PutAgent(ctx, identity.Agent{Name: "hermes", NodeID: "nH", NodeName: "hermes-box", JoinedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	later := c.t.Add(5 * time.Minute)
	if err := s.TouchAgent(ctx, "hermes", later); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchAgent(ctx, "hermes", c.t); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchAgent(ctx, "nobody", later); err != nil {
		t.Fatalf("unknown agent: %v", err)
	}
	seen, err := s.AgentsLastSeen(ctx)
	if err != nil || len(seen) != 1 || !seen["hermes"].Equal(later) {
		t.Fatalf("last seen = %v, %v", seen, err)
	}
	if err := s.PutAgent(ctx, identity.Agent{Name: "hermes", NodeID: "nH2", NodeName: "hermes-new", JoinedAt: later}); err != nil {
		t.Fatal(err)
	}
	if seen, _ := s.AgentsLastSeen(ctx); !seen["hermes"].Equal(later) {
		t.Fatalf("last seen after rejoin = %v", seen)
	}
}

// A new invite for a name retires any earlier unredeemed code for it, so a
// code that was sent to the wrong place stops working once it is replaced.
// Codes for other names are untouched.
func TestNewInviteRetiresOlderCodeForSameName(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	for _, inv := range []identity.Invite{
		{Code: "OLDC-ODE1", Name: "muse", Expires: c.t},
		{Code: "OTHR-ODE2", Name: "hermes", Expires: c.t},
		{Code: "NEWC-ODE3", Name: "muse", Expires: c.t},
	} {
		if err := s.PutInvite(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := s.TakeInvite(ctx, "OLDC-ODE1"); ok {
		t.Fatal("older code for muse should be retired")
	}
	for _, code := range []string{"NEWC-ODE3", "OTHR-ODE2"} {
		if _, ok, _ := s.TakeInvite(ctx, code); !ok {
			t.Fatalf("%s should still work", code)
		}
	}
}

func TestGroupMigrationPreservesRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	req := ask(t, s, "a", "b", "existing")
	if _, err := s.db.Exec("DROP INDEX requests_group"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ALTER TABLE requests DROP COLUMN group_id"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _, err := s.Request(t.Context(), req.ID)
	if err != nil || got.Body != "existing" || got.Group != "" {
		t.Fatalf("%+v %v", got, err)
	}
	req.ID, req.Group = "", "group-test"
	if _, err := s.Enqueue(t.Context(), req, time.Hour); err != nil {
		t.Fatal(err)
	}
	rows, err := s.RequestsByGroup(t.Context(), "a", req.Group)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%+v %v", rows, err)
	}
	rows, err = s.RequestsByGroup(t.Context(), "b", req.Group)
	if err != nil || len(rows) != 0 {
		t.Fatalf("%+v %v", rows, err)
	}
}

func TestGroupMembershipOnlyReadsBoundedMetadata(t *testing.T) {
	s, _ := open(t, ":memory:")
	for range MaxGroupRequests + 2 {
		req := ask(t, s, "a", "b", "large body")
		// Simulate legacy oversized groups and malformed payload metadata.
		if _, err := s.db.Exec(`UPDATE requests SET group_id = 'group-old', chain = 'invalid', attachments = 'invalid' WHERE id = ?`, req.ID); err != nil {
			t.Fatal(err)
		}
	}
	members, err := s.RequestsByGroup(t.Context(), "a", "group-old")
	if err != nil || len(members) != MaxGroupRequests {
		t.Fatalf("%+v %v", members, err)
	}
	for _, member := range members {
		if member.ID == "" || member.To != "b" {
			t.Fatalf("%+v", member)
		}
	}
}

func TestProgressRenewsOnlyActiveClaim(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "muse", "work")
	post := func(agent string) error { return s.SetProgress(ctx, req.ID, agent, "working", 30*time.Minute) }
	if err := post("muse"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("queued: %v", err)
	}
	if _, err := s.Claim(ctx, req.ID, "muse", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(20 * time.Minute)
	if err := post("grokbot"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("non-claimer: %v", err)
	}
	if err := post("muse"); err != nil {
		t.Fatal(err)
	}
	c.advance(15 * time.Minute)
	if transitions, err := s.Sweep(ctx); err != nil || len(transitions) != 0 {
		t.Fatalf("sweep: %v %v", transitions, err)
	}
	res, err := s.Get(ctx, req.ID, "grokbot")
	if err != nil || res.Progress == nil || res.Progress.By != "muse" || !res.Progress.At.Equal(c.t.Add(-15*time.Minute)) {
		t.Fatalf("get: %+v %v", res, err)
	}
	steps, err := s.Trace(ctx, req.TraceID)
	if err != nil || len(steps) != 1 || steps[0].Progress == nil {
		t.Fatalf("trace: %+v %v", steps, err)
	}
	c.advance(16 * time.Minute)
	if err := post("muse"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("expired lease: %v", err)
	}
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, req.ID, "muse", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	res, err = s.Get(ctx, req.ID, "grokbot")
	if err != nil || res.Progress != nil {
		t.Fatalf("stale note: %+v %v", res, err)
	}
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	if err := post("muse"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("answered: %v", err)
	}
}

func TestProgressMigrationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, c := open(t, path)
	req := ask(t, s, "grokbot", "muse", "work")
	ctx := context.Background()
	if _, err := s.Claim(ctx, req.ID, "muse", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProgress(ctx, req.ID, "muse", "working", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	reopened, _ := open(t, path)
	res, err := reopened.Get(ctx, req.ID, "grokbot")
	if err != nil || res.Progress == nil || res.Progress.Note != "working" || !res.Progress.At.Equal(c.t) {
		t.Fatalf("reopened: %+v %v", res, err)
	}
}

func TestProgressNotifyStaysLeaseFree(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req, err := s.Enqueue(ctx, envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindNotify}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, req.ID, "muse", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProgress(ctx, req.ID, "muse", "working", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Hour)
	if transitions, err := s.Sweep(ctx); err != nil || len(transitions) != 0 {
		t.Fatalf("sweep: %v %v", transitions, err)
	}
}

func TestUrgentMigrationAndOrdering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, c := open(t, path)
	normal := ask(t, s, "a", "b", "normal")
	if _, err := s.db.Exec(`ALTER TABLE requests DROP COLUMN urgent`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, c = open(t, path)
	old, _, err := s.Request(t.Context(), normal.ID)
	if err != nil || old.Urgent {
		t.Fatalf("migrated = %+v, %v", old, err)
	}
	c.advance(time.Second)
	urgent, err := s.Enqueue(t.Context(), envelope.Request{From: "a", To: "b", Body: "urgent", Urgent: true}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second)
	later, err := s.Enqueue(t.Context(), envelope.Request{From: "a", To: "b", Body: "later", Urgent: true}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingRequests(t.Context(), "b", 2)
	if err != nil || len(pending) != 2 || pending[0].ID != urgent.ID || !pending[0].Urgent || pending[1].ID != later.ID {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	got, err := s.Deliver(t.Context(), "b", 2, time.Minute)
	if err != nil || len(got) != 2 || got[0].ID != urgent.ID || !got[0].Urgent || got[1].ID != later.ID {
		t.Fatalf("delivery = %+v, %v", got, err)
	}
	got, err = s.Deliver(t.Context(), "b", 2, time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != normal.ID {
		t.Fatalf("remaining = %+v, %v", got, err)
	}
}

func TestQueueStats(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	for _, tc := range []struct {
		status          string
		age, ttl, lease time.Duration
		to              string
	}{
		{"queued", 14 * time.Minute, time.Hour, 0, "muse"},
		{"delivered", 20 * time.Minute, time.Hour, time.Minute, "muse"},
		{"claimed", time.Minute, time.Hour, time.Minute, "muse"},
		{"claimed", time.Hour, time.Hour, -time.Second, "muse"},
		{"claimed", time.Hour, time.Hour, 0, "muse"},
		{"queued", time.Hour, 0, 0, "muse"},
		{"delivered", time.Hour, -time.Second, time.Minute, "muse"},
		{"claimed", time.Hour, -time.Second, time.Minute, "muse"},
		{"answered", time.Hour, time.Hour, 0, "muse"},
		{"failed", time.Hour, time.Hour, 0, "muse"},
		{"declined", time.Hour, time.Hour, 0, "muse"},
		{"cancelled", time.Hour, time.Hour, 0, "muse"},
		{"expired", time.Hour, time.Hour, 0, "muse"},
		{"queued", time.Minute, time.Hour, 0, "instinct"},
	} {
		req := ask(t, s, "grokbot", tc.to, "work")
		lease := int64(0)
		if tc.lease != 0 {
			lease = c.t.Add(tc.lease).UnixMilli()
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE requests SET status=?, created_at=?, expires_at=?, lease_until=? WHERE id=?`, tc.status, c.t.Add(-tc.age).UnixMilli(), c.t.Add(tc.ttl).UnixMilli(), lease, req.ID); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.QueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats["muse"]; got.Queued != 2 || got.Claimed != 1 || !got.OldestQueued.Equal(c.t.Add(-20*time.Minute)) {
		t.Fatalf("muse = %+v", got)
	}
	if len(stats) != 2 || stats["instinct"].Queued != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestQueueStatsUsesStatusIndex(t *testing.T) {
	s, _ := open(t, ":memory:")
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT to_agent, status, COUNT(*), MIN(created_at)
		FROM requests WHERE status IN ('queued', 'delivered', 'claimed') AND expires_at > 0
		AND (status != 'claimed' OR lease_until > 0) AND kind != 'ping' GROUP BY to_agent, status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "requests_status_to") {
		t.Fatalf("plan does not use requests_status_to:\n%s", strings.Join(plan, "\n"))
	}
}

func TestPendingRequestsListsPingsBeyondCap(t *testing.T) {
	s, c := open(t, ":memory:")
	var asks []envelope.Request
	for range 3 {
		asks = append(asks, ask(t, s, "a", "b", "work"))
		c.advance(time.Second)
	}
	ping, err := s.Enqueue(t.Context(), envelope.Request{From: "a", To: "b", Kind: envelope.KindPing, Hop: 1, Chain: []string{}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingRequests(t.Context(), "b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 || pending[0].ID != ping.ID || pending[0].Kind != envelope.KindPing ||
		pending[1].ID != asks[0].ID || pending[2].ID != asks[1].ID {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestQueueStatsIgnoresPings(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	ping, err := s.Enqueue(ctx, envelope.Request{From: "a", To: "muse", Kind: envelope.KindPing, Hop: 1, Chain: []string{}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	req := ask(t, s, "a", "muse", "work")
	stats, err := s.QueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats["muse"]; got.Queued != 1 || !got.OldestQueued.Equal(req.CreatedAt) {
		t.Fatalf("muse = %+v, ping %s", got, ping.ID)
	}
	if _, err := s.Claim(ctx, ping.ID, "muse", time.Minute); err != nil {
		t.Fatal(err)
	}
	stats, err = s.QueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats["muse"]; got.Queued != 1 || got.Claimed != 0 {
		t.Fatalf("muse after ping claim = %+v", got)
	}
}
