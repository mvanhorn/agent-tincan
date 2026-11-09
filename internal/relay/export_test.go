package relay

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// WebSendBudget is the budget a web member's mention ask is packed to.
const WebSendBudget = webSendBudget

// KeyForTest exposes the relay key to tests.
func KeyForTest(s *Server) string { return s.key }

// LoosenPusherForTest lets p send to plain-HTTP push services on loopback
// (test servers) and gives each push timeout. Redirects are still not
// followed.
func LoosenPusherForTest(p *Pusher, timeout time.Duration) {
	p.client = newPushClient(func(netip.Addr) error { return nil })
	p.timeout = timeout
	p.allow = func(u *url.URL) bool { return u.Scheme == "http" && u.Hostname() == "127.0.0.1" }
}

// PushAddrRefused reports whether the push sender refuses to dial ip.
func PushAddrRefused(ip string) bool {
	return refusePushAddr(netip.MustParseAddr(ip)) != nil
}

// DialPushForTest makes one request to hostPort with the real push
// client.
func DialPushForTest(ctx context.Context, hostPort string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+hostPort+"/", nil)
	if err != nil {
		return err
	}
	resp, err := newPushClient(refusePushAddr).Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// Routes lists the patterns Handler serves, and GuestAllowed the ones it
// serves to a guest.
func (s *Server) Routes() []string { return s.routes().patterns }

var GuestAllowed = guestRoutes
