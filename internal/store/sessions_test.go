package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func askFrom(t *testing.T, s *Store, from, session, to, body string) envelope.Request {
	t.Helper()
	req, err := s.Enqueue(context.Background(), envelope.Request{From: from, FromSession: session, To: to, Kind: envelope.KindAsk, Body: body, Hop: 1, Chain: []string{}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func unseenFor(t *testing.T, s *Store, agent, session string) []string {
	t.Helper()
	got, err := s.UnseenRepliesFor(context.Background(), agent, session)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.Request.ID)
	}
	return ids
}

// Two sessions of one agent each see only the replies to their own
// requests, plus replies to requests sent with no session. A caller with no
// session sees them all, as before sessions existed.
func TestUnseenRepliesBySession(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	a := askFrom(t, s, "hermes", "a", "muse", "from A")
	b := askFrom(t, s, "hermes", "b", "muse", "from B")
	old := askFrom(t, s, "hermes", "", "muse", "from an old client")
	for _, req := range []envelope.Request{a, b, old} {
		reply(t, s, req, envelope.StatusAnswered, "done")
	}
	for _, tc := range []struct {
		session string
		want    []string
	}{
		{"a", []string{a.ID, old.ID}},
		{"b", []string{b.ID, old.ID}},
		{"c", []string{old.ID}},
		{"", []string{a.ID, b.ID, old.ID}},
	} {
		got := unseenFor(t, s, "hermes", tc.session)
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("session %q sees %v, want %v", tc.session, got, tc.want)
		}
		if n, err := s.CountUnseenRepliesFor(ctx, "hermes", tc.session); err != nil || n != len(tc.want) {
			t.Errorf("session %q counts %d, %v; want %d", tc.session, n, err, len(tc.want))
		}
	}
	if n, err := s.CountUnseenReplies(ctx, "hermes"); err != nil || n != 3 {
		t.Fatalf("agent-wide count = %d, %v; want 3", n, err)
	}
}

// The open claim a session's ask is filed under is one made from that
// session or with no session; the agent-wide lookup is unchanged.
func TestOpenClaimForSession(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "grokbot", "hermes", "plan the trip")
	if _, err := s.ClaimLeasesFrom(ctx, req.ID, "hermes", "a", time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.OpenClaimFor(ctx, "hermes", "a"); err != nil || !ok || got.ID != req.ID {
		t.Fatalf("session a = %+v %v %v", got, ok, err)
	}
	if got, ok, err := s.OpenClaimFor(ctx, "hermes", "b"); err != nil || ok {
		t.Fatalf("session b found another session's claim: %+v %v %v", got, ok, err)
	}
	if got, ok, err := s.OpenClaim(ctx, "hermes"); err != nil || !ok || got.ID != req.ID {
		t.Fatalf("agent-wide = %+v %v %v", got, ok, err)
	}
	// Claiming again from another session keeps the first claim's session.
	if _, err := s.ClaimLeasesFrom(ctx, req.ID, "hermes", "b", time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.OpenClaimFor(ctx, "hermes", "b"); ok {
		t.Fatal("a re-claim moved the claim to session b")
	}
	// A claim made with no session (tincan inbox) counts for every session.
	other := ask(t, s, "grokbot", "muse", "check the calendar")
	if _, err := s.Claim(ctx, other.ID, "muse", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.OpenClaimFor(ctx, "muse", "z"); err != nil || !ok || got.ID != other.ID {
		t.Fatalf("sessionless claim from session z = %+v %v %v", got, ok, err)
	}
}

// A requests table from before sessions gains both columns on open, and
// its rows read as sessionless: every session still sees their replies.
func TestSessionColumnsMigrateOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	req := ask(t, s, "hermes", "muse", "before the upgrade")
	for _, col := range []string{"from_session", "claim_session"} {
		if _, err := s.db.Exec(`ALTER TABLE requests DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, _ = open(t, path)
	reply(t, s, req, envelope.StatusAnswered, "done")
	if got := unseenFor(t, s, "hermes", "a"); !slices.Equal(got, []string{req.ID}) {
		t.Fatalf("session a after migration sees %v, want [%s]", got, req.ID)
	}
}
