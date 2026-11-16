package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// An agents table from before task hosts gains the column on open, with no
// agent marked, and opening the migrated database again changes nothing. The
// mark survives a reopen, and a clear stores no mark.
func TestAgentTaskHostMigratesTwiceAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE agents (name TEXT PRIMARY KEY, node_id TEXT NOT NULL, node_name TEXT NOT NULL, joined_at INTEGER NOT NULL, kind TEXT, node_user TEXT, last_seen_at INTEGER, version TEXT)`,
		`INSERT INTO agents VALUES ('grokbot', 'nG', 'grok-bot', 1790000000000, 'hermes', NULL, NULL, NULL)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, _ := open(t, path)
	ctx := context.Background()
	if a, ok, err := s.AgentByName(ctx, "grokbot"); err != nil || !ok || a.TaskHost {
		t.Fatalf("after migration = %+v, %v, %v", a, ok, err)
	}
	if err := s.migrateAgentTaskHost(); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if ok, err := s.SetAgentTaskHost(ctx, "grokbot", true); err != nil || !ok {
		t.Fatalf("set: %v, %v", ok, err)
	}
	if ok, err := s.SetAgentTaskHost(ctx, "nobody", true); err != nil || ok {
		t.Fatalf("unknown agent: %v, %v", ok, err)
	}
	s.Close()

	s2, _ := open(t, path)
	if a, _, err := s2.AgentByName(ctx, "grokbot"); err != nil || !a.TaskHost {
		t.Fatalf("after reopen = %+v, %v", a, err)
	}
	if byNode, err := s2.AgentsByNode(ctx, "nG"); err != nil || len(byNode) != 1 || !byNode[0].TaskHost {
		t.Fatalf("by node = %+v, %v", byNode, err)
	}
	if ok, err := s2.SetAgentTaskHost(ctx, "grokbot", false); err != nil || !ok {
		t.Fatalf("clear: %v, %v", ok, err)
	}
	if a, _, _ := s2.AgentByName(ctx, "grokbot"); a.TaskHost {
		t.Fatalf("after clearing = %+v", a)
	}
	var stored sql.NullInt64
	if err := s2.db.QueryRow(`SELECT task_host FROM agents WHERE name = 'grokbot'`).Scan(&stored); err != nil || stored.Valid {
		t.Fatalf("stored mark after clearing = %+v, %v", stored, err)
	}
}

// A rewrite of an agent's row keeps the task host mark only when it carries
// the mark and leaves the agent on the node it had. A row written for a new
// join carries no mark and a moved agent has a new node, so both clear it,
// and no rewrite ever sets a mark the stored row lacked.
func TestPutAgentKeepsTaskHostOnlyOnTheSameNode(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	join := identity.Agent{Name: "grokbot", NodeID: "nG", NodeName: "grok-bot", JoinedAt: c.t}
	mark := func() identity.Agent {
		t.Helper()
		if err := s.PutAgent(ctx, join); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.SetAgentTaskHost(ctx, "grokbot", true); err != nil || !ok {
			t.Fatalf("set: %v, %v", ok, err)
		}
		a, _, err := s.AgentByName(ctx, "grokbot")
		if err != nil || !a.TaskHost {
			t.Fatalf("marked agent = %+v, %v", a, err)
		}
		return a
	}
	host := func() bool {
		t.Helper()
		a, ok, err := s.AgentByName(ctx, "grokbot")
		if err != nil || !ok {
			t.Fatalf("read: %v, %v", ok, err)
		}
		return a.TaskHost
	}

	// The login backfill: the stored row with one field filled in.
	a := mark()
	a.NodeUser = "owner@example.com"
	if err := s.PutAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if !host() {
		t.Fatal("a rewrite on the same node dropped the mark")
	}

	// A new join on the same machine: a fresh row with no mark.
	if err := s.PutAgent(ctx, join); err != nil {
		t.Fatal(err)
	}
	if host() {
		t.Fatal("a new join under the name kept the mark")
	}

	// A move to another node, even when the row read before the move is
	// written back with its mark.
	a = mark()
	a.NodeID, a.NodeName = "nG2", "grok-bot-1"
	if err := s.PutAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if host() {
		t.Fatal("a move to another node kept the mark")
	}

	// Only the setter grants: a row that claims the mark does not get it.
	claim := join
	claim.TaskHost = true
	if err := s.PutAgent(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if host() {
		t.Fatal("a rewrite set a mark the stored row lacked")
	}
	if err := s.PutAgent(ctx, identity.Agent{Name: "scout", NodeID: "nS", NodeName: "scout", JoinedAt: c.t, TaskHost: true}); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := s.AgentByName(ctx, "scout"); a.TaskHost {
		t.Fatalf("a new row was written as a task host: %+v", a)
	}

	// Removing the agent removes the mark with the row.
	mark()
	if _, err := s.DeleteAgent(ctx, "grokbot"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgent(ctx, join); err != nil {
		t.Fatal(err)
	}
	if host() {
		t.Fatal("an agent joined after a removal was a task host")
	}
}
