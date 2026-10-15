package policy

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// sendFrom is send from a session.
func (f *fixture) sendFrom(t *testing.T, from, session, to string) (envelope.Request, error) {
	t.Helper()
	req := envelope.Request{From: from, FromSession: session, To: to, Kind: envelope.KindAsk, Body: "x"}
	if err := f.pol.Prepare(context.Background(), &req); err != nil {
		return req, err
	}
	return f.st.Enqueue(context.Background(), req, time.Hour)
}

// claimFrom claims id for agent from session.
func (f *fixture) claimFrom(t *testing.T, id, agent, session string) {
	t.Helper()
	if _, err := f.st.ClaimLeasesFrom(context.Background(), id, agent, session, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
}

// An ask with no parent is filed under the sending session's own claim and
// never under another session's, but it keeps the agent-wide chain for the
// cycle and hop checks, so a session label never loosens them.
func TestSessionFilesAskButKeepsChecks(t *testing.T) {
	f := newFixture(t, Config{})
	task, err := f.send(t, "grokbot", "instinct", "")
	if err != nil {
		t.Fatal(err)
	}
	f.claimFrom(t, task.ID, "instinct", "a")

	other, err := f.sendFrom(t, "instinct", "b", "muse")
	if err != nil {
		t.Fatal(err)
	}
	if other.ParentID != "" || other.TraceID == task.TraceID {
		t.Fatalf("session b's ask was filed under a's task: parent %q trace %q", other.ParentID, other.TraceID)
	}
	if other.Hop != 2 || !slices.Equal(other.Chain, []string{"grokbot", "instinct"}) {
		t.Fatalf("session b's ask lost the chain checks: hop %d chain %v", other.Hop, other.Chain)
	}
	// Its trace starts at hop 2 with no parent; it is still a trace root.
	traces, err := f.st.RecentTraces(context.Background(), 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(traces, func(st store.TraceStep) bool { return st.Request.ID == other.ID }) {
		t.Fatalf("recent traces leave out session b's own trace %s", other.TraceID)
	}
	if _, err := f.sendFrom(t, "instinct", "made-up", "grokbot"); !errors.Is(err, ErrCycle) {
		t.Fatalf("made-up session asking back to grokbot: %v, want a cycle", err)
	}

	own, err := f.sendFrom(t, "instinct", "a", "muse")
	if err != nil || own.ParentID != task.ID || own.TraceID != task.TraceID || own.Hop != 2 {
		t.Fatalf("session a's ask = parent %q trace %q hop %d (%v), want %s %s 2", own.ParentID, own.TraceID, own.Hop, err, task.ID, task.TraceID)
	}
	plain, err := f.sendFrom(t, "instinct", "", "muse")
	if err != nil || plain.ParentID != task.ID {
		t.Fatalf("sessionless ask parent %q (%v), want %s as before sessions", plain.ParentID, err, task.ID)
	}
}

// The hop limit counts the agent-wide chain even when the session files the
// ask on its own.
func TestSessionCannotResetHopLimit(t *testing.T) {
	f := newFixture(t, Config{HopLimit: 2})
	task, err := f.send(t, "grokbot", "instinct", "")
	if err != nil {
		t.Fatal(err)
	}
	f.claimFrom(t, task.ID, "instinct", "a")
	next, err := f.sendFrom(t, "instinct", "a", "muse")
	if err != nil {
		t.Fatal(err)
	}
	f.claimFrom(t, next.ID, "muse", "x")
	if _, err := f.sendFrom(t, "muse", "fresh", "hermes"); !errors.Is(err, ErrHopLimit) {
		t.Fatalf("fresh session past the hop limit: %v, want ErrHopLimit", err)
	}
}

// A claim taken with no session (tincan inbox or listen) still files a
// session's ask under it, so those chains are never broken.
func TestSessionlessClaimFilesSessionAsk(t *testing.T) {
	f := newFixture(t, Config{})
	task, err := f.send(t, "grokbot", "instinct", "")
	if err != nil {
		t.Fatal(err)
	}
	f.claim(t, task.ID, "instinct")
	got, err := f.sendFrom(t, "instinct", "a", "muse")
	if err != nil || got.ParentID != task.ID {
		t.Fatalf("parent %q (%v), want %s", got.ParentID, err, task.ID)
	}
}
