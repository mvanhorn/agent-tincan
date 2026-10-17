package relay

import (
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Refusals from HardenForBrowsers. They name no header value, so a refused
// page learns nothing it did not send.
var (
	errForeignHost  = errors.New("tincan relay: this address is not one of the relay's own names")
	errCrossOrigin  = errors.New("tincan relay: cross-origin browser requests are refused")
	errNotJSONState = errors.New("tincan relay: browser requests that change state must be application/json")
)

// phonePath is the one page a browser may open from outside the relay's
// own pages (Sec-Fetch-Site: none: a typed address, a bookmark or a
// home-screen icon).
const phonePath = "/phone/"

// HardenForBrowsers wraps h, the relay API as served on a tailnet listener,
// so that no web page an admin device has open can act on or read the relay
// from another origin. hosts are the relay's own names: its MagicDNS short
// and full names, its tailnet IPs and its --listen address (and, for a TLS
// listener, its certificate name), from the same source as the URLs it
// advertises. Every request:
//
//   - must name one of hosts in Host (any port), which defeats DNS
//     rebinding, whose requests carry the attacker's name and no Origin;
//   - when it carries Origin or Sec-Fetch-Site, must be same-origin, except
//     Sec-Fetch-Site: none on GET /phone/;
//   - when a browser marks it (Origin or any Sec-Fetch-* header) and its
//     method changes state, must be Content-Type: application/json, which
//     no form or no-preflight fetch can send.
//
// Go's http.CrossOriginProtection then checks Origin and Sec-Fetch-Site
// again for non-GET methods. Every response, refusals included, forbids
// framing. CLI and MCP clients name the relay by an address it advertises
// and send no browser headers, so they pass untouched.
//
// Wrap tailnet listeners only. Server.Handler() itself stays bare: the
// in-process gateway calls it with Host tincan-relay and test relays serve
// it on loopback. AdminHandler is a local socket, not a browser surface.
func HardenForBrowsers(h http.Handler, hosts []string) http.Handler {
	own := make(map[string]bool, len(hosts))
	for _, n := range hosts {
		if n = hostName(n); n != "" {
			own[n] = true
		}
	}
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusForbidden, errCrossOrigin)
	}))
	next := cop.Handler(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		if !own[hostName(r.Host)] {
			writeErr(w, http.StatusForbidden, errForeignHost)
			return
		}
		if !sameOrigin(r) {
			writeErr(w, http.StatusForbidden, errCrossOrigin)
			return
		}
		if browserMarked(r) && changesState(r.Method) && !isJSON(r.Header.Get("Content-Type")) {
			writeErr(w, http.StatusForbidden, errNotJSONState)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether r's Sec-Fetch-Site and Origin, where present,
// say it came from the relay's own origin.
func sameOrigin(r *http.Request) bool {
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "", "same-origin":
	case "none":
		if r.Method != http.MethodGet || r.URL.Path != phonePath {
			return false
		}
	default:
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return false // includes "null", sent by sandboxed and file pages
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return strings.EqualFold(u.Scheme, scheme) && hostPort(u.Host, scheme) == hostPort(r.Host, scheme)
}

// browserMarked reports whether a browser sent r: browsers add Origin to
// every cross-origin and every non-GET request, and Sec-Fetch-* to every
// request in current engines. Other clients send neither.
func browserMarked(r *http.Request) bool {
	if r.Header.Get("Origin") != "" {
		return true
	}
	for k := range r.Header {
		if strings.HasPrefix(k, "Sec-Fetch-") {
			return true
		}
	}
	return false
}

// changesState reports whether method may change relay state.
func changesState(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}

// hostName is the lower-case host part of a Host value or name, without
// port, IPv6 brackets or a trailing dot.
func hostName(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"), ".")
	return strings.ToLower(h)
}

// hostPort is a Host or Origin authority normalized for comparison: the
// host name as hostName returns it, plus the port unless it is scheme's
// default.
func hostPort(authority, scheme string) string {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = authority, ""
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host = hostName(host)
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}
