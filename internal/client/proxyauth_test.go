package client

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// connectProxy is a forward proxy that tunnels a CONNECT whose
// Proxy-Authorization carries pass and answers any other with 407, as a
// sandbox egress proxy does for an https:// relay.
func connectProxy(t *testing.T, pass string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := ""
		if raw, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Basic "); ok {
			if dec, err := base64.StdEncoding.DecodeString(raw); err == nil {
				_, got, _ = strings.Cut(string(dec), ":")
			}
		}
		if r.Method != http.MethodConnect || got != pass {
			w.Header().Set("Proxy-Authenticate", `Basic realm="egress"`)
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		down, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			up.Close()
			return
		}
		io.WriteString(down, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { io.Copy(up, down); up.Close() }()
		io.Copy(down, up)
		down.Close()
	}))
	t.Cleanup(ts.Close)
	return ts
}

// An https:// relay reached through a proxy sends a CONNECT first, and Go's
// transport reports a refused CONNECT as an error reading only "Proxy
// Authentication Required", with no 407 in it. That must still count as a
// proxy authentication failure: it reads the config again for fresher
// credentials and, failing that, ends with a ProxyAuthError that never
// shows the password.
func TestProxy407OnConnectToHTTPSRelay(t *testing.T) {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "TINCAN_PROXY", "TINCAN_RELAY"} {
		t.Setenv(k, "")
	}
	relaySrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[{"name":"muse","online":true,"wake":"wait"}]}`))
	}))
	t.Cleanup(relaySrv.Close)
	proxy := connectProxy(t, "fresh-secret")
	host := strings.TrimPrefix(proxy.URL, "http://")

	path := filepath.Join(t.TempDir(), "client.json")
	cfg := Config{Relay: relaySrv.URL, Proxy: "http://u:stale-secret@" + host, Agent: "muse"}
	if err := SaveConfigTo(path, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := NewRelayForFile(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(relaySrv.Certificate())
	for _, c := range []*http.Client{r.api, r.polls} {
		c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
	}

	_, err = r.Agents(t.Context())
	if !IsProxyAuth(err) {
		t.Fatalf("Agents error = %v, want a proxy authentication error", err)
	}
	if strings.Contains(err.Error(), "stale-secret") {
		t.Fatalf("error leaks the proxy password: %q", err)
	}

	// A wrapper writes fresh credentials into the config: the next refused
	// CONNECT reads them and the call goes through.
	cfg.Proxy = "http://u:fresh-secret@" + host
	if err := SaveConfigTo(path, cfg); err != nil {
		t.Fatal(err)
	}
	if agents, err := r.Agents(t.Context()); err != nil || len(agents) != 1 {
		t.Fatalf("Agents after the config was rewritten = %+v, %v", agents, err)
	}
}
