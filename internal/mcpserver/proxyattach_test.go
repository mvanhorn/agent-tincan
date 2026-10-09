package mcpserver_test

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
)

// get_attachment through a proxy whose password has rotated: with fresh
// credentials written into client.json the fetch reads them and succeeds;
// without, the tool error names the proxy and the 407 but never the
// password.
func TestGetAttachmentPicksUpRewrittenProxyPassword(t *testing.T) {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "TINCAN_PROXY", "TINCAN_RELAY"} {
		t.Setenv(k, "")
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := ""
		if raw, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Basic "); ok {
			if dec, err := base64.StdEncoding.DecodeString(raw); err == nil {
				_, got, _ = strings.Cut(string(dec), ":")
			}
		}
		if got != "fresh" {
			w.Header().Set("Proxy-Authenticate", `Basic realm="egress"`)
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		if r.URL.Path != "/v1/attachments/att1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello notes")
	}))
	t.Cleanup(proxy.Close)
	pu, _ := url.Parse(proxy.URL)

	session := func(pass string, rewrite bool) func() string {
		path := filepath.Join(t.TempDir(), "client.json")
		cfg := client.Config{Relay: "http://tincan-relay", Proxy: "http://u:" + pass + "@" + pu.Host, Agent: "grokbot"}
		if err := client.SaveConfigTo(path, cfg); err != nil {
			t.Fatal(err)
		}
		r, err := client.NewRelayForFile(cfg, path)
		if err != nil {
			t.Fatal(err)
		}
		if rewrite {
			cfg.Proxy = "http://u:fresh@" + pu.Host
			if err := client.SaveConfigTo(path, cfg); err != nil {
				t.Fatal(err)
			}
		}
		cs := connect(t, mcpserver.New(r, "test"))
		return func() string { return call(t, cs, "get_attachment", map[string]any{"id": "att1"}) }
	}

	if out := session("stale", true)(); !strings.Contains(out, "att1") || !strings.Contains(out, "11 bytes") || strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("get_attachment after the config was rewritten = %q", out)
	}
	out := session("stale-secret", false)()
	for _, want := range []string{"ERROR:", pu.Host, "407", "--proxy-credentials-from-env"} {
		if !strings.Contains(out, want) {
			t.Fatalf("get_attachment = %q, does not say %q", out, want)
		}
	}
	if strings.Contains(out, "stale-secret") {
		t.Fatalf("get_attachment leaks the proxy password: %q", out)
	}
}
