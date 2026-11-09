package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// projectHeader is spelled out here so these tests pin the wire name.
const projectHeader = "X-Tincan-Project"

// window is one client window of an agent: its address, session and label.
type window struct{ addr, session, project string }

func (w window) header() map[string]string {
	out := map[string]string{}
	if w.session != "" {
		out[client.SessionHeader] = w.session
	}
	if w.project != "" {
		out[projectHeader] = w.project
	}
	return out
}

var (
	museAlpha = window{museAddr, "win-alpha", "alpha"}
	museBeta  = window{museAddr, "win-beta", "beta"}
	museOld   = window{museAddr, "", ""} // an old client: no session, no label
)

// keepAlive makes w call the relay without polling, as keepSession does.
func keepAlive(h *harness, w window) {
	h.t.Helper()
	rec := h.raw(w.addr, "GET", "/v1/whoami", nil, 0, w.header())
	if rec.Code != http.StatusOK {
		h.t.Fatalf("whoami from %+v: status %d: %s", w, rec.Code, rec.Body)
	}
}

// sendProject has grokbot ask muse, naming project.
func sendProject(h *harness, project, body string) envelope.SendResponse {
	h.t.Helper()
	rec := h.raw(grokAddr, "POST", "/v1/send", strings.NewReader(fmt.Sprintf(`{"to":"muse","body":%q,"project":%q}`, body, project)), -1, nil)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("send: status %d: %s", rec.Code, rec.Body)
	}
	var sent envelope.SendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sent); err != nil {
		h.t.Fatal(err)
	}
	return sent
}

// projectMatch reads the send response's project_match from its JSON, so
// these tests hold against the wire and not a Go field.
func projectMatch(t *testing.T, sent envelope.SendResponse) string {
	t.Helper()
	raw, err := json.Marshal(sent.Target)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ProjectMatch string `json:"project_match"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.ProjectMatch
}

// poll takes what w's poll delivers and returns the request ids.
func (w window) poll(h *harness) []string {
	h.t.Helper()
	rec := h.raw(w.addr, "GET", "/v1/poll?hold=0", nil, 0, w.header())
	ids := []string{}
	if rec.Code == http.StatusNoContent {
		return ids
	}
	if rec.Code != http.StatusOK {
		h.t.Fatalf("poll from %+v: status %d: %s", w, rec.Code, rec.Body)
	}
	var got pollResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		h.t.Fatal(err)
	}
	for _, r := range got.Requests {
		ids = append(ids, r.ID)
	}
	return ids
}

// windowPeek is what a peek reports.
type windowPeek struct {
	Waiting   int                `json:"waiting"`
	Queued    int                `json:"queued"`
	Urgent    int                `json:"urgent"`
	Pending   []envelope.Pending `json:"pending"`
	Hidden    int                `json:"hidden"`
	HiddenIDs []string           `json:"hidden_ids"`
	raw       string
}

func (w window) peek(h *harness) windowPeek {
	h.t.Helper()
	rec := h.raw(w.addr, "GET", "/v1/poll?hold=0&peek=1", nil, 0, w.header())
	var got windowPeek
	if rec.Code == http.StatusNoContent {
		return got
	}
	if rec.Code != http.StatusOK {
		h.t.Fatalf("peek from %+v: status %d: %s", w, rec.Code, rec.Body)
	}
	got.raw = rec.Body.String()
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		h.t.Fatal(err)
	}
	return got
}

func (w window) claim(h *harness, id string) int {
	h.t.Helper()
	return h.raw(w.addr, "POST", "/v1/requests/"+id+"/claim", nil, 0, w.header()).Code
}

// projectHarness starts a relay past its just-started window, with both of
// muse's windows live.
func projectHarness(t *testing.T) (*harness, *fakeClock) {
	t.Helper()
	h, clk, _ := sweepHarness(t, Config{})
	clk.advance(SessionGrace)
	keepAlive(h, museAlpha)
	keepAlive(h, museBeta)
	return h, clk
}

// A scoped ask with a matching live session reaches only that session, and
// the sender is told one is live.
func TestProjectAskReachesOnlyMatchingSession(t *testing.T) {
	h, _ := projectHarness(t)
	sent := sendProject(h, "alpha", "fix the build")
	if got := projectMatch(t, sent); got != "live" {
		t.Fatalf("project_match = %q, want live", got)
	}
	if sent.Project != "alpha" {
		t.Fatalf("send response project = %q", sent.Project)
	}
	if got := museBeta.poll(h); len(got) != 0 {
		t.Fatalf("other project's session got %v", got)
	}
	if got := museOld.poll(h); len(got) != 0 {
		t.Fatalf("session with no label got %v", got)
	}
	if got := museAlpha.poll(h); !slices.Equal(got, []string{sent.ID}) {
		t.Fatalf("matching session got %v, want [%s]", got, sent.ID)
	}
}

// With no session of the project, the sender is told, nothing is delivered
// for SessionGrace, and then any session takes it.
func TestProjectAskFallsBackAfterGrace(t *testing.T) {
	h, clk := projectHarness(t)
	sent := sendProject(h, "gamma", "anyone there")
	if got := projectMatch(t, sent); got != "none" {
		t.Fatalf("project_match = %q, want none", got)
	}
	clk.advance(SessionGrace - time.Second)
	if got := museBeta.poll(h); len(got) != 0 {
		t.Fatalf("delivered before grace: %v", got)
	}
	clk.advance(time.Second)
	if got := museBeta.poll(h); !slices.Equal(got, []string{sent.ID}) {
		t.Fatalf("after grace got %v, want [%s]", got, sent.ID)
	}
}

// Fallback goes by age queued, not liveness: a matching window that stays
// open but never polls does not strand the request.
func TestProjectFallbackIgnoresLiveSessionThatNeverPolls(t *testing.T) {
	h, clk := projectHarness(t)
	sent := sendProject(h, "alpha", "are you reading this")
	if got := projectMatch(t, sent); got != "live" {
		t.Fatalf("project_match = %q, want live", got)
	}
	for range 5 {
		clk.advance(2 * time.Minute)
		keepAlive(h, museAlpha)
	}
	if got := museBeta.poll(h); !slices.Equal(got, []string{sent.ID}) {
		t.Fatalf("after grace with a whoami-only match got %v, want [%s]", got, sent.ID)
	}
}

// A peek reports what a poll from the same window would take: counts,
// urgent count and pending ids all leave out work for another project, so a
// wrong window is not nudged, until fallback.
func TestProjectPeekMatchesDelivery(t *testing.T) {
	h, clk := projectHarness(t)
	rec := h.raw(grokAddr, "POST", "/v1/send", strings.NewReader(`{"to":"muse","body":"scoped","project":"alpha","urgent":true}`), -1, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	var scoped envelope.Request
	if err := json.Unmarshal(rec.Body.Bytes(), &scoped); err != nil {
		t.Fatal(err)
	}
	if got := museBeta.peek(h); got.Waiting != 0 || got.Queued != 0 || got.Urgent != 0 || got.Pending != nil || got.Hidden != 1 || len(got.HiddenIDs) != 1 || got.HiddenIDs[0] != scoped.ID {
		t.Fatalf("wrong window's peek = %+v", got)
	}
	open := h.send(grokAddr, "muse", "unscoped")
	got := museBeta.peek(h)
	if got.Queued != 1 || got.Urgent != 0 || len(got.Pending) != 1 || got.Pending[0].ID != open.ID || got.Hidden != 1 || len(got.HiddenIDs) != 1 || got.HiddenIDs[0] != scoped.ID {
		t.Fatalf("wrong window's peek with an open ask = %+v", got)
	}
	got = museAlpha.peek(h)
	if got.Queued != 2 || got.Urgent != 1 || len(got.Pending) != 2 || got.Hidden != 0 || got.HiddenIDs != nil {
		t.Fatalf("matching peek = %+v", got)
	}
	if strings.Contains(got.raw, "alpha") {
		t.Fatalf("peek carries the label: %s", got.raw)
	}
	clk.advance(SessionGrace)
	if got := museBeta.peek(h); got.Queued != 2 || got.Urgent != 1 || len(got.Pending) != 2 || got.Hidden != 0 || got.HiddenIDs != nil {
		t.Fatalf("wrong window's peek after fallback = %+v", got)
	}
	if ids := museBeta.poll(h); len(ids) != 2 || ids[0] != scoped.ID {
		t.Fatalf("poll after fallback = %v", ids)
	}
}

// An old client sends no header: it gets unscoped work at once and scoped
// work only after fallback.
func TestOldClientGetsScopedWorkAfterFallback(t *testing.T) {
	h, clk := projectHarness(t)
	open := h.send(grokAddr, "muse", "for anyone")
	scoped := sendProject(h, "alpha", "for alpha")
	if got := museOld.poll(h); !slices.Equal(got, []string{open.ID}) {
		t.Fatalf("old client got %v, want only [%s]", got, open.ID)
	}
	clk.advance(SessionGrace)
	if got := museOld.poll(h); !slices.Equal(got, []string{scoped.ID}) {
		t.Fatalf("old client after fallback got %v, want [%s]", got, scoped.ID)
	}
}

// An unscoped ask is unchanged for a session that advertises a label.
func TestUnscopedAskReachesAnyLabel(t *testing.T) {
	h, _ := projectHarness(t)
	sent := h.send(grokAddr, "muse", "for anyone")
	if got := museBeta.poll(h); !slices.Equal(got, []string{sent.ID}) {
		t.Fatalf("got %v", got)
	}
}

// Claim by id takes the rule Deliver takes, for a queued request and for
// one already delivered to the matching session, and is open after fallback.
func TestProjectClaimFollowsDelivery(t *testing.T) {
	h, clk := projectHarness(t)
	sent := sendProject(h, "alpha", "mine to claim")
	if got := museBeta.claim(h, sent.ID); got != http.StatusConflict {
		t.Fatalf("wrong window's claim of queued = %d, want 409", got)
	}
	if got := museOld.claim(h, sent.ID); got != http.StatusConflict {
		t.Fatalf("no-label claim of queued = %d, want 409", got)
	}
	if got := museAlpha.poll(h); len(got) != 1 {
		t.Fatalf("matching poll = %v", got)
	}
	if got := museBeta.claim(h, sent.ID); got != http.StatusConflict {
		t.Fatalf("wrong window's claim of delivered = %d, want 409", got)
	}
	if got := museAlpha.claim(h, sent.ID); got != http.StatusOK {
		t.Fatalf("matching claim = %d", got)
	}

	late := sendProject(h, "alpha", "taken late")
	clk.advance(SessionGrace)
	if got := museBeta.claim(h, late.ID); got != http.StatusOK {
		t.Fatalf("claim after fallback = %d", got)
	}
}

// A bad label is refused at send and wherever a header is read to route,
// and the limits themselves are accepted.
func TestProjectLabelValidated(t *testing.T) {
	h, _ := projectHarness(t)
	bad := []string{"Upper Case", "UPPER", "has space", "semi;colon", strings.Repeat("a", 65)}
	for _, label := range bad {
		rec := h.raw(grokAddr, "POST", "/v1/send", strings.NewReader(fmt.Sprintf(`{"to":"muse","body":"x","project":%q}`, label)), -1, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("send with project %q: status %d, want 400", label, rec.Code)
		}
		w := window{museAddr, "s", label}
		if rec := h.raw(museAddr, "GET", "/v1/poll?hold=0", nil, 0, w.header()); rec.Code != http.StatusBadRequest {
			t.Errorf("poll with project %q: status %d, want 400", label, rec.Code)
		}
		if rec := h.raw(museAddr, "POST", "/v1/requests/none/claim", nil, 0, w.header()); rec.Code != http.StatusBadRequest {
			t.Errorf("claim with project %q: status %d, want 400", label, rec.Code)
		}
	}
	sendProject(h, strings.Repeat("a", 64), "longest")
	sendProject(h, "my-repo_2.0", "every allowed character")
	rec := h.raw(grokAddr, "POST", "/v1/send", strings.NewReader(`{"to":"muse","body":"","kind":"ping","project":"alpha"}`), -1, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ping naming a project: status %d, want 400", rec.Code)
	}
}

// Relay-side session activity is memory only, so a relay just started says
// nothing about a label rather than claim none is live. Routing does not
// depend on that memory.
func TestProjectMatchUnknownRightAfterStart(t *testing.T) {
	h, _, _ := sweepHarness(t, Config{})
	sent := sendProject(h, "alpha", "just started")
	if got := projectMatch(t, sent); got != "" {
		t.Fatalf("project_match = %q right after start, want none reported", got)
	}
	if got := museBeta.poll(h); len(got) != 0 {
		t.Fatalf("delivered to the wrong window: %v", got)
	}
}

// A label is live only while its session keeps calling.
func TestProjectLabelLapsesAfterGrace(t *testing.T) {
	h, clk := projectHarness(t)
	clk.advance(SessionGrace)
	if got := projectMatch(t, sendProject(h, "alpha", "gone")); got != "none" {
		t.Fatalf("project_match = %q for a window quiet past grace, want none", got)
	}
}

// The clock runs only while a request is queued: time held for the owner
// does not count, so an approved ask still goes to its project first.
func TestHeldProjectAskKeepsItsGraceAfterApproval(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{})
	clk.advance(SessionGrace)
	h.srv.SetPreparer(holdAll{})
	sent := sendProject(h, "alpha", "needs approval")
	if sent.Status != envelope.StatusHeld {
		t.Fatalf("status = %s, want held", sent.Status)
	}
	if got := projectMatch(t, sent); got != "" {
		t.Fatalf("held send's project_match = %q, want none: no session can take it yet", got)
	}
	clk.advance(30 * time.Minute)
	h.do(macAddr, "POST", "/v1/admin/requests/"+sent.ID+"/approve", "", http.StatusOK, nil)
	if got := museBeta.poll(h); len(got) != 0 {
		t.Fatalf("delivered to the wrong window after approval: %v", got)
	}
	if got := museAlpha.poll(h); !slices.Equal(got, []string{sent.ID}) {
		t.Fatalf("matching poll = %v", got)
	}
}

// A target reads no label on a request the owner never released.
func TestDeniedProjectAskHidesLabel(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{})
	clk.advance(SessionGrace)
	h.srv.SetPreparer(holdAll{})
	sent := sendProject(h, "alpha", "needs approval")
	h.do(macAddr, "POST", "/v1/admin/requests/"+sent.ID+"/deny", `{"reason":"no"}`, http.StatusOK, nil)
	rec := h.raw(museAddr, "GET", "/v1/requests/"+sent.ID, nil, 0, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "alpha") {
		t.Fatalf("target read the label: %s", rec.Body)
	}
}

// An agent woken by the relay polls when the ask arrives and is handed
// nothing, so the relay wakes it again, once, when the ask falls back.
func TestProjectFallbackWakesTarget(t *testing.T) {
	h, clk := projectHarness(t)
	rec := &requeueRecorder{}
	h.srv.SetEvents(rec)
	woken := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.requeued)
	}
	sent := sendProject(h, "alpha", "for alpha")
	if got := museOld.poll(h); len(got) != 0 {
		t.Fatalf("poll during the grace = %v", got)
	}
	h.srv.Sweep(t.Context())
	if n := woken(); n != 0 {
		t.Fatalf("wakes during the grace = %d", n)
	}
	clk.advance(SessionGrace)
	h.srv.Sweep(t.Context())
	h.srv.Sweep(t.Context())
	if n := woken(); n != 1 {
		t.Fatalf("wakes at fallback = %d, want 1", n)
	}
	if got := museOld.poll(h); !slices.Equal(got, []string{sent.ID}) {
		t.Fatalf("poll after fallback = %v", got)
	}
}

// A request email is a delivery, so a scoped ask is not mailed until it
// falls back. The reply window still sees it as open.
func TestRequestEmailWaitsForFallback(t *testing.T) {
	h, clk := projectHarness(t)
	open := h.send(grokAddr, "muse", "for anyone")
	scoped := sendProject(h, "alpha", "for alpha")
	asks, err := h.srv.OpenAsks("muse")
	if err != nil || len(asks) != 1 || asks[0].ID != open.ID {
		t.Fatalf("open asks before fallback = %+v %v", asks, err)
	}
	if ok, err := h.srv.emailWindowOpen(t.Context(), "muse"); !ok || err != nil {
		t.Fatalf("email window = %v %v", ok, err)
	}
	h.do(museAddr, "POST", "/v1/requests/"+open.ID+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", "/v1/requests/"+open.ID+"/reply", `{"body":"done"}`, http.StatusOK, nil)
	if ok, err := h.srv.emailWindowOpen(t.Context(), "muse"); !ok || err != nil {
		t.Fatalf("email window with only the scoped ask = %v %v", ok, err)
	}
	clk.advance(SessionGrace)
	asks, err = h.srv.OpenAsks("muse")
	if err != nil || len(asks) != 1 || asks[0].ID != scoped.ID {
		t.Fatalf("open asks after fallback = %+v %v", asks, err)
	}
}

// A label names a session of the target it was sent to, so failover drops
// it: the new target takes the ask at once.
func TestFailoverDropsProject(t *testing.T) {
	h, clk := failoverHarness(t)
	limitAt(h, clk, grokAddr, 90*time.Minute)
	var sent envelope.SendResponse
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"grokbot","body":"summarize this paper","project":"alpha"}`, http.StatusCreated, &sent)
	if sent.To != "muse" || sent.Project != "" {
		t.Fatalf("send response = %+v", sent.Request)
	}
	if got := projectMatch(t, sent); got != "" {
		t.Fatalf("project_match after a move = %q", got)
	}
	if got := museBeta.poll(h); !slices.Equal(got, []string{sent.ID}) {
		t.Fatalf("new target's poll = %v, want [%s]", got, sent.ID)
	}
}

// The label routes delivery and does nothing else: it does not change who
// the sender is, or open an admin route. Rate limits are in project_limits_test.go.
func TestProjectLabelIsNotIdentity(t *testing.T) {
	h, _, _ := sweepHarness(t, Config{})
	as := map[string]string{projectHeader: "muse"}
	rec := h.raw(grokAddr, "POST", "/v1/send", strings.NewReader(`{"to":"instinct","body":"who am I"}`), -1, as)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	var sent envelope.Request
	if err := json.Unmarshal(rec.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.From != "grokbot" {
		t.Fatalf("from = %q", sent.From)
	}
	if rec := h.raw(grokAddr, "GET", "/v1/admin/held", nil, 0, map[string]string{projectHeader: "admin"}); rec.Code != http.StatusForbidden {
		t.Fatalf("admin route with a label: status %d, want 403", rec.Code)
	}
}
