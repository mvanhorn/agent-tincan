package relay_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// joinAnswer is any answer from the join request routes.
type joinAnswer struct {
	ID             string                    `json:"id"`
	Status         string                    `json:"status"`
	Code           string                    `json:"code"`
	Agents         []identity.RequestedAgent `json:"agents"`
	ExpiresAt      time.Time                 `json:"expires_at"`
	AlreadyDecided bool                      `json:"already_decided"`
	Name           string                    `json:"name"`
	Error          string                    `json:"error"`
}

// joinCall sends one request to base and decodes the answer, which must be
// JSON whatever the status.
func joinCall(t *testing.T, method, base, path, body string, hdr map[string]string) (int, joinAnswer) {
	t.Helper()
	code, raw, h := browserCall(t, method, base, path, "127.0.0.1", body, hdr)
	if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s %s: %d %q with Content-Type %q, want JSON", method, path, code, raw, ct)
	}
	var a joinAnswer
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("%s %s: %d %q: %v", method, path, code, raw, err)
	}
	return code, a
}

// fileJoin files a join request from base for entries, each name or
// name:kind.
func fileJoin(t *testing.T, base string, entries ...string) (int, joinAnswer) {
	t.Helper()
	var agents []identity.RequestedAgent
	for _, e := range entries {
		name, kind, _ := strings.Cut(e, ":")
		agents = append(agents, identity.RequestedAgent{Name: name, Kind: kind})
	}
	body, _ := json.Marshal(map[string]any{"agents": agents})
	return joinCall(t, "POST", base, "/v1/join-requests", string(body), nil)
}

// mustFile files and fails the test unless the request is pending.
func mustFile(t *testing.T, base string, entries ...string) joinAnswer {
	t.Helper()
	code, a := fileJoin(t, base, entries...)
	if code != http.StatusOK || a.Status != identity.JoinPending || a.ID == "" {
		t.Fatalf("file %v: %d %+v", entries, code, a)
	}
	return a
}

func joinStatus(t *testing.T, base, id string) (int, joinAnswer) {
	t.Helper()
	return joinCall(t, "GET", base, "/v1/join-requests/"+id, "", nil)
}

func joinClaim(t *testing.T, base, id string) (int, joinAnswer) {
	t.Helper()
	return joinCall(t, "POST", base, "/v1/join-requests/"+id+"/claim", "", nil)
}

// socketDecide approves or denies id over the relay's admin socket.
func socketDecide(t *testing.T, m *testrelay.Mesh, id, decision string) (int, joinAnswer) {
	t.Helper()
	return joinCall(t, "POST", adminSocket(t, m), "/v1/admin/join-requests/"+id+"/"+decision, "", nil)
}

func mustApprove(t *testing.T, m *testrelay.Mesh, id string) {
	t.Helper()
	if code, a := socketDecide(t, m, id, "approve"); code != http.StatusOK || a.Status != identity.JoinApproved {
		t.Fatalf("approve %s: %d %+v", id, code, a)
	}
}

// Covers R1. An unjoined machine files two names and gets an id, a code
// shaped XXX-XXX and pending; the audit names the node and the names but
// never the code.
func TestJoinRequestFiled(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex", "claude-code")
	if !regexp.MustCompile(`^[A-HJ-NP-Z2-9]{3}-[A-HJ-NP-Z2-9]{3}$`).MatchString(a.Code) {
		t.Fatalf("code %q, want XXX-XXX", a.Code)
	}
	if len(a.Agents) != 2 || a.Agents[0] != (identity.RequestedAgent{Name: "codex"}) || a.Agents[1].Name != "claude-code" {
		t.Fatalf("agents %+v", a.Agents)
	}
	if a.ExpiresAt.IsZero() {
		t.Fatal("no expiry")
	}
	rows := auditEvents(t, m, "join_requested")
	if len(rows) != 1 || !strings.Contains(rows[0].Detail, "nLAPTOP") || !strings.Contains(rows[0].Detail, "claude-code") || strings.Contains(rows[0].Detail, a.Code) {
		t.Fatalf("join_requested audit %+v", rows)
	}
	if code, st := joinStatus(t, m.URL("stranger"), a.ID); code != http.StatusOK || st.Status != identity.JoinPending || st.Code != a.Code {
		t.Fatalf("status: %d %+v", code, st)
	}
}

// Covers R2. A name that is an agent, has a live invite, or is held by
// another request answers 409 naming it.
func TestJoinRequestNamesMustBeNew(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Invite(t, "invited")
	other := m.AddMachine(t, "other", testrelay.TailnetNode("nOTHER", "other-box", testrelay.Login))
	mustFile(t, other, "held")
	for _, name := range []string{"grokbot", "invited", "held"} {
		code, a := fileJoin(t, m.URL("stranger"), "fresh", name)
		if code != http.StatusConflict || !strings.Contains(a.Error, name) {
			t.Errorf("file %s: %d %+v, want 409 naming it", name, code, a)
		}
	}
}

// Covers R4 and KTD4. Shared-in machines, machines outside the relay's
// tailnet or with no MagicDNS name, machines carrying a guest and virtual
// callers are refused; a relay that does not know its own tailnet refuses
// everyone.
func TestJoinRequestRefusedNodes(t *testing.T) {
	m := testrelay.NewWithAdminLogin(t, relay.Config{})
	cases := map[string]identity.Node{
		"shared-in flag": {ID: "nS1", Name: "shared", User: testrelay.Login, Key: "nodekey:nS1", DNSName: "shared." + testrelay.Tailnet, SharedIn: true},
		"other tailnet":  {ID: "nS2", Name: "elsewhere", User: testrelay.Login, Key: "nodekey:nS2", DNSName: "elsewhere.other.ts.net"},
		"suffix only":    {ID: "nS3", Name: "sneaky", User: testrelay.Login, Key: "nodekey:nS3", DNSName: "sneaky.x" + testrelay.Tailnet},
		"no dns name":    {ID: "nS4", Name: "nameless", User: testrelay.Login, Key: "nodekey:nS4"},
	}
	for what, n := range cases {
		code, a := fileJoin(t, m.AddMachine(t, what, n), "codex")
		if code != http.StatusForbidden || a.Error == "" {
			t.Errorf("%s: %d %+v, want 403", what, code, a)
		}
	}
	if code, a := fileJoin(t, m.URL("visitor"), "codex"); code != http.StatusForbidden {
		t.Errorf("visitor: %d %+v, want 403", code, a)
	}

	// A machine carrying a guest.
	m.JoinGuest(t, "pal", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 86400})
	m.Who.Set(m.Addr("pal"), testrelay.TailnetNode("nVISIT", "friends-laptop", testrelay.GuestLogin))
	if code, a := fileJoin(t, m.URL("pal"), "codex"); code != http.StatusForbidden {
		t.Errorf("guest node: %d %+v, want 403", code, a)
	}

	// A virtual caller from the gateway.
	virtual := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = identity.VirtualAddr("chatgpt")
		m.Server.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(virtual.Close)
	if code, a := fileJoin(t, virtual.URL, "codex"); code != http.StatusForbidden {
		t.Errorf("virtual caller: %d %+v, want 403", code, a)
	}

	// Fail closed when the relay's own name is unknown.
	m.Server.SetSelfNode(identity.Node{ID: "nRELAY", Name: "tincan-relay"})
	if code, a := fileJoin(t, m.URL("stranger"), "codex"); code != http.StatusForbidden {
		t.Errorf("relay without a MagicDNS name: %d %+v, want 403", code, a)
	}
	if rows := auditEvents(t, m, "join_requested"); len(rows) != 0 {
		t.Fatalf("refused filings audited as requested: %+v", rows)
	}
}

// Covers R4. A tagged machine may file.
func TestJoinRequestTaggedNodeFiles(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	n := testrelay.TailnetNode("nTAG", "ci-runner", "tagged-devices")
	n.Tags = []string{"tag:ci"}
	mustFile(t, m.AddMachine(t, "tagged", n), "codex")
}

// Kinds: any known kind but proxy-sandbox.
func TestJoinRequestKinds(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	for _, e := range []string{"box:proxy-sandbox", "box:no-such-kind"} {
		if code, a := fileJoin(t, m.URL("stranger"), e); code != http.StatusBadRequest {
			t.Errorf("%s: %d %+v, want 400", e, code, a)
		}
	}
	a := mustFile(t, m.URL("stranger"), "box:codex")
	if a.Agents[0].Kind != "codex" {
		t.Fatalf("agents %+v", a.Agents)
	}
}

// A rebuilt machine that rebind would re-admit is sent to tincan rejoin,
// and filing does not re-admit it on the way.
func TestJoinRequestRebuiltMachineToldToRejoin(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	url := m.Rebuild(t, "grokbot", "grok-bot")
	code, a := fileJoin(t, url, "codex")
	if code != http.StatusConflict || !strings.Contains(a.Error, "tincan rejoin") || !strings.Contains(a.Error, "grokbot") {
		t.Fatalf("rebuilt machine: %d %+v, want 409 naming tincan rejoin", code, a)
	}
	if g, _, _ := m.Dir.Agent(t.Context(), "grokbot"); g.NodeID != "nGROK" {
		t.Fatalf("filing re-admitted grokbot: %+v", g)
	}
}

// Covers R3. One open request per node, five per hour per node, ten
// pending in total; each answers 429.
func TestJoinRequestCaps(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	base := m.URL("stranger")
	first := mustFile(t, base, "one")
	if code, a := fileJoin(t, base, "two"); code != http.StatusTooManyRequests || a.Error == "" {
		t.Fatalf("second open request: %d %+v, want 429", code, a)
	}
	socketDecide(t, m, first.ID, "deny")
	for i := range 4 {
		a := mustFile(t, base, "n"+string(rune('a'+i)))
		socketDecide(t, m, a.ID, "deny")
	}
	if code, a := fileJoin(t, base, "sixth"); code != http.StatusTooManyRequests || !strings.Contains(a.Error, "hour") {
		t.Fatalf("sixth in an hour: %d %+v, want 429", code, a)
	}

	for i := range 10 {
		id := "nP" + string(rune('a'+i))
		mustFile(t, m.AddMachine(t, id, testrelay.TailnetNode(id, "box-"+id, testrelay.Login)), "p"+string(rune('a'+i)))
	}
	if code, a := fileJoin(t, m.AddMachine(t, "eleventh", testrelay.TailnetNode("nP11", "box-11", testrelay.Login)), "p11"); code != http.StatusTooManyRequests {
		t.Fatalf("eleventh pending: %d %+v, want 429", code, a)
	}
}

// Covers R8. Another machine cannot read or claim the request, and the
// request is unchanged; an unknown id is a JSON 404.
func TestJoinRequestOtherNodeRefused(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	if code, st := joinStatus(t, m.URL("grokbot"), a.ID); code != http.StatusForbidden || st.Code != "" || st.Status != "" {
		t.Fatalf("status from another node: %d %+v, want a bare 403", code, st)
	}
	mustApprove(t, m, a.ID)
	if code, st := joinClaim(t, m.URL("muse"), a.ID); code != http.StatusForbidden {
		t.Fatalf("claim from another node: %d %+v, want 403", code, st)
	}
	if code, st := joinStatus(t, m.URL("stranger"), a.ID); code != http.StatusOK || st.Status != identity.JoinApproved {
		t.Fatalf("status after: %d %+v, want approved", code, st)
	}
	for _, path := range []string{"/v1/join-requests/nope", "/v1/join-requests/nope/claim"} {
		method := "GET"
		if strings.HasSuffix(path, "claim") {
			method = "POST"
		}
		if code, st := joinCall(t, method, m.URL("stranger"), path, "", nil); code != http.StatusNotFound || st.Error == "" {
			t.Fatalf("%s: %d %+v, want JSON 404", path, code, st)
		}
	}
}

// Covers R9. A claim before approval binds nothing; after approval the
// recorded node gets every name once, with a joined audit row each, and a
// second claim answers delivered with no agents.
func TestJoinRequestClaim(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	base := m.URL("stranger")
	a := mustFile(t, base, "codex", "helper:hermes")
	if code, st := joinClaim(t, base, a.ID); code != http.StatusOK || st.Status != identity.JoinPending || len(st.Agents) != 0 {
		t.Fatalf("claim before approval: %d %+v", code, st)
	}
	if ok, _ := m.Dir.Has(t.Context(), "codex"); ok {
		t.Fatal("claim before approval bound codex")
	}
	mustApprove(t, m, a.ID)
	code, st := joinClaim(t, base, a.ID)
	if code != http.StatusOK || st.Status != identity.JoinDelivered || len(st.Agents) != 2 || st.Agents[1] != (identity.RequestedAgent{Name: "helper", Kind: "hermes"}) {
		t.Fatalf("claim: %d %+v", code, st)
	}
	for _, name := range []string{"codex", "helper"} {
		ag, ok, _ := m.Dir.Agent(t.Context(), name)
		if !ok || ag.NodeID != "nLAPTOP" {
			t.Fatalf("%s after claim: %+v %v", name, ag, ok)
		}
	}
	if h, _, _ := m.Dir.Agent(t.Context(), "helper"); h.Kind != "hermes" {
		t.Fatalf("helper kind %q", h.Kind)
	}
	joined := auditEvents(t, m, "joined")
	var names []string
	for _, e := range joined {
		if strings.Contains(e.Detail, a.ID) {
			names = append(names, e.Actor)
		}
	}
	if strings.Join(names, ",") != "codex,helper" {
		t.Fatalf("joined audit actors %v", names)
	}
	if code, st := joinClaim(t, base, a.ID); code != http.StatusOK || st.Status != identity.JoinDelivered || len(st.Agents) != 0 {
		t.Fatalf("second claim: %d %+v, want delivered with no agents", code, st)
	}
}

// Covers R8. A claim from a rebuilt machine (a new node id) answers 403 and
// leaves the request approved; the recorded node with a new key voids it.
func TestJoinRequestClaimBoundToNodeAndKey(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	mustApprove(t, m, a.ID)
	rebuilt := m.Rebuild(t, "stranger", "old-laptop")
	if code, st := joinClaim(t, rebuilt, a.ID); code != http.StatusForbidden {
		t.Fatalf("claim from rebuilt node: %d %+v, want 403", code, st)
	}
	rekeyed := testrelay.TailnetNode("nLAPTOP", "old-laptop", testrelay.Login)
	rekeyed.Key = "nodekey:new"
	url := m.AddMachine(t, "rekeyed", rekeyed)
	if code, st := joinClaim(t, url, a.ID); code != http.StatusOK || st.Status != identity.JoinVoided {
		t.Fatalf("claim with a new key: %d %+v, want voided", code, st)
	}
	if ok, _ := m.Dir.Has(t.Context(), "codex"); ok {
		t.Fatal("a voided claim bound codex")
	}
	if rows := auditEvents(t, m, "join_voided"); len(rows) != 1 || !strings.Contains(rows[0].Detail, a.ID) {
		t.Fatalf("join_voided audit %+v", rows)
	}
}

// Covers R8. The recorded node reading status with a changed key voids a
// pending request.
func TestJoinRequestStatusWithNewKeyVoids(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	rekeyed := testrelay.TailnetNode("nLAPTOP", "old-laptop", testrelay.Login)
	rekeyed.Key = "nodekey:new"
	if code, st := joinStatus(t, m.AddMachine(t, "rekeyed", rekeyed), a.ID); code != http.StatusOK || st.Status != identity.JoinVoided {
		t.Fatalf("status with a new key: %d %+v, want voided", code, st)
	}
	if code, st := socketDecide(t, m, a.ID, "approve"); code != http.StatusConflict || st.Status != identity.JoinVoided {
		t.Fatalf("approve after void: %d %+v, want 409 voided", code, st)
	}
}

// An approved, unclaimed request still holds its names against invites
// and other requests.
func TestJoinRequestApprovedHoldsNames(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	mustApprove(t, m, a.ID)
	if _, err := m.Client(t, "admin").Invite(t.Context(), "codex"); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("invite for a held name: %v, want 409", err)
	}
	other := m.AddMachine(t, "other", testrelay.TailnetNode("nOTHER", "other-box", testrelay.Login))
	if code, st := fileJoin(t, other, "codex"); code != http.StatusConflict {
		t.Fatalf("request for a held name: %d %+v, want 409", code, st)
	}
}

// Two machines filing for the same name at once: exactly one is stored.
func TestJoinRequestConcurrentFilings(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	var bases []string
	for i := range 6 {
		id := "nC" + string(rune('a'+i))
		bases = append(bases, m.AddMachine(t, id, testrelay.TailnetNode(id, "box-"+id, testrelay.Login)))
	}
	var ok, taken atomic.Int32
	var wg sync.WaitGroup
	for _, b := range bases {
		wg.Go(func() {
			switch code, _ := fileJoin(t, b, "racer"); code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusConflict:
				taken.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 1 || taken.Load() != int32(len(bases)-1) {
		t.Fatalf("stored %d, refused %d; want exactly one stored", ok.Load(), taken.Load())
	}
}

// Filing and minting an invite for the same name at once: exactly one of
// them succeeds, and the name ends up bound at most once.
func TestJoinRequestRacesInvite(t *testing.T) {
	for range 5 {
		m := testrelay.New(t, relay.Config{})
		var inviteCode string
		var filed joinAnswer
		var fileStatus int
		var wg sync.WaitGroup
		wg.Go(func() { inviteCode, _ = m.Client(t, "admin").Invite(t.Context(), "racer") })
		wg.Go(func() { fileStatus, filed = fileJoin(t, m.URL("stranger"), "racer") })
		wg.Wait()
		if inviteCode != "" && fileStatus == http.StatusOK {
			t.Fatal("both the invite and the request took the name")
		}
		bound := 0
		if inviteCode != "" {
			taker := m.AddMachine(t, "taker", testrelay.TailnetNode("nTAKER", "taker", testrelay.Login))
			r, err := client.NewRelayFor(client.Config{Relay: taker})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Join(t.Context(), inviteCode); err == nil {
				bound++
			}
		}
		if fileStatus == http.StatusOK {
			if code, _ := socketDecide(t, m, filed.ID, "approve"); code == http.StatusOK {
				if _, st := joinClaim(t, m.URL("stranger"), filed.ID); st.Status == identity.JoinDelivered {
					bound++
				}
			}
		}
		if bound > 1 {
			t.Fatalf("racer bound %d times", bound)
		}
		if inviteCode == "" && fileStatus != http.StatusOK {
			t.Fatal("both the invite and the request were refused")
		}
	}
}

// Two claims at once: exactly one delivers the agents.
func TestJoinRequestConcurrentClaims(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	mustApprove(t, m, a.ID)
	var withAgents atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			code, st := joinClaim(t, m.URL("stranger"), a.ID)
			if code != http.StatusOK || st.Status != identity.JoinDelivered {
				t.Errorf("claim: %d %+v", code, st)
			}
			if len(st.Agents) > 0 {
				withAgents.Add(1)
			}
		})
	}
	wg.Wait()
	if withAgents.Load() != 1 {
		t.Fatalf("%d claims delivered agents, want 1", withAgents.Load())
	}
	if rows := auditEvents(t, m, "joined"); len(rows) != 4 { // three mesh agents and codex
		t.Fatalf("joined rows %d, want 4", len(rows))
	}
}

// Covers R9 and KTD11. A pending or an approved request expires 10 minutes
// after filing; the sweep audits join_expired for each and a waiting status
// read returns.
func TestJoinRequestExpires(t *testing.T) {
	clk := &movingClock{t: time.Now()}
	m := testrelay.New(t, relay.Config{Now: clk.Now})
	pending := mustFile(t, m.URL("stranger"), "codex")
	other := m.AddMachine(t, "other", testrelay.TailnetNode("nOTHER", "other-box", testrelay.Login))
	approved := mustFile(t, other, "helper")
	mustApprove(t, m, approved.ID)

	clk.Add(identity.JoinRequestTTL + time.Second)
	m.Server.Sweep(t.Context())
	rows := auditEvents(t, m, "join_expired")
	if len(rows) != 2 {
		t.Fatalf("join_expired rows %+v, want 2", rows)
	}
	for _, r := range rows {
		if strings.Contains(r.Detail, pending.Code) || strings.Contains(r.Detail, approved.Code) {
			t.Fatalf("join_expired carries the code: %s", r.Detail)
		}
	}
	if code, st := joinStatus(t, m.URL("stranger"), pending.ID); code != http.StatusOK || st.Status != identity.JoinExpired || st.Code != "" {
		t.Fatalf("status after expiry: %d %+v", code, st)
	}
	if code, st := joinClaim(t, other, approved.ID); code != http.StatusOK || st.Status != identity.JoinExpired {
		t.Fatalf("claim after expiry: %d %+v", code, st)
	}
}

// A status read with wait returns when the owner decides.
func TestJoinRequestStatusWaits(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	done := make(chan joinAnswer, 1)
	go func() {
		_, st := joinCall(t, "GET", m.URL("stranger"), "/v1/join-requests/"+a.ID+"?wait=20", "", nil)
		done <- st
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	mustApprove(t, m, a.ID)
	select {
	case st := <-done:
		if st.Status != identity.JoinApproved {
			t.Fatalf("waiting status got %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("status still waiting %v after approval", time.Since(start))
	}
}

// A relay restart keeps pending and approved requests: a new server over
// the same store reads them.
func TestJoinRequestSurvivesRestart(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	pending := mustFile(t, m.URL("stranger"), "codex")
	other := m.AddMachine(t, "other", testrelay.TailnetNode("nOTHER", "other-box", testrelay.Login))
	approved := mustFile(t, other, "helper")
	mustApprove(t, m, approved.ID)

	again := relay.New(m.Dir, m.Store, relay.Config{TaskBriefing: "off"})
	again.SetSelfNode(testrelay.RelayNode)
	serveAs := func(machine string) string {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = m.Addr(machine)
			again.Handler().ServeHTTP(w, r)
		}))
		t.Cleanup(ts.Close)
		return ts.URL
	}
	if _, st := joinStatus(t, serveAs("stranger"), pending.ID); st.Status != identity.JoinPending {
		t.Fatalf("pending after restart: %+v", st)
	}
	if _, st := joinClaim(t, serveAs("other"), approved.ID); st.Status != identity.JoinDelivered || len(st.Agents) != 1 {
		t.Fatalf("claim after restart: %+v", st)
	}
}

// movingClock is a clock tests move forward by hand.
type movingClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *movingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *movingClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
