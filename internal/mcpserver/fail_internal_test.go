package mcpserver

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// A tool that failed because the SOCKS proxy could not reach the relay
// tells the agent the tunnel is down, that nothing was sent, and that an
// emailed request can be answered by email, as the CLI does.
func TestFailCarriesTunnelHint(t *testing.T) {
	socks := &url.Error{Op: "Get", URL: "http://tincan-relay/v1/inbox", Err: &net.OpError{Op: "socks connect", Net: "tcp", Err: errors.New("unknown error general SOCKS server failure")}}
	res, _, err := fail(socks)
	if err != nil || !res.IsError {
		t.Fatalf("fail = %+v, %v; want an error result", res, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"socks connect", "tailnet tunnel to the relay is not up", "nothing was sent", "replying to that email"} {
		if !strings.Contains(text, want) {
			t.Fatalf("result %q does not say %q", text, want)
		}
	}
}

// A tool whose proxy refused an expired password returns the proxy error as
// a tool error naming the proxy and the fix, never the password.
func TestFailReportsExpiredProxyPassword(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	host := strings.TrimPrefix(proxy.URL, "http://")
	r, err := client.NewRelay("http://tincan-relay", "http://u:expired-secret@"+host)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Poll(t.Context(), 0)
	res, _, _ := fail(err)
	if !res.IsError {
		t.Fatalf("fail = %+v; want an error result", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{host, "407", "--proxy-credentials-from-env"} {
		if !strings.Contains(text, want) {
			t.Fatalf("result %q does not say %q", text, want)
		}
	}
	if strings.Contains(text, "expired-secret") {
		t.Fatalf("result leaks the proxy password: %q", text)
	}
}
