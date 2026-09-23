package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
	"github.com/mvanhorn/agent-tincan/internal/store"
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
