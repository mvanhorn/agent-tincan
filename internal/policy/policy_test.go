package policy

import (
	"context"
	"errors"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

type fixture struct {
	st  *store.Store
	pol *Policy
	now time.Time
}

func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{st: st, now: time.Unix(1_790_000_000, 0)}
	cfg.Now = func() time.Time { return f.now }
	f.pol = New(st, cfg)
	return f
}

// send prepares and enqueues a request the way the relay does.
func (f *fixture) send(t *testing.T, from, to, parent string) (envelope.Request, error) {
	t.Helper()
	req := envelope.Request{From: from, To: to, Kind: envelope.KindAsk, Body: "x", ParentID: parent}
	if err := f.pol.Prepare(context.Background(), &req); err != nil {
		return req, err
	}
	return f.st.Enqueue(context.Background(), req, time.Hour)
}

func (f *fixture) claim(t *testing.T, id, agent string) {
	t.Helper()
	if _, err := f.st.Claim(context.Background(), id, agent, time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestNewChain(t *testing.T) {
	f := newFixture(t, Config{})
	req, err := f.send(t, "instinct", "muse", "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Hop != 1 || len(req.Chain) != 1 || req.Chain[0] != "instinct" || req.TraceID != req.ID {
		t.Fatalf("new chain = %+v", req)
	}
}

// AE3: Instinct asks Muse, Muse (holding that claim) tells Grok Bot.
func TestChainContinuesThroughParent(t *testing.T) {
	f := newFixture(t, Config{})
	first, _ := f.send(t, "instinct", "muse", "")
	f.claim(t, first.ID, "muse")
	second, err := f.send(t, "muse", "grokbot", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Hop != 2 || second.TraceID != first.TraceID || len(second.Chain) != 2 || second.Chain[0] != "instinct" || second.Chain[1] != "muse" {
		t.Fatalf("continued chain = %+v", second)
	}
}

// A sender that forgets the parent still continues the chain it is handling.
func TestOpenClaimUsedWhenParentMissing(t *testing.T) {
	f := newFixture(t, Config{})
	first, _ := f.send(t, "grokbot", "instinct", "")
	f.claim(t, first.ID, "instinct")
	second, _ := f.send(t, "instinct", "muse", first.ID)
	f.claim(t, second.ID, "muse")
	third, err := f.send(t, "muse", "claude-code", "")
	if err != nil {
		t.Fatal(err)
	}
	if third.Hop != 3 || third.TraceID != first.TraceID || third.ParentID != second.ID {
		t.Fatalf("implicit parent = %+v", third)
	}
}

// AE4: a request back to an agent already in the chain is a cycle.
func TestCycleRejectedButNewChainAllowed(t *testing.T) {
	f := newFixture(t, Config{})
	first, _ := f.send(t, "grokbot", "instinct", "")
	f.claim(t, first.ID, "instinct")
	second, _ := f.send(t, "instinct", "muse", first.ID)
	f.claim(t, second.ID, "muse")
	if _, err := f.send(t, "muse", "grokbot", second.ID); !errors.Is(err, ErrCycle) {
		t.Fatalf("want ErrCycle, got %v", err)
	}
	// Muse finishes its claim, so a fresh request is a new chain.
	f.st.Reply(context.Background(), second.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "ok"})
	fresh, err := f.send(t, "muse", "grokbot", "")
	if err != nil || fresh.Hop != 1 {
		t.Fatalf("new chain from muse = %+v, %v", fresh, err)
	}
}

func TestHopLimit(t *testing.T) {
	f := newFixture(t, Config{HopLimit: 4})
	agents := []string{"a1", "a2", "a3", "a4", "a5", "a6"}
	parent := ""
	for i := range 4 {
		req, err := f.send(t, agents[i], agents[i+1], parent)
		if err != nil {
			t.Fatalf("hop %d rejected: %v", i+1, err)
		}
		f.claim(t, req.ID, agents[i+1])
		parent = req.ID
	}
	if _, err := f.send(t, agents[4], agents[5], parent); !errors.Is(err, ErrHopLimit) {
		t.Fatalf("hop 5: want ErrHopLimit, got %v", err)
	}
}

func TestParentMustBeHeldBySender(t *testing.T) {
	f := newFixture(t, Config{})
	first, _ := f.send(t, "grokbot", "instinct", "")
	if _, err := f.send(t, "muse", "claude-code", first.ID); !errors.Is(err, ErrBadParent) {
		t.Fatalf("parent addressed to someone else: want ErrBadParent, got %v", err)
	}
	if _, err := f.send(t, "muse", "claude-code", "nope"); !errors.Is(err, ErrBadParent) {
		t.Fatalf("unknown parent: want ErrBadParent, got %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t, Config{PerMinute: 3})
	for i := range 3 {
		if _, err := f.send(t, "muse", "grokbot", ""); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if _, err := f.send(t, "muse", "grokbot", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if _, err := f.send(t, "instinct", "grokbot", ""); err != nil {
		t.Fatalf("other senders are not limited: %v", err)
	}
	f.now = f.now.Add(61 * time.Second)
	if _, err := f.send(t, "muse", "grokbot", ""); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// An agent handling two requests at once has no single chain to continue, so
// a parentless ask starts a new chain instead of guessing (and must not be
// rejected as a cycle against the chain it guessed).
func TestTwoOpenClaimsStartNewChain(t *testing.T) {
	f := newFixture(t, Config{})
	a, _ := f.send(t, "grokbot", "muse", "")
	f.claim(t, a.ID, "muse")
	b, _ := f.send(t, "instinct", "muse", "")
	f.claim(t, b.ID, "muse")
	// Guessing either chain is wrong; guessing b's would also reject this as a cycle.
	req, err := f.send(t, "muse", "instinct", "")
	if err != nil {
		t.Fatalf("parentless ask with two open claims: %v", err)
	}
	if req.Hop != 1 || req.ParentID != "" || req.TraceID != req.ID {
		t.Fatalf("want a new chain, got %+v", req)
	}
}

// A notify the agent claimed long ago never closes, so it must not become the
// implicit parent of the agent's later asks.
func TestClaimedNotifyIsNotImplicitParent(t *testing.T) {
	f := newFixture(t, Config{})
	n := envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindNotify, Body: "fyi"}
	if err := f.pol.Prepare(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	n, err := f.st.Enqueue(context.Background(), n, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.claim(t, n.ID, "muse")
	req, err := f.send(t, "muse", "grokbot", "")
	if err != nil {
		t.Fatalf("ask after a claimed notify: %v", err)
	}
	if req.Hop != 1 || req.ParentID != "" {
		t.Fatalf("stale notify became the parent: %+v", req)
	}
}

func TestUrgentLimit(t *testing.T) {
	f := newFixture(t, Config{})
	send := func(from string, urgent bool) error {
		return f.pol.Prepare(t.Context(), &envelope.Request{From: from, To: "target", Urgent: urgent})
	}
	for range 5 {
		if err := send("sender", true); err != nil {
			t.Fatal(err)
		}
	}
	if err := send("sender", true); !errors.Is(err, ErrUrgentLimited) {
		t.Fatalf("limit = %v", err)
	} else if status, ok := errors.AsType[*relay.StatusError](err); !ok || status.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %v", err)
	}
	if err := send("sender", false); err != nil {
		t.Fatal(err)
	}
	if err := send("other", true); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if err := send("sender", true); err != nil {
		t.Fatal(err)
	}
}

func TestUrgentConfiguredLimit(t *testing.T) {
	f := newFixture(t, Config{UrgentPerHour: 1})
	req := envelope.Request{From: "sender", To: "target", Urgent: true}
	if err := f.pol.Prepare(t.Context(), &req); err != nil {
		t.Fatal(err)
	}
	if err := f.pol.Prepare(t.Context(), &req); !errors.Is(err, ErrUrgentLimited) {
		t.Fatalf("limit = %v", err)
	}
}

func TestRefundReturnsUrgentSlot(t *testing.T) {
	f := newFixture(t, Config{UrgentPerHour: 1})
	req := envelope.Request{From: "sender", To: "target", Urgent: true}
	if err := f.pol.Prepare(t.Context(), &req); err != nil {
		t.Fatal(err)
	}
	f.pol.Refund(req)
	if err := f.pol.Prepare(t.Context(), &req); err != nil {
		t.Fatalf("refunded slot still spent: %v", err)
	}
	f.pol.Refund(envelope.Request{From: "sender", To: "target"})
	if err := f.pol.Prepare(t.Context(), &req); !errors.Is(err, ErrUrgentLimited) {
		t.Fatalf("a non-urgent refund gave back an urgent slot: %v", err)
	}
}

func TestRefundGivesBackTheFailedSendsOwnSlot(t *testing.T) {
	f := newFixture(t, Config{UrgentPerHour: 2})
	failed := envelope.Request{From: "sender", To: "target", Urgent: true}
	if err := f.pol.Prepare(t.Context(), &failed); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(30 * time.Minute)
	ok := envelope.Request{From: "sender", To: "target", Urgent: true}
	if err := f.pol.Prepare(t.Context(), &ok); err != nil {
		t.Fatal(err)
	}
	// The older send fails; its slot, not the newer one, comes back.
	f.pol.Refund(failed)
	f.now = f.now.Add(45 * time.Minute) // past the refunded slot's hour, inside the kept one's
	if err := f.pol.Prepare(t.Context(), &envelope.Request{From: "sender", To: "target", Urgent: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.pol.Prepare(t.Context(), &envelope.Request{From: "sender", To: "target", Urgent: true}); !errors.Is(err, ErrUrgentLimited) {
		t.Fatalf("limit after refund = %v; the kept send's slot was lost", err)
	}
}

func TestPingRejectsInferredParentAndUsesRateLimit(t *testing.T) {
	f := newFixture(t, Config{PerMinute: 1})
	parent, err := f.send(t, "instinct", "muse", "")
	if err != nil {
		t.Fatal(err)
	}
	f.claim(t, parent.ID, "muse")
	req := envelope.Request{From: "muse", To: "grokbot", Kind: envelope.KindPing}
	if err := f.pol.Prepare(t.Context(), &req); err == nil {
		t.Fatal("ping continued inferred parent")
	}
	req = envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindPing}
	if err := f.pol.Prepare(t.Context(), &req); err != nil {
		t.Fatal(err)
	}
	if err := f.pol.Prepare(t.Context(), &req); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate: %v", err)
	}
}

// prepareTo runs Prepare for one ask from grokbot to target and returns the
// prepared request, with the policy metadata the store never returns.
func (f *fixture) prepareTo(t *testing.T, target string) envelope.Request {
	t.Helper()
	req := envelope.Request{From: "grokbot", To: target, Kind: envelope.KindAsk, Body: "x"}
	if err := f.pol.Prepare(t.Context(), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func (f *fixture) agent(t *testing.T, name, kind string) {
	t.Helper()
	if err := f.st.PutAgent(t.Context(), identity.Agent{Name: name, NodeID: "n-" + name, Kind: kind}); err != nil {
		t.Fatal(err)
	}
}

func writeApproval(t *testing.T, body string) *Approval {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// AE5: an agent's ask to a council-kind agent waits for the owner even with
// no approval.json, and for long enough to be approved.
func TestCouncilHeldByDefaultWithoutApproval(t *testing.T) {
	f := newFixture(t, Config{})
	f.agent(t, "council", "council")
	req := f.prepareTo(t, "council")
	if req.Status != envelope.StatusHeld || req.HoldTTL != 2*time.Hour || req.ApprovalNotify != "" {
		t.Fatalf("council ask = status %q ttl %v notify %q; want held for 2h with no notice", req.Status, req.HoldTTL, req.ApprovalNotify)
	}
}

// AE5: an agent's ask to the pass teammate waits for the owner even with
// no approval.json, because approving it sends one of the owner's chats to
// another vendor.
func TestPassHeldByDefaultWithoutApproval(t *testing.T) {
	f := newFixture(t, Config{})
	f.agent(t, "pass", "pass")
	req := f.prepareTo(t, "pass")
	if req.Status != envelope.StatusHeld || req.HoldTTL != 2*time.Hour {
		t.Fatalf("pass ask = status %q ttl %v; want held for 2h", req.Status, req.HoldTTL)
	}
}

// An approval.json that gates other agents but names no council still holds
// council asks, with the file's hold TTL and notify target.
func TestCouncilHeldWhenApprovalHasNoEntry(t *testing.T) {
	a := writeApproval(t, `{"gate":{"muse":{"from":"*"}},"notify":"instinct","hold_ttl":"30m"}`)
	f := newFixture(t, Config{Approval: a})
	f.agent(t, "council", "council")
	req := f.prepareTo(t, "council")
	if req.Status != envelope.StatusHeld || req.HoldTTL != 30*time.Minute || req.ApprovalNotify != "instinct" {
		t.Fatalf("council ask = status %q ttl %v notify %q; want held 30m, notify instinct", req.Status, req.HoldTTL, req.ApprovalNotify)
	}
}

// An explicit entry wins: {"from": []} holds nothing.
func TestCouncilExplicitEmptyEntryDelivers(t *testing.T) {
	a := writeApproval(t, `{"gate":{"council":{"from":[]}}}`)
	f := newFixture(t, Config{Approval: a})
	f.agent(t, "council", "council")
	if req := f.prepareTo(t, "council"); req.Status != "" {
		t.Fatalf("council ask with explicit empty entry = %q, want delivered", req.Status)
	}
}

// A failed kind lookup holds rather than delivers.
func TestCouncilKindLookupErrorHolds(t *testing.T) {
	f := newFixture(t, Config{})
	f.agent(t, "muse", "codex")
	f.pol.kindOf = func(context.Context, string) (string, error) { return "", errors.New("disk gone") }
	req := f.prepareTo(t, "muse")
	if req.Status != envelope.StatusHeld || req.HoldTTL <= 0 {
		t.Fatalf("lookup failure = status %q ttl %v; want held", req.Status, req.HoldTTL)
	}
}

// Other kinds are unchanged: no approval.json means no hold.
func TestNonCouncilNotHeldWithoutApproval(t *testing.T) {
	f := newFixture(t, Config{})
	f.agent(t, "muse", "codex")
	if req := f.prepareTo(t, "muse"); req.Status != "" || req.HoldTTL != 0 {
		t.Fatalf("codex ask = status %q ttl %v, want delivered", req.Status, req.HoldTTL)
	}
}

// A move runs the send checks against the new target without charging the
// sender's rate: a target already in the chain, past the hop limit, gated
// by approval.json or held by default for its kind is refused; any other
// passes, even for a sender at its rate limit.
func TestCheckMove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"grok-web":{"from":"*"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	approval, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, Config{Approval: approval, PerMinute: 1})
	ctx := context.Background()
	for _, a := range []identity.Agent{{Name: "council", NodeID: "n1", Kind: "council"}, {Name: "gemini-web", NodeID: "n2", Kind: "gemini-web"}} {
		if err := f.st.PutAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	req, err := f.send(t, "codex", "claude-web", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(t, "codex", "muse", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the rate limit is not reached: %v", err)
	}
	if err := f.pol.CheckMove(ctx, req, "gemini-web"); err != nil {
		t.Fatalf("open target refused: %v", err)
	}
	for to, want := range map[string]int{"codex": http.StatusConflict, "grok-web": http.StatusConflict, "council": http.StatusConflict} {
		err := f.pol.CheckMove(ctx, req, to)
		var se *relay.StatusError
		if !errors.As(err, &se) || se.Code != want {
			t.Errorf("%s: %v", to, err)
		}
	}
	long := req
	long.Hop = DefaultHopLimit + 1
	if err := f.pol.CheckMove(ctx, long, "gemini-web"); !errors.Is(err, ErrHopLimit) {
		t.Fatalf("hop limit: %v", err)
	}
}

// NoParent, which only the relay sets, starts a new chain whatever claims the
// sender holds, for an agent-wide and a session-scoped send alike.
func TestNoParentStartsNewChain(t *testing.T) {
	f := newFixture(t, Config{})
	ctx := context.Background()
	asked, err := f.send(t, "instinct", "grokbot", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Deliver(ctx, "grokbot", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Claim(ctx, asked.ID, "grokbot", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"", "other"} {
		req := envelope.Request{From: "grokbot", To: "instinct", Kind: envelope.KindAsk, Body: "x", NoParent: true, FromSession: session}
		if err := f.pol.Prepare(ctx, &req); err != nil {
			t.Fatalf("session %q: %v", session, err)
		}
		if req.ParentID != "" || req.Hop != 1 || len(req.Chain) != 1 || req.Chain[0] != "grokbot" {
			t.Fatalf("session %q: %+v", session, req)
		}
	}
	req := envelope.Request{From: "grokbot", To: "instinct", Kind: envelope.KindAsk, Body: "x"}
	if err := f.pol.Prepare(ctx, &req); !errors.Is(err, ErrCycle) {
		t.Fatalf("without NoParent: %v", err)
	}
}
