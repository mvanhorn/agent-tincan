package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
)

// Gateway serves the public MCP endpoint and OAuth for virtual agents.
type Gateway struct {
	base    string // public base URL, e.g. https://tincan-gateway.tail1234.ts.net
	oauth   *OAuth
	relay   http.Handler // the relay's agent API, called in-process
	version string

	mu       sync.Mutex
	failures []time.Time // recent bad login codes, for lockout
	servers  sync.Map    // agent name -> *mcp.Server, built once per agent
}

// New builds a gateway. relay is the relay's agent API handler; requests are
// forwarded to it in-process with a virtual remote address after the bearer
// token checks out, so the relay attributes them to the token's agent.
func New(base string, oauth *OAuth, relay http.Handler, version string) *Gateway {
	return &Gateway{base: strings.TrimRight(base, "/"), oauth: oauth, relay: relay, version: version}
}

// Handler is the public handler. Anything not listed here is 404.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", g.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", g.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", g.authServer)
	mux.HandleFunc("POST /register", g.register)
	mux.HandleFunc("GET /authorize", g.authorizePage)
	mux.HandleFunc("POST /authorize", g.authorizeSubmit)
	mux.HandleFunc("POST /token", g.token)
	mux.Handle("/mcp", g.requireToken(mcp.NewStreamableHTTPHandler(g.serverFor, &mcp.StreamableHTTPOptions{Stateless: true})))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client.LimitBody(w, r, client.Defaults(client.RelayAPI).MaxBodyBytes)
		mux.ServeHTTP(w, r)
	})
}

type agentKey struct{}

func (g *Gateway) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		agent, err := "", error(nil)
		if ok {
			agent, err = g.oauth.Validate(r.Context(), strings.TrimSpace(tok))
		}
		if !ok || err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+g.base+`/.well-known/oauth-protected-resource"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, agent)))
	})
}

// serverFor returns the MCP server whose relay calls are attributed to the
// token's agent, building it on first use.
func (g *Gateway) serverFor(r *http.Request) *mcp.Server {
	agent, _ := r.Context().Value(agentKey{}).(string)
	if s, ok := g.servers.Load(agent); ok {
		return s.(*mcp.Server)
	}
	rc := client.NewRelayHTTP("http://tincan-relay", &http.Client{Transport: inProcess{h: g.relay, addr: identity.VirtualAddr(agent)}})
	s, _ := g.servers.LoadOrStore(agent, mcpserver.New(rc, g.version))
	return s.(*mcp.Server)
}

// inProcess serves relay calls without a network hop, stamping the virtual
// remote address the relay's resolver maps to the agent.
type inProcess struct {
	h    http.Handler
	addr string
}

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.RemoteAddr = t.addr
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func (g *Gateway) protectedResource(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 g.base + "/mcp",
		"authorization_servers":    []string{g.base},
		"bearer_methods_supported": []string{"header"},
	})
}

func (g *Gateway) authServer(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                g.base,
		"authorization_endpoint":                g.base + "/authorize",
		"token_endpoint":                        g.base + "/token",
		"registration_endpoint":                 g.base + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// Limits on the public, unauthenticated registration endpoint.
const (
	registerMaxBody     = 16 << 10
	registerMaxRedirect = 5
	redirectMaxLen      = 2048
)

func (g *Gateway) register(w http.ResponseWriter, r *http.Request) {
	client.LimitBody(w, r, registerMaxBody)
	var in struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.RedirectURIs) == 0 {
		oauthErr(w, "invalid_client_metadata", "redirect_uris required")
		return
	}
	if len(in.RedirectURIs) > registerMaxRedirect {
		oauthErr(w, "invalid_client_metadata", "at most 5 redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if len(u) > redirectMaxLen {
			oauthErr(w, "invalid_redirect_uri", "redirect URI too long")
			return
		}
		if !validRedirect(u) {
			oauthErr(w, "invalid_redirect_uri", "redirect URIs must be https (or http on localhost)")
			return
		}
	}
	id, err := g.oauth.RegisterClient(r.Context(), strings.Join(in.RedirectURIs, "\n"))
	if errors.Is(err, ErrTooManyClients) {
		oauthErrStatus(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many registered clients; try again later")
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id": id, "redirect_uris": in.RedirectURIs, "client_name": in.ClientName,
		"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
	})
}

func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return u.Scheme == "https" || (u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1"))
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>Agent Tincan</title>
<style>body{font:16px system-ui;max-width:28rem;margin:4rem auto;padding:0 1rem}input{font:inherit;padding:.5rem;width:100%;box-sizing:border-box}button{font:inherit;padding:.5rem 1rem;margin-top:1rem}</style></head>
<body><h1>Connect to Agent Tincan</h1>
<p>Enter the one-time code from <code>tincan connect &lt;name&gt;</code>.</p>
{{if .Error}}<p style="color:#b00">{{.Error}}</p>{{end}}
<form method="post" action="/authorize">
<input name="login_code" placeholder="XXXX-XXXX" autocomplete="one-time-code" autofocus>
<input type="hidden" name="client_id" value="{{.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.Redirect}}">
<input type="hidden" name="code_challenge" value="{{.Challenge}}">
<input type="hidden" name="state" value="{{.State}}">
<button type="submit">Connect</button></form></body></html>`))

type authReq struct {
	ClientID, Redirect, Challenge, State, Error string
}

func (g *Gateway) checkAuthReq(ctx context.Context, a authReq, method string) string {
	uris, err := g.oauth.ClientRedirects(ctx, a.ClientID)
	if err != nil {
		return "unknown client"
	}
	if !slices.Contains(strings.Split(uris, "\n"), a.Redirect) {
		return "redirect_uri does not match the registered client"
	}
	if a.Challenge == "" || (method != "" && method != "S256") {
		return "PKCE S256 is required"
	}
	return ""
}

func (g *Gateway) authorizePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a := authReq{ClientID: q.Get("client_id"), Redirect: q.Get("redirect_uri"), Challenge: q.Get("code_challenge"), State: q.Get("state")}
	if q.Get("response_type") != "code" {
		http.Error(w, "response_type must be code", http.StatusBadRequest)
		return
	}
	if msg := g.checkAuthReq(r.Context(), a, q.Get("code_challenge_method")); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	if err := loginPage.Execute(w, a); err != nil {
		log.Printf("gateway login page: %v", err)
	}
}

func (g *Gateway) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	a := authReq{ClientID: r.PostForm.Get("client_id"), Redirect: r.PostForm.Get("redirect_uri"), Challenge: r.PostForm.Get("code_challenge"), State: r.PostForm.Get("state")}
	if msg := g.checkAuthReq(r.Context(), a, ""); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if g.lockedOut() {
		http.Error(w, "too many wrong codes; wait 10 minutes and mint a new code", http.StatusTooManyRequests)
		return
	}
	code, err := g.oauth.ExchangeLogin(r.Context(), strings.ToUpper(strings.TrimSpace(r.PostForm.Get("login_code"))), a.ClientID, a.Redirect, a.Challenge)
	if err != nil {
		g.fail()
		a.Error = "That code is wrong, used, or expired."
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = loginPage.Execute(w, a)
		return
	}
	u, _ := url.Parse(a.Redirect)
	q := u.Query()
	q.Set("code", code)
	if a.State != "" {
		q.Set("state", a.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// lockedOut stops brute-forcing the login code: five wrong codes in ten
// minutes locks the login page for everyone until the window passes.
func (g *Gateway) lockedOut() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	g.failures = slices.DeleteFunc(g.failures, func(t time.Time) bool { return t.Before(cutoff) })
	return len(g.failures) >= 5
}

func (g *Gateway) fail() {
	g.mu.Lock()
	g.failures = append(g.failures, time.Now())
	g.mu.Unlock()
}

func (g *Gateway) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthErr(w, "invalid_request", "bad form")
		return
	}
	f := r.PostForm
	var t Tokens
	var err error
	switch f.Get("grant_type") {
	case "authorization_code":
		t, err = g.oauth.ExchangeAuthCode(r.Context(), f.Get("code"), f.Get("client_id"), f.Get("redirect_uri"), f.Get("code_verifier"))
	case "refresh_token":
		t, err = g.oauth.Refresh(r.Context(), f.Get("refresh_token"), f.Get("client_id"))
	default:
		oauthErr(w, "unsupported_grant_type", "")
		return
	}
	if err != nil {
		oauthErr(w, "invalid_grant", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, t)
}

func oauthErr(w http.ResponseWriter, code, desc string) {
	oauthErrStatus(w, http.StatusBadRequest, code, desc)
}

func oauthErrStatus(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Connector implements relay.Connector for the gateway.
type Connector struct {
	Dir   *identity.Directory
	OAuth *OAuth
	Base  string
}

// Connect binds the virtual agent and mints a login code.
func (c Connector) Connect(ctx context.Context, name string) (string, string, error) {
	if err := c.Dir.BindVirtual(ctx, name); err != nil {
		return "", "", err
	}
	code, err := c.OAuth.MintLoginCode(ctx, name)
	return code, strings.TrimRight(c.Base, "/") + "/mcp", err
}

// Revoke drops the agent's tokens.
func (c Connector) Revoke(ctx context.Context, name string) error {
	return c.OAuth.RevokeAgent(ctx, name)
}
