package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// oldRelay is a relay that answers an invite as a relay from before guests
// does: it mints a code and ignores the guest field. guests sets its
// capability, so the second check is reachable too.
func oldRelay(t *testing.T, guests bool) (*client.Relay, *int) {
	t.Helper()
	invites := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(client.Capabilities{Guests: guests})
		case "/v1/admin/invite":
			invites++
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "friend", "code": "AAAA-BBBB"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	r, err := client.NewRelayFor(client.Config{Relay: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return r, &invites
}

// A relay without the capability never sees the invite: an old relay would
// ignore the guest field and mint a full teammate.
func TestInviteGuestRefusesARelayWithoutTheCapability(t *testing.T) {
	r, invites := oldRelay(t, false)
	_, err := r.InviteGuest(t.Context(), "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	if !errors.Is(err, client.ErrGuestsUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if *invites != 0 {
		t.Fatalf("%d invites sent", *invites)
	}
}

// A relay that took the invite but echoed no limits gets no code printed.
func TestInviteGuestRefusesACodeWithNoLimits(t *testing.T) {
	r, _ := oldRelay(t, true)
	inv, err := r.InviteGuest(t.Context(), "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	if !errors.Is(err, client.ErrGuestsUnsupported) || inv.Code != "" {
		t.Fatalf("invite = %+v, %v", inv, err)
	}
	if strings.Contains(err.Error(), "AAAA") {
		t.Fatalf("error carries the code: %v", err)
	}
}

// An older relay would drop the guest field and bind a full teammate, so
// ConnectGuest asks first, and a relay that takes the call but does not echo
// the limits gets no code out of the client.
func TestConnectGuestRefusesRelayWithoutSupport(t *testing.T) {
	for name, tc := range map[string]struct {
		caps    string
		connect string
		calls   int
	}{
		"no capability": {caps: `{"guests":true}`, calls: 0},
		"no echo":       {caps: `{"guests":true,"gateway_guests":true}`, connect: `{"name":"pal","code":"AAAA-BBBB","url":"https://example.test/mcp"}`, calls: 1},
	} {
		t.Run(name, func(t *testing.T) {
			connects := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/capabilities":
					_, _ = w.Write([]byte(tc.caps))
				case "/v1/admin/connect":
					connects++
					_, _ = w.Write([]byte(tc.connect))
				}
			}))
			defer ts.Close()
			r := client.NewRelayHTTP(ts.URL, ts.Client())
			out, err := r.ConnectGuest(t.Context(), "pal", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
			if !errors.Is(err, client.ErrGuestsUnsupported) || out.Code != "" || connects != tc.calls {
				t.Fatalf("out = %+v, err = %v, connects = %d", out, err, connects)
			}
		})
	}
}
