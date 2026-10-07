package cli

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// tailnetPrefixes are the address ranges Tailscale assigns to tailnet
// machines: the CGNAT range for IPv4 and Tailscale's ULA range for IPv6.
var tailnetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// tailnetHost reports whether host (a name or an IP, without the port) is
// a tailnet machine: an address in a Tailscale range or a MagicDNS name
// under ts.net.
func tailnetHost(host string) bool {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		for _, p := range tailnetPrefixes {
			if p.Contains(ip.Unmap()) {
				return true
			}
		}
		return false
	}
	return strings.HasSuffix(strings.TrimSuffix(strings.ToLower(host), "."), ".ts.net")
}

// wakeHTTPClient is the waker's HTTP client for a relay on its own tsnet
// node. A wake webhook at a tailnet address (Hermes on the owner's Mac, an
// OpenClaw gateway) goes through a transport that dials from the relay's
// tsnet node with no proxy: on a host whose tailscaled runs without a TUN
// device (a sandboxed VM in userspace networking), the operating system
// cannot route to tailnet addresses at all, so an ordinary dial just times
// out. Every other request (AgentMail, public webhooks) keeps the relay's
// usual transport exactly as it was, proxy included.
func wakeHTTPClient(dialTailnet func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	c := client.New(client.APIClient)
	tailnet := &http.Transport{DialContext: dialTailnet}
	if tr, ok := c.Transport.(*http.Transport); ok {
		tailnet = tr.Clone()
		tailnet.Proxy = nil
		tailnet.DialContext = dialTailnet
	}
	c.Transport = wakeRoute{public: c.Transport, tailnet: tailnet}
	return c
}

// wakeRoute picks a transport by the request's own destination host, so a
// proxy at a tailnet address never pulls public traffic onto the tailnet
// route.
type wakeRoute struct {
	public, tailnet http.RoundTripper
}

// RoundTrip sends r through the tailnet transport when its host is on the
// tailnet, otherwise through the public one.
func (w wakeRoute) RoundTrip(r *http.Request) (*http.Response, error) {
	if tailnetHost(r.URL.Hostname()) {
		return w.tailnet.RoundTrip(r)
	}
	return w.public.RoundTrip(r)
}
