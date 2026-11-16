package policy

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// carrying stores an ask from from to to whose body carries the text of
// contributors, as a task's ask does, and has to claim it from session.
func (f *fixture) carrying(t *testing.T, from, to, session string, contributors ...string) envelope.Request {
	t.Helper()
	req := envelope.Request{From: from, To: to, Kind: envelope.KindAsk, Body: "x", Contributors: contributors}
	if err := f.pol.Prepare(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	req, err := f.st.Enqueue(context.Background(), req, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.claimFrom(t, req.ID, to, session)
	return req
}

// The contributors of a request follow every ask sent while handling it,
// through a named parent, an inferred one and any number of hops, so a rule
// that holds a contributor's asks to a target holds each of them. They are
// stored with each ask, which is what a later move reads. An ask with no
// such contributor behind it is decided as before.
func TestContributorsFollowAsksSentWhileHandling(t *testing.T) {
	f := newFixture(t, Config{Approval: writeApproval(t, `{"gate":{"muse":{"from":["scout"]}}}`)})
	ctx := context.Background()
	first := f.carrying(t, "grokbot", "instinct", "", "scout", envelope.TaskOwner, "scout", "")
	if !slices.Equal(first.Contributors, []string{"scout"}) {
		t.Fatalf("contributors as prepared = %v, want scout once and no owner", first.Contributors)
	}

	direct, err := f.send(t, "instinct", "muse", first.ID)
	if err != nil || direct.Status != envelope.StatusHeld || !slices.Equal(direct.Contributors, []string{"scout"}) {
		t.Fatalf("ask to the gated target under a named parent = %+v, %v", direct, err)
	}
	if got, _, err := f.st.Request(ctx, direct.ID); err != nil || !slices.Equal(got.Contributors, []string{"scout"}) {
		t.Fatalf("stored child = %+v, %v", got, err)
	}
	inferred, err := f.send(t, "instinct", "muse", "")
	if err != nil || inferred.Status != envelope.StatusHeld || inferred.ParentID != first.ID {
		t.Fatalf("ask to the gated target under an inferred parent = %+v, %v", inferred, err)
	}

	// Two hops on, through an agent the rule does not name.
	second, err := f.send(t, "instinct", "hermes", first.ID)
	if err != nil || second.Status == envelope.StatusHeld || !slices.Equal(second.Contributors, []string{"scout"}) {
		t.Fatalf("ask to an ungated target = %+v, %v", second, err)
	}
	f.claim(t, second.ID, "hermes")
	third, err := f.send(t, "hermes", "muse", second.ID)
	if err != nil || third.Status != envelope.StatusHeld || !slices.Equal(third.Contributors, []string{"scout"}) {
		t.Fatalf("ask to the gated target two hops on = %+v, %v", third, err)
	}
	// A move of the ungated ask to the gated target is refused for them too.
	if err := f.pol.CheckMove(ctx, second, "muse"); !errors.Is(err, ErrMoveHeld) {
		t.Fatalf("move to the gated target: %v, want %v", err, ErrMoveHeld)
	}

	// A follow-on's parent is the request it follows.
	followed, _, err := f.st.Request(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	then := envelope.Request{From: followed.From, To: "muse", Kind: envelope.KindAsk, Body: "x", FollowOf: followed.ID}
	if err := f.pol.Prepare(ctx, &then); err != nil || then.Status != envelope.StatusHeld || !slices.Equal(then.Contributors, []string{"scout"}) {
		t.Fatalf("follow-on to the gated target = %+v, %v", then, err)
	}
}

// With nobody the rule names anywhere behind it, the same shape is not held.
func TestContributorsOfUngatedAgentsHoldNothing(t *testing.T) {
	f := newFixture(t, Config{Approval: writeApproval(t, `{"gate":{"muse":{"from":["scout"]}}}`)})
	first := f.carrying(t, "grokbot", "instinct", "", "codex")
	second, err := f.send(t, "instinct", "hermes", first.ID)
	if err != nil || second.Status == envelope.StatusHeld {
		t.Fatalf("ask to an ungated target = %+v, %v", second, err)
	}
	f.claim(t, second.ID, "hermes")
	third, err := f.send(t, "hermes", "muse", second.ID)
	if err != nil || third.Status == envelope.StatusHeld || !slices.Equal(third.Contributors, []string{"codex"}) {
		t.Fatalf("ask to the gated target with no gated agent behind it = %+v, %v", third, err)
	}
	plain, err := f.send(t, "codex", "muse", "")
	if err != nil || plain.Status == envelope.StatusHeld || plain.Contributors != nil {
		t.Fatalf("plain ask = %+v, %v", plain, err)
	}
}

// A session's ask that is filed on its own, outside another session's
// claim, still carries that claim's contributors, as it keeps its chain.
func TestSessionGuardKeepsContributors(t *testing.T) {
	f := newFixture(t, Config{Approval: writeApproval(t, `{"gate":{"muse":{"from":["scout"]}}}`)})
	f.carrying(t, "grokbot", "instinct", "a", "scout")
	other, err := f.sendFrom(t, "instinct", "b", "muse")
	if err != nil || other.ParentID != "" {
		t.Fatalf("session b's ask = %+v, %v; want it filed on its own", other, err)
	}
	if other.Status != envelope.StatusHeld || !slices.Equal(other.Contributors, []string{"scout"}) {
		t.Fatalf("session b's ask shed the claim's contributors: %+v", other)
	}
}

// Contributors are no part of the chain: an ask to one of them is no cycle,
// they add no hop, and a move to one is no loop.
func TestContributorsLeaveLoopAndHopChecksAlone(t *testing.T) {
	f := newFixture(t, Config{HopLimit: 2})
	ctx := context.Background()
	first := f.carrying(t, "grokbot", "instinct", "", "muse", "scout", "codex", "hermes")
	child, err := f.send(t, "instinct", "muse", first.ID)
	if err != nil {
		t.Fatalf("ask to a contributor of the parent: %v", err)
	}
	if child.Hop != 2 || !slices.Equal(child.Chain, []string{"grokbot", "instinct"}) {
		t.Fatalf("child = hop %d chain %v, want hop 2 and the chain without contributors", child.Hop, child.Chain)
	}
	if err := f.pol.CheckMove(ctx, child, "scout"); err != nil {
		t.Fatalf("move to a contributor: %v", err)
	}
	// The checks themselves are as they were.
	if _, err := f.send(t, "instinct", "grokbot", first.ID); !errors.Is(err, ErrCycle) {
		t.Fatalf("ask back to the chain: %v, want a cycle", err)
	}
	f.claim(t, child.ID, "muse")
	if _, err := f.send(t, "muse", "scout", child.ID); !errors.Is(err, ErrHopLimit) {
		t.Fatalf("third hop: %v, want the hop limit", err)
	}
}
