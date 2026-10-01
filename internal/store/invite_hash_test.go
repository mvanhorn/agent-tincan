package store

// Tests for the invite-digest migration (migrateInviteCodeHash): legacy raw
// rows are invalidated, valid digest rows survive — including mixed tables —
// and the migration is a stable no-op on later opens.

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// digestRow is a stand-in for a real 64-hex-char invite digest as produced by
// identity.hashInviteCode.
const digestRow = "a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3"

// A mixed old table — one legacy raw code, one already-digested row —
// migrates with only the legacy row invalidated; the digest row redeems.
func TestMigrateInviteCodeHashPreservesDigestsInMixedTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE invites (code TEXT PRIMARY KEY, name TEXT NOT NULL, expires INTEGER NOT NULL)`,
		`INSERT INTO invites VALUES ('AAAA-BBBB', 'muse', 1790000600000)`,
		`INSERT INTO invites VALUES ('` + digestRow + `', 'codex', 1790000600000)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, _ := open(t, path)
	ctx := context.Background()
	if inv, ok, err := s.TakeInvite(ctx, "AAAA-BBBB"); err != nil || ok {
		t.Fatalf("legacy raw invite after migration = %+v, %v, %v; want it invalidated", inv, ok, err)
	}
	if inv, ok, err := s.TakeInvite(ctx, digestRow); err != nil || !ok || inv.Name != "codex" {
		t.Fatalf("digest invite after migration = %+v, %v, %v; want it preserved", inv, ok, err)
	}
}

// The migration runs on every open: digest rows written by this version must
// still redeem after a reopen (regression guard — the migration must never
// treat its own output as legacy).
func TestMigrateInviteCodeHashIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idempotent.db")
	s, c := open(t, path)
	ctx := context.Background()
	if err := s.PutInvite(ctx, identity.Invite{Code: digestRow, Name: "muse", Kind: "hermes", Expires: c.t.Add(identity.InviteTTL)}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, _ := open(t, path)
	if inv, ok, err := s2.TakeInvite(ctx, digestRow); err != nil || !ok || inv.Kind != "hermes" {
		t.Fatalf("digest invite after reopen = %+v, %v, %v", inv, ok, err)
	}
	s2.Close()

	// And again: repeated opens stay a no-op for digest rows.
	s3, _ := open(t, path)
	if err := s3.PutInvite(ctx, identity.Invite{Code: digestRow, Name: "muse", Expires: c.t.Add(identity.InviteTTL)}); err != nil {
		t.Fatal(err)
	}
	s3.Close()
	s4, _ := open(t, path)
	if _, ok, err := s4.TakeInvite(ctx, digestRow); err != nil || !ok {
		t.Fatalf("digest invite after third open = %v, %v", ok, err)
	}
}

// A 64-char row that is not lowercase hex is not a valid digest shape and is
// invalidated like any other legacy row.
func TestMigrateInviteCodeHashRejectsNonHexShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonhex.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	notHex := strings.Repeat("z", 64)
	upperHex := strings.Repeat("A3", 32)
	for _, stmt := range []string{
		`CREATE TABLE invites (code TEXT PRIMARY KEY, name TEXT NOT NULL, expires INTEGER NOT NULL)`,
		`INSERT INTO invites VALUES ('` + notHex + `', 'muse', 1790000600000)`,
		`INSERT INTO invites VALUES ('` + upperHex + `', 'codex', 1790000600000)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, _ := open(t, path)
	ctx := context.Background()
	if _, ok, err := s.TakeInvite(ctx, notHex); err != nil || ok {
		t.Fatalf("non-hex 64-char row survived migration: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.TakeInvite(ctx, upperHex); err != nil || ok {
		t.Fatalf("uppercase-hex 64-char row survived migration: ok=%v err=%v", ok, err)
	}
}
