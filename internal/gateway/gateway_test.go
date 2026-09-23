package gateway_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/gateway"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

type env struct {
	m     *testrelay.Mesh
	gw    *httptest.Server
	oauth *gateway.OAuth
	conn  gateway.Connector
}

func setup(t *testing.T) *env {
	t.Helper()
	m := testrelay.New(t, relay.Config{MaxWait: 3 * time.Second})
	oauth, err := gateway.NewOAuth(m.Store.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{m: m, oauth: oauth}
	var gwHandler http.Handler
	e.gw = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gwHandler.ServeHTTP(w, r) }))
	t.Cleanup(e.gw.Close)
	gwHandler = gateway.New(e.gw.URL, oauth, m.Server.Handler(), "test").Handler()
	e.conn = gateway.Connector{Dir: m.Dir, OAuth: oauth, Base: e.gw.URL}
	m.Server.SetConnector(e.conn)
	return e
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// login runs the ChatGPT-side OAuth dance and returns an access token.
func (e *env) login(t *testing.T, code string) (string, *http.Response) {
	t.Helper()
	s, resp := e.loginSession(t, code)
	return s.AccessToken, resp
}

// session is what a completed login leaves ChatGPT holding.
type session struct {
	gateway.Tokens
	ClientID string
}

// loginSession runs the OAuth dance and returns the tokens and client_id.
func (e *env) loginSession(t *testing.T, code string) (session, *http.Response) {
	t.Helper()
	const redirect = "https://chatgpt.com/connector_platform_oauth_redirect"
	reg, _ := http.Post(e.gw.URL+"/register", "application/json", strings.NewReader(`{"redirect_uris":["`+redirect+`"],"client_name":"ChatGPT"}`))
	var client struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(reg.Body).Decode(&client)
	reg.Body.Close()
	verifier := "a-long-random-verifier-string-for-pkce-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	page, _ := http.Get(e.gw.URL + "/authorize?" + url.Values{"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"xyz"}}.Encode())
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != 200 || !strings.Contains(string(body), "tincan connect chatgpt") {
		t.Fatalf("login page %d: %s", page.StatusCode, body)
	}
	resp, err := noRedirect.PostForm(e.gw.URL+"/authorize", url.Values{"login_code": {code}, "client_id": {client.ClientID},
		"redirect_uri": {redirect}, "code_challenge": {challenge}, "state": {"xyz"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound {
		return session{}, resp
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("state") != "xyz" {
		t.Fatalf("state not echoed: %s", loc)
	}
	tok, _ := http.PostForm(e.gw.URL+"/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"client_id": {client.ClientID}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
	var tokens gateway.Tokens
	json.NewDecoder(tok.Body).Decode(&tokens)
	tok.Body.Close()
	if tokens.AccessToken == "" {
		t.Fatalf("no access token (status %d)", tok.StatusCode)
	}
	return session{Tokens: tokens, ClientID: client.ClientID}, resp
}

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func (e *env) mcpSession(t *testing.T, tok string) *mcp.ClientSession {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: e.gw.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{tok}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "chatgpt"}, nil).Connect(t.Context(), tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// AE8: ChatGPT logs in once with Matt's code, then asks Instinct something.
func TestChatGPTConnectsAndAsks(t *testing.T) {
	e := setup(t)
	code, mcpURL, err := e.conn.Connect(context.Background(), "chatgpt")
	if err != nil || mcpURL != e.gw.URL+"/mcp" {
		t.Fatalf("connect: %v %s", err, mcpURL)
	}
	tok, _ := e.login(t, code)
	cs := e.mcpSession(t, tok)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "ask", Arguments: map[string]any{"to": "instinct", "message": "what's in the report", "wait_seconds": 1}})
	if err != nil || res.IsError {
		t.Fatalf("ask: %v %+v", err, res)
	}
	in, err := e.m.Client(t, "instinct").Poll(context.Background(), 0)
	reqs := in.Requests
	if err != nil || len(reqs) != 1 || reqs[0].From != "chatgpt" {
		t.Fatalf("instinct got %+v, %v", reqs, err)
	}
}

func TestMCPNeedsAToken(t *testing.T) {
	e := setup(t)
	resp, _ := http.Post(e.gw.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("status %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	req, _ := http.NewRequest("POST", e.gw.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer made-up")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("bad token: status %d", resp.StatusCode)
	}
}

func TestLoginCodeRules(t *testing.T) {
	e := setup(t)
	code, _, _ := e.conn.Connect(context.Background(), "chatgpt")
	if _, resp := e.login(t, "WRNG-CODE"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code: status %d", resp.StatusCode)
	}
	e.login(t, code)
	if _, resp := e.login(t, code); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused code: status %d", resp.StatusCode)
	}
}

func TestBruteForceLockout(t *testing.T) {
	e := setup(t)
	code, _, _ := e.conn.Connect(context.Background(), "chatgpt")
	for range 5 {
		e.login(t, "BAAD-CODE")
	}
	if _, resp := e.login(t, code); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after 5 bad codes the right code should be locked out, got %d", resp.StatusCode)
	}
}

func TestRemoveRevokesAccess(t *testing.T) {
	e := setup(t)
	code, _, _ := e.conn.Connect(context.Background(), "chatgpt")
	tok, _ := e.login(t, code)
	e.m.Client(t, "admin").Remove(context.Background(), "chatgpt")
	req, _ := http.NewRequest("POST", e.gw.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("token still works after remove: %d", resp.StatusCode)
	}
}

// The public side serves only MCP and OAuth; the agent API is not there.
func TestAgentAPINotExposed(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/v1/poll", "/v1/send", "/v1/agents", "/v1/admin/invite", "/v1/join"} {
		resp, _ := http.Get(e.gw.URL + p)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s reachable on the gateway: %d", p, resp.StatusCode)
		}
	}
}

func TestRedirectMustBeHTTPS(t *testing.T) {
	e := setup(t)
	resp, _ := http.Post(e.gw.URL+"/register", "application/json", strings.NewReader(`{"redirect_uris":["http://evil.example/cb"]}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain-http redirect accepted: %d", resp.StatusCode)
	}
}

func TestConnectEndpointIsAdminOnly(t *testing.T) {
	e := setup(t)
	var out struct{ Code, URL string }
	if err := e.m.Client(t, "admin").Raw(context.Background(), "POST", "/v1/admin/connect", map[string]string{"name": "chatgpt"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Code == "" || out.URL != e.gw.URL+"/mcp" {
		t.Fatalf("connect = %+v", out)
	}
	if err := e.m.Client(t, "muse").Raw(context.Background(), "POST", "/v1/admin/connect", map[string]string{"name": "chatgpt"}, nil); err == nil {
		t.Fatal("muse should not be able to connect agents")
	}
}

func (e *env) refresh(t *testing.T, refreshToken, clientID string) (gateway.Tokens, int) {
	t.Helper()
	resp, err := http.PostForm(e.gw.URL+"/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tokens gateway.Tokens
	json.NewDecoder(resp.Body).Decode(&tokens)
	return tokens, resp.StatusCode
}

// ChatGPT's access tokens expire hourly, so long-lived connections live on
// refresh: it must issue a working token, rotate single-use, and bind to the
// client that got it.
func TestRefreshRotatesTokens(t *testing.T) {
	e := setup(t)
	code, _, _ := e.conn.Connect(context.Background(), "chatgpt")
	s, _ := e.loginSession(t, code)
	if s.RefreshToken == "" || s.ClientID == "" {
		t.Fatalf("login left no refresh token or client_id: %+v", s)
	}

	fresh, status := e.refresh(t, s.RefreshToken, s.ClientID)
	if status != http.StatusOK || fresh.AccessToken == "" || fresh.RefreshToken == "" {
		t.Fatalf("refresh: status %d, %+v", status, fresh)
	}
	if fresh.AccessToken == s.AccessToken || fresh.RefreshToken == s.RefreshToken {
		t.Fatal("refresh returned the old tokens")
	}
	cs := e.mcpSession(t, fresh.AccessToken)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_agents with refreshed token: %v %+v", err, res)
	}

	if tok, status := e.refresh(t, s.RefreshToken, s.ClientID); status != http.StatusBadRequest || tok.AccessToken != "" {
		t.Fatalf("reused refresh token: status %d, %+v", status, tok)
	}

	if tok, status := e.refresh(t, fresh.RefreshToken, "tincan-someone-else"); status != http.StatusBadRequest || tok.AccessToken != "" {
		t.Fatalf("refresh with mismatched client_id: status %d, %+v", status, tok)
	}
}

func register(t *testing.T, e *env, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(e.gw.URL+"/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestRegisterRejectsTooManyRedirects(t *testing.T) {
	e := setup(t)
	uris := make([]string, 6)
	for i := range uris {
		uris[i] = `"https://chatgpt.com/cb` + string(rune('a'+i)) + `"`
	}
	resp, out := register(t, e, `{"redirect_uris":[`+strings.Join(uris, ",")+`]}`)
	if resp.StatusCode != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("6 redirect URIs: status %d, %v", resp.StatusCode, out)
	}
}

func TestRegisterRejectsOversizeRedirect(t *testing.T) {
	e := setup(t)
	long := "https://chatgpt.com/" + strings.Repeat("a", 2048)
	resp, out := register(t, e, `{"redirect_uris":["`+long+`"]}`)
	if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_redirect_uri" {
		t.Fatalf("oversize redirect URI: status %d, %v", resp.StatusCode, out)
	}
}

func TestRegisterRejectsOversizeBody(t *testing.T) {
	e := setup(t)
	resp, out := register(t, e, `{"redirect_uris":["https://chatgpt.com/cb"],"client_name":"`+strings.Repeat("x", 20<<10)+`"}`)
	if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_client_metadata" {
		t.Fatalf("20 KB body: status %d, %v", resp.StatusCode, out)
	}
}

func countRows(t *testing.T, e *env, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.m.Store.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Registration is public, so abandoned clients are pruned and the table is
// capped; expired codes and tokens are swept at the same moment.
func TestRegisterPrunesStaleClients(t *testing.T) {
	e := setup(t)
	db := e.m.Store.DB()
	now := time.Now()
	old := now.Add(-25 * time.Hour).UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO gw_clients(client_id, redirect_uris, created_at) VALUES (?, ?, ?)`, []any{"stale", "https://x/cb", old}},
		{`INSERT INTO gw_clients(client_id, redirect_uris, created_at) VALUES (?, ?, ?)`, []any{"kept", "https://x/cb", old}},
		{`INSERT INTO gw_clients(client_id, redirect_uris, created_at) VALUES (?, ?, ?)`, []any{"recent", "https://x/cb", now.UnixMilli()}},
		{`INSERT INTO gw_tokens(hash, kind, agent, client_id, expires_at) VALUES (?, ?, ?, ?, ?)`, []any{"live", "refresh", "chatgpt", "kept", now.Add(time.Hour).UnixMilli()}},
		{`INSERT INTO gw_tokens(hash, kind, agent, client_id, expires_at) VALUES (?, ?, ?, ?, ?)`, []any{"dead", "access", "chatgpt", "recent", now.Add(-time.Minute).UnixMilli()}},
		{`INSERT INTO gw_codes(hash, kind, agent, expires_at) VALUES (?, ?, ?, ?)`, []any{"deadcode", "login", "chatgpt", now.Add(-time.Minute).UnixMilli()}},
		{`INSERT INTO gw_codes(hash, kind, agent, expires_at) VALUES (?, ?, ?, ?)`, []any{"livecode", "login", "chatgpt", now.Add(time.Minute).UnixMilli()}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if resp, out := register(t, e, `{"redirect_uris":["https://chatgpt.com/cb"]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %v", resp.StatusCode, out)
	}
	for id, want := range map[string]int{"stale": 0, "kept": 1, "recent": 1} {
		if got := countRows(t, e, `SELECT count(*) FROM gw_clients WHERE client_id = ?`, id); got != want {
			t.Errorf("client %s: %d rows, want %d", id, got, want)
		}
	}
	if got := countRows(t, e, `SELECT count(*) FROM gw_tokens WHERE hash = 'dead'`); got != 0 {
		t.Error("expired token not swept")
	}
	if got := countRows(t, e, `SELECT count(*) FROM gw_tokens WHERE hash = 'live'`); got != 1 {
		t.Error("live token swept")
	}
	if got := countRows(t, e, `SELECT count(*) FROM gw_codes WHERE hash = 'deadcode'`); got != 0 {
		t.Error("expired code not swept")
	}
	if got := countRows(t, e, `SELECT count(*) FROM gw_codes WHERE hash = 'livecode'`); got != 1 {
		t.Error("live code swept")
	}
}

func TestRegisterCapsClientCount(t *testing.T) {
	e := setup(t)
	tx, err := e.m.Store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for i := range 1000 {
		if _, err := tx.Exec(`INSERT INTO gw_clients(client_id, redirect_uris, created_at) VALUES (?, ?, ?)`, "c"+strconv.Itoa(i), "https://x/cb", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	resp, out := register(t, e, `{"redirect_uris":["https://chatgpt.com/cb"]}`)
	if resp.StatusCode != http.StatusTooManyRequests || out["error"] == nil {
		t.Fatalf("register at the cap: status %d, %v", resp.StatusCode, out)
	}
	if got := countRows(t, e, `SELECT count(*) FROM gw_clients`); got != 1000 {
		t.Fatalf("clients = %d, want 1000", got)
	}
}
