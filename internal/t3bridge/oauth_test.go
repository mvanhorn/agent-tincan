package t3bridge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateOrigin(t *testing.T) {
	ok := []struct {
		in, want string
	}{
		{"http://127.0.0.1:3773", "http://127.0.0.1:3773"},
		{"http://localhost:3773/", "http://localhost:3773"},
		{"http://[::1]:3773/mcp", "http://[::1]:3773"},
		{"https://t3.example.com", "https://t3.example.com"},
		{"https://t3.example.com/mcp", "https://t3.example.com"},
	}
	for _, tc := range ok {
		got, err := ValidateOrigin(tc.in)
		if err != nil {
			t.Fatalf("ValidateOrigin(%q) error = %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ValidateOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	bad := []string{
		"",
		"http://t3.example.com",
		"http://100.64.0.1:3773",
		"ftp://127.0.0.1:3773",
		"https://user:pass@t3.example.com",
		"http://127.0.0.1:3773?foo=bar",
		"http://127.0.0.1:3773/extra/path",
	}
	for _, raw := range bad {
		if _, err := ValidateOrigin(raw); err == nil {
			t.Fatalf("ValidateOrigin(%q) succeeded, want error", raw)
		}
	}
}

func TestDiscoverLocalOrigin(t *testing.T) {
	home := t.TempDir()
	ud := filepath.Join(home, "userdata")
	if err := os.MkdirAll(ud, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ud, "server-runtime.json"), []byte(`{"origin":"http://127.0.0.1:3773","port":3773,"serverVersion":"0.1.13-nightly"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := DiscoverLocalOrigin(home)
	if err != nil {
		t.Fatalf("DiscoverLocalOrigin: %v", err)
	}
	if rt.Origin != "http://127.0.0.1:3773" || rt.ServerVersion != "0.1.13-nightly" {
		t.Fatalf("unexpected runtime: %+v", rt)
	}
}

func TestSaveAndLoadAuthPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "priv")
	path := filepath.Join(dir, "t3-auth.json")
	want := AuthConfig{
		Origin:   "http://127.0.0.1:3773",
		Token:    "super-secret-bearer-token",
		ClientID: "cid-1",
		IssuedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveAuth(path, want); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	got, err := LoadAuth(path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if got.Token != want.Token || got.Origin != want.Origin {
		t.Fatalf("LoadAuth mismatch: %+v", got)
	}
	if sum := got.Summary(); sum.Origin != want.Origin || sum.ClientID != "cid-1" {
		t.Fatalf("Summary mismatch: %+v", sum)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuth(path); err == nil {
		t.Fatal("LoadAuth accepted 0644 file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuth(path); err == nil {
		t.Fatal("LoadAuth accepted 0755 dir")
	}
}

func TestRunOAuthPKCE(t *testing.T) {
	var seenChallenge, seenRedirect string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              srv.URL + "/mcp",
				"authorization_servers": []string{srv.URL},
			})
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 srv.URL,
				"authorization_endpoint": srv.URL + "/oauth/mcp/authorize",
				"token_endpoint":         srv.URL + "/oauth/mcp/token",
				"registration_endpoint":  srv.URL + "/oauth/mcp/register",
			})
		case "/oauth/mcp/register":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad json", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "reg-client-42"})
		case "/oauth/mcp/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", 400)
				return
			}
			verifier := r.FormValue("code_verifier")
			sum := sha256.Sum256([]byte(verifier))
			gotChal := base64.RawURLEncoding.EncodeToString(sum[:])
			if gotChal != seenChallenge {
				http.Error(w, "pkce mismatch", 400)
				return
			}
			if r.FormValue("code") != "auth-code-1" || r.FormValue("client_id") != "reg-client-42" || r.FormValue("redirect_uri") != seenRedirect {
				http.Error(w, "param mismatch", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "t3-tok-abc",
				"token_type":   "Bearer",
				"expires_in":   3600,
				"scope":        "orchestration:read orchestration:operate",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg, err := RunOAuthPKCE(context.Background(), OAuthOptions{
		Origin: srv.URL,
		OpenBrowser: func(raw string) error {
			u, err := url.Parse(raw)
			if err != nil {
				return err
			}
			seenChallenge = u.Query().Get("code_challenge")
			seenRedirect = u.Query().Get("redirect_uri")
			state := u.Query().Get("state")
			go func() {
				resp, err := http.Get(seenRedirect + "?code=auth-code-1&state=" + url.QueryEscape(state))
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
			return nil
		},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunOAuthPKCE: %v", err)
	}
	if cfg.Token != "t3-tok-abc" || cfg.ClientID != "reg-client-42" || cfg.ExpiresAt.IsZero() {
		t.Fatalf("unexpected auth config: %+v", cfg)
	}
}

func TestRunOAuthPKCEStateMismatch(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{srv.URL}})
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 srv.URL,
				"authorization_endpoint": srv.URL + "/oauth/mcp/authorize",
				"token_endpoint":         srv.URL + "/oauth/mcp/token",
			})
		}
	}))
	defer srv.Close()

	_, err := RunOAuthPKCE(context.Background(), OAuthOptions{
		Origin: srv.URL,
		OpenBrowser: func(raw string) error {
			u, _ := url.Parse(raw)
			redir := u.Query().Get("redirect_uri")
			go func() {
				resp, err := http.Get(redir + "?code=abc&state=wrong-state")
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
			return nil
		},
		Timeout: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("want state mismatch error, got %v", err)
	}
}
