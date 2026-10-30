package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// In these tests grokbot is claude-web (claude.ai) and muse is gemini-web
// (gemini.google.com), both online; instinct asks.
func failoverHarness(t *testing.T) (*harness, *fakeClock) {
	t.Helper()
	h, clk, _ := statusHarness(t)
	for name, kind := range map[string]string{"grokbot": "claude-web", "muse": "gemini-web"} {
		if ok, err := h.st.SetAgentKind(t.Context(), name, kind); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	listen(h, grokAddr, museAddr)
	clearLimit(h, clk, grokAddr)
	clearLimit(h, clk, museAddr)
	return h, clk
}

// listen marks the agents at addrs online with a peek, which delivers
// nothing.
func listen(h *harness, addrs ...string) {
	h.t.Helper()
	for _, addr := range addrs {
		req := httptest.NewRequest("GET", "/v1/poll?hold=0&peek=1", nil)
		req.RemoteAddr = addr
		h.h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

var webSite = map[string]string{grokAddr: "claude.ai", museAddr: "gemini.google.com"}

// limitAt reports, as the agent at addr, that its site is out of messages
// for d, and returns the reset time as the relay stores it.
func limitAt(h *harness, clk *fakeClock, addr string, d time.Duration) time.Time {
	h.t.Helper()
	until := clk.Now().Add(d).UTC().Truncate(time.Millisecond)
	body := fmt.Sprintf(`{"site":%q,"state":"limited","until":%q,"observed_at":%q}`, webSite[addr], until.Format(time.RFC3339Nano), clk.Now().Format(time.RFC3339Nano))
	h.do(addr, "PUT", "/v1/agents/self/web-status", body, http.StatusNoContent, nil)
	clk.advance(time.Millisecond)
	return until
}

// clearLimit reports, as the agent at addr, that it is signed in again.
func clearLimit(h *harness, clk *fakeClock, addr string) {
	h.t.Helper()
	body := fmt.Sprintf(`{"site":%q,"state":"authenticated","observed_at":%q}`, webSite[addr], clk.Now().Format(time.RFC3339Nano))
	h.do(addr, "PUT", "/v1/agents/self/web-status", body, http.StatusNoContent, nil)
	clk.advance(time.Millisecond)
}

func handback(h *harness, addr, id string, until time.Time, want int) client.HandbackResult {
	h.t.Helper()
	var out client.HandbackResult
	var dst any
	if want == http.StatusOK {
		dst = &out
	}
	h.do(addr, "POST", "/v1/requests/"+id+"/handback", fmt.Sprintf(`{"reason":"limited","until":%q}`, until.Format(time.RFC3339Nano)), want, dst)
	return out
}

func getAs(h *harness, addr, id string) store.Result {
	h.t.Helper()
	var res store.Result
	h.do(addr, "GET", "/v1/requests/"+id, "", http.StatusOK, &res)
	return res
}

func auditEvents(h *harness, event string) []store.AuditEvent {
	h.t.Helper()
	all, err := h.st.AuditEvents(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.DeleteFunc(all, func(e store.AuditEvent) bool { return e.Event != event })
}

// AE1: an ask to limited claude-web moves to gemini-web, keeps its id,
// sender and chain, and the asker sees one note naming who took it.
func TestFailoverMovesQueuedAsk(t *testing.T) {
	h, clk := failoverHarness(t)
	until := limitAt(h, clk, grokAddr, 90*time.Minute)
	var sent envelope.SendResponse
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"grokbot","body":"summarize this paper"}`, http.StatusCreated, &sent)
	if sent.To != "muse" || !slices.Equal(sent.MovedFrom, []string{"grokbot"}) {
		t.Fatalf("send response = %+v", sent.Request)
	}
	res := getAs(h, instinctAddr, sent.ID)
	if res.Request.To != "muse" || res.Request.From != "instinct" || !slices.Equal(res.Request.Chain, []string{"instinct"}) || res.Status != envelope.StatusQueued {
		t.Fatalf("request = %+v %s", res.Request, res.Status)
	}
	want := "grokbot is out of messages until " + client.UTCClock(until, clk.Now()) + "; muse took your request"
	if res.RelayNote == nil || !strings.Contains(res.RelayNote.Note, want) || !strings.Contains(res.RelayNote.Note, "without any earlier conversation") {
		t.Fatalf("note = %+v", res.RelayNote)
	}
	if n := relayNotices(h, "instinct"); len(n) != 1 || !strings.Contains(n[0].Body, want) {
		t.Fatalf("notices = %+v", n)
	}
	if m := auditEvents(h, "moved"); len(m) != 1 || m[0].RequestID != sent.ID || !strings.Contains(m[0].Detail, `"from":"grokbot"`) || !strings.Contains(m[0].Detail, `"to":"muse"`) {
		t.Fatalf("moved audit = %+v", m)
	}
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	var got pollResult
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("muse got %+v", got)
	}
}

// AE3: a claimed ask handed back unsent moves; gemini-web's reply reaches
// the asker on the same id.
func TestFailoverHandbackMoves(t *testing.T) {
	h, clk := failoverHarness(t)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	until := limitAt(h, clk, grokAddr, time.Hour)
	out := handback(h, grokAddr, sent.ID, until, http.StatusOK)
	if out.Outcome != client.HandbackMoved || out.To != "muse" {
		t.Fatalf("handback = %+v", out)
	}
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"too late"}`, http.StatusNotFound, nil)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"the summary"}`, http.StatusOK, nil)
	res := getAs(h, instinctAddr, sent.ID)
	if res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.From != "muse" || res.Reply.Body != "the summary" {
		t.Fatalf("asker sees %+v", res)
	}
	if hb := auditEvents(h, "handed_back"); len(hb) != 1 || !strings.Contains(hb[0].Detail, `"outcome":"moved"`) {
		t.Fatalf("handed_back audit = %+v", hb)
	}
}

// AE2: a council member ask does not move; its handback gets 409 and it is
// still the limited agent's to answer, which fails it.
func TestFailoverCouncilAskStays(t *testing.T) {
	h, clk := failoverHarness(t)
	if ok, err := h.st.SetAgentKind(t.Context(), "instinct", "council"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	until := limitAt(h, clk, grokAddr, time.Hour)
	sent := h.send(instinctAddr, "grokbot", "a council question")
	if sent.To != "grokbot" {
		t.Fatalf("council ask moved: %+v", sent)
	}
	h.srv.Sweep(t.Context())
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	handback(h, grokAddr, sent.ID, until, http.StatusConflict)
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"out of messages","status":"failed"}`, http.StatusOK, nil)
	if res := getAs(h, instinctAddr, sent.ID); res.Status != envelope.StatusFailed || res.Reply.From != "grokbot" {
		t.Fatalf("asker sees %+v", res)
	}
}

// Asks that were held, are notifies, are in a group, came from a room
// mention, carry attachments, name a conversation, have a clarification
// exchange, may have been sent already, or involve dot-web, a council or
// a non-web target never move.
func TestFailoverIneligible(t *testing.T) {
	ask := envelope.Request{Kind: envelope.KindAsk, Body: "summarize this paper"}
	with := func(f func(*envelope.Request)) envelope.Request { r := ask; f(&r); return r }
	if why := failoverIneligible(ask, false, "codex", "claude-web"); why != "" {
		t.Fatalf("plain ask: %s", why)
	}
	for name, tc := range map[string]struct {
		req      envelope.Request
		claimed  bool
		from, to string
	}{
		"held":           {req: with(func(r *envelope.Request) { r.WasHeld, r.Approved = true, true })},
		"notify":         {req: with(func(r *envelope.Request) { r.Kind = envelope.KindNotify })},
		"group":          {req: with(func(r *envelope.Request) { r.Group = "g1" })},
		"room mention":   {req: with(func(r *envelope.Request) { r.RoomPostID = 7 })},
		"attachments":    {req: with(func(r *envelope.Request) { r.Attachments = []envelope.Attachment{{ID: "a"}} })},
		"conversation":   {req: with(func(r *envelope.Request) { r.Body = "  Conversation: abc123\nand more" })},
		"exchange":       {req: with(func(r *envelope.Request) { r.Exchanges = []envelope.Exchange{{Question: "which?"}} })},
		"resumed":        {req: with(func(r *envelope.Request) { r.Resumed = true })},
		"maybe sent":     {req: ask, claimed: true},
		"to dot-web":     {req: ask, to: "dot-web"},
		"from dot-web":   {req: ask, from: "dot-web"},
		"to duet-web":    {req: ask, to: "duet-web"},
		"from duet-web":  {req: ask, from: "duet-web"},
		"from council":   {req: ask, from: "council"},
		"to a CLI":       {req: ask, to: "codex"},
		"to copilot-cli": {req: ask, to: "copilot-cli"},
		"to cursor-cli":  {req: ask, to: "cursor-cli"},
		"to no kind":     {req: ask, to: "-"},
		"to the council": {req: ask, to: "council"},
	} {
		from, to := tc.from, tc.to
		if from == "" {
			from = "codex"
		}
		switch to {
		case "":
			to = "claude-web"
		case "-":
			to = ""
		}
		if failoverIneligible(tc.req, tc.claimed, from, to) == "" {
			t.Errorf("%s: would move", name)
		}
	}
}

// When no teammate can take it, the ask stays, is held back until the
// reset, and the asker is told it waits; the limited agent is not handed
// it before then.
func TestFailoverWaitsWithNoTarget(t *testing.T) {
	h, clk := failoverHarness(t)
	limitAt(h, clk, museAddr, 2*time.Hour)
	until := limitAt(h, clk, grokAddr, 30*time.Minute)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	res := getAs(h, instinctAddr, sent.ID)
	if res.Request.To != "grokbot" || res.RelayNote == nil || !strings.Contains(res.RelayNote.Note, "it waits") {
		t.Fatalf("asker sees %+v %+v", res.Request, res.RelayNote)
	}
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 1 {
		t.Fatalf("notices = %+v", n)
	}
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	clk.advance(until.Sub(clk.Now()) + time.Second)
	var got pollResult
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("after the reset grokbot got %+v", got)
	}
}

// A held-back ask moves on a later sweep once a teammate is free.
func TestFailoverSweepMovesLater(t *testing.T) {
	h, clk := failoverHarness(t)
	limitAt(h, clk, museAddr, time.Hour)
	limitAt(h, clk, grokAddr, time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	clearLimit(h, clk, museAddr)
	h.srv.Sweep(t.Context())
	if res := getAs(h, instinctAddr, sent.ID); res.Request.To != "muse" {
		t.Fatalf("not moved by the sweep: %+v", res.Request)
	}
	if n := relayNotices(h, "instinct"); len(n) != 2 {
		t.Fatalf("notices = %+v", n)
	}
}

// An ask moved from claude-web to gemini-web never moves back.
func TestFailoverNeverMovesBack(t *testing.T) {
	h, clk := failoverHarness(t)
	limitAt(h, clk, grokAddr, time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	clearLimit(h, clk, grokAddr)
	limitAt(h, clk, museAddr, time.Hour)
	listen(h, grokAddr, museAddr)
	h.srv.Sweep(t.Context())
	if res := getAs(h, instinctAddr, sent.ID); res.Request.To != "muse" {
		t.Fatalf("moved back: %+v", res.Request)
	}
}

// refusingPreparer refuses every move to refuse, as an approval.json gate
// on it would.
type refusingPreparer struct{ refuse string }

func (refusingPreparer) Prepare(ctx context.Context, req *envelope.Request) error {
	return newChain{}.Prepare(ctx, req)
}

func (p refusingPreparer) CheckMove(_ context.Context, _ envelope.Request, to string) error {
	if to == p.refuse {
		return errors.New("gated")
	}
	return nil
}

// A move the policy refuses (a target gated by approval.json) does not
// happen; the ask stays.
func TestFailoverRefusedMoveStays(t *testing.T) {
	h, clk := failoverHarness(t)
	h.srv.SetPreparer(refusingPreparer{refuse: "muse"})
	limitAt(h, clk, grokAddr, time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	if res := getAs(h, instinctAddr, sent.ID); res.Request.To != "grokbot" {
		t.Fatalf("moved to a gated target: %+v", res.Request)
	}
}

// A target listed in failover.json is never chosen.
func TestFailoverExcludedTarget(t *testing.T) {
	h, clk := failoverHarness(t)
	path := filepath.Join(t.TempDir(), FailoverFile)
	if err := os.WriteFile(path, []byte(`{"exclude":["muse"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.FailoverPath = path
	limitAt(h, clk, grokAddr, time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	if res := getAs(h, instinctAddr, sent.ID); res.Request.To != "grokbot" {
		t.Fatalf("moved to an excluded target: %+v", res.Request)
	}
	// A bad file fails closed: nothing moves while it is broken.
	if err := os.WriteFile(path, []byte(`{"exclude":"muse"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.srv.Sweep(t.Context())
	if res := getAs(h, instinctAddr, sent.ID); res.Request.To != "grokbot" {
		t.Fatalf("moved while failover.json is invalid: %+v", res.Request)
	}
}

// An invalid failover.json (an entry that is not an agent name, or a
// top-level null) moves no ask: a new one to a limited agent is held back
// on it instead.
func TestFailoverInvalidFileMovesNothing(t *testing.T) {
	for name, content := range map[string]string{
		"bad name": `{"exclude":["not a name!"]}`,
		"null":     `null`,
	} {
		t.Run(name, func(t *testing.T) {
			h, clk := failoverHarness(t)
			path := filepath.Join(t.TempDir(), FailoverFile)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			h.srv.cfg.FailoverPath = path
			limitAt(h, clk, grokAddr, time.Hour)
			sent := h.send(instinctAddr, "grokbot", "summarize this paper")
			h.srv.Sweep(t.Context())
			res := getAs(h, instinctAddr, sent.ID)
			if res.Request.To != "grokbot" || res.RelayNote == nil || !strings.Contains(res.RelayNote.Note, "it waits") {
				t.Fatalf("asker sees %+v %+v", res.Request, res.RelayNote)
			}
			if m := auditEvents(h, "moved"); len(m) != 0 {
				t.Fatalf("moved audit = %+v", m)
			}
		})
	}
}

// A web teammate that has never reported a signed-in observation is not a
// failover target.
func TestFailoverSkipsUnobservedTarget(t *testing.T) {
	h, clk, _ := statusHarness(t)
	for name, kind := range map[string]string{"grokbot": "claude-web", "muse": "gemini-web"} {
		if ok, err := h.st.SetAgentKind(t.Context(), name, kind); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	listen(h, grokAddr, museAddr)
	limitAt(h, clk, grokAddr, time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	if sent.To != "grokbot" {
		t.Fatalf("moved to a teammate with no sign-in observation: %+v", sent)
	}
}

// A held-back ask is released as soon as its target reports it is signed
// in again, without waiting for the old reset time.
func TestFailoverHeldAskReleasedWhenLimitClears(t *testing.T) {
	h, clk := failoverHarness(t)
	limitAt(h, clk, museAddr, 2*time.Hour)
	limitAt(h, clk, grokAddr, time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	clearLimit(h, clk, grokAddr)
	h.srv.Sweep(t.Context())
	var got pollResult
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("after the limit cleared grokbot got %+v", got)
	}
}

// When the target reports an earlier reset, a held-back ask is released
// at the new time rather than the old one.
func TestFailoverHeldAskReleasedAtShorterLimit(t *testing.T) {
	h, clk := failoverHarness(t)
	limitAt(h, clk, museAddr, 3*time.Hour)
	limitAt(h, clk, grokAddr, 2*time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	shorter := limitAt(h, clk, grokAddr, 30*time.Minute)
	h.srv.Sweep(t.Context())
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	clk.advance(shorter.Sub(clk.Now()) + time.Second)
	var got pollResult
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("after the shorter reset grokbot got %+v", got)
	}
}

// A held-back ask is not reported as waiting by a peek and cannot be
// claimed by id before its time.
func TestHeldAskNotVisibleEarly(t *testing.T) {
	h, clk := failoverHarness(t)
	limitAt(h, clk, museAddr, 2*time.Hour)
	limitAt(h, clk, grokAddr, time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	h.do(grokAddr, "GET", "/v1/poll?hold=0&peek=1", "", http.StatusNoContent, nil)
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusConflict, nil)
	if n := h.srv.QueuedCount("grokbot"); n != 0 {
		t.Fatalf("queued count = %d", n)
	}
	if ids, err := h.srv.QueuedIDs("grokbot"); err != nil || len(ids) != 0 {
		t.Fatalf("queued ids = %v %v", ids, err)
	}
}

// An ask whose claim lapsed and was requeued may already have been typed
// into a site, so it stays on its target when the target is limited, even
// with a free teammate.
func TestFailoverLapsedClaimStays(t *testing.T) {
	h, clk := failoverHarness(t)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(DefaultClaimLease + time.Second)
	h.srv.Sweep(t.Context())
	listen(h, grokAddr, museAddr)
	limitAt(h, clk, grokAddr, time.Hour)
	h.srv.Sweep(t.Context())
	if res := getAs(h, instinctAddr, sent.ID); res.Request.To != "grokbot" {
		t.Fatalf("a lapsed claim moved: %+v", res.Request)
	}
	if m := auditEvents(h, "moved"); len(m) != 0 {
		t.Fatalf("moved audit = %+v", m)
	}
}

// After a move the old target cannot claim, reply to, ask a question on,
// post progress on or read the ask: each is not found.
func TestFailoverOldTargetLosesTheAsk(t *testing.T) {
	h, clk := failoverHarness(t)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	handback(h, grokAddr, sent.ID, limitAt(h, clk, grokAddr, time.Hour), http.StatusOK)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/claim", ""},
		{"POST", "/reply", `{"body":"x"}`},
		{"POST", "/reply", `{"body":"which paper?","status":"needs_input"}`},
		{"POST", "/progress", `{"note":"working"}`},
		{"GET", "", ""},
	} {
		h.do(grokAddr, c.method, "/v1/requests/"+sent.ID+c.path, c.body, http.StatusNotFound, nil)
	}
}

// Handback rules: only the claimant hands back, the reset time is capped
// at the limit the agent reported, and an agent that reported none gets
// 409.
func TestHandbackRules(t *testing.T) {
	h, clk := failoverHarness(t)
	limitAt(h, clk, museAddr, 3*time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	handback(h, grokAddr, sent.ID, clk.Now().Add(time.Hour), http.StatusConflict)
	handback(h, instinctAddr, sent.ID, clk.Now().Add(time.Hour), http.StatusNotFound)
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/handback", `{"reason":"tired","until":"`+clk.Now().Add(time.Hour).Format(time.RFC3339)+`"}`, http.StatusBadRequest, nil)
	reported := limitAt(h, clk, grokAddr, 20*time.Minute)
	out := handback(h, grokAddr, sent.ID, clk.Now().Add(2*time.Hour), http.StatusOK)
	if out.Outcome != client.HandbackRequeued || !out.Until.Equal(reported) {
		t.Fatalf("handback = %+v, want requeued until %v", out, reported)
	}
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	clk.advance(reported.Sub(clk.Now()) + time.Second)
	var got pollResult
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 {
		t.Fatalf("not redelivered after the capped reset: %+v", got)
	}
}

// queueRecorder records the requests the relay queued, for wakes.
type queueRecorder struct {
	mu  sync.Mutex
	got []envelope.Request
}

func (q *queueRecorder) Queued(_ context.Context, req envelope.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.got = append(q.got, req)
}

// A moved ask wakes its new target, not the limited one.
func TestFailoverWakesNewTarget(t *testing.T) {
	h, clk := failoverHarness(t)
	rec := &queueRecorder{}
	h.srv.SetEvents(rec)
	limitAt(h, clk, grokAddr, time.Hour)
	h.send(instinctAddr, "grokbot", "summarize this paper")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	// The asker's relay notice is queued too; the ask itself goes to muse
	// only.
	asks := slices.DeleteFunc(slices.Clone(rec.got), func(r envelope.Request) bool { return r.From == client.RelaySender })
	if len(asks) != 1 || asks[0].To != "muse" {
		t.Fatalf("woken for %+v", rec.got)
	}
}

// A watch on a limited web agent does not fire until the limit clears.
func TestWatchOnLimitedAgentWaits(t *testing.T) {
	h, clk := failoverHarness(t)
	until := limitAt(h, clk, grokAddr, 10*time.Minute)
	watch(h, instinctAddr, "grokbot", http.StatusCreated)
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 0 {
		t.Fatalf("fired while limited: %+v", n)
	}
	clk.advance(until.Sub(clk.Now()) + time.Second)
	listen(h, grokAddr)
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 1 || !strings.Contains(n[0].Body, "grokbot is idle now") {
		t.Fatalf("notices = %+v", n)
	}
}

// A relay that takes handbacks says so.
func TestCapabilitiesHandback(t *testing.T) {
	h, _ := failoverHarness(t)
	var caps client.Capabilities
	h.do(grokAddr, "GET", "/v1/capabilities", "", http.StatusOK, &caps)
	if !caps.Handback {
		t.Fatalf("caps = %+v", caps)
	}
}

// An ask whose earlier claim lapsed may already have been typed into a
// site, so a later claimant cannot hand it back to move: the handback gets
// 409 and the ask stays claimed with its target.
func TestFailoverReclaimedAskHandbackRefused(t *testing.T) {
	h, clk := failoverHarness(t)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(DefaultClaimLease + time.Second)
	h.srv.Sweep(t.Context())
	listen(h, grokAddr, museAddr)
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	until := limitAt(h, clk, grokAddr, time.Hour)
	handback(h, grokAddr, sent.ID, until, http.StatusConflict)
	if res := getAs(h, instinctAddr, sent.ID); res.Request.To != "grokbot" || res.Status != envelope.StatusClaimed {
		t.Fatalf("a reclaimed ask moved: %+v %s", res.Request, res.Status)
	}
	if m := auditEvents(h, "moved"); len(m) != 0 {
		t.Fatalf("moved audit = %+v", m)
	}
}

// joinWebAgent joins name at addr as a web chat agent of kind on site,
// online and signed in.
func joinWebAgent(h *harness, clk *fakeClock, name, addr, kind, site string) {
	h.t.Helper()
	h.who.Set(addr, identity.Node{ID: "n" + name, Name: name})
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"`+name+`"}`, http.StatusOK, &inv)
	h.do(addr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	if ok, err := h.st.SetAgentKind(h.t.Context(), name, kind); !ok || err != nil {
		h.t.Fatal(ok, err)
	}
	listen(h, addr)
	body := fmt.Sprintf(`{"site":%q,"state":"authenticated","observed_at":%q}`, site, clk.Now().Format(time.RFC3339Nano))
	h.do(addr, "PUT", "/v1/agents/self/web-status", body, http.StatusNoContent, nil)
	clk.advance(time.Millisecond)
}

// A sweep that moves several waiting asks spreads them over the free
// teammates by backlog rather than sending them all to the first.
func TestFailoverSweepSpreadsAsks(t *testing.T) {
	h, clk := failoverHarness(t)
	const scribeAddr = "100.0.0.5:1"
	joinWebAgent(h, clk, "scribe", scribeAddr, "chatgpt-web", "chatgpt.com")
	limitAt(h, clk, museAddr, time.Hour)
	body := fmt.Sprintf(`{"site":"chatgpt.com","state":"limited","until":%q,"observed_at":%q}`, clk.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), clk.Now().Format(time.RFC3339Nano))
	h.do(scribeAddr, "PUT", "/v1/agents/self/web-status", body, http.StatusNoContent, nil)
	clk.advance(time.Millisecond)
	limitAt(h, clk, grokAddr, time.Hour)
	var ids []string
	for i := range 4 {
		ids = append(ids, h.send(instinctAddr, "grokbot", fmt.Sprintf("question %d", i)).ID)
	}
	clearLimit(h, clk, museAddr)
	body = fmt.Sprintf(`{"site":"chatgpt.com","state":"authenticated","observed_at":%q}`, clk.Now().Format(time.RFC3339Nano))
	h.do(scribeAddr, "PUT", "/v1/agents/self/web-status", body, http.StatusNoContent, nil)
	clk.advance(time.Millisecond)
	listen(h, museAddr, scribeAddr)
	h.srv.Sweep(t.Context())
	took := map[string]int{}
	for _, id := range ids {
		took[getAs(h, instinctAddr, id).Request.To]++
	}
	if took["muse"] != 2 || took["scribe"] != 2 {
		t.Fatalf("asks went %v, want two each to muse and scribe", took)
	}
}

// A hold that ends before the ask expires (the 24h default) holds it as
// before; one that would end after fails it at once, with a failed reply
// that tells the asker why, instead of letting it lapse unsent.
func TestFailoverHoldVersusExpiry(t *testing.T) {
	t.Run("short hold", func(t *testing.T) {
		h, clk := failoverHarness(t)
		limitAt(h, clk, museAddr, 48*time.Hour)
		limitAt(h, clk, grokAddr, 23*time.Hour)
		sent := h.send(instinctAddr, "grokbot", "summarize this paper")
		res := getAs(h, instinctAddr, sent.ID)
		if res.Status != envelope.StatusQueued || res.Reply != nil || res.RelayNote == nil || !strings.Contains(res.RelayNote.Note, "it waits") {
			t.Fatalf("asker sees %+v", res)
		}
	})
	t.Run("long hold", func(t *testing.T) {
		h, clk := failoverHarness(t)
		limitAt(h, clk, museAddr, 48*time.Hour)
		until := limitAt(h, clk, grokAddr, 30*time.Hour)
		sent := h.send(instinctAddr, "grokbot", "summarize this paper")
		res := getAs(h, instinctAddr, sent.ID)
		want := "grokbot is out of messages until " + client.UTCClock(until, clk.Now()) + ", after this ask would expire; ask another teammate or re-ask after the reset"
		if res.Status != envelope.StatusFailed || res.Reply == nil || !strings.Contains(res.Reply.Body, want) {
			t.Fatalf("asker sees %+v", res)
		}
		if n := relayNotices(h, "instinct"); len(n) != 0 {
			t.Fatalf("a wait note went out too: %+v", n)
		}
		if a := auditEvents(h, "limit_outlives"); len(a) != 1 || a[0].RequestID != sent.ID {
			t.Fatalf("limit_outlives audit = %+v", a)
		}
	})
	t.Run("long handback", func(t *testing.T) {
		h, clk := failoverHarness(t)
		limitAt(h, clk, museAddr, 48*time.Hour)
		sent := h.send(instinctAddr, "grokbot", "summarize this paper")
		h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
		until := limitAt(h, clk, grokAddr, 30*time.Hour)
		out := handback(h, grokAddr, sent.ID, until, http.StatusOK)
		if out.Outcome != client.HandbackFailed {
			t.Fatalf("handback = %+v", out)
		}
		if res := getAs(h, instinctAddr, sent.ID); res.Status != envelope.StatusFailed || res.Reply == nil || !strings.Contains(res.Reply.Body, "after this ask would expire") {
			t.Fatalf("asker sees %+v", res)
		}
	})
	t.Run("limit extended past expiry", func(t *testing.T) {
		h, clk := failoverHarness(t)
		limitAt(h, clk, museAddr, 48*time.Hour)
		limitAt(h, clk, grokAddr, time.Hour)
		sent := h.send(instinctAddr, "grokbot", "summarize this paper")
		if res := getAs(h, instinctAddr, sent.ID); res.Status != envelope.StatusQueued {
			t.Fatalf("not held: %+v", res)
		}
		limitAt(h, clk, grokAddr, 30*time.Hour)
		h.srv.Sweep(t.Context())
		if res := getAs(h, instinctAddr, sent.ID); res.Status != envelope.StatusFailed || res.Reply == nil || !strings.Contains(res.Reply.Body, "after this ask would expire") {
			t.Fatalf("asker sees %+v", res)
		}
	})
}

// A hold the old relay saved past the ask's expiry, with the reported reset
// unchanged since, still fails the ask on the next sweep instead of letting
// it lapse unsent.
func TestFailoverSweepFailsSavedLongHold(t *testing.T) {
	h, clk := failoverHarness(t)
	limitAt(h, clk, museAddr, 48*time.Hour)
	limitAt(h, clk, grokAddr, time.Hour)
	sent := h.send(instinctAddr, "grokbot", "summarize this paper")
	if res := getAs(h, instinctAddr, sent.ID); res.Status != envelope.StatusQueued {
		t.Fatalf("not held: %+v", res)
	}
	until := clk.Now().Add(30 * time.Hour).UTC().Truncate(time.Millisecond)
	if _, err := h.st.HoldBack(t.Context(), sent.ID, "grokbot", until, false); err != nil {
		t.Fatal(err)
	}
	if got := limitAt(h, clk, grokAddr, 30*time.Hour); !got.Equal(until) {
		t.Fatalf("limit until %v, hold until %v", got, until)
	}
	h.srv.Sweep(t.Context())
	if res := getAs(h, instinctAddr, sent.ID); res.Status != envelope.StatusFailed || res.Reply == nil || !strings.Contains(res.Reply.Body, "after this ask would expire") {
		t.Fatalf("asker sees %+v", res)
	}
}

// Failing an ask whose hold would outlive it keeps HoldBack's conditions:
// an ask its target claimed after failover read it as queued, or a
// handback whose claim lapsed meanwhile, is left alone with ErrWrongState.
func TestFailOutlivedHoldSparesChangedAsk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claimed bool
	}{{"claimed after the read", false}, {"handback lease lapsed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h, clk := failoverHarness(t)
			ctx := t.Context()
			limitAt(h, clk, museAddr, 48*time.Hour)
			sent := h.send(instinctAddr, "grokbot", "summarize this paper")
			stale, _, err := h.st.Request(ctx, sent.ID)
			if err != nil {
				t.Fatal(err)
			}
			h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
			if tc.claimed {
				if stale, _, err = h.st.Request(ctx, sent.ID); err != nil {
					t.Fatal(err)
				}
				clk.advance(DefaultClaimLease + time.Second)
			}
			until := limitAt(h, clk, grokAddr, 30*time.Hour)
			f, _, err := h.srv.failoverFacts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.srv.failover(ctx, stale, f, until, time.Time{}, tc.claimed); !errors.Is(err, store.ErrWrongState) {
				t.Fatalf("failover = %v, want ErrWrongState", err)
			}
			if res := getAs(h, instinctAddr, sent.ID); res.Reply != nil || res.Status == envelope.StatusFailed {
				t.Fatalf("asker sees %+v", res)
			}
			if a := auditEvents(h, "limit_outlives"); len(a) != 0 {
				t.Fatalf("limit_outlives audit = %+v", a)
			}
		})
	}
}
