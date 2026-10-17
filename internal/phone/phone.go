// Package phone is the owner's installable page: the live mesh with each
// agent's status, the asks held for approval, and approve and deny. The
// relay serves it at Prefix on its tailnet listeners, to admin devices
// only; the page reads and acts through the relay's existing routes.
//
// The page is static. Everything it shows from the relay (agent names,
// good-at lines, request text, attachment names) is inserted as text, never
// as markup, and its Content-Security-Policy runs only its own script.
package phone

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var embedded embed.FS

// files is the page's files, rooted at the static directory.
var files = func() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

// Prefix is where the relay serves the page. A browser may open exactly
// this address directly (relay.HardenForBrowsers); everything else it
// loads comes from the page itself.
const Prefix = "/phone/"

// CSP is the page's Content-Security-Policy: its own script, style, images,
// manifest and service worker, fetches only to the relay, and no framing,
// forms or base changes.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; manifest-src 'self'; worker-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// contentTypes are the types of the page's files, by extension.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".png":         "image/png",
}

// Handler serves the page's files under Prefix, Prefix itself being the
// page. It checks no identity: the relay calls it only for admin devices.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, Prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if name == "" {
			name = "index.html"
		}
		ctype, known := contentTypes[path.Ext(name)]
		if !known || !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", ctype)
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// The relay may be upgraded under an installed page; always
		// revalidate so the page and the relay stay in step.
		h.Set("Cache-Control", "no-cache")
		if r.Method == http.MethodHead {
			return
		}
		// A write error means the browser went away; there is no one to tell.
		_, _ = w.Write(body)
	})
}
