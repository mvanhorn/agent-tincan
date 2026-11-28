// Package t3bridge connects Agent Tincan to a local or remote T3 Code server
// over T3 Code's external /mcp endpoint (tools and SEP-3415 MCP Events).
package t3bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultAuthFileName sits beside the t3 teammate's TINCAN_CONFIG.
	DefaultAuthFileName = "t3-auth.json"
	maxMetadataBytes    = 1 << 20
	oauthHTTPTimeout    = 15 * time.Second
)

// AuthConfig holds the OAuth bearer credential for T3 Code's /mcp endpoint.
// Token must never be logged, printed in errors, or returned in status JSON.
type AuthConfig struct {
	Origin    string    `json:"origin"`
	Token     string    `json:"token"`
	ClientID  string    `json:"client_id,omitempty"`
	Scope     string    `json:"scope,omitempty"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// AuthSummary is the non-secret view of AuthConfig for doctor and logs.
type AuthSummary struct {
	Origin    string    `json:"origin"`
	ClientID  string    `json:"client_id,omitempty"`
	Scope     string    `json:"scope,omitempty"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Summary strips secrets from AuthConfig.
func (a AuthConfig) Summary() AuthSummary {
	return AuthSummary{
		Origin:    a.Origin,
		ClientID:  a.ClientID,
		Scope:     a.Scope,
		IssuedAt:  a.IssuedAt,
		ExpiresAt: a.ExpiresAt,
	}
}

// DefaultT3Home returns ~/.t3 unless T3CODE_HOME overrides it.
func DefaultT3Home(home string) string {
	if v := strings.TrimSpace(os.Getenv("T3CODE_HOME")); v != "" {
		return v
	}
	return filepath.Join(home, ".t3")
}

// AuthPathForConfig returns the default t3-auth.json path beside configPath.
func AuthPathForConfig(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), DefaultAuthFileName)
}

// ServerRuntime is the subset of ~/.t3/userdata/server-runtime.json we read.
type ServerRuntime struct {
	Origin        string `json:"origin"`
	Port          int    `json:"port"`
	PID           int    `json:"pid"`
	ServerVersion string `json:"serverVersion"`
}

// DiscoverLocalOrigin reads ~/.t3/userdata/server-runtime.json and validates its origin.
func DiscoverLocalOrigin(t3Home string) (ServerRuntime, error) {
	path := filepath.Join(t3Home, "userdata", "server-runtime.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return ServerRuntime{}, fmt.Errorf("read T3 server runtime state %s: %w", path, err)
	}
	var rt ServerRuntime
	if err := json.Unmarshal(raw, &rt); err != nil {
		return ServerRuntime{}, fmt.Errorf("parse T3 server runtime state %s: %w", path, err)
	}
	origin, err := ValidateOrigin(rt.Origin)
	if err != nil {
		return ServerRuntime{}, fmt.Errorf("T3 server runtime state %s: %w", path, err)
	}
	rt.Origin = origin
	return rt, nil
}

// ValidateOrigin checks that raw is an http loopback origin or an https origin,
// with no credentials, query, fragment, or extra path.
func ValidateOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("T3 origin URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid T3 origin URL: %w", err)
	}
	if u.User != nil {
		return "", errors.New("T3 origin URL must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("T3 origin URL must not contain query parameters or fragments")
	}
	cleanPath := strings.TrimRight(u.Path, "/")
	if cleanPath != "" && cleanPath != "/mcp" {
		return "", fmt.Errorf("T3 origin URL must not include path %q", u.Path)
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("T3 origin URL has no host")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !isLoopbackHost(host) {
			return "", fmt.Errorf("non-loopback T3 URL must use https (got %s://%s)", scheme, u.Host)
		}
	default:
		return "", fmt.Errorf("unsupported T3 URL scheme %q (want http on loopback or https)", u.Scheme)
	}
	return scheme + "://" + u.Host, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newNoRedirectClient builds an HTTP client that refuses redirects so bearer
// tokens and OAuth metadata calls never follow untrusted Location headers.
func newNoRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not allowed on T3 MCP requests")
		},
	}
}

// LoadAuth reads and validates a 0600 t3-auth.json file inside a 0700 directory.
func LoadAuth(path string) (AuthConfig, error) {
	if err := checkPrivateFile(path); err != nil {
		return AuthConfig{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return AuthConfig{}, fmt.Errorf("read T3 auth file %s: %w", path, err)
	}
	var cfg AuthConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return AuthConfig{}, fmt.Errorf("parse T3 auth file %s: %w", path, err)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return AuthConfig{}, fmt.Errorf("T3 auth file %s has no token", path)
	}
	if cfg.Origin != "" {
		origin, err := ValidateOrigin(cfg.Origin)
		if err != nil {
			return AuthConfig{}, fmt.Errorf("T3 auth file %s: %w", path, err)
		}
		cfg.Origin = origin
	}
	return cfg, nil
}

// LoadTokenFile reads a raw bearer token from a 0600 file inside a 0700 dir.
func LoadTokenFile(path string) (string, error) {
	if err := checkPrivateFile(path); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

// SaveAuth writes AuthConfig atomically at 0600 inside a 0700 directory.
func SaveAuth(path string, cfg AuthConfig) error {
	if strings.TrimSpace(cfg.Token) == "" {
		return errors.New("refusing to save empty T3 auth token")
	}
	if cfg.Origin != "" {
		origin, err := ValidateOrigin(cfg.Origin)
		if err != nil {
			return err
		}
		cfg.Origin = origin
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writePrivateAtomic(path, data)
}

func checkPrivateFile(path string) error {
	dir := filepath.Dir(path)
	di, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat credentials directory %s: %w", dir, err)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("credentials directory %s has mode %04o; it must be private (chmod 700 %s)", dir, perm, dir)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat credentials file %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("credentials file %s must not be a symlink", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("credentials file %s has mode %04o; it must be private (chmod 600 %s)", path, perm, path)
	}
	return nil
}

func writePrivateAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".t3-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// OAuthOptions configures the interactive OAuth 2.1 PKCE login against T3 Code.
type OAuthOptions struct {
	Origin      string
	ClientName  string
	Scopes      []string
	OpenBrowser func(string) error
	PrintURL    func(string)
	HTTPClient  *http.Client
	Timeout     time.Duration
}

type protectedResourceMeta struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type authServerMeta struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

// RunOAuthPKCE runs OAuth 2.1 PKCE with dynamic client registration against a
// T3 Code server's /mcp protected-resource metadata and returns the new AuthConfig.
func RunOAuthPKCE(ctx context.Context, opts OAuthOptions) (AuthConfig, error) {
	origin, err := ValidateOrigin(opts.Origin)
	if err != nil {
		return AuthConfig{}, err
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = newNoRedirectClient(oauthHTTPTimeout)
	}
	clientName := opts.ClientName
	if clientName == "" {
		clientName = "Agent Tincan"
	}
	scopes := opts.Scopes
	if len(scopes) == 0 {
		scopes = []string{"orchestration:read", "orchestration:operate"}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	// 1. Discover protected-resource and authorization-server metadata.
	var prm protectedResourceMeta
	if err := getJSON(ctx, httpClient, origin+"/.well-known/oauth-protected-resource/mcp", &prm); err != nil {
		return AuthConfig{}, fmt.Errorf("discover T3 OAuth resource metadata: %w", err)
	}
	asOrigin := origin
	if len(prm.AuthorizationServers) > 0 && prm.AuthorizationServers[0] != "" {
		validated, err := ValidateOrigin(prm.AuthorizationServers[0])
		if err != nil {
			return AuthConfig{}, fmt.Errorf("invalid T3 authorization server URL: %w", err)
		}
		asOrigin = validated
	}
	var asm authServerMeta
	if err := getJSON(ctx, httpClient, asOrigin+"/.well-known/oauth-authorization-server", &asm); err != nil {
		return AuthConfig{}, fmt.Errorf("discover T3 OAuth server metadata: %w", err)
	}
	if err := validateSameOriginEndpoint(asOrigin, asm.AuthorizationEndpoint); err != nil {
		return AuthConfig{}, fmt.Errorf("authorization endpoint: %w", err)
	}
	if err := validateSameOriginEndpoint(asOrigin, asm.TokenEndpoint); err != nil {
		return AuthConfig{}, fmt.Errorf("token endpoint: %w", err)
	}
	if asm.RegistrationEndpoint != "" {
		if err := validateSameOriginEndpoint(asOrigin, asm.RegistrationEndpoint); err != nil {
			return AuthConfig{}, fmt.Errorf("registration endpoint: %w", err)
		}
	}

	// 2. Bind a loopback callback listener before registering the client.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return AuthConfig{}, fmt.Errorf("listen for OAuth callback: %w", err)
	}
	defer ln.Close()
	redirectURI := fmt.Sprintf("http://%s/callback", ln.Addr().String())

	// 3. Dynamically register a public PKCE client when registration is advertised.
	clientID := "agent-tincan"
	if asm.RegistrationEndpoint != "" {
		regReq := map[string]any{
			"client_name":                clientName,
			"redirect_uris":              []string{redirectURI},
			"grant_types":                []string{"authorization_code"},
			"response_types":             []string{"code"},
			"token_endpoint_auth_method": "none",
		}
		var regResp struct {
			ClientID string `json:"client_id"`
		}
		if err := postJSON(ctx, httpClient, asm.RegistrationEndpoint, regReq, &regResp); err != nil {
			return AuthConfig{}, fmt.Errorf("register T3 OAuth client: %w", err)
		}
		if strings.TrimSpace(regResp.ClientID) == "" {
			return AuthConfig{}, errors.New("T3 OAuth registration returned empty client_id")
		}
		clientID = regResp.ClientID
	}

	// 4. Generate PKCE S256 verifier/challenge and CSRF state.
	verifier, err := randomURLToken(32)
	if err != nil {
		return AuthConfig{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state, err := randomHex(16)
	if err != nil {
		return AuthConfig{}, err
	}

	resource := prm.Resource
	if resource == "" {
		resource = origin + "/mcp"
	}
	authURL, err := url.Parse(asm.AuthorizationEndpoint)
	if err != nil {
		return AuthConfig{}, err
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", resource)
	authURL.RawQuery = q.Encode()

	// 5. Serve callback and launch browser.
	type cbResult struct {
		code string
		err  error
	}
	done := make(chan cbResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		rq := r.URL.Query()
		if errParam := rq.Get("error"); errParam != "" {
			desc := rq.Get("error_description")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "T3 Code authorization was denied. You can close this tab.\n")
			select {
			case done <- cbResult{err: fmt.Errorf("authorization failed (%s: %s)", errParam, desc)}:
			default:
			}
			return
		}
		if rq.Get("state") != state {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Invalid OAuth state. You can close this tab.\n")
			select {
			case done <- cbResult{err: errors.New("OAuth callback state mismatch")}:
			default:
			}
			return
		}
		code := strings.TrimSpace(rq.Get("code"))
		if code == "" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Missing authorization code. You can close this tab.\n")
			select {
			case done <- cbResult{err: errors.New("OAuth callback missing authorization code")}:
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Agent Tincan is now authorized with T3 Code. You can close this tab.\n")
		select {
		case done <- cbResult{code: code}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if opts.PrintURL != nil {
		opts.PrintURL(authURL.String())
	}
	if opts.OpenBrowser != nil {
		_ = opts.OpenBrowser(authURL.String())
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var code string
	select {
	case <-waitCtx.Done():
		return AuthConfig{}, fmt.Errorf("waiting for T3 Code OAuth approval: %w", waitCtx.Err())
	case res := <-done:
		if res.err != nil {
			return AuthConfig{}, res.err
		}
		code = res.code
	}

	// 6. Exchange authorization code for bearer token.
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	form.Set("resource", resource)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, asm.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return AuthConfig{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return AuthConfig{}, fmt.Errorf("exchange T3 OAuth code: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return AuthConfig{}, fmt.Errorf("read T3 OAuth token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return AuthConfig{}, fmt.Errorf("T3 OAuth token exchange failed: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return AuthConfig{}, fmt.Errorf("decode T3 OAuth token response: %w", err)
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return AuthConfig{}, errors.New("T3 OAuth token response contained no access_token")
	}
	now := time.Now().UTC()
	out := AuthConfig{
		Origin:   origin,
		Token:    tok.AccessToken,
		ClientID: clientID,
		Scope:    tok.Scope,
		IssuedAt: now,
	}
	if tok.ExpiresIn > 0 {
		out.ExpiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return out, nil
}

func validateSameOriginEndpoint(baseOrigin, endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("endpoint URL is empty")
	}
	bu, err := url.Parse(baseOrigin)
	if err != nil {
		return err
	}
	eu, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if eu.User != nil {
		return errors.New("endpoint must not contain credentials")
	}
	if !strings.EqualFold(bu.Scheme, eu.Scheme) || !strings.EqualFold(bu.Host, eu.Host) {
		return fmt.Errorf("endpoint origin %s://%s does not match server origin %s", eu.Scheme, eu.Host, baseOrigin)
	}
	return nil
}

func getJSON(ctx context.Context, hc *http.Client, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, target)
	}
	return json.Unmarshal(body, out)
}

func postJSON(ctx context.Context, hc *http.Client, target string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, target)
	}
	return json.Unmarshal(body, out)
}

func randomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
