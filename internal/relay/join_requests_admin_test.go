package relay_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// joinListing is one entry of the owner's join request listing.
type joinListing struct {
	ID             string                    `json:"id"`
	Code           string                    `json:"code"`
	NodeName       string                    `json:"node_name"`
	NodeUser       string                    `json:"node_user"`
	Tags           []string                  `json:"tags"`
	Addrs          []string                  `json:"addrs"`
	Agents         []identity.RequestedAgent `json:"agents"`
	NodeAgents     []string                  `json:"node_agents"`
	FiledAt        time.Time                 `json:"filed_at"`
	ExpiresAt      time.Time                 `json:"expires_at"`
	AgeSeconds     *int                      `json:"age_seconds"`
	ApproveNonce   string                    `json:"approve_nonce"`
	DenyNonce      string                    `json:"deny_nonce"`
	NonceExpiresAt time.Time                 `json:"nonce_expires_at"`
}

// listJoins reads the listing from base with hdr, as the page or the CLI.
func listJoins(t *testing.T, base string, hdr map[string]string) (int, []joinListing) {
	t.Helper()
	code, raw, _ := browserCall(t, "GET", base, "/v1/admin/join-requests", "127.0.0.1", "", hdr)
	if code != http.StatusOK {
		return code, nil
	}
	var out struct {
		Requests []joinListing `json:"requests"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("listing %q: %v", raw, err)
	}
	return code, out.Requests
}

// listingFor is the one listing entry for id.
func listingFor(t *testing.T, base, id string) joinListing {
	t.Helper()
	code, all := listJoins(t, base, sameSite)
	if code != http.StatusOK {
		t.Fatalf("listing: %d", code)
	}
	for _, e := range all {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("listing has no %s: %+v", id, all)
	return joinListing{}
}

// pageDecide decides id from the page at base with nonce.
func pageDecide(t *testing.T, base, id, decision, nonce string) (int, joinAnswer) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"nonce": nonce})
	return joinCall(t, "POST", base, "/v1/admin/join-requests/"+id+"/"+decision, string(body), browserJSON)
}

// Covers R5. The listing shows what the owner needs to judge a request and
// one nonce per decision to a tailnet admin, none over the socket, and is
// refused to other callers.
func TestJoinListing(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("grokbot"), "codex", "helper:hermes")
	page := hardened(t, m, "admin")
	e := listingFor(t, page, a.ID)
	if e.Code != a.Code || e.NodeName != "grok-bot" || e.NodeUser != testrelay.Login || len(e.Agents) != 2 || e.Agents[1].Kind != "hermes" ||
		len(e.NodeAgents) != 1 || e.NodeAgents[0] != "grokbot" || e.FiledAt.IsZero() || e.ExpiresAt.IsZero() || e.AgeSeconds == nil {
		t.Fatalf("listing entry %+v", e)
	}
	if e.ApproveNonce == "" || e.DenyNonce == "" || e.ApproveNonce == e.DenyNonce || e.NonceExpiresAt.IsZero() {
		t.Fatalf("listing nonces %+v", e)
	}
	_, sock := listJoins(t, adminSocket(t, m), nil)
	if len(sock) != 1 || sock[0].ApproveNonce != "" || sock[0].Code != a.Code {
		t.Fatalf("socket listing %+v", sock)
	}
	if code, _ := listJoins(t, m.URL("muse"), nil); code != http.StatusForbidden {
		t.Fatalf("listing for an agent: %d, want 403", code)
	}
	if code, _ := listJoins(t, m.URL("stranger"), nil); code != http.StatusForbidden {
		t.Fatalf("listing for an unjoined machine: %d, want 403", code)
	}
	// A decided request leaves the listing, code and all.
	socketDecide(t, m, a.ID, "deny")
	if _, all := listJoins(t, page, sameSite); len(all) != 0 {
		t.Fatalf("listing after deny %+v", all)
	}
}

// Covers R6. The socket approves and denies; a deny is final.
func TestJoinDecideOverSocket(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	mustApprove(t, m, a.ID)
	other := m.AddMachine(t, "other", testrelay.TailnetNode("nOTHER", "other-box", testrelay.Login))
	b := mustFile(t, other, "helper")
	if code, st := socketDecide(t, m, b.ID, "deny"); code != http.StatusOK || st.Status != identity.JoinDenied {
		t.Fatalf("deny: %d %+v", code, st)
	}
	if code, st := socketDecide(t, m, b.ID, "approve"); code != http.StatusConflict || st.Status != identity.JoinDenied {
		t.Fatalf("approve after deny: %d %+v, want 409 denied", code, st)
	}
	if code, st := joinStatus(t, other, b.ID); st.Status != identity.JoinDenied {
		t.Fatalf("status after deny: %d %+v", code, st)
	}
	if code, st := socketDecide(t, m, "nope", "approve"); code != http.StatusNotFound || st.Error == "" {
		t.Fatalf("unknown id: %d %+v, want JSON 404", code, st)
	}
}

// Covers R6 and KTD2. A tailnet admin call that is not browser-marked is
// refused, even with a nonce.
func TestJoinDecideTailnetCLIRefused(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	e := listingFor(t, hardened(t, m, "admin"), a.ID)
	body, _ := json.Marshal(map[string]string{"nonce": e.ApproveNonce})
	code, st := joinCall(t, "POST", m.URL("admin"), "/v1/admin/join-requests/"+a.ID+"/approve", string(body), map[string]string{"Content-Type": "application/json"})
	if code != http.StatusForbidden {
		t.Fatalf("tailnet CLI approve: %d %+v, want 403", code, st)
	}
	if _, st := joinStatus(t, m.URL("stranger"), a.ID); st.Status != identity.JoinPending {
		t.Fatalf("status %+v, want pending", st)
	}
}

// Covers R6. The page approves with a valid nonce; a reused nonce answers
// the decided state; a wrong-decision or another device's nonce answers 409
// and changes nothing.
func TestJoinDecideFromPage(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	page := hardened(t, m, "admin")
	e := listingFor(t, page, a.ID)

	if code, st := pageDecide(t, page, a.ID, "approve", e.DenyNonce); code != http.StatusConflict {
		t.Fatalf("approve with the deny nonce: %d %+v, want 409", code, st)
	}
	twin := machineAt(t, m, "100.0.3.1:1", identity.Node{ID: "nMAC2", Name: "macbook-pro-44", User: testrelay.Login, DNSName: "macbook-pro-44." + testrelay.Tailnet})
	if code, st := pageDecide(t, twin, a.ID, "approve", e.ApproveNonce); code != http.StatusConflict {
		t.Fatalf("approve with another device's nonce: %d %+v, want 409", code, st)
	}
	if code, st := pageDecide(t, page, a.ID, "approve", ""); code != http.StatusConflict {
		t.Fatalf("approve with no nonce: %d %+v, want 409", code, st)
	}
	if m.Server.JoinNoncesLive(a.ID) == 0 {
		t.Fatal("the listing left no live nonces")
	}
	if code, st := pageDecide(t, page, a.ID, "approve", e.ApproveNonce); code != http.StatusOK || st.Status != identity.JoinApproved || st.AlreadyDecided {
		t.Fatalf("page approve: %d %+v", code, st)
	}
	if n := m.Server.JoinNoncesLive(a.ID); n != 0 {
		t.Fatalf("%d nonces live after the decision, want all voided", n)
	}
	if code, st := pageDecide(t, page, a.ID, "approve", e.ApproveNonce); code != http.StatusOK || !st.AlreadyDecided || st.Status != identity.JoinApproved {
		t.Fatalf("reused nonce: %d %+v, want already decided", code, st)
	}
	// The deny nonce from the same listing was voided by the decision.
	if code, st := pageDecide(t, page, a.ID, "deny", e.DenyNonce); code != http.StatusOK || !st.AlreadyDecided || st.Status != identity.JoinApproved {
		t.Fatalf("deny after approve: %d %+v, want already decided approved", code, st)
	}
	rows := auditEvents(t, m, "join_approved")
	if len(rows) != 1 || !strings.Contains(rows[0].Detail, "macbook-pro-44") || !strings.Contains(rows[0].Detail, "page") ||
		!strings.Contains(rows[0].Detail, "codex") || !strings.Contains(rows[0].Detail, "nLAPTOP") || strings.Contains(rows[0].Detail, a.Code) || strings.Contains(rows[0].Detail, e.ApproveNonce) {
		t.Fatalf("join_approved audit %+v", rows)
	}
}

// A held request's nonce cannot decide a join request with the same id,
// nor the other way round: join nonces live under join:<id>.
func TestJoinNoncesAreNamespaced(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	page := hardened(t, m, "admin")
	e := listingFor(t, page, a.ID)
	body, _ := json.Marshal(map[string]string{"nonce": e.ApproveNonce})
	code, raw, _ := browserCall(t, "POST", page, "/v1/admin/requests/"+a.ID+"/approve", "127.0.0.1", string(body), browserJSON)
	if code == http.StatusOK {
		t.Fatalf("a join nonce decided a held request: %d %s", code, raw)
	}
	if code, st := pageDecide(t, page, a.ID, "approve", e.ApproveNonce); code != http.StatusOK || st.Status != identity.JoinApproved {
		t.Fatalf("join nonce after the held attempt: %d %+v", code, st)
	}
}

// Covers R6. An admin device that filed the request cannot approve it,
// even with a nonce the listing gave it.
func TestJoinDecideRequestingNodeRefused(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("admin"), "codex")
	page := hardened(t, m, "admin")
	e := listingFor(t, page, a.ID)
	for _, d := range [][2]string{{"approve", e.ApproveNonce}, {"deny", e.DenyNonce}} {
		if code, st := pageDecide(t, page, a.ID, d[0], d[1]); code != http.StatusForbidden {
			t.Fatalf("%s from the requesting node: %d %+v, want 403", d[0], code, st)
		}
	}
	if _, st := joinStatus(t, m.URL("admin"), a.ID); st.Status != identity.JoinPending {
		t.Fatalf("status %+v, want pending", st)
	}
	mustApprove(t, m, a.ID)
}

// A request filed from the relay host's own node is not approved over the
// socket on that host; the page on another device still can.
func TestJoinDecideRelayHostOverSocketRefused(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	self := testrelay.TailnetNode("nLAPTOP", "old-laptop", testrelay.Login)
	m.Server.SetSelfNode(self)
	a := mustFile(t, m.URL("stranger"), "codex")
	for _, d := range []string{"approve", "deny"} {
		if code, st := socketDecide(t, m, a.ID, d); code != http.StatusForbidden {
			t.Fatalf("socket %s of the relay host's request: %d %+v, want 403", d, code, st)
		}
	}
	page := hardened(t, m, "admin")
	e := listingFor(t, page, a.ID)
	if code, st := pageDecide(t, page, a.ID, "approve", e.ApproveNonce); code != http.StatusOK {
		t.Fatalf("page approve: %d %+v", code, st)
	}
}

// A relay that does not know its own node, as after a restart whose
// self-node read failed, cannot tell a request from its own host, so the
// socket decides nothing; the page on another device still can.
func TestJoinDecideOverSocketRefusedWithoutSelfNode(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	m.Server.SetSelfNode(identity.Node{})
	for _, d := range []string{"approve", "deny"} {
		if code, st := socketDecide(t, m, a.ID, d); code != http.StatusForbidden {
			t.Fatalf("socket %s with no self node: %d %+v, want 403", d, code, st)
		}
	}
	page := hardened(t, m, "admin")
	e := listingFor(t, page, a.ID)
	if code, st := pageDecide(t, page, a.ID, "approve", e.ApproveNonce); code != http.StatusOK {
		t.Fatalf("page approve: %d %+v", code, st)
	}
}

// Approve and deny at once: exactly one lands.
func TestJoinDecideRace(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, d := range []string{"approve", "deny"} {
		wg.Go(func() { codes[i], _ = socketDecide(t, m, a.ID, d) })
	}
	wg.Wait()
	if (codes[0] == http.StatusOK) == (codes[1] == http.StatusOK) {
		t.Fatalf("approve %d, deny %d: want exactly one to land", codes[0], codes[1])
	}
	if n := len(auditEvents(t, m, "join_approved")) + len(auditEvents(t, m, "join_denied")); n != 1 {
		t.Fatalf("%d decision audit rows, want 1", n)
	}
}

// Covers R11. A deny over the socket is audited with the node, the names
// and where it was decided, never the code.
func TestJoinDenyAudit(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	a := mustFile(t, m.URL("stranger"), "codex")
	socketDecide(t, m, a.ID, "deny")
	rows := auditEvents(t, m, "join_denied")
	if len(rows) != 1 || !strings.Contains(rows[0].Detail, "socket") || !strings.Contains(rows[0].Detail, "codex") ||
		!strings.Contains(rows[0].Detail, "old-laptop") || strings.Contains(rows[0].Detail, a.Code) || rows[0].Actor != identity.LocalAdmin {
		t.Fatalf("join_denied audit %+v", rows)
	}
}

// Covers R12 and KTD7. A new request pushes count plus the join flag and
// nothing from the requester.
func TestPushOnJoinRequest(t *testing.T) {
	m := pushMesh(t, 2*time.Second)
	svc := newPushService(t, http.StatusCreated)
	k := newBrowserKeys(t)
	if code, body := subscribe(t, m, svc.srv.URL+"/push/one", k.json()); code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	mustFile(t, m.URL("stranger"), "codex")
	svc.waitFor(t, 1)
	payload := k.decrypt(t, svc.bodies[0])
	if strings.Contains(payload, "codex") || strings.Contains(payload, "old-laptop") {
		t.Fatalf("push carries requester text: %s", payload)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(payload), &msg); err != nil || len(msg) != 2 || msg["count"] != float64(1) || msg["join"] != float64(1) {
		t.Fatalf("push payload %s, want count and join only", payload)
	}
}

// KTD7. With a held ask waiting too, the round sends today's held payload.
func TestPushHeldWinsOverJoin(t *testing.T) {
	m := pushMesh(t, 2*time.Second)
	svc := newPushService(t, http.StatusCreated)
	k := newBrowserKeys(t)
	if code, body := subscribe(t, m, svc.srv.URL+"/push/one", k.json()); code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	mustFile(t, m.URL("stranger"), "codex")
	svc.waitFor(t, 1)
	holdOne(t, m)
	svc.waitFor(t, 2)
	var msg map[string]any
	if err := json.Unmarshal([]byte(k.decrypt(t, svc.bodies[1])), &msg); err != nil || len(msg) != 1 || msg["count"] != float64(1) {
		t.Fatalf("held push %v %v, want only the held count", msg, err)
	}
}
