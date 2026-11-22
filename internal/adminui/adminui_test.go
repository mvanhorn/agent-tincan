package adminui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// Every file is served under Prefix with its type, the page's CSP and no
// sniffing; anything else is 404.
func TestHandlerServesTheFiles(t *testing.T) {
	want := map[string]string{
		Prefix:               "text/html",
		Prefix + "app.js":    "text/javascript",
		Prefix + "style.css": "text/css",
	}
	for p, ctype := range want {
		w := get(t, p)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), ctype) {
			t.Errorf("GET %s: %d %q, want 200 %s", p, w.Code, w.Header().Get("Content-Type"), ctype)
		}
		if w.Header().Get("Content-Security-Policy") != CSP || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s headers: %v", p, w.Header())
		}
	}
	for _, p := range []string{Prefix + "missing.js", Prefix + "icons", "/elsewhere/app.js"} {
		if w := get(t, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
}

// The CSP allows only the page's own script, styles and fetches, and no
// framing.
func TestCSP(t *testing.T) {
	for _, d := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "form-action 'none'", "base-uri 'none'"} {
		if !strings.Contains(CSP, d) {
			t.Errorf("CSP lacks %q", d)
		}
	}
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "data:", "http:", "https:"} {
		if strings.Contains(CSP, bad) {
			t.Errorf("CSP allows %q", bad)
		}
	}
}

// The page loads its data from the relay's existing admin routes.
func TestPageReadsAdminRoutes(t *testing.T) {
	w := get(t, Prefix+"app.js")
	body := w.Body.String()
	for _, route := range []string{"/v1/agents", "/v1/admin/urls", "/v1/admin/kinds", "/v1/admin/invite", "/v1/admin/remove"} {
		if !strings.Contains(body, route) {
			t.Errorf("page does not read %s", route)
		}
	}
}
