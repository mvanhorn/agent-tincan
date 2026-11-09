package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func stubTailnet(t *testing.T, selves ...client.TailnetSelf) {
	t.Helper()
	old := tailnetSelves
	tailnetSelves = func(context.Context) []client.TailnetSelf { return selves }
	t.Cleanup(func() { tailnetSelves = old })
}

func tailnetSelf(version string, ssh bool, tags ...string) client.TailnetSelf {
	return client.TailnetSelf{Version: version, Running: true, Tags: tags, SSH: &ssh}
}

// Release builds carry their commit hashes in the long version, so a hash
// suffix is still a release. Anything else cannot be trusted to hold the fix.
func TestTailnetCheckVersion(t *testing.T) {
	for _, tc := range []struct {
		version, status, want string
	}{
		{"1.104.3", "warn", "older than the minimum 1.104.4"},
		{"1.104.3-tabc1234-gdef5678", "warn", "Tailscale 1.104.3 is older"},
		{"1.102.9", "warn", "older"},
		{"1.9.99", "warn", "older"},
		{"0.200.0", "warn", "older"},
		{"1.104.4", "ok", "Tailscale 1.104.4"},
		{"1.104.4-tabc1234-gdef5678", "ok", "Tailscale 1.104.4"},
		{"1.104.4-tabc1234", "ok", "Tailscale 1.104.4"},
		{"1.110.0", "ok", "Tailscale 1.110.0"},
		{"2.0.0", "ok", "Tailscale 2.0.0"},
		{"1.104.4-pre", "warn", "unknown"},
		{"1.103.50-tabc1234-gdef5678", "warn", "Tailscale 1.103.50 is older"},
		{"1.105.12-tabc1234-gdef5678", "warn", "unknown"},
		{"1.105.12", "warn", "unknown"},
		{"1.106.0-pre", "warn", "unknown"},
		{"1.106.0-dev20261030-tabc1234", "warn", "unknown"},
		{"1.106.0-dev20261030-tabc1234-dirty", "warn", "unknown"},
		{"1.106.0-7-tabc1234-gdef5678", "warn", "unknown"},
		{"1.106.0-ERR-BuildInfo", "warn", "unknown"},
		{"1.104", "warn", "unknown"},
		{"banana", "warn", "unknown"},
		{"", "warn", "unknown"},
		{"99999999999999999999.0.0", "warn", "unknown"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			c := tailnetCheck(tailnetSelf(tc.version, false, "tag:agent"), roleAgent)
			if c.Name != "tailnet" || c.Status != tc.status || !strings.Contains(c.Detail, tc.want) {
				t.Fatalf("got %+v, want %s containing %q", c, tc.status, tc.want)
			}
		})
	}
}

// Tailscale SSH is reported either way, and on an old version the fix says
// why it matters here.
func TestTailnetCheckSSH(t *testing.T) {
	c := tailnetCheck(tailnetSelf("1.104.3", true, "tag:agent"), roleAgent)
	if c.Status != "warn" || !strings.Contains(c.Detail, "Tailscale SSH on") {
		t.Fatalf("old with SSH: %+v", c)
	}
	for _, want := range []string{"1.104.4", "TS-2026-014 to TS-2026-017", "Tailscale SSH is on here"} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix lacks %q: %s", want, c.Fix)
		}
	}

	c = tailnetCheck(tailnetSelf("1.104.3", false, "tag:agent"), roleAgent)
	if !strings.Contains(c.Detail, "Tailscale SSH off") || strings.Contains(c.Fix, "Tailscale SSH is on here") {
		t.Fatalf("old without SSH: %+v", c)
	}

	c = tailnetCheck(tailnetSelf("1.104.4", true, "tag:agent"), roleAgent)
	if c.Status != "ok" || c.Fix != "" || !strings.Contains(c.Detail, "Tailscale SSH on") {
		t.Fatalf("current with SSH: %+v", c)
	}

	s := tailnetSelf("1.104.4", false, "tag:agent")
	s.SSH = nil
	c = tailnetCheck(s, roleAgent)
	if c.Status != "ok" || !strings.Contains(c.Detail, "Tailscale SSH unknown") {
		t.Fatalf("prefs unreadable: %+v", c)
	}
}

// Only a machine the relay refused as an admin is warned about tags: admin
// devices are untagged by design, and an unknown role proves nothing.
func TestTailnetCheckTags(t *testing.T) {
	stopped := tailnetSelf("1.104.4", false)
	stopped.Running = false
	for _, tc := range []struct {
		name         string
		self         client.TailnetSelf
		role         deviceRole
		status, want string
		fix          string
	}{
		{"untagged agent", tailnetSelf("1.104.4", false), roleAgent, "warn", "this agent machine is untagged", "tag:agent"},
		{"untagged admin", tailnetSelf("1.104.4", false), roleAdmin, "ok", "admin device", ""},
		{"untagged, role unknown", tailnetSelf("1.104.4", false), roleUnknown, "ok", "tag check skipped", ""},
		{"tagged agent", tailnetSelf("1.104.4", false, "tag:agent", "tag:ci"), roleAgent, "ok", "tagged tag:agent, tag:ci", ""},
		{"not running", stopped, roleAgent, "ok", "tags unknown", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tailnetCheck(tc.self, tc.role)
			if c.Status != tc.status || !strings.Contains(c.Detail, tc.want) {
				t.Fatalf("got %+v, want %s containing %q", c, tc.status, tc.want)
			}
			if tc.fix == "" && c.Fix != "" {
				t.Fatalf("unexpected fix: %+v", c)
			}
			if !strings.Contains(c.Fix, tc.fix) {
				t.Fatalf("fix lacks %q: %+v", tc.fix, c)
			}
		})
	}
}

// The check warns at worst, with every problem named in one line.
func TestTailnetCheckNeverFails(t *testing.T) {
	c := tailnetCheck(tailnetSelf("1.104.3", true), roleAgent)
	if c.Status != "warn" {
		t.Fatalf("got %+v", c)
	}
	for _, want := range []string{"Update Tailscale", "tag:agent"} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix lacks %q: %s", want, c.Fix)
		}
	}
}

func TestTailnetChecksNoTailscale(t *testing.T) {
	stubTailnet(t)
	got := tailnetChecks(t.Context(), nil, false)
	if len(got) != 1 || got[0].Name != "tailnet" || got[0].Status != "ok" || !strings.HasPrefix(got[0].Detail, "skipped:") || got[0].Fix != "" {
		t.Fatalf("got %+v", got)
	}
}

// A host can run one tailscaled per tailnet; each gets its own line.
func TestTailnetChecksEachDaemon(t *testing.T) {
	second := tailnetSelf("1.104.3", false, "tag:agent")
	second.Socket = "/run/other/tailscaled.sock"
	stubTailnet(t, tailnetSelf("1.104.4", false, "tag:agent"), second)
	got := tailnetChecks(t.Context(), nil, false)
	if len(got) != 2 || got[0].Status != "ok" || got[1].Status != "warn" || !strings.Contains(got[1].Detail, second.Socket) {
		t.Fatalf("got %+v", got)
	}
}

// The relay says who this machine is. 403 is its refusal; an error or a
// relay without the route says nothing, so no tag warning follows.
func TestTailnetChecksRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		status string
		want   string
	}{
		{"admin", 200, "ok", "admin device"},
		{"agent", 403, "warn", "untagged"},
		{"old relay", 404, "ok", "tag check skipped"},
		{"relay error", 500, "ok", "tag check skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubTailnet(t, tailnetSelf("1.104.4", false))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/admin/held" {
					t.Errorf("unexpected route %s", r.URL.Path)
				}
				w.WriteHeader(tc.code)
				if tc.code == 200 {
					fmt.Fprint(w, `[]`)
				} else {
					fmt.Fprint(w, `{"error":"no"}`)
				}
			}))
			defer srv.Close()
			r, err := client.NewRelayFor(client.Config{Relay: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			got := tailnetChecks(t.Context(), r, false)
			if len(got) != 1 || got[0].Status != tc.status || !strings.Contains(got[0].Detail, tc.want) {
				t.Fatalf("got %+v, want %s containing %q", got, tc.status, tc.want)
			}
		})
	}
}

// A guest's machine is on its own account's tailnet, so the owner's tag rule
// has nothing to say to it, though the relay refuses it the admin route as it
// does any non-admin. The tag advice stays out of the report, and a real
// problem on the machine still warns.
func TestDoctorGuestIsNotToldToTag(t *testing.T) {
	m := testrelay.NewWithAdminLogin(t, relay.Config{})
	m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	useConfig(t, client.Config{Relay: m.URL("friend"), Agent: "friend"})

	tailnet := func() check {
		t.Helper()
		for _, c := range runDoctor(t.Context(), "", nil).Checks {
			if c.Name == "tailnet" {
				return c
			}
		}
		t.Fatal("no tailnet check")
		return check{}
	}
	stubTailnet(t, tailnetSelf("1.104.4", false))
	if c := tailnet(); c.Status != "ok" || strings.Contains(c.Fix, "tag") || !strings.Contains(c.Detail, "guest") {
		t.Fatalf("got %+v", c)
	}
	stubTailnet(t, tailnetSelf("1.104.3", false))
	if c := tailnet(); c.Status != "warn" || strings.Contains(c.Fix, "tag:agent") {
		t.Fatalf("got %+v", c)
	}
}

// The relay answers for the one node that reached it, and with several
// tailscaleds running that could be any of them, so it is not asked and no
// line takes its word.
func TestTailnetChecksSeveralDaemonsSkipRole(t *testing.T) {
	for _, code := range []int{200, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			second := tailnetSelf("1.104.4", false)
			second.Socket = "/run/other/tailscaled.sock"
			stubTailnet(t, tailnetSelf("1.104.4", false), second)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected relay call: %s", r.URL.Path)
				w.WriteHeader(code)
				fmt.Fprint(w, `[]`)
			}))
			defer srv.Close()
			r, err := client.NewRelayFor(client.Config{Relay: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			got := tailnetChecks(t.Context(), r, false)
			if len(got) != 2 {
				t.Fatalf("got %+v", got)
			}
			for _, c := range got {
				if c.Status != "ok" || c.Fix != "" || !strings.Contains(c.Detail, "tag check skipped") {
					t.Errorf("got %+v", c)
				}
			}
		})
	}
}

// A stopped tailscaled cannot be how this machine reached the relay, so the
// one running daemon still gets the relay's answer.
func TestTailnetChecksRoleForTheRunningDaemon(t *testing.T) {
	stopped := tailnetSelf("1.104.4", false)
	stopped.Running = false
	stopped.Socket = "/run/other/tailscaled.sock"
	stubTailnet(t, stopped, tailnetSelf("1.104.4", false))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":"no"}`)
	}))
	defer srv.Close()
	r, err := client.NewRelayFor(client.Config{Relay: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got := tailnetChecks(t.Context(), r, false)
	if len(got) != 2 || got[0].Status != "ok" || !strings.Contains(got[0].Detail, "tags unknown") ||
		got[1].Status != "warn" || !strings.Contains(got[1].Detail, "untagged") {
		t.Fatalf("got %+v", got)
	}
}

// Through a remote proxy the relay answers for the proxy's node, so it is not
// asked and no line takes its word.
func TestTailnetChecksRemoteProxySkipsRole(t *testing.T) {
	stubTailnet(t, tailnetSelf("1.104.4", false))
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Errorf("unexpected relay call: %s", req.URL.Path)
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"error":"no"}`)), Request: req}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	r, err := client.NewRelayFor(client.Config{Relay: "http://tincan-relay", Proxy: "http://hatch-egress-proxy:3130"})
	if err != nil {
		t.Fatal(err)
	}
	got := tailnetChecks(t.Context(), r, false)
	if len(got) != 1 || got[0].Status != "ok" || !strings.Contains(got[0].Detail, "tag check skipped") {
		t.Fatalf("got %+v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// A userspace tailscaled's proxy on loopback is this machine's own node, so
// the relay's answer still applies.
func TestTailnetChecksLoopbackProxyKeepsRole(t *testing.T) {
	stubTailnet(t, tailnetSelf("1.104.4", false))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":"no"}`)
	}))
	defer proxy.Close()
	r, err := client.NewRelayFor(client.Config{Relay: "http://tincan-relay", Proxy: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	got := tailnetChecks(t.Context(), r, false)
	if len(got) != 1 || got[0].Status != "warn" || !strings.Contains(got[0].Detail, "untagged") {
		t.Fatalf("got %+v", got)
	}
}

// The doctor reports the host's Tailscale even before a join, and a warning
// there does not turn the report into a failure.
func TestRunDoctorTailnet(t *testing.T) {
	useConfig(t, client.Config{})
	stubTailnet(t, tailnetSelf("1.104.3", true))
	rep := runDoctor(t.Context(), "", nil)
	var got []check
	for _, c := range rep.Checks {
		if c.Name == "tailnet" {
			got = append(got, c)
		}
	}
	if len(got) != 1 || got[0].Status != "warn" || !strings.Contains(got[0].Detail, "tag check skipped") {
		t.Fatalf("got %+v", got)
	}
}
