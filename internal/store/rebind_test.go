package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// A rebuilt machine is re-admitted end to end on SQLite. fakeWho has no
// NodeOnline, so the old node counts as offline. The agent keeps its kind,
// its recorded login moves with it, and its queued and claimed requests
// (keyed by name) are still its own.
func TestRebuiltMachineReadmittedOnSQLite(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	who := fakeWho{"100.0.0.1:1": {ID: "nMAC", Name: "macbook-pro-44"}, "100.0.0.3:1": {ID: "nINST", Name: "instinct", User: "mvanhorn@gmail.com"}}
	dir := identity.NewDirectory(s, who, identity.Config{Admins: []string{"macbook-pro-44"}})
	code, err := dir.InviteKind(ctx, identity.LocalAdmin, "instinct", "e2b-email")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Join(ctx, "100.0.0.3:1", code); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := s.AgentByName(ctx, "instinct"); a.NodeUser != "mvanhorn@gmail.com" {
		t.Fatalf("login not stored: %+v", a)
	}
	queued := ask(t, s, "grokbot", "instinct", "queued before the rebuild")
	claimed := ask(t, s, "grokbot", "instinct", "claimed before the rebuild")
	if _, err := s.Claim(ctx, claimed.ID, "instinct", 0); err != nil {
		t.Fatal(err)
	}

	delete(who, "100.0.0.3:1")
	who["100.0.0.7:1"] = identity.Node{ID: "nINST2", Name: "instinct-1", User: "mvanhorn@gmail.com"}
	res, err := dir.ResolveAgent(ctx, "100.0.0.7:1", "")
	if err != nil || res.Name != "instinct" || res.Rebind == nil || res.Rebind.OldNode != "nINST" {
		t.Fatalf("resolve rebuilt = %+v, %v", res, err)
	}
	a, _, _ := s.AgentByName(ctx, "instinct")
	if a.NodeID != "nINST2" || a.NodeName != "instinct-1" || a.Kind != "e2b-email" || a.NodeUser != "mvanhorn@gmail.com" {
		t.Fatalf("after rebind = %+v", a)
	}
	got, err := s.Deliver(ctx, "instinct", 10, 0)
	if err != nil || len(got) != 1 || got[0].ID != queued.ID {
		t.Fatalf("queued after rebind = %+v, %v", got, err)
	}
	if _, err := s.Reply(ctx, claimed.ID, "instinct", envelope.Reply{Body: "done", Status: envelope.StatusAnswered}); err != nil {
		t.Fatalf("reply to claimed after rebind: %v", err)
	}
}

// An agents table from before logins were recorded gains node_user on open;
// its agents read back with no login.
func TestAgentsTableGainsLoginColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE agents (name TEXT PRIMARY KEY, node_id TEXT NOT NULL, node_name TEXT NOT NULL, joined_at INTEGER NOT NULL, kind TEXT)`,
		`INSERT INTO agents VALUES ('instinct', 'nINST', 'instinct', 1790000000000, 'e2b-email')`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, c := open(t, path)
	ctx := context.Background()
	if a, ok, err := s.AgentByName(ctx, "instinct"); err != nil || !ok || a.NodeUser != "" || a.Kind != "e2b-email" {
		t.Fatalf("old agent after migration = %+v, %v, %v", a, ok, err)
	}
	if err := s.PutAgent(ctx, identity.Agent{Name: "muse", NodeID: "nMUSE", NodeName: "muse", NodeUser: "mvanhorn@gmail.com", JoinedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, _ := open(t, path)
	if a, _, _ := s2.AgentByName(ctx, "muse"); a.NodeUser != "mvanhorn@gmail.com" {
		t.Fatalf("login after reopen = %+v", a)
	}
}

// An invites table from before invites carried a kind gains the column on
// open: a pending code survives with no kind, and a kind stored afterwards
// reads back after reopening.
func TestInvitesTableGainsKindColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE invites (code TEXT PRIMARY KEY, name TEXT NOT NULL, expires INTEGER NOT NULL)`,
		`INSERT INTO invites VALUES ('OLDC-ODE2', 'instinct', 1790000600000)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, c := open(t, path)
	ctx := context.Background()
	if inv, ok, err := s.TakeInvite(ctx, "OLDC-ODE2"); err != nil || !ok || inv.Name != "instinct" || inv.Kind != "" {
		t.Fatalf("old invite after migration = %+v, %v, %v", inv, ok, err)
	}
	if err := s.PutInvite(ctx, identity.Invite{Code: "NEWC-ODE3", Name: "muse", Kind: "hermes", Expires: c.t.Add(identity.InviteTTL)}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, _ := open(t, path)
	if inv, ok, err := s2.TakeInvite(ctx, "NEWC-ODE3"); err != nil || !ok || inv.Kind != "hermes" || inv.Name != "muse" {
		t.Fatalf("invite after reopen = %+v, %v, %v", inv, ok, err)
	}
}
