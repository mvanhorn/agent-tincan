package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

func approvalFile(t *testing.T, body string) *Approval {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approval.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func prepare(t *testing.T, f *fixture, from, to, parent string, kind envelope.Kind) envelope.Request {
	t.Helper()
	req := envelope.Request{From: from, To: to, Kind: kind, Body: "x", ParentID: parent}
	if err := f.pol.Prepare(t.Context(), &req); err != nil {
		t.Fatalf("%s -> %s: %v", from, to, err)
	}
	return req
}

// Requests to a dot are held for the owner with no config at all: the
// default TTL and no notify agent, exactly what a gated target gets from an
// approval.json without hold_ttl or notify.
func TestDotHeldByDefaultWithoutConfig(t *testing.T) {
	for _, a := range []*Approval{nil, approvalFile(t, "")} {
		f := newFixture(t, Config{Approval: a})
		f.agent(t, "dot-web", onboard.KindDotWeb)
		for _, kind := range []envelope.Kind{envelope.KindAsk, envelope.KindNotify} {
			req := prepare(t, f, "muse", "dot-web", "", kind)
			if req.Status != envelope.StatusHeld || req.HoldTTL != 2*time.Hour || req.ApprovalNotify != "" {
				t.Fatalf("%s to dot (approval %v) = status %q ttl %v notify %q", kind, a != nil, req.Status, req.HoldTTL, req.ApprovalNotify)
			}
		}
		ping := prepare(t, f, "muse", "dot-web", "", envelope.KindPing)
		if ping.Status == envelope.StatusHeld {
			t.Fatal("a ping to a dot was held")
		}
	}
}

// With an approval.json that gates other targets, a dot's default hold uses
// its hold_ttl and notify, the same as a gated target.
func TestDotDefaultHoldUsesPolicyTTLAndNotify(t *testing.T) {
	f := newFixture(t, Config{Approval: approvalFile(t, `{"gate":{"muse":{"from":"*"}},"hold_ttl":"3h","notify":"instinct"}`)})
	f.agent(t, "dot-web", onboard.KindDotWeb)
	req := prepare(t, f, "grokbot", "dot-web", "", envelope.KindAsk)
	if req.Status != envelope.StatusHeld || req.HoldTTL != 3*time.Hour || req.ApprovalNotify != "instinct" {
		t.Fatalf("dot hold = %q %v %q", req.Status, req.HoldTTL, req.ApprovalNotify)
	}
}

// An approval.json entry for the dot replaces the default: a sender it
// allows goes straight through, one it gates is held.
func TestDotApprovalEntryOverridesDefault(t *testing.T) {
	f := newFixture(t, Config{Approval: approvalFile(t, `{"gate":{"dot-web":{"unless":["muse"]}}}`)})
	f.agent(t, "dot-web", onboard.KindDotWeb)
	if req := prepare(t, f, "muse", "dot-web", "", envelope.KindAsk); req.Status == envelope.StatusHeld {
		t.Fatal("an allowed sender was held")
	}
	if req := prepare(t, f, "grokbot", "dot-web", "", envelope.KindAsk); req.Status != envelope.StatusHeld {
		t.Fatal("a sender the entry gates was not held")
	}
	// An entry that gates nobody also opens the dot to everyone.
	g := newFixture(t, Config{Approval: approvalFile(t, `{"gate":{"dot-web":{"from":[]}}}`)})
	g.agent(t, "dot-web", onboard.KindDotWeb)
	if req := prepare(t, g, "grokbot", "dot-web", "", envelope.KindAsk); req.Status == envelope.StatusHeld {
		t.Fatal("an empty from entry did not open the dot")
	}
}

// Other kinds, and agents with no kind, are not held without an entry. That
// includes every other web agent: only dot-web fronts an agent that acts in
// the owner's apps.
func TestOtherKindsNotHeldByDefault(t *testing.T) {
	for _, a := range []*Approval{nil, approvalFile(t, "")} {
		f := newFixture(t, Config{Approval: a})
		f.agent(t, "cx", onboard.KindCodex)
		f.agent(t, "fo", onboard.KindScheduled)
		targets := []string{"cx", "fo", "muse"}
		for _, kind := range []string{onboard.KindChatGPTWeb, onboard.KindClaudeWeb, onboard.KindGrokWeb, onboard.KindGeminiWeb, onboard.KindPerplexityWeb, onboard.KindCopilotWeb} {
			f.agent(t, kind, kind)
			targets = append(targets, kind)
		}
		for _, to := range targets {
			if req := prepare(t, f, "grokbot", to, "", envelope.KindAsk); req.Status == envelope.StatusHeld {
				t.Errorf("request to %s held", to)
			}
		}
	}
}

// The dot's own asks are not held by default. An owner who wants them held
// gates the targets with from: ["dot-web"], which matches anywhere in the chain.
func TestGateFromDotHoldsAnywhereInChain(t *testing.T) {
	f := newFixture(t, Config{Approval: approvalFile(t, `{"gate":{"cx":{"from":["dot-web"]}}}`)})
	f.agent(t, "dot-web", onboard.KindDotWeb)
	f.agent(t, "cx", onboard.KindCodex)
	own := prepare(t, f, "dot-web", "muse", "", envelope.KindAsk)
	if own.Status == envelope.StatusHeld {
		t.Fatal("the dot's own ask to an ungated agent was held")
	}
	if req := prepare(t, f, "dot-web", "cx", "", envelope.KindAsk); req.Status != envelope.StatusHeld {
		t.Fatal("dot -> cx not held")
	}
	first, err := f.st.Enqueue(t.Context(), own, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.claim(t, first.ID, "muse")
	req := prepare(t, f, "muse", "cx", first.ID, envelope.KindAsk)
	if req.Status != envelope.StatusHeld || len(req.Chain) != 2 || req.Chain[0] != "dot-web" {
		t.Fatalf("dot -> muse -> cx = %q %v", req.Status, req.Chain)
	}
}

// convened returns the id of a convening ask from the owner's agent to the
// council, released by the owner when approved, and claimed by the council.
func convened(t *testing.T, f *fixture, approved bool) string {
	t.Helper()
	req := prepare(t, f, "instinct", "council", "", envelope.KindAsk)
	stored, err := f.st.Enqueue(t.Context(), req, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if approved {
		if stored.Status != envelope.StatusHeld {
			t.Fatalf("convening ask to the council = %q, want held", stored.Status)
		}
		if _, err := f.st.Release(t.Context(), stored.ID, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	f.claim(t, stored.ID, "council")
	return stored.ID
}

func councilFixture(t *testing.T, a *Approval) *fixture {
	t.Helper()
	f := newFixture(t, Config{Approval: a})
	f.agent(t, "council", onboard.KindCouncil)
	f.agent(t, "dot-web", onboard.KindDotWeb)
	return f
}

// A council's member asks to the dot continue a council question the owner
// already approved, so the dot's default hold does not hold them again.
func TestCouncilAskToDotPassesWhenParentApproved(t *testing.T) {
	for _, a := range []*Approval{nil, approvalFile(t, "")} {
		f := councilFixture(t, a)
		parent := convened(t, f, true)
		for _, kind := range []envelope.Kind{envelope.KindAsk, envelope.KindNotify} {
			if req := prepare(t, f, "council", "dot-web", parent, kind); req.Status == envelope.StatusHeld {
				t.Fatalf("council %s to the dot under an approved council ask was held (approval %v)", kind, a != nil)
			}
		}
		// The claimed parent is found without naming it, too.
		if req := prepare(t, f, "council", "dot-web", "", envelope.KindAsk); req.Status == envelope.StatusHeld {
			t.Fatal("council ask to the dot under its claimed, approved request was held")
		}
	}
}

// A parent the owner never approved does not open the dot.
func TestCouncilAskToDotHeldWhenParentUnapproved(t *testing.T) {
	// An entry that gates nobody lets the convening ask through unheld.
	f := councilFixture(t, approvalFile(t, `{"gate":{"council":{"from":[]}}}`))
	parent := convened(t, f, false)
	if req := prepare(t, f, "council", "dot-web", parent, envelope.KindAsk); req.Status != envelope.StatusHeld {
		t.Fatal("council ask to the dot under an unapproved parent was not held")
	}
}

// Only a council's asks pass: an agent handling an approved request that
// did not go to a council is still held, and so is a fresh ask.
func TestNonCouncilAskToDotStillHeld(t *testing.T) {
	f := councilFixture(t, nil)
	f.agent(t, "cx", onboard.KindCodex)
	// An approved request to a non-council agent (gated by nothing but
	// released the same way) does not open the dot for its handler.
	g := councilFixture(t, approvalFile(t, `{"gate":{"cx":{"from":"*"}}}`))
	g.agent(t, "cx", onboard.KindCodex)
	req := prepare(t, g, "instinct", "cx", "", envelope.KindAsk)
	stored, err := g.st.Enqueue(t.Context(), req, time.Hour)
	if err != nil || stored.Status != envelope.StatusHeld {
		t.Fatalf("gated ask to cx = %q %v", stored.Status, err)
	}
	if _, err := g.st.Release(t.Context(), stored.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	g.claim(t, stored.ID, "cx")
	if r := prepare(t, g, "cx", "dot-web", stored.ID, envelope.KindAsk); r.Status != envelope.StatusHeld {
		t.Fatal("ask to the dot under an approved non-council request was not held")
	}
	for _, from := range []string{"grokbot", "cx", "council"} {
		if r := prepare(t, f, from, "dot-web", "", envelope.KindAsk); r.Status != envelope.StatusHeld {
			t.Errorf("fresh ask from %s to the dot was not held", from)
		}
	}
	// The convening ask to the council itself stays held.
	if r := prepare(t, f, "instinct", "council", "", envelope.KindAsk); r.Status != envelope.StatusHeld {
		t.Error("convening ask to the council was not held")
	}
}

// An approval.json entry that gates the dot from the council still holds
// the council's asks, even under an approved council question.
func TestApprovalGateHoldsCouncilAskToDot(t *testing.T) {
	f := councilFixture(t, approvalFile(t, `{"gate":{"dot-web":{"from":["council"]}}}`))
	parent := convened(t, f, true)
	if req := prepare(t, f, "council", "dot-web", parent, envelope.KindAsk); req.Status != envelope.StatusHeld {
		t.Fatal("an approval.json gate on the dot from the council did not hold the council's ask")
	}
}

// Only the dot's hold is lifted: an approved council asking another
// council-kind agent is still held for that council's own approval.
func TestApprovedCouncilAskToAnotherCouncilHeld(t *testing.T) {
	f := councilFixture(t, nil)
	f.agent(t, "council2", onboard.KindCouncil)
	parent := convened(t, f, true)
	if req := prepare(t, f, "council", "council2", parent, envelope.KindAsk); req.Status != envelope.StatusHeld {
		t.Fatal("an approved council's ask to another council was not held")
	}
}

// A Duet channel's agent is held like a dot: by default with no config,
// and not again for a council question the owner approved.
func TestDuetHeldByDefaultLikeADot(t *testing.T) {
	f := councilFixture(t, nil)
	f.agent(t, "duet-web", onboard.KindDuetWeb)
	if req := prepare(t, f, "muse", "duet-web", "", envelope.KindAsk); req.Status != envelope.StatusHeld {
		t.Fatalf("ask to duet-web = %q", req.Status)
	}
	parent := convened(t, f, true)
	if req := prepare(t, f, "council", "duet-web", parent, envelope.KindAsk); req.Status == envelope.StatusHeld {
		t.Fatal("council ask to duet-web under an approved council ask was held")
	}
}
