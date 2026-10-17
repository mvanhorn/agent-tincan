package relay_test

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// relayNames are the relay's own names in these tests: its MagicDNS short
// and full names, its tailnet IPs, and the loopback address httptest serves
// on, which stands in for the --listen address.
var relayNames = []string{"tincan-relay", "tincan-relay.example.ts.net", "100.64.0.1", "fd7a:115c:a1e0::1", "127.0.0.1"}

// hardened serves the mesh's relay API behind the browser hardening, as the
// relay's tailnet listener does, to callers the relay sees as machine.
func hardened(t *testing.T, m *testrelay.Mesh, machine string) string {
	t.Helper()
	h := relay.HardenForBrowsers(m.Server.Handler(), relayNames)
	addr := m.Addr(machine)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = addr
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// browserCall sends one request to base with Host host and the given
// headers, and returns the status and body.
func browserCall(t *testing.T, method, base, path, host, body string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), resp.Header
}

// Covers AE6: a hostile page open on an admin device cannot remove an agent.
func TestBrowserCrossOriginAdminRemoveRefused(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	base := hardened(t, m, "admin")
	for _, hdr := range []map[string]string{
		{"Origin": "https://evil.example", "Content-Type": "application/json"},
		{"Origin": "https://evil.example", "Content-Type": "text/plain"},
		{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"},
		{"Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain"},
		{"Origin": "null", "Content-Type": "application/json"},
	} {
		code, body, _ := browserCall(t, "POST", base, "/v1/admin/remove", "tincan-relay", `{"name":"muse"}`, hdr)
		if code != http.StatusForbidden {
			t.Fatalf("cross-origin remove with %v: %d %s, want 403", hdr, code, body)
		}
	}
	if _, ok, err := m.Dir.Agent(t.Context(), "muse"); err != nil || !ok {
		t.Fatalf("muse after cross-origin removes: found %v, %v; want still joined", ok, err)
	}
}

// A DNS-rebinding page reaches the relay under the attacker's own name and
// sends no Origin or Sec-Fetch headers; the Host check alone refuses it.
func TestBrowserRebindingHostRefused(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	base := hardened(t, m, "admin")
	for _, host := range []string{"evil.example", "evil.example:80", "tincan-relay.evil.example", "localhost", "100.64.0.2"} {
		code, body, hdr := browserCall(t, "GET", base, "/v1/admin/held", host, "", nil)
		if code != http.StatusForbidden || strings.Contains(body, "[") {
			t.Fatalf("held via Host %q: %d %s, want 403 with no list", host, code, body)
		}
		if hdr.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("refusal for Host %q lacks X-Frame-Options: %v", host, hdr)
		}
	}
	for _, host := range []string{"tincan-relay", "TINCAN-RELAY.example.ts.net.", "100.64.0.1:80", "[fd7a:115c:a1e0::1]:80", "tincan-relay:8080"} {
		if code, body, _ := browserCall(t, "GET", base, "/v1/admin/held", host, "", nil); code != http.StatusOK {
			t.Fatalf("held via the relay's own Host %q: %d %s, want 200", host, code, body)
		}
	}
}

// A same-origin text/plain POST (what a form can send without a preflight)
// never reaches a state-changing route.
func TestBrowserStateChangeNeedsJSON(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	base := hardened(t, m, "grokbot")
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		hdr := map[string]string{"Sec-Fetch-Site": "same-origin", "X-Tincan-Agent": "grokbot"}
		if ct != "" {
			hdr["Content-Type"] = ct
		}
		code, body, _ := browserCall(t, "POST", base, "/v1/send", "tincan-relay", `{"to":"muse","body":"hi"}`, hdr)
		if code != http.StatusForbidden {
			t.Fatalf("same-origin %q send: %d %s, want 403", ct, code, body)
		}
	}
	in, err := m.Client(t, "muse").Poll(t.Context(), 0)
	if err != nil || len(in.Requests) != 0 {
		t.Fatalf("muse inbox after refused sends: %+v %v", in.Requests, err)
	}
}

// A same-origin JSON POST from the relay's own name passes the hardening
// and meets the relay's own admin check: allowed from an admin device,
// refused by that check (not the hardening) from any other machine.
func TestBrowserSameOriginJSONReachesAdminCheck(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	for _, hdr := range []map[string]string{
		{"Origin": "http://tincan-relay", "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"},
		{"Origin": "http://tincan-relay", "Content-Type": "application/json; charset=utf-8"},
		{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"},
	} {
		code, body, _ := browserCall(t, "POST", hardened(t, m, "admin"), "/v1/admin/invite", "tincan-relay", `{"name":"newbie"}`, hdr)
		if code != http.StatusOK || !strings.Contains(body, "code") {
			t.Fatalf("same-origin invite from the admin device with %v: %d %s", hdr, code, body)
		}
		code, body, _ = browserCall(t, "POST", hardened(t, m, "stranger"), "/v1/admin/invite", "tincan-relay", `{"name":"newbie"}`, hdr)
		if code != http.StatusForbidden || !strings.Contains(body, identity.ErrNotAdmin.Error()) {
			t.Fatalf("same-origin invite from a stranger with %v: %d %s, want the admin check's 403", hdr, code, body)
		}
	}
}

// Origin and Sec-Fetch-Site must say same-origin whenever a browser sends
// them, on reads too. The one exception is Sec-Fetch-Site: none (a typed
// address or home-screen icon) on GET /phone/.
func TestBrowserOriginRules(t *testing.T) {
	reached := false
	h := relay.HardenForBrowsers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}), relayNames)
	cases := []struct {
		name, method, path, host string
		hdr                      map[string]string
		tls                      bool
		ok                       bool
	}{
		{"no browser headers", "GET", "/v1/poll", "tincan-relay", nil, false, true},
		{"phone opened from home screen", "GET", "/phone/", "tincan-relay", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate"}, false, true},
		{"phone without the slash from home screen", "GET", "/phone", "tincan-relay", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate"}, false, true},
		{"phone same-origin", "GET", "/phone/", "tincan-relay.example.ts.net", map[string]string{"Sec-Fetch-Site": "same-origin"}, false, true},
		{"same-origin read", "GET", "/v1/admin/held", "tincan-relay", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://tincan-relay"}, false, true},
		{"same-origin over TLS", "POST", "/v1/send", "tincan-relay.example.ts.net", map[string]string{"Origin": "https://tincan-relay.example.ts.net", "Content-Type": "application/json"}, true, true},
		{"same-origin by IP and port", "POST", "/v1/send", "100.64.0.1:8080", map[string]string{"Origin": "http://100.64.0.1:8080", "Content-Type": "application/json"}, false, true},

		{"none on another read", "GET", "/v1/admin/held", "tincan-relay", map[string]string{"Sec-Fetch-Site": "none"}, false, false},
		{"none on a phone POST", "POST", "/phone/", "tincan-relay", map[string]string{"Sec-Fetch-Site": "none", "Content-Type": "application/json"}, false, false},
		{"none on a phone POST without the slash", "POST", "/phone", "tincan-relay", map[string]string{"Sec-Fetch-Site": "none", "Content-Type": "application/json"}, false, false},
		{"none on a deeper phone path", "GET", "/phone/x", "tincan-relay", map[string]string{"Sec-Fetch-Site": "none"}, false, false},
		{"phone with a rebound Host", "GET", "/phone/", "evil.example", map[string]string{"Sec-Fetch-Site": "none"}, false, false},
		{"cross-site read", "GET", "/v1/admin/held", "tincan-relay", map[string]string{"Sec-Fetch-Site": "cross-site"}, false, false},
		{"same-site read", "GET", "/v1/admin/held", "tincan-relay", map[string]string{"Sec-Fetch-Site": "same-site"}, false, false},
		{"cross-origin read", "GET", "/v1/trace", "tincan-relay", map[string]string{"Origin": "https://evil.example"}, false, false},
		{"other relay name as origin", "POST", "/v1/send", "tincan-relay", map[string]string{"Origin": "http://100.64.0.1", "Content-Type": "application/json"}, false, false},
		{"other port as origin", "POST", "/v1/send", "tincan-relay", map[string]string{"Origin": "http://tincan-relay:8080", "Content-Type": "application/json"}, false, false},
		{"other scheme as origin", "POST", "/v1/send", "tincan-relay", map[string]string{"Origin": "https://tincan-relay", "Content-Type": "application/json"}, false, false},
		{"null origin", "POST", "/v1/send", "tincan-relay", map[string]string{"Origin": "null", "Content-Type": "application/json"}, false, false},
		{"cross-origin preflight", "OPTIONS", "/v1/send", "tincan-relay", map[string]string{"Origin": "https://evil.example"}, false, false},
		{"same-origin PUT without JSON", "PUT", "/v1/agents/self/working-on", "tincan-relay", map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "text/plain"}, false, false},
		{"same-origin DELETE without a type", "DELETE", "/v1/agents/self/working-on", "tincan-relay", map[string]string{"Origin": "http://tincan-relay"}, false, false},
		{"empty Host", "GET", "/v1/poll", "", nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached = false
			r := httptest.NewRequest(c.method, "http://placeholder"+c.path, strings.NewReader("{}"))
			r.Host = c.host
			if c.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for k, v := range c.hdr {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if reached != c.ok {
				t.Fatalf("reached the relay: %v, want %v (status %d %s)", reached, c.ok, w.Code, w.Body)
			}
			if !c.ok && w.Code != http.StatusForbidden {
				t.Fatalf("refusal status %d, want 403", w.Code)
			}
			if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Fatalf("X-Frame-Options %q, want DENY", got)
			}
			if got := w.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
				t.Fatalf("Content-Security-Policy %q, want frame-ancestors 'none'", got)
			}
		})
	}
}

// The Go client sends no browser headers and names the relay by an address
// the relay advertises, so the hardening never gets in its way.
func TestBrowserHardeningLeavesGoClientAlone(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	asker, err := client.NewRelayFor(client.Config{Relay: hardened(t, m, "grokbot"), Agent: "grokbot"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := client.NewRelayFor(client.Config{Relay: hardened(t, m, "muse"), Agent: "muse"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := asker.Send(t.Context(), "muse", "are you there?", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	in, err := worker.Poll(t.Context(), 0)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].ID != req.ID {
		t.Fatalf("worker poll: %+v %v", in.Requests, err)
	}
	if _, err := worker.Reply(t.Context(), req.ID, "here", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	res, err := asker.Get(t.Context(), req.ID, 0)
	if err != nil || res.Reply == nil || res.Reply.Body != "here" {
		t.Fatalf("asker get: %+v %v", res, err)
	}
}
