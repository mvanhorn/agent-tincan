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
	got, err := s.UnseenRepliesFor(context.Background(), agent, ReplyScope{Session: session, AllLive: true})
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
		if n, err := s.CountUnseenRepliesFor(ctx, "hermes", ReplyScope{Session: tc.session, AllLive: true}); err != nil || n != len(tc.want) {
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

// Once the relay knows which sessions are still live, a reply to a session
// that is not (a closed window, or one whose MCP server restarted under a
// new id) opens to the others, while a live session's replies stay its own.
func TestUnseenRepliesOpenWhenSessionIsGone(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	a := askFrom(t, s, "hermes", "a", "muse", "from A")
	b := askFrom(t, s, "hermes", "b", "muse", "from B")
	gone := askFrom(t, s, "hermes", "gone", "muse", "from a closed window")
	for _, req := range []envelope.Request{a, b, gone} {
		reply(t, s, req, envelope.StatusAnswered, "done")
	}
	for _, tc := range []struct {
		name  string
		scope ReplyScope
		want  []string
	}{
		{"a with b live", ReplyScope{Session: "a", Live: []string{"b"}}, []string{a.ID, gone.ID}},
		{"a listed live itself", ReplyScope{Session: "a", Live: []string{"a", "b"}}, []string{a.ID, gone.ID}},
		{"b alone", ReplyScope{Session: "b"}, []string{a.ID, b.ID, gone.ID}},
		{"a keeps everyone", ReplyScope{Session: "a", Live: []string{"b"}, AllLive: true}, []string{a.ID}},
	} {
		got, err := s.UnseenRepliesFor(ctx, "hermes", tc.scope)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.Request.ID)
		}
		slices.Sort(ids)
		slices.Sort(tc.want)
		if !slices.Equal(ids, tc.want) {
			t.Errorf("%s: sees %v, want %v", tc.name, ids, tc.want)
		}
		if n, err := s.CountUnseenRepliesFor(ctx, "hermes", tc.scope); err != nil || n != len(tc.want) {
			t.Errorf("%s: counts %d, %v; want %d", tc.name, n, err, len(tc.want))
		}
	}
}
