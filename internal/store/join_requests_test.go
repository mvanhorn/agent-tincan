package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

var joinNode = identity.Node{ID: "nNEW", Name: "new-mac", User: "owner@example.com", Key: "nodekey:aa"}

var joinSeq int

// fileJoin inserts a pending request from node for names, filed at now.
func fileJoin(s *Store, now time.Time, node string, names ...string) (identity.JoinRequest, error) {
	joinSeq++
	r := identity.JoinRequest{
		ID: fmt.Sprintf("jr%d", joinSeq), NodeID: node, NodeKey: "nodekey:aa", NodeName: "new-mac", NodeUser: "owner@example.com",
		Tags: []string{"tag:agents"}, Addrs: []netip.Addr{netip.MustParseAddr("100.64.0.9")},
		Code: "K7Q-4MD", Status: identity.JoinPending, Filed: now, Expires: now.Add(identity.JoinRequestTTL),
	}
	for _, n := range names {
		r.Agents = append(r.Agents, identity.RequestedAgent{Name: n, Kind: "codex"})
	}
	return r, s.InsertJoinRequest(context.Background(), r, now)
}

func mustFileJoin(t *testing.T, s *Store, now time.Time, node string, names ...string) identity.JoinRequest {
	t.Helper()
	r, err := fileJoin(s, now, node, names...)
	if err != nil {
		t.Fatalf("file %v from %s: %v", names, node, err)
	}
	return r
}

// A request round-trips with every recorded field.
func TestJoinRequestRoundTrip(t *testing.T) {
	s, c := open(t, ":memory:")
	r := mustFileJoin(t, s, c.t, "nNEW", "codex", "helper")
	got, ok, err := s.JoinRequest(context.Background(), r.ID, c.t)
	if err != nil || !ok {
		t.Fatalf("get = %v, %v", ok, err)
	}
	if got.NodeID != "nNEW" || got.NodeKey != "nodekey:aa" || got.NodeName != "new-mac" || got.NodeUser != "owner@example.com" ||
		len(got.Tags) != 1 || len(got.Addrs) != 1 || got.Addrs[0] != r.Addrs[0] || got.Code != "K7Q-4MD" || got.Status != identity.JoinPending ||
		len(got.Agents) != 2 || got.Agents[1] != (identity.RequestedAgent{Name: "helper", Kind: "codex"}) ||
		!got.Filed.Equal(r.Filed) || !got.Expires.Equal(r.Expires) {
		t.Fatalf("round trip = %+v", got)
	}
	if _, ok, _ := s.JoinRequest(context.Background(), "nope", c.t); ok {
		t.Fatal("an unknown id was found")
	}
	pending, err := s.PendingJoinRequests(context.Background(), c.t)
	if err != nil || len(pending) != 1 || pending[0].ID != r.ID {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

// A node may have one open request at a time; an approved one counts.
func TestJoinRequestOneOpenPerNode(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	r := mustFileJoin(t, s, c.t, "nNEW", "codex")
	if _, err := fileJoin(s, c.t, "nNEW", "helper"); !errors.Is(err, identity.ErrJoinCap) {
		t.Fatalf("second open request = %v", err)
	}
	if _, err := s.ApproveJoinRequest(ctx, r.ID, "local-admin", c.t); err != nil {
		t.Fatal(err)
	}
	if _, err := fileJoin(s, c.t, "nNEW", "helper"); !errors.Is(err, identity.ErrJoinCap) {
		t.Fatalf("request beside an approved one = %v", err)
	}
	mustFileJoin(t, s, c.t, "nOTHER", "helper")
}

// The sixth request from one node within an hour is refused, also after a
// restart; it is accepted once the first is an hour old.
func TestJoinRequestHourlyCapSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, c := open(t, path)
	ctx := context.Background()
	start := c.t
	for i := range identity.MaxJoinRequestsPerHour {
		r := mustFileJoin(t, s, c.t, "nNEW", fmt.Sprintf("a%d", i))
		if _, err := s.DenyJoinRequest(ctx, r.ID, "local-admin", c.t); err != nil {
			t.Fatal(err)
		}
		c.advance(time.Minute)
	}
	s.Close()
	s, _ = open(t, path)
	if _, err := fileJoin(s, c.t, "nNEW", "late"); !errors.Is(err, identity.ErrJoinCap) {
		t.Fatalf("sixth request in an hour = %v", err)
	}
	if _, err := s.SweepJoinRequests(ctx, c.t); err != nil {
		t.Fatal(err)
	}
	if _, err := fileJoin(s, c.t, "nNEW", "late"); !errors.Is(err, identity.ErrJoinCap) {
		t.Fatalf("sixth request in an hour after a sweep = %v", err)
	}
	mustFileJoin(t, s, start.Add(time.Hour), "nNEW", "late")
}

// The relay holds at most ten pending requests.
func TestJoinRequestPendingCap(t *testing.T) {
	s, c := open(t, ":memory:")
	for i := range identity.MaxPendingJoinRequests {
		mustFileJoin(t, s, c.t, fmt.Sprintf("n%d", i), fmt.Sprintf("a%d", i))
	}
	if _, err := fileJoin(s, c.t, "n99", "a99"); !errors.Is(err, identity.ErrJoinCap) {
		t.Fatalf("eleventh pending request = %v", err)
	}
}

// Decisions happen once, on a live request.
func TestJoinRequestDecisions(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()

	expired := mustFileJoin(t, s, c.t, "n1", "a1")
	if _, err := s.ApproveJoinRequest(ctx, expired.ID, "local-admin", c.t.Add(identity.JoinRequestTTL)); !errors.Is(err, identity.ErrJoinState) {
		t.Fatalf("approve after expiry = %v", err)
	}

	approved := mustFileJoin(t, s, c.t, "n2", "a2")
	got, err := s.ApproveJoinRequest(ctx, approved.ID, "phone:pixel", c.t)
	if err != nil || got.Status != identity.JoinApproved || got.DecidedBy != "phone:pixel" || !got.Decided.Equal(c.t) {
		t.Fatalf("approve = %+v, %v", got, err)
	}
	if got, err := s.DenyJoinRequest(ctx, approved.ID, "local-admin", c.t); !errors.Is(err, identity.ErrJoinState) || got.Status != identity.JoinApproved {
		t.Fatalf("deny after approve = %+v, %v", got, err)
	}

	denied := mustFileJoin(t, s, c.t, "n3", "a3")
	got, err = s.DenyJoinRequest(ctx, denied.ID, "local-admin", c.t)
	if err != nil || got.Status != identity.JoinDenied || len(got.Agents) != 1 {
		t.Fatalf("deny = %+v, %v", got, err)
	}
	if _, err := s.ApproveJoinRequest(ctx, denied.ID, "local-admin", c.t); !errors.Is(err, identity.ErrJoinState) {
		t.Fatalf("approve after deny = %v", err)
	}
	if _, err := s.ApproveJoinRequest(ctx, "nope", "local-admin", c.t); !errors.Is(err, identity.ErrJoinNotFound) {
		t.Fatalf("approve of an unknown id = %v", err)
	}
}

// A final row keeps its status but no names or code, and no longer holds
// its names.
func TestFinalJoinRequestIsSlim(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	r := mustFileJoin(t, s, c.t, "nNEW", "codex")
	if held, err := s.JoinNameHeld(ctx, "codex", "", c.t); err != nil || !held {
		t.Fatalf("held while pending = %v, %v", held, err)
	}
	if held, _ := s.JoinNameHeld(ctx, "codex", r.ID, c.t); held {
		t.Fatal("a request's own hold was not skipped")
	}
	if _, err := s.VoidJoinRequest(ctx, r.ID, c.t); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.JoinRequest(ctx, r.ID, c.t)
	if got.Status != identity.JoinVoided || len(got.Agents) != 0 || got.Code != "" || !got.Closed.Equal(c.t) {
		t.Fatalf("final row = %+v", got)
	}
	if held, _ := s.JoinNameHeld(ctx, "codex", "", c.t); held {
		t.Fatal("a final request still holds its name")
	}
}

// The sweep expires pending and approved requests ten minutes after filing,
// returning them with their names, and deletes final rows an hour later.
func TestJoinRequestSweep(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	pending := mustFileJoin(t, s, c.t, "n1", "a1")
	approved := mustFileJoin(t, s, c.t, "n2", "a2")
	if _, err := s.ApproveJoinRequest(ctx, approved.ID, "local-admin", c.t); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SweepJoinRequests(ctx, c.t.Add(identity.JoinRequestTTL-time.Second)); err != nil || len(got) != 0 {
		t.Fatalf("early sweep = %+v, %v", got, err)
	}
	at := c.t.Add(identity.JoinRequestTTL)
	if held, _ := s.JoinNameHeld(ctx, "a1", "", at); held {
		t.Fatal("an expired request holds its name before the sweep")
	}
	got, err := s.SweepJoinRequests(ctx, at)
	if err != nil || len(got) != 2 || got[0].ID != pending.ID || got[0].Status != identity.JoinExpired || got[0].Agents[0].Name != "a1" || got[1].ID != approved.ID {
		t.Fatalf("sweep = %+v, %v", got, err)
	}
	row, ok, _ := s.JoinRequest(ctx, pending.ID, at)
	if !ok || row.Status != identity.JoinExpired || len(row.Agents) != 0 || row.Code != "" {
		t.Fatalf("expired row = %+v, %v", row, ok)
	}
	if got, _ := s.SweepJoinRequests(ctx, at.Add(identity.JoinFinalKeep-time.Second)); len(got) != 0 {
		t.Fatalf("second sweep expired %+v again", got)
	}
	if _, ok, _ := s.JoinRequest(ctx, pending.ID, at.Add(identity.JoinFinalKeep-time.Second)); !ok {
		t.Fatal("a final row was deleted early")
	}
	if _, err := s.SweepJoinRequests(ctx, at.Add(identity.JoinFinalKeep)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.JoinRequest(ctx, pending.ID, at.Add(identity.JoinFinalKeep)); ok {
		t.Fatal("a final row outlived the hour")
	}
}

// The claim writes every agent row and the delivered status together, or
// nothing at all.
func TestJoinRequestClaimIsAllOrNothing(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	r := mustFileJoin(t, s, c.t, joinNode.ID, "codex", "helper")
	if _, err := s.ClaimJoinRequest(ctx, r.ID, joinNode, c.t); !errors.Is(err, identity.ErrJoinState) {
		t.Fatalf("claim while pending = %v", err)
	}
	if _, err := s.ApproveJoinRequest(ctx, r.ID, "local-admin", c.t); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJoinRequest(ctx, r.ID, identity.Node{ID: joinNode.ID, Key: "nodekey:zz"}, c.t); !errors.Is(err, identity.ErrJoinWrongNode) {
		t.Fatalf("claim with another key = %v", err)
	}
	if err := s.PutAgent(ctx, identity.Agent{Name: "helper", NodeID: "nOTHER", NodeName: "other", JoinedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJoinRequest(ctx, r.ID, joinNode, c.t); err == nil {
		t.Fatal("claim over an existing agent succeeded")
	}
	if _, ok, _ := s.AgentByName(ctx, "codex"); ok {
		t.Fatal("a failed claim left codex bound")
	}
	if a, _, _ := s.AgentByName(ctx, "helper"); a.NodeID != "nOTHER" {
		t.Fatalf("a failed claim moved helper: %+v", a)
	}
	if got, _, _ := s.JoinRequest(ctx, r.ID, c.t); got.Status != identity.JoinApproved {
		t.Fatalf("status after a failed claim = %s", got.Status)
	}
	if _, err := s.DeleteAgent(ctx, "helper"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimJoinRequest(ctx, r.ID, joinNode, c.t)
	if err != nil || got.Status != identity.JoinDelivered || len(got.Agents) != 2 {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	agents, _ := s.AgentsByNode(ctx, joinNode.ID)
	if len(agents) != 2 || agents[0].Name != "codex" || agents[0].Kind != "codex" || agents[0].NodeUser != "owner@example.com" || agents[0].NodeName != "new-mac" {
		t.Fatalf("agents = %+v", agents)
	}
	if _, err := s.ClaimJoinRequest(ctx, r.ID, joinNode, c.t); !errors.Is(err, identity.ErrJoinState) {
		t.Fatalf("second claim = %v", err)
	}
}

// The directory runs file, approve and claim end to end on SQLite.
func TestJoinRequestDirectoryOnSQLite(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	dir := identity.NewDirectory(s, fakeWho{}, identity.Config{})
	r, err := dir.FileJoinRequest(ctx, joinNode, []identity.RequestedAgent{{Name: "codex", Kind: "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveJoinRequest(ctx, r.ID, "local-admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := dir.ClaimJoinRequest(ctx, r.ID, joinNode); err != nil || got.Status != identity.JoinDelivered {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	if a, ok, _ := dir.Agent(ctx, "codex"); !ok || a.NodeID != joinNode.ID {
		t.Fatalf("codex = %+v, %v", a, ok)
	}
}

// The live-invite lookup ignores expired invites.
func TestLiveInviteNamedOnSQLite(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	if err := s.PutInvite(ctx, identity.Invite{Code: digestRow, Name: "muse", Expires: c.t.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.LiveInviteNamed(ctx, "muse", c.t); err != nil || !ok {
		t.Fatalf("live invite = %v, %v", ok, err)
	}
	if ok, _ := s.LiveInviteNamed(ctx, "muse", c.t.Add(2*time.Minute)); ok {
		t.Fatal("an expired invite counted as live")
	}
}

// A relay database from before join requests opens, keeps its agents, and
// files requests.
func TestOldDatabaseMigratesJoinRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE agents (name TEXT PRIMARY KEY, node_id TEXT NOT NULL UNIQUE, node_name TEXT NOT NULL, joined_at INTEGER NOT NULL)`,
		`INSERT INTO agents VALUES ('grokbot', 'nGROK', 'grok-bot', 1790000000000)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()
	s, c := open(t, path)
	if agents, err := s.Agents(context.Background()); err != nil || len(agents) != 1 {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
	mustFileJoin(t, s, c.t, "nNEW", "codex")
	s.Close()
	s, _ = open(t, path)
	if pending, err := s.PendingJoinRequests(context.Background(), c.t); err != nil || len(pending) != 1 {
		t.Fatalf("pending after reopen = %+v, %v", pending, err)
	}
}
