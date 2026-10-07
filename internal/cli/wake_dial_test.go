package cli

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A relay on its own tsnet node, on a host whose tailscaled has no TUN
// device, can only reach tailnet machines through tsnet. Wake webhooks to a
// tailnet address (100.64.0.0/10, the Tailscale IPv6 range, or a *.ts.net
// name) must go through the tailnet dialer; every other address keeps the
// ordinary dialer, so AgentMail and public webhooks are unchanged.
func TestWakeClientDialsTailnetAddressesThroughTsnet(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(ts.Close)
	real := ts.Listener.Addr().String()
	var mu sync.Mutex
	var viaTailnet []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		viaTailnet = append(viaTailnet, addr)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, real) // the fake tailnet reaches the test server
	}
	c := wakeHTTPClient(dial)
	for _, u := range []string{"http://100.80.229.80:8644/hooks", "http://[fd7a:115c:a1e0::1]:8644/", "http://hermes.tail2b6977.ts.net:8644/"} {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTeapot {
			t.Fatalf("GET %s = %d, want the test server's 418", u, resp.StatusCode)
		}
	}
	if len(viaTailnet) != 3 || !strings.HasPrefix(viaTailnet[0], "100.80.229.80:") {
		t.Fatalf("tailnet dials = %v, want all three tailnet URLs", viaTailnet)
	}
	resp, err := c.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET %s: %v", ts.URL, err)
	}
	resp.Body.Close()
	if len(viaTailnet) != 3 {
		t.Fatalf("a non-tailnet URL went through the tailnet dialer: %v", viaTailnet)
	}
}

// The tsnet relay's resolver offers its node's dialer, which is how runRelay
// knows to send tailnet wakes through it.
var _ tailnetDialer = tailnetResolver{}

// Only Tailscale ranges and MagicDNS names count as tailnet hosts.
func TestTailnetHost(t *testing.T) {
	for host, want := range map[string]bool{
		"100.80.229.80": true, "100.64.0.1": true, "100.127.255.255": true, "[fd7a:115c:a1e0::5]": true,
		"hermes.tail2b6977.ts.net": true, "Hermes.Tail2b6977.TS.NET.": true,
		"100.128.0.1": false, "10.0.0.5": false, "api.agentmail.to": false, "example.com": false, "fd00::1": false,
	} {
		if got := tailnetHost(host); got != want {
			t.Errorf("tailnetHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// rtFunc is an http.RoundTripper made from a function.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The route is chosen by the request's destination, never by whatever
// address the transport ends up dialing: a public wake or an AgentMail call
// keeps the relay's original transport (with its proxy, even a proxy at a
// tailnet address), and only a request whose own host is on the tailnet
// goes through the tsnet transport.
func TestWakeRouteFollowsDestination(t *testing.T) {
	var public, tailnet []string
	ok := func(r *http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}
	}
	rt := wakeRoute{
		public: rtFunc(func(r *http.Request) (*http.Response, error) { public = append(public, r.URL.Host); return ok(r), nil }),
		tailnet: rtFunc(func(r *http.Request) (*http.Response, error) {
			tailnet = append(tailnet, r.URL.Host)
			return ok(r), nil
		}),
	}
	c := &http.Client{Transport: rt}
	for _, u := range []string{"https://api.agentmail.to/v0/inboxes", "https://hooks.example.com/wake", "http://100.80.229.80:8644/", "http://hermes.tail2b6977.ts.net/"} {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if strings.Join(public, ",") != "api.agentmail.to,hooks.example.com" {
		t.Fatalf("public transport got %v, want the two public hosts", public)
	}
	if strings.Join(tailnet, ",") != "100.80.229.80:8644,hermes.tail2b6977.ts.net" {
		t.Fatalf("tailnet transport got %v, want the two tailnet hosts", tailnet)
	}
}
