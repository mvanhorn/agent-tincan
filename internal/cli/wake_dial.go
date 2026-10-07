package cli

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
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
// OpenClaw gateway) is dialed through dialTailnet, the relay's tsnet node,
// with no proxy: on a host whose tailscaled runs without a TUN device (a
// sandboxed VM in userspace networking), the operating system cannot route
// to tailnet addresses at all, so an ordinary dial just times out. Every
// other address (AgentMail, public webhooks) keeps the relay's usual
// client: its timeout, proxy settings and dialer.
func wakeHTTPClient(dialTailnet func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	c := client.New(client.APIClient)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		return c
	}
	tr = tr.Clone()
	proxy, dial := tr.Proxy, tr.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	tr.Proxy = func(r *http.Request) (*url.URL, error) {
		if tailnetHost(r.URL.Hostname()) || proxy == nil {
			return nil, nil
		}
		return proxy(r)
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(addr); err == nil && tailnetHost(host) {
			return dialTailnet(ctx, network, addr)
		}
		return dial(ctx, network, addr)
	}
	c.Transport = tr
	return c
}
