package relay_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// phoneRoutes are every route the phone page loads: the page itself, its
// static files, and the data it reads.
var phoneRoutes = []string{"/phone/", "/phone/app.js", "/phone/style.css", "/phone/manifest.webmanifest", "/phone/sw.js", "/phone/icons/icon-192.png", "/v1/admin/held"}

// sameSite is what a browser adds to a fetch from the relay's own page.
var sameSite = map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}

// browserJSON is what a browser adds to a state-changing fetch from the
// page the relay serves as http://127.0.0.1.
var browserJSON = map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Origin": "http://127.0.0.1", "Content-Type": "application/json"}

// machineAt serves the hardened relay to callers the relay sees at addr,
// which the test registers in the mesh's fake tailnet as node.
func machineAt(t *testing.T, m *testrelay.Mesh, addr string, node identity.Node) string {
	t.Helper()
	m.Who.Set(addr, node)
	h := relay.HardenForBrowsers(m.Server.Handler(), relayNames)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = addr
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// An admin device loads the page and its data; a joined agent, a stranger
// and a tagged node named like an admin get 403 on every phone route,
// static files included.
func TestPhonePageAdminOnly(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	admin := hardened(t, m, "admin")
	code, body, hdr := browserCall(t, "GET", admin, "/phone/", "127.0.0.1", "", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate"})
	if code != http.StatusOK || !strings.Contains(body, "<html") {
		t.Fatalf("admin opening /phone/: %d %.200s", code, body)
	}
	csp := hdr.Get("Content-Security-Policy")
	for _, d := range []string{"script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "default-src 'none'"} {
		if !strings.Contains(csp, d) {
			t.Errorf("page CSP %q lacks %q", csp, d)
		}
	}
	for _, p := range phoneRoutes {
		if code, body, _ := browserCall(t, "GET", admin, p, "127.0.0.1", "", sameSite); code != http.StatusOK {
			t.Errorf("admin GET %s: %d %.200s", p, code, body)
		}
	}
	if code, _, hdr := browserCall(t, "GET", admin, "/phone/sw.js", "127.0.0.1", "", sameSite); code != http.StatusOK || !strings.Contains(hdr.Get("Content-Type"), "javascript") {
		t.Errorf("service worker: %d %s, want a JavaScript type", code, hdr.Get("Content-Type"))
	}
	if code, _, _ := browserCall(t, "GET", admin, "/phone/nope.js", "127.0.0.1", "", sameSite); code != http.StatusNotFound {
		t.Errorf("missing static file: %d, want 404", code)
	}

	tagged := machineAt(t, m, "100.0.0.7:1", identity.Node{ID: "nTAGGED", Name: "macbook-pro-44", User: testrelay.Login, Tags: []string{"tag:agent"}})
	for who, base := range map[string]string{"joined agent": hardened(t, m, "grokbot"), "stranger": hardened(t, m, "stranger"), "tagged admin name": tagged} {
		for _, p := range phoneRoutes {
			if code, _, _ := browserCall(t, "GET", base, p, "127.0.0.1", "", sameSite); code != http.StatusForbidden {
				t.Errorf("%s GET %s: %d, want 403", who, p, code)
			}
		}
		if code, _, _ := browserCall(t, "GET", base, "/phone/", "127.0.0.1", "", map[string]string{"Sec-Fetch-Site": "none"}); code != http.StatusForbidden {
			t.Errorf("%s opening /phone/: %d, want 403", who, code)
		}
	}
}

// heldEntry is one item of the admin held listing as the page reads it.
type heldEntry struct {
	envelope.Request
	ToKind         string    `json:"to_kind"`
	ApproveNonce   string    `json:"approve_nonce"`
	DenyNonce      string    `json:"deny_nonce"`
	NonceExpiresAt time.Time `json:"nonce_expires_at"`
}

// decision is the relay's answer to an approve or deny from the page.
type decision struct {
	ID             string          `json:"id"`
	Status         envelope.Status `json:"status"`
	AlreadyDecided bool            `json:"already_decided"`
	Error          string          `json:"error"`
}

// gatedMesh is a mesh whose approval policy holds every request to muse.
func gatedMesh(t *testing.T, cfg relay.Config) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, cfg)
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	return m
}

func holdOne(t *testing.T, m *testrelay.Mesh) envelope.Request {
	t.Helper()
	req, err := m.Client(t, "grokbot").Send(t.Context(), "muse", "<b>call the bank</b>", envelope.KindAsk, "", false)
	if err != nil || req.Status != envelope.StatusHeld {
		t.Fatalf("send: %+v %v, want held", req, err)
	}
	return req
}

// listHeld reads the held listing from base as the page does.
func listHeld(t *testing.T, base string) []heldEntry {
	t.Helper()
	code, body, _ := browserCall(t, "GET", base, "/v1/admin/held", "127.0.0.1", "", sameSite)
	if code != http.StatusOK {
		t.Fatalf("held: %d %s", code, body)
	}
	var out []heldEntry
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func decide(t *testing.T, base, verb, id, nonce string) (int, decision) {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"nonce": nonce})
	code, body, _ := browserCall(t, "POST", base, "/v1/admin/requests/"+id+"/"+verb, "127.0.0.1", string(in), browserJSON)
	var out decision
	json.Unmarshal([]byte(body), &out)
	return code, out
}

// Approving from the page needs the nonce issued with the held item;
// using it again returns the final state rather than releasing twice, and
// the audit records the device.
func TestPhoneApproveWithNonce(t *testing.T) {
	m := gatedMesh(t, relay.Config{})
	req := holdOne(t, m)
	admin := hardened(t, m, "admin")

	// The CLI's listing over the tailnet carries nonces too (it cannot be
	// told from a plain-http page), but the CLI decides without them.
	var cli []heldEntry
	if err := m.Client(t, "admin").Raw(t.Context(), "GET", "/v1/admin/held", nil, &cli); err != nil {
		t.Fatal(err)
	}
	if len(cli) != 1 || cli[0].ID != req.ID {
		t.Fatalf("the CLI's held listing: %+v", cli)
	}

	items := listHeld(t, admin)
	if len(items) != 1 || items[0].ApproveNonce == "" || items[0].DenyNonce == "" || items[0].ApproveNonce == items[0].DenyNonce || items[0].NonceExpiresAt.IsZero() {
		t.Fatalf("page held listing: %+v", items)
	}
	if items[0].Body != "<b>call the bank</b>" || items[0].To != "muse" || items[0].From != "grokbot" {
		t.Fatalf("held item: %+v", items[0])
	}

	if code, out := decide(t, admin, "approve", req.ID, ""); code != http.StatusConflict || out.AlreadyDecided {
		t.Fatalf("approve with no nonce: %d %+v, want 409", code, out)
	}
	code, out := decide(t, admin, "approve", req.ID, items[0].ApproveNonce)
	if code != http.StatusOK || out.Status != envelope.StatusQueued {
		t.Fatalf("approve: %d %+v", code, out)
	}
	in, err := m.Client(t, "muse").Poll(t.Context(), time.Millisecond)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].ID != req.ID {
		t.Fatalf("muse after approval: %+v %v", in.Requests, err)
	}
	code, out = decide(t, admin, "approve", req.ID, items[0].ApproveNonce)
	if code != http.StatusOK || !out.AlreadyDecided || out.Status == envelope.StatusHeld {
		t.Fatalf("reused nonce: %d %+v, want the final state", code, out)
	}
	events, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	approved := 0
	for _, e := range events {
		if e.Event == "approved" && e.RequestID == req.ID {
			approved++
			if !strings.Contains(e.Detail, `"device":"macbook-pro-44"`) {
				t.Errorf("approved audit detail %q lacks the device", e.Detail)
			}
			if strings.Contains(e.Detail, items[0].ApproveNonce) {
				t.Error("the nonce reached the audit log")
			}
		}
	}
	if approved != 1 {
		t.Fatalf("approved %d times, want once", approved)
	}
}

// Over plain http to a tailnet name a browser sends no Sec-Fetch-* and no
// Origin on a same-origin GET, so the held listing cannot tell it from the
// CLI. It still carries nonces, and the page's approve, which does carry
// Origin, succeeds with one. The local admin socket gets none.
func TestPhoneApproveOverPlainHTTP(t *testing.T) {
	m := gatedMesh(t, relay.Config{})
	req := holdOne(t, m)
	admin := hardened(t, m, "admin")
	const host = "tincan-relay.example.ts.net"

	code, body, _ := browserCall(t, "GET", admin, "/v1/admin/held", host, "", nil)
	if code != http.StatusOK {
		t.Fatalf("held: %d %s", code, body)
	}
	var items []heldEntry
	if err := json.Unmarshal([]byte(body), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ApproveNonce == "" || items[0].DenyNonce == "" {
		t.Fatalf("held listing without browser headers: %+v, want nonces", items)
	}
	in, _ := json.Marshal(map[string]string{"nonce": items[0].ApproveNonce})
	code, body, _ = browserCall(t, "POST", admin, "/v1/admin/requests/"+req.ID+"/approve", host, string(in), map[string]string{"Origin": "http://" + host, "Content-Type": "application/json"})
	var out decision
	json.Unmarshal([]byte(body), &out)
	if code != http.StatusOK || out.Status != envelope.StatusQueued {
		t.Fatalf("approve over plain http: %d %s", code, body)
	}

	holdOne(t, m)
	local := httptest.NewServer(m.Server.AdminHandler())
	t.Cleanup(local.Close)
	code, body, _ = browserCall(t, "GET", local.URL, "/v1/admin/held", "127.0.0.1", "", nil)
	if code != http.StatusOK || strings.Contains(body, "nonce") {
		t.Fatalf("local admin held listing: %d %s, want no nonces", code, body)
	}
}

// A nonce works only for its own decision, on the device it was issued
// to, for 10 minutes.
func TestPhoneNonceBinding(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m := gatedMesh(t, relay.Config{Now: clock})
	req := holdOne(t, m)
	admin := hardened(t, m, "admin")
	other := machineAt(t, m, "100.0.0.8:1", identity.Node{ID: "nMAC2", Name: "macbook-pro-44", User: testrelay.Login})

	items := listHeld(t, admin)
	if code, out := decide(t, admin, "approve", req.ID, items[0].DenyNonce); code != http.StatusConflict {
		t.Fatalf("approve with the deny nonce: %d %+v, want 409", code, out)
	}
	if code, out := decide(t, other, "approve", req.ID, items[0].ApproveNonce); code != http.StatusConflict {
		t.Fatalf("approve from another device: %d %+v, want 409", code, out)
	}
	mu.Lock()
	now = now.Add(11 * time.Minute)
	mu.Unlock()
	if code, out := decide(t, admin, "approve", req.ID, items[0].ApproveNonce); code != http.StatusConflict {
		t.Fatalf("approve with an 11-minute-old nonce: %d %+v, want 409", code, out)
	}
	if _, st, err := m.Store.Request(t.Context(), req.ID); err != nil || st != envelope.StatusHeld {
		t.Fatalf("request after refused approvals: %s %v, want held", st, err)
	}
	fresh := listHeld(t, admin)
	if code, out := decide(t, admin, "deny", req.ID, fresh[0].DenyNonce); code != http.StatusOK || out.Status != envelope.StatusDeclined {
		t.Fatalf("deny with a fresh nonce: %d %+v", code, out)
	}
	if code, out := decide(t, admin, "approve", req.ID, fresh[0].ApproveNonce); code != http.StatusOK || !out.AlreadyDecided || out.Status != envelope.StatusDeclined {
		t.Fatalf("approve after deny: %d %+v, want already decided as declined", code, out)
	}
}

// Approve from the page and deny from the CLI at the same moment end in
// one final state that both callers agree on.
func TestPhoneApproveRacesCLIDeny(t *testing.T) {
	for range 10 {
		m := gatedMesh(t, relay.Config{})
		req := holdOne(t, m)
		admin := hardened(t, m, "admin")
		nonce := listHeld(t, admin)[0].ApproveNonce
		var wg sync.WaitGroup
		var page decision
		var pageCode int
		var cliErr error
		wg.Go(func() { pageCode, page = decide(t, admin, "approve", req.ID, nonce) })
		wg.Go(func() {
			cliErr = m.Client(t, "admin").Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/deny", map[string]string{"reason": "no"}, nil)
		})
		wg.Wait()
		_, final, err := m.Store.Request(t.Context(), req.ID)
		if err != nil {
			t.Fatal(err)
		}
		if pageCode != http.StatusOK {
			t.Fatalf("page approve: %d %+v", pageCode, page)
		}
		switch final {
		case envelope.StatusQueued, envelope.StatusDelivered:
			if page.AlreadyDecided || cliErr == nil {
				t.Fatalf("approved, but page %+v and CLI deny %v", page, cliErr)
			}
		case envelope.StatusDeclined:
			if !page.AlreadyDecided || page.Status != envelope.StatusDeclined || cliErr != nil {
				t.Fatalf("declined, but page %+v and CLI deny %v", page, cliErr)
			}
		default:
			t.Fatalf("final state %s", final)
		}
	}
}
