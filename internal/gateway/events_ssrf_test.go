package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestValidateCallbackURL(t *testing.T) {
	rejected := []string{
		"",
		"http://hooks.example.com/events",
		"ftp://hooks.example.com/events",
		"https://user:pass@hooks.example.com/events",
		"https://localhost/events",
		"https://sub.localhost/events",
		"https://127.0.0.1/events",
		"https://127.0.0.1:8443/events",
		"https://169.254.169.254/latest/meta-data/",
		"https://10.0.0.1/events",
		"https://172.16.0.5/events",
		"https://192.168.1.10/events",
		"https://100.64.0.1/events",
		"https://100.100.100.100/events",
		"https://[::1]/events",
		"https://[fe80::1]/events",
		"https://[fd7a:115c:a1e0::1]/events",
		"https://[::ffff:127.0.0.1]/events",
		"https://[::ffff:100.64.0.1]/events",
		"https://[::ffff:169.254.169.254]/events",
		"https://0.0.0.0/events",
		"https://hooks.example.com/" + strings.Repeat("a", 2050),
	}
	for _, raw := range rejected {
		if _, err := ValidateCallbackURL(raw); err == nil {
			t.Errorf("ValidateCallbackURL(%q) accepted, want rejection", raw)
		}
	}

	accepted := []string{
		"https://chatgpt.com/connector_platform/events",
		"https://1.1.1.1/webhook",
		"https://8.8.8.8:8443/webhook?token=abc",
		"https://[2606:4700:4700::1111]/webhook",
	}
	for _, raw := range accepted {
		if _, err := ValidateCallbackURL(raw); err != nil {
			t.Errorf("ValidateCallbackURL(%q) failed: %v", raw, err)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	blocked := []string{
		"0.0.0.0",
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.0.1",
		"169.254.169.254",
		"100.64.0.1",
		"100.127.255.254",
		"224.0.0.1",
		"255.255.255.255",
		"::",
		"::1",
		"fe80::1",
		"fc00::1",
		"fd7a:115c:a1e0::1",
		"::ffff:127.0.0.1",
		"::ffff:100.64.0.1",
		"64:ff9b::10.0.0.1",
	}
	for _, s := range blocked {
		if IsPublicIP(netip.MustParseAddr(s)) {
			t.Errorf("IsPublicIP(%s) = true, want false", s)
		}
	}
	public := []string{
		"1.1.1.1",
		"8.8.8.8",
		"104.18.12.34",
		"2606:4700:4700::1111",
	}
	for _, s := range public {
		if !IsPublicIP(netip.MustParseAddr(s)) {
			t.Errorf("IsPublicIP(%s) = false, want true", s)
		}
	}
}

type fakeResolver struct {
	addrs []netip.Addr
	err   error
}

func (r fakeResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r.addrs, r.err
}

func TestSafeWebhookClientBlocksPrivateDNSAndRedirects(t *testing.T) {
	cases := []struct {
		name  string
		addrs []netip.Addr
	}{
		{"loopback", []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{"metadata", []netip.Addr{netip.MustParseAddr("169.254.169.254")}},
		{"tailscale", []netip.Addr{netip.MustParseAddr("100.64.0.42")}},
		{"mixed_public_and_private", []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("10.0.0.1")}},
		{"ipv6_ula", []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::1")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := safeWebhookClientWithResolver(2*time.Second, fakeResolver{addrs: tc.addrs})
			req, _ := http.NewRequest(http.MethodPost, "https://webhook.example/hook", nil)
			resp, err := c.Do(req)
			if err == nil {
				resp.Body.Close()
				t.Fatalf("expected SSRF block for %s, got HTTP %d", tc.name, resp.StatusCode)
			}
			if !errors.Is(err, ErrBlockedCallbackIP) {
				t.Fatalf("err = %v, want ErrBlockedCallbackIP", err)
			}
		})
	}

	// Verify CheckRedirect refuses HTTP 302 redirects to private IPs (or anywhere).
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirSrv.Close()

	safe := SafeWebhookClient(2 * time.Second)
	// Test CheckRedirect policy directly on a local server transport.
	clientWithPolicy := &http.Client{
		CheckRedirect: safe.CheckRedirect,
	}
	resp, err := clientWithPolicy.Post(redirSrv.URL, "application/json", strings.NewReader("{}"))
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrRedirectsDisallowed) {
		t.Fatalf("redirect err = %v, want ErrRedirectsDisallowed", err)
	}
}

func TestSanitizeCallbackURL(t *testing.T) {
	got := SanitizeCallbackURL("https://user:secret@hooks.example.com:8443/v1/cb?token=supersecret#frag")
	want := "https://hooks.example.com:8443/v1/cb"
	if got != want {
		t.Fatalf("SanitizeCallbackURL = %q, want %q", got, want)
	}
}
