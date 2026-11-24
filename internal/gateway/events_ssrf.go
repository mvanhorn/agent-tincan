package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const maxCallbackURLLen = 2048

var (
	// ErrInvalidCallbackURL means the callback URL failed syntax, scheme, or host checks.
	ErrInvalidCallbackURL = errors.New("invalid callback URL")
	// ErrBlockedCallbackIP means the callback URL or its DNS resolution targeted a non-public IP.
	ErrBlockedCallbackIP = errors.New("callback IP address is not publicly routable")
	// ErrRedirectsDisallowed means the callback endpoint attempted an HTTP redirect.
	ErrRedirectsDisallowed = errors.New("webhook redirects are not allowed")
)

// blockedPrefixes covers non-public ranges beyond netip.Addr's built-in checks,
// including Tailscale/CGNAT (100.64.0.0/10), NAT64, documentation, and reserved space.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fec0::/10"),
}

// IsPublicIP reports whether addr is a publicly routable unicast IP address.
// It rejects loopback, RFC1918 private, link-local (including cloud metadata),
// multicast, unspecified, IPv6 ULA, Tailscale/CGNAT (100.64.0.0/10), NAT64,
// and reserved ranges. IPv4-mapped IPv6 addresses are unmapped before checking.
func IsPublicIP(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// ValidateCallbackURL validates that raw is a public HTTPS webhook URL.
func ValidateCallbackURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > maxCallbackURLLen {
		return nil, fmt.Errorf("%w: URL must be non-empty and at most %d bytes", ErrInvalidCallbackURL, maxCallbackURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCallbackURL, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fmt.Errorf("%w: scheme must be https", ErrInvalidCallbackURL)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: userinfo is not allowed", ErrInvalidCallbackURL)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return nil, fmt.Errorf("%w: missing host", ErrInvalidCallbackURL)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("%w: localhost is not allowed", ErrBlockedCallbackIP)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !IsPublicIP(addr) {
			return nil, fmt.Errorf("%w: %s", ErrBlockedCallbackIP, addr)
		}
	}
	return u, nil
}

// SanitizeCallbackURL returns scheme://host/path with any userinfo, query
// string, and fragment stripped so logs and doctor output never leak tokens.
func SanitizeCallbackURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<invalid-url>"
	}
	clean := url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   u.Path,
	}
	return clean.String()
}

type ipResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// SafeWebhookClient returns an HTTP client hardened against SSRF for MCP
// Events verification and delivery: HTTPS only, redirects disabled, proxy
// bypassed, and both DNS resolution and socket connect validated against
// IsPublicIP.
func SafeWebhookClient(timeout time.Duration) *http.Client {
	return safeWebhookClientWithResolver(timeout, net.DefaultResolver)
}

func safeWebhookClientWithResolver(timeout time.Duration, resolver ipResolver) *http.Client {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrBlockedCallbackIP, err)
			}
			if !IsPublicIP(ap.Addr()) {
				return fmt.Errorf("%w: %s", ErrBlockedCallbackIP, ap.Addr())
			}
			return nil
		},
	}
	base := &http.Transport{
		Proxy: nil, // Never route public callbacks through an HTTP proxy.
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidCallbackURL, err)
			}
			var addrs []netip.Addr
			if ip, err := netip.ParseAddr(host); err == nil {
				addrs = []netip.Addr{ip}
			} else {
				addrs, err = resolver.LookupNetIP(ctx, "ip", host)
				if err != nil {
					return nil, err
				}
			}
			if len(addrs) == 0 {
				return nil, fmt.Errorf("%w: no IP addresses resolved for %s", ErrBlockedCallbackIP, host)
			}
			// Fail closed if any resolved IP is non-public.
			for _, a := range addrs {
				if !IsPublicIP(a) {
					return nil, fmt.Errorf("%w: %s resolved to %s", ErrBlockedCallbackIP, host, a)
				}
			}
			var lastErr error
			for _, a := range addrs {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: httpsOnlyTransport{base: base},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectsDisallowed
		},
	}
}

type httpsOnlyTransport struct {
	base http.RoundTripper
}

func (t httpsOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, err := ValidateCallbackURL(req.URL.String()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}
