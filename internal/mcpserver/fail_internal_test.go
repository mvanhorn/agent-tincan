package mcpserver

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
