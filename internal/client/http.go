// Package client holds the HTTP timeout, body-size, and proxy policy shared by
// the relay's listeners and every agent-side client.
//
// Adapted from agentcookie internal/cli/httpserver/httpserver.go (commit
// 290cd74). The profiles are tincan's; the proxy-aware transport is unchanged
// because it is the proven path for client-only sandboxes like Muse, which can
// only reach the tailnet through HTTP_PROXY.
package client

import (
	"net"
	"net/http"
	"time"
)

// Profile names the route a configuration is for.
type Profile int

const (
	// RelayAPI is the relay's agent API: send, claim, reply, cancel, and the
	// long-poll endpoints. WriteTimeout must exceed the longest long-poll hold
	// so a held poll is not cut off before it answers.
	RelayAPI Profile = iota

	// APIClient is an agent-side client for short calls (send, claim, reply).
	APIClient

	// PollClient is an agent-side client for long-polls. Its timeout must
	// exceed the relay's hold time plus network slack.
	PollClient
)

// dialTimeout bounds connecting to the relay (or the proxy in front of it).
const dialTimeout = 5 * time.Second

// DefaultPollHold is how long the relay holds a long-poll open before it
// answers "nothing pending". U1 tunes this below Muse's proxy idle timeout.
const DefaultPollHold = 25 * time.Second

// Settings carries the resolved values for a Profile.
type Settings struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	MaxBodyBytes      int64
	ClientTimeout     time.Duration
}

// Defaults returns the baseline Settings for a profile.
func Defaults(p Profile) Settings {
	switch p {
	case RelayAPI:
		return Settings{
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      DefaultPollHold + 35*time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    16 * 1024,
			MaxBodyBytes:      1 << 20,
		}
	case APIClient:
		return Settings{ClientTimeout: 30 * time.Second}
	case PollClient:
		return Settings{ClientTimeout: DefaultPollHold + 30*time.Second}
	}
	return Settings{}
}

// Configure applies a profile's server-side settings to srv and returns it.
func Configure(srv *http.Server, p Profile) *http.Server {
	s := Defaults(p)
	srv.ReadHeaderTimeout = s.ReadHeaderTimeout
	srv.ReadTimeout = s.ReadTimeout
	srv.WriteTimeout = s.WriteTimeout
	srv.IdleTimeout = s.IdleTimeout
	srv.MaxHeaderBytes = s.MaxHeaderBytes
	return srv
}

// New returns an http.Client for the given client profile. Unknown profiles
// fall back to a 30-second timeout.
//
// Transport is always a clone of DefaultTransport with ProxyFromEnvironment,
// so HTTP_PROXY, HTTPS_PROXY, and NO_PROXY are honored explicitly. If a test
// has replaced DefaultTransport with a stub RoundTripper, that stub is used.
func New(p Profile) *http.Client {
	timeout := Defaults(p).ClientTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout, Transport: proxyTransport()}
}

func proxyTransport() http.RoundTripper {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	cloned := tr.Clone()
	cloned.Proxy = http.ProxyFromEnvironment
	// A relay that moved leaves its old tailnet address silent. Give up
	// on connecting after dialTimeout so the client looks for the relay
	// instead of waiting out the whole request timeout.
	cloned.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	return cloned
}

// LimitBody caps r.Body at max bytes. Reads past the cap return
// *http.MaxBytesError.
func LimitBody(w http.ResponseWriter, r *http.Request, max int64) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
}
