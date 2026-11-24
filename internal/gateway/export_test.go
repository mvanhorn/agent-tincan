package gateway

import (
	"net/http"
	"net/url"
)

// AllowUnsafeCallbacksForTest configures an EventHub to use client (such as
// httptest.Server's Client) without blocking loopback URLs. This file is
// compiled only during `go test`, so production builds can never bypass SSRF.
func AllowUnsafeCallbacksForTest(h *EventHub, client *http.Client) {
	h.clientMu.Lock()
	defer h.clientMu.Unlock()
	if client != nil {
		h.verifyClient = client
		h.deliverClient = client
	}
	h.validateURL = func(raw string) (*url.URL, error) {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, ErrInvalidCallbackURL
		}
		return u, nil
	}
}
