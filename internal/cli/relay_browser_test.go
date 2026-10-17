package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// rawCall sends one request to the relay at base with Host host and the
// given headers, and returns the status and response headers.
func rawCall(t *testing.T, method, base, path, host, body string, hdr map[string]string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

var inviteCode = regexp.MustCompile(`\): (\S+)\n`)

// The relay command serves its tailnet API behind the browser hardening: a
// hostile page or a rebound name is refused, while the real CLI invites,
// approves and removes, an agent sends and polls, and a client that
// followed a relay move to a new address still gets through.
func TestRelayProcessRefusesBrowsersButNotTheCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a relay process")
	}
	dir := relayStateDir(t)
	if err := os.WriteFile(filepath.Join(dir, "approval.json"), []byte(`{"gate":{"worker":{"from":"*"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startRelayProcessIn(t, dir)
	useConfig(t, client.Config{})
	asker := p.agent(t, "asker")

	// Covers AE6 against the real relay command, and DNS rebinding.
	if code, hdr := rawCall(t, "POST", p.url, "/v1/admin/remove", "", `{"name":"asker"}`,
		map[string]string{"Origin": "https://evil.example", "Content-Type": "application/json"}); code != http.StatusForbidden || hdr.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("cross-origin remove: %d %v, want 403 with X-Frame-Options DENY", code, hdr)
	}
	if code, _ := rawCall(t, "GET", p.url, "/v1/admin/held", "evil.example", "", nil); code != http.StatusForbidden {
		t.Fatalf("held via a rebound Host: %d, want 403", code)
	}

	out, err := run(t, inviteCmd(), "worker", "--relay", p.url)
	m := inviteCode.FindStringSubmatch(out)
	if err != nil || m == nil {
		t.Fatalf("tincan invite: %q %v", out, err)
	}
	worker, err := client.NewRelayFor(client.Config{Relay: p.url, Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Join(t.Context(), m[1]); err != nil {
		t.Fatal(err)
	}
	req, err := asker.Send(t.Context(), "worker", "please call", envelope.KindAsk, "", false)
	if err != nil || req.Status != envelope.StatusHeld {
		t.Fatalf("send: %+v %v, want held", req, err)
	}
	if out, err := run(t, heldCmd(), "--relay", p.url); err != nil || !strings.Contains(out, req.ID) {
		t.Fatalf("tincan held: %q %v", out, err)
	}
	if _, err := run(t, approvalCmd("approve"), req.ID, "--relay", p.url); err != nil {
		t.Fatalf("tincan approve: %v", err)
	}
	in, err := worker.Poll(t.Context(), 0)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].ID != req.ID {
		t.Fatalf("worker poll after approval: %+v %v", in.Requests, err)
	}

	// A client whose saved address stopped answering finds the relay at an
	// address it advertises, and that address passes the Host check.
	var who struct {
		RelayKey string `json:"relay_key"`
	}
	if err := asker.Raw(t.Context(), "GET", "/v1/whoami", nil, &who); err != nil || who.RelayKey == "" {
		t.Fatalf("whoami: %+v %v", who, err)
	}
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	old := "http://" + dead.Addr().String()
	dead.Close()
	moved, err := client.NewRelayFor(client.Config{Relay: old, Agent: "asker", RelayKey: who.RelayKey, RelayURLs: []string{p.url}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := moved.Agents(t.Context()); err != nil || moved.Base() != p.url {
		t.Fatalf("after the move: base %s, %v; want %s", moved.Base(), err, p.url)
	}

	if _, err := run(t, removeCmd(), "worker", "--relay", p.url); err != nil {
		t.Fatalf("tincan remove: %v", err)
	}
	if _, err := worker.Poll(t.Context(), 0); err == nil {
		t.Fatal("removed worker can still poll")
	}
}

// A ChatGPT or Sesame agent reaches the relay through the in-process
// gateway, which calls the relay's handler directly with Host tincan-relay.
// The browser hardening wraps only the tailnet listener, so with it running
// a gateway agent still asks and reads its reply.
func TestRelayProcessGatewayAgentUnaffectedByHardening(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a relay process")
	}
	// The gateway's port is picked free here and bound by the relay process,
	// so another process can take it in between: retry on a fresh port.
	var p *relayProcess
	var gwURL string
	for try := 0; p == nil; try++ {
		free, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		gwAddr := free.Addr().String()
		free.Close()
		gwURL = "http://" + gwAddr
		p, err = tryRelayProcessIn(t, relayStateDir(t), "--chatgpt-gateway", "--gateway-listen", gwAddr, "--gateway-url", gwURL)
		if err != nil && (try == 2 || !strings.Contains(err.Error(), "address already in use")) {
			t.Fatal(err)
		}
	}
	useConfig(t, client.Config{})
	worker := p.agent(t, "worker")

	out, err := run(t, connectCmd(), "chatgpt", "--relay", p.url)
	if err != nil {
		t.Fatalf("tincan connect: %q %v", out, err)
	}
	m := regexp.MustCompile(`one-time code[^\n]*\n\s+(\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no code in %q", out)
	}
	tok := gatewayLogin(t, gwURL, m[1])
	tr := &mcp.StreamableClientTransport{Endpoint: gwURL + "/mcp", HTTPClient: &http.Client{Transport: bearerToken(tok)}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "chatgpt"}, nil).Connect(t.Context(), tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "ask", Arguments: map[string]any{"to": "worker", "message": "status?", "wait_seconds": 0}})
	if err != nil || res.IsError {
		t.Fatalf("gateway ask: %v %+v", err, res)
	}
	in, err := worker.Poll(t.Context(), 0)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].From != "chatgpt" {
		t.Fatalf("worker poll: %+v %v", in.Requests, err)
	}
	if _, err := worker.Reply(t.Context(), in.Requests[0].ID, "all green", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "check_inbox", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("gateway check_inbox: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.Content)
	if !strings.Contains(string(raw), "all green") {
		t.Fatalf("gateway check_inbox lacks the reply: %s", raw)
	}
}

// The Host allowlist covers every name the relay advertises and the
// --listen address, and refuses to be empty.
func TestRelayHosts(t *testing.T) {
	got, err := relayHosts([]string{"tincan-relay.tail.ts.net", "tincan-relay", "100.64.0.1"},
		[]string{"http://tincan-relay.tail.ts.net:8080", "http://100.64.0.1:8080"}, "100.64.0.1:8080")
	if err != nil || strings.Join(got, ",") != "100.64.0.1,tincan-relay,tincan-relay.tail.ts.net" {
		t.Fatalf("relayHosts = %q, %v", got, err)
	}
	got, err = relayHosts(nil, nil, "100.101.102.103:80")
	if err != nil || strings.Join(got, ",") != "100.101.102.103" {
		t.Fatalf("relayHosts with only --listen = %q, %v", got, err)
	}
	if _, err := relayHosts(nil, nil, ""); err == nil {
		t.Fatal("relayHosts with no names: want an error")
	}
}

type bearerToken string

func (b bearerToken) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// gatewayLogin runs the connector side of the gateway's OAuth login with a
// one-time code from tincan connect and returns the access token.
func gatewayLogin(t *testing.T, base, code string) string {
	t.Helper()
	const redirect = "https://chatgpt.com/connector_platform_oauth_redirect"
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	reg, err := http.Post(base+"/register", "application/json", strings.NewReader(`{"redirect_uris":["`+redirect+`"],"client_name":"ChatGPT"}`))
	if err != nil {
		t.Fatal(err)
	}
	var cl struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(reg.Body).Decode(&cl)
	reg.Body.Close()
	verifier := "a-long-random-verifier-string-for-pkce-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	resp, err := noRedirect.PostForm(base+"/authorize", url.Values{"login_code": {code}, "client_id": {cl.ClientID},
		"redirect_uri": {redirect}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "state": {"s"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil {
		t.Fatalf("authorize: %d %v", resp.StatusCode, err)
	}
	tok, err := http.PostForm(base+"/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"client_id": {cl.ClientID}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(tok.Body).Decode(&out)
	if out.AccessToken == "" {
		t.Fatalf("no access token (status %d)", tok.StatusCode)
	}
	return out.AccessToken
}
