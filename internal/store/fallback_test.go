package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// An agents table from before fallbacks gains the column on open. A fallback
// survives a rejoin that rewrites the row and a reopen, "" clears it, and
// removing the fallback agent clears it on every agent that names it.
func TestAgentFallbackMigratesPersistsAndClearsOnRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE agents (name TEXT PRIMARY KEY, node_id TEXT NOT NULL, node_name TEXT NOT NULL, joined_at INTEGER NOT NULL, kind TEXT, node_user TEXT, last_seen_at INTEGER, version TEXT)`,
		`INSERT INTO agents VALUES ('claude-code', 'nM', 'mac', 1790000000000, 'claude-code', NULL, NULL, NULL)`,
		`INSERT INTO agents VALUES ('claude-window', 'nM', 'mac', 1790000000000, 'claude-code', NULL, NULL, NULL)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, c := open(t, path)
	ctx := context.Background()
	if a, _, err := s.AgentByName(ctx, "claude-code"); err != nil || a.Fallback != "" {
		t.Fatalf("after migration = %+v, %v", a, err)
	}
	if ok, err := s.SetAgentFallback(ctx, "claude-code", "claude-window"); err != nil || !ok {
		t.Fatalf("set: %v, %v", ok, err)
	}
	if ok, err := s.SetAgentFallback(ctx, "nobody", "claude-window"); err != nil || ok {
		t.Fatalf("unknown agent: %v, %v", ok, err)
	}
	if err := s.PutAgent(ctx, identity.Agent{Name: "claude-code", NodeID: "nM2", NodeName: "mac-2", JoinedAt: c.t, Kind: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := s.AgentByName(ctx, "claude-code"); a.Fallback != "claude-window" || a.NodeID != "nM2" {
		t.Fatalf("after rejoin = %+v", a)
	}
	s.Close()
	s2, _ := open(t, path)
	if a, _, err := s2.AgentByName(ctx, "claude-code"); err != nil || a.Fallback != "claude-window" {
		t.Fatalf("after reopen = %+v, %v", a, err)
	}
	if ok, err := s2.SetAgentFallback(ctx, "claude-code", ""); err != nil || !ok {
		t.Fatalf("clear: %v, %v", ok, err)
	}
	if a, _, _ := s2.AgentByName(ctx, "claude-code"); a.Fallback != "" {
		t.Fatalf("after clearing = %+v", a)
	}
	if _, err := s2.SetAgentFallback(ctx, "claude-code", "claude-window"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.DeleteAgent(ctx, "claude-window"); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := s2.AgentByName(ctx, "claude-code"); a.Fallback != "" {
		t.Fatalf("fallback to a removed agent = %+v", a)
	}
}

// An untaken ask moves to the fallback with its id, sender, chain, body,
// urgency and guest mark; the old target joins moved_from, the project
// label goes, and its queue clock restarts, so the new target gets a full
// grace before its own untaken check.
func TestRerouteUntakenMoves(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req, err := s.Enqueue(ctx, envelope.Request{From: "instinct", To: "claude-code", Kind: envelope.KindAsk, Body: "review the plan", Hop: 1, Chain: []string{"instinct"},
		Urgent: true, Project: "agent-tincan", FromGuest: true, GuestApproved: true}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(20 * time.Minute)
	moved, err := s.RerouteUntaken(ctx, req.ID, "claude-code", "claude-window")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != req.ID || moved.From != "instinct" || moved.To != "claude-window" || moved.Body != req.Body || moved.TraceID != req.TraceID ||
		!slices.Equal(moved.Chain, req.Chain) || !moved.Urgent || !moved.FromGuest || !moved.GuestApproved || moved.Project != "" ||
		!slices.Equal(moved.MovedFrom, []string{"claude-code"}) {
		t.Fatalf("moved = %+v", moved)
	}
	since, err := s.UntakenSince(ctx, req.ID)
	if err != nil || !since.Equal(c.t) {
		t.Fatalf("queue clock = %v, %v; want %v", since, err, c.t)
	}
	if !moved.Rerouted {
		t.Fatalf("moved ask not marked rerouted: %+v", moved)
	}
	// One hop only: a moved ask never moves again, back or onward, and
	// failover does not carry it on either.
	if _, err := s.Move(ctx, req.ID, "claude-window", "gemini-web", false); !errors.Is(err, ErrWrongState) {
		t.Fatalf("failover after a fallback move: %v", err)
	}
	if _, err := s.RerouteUntaken(ctx, req.ID, "claude-window", "codex"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("second hop: %v", err)
	}
	if _, err := s.RerouteUntaken(ctx, req.ID, "claude-window", "claude-code"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("ping-pong: %v", err)
	}
	if _, err := s.Claim(ctx, req.ID, "claude-window", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "claude-window", envelope.Reply{Status: envelope.StatusAnswered, Body: "looks good"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Get(ctx, req.ID, "instinct")
	if err != nil || res.Reply == nil || res.Reply.From != "claude-window" || res.Reply.Body != "looks good" {
		t.Fatalf("asker sees %+v %v", res, err)
	}
}

// The move is one conditional write: an ask its target claimed after the
// sweep read it, one claimed once and requeued, one the asker cancelled,
// and one that is no longer the old target's are left where they are.
func TestRerouteUntakenRaceSafe(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()

	claimed := ask(t, s, "instinct", "claude-code", "claimed in the race")
	if _, err := s.Claim(ctx, claimed.ID, "claude-code", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RerouteUntaken(ctx, claimed.ID, "claude-code", "claude-window"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("claimed: %v", err)
	}
	if got, _, _ := s.Request(ctx, claimed.ID); got.To != "claude-code" {
		t.Fatalf("claimed ask moved to %s", got.To)
	}

	requeued := ask(t, s, "instinct", "claude-code", "claimed once")
	if _, err := s.Claim(ctx, requeued.ID, "claude-code", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE requests SET status = 'queued', claim_lapsed = 1 WHERE id = ?`, requeued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RerouteUntaken(ctx, requeued.ID, "claude-code", "claude-window"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("requeued after a claim: %v", err)
	}

	cancelled := ask(t, s, "instinct", "claude-code", "cancelled")
	if _, err := s.Cancel(ctx, cancelled.ID, "instinct"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RerouteUntaken(ctx, cancelled.ID, "claude-code", "claude-window"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("cancelled: %v", err)
	}

	other := ask(t, s, "instinct", "codex", "not claude-code's")
	if _, err := s.RerouteUntaken(ctx, other.ID, "claude-code", "claude-window"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("wrong old target: %v", err)
	}
}
