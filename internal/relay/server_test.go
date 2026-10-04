package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

const (
	macAddr      = "100.0.0.1:1"
	grokAddr     = "100.0.0.2:1"
	instinctAddr = "100.0.0.3:1"
	museAddr     = "100.0.0.4:1"
	strangerAddr = "100.0.0.9:1"
)

type harness struct {
	t   *testing.T
	srv *Server
	h   http.Handler
	st  *store.Store
	who *identitytest.Resolver
}

// newHarness starts a relay with grokbot, instinct, and muse joined, using a
// fake WhoIs so tests pick each caller's tailnet identity directly.
func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	return newHarnessDir(t, cfg, identity.Config{})
}

// newHarnessDir is newHarness with directory settings (admins are set here).
func newHarnessDir(t *testing.T, cfg Config, dcfg identity.Config) *harness {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	who := identitytest.New(map[string]identity.Node{
		macAddr:      {ID: "nMAC", Name: "macbook-pro-44"},
		grokAddr:     {ID: "nGROK", Name: "grok-bot"},
		instinctAddr: {ID: "nINST", Name: "instinct"},
		museAddr:     {ID: "nMUSE", Name: "muse"},
		strangerAddr: {ID: "nLAPTOP", Name: "old-laptop"},
	})
	dcfg.Admins = []string{"macbook-pro-44"}
	dir := identity.NewDirectory(st, who, dcfg)
	srv := New(dir, st, cfg)
	h := &harness{t: t, srv: srv, h: srv.Handler(), st: st, who: who}
	for name, addr := range map[string]string{"grokbot": grokAddr, "instinct": instinctAddr, "muse": museAddr} {
		var inv struct{ Code string }
		h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"`+name+`"}`, http.StatusOK, &inv)
		h.do(addr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	}
	return h
}

func (h *harness) do(addr, method, path, body string, want int, out any) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != want {
		h.t.Fatalf("%s %s from %s: status %d, want %d: %s", method, path, addr, rec.Code, want, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			h.t.Fatalf("decode %s: %v: %s", path, err, rec.Body.String())
		}
	}
	return rec
}

func (h *harness) send(addr, to, body string) envelope.Request {
	h.t.Helper()
	var req envelope.Request
	h.do(addr, "POST", "/v1/send", `{"to":"`+to+`","body":"`+body+`"}`, http.StatusCreated, &req)
	return req
}

type pollResult struct {
	Requests []envelope.Request `json:"requests"`
}

// AE1: a poller already waiting gets a new request in well under a second.
func TestWaitingPollerGetsRequestFast(t *testing.T) {
	h := newHarness(t, Config{PollHold: 5 * time.Second})
	var wg sync.WaitGroup
	var got pollResult
	var took time.Duration
	wg.Go(func() {
		start := time.Now()
		h.do(instinctAddr, "GET", "/v1/poll", "", http.StatusOK, &got)
		took = time.Since(start)
	})
	time.Sleep(100 * time.Millisecond) // let the poll start waiting
	sent := h.send(grokAddr, "instinct", "what is in the report")
	wg.Wait()
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("poll got %+v", got)
	}
	if took > time.Second {
		t.Fatalf("delivery took %v, want under 1s after send", took)
	}
	if got.Requests[0].From != "grokbot" {
		t.Fatalf("from = %q, want grokbot", got.Requests[0].From)
	}
}

// AE2: sent while the target is away, delivered on its next poll.
func TestQueuedUntilNextPoll(t *testing.T) {
	h := newHarness(t, Config{})
	sent := h.send(grokAddr, "muse", "call Joe's Garage")
	var got pollResult
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("poll got %+v", got)
	}
}

func TestPollTimesOutWithNoContent(t *testing.T) {
	h := newHarness(t, Config{PollHold: 200 * time.Millisecond})
	start := time.Now()
	h.do(museAddr, "GET", "/v1/poll", "", http.StatusNoContent, nil)
	if d := time.Since(start); d < 150*time.Millisecond || d > 2*time.Second {
		t.Fatalf("empty poll returned after %v", d)
	}
}

// Full ask, claim, reply, and a waiting get-reply on the sender side.
func TestAskClaimReplyWithWaitingSender(t *testing.T) {
	h := newHarness(t, Config{MaxWait: 5 * time.Second})
	sent := h.send(grokAddr, "instinct", "rows?")
	var res store.Result
	var wg sync.WaitGroup
	wg.Go(func() {
		h.do(grokAddr, "GET", "/v1/requests/"+sent.ID+"?wait=5", "", http.StatusOK, &res)
	})
	time.Sleep(100 * time.Millisecond)
	h.do(instinctAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	h.do(instinctAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"3 rows"}`, http.StatusOK, nil)
	wg.Wait()
	if res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.Body != "3 rows" {
		t.Fatalf("sender saw %+v", res)
	}
}

// AE5: a tailnet machine that never joined is refused.
func TestUnjoinedMachineRejected(t *testing.T) {
	h := newHarness(t, Config{})
	h.do(strangerAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusForbidden, nil)
	h.do(strangerAddr, "GET", "/v1/poll?hold=0", "", http.StatusForbidden, nil)
}

// doAs is do with the X-Tincan-Agent header the client sends.
func (h *harness) doAs(addr, agent, method, path, body string, want int, out any) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = addr
	req.Header.Set(client.AgentHeader, agent)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != want {
		h.t.Fatalf("%s %s from %s as %q: status %d, want %d: %s", method, path, addr, agent, rec.Code, want, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			h.t.Fatalf("decode %s: %v: %s", path, err, rec.Body.String())
		}
	}
	return rec
}

// joinAt invites name and joins it from addr.
func (h *harness) joinAt(addr, name string) {
	h.t.Helper()
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"`+name+`"}`, http.StatusOK, &inv)
	h.do(addr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
}

// Two agents on one machine: the header picks which one a request is from.
func TestTwoAgentsOnOneMachine(t *testing.T) {
	h := newHarness(t, Config{})
	h.joinAt(strangerAddr, "claude-code")
	// One agent on the node: no header needed.
	var req envelope.Request
	h.do(strangerAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusCreated, &req)
	if req.From != "claude-code" {
		t.Fatalf("single-agent from = %q", req.From)
	}
	h.joinAt(strangerAddr, "codex")
	for _, name := range []string{"codex", "claude-code"} {
		h.doAs(strangerAddr, name, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusCreated, &req)
		if req.From != name {
			t.Fatalf("header %s: from = %q", name, req.From)
		}
	}
	// Each agent polls its own inbox.
	h.send(grokAddr, "codex", "for codex")
	var got pollResult
	h.doAs(strangerAddr, "codex", "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].To != "codex" {
		t.Fatalf("codex poll = %+v", got)
	}
	h.doAs(strangerAddr, "claude-code", "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	// No header with two agents: rejected, naming both.
	rec := h.do(strangerAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusForbidden, nil)
	if b := rec.Body.String(); !strings.Contains(b, "claude-code") || !strings.Contains(b, "codex") {
		t.Fatalf("ambiguity error should list both agents: %s", b)
	}
	// Removing codex leaves claude-code working without a header.
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"codex"}`, http.StatusOK, nil)
	h.do(strangerAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusCreated, &req)
	if req.From != "claude-code" {
		t.Fatalf("after removing codex from = %q", req.From)
	}
	h.doAs(strangerAddr, "codex", "GET", "/v1/poll?hold=0", "", http.StatusForbidden, nil)
}

// The header cannot borrow another machine's agent.
func TestHeaderNamingAnotherMachinesAgentRejected(t *testing.T) {
	h := newHarness(t, Config{})
	rec := h.doAs(museAddr, "grokbot", "POST", "/v1/send", `{"to":"instinct","body":"hi"}`, http.StatusForbidden, nil)
	if !strings.Contains(rec.Body.String(), identity.ErrNotJoined.Error()) {
		t.Fatalf("want not joined, got %s", rec.Body.String())
	}
	h.doAs(museAddr, "grokbot", "GET", "/v1/poll?hold=0", "", http.StatusForbidden, nil)
	// The matching header is fine.
	h.doAs(museAddr, "muse", "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
}

// AE6: claiming to be someone else changes nothing.
func TestSpoofedFromIsIgnored(t *testing.T) {
	h := newHarness(t, Config{})
	var req envelope.Request
	h.do(museAddr, "POST", "/v1/send", `{"from":"grokbot","to":"instinct","body":"hi"}`, http.StatusCreated, &req)
	if req.From != "muse" {
		t.Fatalf("from = %q, want muse", req.From)
	}
}

func TestThirdPartyCannotTouchRequest(t *testing.T) {
	h := newHarness(t, Config{})
	sent := h.send(grokAddr, "muse", "x")
	h.do(instinctAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusForbidden, nil)
	h.do(instinctAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"x"}`, http.StatusForbidden, nil)
	h.do(instinctAddr, "GET", "/v1/requests/"+sent.ID, "", http.StatusNotFound, nil)
	h.do(instinctAddr, "POST", "/v1/requests/"+sent.ID+"/cancel", "", http.StatusForbidden, nil)
}

func TestSendToUnknownAgent(t *testing.T) {
	h := newHarness(t, Config{})
	h.do(grokAddr, "POST", "/v1/send", `{"to":"nobody","body":"x"}`, http.StatusNotFound, nil)
}

func TestSenderCancels(t *testing.T) {
	h := newHarness(t, Config{})
	sent := h.send(grokAddr, "muse", "x")
	h.do(grokAddr, "POST", "/v1/requests/"+sent.ID+"/cancel", "", http.StatusOK, nil)
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
}

func TestRemoveCancelsQueuedRequests(t *testing.T) {
	h := newHarness(t, Config{})
	sent := h.send(grokAddr, "muse", "x")
	h.do(museAddr, "POST", "/v1/admin/remove", `{"name":"grokbot"}`, http.StatusForbidden, nil)
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"muse"}`, http.StatusOK, nil)
	var res store.Result
	h.do(grokAddr, "GET", "/v1/requests/"+sent.ID, "", http.StatusOK, &res)
	if res.Status != envelope.StatusCancelled {
		t.Fatalf("status = %s, want cancelled", res.Status)
	}
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusForbidden, nil)
}

// Removing an agent also withdraws what it sent: the target must not act on
// a request from an agent that no longer exists.
func TestRemoveCancelsRequestsTheAgentSent(t *testing.T) {
	h := newHarness(t, Config{})
	sent := h.send(grokAddr, "muse", "x")
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"grokbot"}`, http.StatusOK, nil)
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	if _, st, err := h.st.Request(context.Background(), sent.ID); err != nil || st != envelope.StatusCancelled {
		t.Fatalf("status = %s, %v, want cancelled", st, err)
	}
}

type failingConnector struct{}

func (failingConnector) Connect(context.Context, string) (string, string, error) {
	return "", "", errors.New("unused")
}
func (failingConnector) Revoke(context.Context, string) error { return errors.New("token store down") }

// If tokens cannot be revoked, the removal fails and nothing changes: the
// agent stays bound and its requests stay open.
func TestRemoveFailsWhenRevokeFails(t *testing.T) {
	h := newHarness(t, Config{})
	h.srv.SetConnector(failingConnector{})
	sent := h.send(grokAddr, "muse", "x")
	rec := h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"muse"}`, http.StatusInternalServerError, nil)
	if !strings.Contains(rec.Body.String(), "token store down") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	var out struct{ Agents []client.AgentInfo }
	h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, &out)
	found := false
	for _, a := range out.Agents {
		found = found || a.Name == "muse"
	}
	if !found {
		t.Fatalf("muse was removed despite the revoke failure: %+v", out.Agents)
	}
	if _, st, _ := h.st.Request(context.Background(), sent.ID); st != envelope.StatusQueued {
		t.Fatalf("request status = %s, want queued", st)
	}
}

func TestAgentsListShowsPresence(t *testing.T) {
	h := newHarness(t, Config{})
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	var out struct{ Agents []client.AgentInfo }
	h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, &out)
	online := map[string]bool{}
	for _, a := range out.Agents {
		online[a.Name] = a.Online
		if a.Wake != "none" {
			t.Errorf("%s wake = %q, want none by default", a.Name, a.Wake)
		}
	}
	if len(out.Agents) != 3 || !online["muse"] || online["instinct"] {
		t.Fatalf("agents = %+v", out.Agents)
	}
}

func TestLocalAdminSocketCanInvite(t *testing.T) {
	h := newHarness(t, Config{})
	req := httptest.NewRequest("POST", "/v1/admin/invite", strings.NewReader(`{"name":"claude-code"}`))
	req.RemoteAddr = "@"
	rec := httptest.NewRecorder()
	h.srv.AdminHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("local invite: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSweepRequeuesAndWakesPoller(t *testing.T) {
	h := newHarness(t, Config{PollHold: 5 * time.Second, DeliveryLease: time.Millisecond})
	sent := h.send(grokAddr, "muse", "x")
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil) // delivered, never claimed
	time.Sleep(10 * time.Millisecond)
	var got pollResult
	var wg sync.WaitGroup
	wg.Go(func() { h.do(museAddr, "GET", "/v1/poll", "", http.StatusOK, &got) })
	time.Sleep(50 * time.Millisecond)
	h.srv.Sweep(context.Background())
	wg.Wait()
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("redelivery got %+v", got)
	}
}

func TestOversizeBodyRejected(t *testing.T) {
	h := newHarness(t, Config{})
	big := strings.Repeat("x", 2<<20)
	h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"`+big+`"}`, http.StatusRequestEntityTooLarge, nil)
}

type traceOut struct {
	Steps  []store.TraceStep  `json:"steps"`
	Events []store.AuditEvent `json:"events"`
}

// AE3 chain shows up in order, with its audit trail, for participants and
// for Matt's device, but not for an agent outside the chain.
func TestTraceVisibility(t *testing.T) {
	h := newHarness(t, Config{})
	h.srv.SetPreparer(chainPrep{h.st})
	first := h.send(instinctAddr, "muse", "call the dentist")
	h.do(museAddr, "POST", "/v1/requests/"+first.ID+"/claim", "", http.StatusOK, nil)
	var second envelope.Request
	h.do(museAddr, "POST", "/v1/send", `{"to":"grokbot","body":"new time Tue 3pm","parent_id":"`+first.ID+`"}`, http.StatusCreated, &second)

	var tr traceOut
	h.do(macAddr, "GET", "/v1/trace/"+first.TraceID, "", http.StatusOK, &tr)
	if len(tr.Steps) != 2 || tr.Steps[1].Request.From != "muse" || tr.Steps[1].Request.To != "grokbot" {
		t.Fatalf("trace steps = %+v", tr.Steps)
	}
	var events []string
	for _, e := range tr.Events {
		events = append(events, e.Event)
	}
	if strings.Join(events, ",") != "queued,claimed,queued" {
		t.Fatalf("events = %v", events)
	}
	h.do(grokAddr, "GET", "/v1/trace/"+first.TraceID, "", http.StatusOK, nil) // participant
	solo := h.send(grokAddr, "instinct", "private")
	h.do(museAddr, "GET", "/v1/trace/"+solo.TraceID, "", http.StatusNotFound, nil)
	h.do(museAddr, "GET", "/v1/trace", "", http.StatusForbidden, nil)
	var recent struct{ Traces []store.TraceStep }
	h.do(macAddr, "GET", "/v1/trace?limit=5", "", http.StatusOK, &recent)
	if len(recent.Traces) != 2 {
		t.Fatalf("recent = %+v", recent.Traces)
	}
}

func TestRejectionsAreAudited(t *testing.T) {
	h := newHarness(t, Config{})
	h.srv.SetPreparer(rejectAll{})
	h.do(museAddr, "POST", "/v1/send", `{"to":"grokbot","body":"x"}`, http.StatusConflict, nil)
	var out struct {
		OK      bool
		Checked int
	}
	h.do(macAddr, "GET", "/v1/admin/audit/verify", "", http.StatusOK, &out)
	if !out.OK {
		t.Fatalf("verify = %+v", out)
	}
	rows, _ := h.st.DB().Query(`SELECT event, actor, detail FROM audit WHERE event = 'rejected'`)
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("rejection not audited")
	}
	var ev, actor, detail string
	rows.Scan(&ev, &actor, &detail)
	if actor != "muse" || !strings.Contains(detail, "loop") {
		t.Fatalf("rejected row = %s %s %s", ev, actor, detail)
	}
}

type rejectAll struct{}

func (rejectAll) Prepare(context.Context, *envelope.Request) error {
	return &StatusError{Code: http.StatusConflict, Err: errors.New("request would loop")}
}

// chainPrep is a minimal parent-following preparer for relay tests (the real
// one lives in package policy, which imports relay).
type chainPrep struct{ st *store.Store }

func (c chainPrep) Prepare(ctx context.Context, req *envelope.Request) error {
	if req.ParentID == "" {
		req.Hop, req.Chain = 1, []string{req.From}
		return nil
	}
	p, _, err := c.st.Request(ctx, req.ParentID)
	if err != nil {
		return err
	}
	req.TraceID, req.Hop, req.Chain = p.TraceID, p.Hop+1, append(append([]string{}, p.Chain...), req.From)
	return nil
}

// A listener peeks: it learns requests are waiting without taking them, so
// the agent's own inbox check still receives them.
func TestPeekDoesNotDeliver(t *testing.T) {
	h := newHarness(t, Config{PollHold: 5 * time.Second})
	var peek struct{ Waiting int }
	var wg sync.WaitGroup
	wg.Go(func() { h.do(museAddr, "GET", "/v1/poll?peek=1", "", http.StatusOK, &peek) })
	time.Sleep(100 * time.Millisecond)
	if !h.srv.Online("muse") {
		t.Error("an agent holding a poll should count as online")
	}
	sent := h.send(grokAddr, "muse", "call Joe's Garage")
	wg.Wait()
	if peek.Waiting != 1 {
		t.Fatalf("peek = %+v", peek)
	}
	var got pollResult
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("request was taken by the peek: %+v", got)
	}
}

type fixedWake map[string]string

func (f fixedWake) WakeMethod(a string) string { return f[a] }

func TestAgentsListShowsOnlyWakeMethodName(t *testing.T) {
	h := newHarness(t, Config{})
	h.srv.SetWakeNamer(fixedWake{"grokbot": "webhook", "instinct": "email", "muse": "wait"})
	rec := h.do(museAddr, "GET", "/v1/agents", "", http.StatusOK, nil)
	body := rec.Body.String()
	for _, want := range []string{`"wake":"webhook"`, `"wake":"email"`, `"wake":"wait"`} {
		if !strings.Contains(body, want) {
			t.Errorf("agents list missing %s: %s", want, body)
		}
	}
	for _, leak := range []string{"http://", "https://", "@", "bearer"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("agents list leaks %q: %s", leak, body)
		}
	}
}

// An admin device that never joined can read the roster (onboarding runs
// there); an unjoined non-admin machine still cannot.
func TestAgentsListAdmitsUnjoinedAdmin(t *testing.T) {
	h := newHarness(t, Config{})
	var out struct{ Agents []client.AgentInfo }
	h.do(macAddr, "GET", "/v1/agents", "", http.StatusOK, &out)
	if len(out.Agents) != 3 {
		t.Fatalf("admin roster = %+v", out.Agents)
	}
	rec := h.do(strangerAddr, "GET", "/v1/agents", "", http.StatusForbidden, nil)
	if !strings.Contains(rec.Body.String(), identity.ErrNotJoined.Error()) {
		t.Fatalf("want not joined, got %s", rec.Body.String())
	}
}

func kinds(t *testing.T, h *harness) map[string]string {
	t.Helper()
	var out struct{ Agents []client.AgentInfo }
	h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, &out)
	m := map[string]string{}
	for _, a := range out.Agents {
		m[a.Name] = a.Kind
	}
	return m
}

// An invite can carry the agent's kind; it is applied when the agent joins
// and shows in the roster. An admin can change it later; nobody else can.
func TestInviteKindAppliedOnJoinAndAdminSetsKind(t *testing.T) {
	h := newHarness(t, Config{})
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"hermes","kind":"hermes"}`, http.StatusOK, &inv)
	h.do(strangerAddr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	if k := kinds(t, h); k["hermes"] != "hermes" || k["muse"] != "" {
		t.Fatalf("kinds after join = %v", k)
	}
	rec := h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, nil)
	if strings.Contains(rec.Body.String(), `"name":"muse","online":false,"wake":"none","kind"`) {
		t.Fatalf("an unknown kind should be omitted: %s", rec.Body.String())
	}

	h.do(macAddr, "PUT", "/v1/agents/hermes/kind", `{"kind":"openclaw"}`, http.StatusOK, nil)
	if k := kinds(t, h); k["hermes"] != "openclaw" {
		t.Fatalf("kinds after admin set = %v", k)
	}
	// Non-admins, joined or not, are refused and nothing changes.
	h.do(grokAddr, "PUT", "/v1/agents/hermes/kind", `{"kind":"codex"}`, http.StatusForbidden, nil)
	h.do(strangerAddr, "PUT", "/v1/agents/hermes/kind", `{"kind":"codex"}`, http.StatusForbidden, nil)
	if k := kinds(t, h); k["hermes"] != "openclaw" {
		t.Fatalf("non-admin changed kind: %v", k)
	}
	// Unknown agents and unknown kinds are rejected; "" clears.
	h.do(macAddr, "PUT", "/v1/agents/nobody/kind", `{"kind":"codex"}`, http.StatusNotFound, nil)
	h.do(macAddr, "PUT", "/v1/agents/hermes/kind", `{"kind":"codx"}`, http.StatusBadRequest, nil)
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"cx","kind":"codx"}`, http.StatusBadRequest, nil)
	h.do(macAddr, "PUT", "/v1/agents/hermes/kind", `{"kind":""}`, http.StatusOK, nil)
	if k := kinds(t, h); k["hermes"] != "" {
		t.Fatalf("kind not cleared: %v", k)
	}
}

// The command-woken Gemini teammate's kind is one the relay accepts, on an
// invite and when an admin sets it.
func TestRelayAcceptsGeminiCLIKind(t *testing.T) {
	h := newHarness(t, Config{})
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"gemini-cli","kind":"gemini-cli"}`, http.StatusOK, &inv)
	h.do(strangerAddr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	if k := kinds(t, h); k["gemini-cli"] != "gemini-cli" {
		t.Fatalf("kinds after join = %v", k)
	}
	h.do(macAddr, "PUT", "/v1/agents/muse/kind", `{"kind":"gemini-cli"}`, http.StatusOK, nil)
	if k := kinds(t, h); k["muse"] != "gemini-cli" {
		t.Fatalf("kinds after admin set = %v", k)
	}
}

// The command-woken Grok CLI teammate's kind is one the relay accepts, on
// an invite and when an admin sets it.
func TestRelayAcceptsGrokCLIKind(t *testing.T) {
	h := newHarness(t, Config{})
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"grok-cli","kind":"grok-cli"}`, http.StatusOK, &inv)
	h.do(strangerAddr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	if k := kinds(t, h); k["grok-cli"] != "grok-cli" {
		t.Fatalf("kinds after join = %v", k)
	}
	h.do(macAddr, "PUT", "/v1/agents/muse/kind", `{"kind":"grok-cli"}`, http.StatusOK, nil)
	if k := kinds(t, h); k["muse"] != "grok-cli" {
		t.Fatalf("kinds after admin set = %v", k)
	}
}

// The council service's kind is one the relay accepts, on an invite and when
// an admin sets it.
func TestRelayAcceptsCouncilKind(t *testing.T) {
	h := newHarness(t, Config{})
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"council","kind":"council"}`, http.StatusOK, &inv)
	h.do(strangerAddr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	if k := kinds(t, h); k["council"] != "council" {
		t.Fatalf("kinds after join = %v", k)
	}
}

// The local admin socket can set a kind too.
func TestLocalAdminSocketCanSetKind(t *testing.T) {
	h := newHarness(t, Config{})
	req := httptest.NewRequest("PUT", "/v1/agents/muse/kind", strings.NewReader(`{"kind":"proxy-sandbox"}`))
	req.RemoteAddr = "@"
	rec := httptest.NewRecorder()
	h.srv.AdminHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("local set kind: %d %s", rec.Code, rec.Body.String())
	}
	if k := kinds(t, h); k["muse"] != "proxy-sandbox" {
		t.Fatalf("kinds = %v", k)
	}
}

func goodAts(t *testing.T, h *harness) map[string]string {
	t.Helper()
	var out struct{ Agents []client.AgentInfo }
	h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, &out)
	m := map[string]string{}
	for _, a := range out.Agents {
		m[a.Name] = a.GoodAt
	}
	return m
}

// An agent joined under a product runtime name without a kind shows that
// product's stock line, and an owner line replaces it.
func TestRosterStockGoodAtFromProductName(t *testing.T) {
	h := newHarness(t, Config{})
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"claude-code"}`, http.StatusOK, &inv)
	h.do(strangerAddr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	if k := kinds(t, h); k["claude-code"] != "" {
		t.Fatalf("claude-code joined with kind %q, want none", k["claude-code"])
	}
	if g := goodAts(t, h)["claude-code"]; !strings.HasPrefix(g, "Claude Code in a terminal") {
		t.Fatalf("claude-code stock line = %q", g)
	}
	h.do(macAddr, "PUT", "/v1/agents/claude-code/good-at", `{"good_at":"reviews PRs"}`, http.StatusOK, nil)
	if g := goodAts(t, h)["claude-code"]; g != "reviews PRs" {
		t.Fatalf("claude-code line after owner set = %q", g)
	}
}

// The local admin socket can set a good-at line, and the change is audited
// with the agent as actor and the new line as detail.
func TestLocalAdminSocketCanSetGoodAtAndItIsAudited(t *testing.T) {
	h := newHarness(t, Config{})
	req := httptest.NewRequest("PUT", "/v1/agents/muse/good-at", strings.NewReader(`{"good_at":"phone calls"}`))
	req.RemoteAddr = "@"
	rec := httptest.NewRecorder()
	h.srv.AdminHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("local set good-at: %d %s", rec.Code, rec.Body.String())
	}
	if g := goodAts(t, h); g["muse"] != "phone calls" {
		t.Fatalf("good-at = %v", g)
	}
	events, err := h.st.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Event == "good-at" {
			if e.Actor != "muse" || !strings.Contains(e.Detail, "phone calls") {
				t.Fatalf("good-at audit = %+v", e)
			}
			return
		}
	}
	t.Fatalf("no good-at audit in %+v", events)
}

// A body without good_at, or with good_at null, is refused rather than read
// as a clear, so a typo in the field name cannot erase the owner's line.
func TestSetGoodAtRequiresTheField(t *testing.T) {
	h := newHarness(t, Config{})
	set := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/v1/agents/muse/good-at", strings.NewReader(body))
		req.RemoteAddr = "@"
		rec := httptest.NewRecorder()
		h.srv.AdminHandler().ServeHTTP(rec, req)
		return rec
	}
	if rec := set(`{"good_at":"phone calls"}`); rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{`{}`, `{"good_a":"x"}`, `{"good_at":null}`} {
		if rec := set(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", body, rec.Code, rec.Body.String())
		}
	}
	if g := goodAts(t, h); g["muse"] != "phone calls" {
		t.Fatalf("line changed by a refused body: %v", g)
	}
	if rec := set(`{"good_at":""}`); rec.Code != http.StatusOK {
		t.Fatalf("explicit clear: %d %s", rec.Code, rec.Body.String())
	}
	if g := goodAts(t, h); g["muse"] != "" {
		t.Fatalf("explicit clear left %v", g)
	}
}

type queuedRecorder struct {
	mu sync.Mutex
	to []string
}

func (q *queuedRecorder) Queued(_ context.Context, req envelope.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.to = append(q.to, req.To)
}

// A relay-woken agent (webhook or email) that claims a request and then
// times out has no poller waiting, so the requeue must fire the wake again
// or the request sits until something unrelated wakes the agent.
func TestSweepRequeueWakesAgainForRelayWake(t *testing.T) {
	h := newHarness(t, Config{DeliveryLease: time.Millisecond})
	rec := &queuedRecorder{}
	h.srv.SetEvents(rec)
	h.send(grokAddr, "muse", "x")
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil) // delivered, never claimed
	time.Sleep(10 * time.Millisecond)
	h.srv.Sweep(context.Background())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.to) != 2 || rec.to[1] != "muse" {
		t.Fatalf("wake events = %v, want the original queue and one for the requeue", rec.to)
	}
}

type requeueRecorder struct {
	queuedRecorder
	requeued []string
}

func (q *requeueRecorder) Requeued(_ context.Context, req envelope.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.requeued = append(q.requeued, req.To)
}

func TestSweepPrefersRequeuedHook(t *testing.T) {
	h := newHarness(t, Config{DeliveryLease: time.Millisecond})
	rec := &requeueRecorder{}
	h.srv.SetEvents(rec)
	h.send(grokAddr, "muse", "x")
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil)
	time.Sleep(10 * time.Millisecond)
	h.srv.Sweep(context.Background())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.to) != 1 || len(rec.requeued) != 1 || rec.requeued[0] != "muse" {
		t.Fatalf("queued = %v, requeued = %v; want the requeue to use Requeued", rec.to, rec.requeued)
	}
}

func TestGroupSendLimit(t *testing.T) {
	h := newHarness(t, Config{})
	body := `{"to":"muse","body":"hello","group":"group-limit"}`
	for range store.MaxGroupRequests {
		h.do(grokAddr, "POST", "/v1/send", body, http.StatusCreated, nil)
	}
	rec := h.do(grokAddr, "POST", "/v1/send", body, http.StatusBadRequest, nil)
	if !strings.Contains(rec.Body.String(), "group already has 8 requests from this sender") {
		t.Fatal(rec.Body.String())
	}
	// A different sender has a separate bound; ungrouped sends stay additive.
	h.do(instinctAddr, "POST", "/v1/send", body, http.StatusCreated, nil)
	h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"hello"}`, http.StatusCreated, nil)
	var members []envelope.GroupMember
	h.do(grokAddr, "GET", "/v1/groups/group-limit", "", http.StatusOK, &members)
	if len(members) != store.MaxGroupRequests {
		t.Fatalf("%+v", members)
	}
}

func TestProgressAuthorizationAndPrivacy(t *testing.T) {
	h := newHarness(t, Config{})
	req := h.send(grokAddr, "muse", "work")
	path := "/v1/requests/" + req.ID
	h.do(museAddr, "POST", path+"/progress", `{"note":"working"}`, 409, nil)
	h.do(museAddr, "POST", path+"/claim", "", 200, nil)
	h.do(grokAddr, "POST", path+"/progress", `{"note":"working"}`, 409, nil)
	h.do(instinctAddr, "POST", path+"/progress", `{"note":"working"}`, 409, nil)
	h.do(museAddr, "POST", path+"/progress", `{"note":""}`, 400, nil)
	h.do(museAddr, "POST", path+"/progress", `{"note":"`+strings.Repeat("é", 513)+`"}`, 413, nil)
	note := strings.Repeat("é", 512)
	wake := h.srv.hub.wait(requestKey(req.ID))
	inboxWake := h.srv.hub.wait(inboxKey("grokbot"))
	h.do(museAddr, "POST", path+"/progress", `{"note":"`+note+`","by":"grokbot"}`, 200, nil)
	select {
	case <-wake:
		t.Fatal("progress woke held get")
	default:
	}
	select {
	case <-inboxWake:
		t.Fatal("progress woke inbox")
	default:
	}
	var res envelope.Result
	h.do(grokAddr, "GET", path, "", 200, &res)
	if res.Progress == nil || res.Progress.Note != note || res.Progress.By != "muse" {
		t.Fatalf("result: %+v", res)
	}
	h.do(museAddr, "POST", path+"/progress", `{"note":"latest"}`, 200, nil)
	steps, err := h.st.Trace(context.Background(), req.TraceID)
	if err != nil || len(steps) != 1 || steps[0].Progress.Note != "latest" {
		t.Fatalf("trace: %+v %v", steps, err)
	}
	events, err := h.st.AuditForTrace(context.Background(), req.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if strings.Contains(event.Detail, note) || strings.Contains(event.Detail, "latest") {
			t.Fatal("note leaked to audit")
		}
		if event.Event == "progress" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("progress events: %d", count)
	}
	h.do(museAddr, "POST", path+"/reply", `{"body":"done"}`, 200, nil)
	h.do(museAddr, "POST", path+"/progress", `{"note":"late"}`, 409, nil)
}

func TestSweepUrgentLeaseExpiryWakesWithoutDebounce(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		name := "delivery"
		if claimed {
			name = "claim"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{})
			var req envelope.Request
			h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"urgent","urgent":true}`, http.StatusCreated, &req)
			ctx := t.Context()
			if claimed {
				if _, err := h.st.Claim(ctx, req.ID, "muse", -time.Second); err != nil {
					t.Fatal(err)
				}
			} else {
				got, err := h.st.Deliver(ctx, "muse", 1, -time.Second)
				if err != nil || len(got) != 1 {
					t.Fatalf("deliver = %v, %v", got, err)
				}
			}
			woken := make(chan struct{}, 1)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				woken <- struct{}{}
			}))
			defer ts.Close()
			waker := wake.New(wake.Config{"muse": {Method: wake.Webhook, URL: ts.URL}}, h.st, wake.Options{Debounce: 2 * time.Second})
			defer waker.Flush()
			h.srv.SetEvents(waker)
			h.srv.Sweep(ctx)
			select {
			case <-woken:
			case <-time.After(time.Second):
				t.Fatal("urgent requeue waited for debounce")
			}
			stored, status, err := h.st.Request(ctx, req.ID)
			if err != nil || status != envelope.StatusQueued || !stored.Urgent {
				t.Fatalf("requeued request = %+v, %s, %v", stored, status, err)
			}
		})
	}
}

func TestPingGateTracksReceivingPollers(t *testing.T) {
	for _, pollPath := range []string{"/v1/poll?hold=0", "/v1/poll?peek=1&hold=0"} {
		t.Run(pollPath, func(t *testing.T) {
			h := newHarness(t, Config{})
			advertise := func(path, features string) {
				req := httptest.NewRequest("GET", path, nil)
				req.RemoteAddr = museAddr
				req.Header.Set(client.FeaturesHeader, features)
				rec := httptest.NewRecorder()
				h.h.ServeHTTP(rec, req)
				if rec.Code != 200 && rec.Code != 204 {
					t.Fatalf("advertise: %d %s", rec.Code, rec.Body)
				}
			}
			ping := func(status int) {
				rec := h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","kind":"ping"}`, status, nil)
				if status == 409 && !strings.Contains(rec.Body.String(), "has not advertised ping support; use ask") {
					t.Fatal(rec.Body.String())
				}
			}
			advertise("/v1/agents", "ping")
			ping(409)
			advertise(pollPath, "")
			advertise("/v1/agents", "ping")
			ping(409)
			advertise(pollPath, "ping")
			ping(409)
			// Reconstruct from the store as on relay restart.
			h.srv = New(h.srv.dir, h.st, Config{})
			h.h = h.srv.Handler()
			ping(409)
			h.srv.mu.Lock()
			state := h.srv.pollFeatures["muse"]
			state.LastUnsupported = time.Now().Add(-legacyPollWindow - time.Second)
			h.srv.pollFeatures["muse"] = state
			h.srv.mu.Unlock()
			ping(201)
		})
	}
}

type refundingPreparer struct{ refunded []envelope.Request }

func (p *refundingPreparer) Prepare(_ context.Context, req *envelope.Request) error {
	return newChain{}.Prepare(context.Background(), req)
}

func (p *refundingPreparer) Refund(req envelope.Request) { p.refunded = append(p.refunded, req) }

func TestFailedSendIsRefunded(t *testing.T) {
	h := newHarness(t, Config{})
	prep := &refundingPreparer{}
	h.srv.SetPreparer(prep)
	h.srv.SetAttachmentDir(t.TempDir())
	// An attachment id that was never uploaded fails at queue time, after Prepare.
	h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"x","urgent":true,"attachments":[{"id":"missing"}]}`, http.StatusBadRequest, nil)
	if len(prep.refunded) != 1 || !prep.refunded[0].Urgent || prep.refunded[0].From != "grokbot" {
		t.Fatalf("refunded = %+v", prep.refunded)
	}
	h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"x","urgent":true}`, http.StatusCreated, nil)
	if len(prep.refunded) != 1 {
		t.Fatalf("a queued send was refunded: %+v", prep.refunded)
	}
}

func TestPeekCountsAllUrgentRequests(t *testing.T) {
	h := newHarness(t, Config{})
	for range MaxPeekPending + 5 {
		if _, err := h.st.Enqueue(context.Background(), envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindAsk, Body: "x", Urgent: true}, time.Hour); err != nil {
			h.t.Fatal(err)
		}
	}
	var out client.Waiting
	h.do(museAddr, "GET", "/v1/poll?hold=0&peek=1", "", http.StatusOK, &out)
	if out.Urgent != MaxPeekPending+5 || len(out.Pending) != MaxPeekPending {
		t.Fatalf("peek = urgent %d, pending %d", out.Urgent, len(out.Pending))
	}
}

func TestPingCapablePollAndExactPeekCount(t *testing.T) {
	h := newHarness(t, Config{})
	req := httptest.NewRequest("GET", "/v1/poll?peek=1&hold=0", nil)
	req.RemoteAddr = museAddr
	req.Header.Set(client.FeaturesHeader, "ping")
	h.h.ServeHTTP(httptest.NewRecorder(), req)
	// A CLI without the header must not remove poller support.
	h.do(museAddr, "GET", "/v1/agents", "", 200, nil)
	h.srv = New(h.srv.dir, h.st, Config{})
	h.h = h.srv.Handler()
	for range MaxPeekPending + 1 {
		h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","kind":"ping"}`, 201, nil)
	}
	h.send(grokAddr, "muse", "work behind pings")
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	var waiting client.Waiting
	if err := json.Unmarshal(rec.Body.Bytes(), &waiting); err != nil {
		t.Fatal(err)
	}
	// Pings have their own cap, so the work behind them is still named.
	if waiting.Total != MaxPeekPending+2 || waiting.Pings != MaxPeekPending+1 || len(waiting.Pending) != MaxPeekPending+1 ||
		waiting.Pending[MaxPeekPending].Kind == envelope.KindPing {
		t.Fatalf("peek: %+v", waiting)
	}
}

func TestPeekListsPingBehindFullBacklog(t *testing.T) {
	h := newHarness(t, Config{})
	for _, kind := range append(slices.Repeat([]envelope.Kind{envelope.KindAsk}, MaxPeekPending+5), envelope.KindPing) {
		if _, err := h.st.Enqueue(context.Background(), envelope.Request{From: "grokbot", To: "muse", Kind: kind, Body: "x"}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	var out client.Waiting
	h.do(museAddr, "GET", "/v1/poll?hold=0&peek=1", "", http.StatusOK, &out)
	if out.Pings != 1 || len(out.Pending) != MaxPeekPending+1 || out.Pending[0].Kind != envelope.KindPing {
		t.Fatalf("peek = pings %d, pending %d, first %+v", out.Pings, len(out.Pending), out.Pending[0])
	}
}

func TestAgentsListQueueStats(t *testing.T) {
	h := newHarness(t, Config{})
	ctx := context.Background()
	req, err := h.st.Enqueue(ctx, envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindAsk}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := h.st.Enqueue(ctx, envelope.Request{From: "grokbot", To: "muse", Kind: envelope.KindAsk}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.Claim(ctx, claimed.ID, "muse", time.Minute); err != nil {
		t.Fatal(err)
	}
	var out client.Roster
	rec := h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, &out)
	for _, a := range out.Agents {
		if a.Name == "muse" && (a.Queued != 1 || a.Claimed != 1 || !a.OldestQueued.Equal(req.CreatedAt)) {
			t.Fatalf("muse = %+v", a)
		}
	}
	var raw struct{ Agents []map[string]json.RawMessage }
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, a := range raw.Agents {
		for _, field := range []string{"queued", "oldest_queued_at", "claimed"} {
			if _, present := a[field]; present != (string(a["name"]) == `"muse"`) {
				t.Fatalf("field %s: %s", field, rec.Body.String())
			}
		}
	}
	var old struct {
		Agents []struct {
			Name   string
			Online bool
			Wake   string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &old); err != nil || len(old.Agents) != 3 {
		t.Fatalf("old client = %+v, %v", old, err)
	}
	var legacy client.Roster
	if err := json.Unmarshal([]byte(`{"agents":[{"name":"muse","online":true,"wake":"wait"}]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Agents[0].Backlog(time.Now()) != "" {
		t.Fatalf("legacy = %+v", legacy)
	}
}

// Stop, called when the relay shuts down, answers held polls, peeks, and
// get-reply waits at once with their usual "nothing yet" answers, and later
// ones no longer hold.
func TestStopEndsHeldPollsAndWaits(t *testing.T) {
	h := newHarness(t, Config{PollHold: 20 * time.Second, MaxWait: 20 * time.Second})
	sent := h.send(grokAddr, "instinct", "rows?")
	var wg sync.WaitGroup
	var res store.Result
	start := time.Now()
	wg.Go(func() { h.do(museAddr, "GET", "/v1/poll", "", http.StatusNoContent, nil) })
	wg.Go(func() { h.do(museAddr, "GET", "/v1/poll?peek=1", "", http.StatusNoContent, nil) })
	wg.Go(func() { h.do(grokAddr, "GET", "/v1/requests/"+sent.ID+"?wait=20", "", http.StatusOK, &res) })
	time.Sleep(100 * time.Millisecond) // let them start holding
	h.srv.Stop()
	h.srv.Stop() // idempotent
	wg.Wait()
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("held calls answered %v after Stop", took)
	}
	if res.Request.ID != sent.ID || res.Done() {
		t.Fatalf("held wait answered %+v, want the pending request", res)
	}
	start = time.Now()
	h.do(museAddr, "GET", "/v1/poll", "", http.StatusNoContent, nil)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a poll after Stop held for %v", took)
	}
	// Waiting requests are still delivered.
	var got pollResult
	h.do(instinctAddr, "GET", "/v1/poll", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("poll after Stop got %+v", got)
	}
}

// --- retention by recipient kind (KTD2) ---

// ttlHarness is a harness on a fake clock with muse set to the notes kind,
// instinct to history, and grokbot left without a kind.
func ttlHarness(t *testing.T, cfg Config) (*harness, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Now()}
	cfg.Now = clock.Now
	h := newHarness(t, cfg)
	h.st.SetClock(clock.Now)
	h.do(macAddr, "PUT", "/v1/agents/muse/kind", `{"kind":"notes"}`, http.StatusOK, nil)
	h.do(macAddr, "PUT", "/v1/agents/instinct/kind", `{"kind":"history"}`, http.StatusOK, nil)
	return h, clock
}

// ttlOf reads how long a request was given to live when it was last
// (re)queued: expires_at minus updated_at.
func ttlOf(t *testing.T, h *harness, id string) time.Duration {
	t.Helper()
	var exp, upd int64
	if err := h.st.DB().QueryRow(`SELECT expires_at, updated_at FROM requests WHERE id = ?`, id).Scan(&exp, &upd); err != nil {
		t.Fatal(err)
	}
	return time.Duration(exp-upd) * time.Millisecond
}

func statusOf(t *testing.T, h *harness, addr, id string) envelope.Status {
	t.Helper()
	var res envelope.Result
	h.do(addr, "GET", "/v1/requests/"+id, "", http.StatusOK, &res)
	return res.Status
}

func TestRequestTTLChosenByRecipientKind(t *testing.T) {
	h, _ := ttlHarness(t, Config{})
	notes := h.send(grokAddr, "muse", "save this")
	history := h.send(grokAddr, "instinct", "what did I ask")
	unset := h.send(museAddr, "grokbot", "hi")
	if got := ttlOf(t, h, notes.ID); got != 30*24*time.Hour {
		t.Errorf("notes ttl = %v, want 720h", got)
	}
	if got := ttlOf(t, h, history.ID); got != 24*time.Hour {
		t.Errorf("history ttl = %v, want 24h", got)
	}
	if got := ttlOf(t, h, unset.ID); got != 24*time.Hour {
		t.Errorf("unset-kind ttl = %v, want 24h", got)
	}
}

func TestNotesTTLOverride(t *testing.T) {
	h, clock := ttlHarness(t, Config{NotesRequestTTL: 72 * time.Hour})
	notes := h.send(grokAddr, "muse", "save this")
	if got := ttlOf(t, h, notes.ID); got != 72*time.Hour {
		t.Fatalf("notes ttl = %v, want 72h", got)
	}
	clock.advance(71 * time.Hour)
	h.srv.Sweep(context.Background())
	if s := statusOf(t, h, grokAddr, notes.ID); s != envelope.StatusQueued {
		t.Fatalf("at 71h status = %s, want queued", s)
	}
	clock.advance(2 * time.Hour)
	h.srv.Sweep(context.Background())
	if s := statusOf(t, h, grokAddr, notes.ID); s != envelope.StatusExpired {
		t.Fatalf("at 73h status = %s, want expired", s)
	}
}

// A notes request outlives the 24h window: still delivered at day 29,
// requeued when its claim lapses, and expired (as the asker sees it) past 30d.
func TestNotesRequestLifecycleOverThirtyDays(t *testing.T) {
	h, clock := ttlHarness(t, Config{ClaimLease: time.Hour})
	notes := h.send(grokAddr, "muse", "save this")
	history := h.send(grokAddr, "instinct", "what did I ask")
	clock.advance(25 * time.Hour)
	h.srv.Sweep(context.Background())
	if s := statusOf(t, h, grokAddr, history.ID); s != envelope.StatusExpired {
		t.Fatalf("history at 25h = %s, want expired", s)
	}
	clock.advance(28 * 24 * time.Hour) // day 29
	h.srv.Sweep(context.Background())
	var got pollResult
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != notes.ID {
		t.Fatalf("day 29 poll = %+v", got)
	}
	h.do(museAddr, "POST", "/v1/requests/"+notes.ID+"/claim", "", http.StatusOK, nil)
	clock.advance(2 * time.Hour) // claim lease lapses, still inside the TTL
	h.srv.Sweep(context.Background())
	if s := statusOf(t, h, grokAddr, notes.ID); s != envelope.StatusQueued {
		t.Fatalf("after lease lapse = %s, want queued", s)
	}
	clock.advance(2 * 24 * time.Hour) // past day 30
	h.srv.Sweep(context.Background())
	if s := statusOf(t, h, grokAddr, notes.ID); s != envelope.StatusExpired {
		t.Fatalf("past 30d = %s, want expired", s)
	}
}

// lockedBuffer collects log output written from any goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A failed recipient lookup must not cut a notes request to 24h: the relay
// logs the failure and keeps the longer notes window, on send and approve.
func TestRequestTTLLookupErrorKeepsNotesWindow(t *testing.T) {
	h, _ := ttlHarness(t, Config{})
	logs, prev := &lockedBuffer{}, log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	failing := errors.New("directory unavailable")
	h.srv.lookupAgent = func(context.Context, string) (identity.Agent, bool, error) {
		return identity.Agent{}, false, failing
	}
	sent := h.send(grokAddr, "muse", "save this")
	if got := ttlOf(t, h, sent.ID); got != 30*24*time.Hour {
		t.Errorf("send ttl on lookup error = %v, want 720h", got)
	}
	h.srv.SetPreparer(holdAll{})
	held := h.send(grokAddr, "muse", "save that")
	h.do(macAddr, "POST", "/v1/admin/requests/"+held.ID+"/approve", "", http.StatusOK, nil)
	if got := ttlOf(t, h, held.ID); got != 30*24*time.Hour {
		t.Errorf("approve ttl on lookup error = %v, want 720h", got)
	}
	if out := logs.String(); !strings.Contains(out, "muse") || !strings.Contains(out, failing.Error()) {
		t.Errorf("lookup failure not logged: %q", out)
	}
}

// holdAll holds every request for an hour, as the approval gate would.
type holdAll struct{}

func (holdAll) Prepare(_ context.Context, req *envelope.Request) error {
	req.Status, req.HoldTTL = envelope.StatusHeld, time.Hour
	return nil
}

// An approved held request to a notes agent gets the notes window, not 24h.
func TestApprovedHeldNotesRequestKeepsNotesTTL(t *testing.T) {
	h, _ := ttlHarness(t, Config{})
	h.srv.SetPreparer(holdAll{})
	notes := h.send(grokAddr, "muse", "save this")
	history := h.send(grokAddr, "instinct", "what did I ask")
	h.do(macAddr, "POST", "/v1/admin/requests/"+notes.ID+"/approve", "", http.StatusOK, nil)
	h.do(macAddr, "POST", "/v1/admin/requests/"+history.ID+"/approve", "", http.StatusOK, nil)
	if got := ttlOf(t, h, notes.ID); got != 30*24*time.Hour {
		t.Errorf("approved notes ttl = %v, want 720h", got)
	}
	if got := ttlOf(t, h, history.ID); got != 24*time.Hour {
		t.Errorf("approved history ttl = %v, want 24h", got)
	}
}

// scheduleHarness joins fo from strangerAddr on a fake clock and installs a
// real waker with fo on a 5m schedule, instinct on 1m, muse waiting, and
// grokbot on a webhook.
func scheduleHarness(t *testing.T) (*harness, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_790_000_000, 0)}
	h := newHarnessDir(t, Config{Now: clk.Now}, identity.Config{Now: clk.Now})
	h.joinAt(strangerAddr, "fo")
	h.srv.SetWakeNamer(wake.New(wake.Config{
		"fo":       {Method: wake.Schedule, Every: "5m"},
		"instinct": {Method: wake.Schedule, Every: "1m"},
		"muse":     {Method: wake.Wait},
		"grokbot":  {Method: wake.Webhook, URL: "http://127.0.0.1:1/hook"},
	}, nil, wake.Options{}))
	return h, clk
}

// The roster reports a schedule agent's interval, the reply window a sender
// should expect, and whether it has stopped checking its inbox: overdue once
// its last poll (or its join, if it never polled) is more than two intervals
// plus a 5m grace ago. Sending asks does not count as checking.
func TestRosterScheduleFacts(t *testing.T) {
	h, clk := scheduleHarness(t)
	fo := agentInfo(t, h, macAddr, "fo")
	if fo.Wake != wake.Schedule || fo.CheckEverySeconds != 300 || fo.ExpectReplySeconds != 600 || fo.Overdue {
		t.Fatalf("just joined, never polled: %+v", fo)
	}
	if in := agentInfo(t, h, macAddr, "instinct"); in.CheckEverySeconds != 60 || in.ExpectReplySeconds != 360 {
		t.Fatalf("1m schedule: %+v", in)
	}
	clk.advance(30 * time.Minute)
	if fo := agentInfo(t, h, macAddr, "fo"); !fo.Overdue {
		t.Fatalf("never polled, joined 30m ago: want overdue: %+v", fo)
	}
	h.do(strangerAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	clk.advance(3 * time.Minute)
	if fo := agentInfo(t, h, macAddr, "fo"); fo.Overdue {
		t.Fatalf("polled 3m ago: want not overdue: %+v", fo)
	}
	clk.advance(12 * time.Minute) // exactly 2*5m + 5m since the poll
	if fo := agentInfo(t, h, macAddr, "fo"); fo.Overdue {
		t.Fatalf("polled exactly 15m ago: want not overdue yet: %+v", fo)
	}
	clk.advance(9 * time.Minute)
	h.send(strangerAddr, "muse", "still here")
	clk.advance(time.Minute)
	if fo := agentInfo(t, h, macAddr, "fo"); !fo.Overdue {
		t.Fatalf("polled 25m ago, sent 1m ago: want overdue: %+v", fo)
	}
	rec := h.do(macAddr, "GET", "/v1/agents", "", http.StatusOK, nil)
	var raw struct{ Agents []map[string]any }
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, a := range raw.Agents {
		if a["name"] != "muse" && a["name"] != "grokbot" {
			continue
		}
		for _, k := range []string{"check_every_seconds", "expect_reply_seconds", "overdue"} {
			if _, ok := a[k]; ok {
				t.Errorf("%s roster entry has %s: %v", a["name"], k, a)
			}
		}
	}
}

// The send response names the recipient's schedule so the asker can say when
// to expect a reply; a recipient on any other method gets no target.
func TestSendResponseCarriesScheduleTarget(t *testing.T) {
	h, clk := scheduleHarness(t)
	var out struct {
		envelope.Request
		Target *envelope.Target `json:"target"`
	}
	h.do(grokAddr, "POST", "/v1/send", `{"to":"fo","body":"hi"}`, http.StatusCreated, &out)
	if out.ID == "" || out.Target == nil || out.Target.CheckEverySeconds != 300 || out.Target.ExpectReplySeconds != 600 || out.Target.Overdue {
		t.Fatalf("send to schedule agent: %+v target %+v", out.Request, out.Target)
	}
	clk.advance(30 * time.Minute)
	out.Target = nil
	h.do(grokAddr, "POST", "/v1/send", `{"to":"fo","body":"hi again"}`, http.StatusCreated, &out)
	if out.Target == nil || !out.Target.Overdue {
		t.Fatalf("send to overdue schedule agent: target %+v", out.Target)
	}
	for _, to := range []string{"grokbot", "muse"} {
		from := museAddr
		if to == "muse" {
			from = grokAddr
		}
		rec := h.do(from, "POST", "/v1/send", `{"to":"`+to+`","body":"hi"}`, http.StatusCreated, nil)
		if strings.Contains(rec.Body.String(), `"target"`) {
			t.Errorf("send to %s has a target: %s", to, rec.Body.String())
		}
	}
}
