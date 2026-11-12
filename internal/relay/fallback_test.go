package relay

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// fallbackHarness is untakenHarness with the queued recorder kept, and
// muse's fallback set to scout (strangerAddr).
func fallbackHarness(t *testing.T) (*harness, *fakeClock, *fakeWaker, *queuedRecorder) {
	t.Helper()
	h, clk, fw, rec := noticeHarness(t)
	h.st.SetClock(clk.Now)
	fw.methods["muse"] = "channel"
	setFallback(h, "muse", "scout", http.StatusOK)
	return h, clk, fw, rec
}

func setFallback(h *harness, agent, to string, want int) {
	h.t.Helper()
	h.do(macAddr, "PUT", "/v1/agents/"+agent+"/fallback", `{"fallback":"`+to+`"}`, want, nil)
}

// rerouteNotices polls addr and returns the rerouted notices it was
// delivered.
func rerouteNotices(t *testing.T, h *harness, addr string) []envelope.Request {
	t.Helper()
	var out []envelope.Request
	for _, r := range inbox(t, h, addr) {
		if r.From == "relay" && strings.Contains(r.Body, "so the relay moved it to") {
			out = append(out, r)
		}
	}
	return out
}

// auditCount counts audit events named event whose detail contains part.
func auditMatches(t *testing.T, st *store.Store, event, part string) int {
	t.Helper()
	events, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Event == event && strings.Contains(e.Detail, part) {
			n++
		}
	}
	return n
}

// An ask muse leaves untaken moves to its fallback, scout, after a full
// grace and not before: same id, trace and body, the asker told once that
// it moved and that nothing had worked on it, scout woken like a fresh
// send, and scout's answer back to the original asker on the same request.
// Later sweeps move and tell nothing more.
func TestFallbackMovesUntakenAskOnce(t *testing.T) {
	h, clk, _, rec := fallbackHarness(t)
	ctx := context.Background()
	ask := h.send(instinctAddr, "muse", "review the deploy plan")

	clk.advance(5 * time.Minute)
	pollAll(t, h, strangerAddr)
	h.srv.Sweep(ctx)
	if res := getAs(h, instinctAddr, ask.ID); res.Request.To != "muse" {
		t.Fatalf("moved before a full grace: %+v", res.Request)
	}

	clk.advance(DefaultWakeGrace)
	pollAll(t, h, strangerAddr)
	h.srv.Sweep(ctx)
	res := getAs(h, instinctAddr, ask.ID)
	if res.Request.To != "scout" || res.Status != envelope.StatusQueued || !slices.Equal(res.Request.MovedFrom, []string{"muse"}) ||
		res.Request.TraceID != ask.TraceID || res.Request.Body != "review the deploy plan" {
		t.Fatalf("after grace = %s %+v", res.Status, res.Request)
	}
	if !slices.Contains(rec.to, "scout") {
		t.Fatalf("scout was not woken: %v", rec.to)
	}
	got := rerouteNotices(t, h, instinctAddr)
	if len(got) != 1 {
		t.Fatalf("asker notices = %+v, want one", got)
	}
	for _, want := range []string{
		"muse had not taken this request in 15m, so the relay moved it to scout",
		"Nothing had worked on it before the move.",
		"It keeps its id, and scout's reply comes back on this request.",
		ask.ID,
	} {
		if !strings.Contains(got[0].Body, want) {
			t.Errorf("notice lacks %q: %s", want, got[0].Body)
		}
	}
	if untaken := untakenNotices(t, h, instinctAddr); len(untaken) != 0 {
		t.Fatalf("also told untaken: %+v", untaken)
	}
	if n := auditMatches(t, h.st, "rerouted", `"to":"scout"`); n != 1 {
		t.Fatalf("rerouted audit rows = %d", n)
	}

	clk.advance(2 * DefaultWakeGrace)
	pollAll(t, h, museAddr)
	h.srv.Sweep(ctx)
	h.srv.Sweep(ctx)
	for _, r := range rerouteNotices(t, h, instinctAddr) {
		if r.ID != got[0].ID {
			t.Fatalf("told again: %s", r.Body)
		}
	}
	if n := auditMatches(t, h.st, "rerouted", ""); n != 1 {
		t.Fatalf("rerouted audit rows after more sweeps = %d", n)
	}

	in := inbox(t, h, strangerAddr)
	if !slices.ContainsFunc(in, func(r envelope.Request) bool { return r.ID == ask.ID && r.From == "instinct" }) {
		t.Fatalf("scout inbox = %+v", in)
	}
	h.do(museAddr, "POST", "/v1/requests/"+ask.ID+"/claim", "", http.StatusNotFound, nil)
	h.do(strangerAddr, "POST", "/v1/requests/"+ask.ID+"/claim", "", http.StatusOK, nil)
	h.do(strangerAddr, "POST", "/v1/requests/"+ask.ID+"/reply", `{"body":"ship it"}`, http.StatusOK, nil)
	res = getAs(h, instinctAddr, ask.ID)
	if res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.Body != "ship it" || res.Reply.From != "scout" {
		t.Fatalf("asker sees %s %+v", res.Status, res.Reply)
	}
}

// With no fallback, or with the fallback cleared, the ask stays and the
// asker gets the plain untaken note.
func TestFallbackNotSetGivesUntakenNote(t *testing.T) {
	h, clk, _, _ := fallbackHarness(t)
	setFallback(h, "muse", "", http.StatusOK)
	ask := h.send(instinctAddr, "muse", "x")
	clk.advance(2 * DefaultWakeGrace)
	pollAll(t, h, strangerAddr)
	h.srv.Sweep(context.Background())
	if res := getAs(h, instinctAddr, ask.ID); res.Request.To != "muse" {
		t.Fatalf("moved with no fallback: %+v", res.Request)
	}
	if got := untakenNotices(t, h, instinctAddr); len(got) != 1 {
		t.Fatalf("untaken notices = %+v", got)
	}
	if n := auditMatches(t, h.st, "reroute_skipped", ""); n != 0 {
		t.Fatalf("skip audited with no fallback set: %d", n)
	}
}

// An offline fallback takes nothing: the ask stays with muse, the asker
// gets the untaken note, and the skip is audited with why. The untaken
// note marks the ask, so a later sweep with scout online does not move it.
func TestFallbackOfflineGivesUntakenNote(t *testing.T) {
	h, clk, _, _ := fallbackHarness(t)
	ctx := context.Background()
	ask := h.send(instinctAddr, "muse", "x")
	clk.advance(2 * DefaultWakeGrace)
	h.srv.Sweep(ctx)
	if res := getAs(h, instinctAddr, ask.ID); res.Request.To != "muse" {
		t.Fatalf("moved to an offline fallback: %+v", res.Request)
	}
	if got := untakenNotices(t, h, instinctAddr); len(got) != 1 {
		t.Fatalf("untaken notices = %+v", got)
	}
	if n := auditMatches(t, h.st, "reroute_skipped", "the fallback is offline"); n != 1 {
		t.Fatalf("reroute_skipped rows = %d", n)
	}
	clk.advance(DefaultWakeGrace)
	pollAll(t, h, strangerAddr)
	h.srv.Sweep(ctx)
	if res := getAs(h, instinctAddr, ask.ID); res.Request.To != "muse" {
		t.Fatalf("moved after the untaken note: %+v", res.Request)
	}
}

// One hop only. With muse and scout each the other's fallback, an ask moved
// to scout that scout also leaves untaken stays with scout, and the asker
// gets scout's untaken note: no ping-pong, no chain.
func TestFallbackNoSecondHop(t *testing.T) {
	h, clk, fw, _ := fallbackHarness(t)
	fw.methods["scout"] = "channel"
	setFallback(h, "scout", "muse", http.StatusOK)
	ctx := context.Background()
	ask := h.send(instinctAddr, "muse", "x")
	clk.advance(2 * DefaultWakeGrace)
	pollAll(t, h, strangerAddr)
	h.srv.Sweep(ctx)
	if res := getAs(h, instinctAddr, ask.ID); res.Request.To != "scout" {
		t.Fatalf("first hop: %+v", res.Request)
	}
	// Inside scout's own grace from the move: nothing.
	clk.advance(DefaultWakeGrace / 2)
	pollAll(t, h, museAddr)
	h.srv.Sweep(ctx)
	if got := untakenNotices(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("scout's untaken note before its own grace: %+v", got)
	}
	clk.advance(DefaultWakeGrace)
	pollAll(t, h, museAddr)
	h.srv.Sweep(ctx)
	res := getAs(h, instinctAddr, ask.ID)
	if res.Request.To != "scout" || !slices.Equal(res.Request.MovedFrom, []string{"muse"}) {
		t.Fatalf("second hop: %+v", res.Request)
	}
	got := untakenNotices(t, h, instinctAddr)
	if len(got) != 1 || !strings.Contains(got[0].Body, "scout has not taken this request") {
		t.Fatalf("untaken notices = %+v", got)
	}
	if n := auditMatches(t, h.st, "reroute_skipped", "it moved once already"); n != 1 {
		t.Fatalf("reroute_skipped rows = %d", n)
	}
}

// A move the approval policy would hold is refused: the ask stays with
// muse and the asker gets the untaken note.
func TestFallbackRespectsMoveChecks(t *testing.T) {
	h, clk, _, _ := fallbackHarness(t)
	h.srv.SetPreparer(refusingPreparer{refuse: "scout"})
	ask := h.send(instinctAddr, "muse", "x")
	clk.advance(2 * DefaultWakeGrace)
	pollAll(t, h, strangerAddr)
	h.srv.Sweep(context.Background())
	if res := getAs(h, instinctAddr, ask.ID); res.Request.To != "muse" {
		t.Fatalf("moved past the gate: %+v", res.Request)
	}
	if got := untakenNotices(t, h, instinctAddr); len(got) != 1 {
		t.Fatalf("untaken notices = %+v", got)
	}
	if n := auditMatches(t, h.st, "reroute_skipped", "the move checks refused it"); n != 1 {
		t.Fatalf("reroute_skipped rows = %d", n)
	}
}

// Asks the fallback never takes: one from the fallback itself (it would
// loop), a grouped one and an ask held for approval.
func TestFallbackSkipsAskerGroupAndHeld(t *testing.T) {
	h, clk, _, _ := fallbackHarness(t)
	ctx := context.Background()
	fromScout := h.send(strangerAddr, "muse", "from the fallback itself")
	var grouped envelope.Request
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"muse","body":"fan out","group":"g1"}`, http.StatusCreated, &grouped)
	clk.advance(2 * DefaultWakeGrace)
	pollAll(t, h, strangerAddr)
	h.srv.Sweep(ctx)
	if res := getAs(h, strangerAddr, fromScout.ID); res.Request.To != "muse" {
		t.Fatalf("moved to its own asker: %+v", res.Request)
	}
	if res := getAs(h, instinctAddr, grouped.ID); res.Request.To != "muse" {
		t.Fatalf("grouped ask moved: %+v", res.Request)
	}

	h.srv.SetPreparer(holdingPreparer{})
	held := h.send(instinctAddr, "muse", "held")
	clk.advance(2 * DefaultWakeGrace)
	pollAll(t, h, strangerAddr)
	h.srv.Sweep(ctx)
	if res := getAs(h, instinctAddr, held.ID); res.Request.To != "muse" || res.Status != envelope.StatusHeld {
		t.Fatalf("held ask = %s %+v", res.Status, res.Request)
	}
}

// Only an admin sets a fallback, and only to another joined teammate that
// can take asks. The roster shows it to teammates.
func TestFallbackConfig(t *testing.T) {
	h, _, _, _ := fallbackHarness(t)
	h.do(instinctAddr, "PUT", "/v1/agents/muse/fallback", `{"fallback":"grokbot"}`, http.StatusForbidden, nil)
	h.do(macAddr, "PUT", "/v1/agents/muse/fallback", `{}`, http.StatusBadRequest, nil)
	setFallback(h, "muse", "muse", http.StatusBadRequest)
	setFallback(h, "muse", "nobody", http.StatusNotFound)
	setFallback(h, "nobody", "muse", http.StatusNotFound)
	h.do(macAddr, "PUT", "/v1/agents/grokbot/kind", `{"kind":"phone"}`, http.StatusOK, nil)
	setFallback(h, "muse", "grokbot", http.StatusBadRequest)

	if a := agentInfo(t, h, instinctAddr, "muse"); a.Fallback != "scout" || a.FallbackField() != "fallback=scout" {
		t.Fatalf("roster entry = %+v", a)
	}
	if a := agentInfo(t, h, macAddr, "scout"); a.Fallback != "" {
		t.Fatalf("scout has a fallback: %+v", a)
	}
	if n := auditMatches(t, h.st, "fallback", `"fallback":"scout"`); n != 1 {
		t.Fatalf("fallback audit rows = %d", n)
	}
	setFallback(h, "muse", "", http.StatusOK)
	if a := agentInfo(t, h, instinctAddr, "muse"); a.Fallback != "" {
		t.Fatalf("after clear = %+v", a)
	}
}
