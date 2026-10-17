package phone

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
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
		Prefix:                                "text/html",
		Prefix + "app.js":                     "text/javascript",
		Prefix + "sw.js":                      "text/javascript",
		Prefix + "style.css":                  "text/css",
		Prefix + "manifest.webmanifest":       "application/manifest+json",
		Prefix + "icons/icon-192.png":         "image/png",
		Prefix + "icons/icon-512.png":         "image/png",
		Prefix + "icons/apple-touch-icon.png": "image/png",
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
	for _, p := range []string{Prefix + "missing.js", Prefix + "icons", Prefix + "icons/", Prefix + "phone.go", "/elsewhere/app.js"} {
		if w := get(t, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
}

// The CSP allows only the page's own script, styles, fetches and worker,
// and no framing.
func TestCSP(t *testing.T) {
	for _, d := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "form-action 'none'", "base-uri 'none'"} {
		if !strings.Contains(CSP, d) {
			t.Errorf("CSP lacks %q", d)
		}
	}
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "*", "data:", "http:", "https:"} {
		if strings.Contains(CSP, bad) {
			t.Errorf("CSP allows %q", bad)
		}
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(files, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Agent-supplied text (names, good-at lines, request text, attachment
// names) reaches the page only as text: no script builds markup from a
// string, evaluates code, or loads anything from another origin.
func TestScriptsInsertTextOnly(t *testing.T) {
	sinks := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*["']on|\.src\s*=|srcdoc|createContextualFragment|DOMParser`)
	for _, name := range []string{"app.js", "sw.js"} {
		src := read(t, name)
		if m := sinks.FindString(src); m != "" {
			t.Errorf("%s uses %q", name, m)
		}
		if strings.Contains(src, "http://") || strings.Contains(src, "https://") {
			t.Errorf("%s names an absolute URL", name)
		}
	}
	if !strings.Contains(read(t, "app.js"), "textContent") {
		t.Error("app.js should set text with textContent")
	}
}

// The page has no inline script, style or event handler (the CSP would
// block them) and loads only its own files.
func TestPageLoadsOnlyItsOwnFiles(t *testing.T) {
	html := read(t, "index.html")
	for _, m := range regexp.MustCompile(`<script[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, `src="app.js"`) {
			t.Errorf("inline or foreign script: %s", m)
		}
	}
	for _, bad := range []string{"<style", " style=", " onclick=", " onload=", "http://", "https://", "javascript:"} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html contains %q", bad)
		}
	}
	for _, ref := range regexp.MustCompile(`(?:href|src)="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if _, err := fs.Stat(files, ref[1]); err != nil {
			t.Errorf("index.html references %s, which is not served", ref[1])
		}
	}
	if !strings.Contains(html, `aria-live="polite"`) {
		t.Error("index.html has no live region for announcements")
	}
	if !strings.Contains(html, "viewport-fit=cover") {
		t.Error("index.html does not cover the safe areas")
	}
}

// The manifest makes the page installable from Prefix, with icons that
// are served.
func TestManifest(t *testing.T) {
	var m struct {
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if err := json.Unmarshal([]byte(read(t, "manifest.webmanifest")), &m); err != nil {
		t.Fatal(err)
	}
	if m.StartURL != "./" || m.Scope != "./" || m.Display != "standalone" || len(m.Icons) == 0 {
		t.Fatalf("manifest = %+v", m)
	}
	for _, ic := range m.Icons {
		if _, err := fs.Stat(files, ic.Src); err != nil {
			t.Errorf("manifest icon %s is not served", ic.Src)
		}
	}
}

// Deny comes before Approve on every held card, and both have accessible
// names.
func TestDenyBeforeApprove(t *testing.T) {
	src := read(t, "app.js")
	deny, approve := strings.Index(src, `el("button", "deny"`), strings.Index(src, `el("button", "approve"`)
	if deny < 0 || approve < 0 || deny > approve {
		t.Fatalf("deny at %d, approve at %d: Deny must be built first", deny, approve)
	}
	if !strings.Contains(src, `actions.append(deny, approve)`) {
		t.Error("Deny must come first in the card's actions")
	}
	if strings.Count(src, `setAttribute("aria-label"`) < 2 {
		t.Error("Deny and Approve need accessible names")
	}
	if !strings.Contains(read(t, "style.css"), "min-height: 44px") {
		t.Error("buttons must be at least 44px tall")
	}
}
