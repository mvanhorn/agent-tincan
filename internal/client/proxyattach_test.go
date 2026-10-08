package client_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

const proxiedNotes = "hello notes"

// relayProxy is a forward proxy that, for a call whose Proxy-Authorization
// carries pass, answers as a relay holding one attachment (att1, the text
// proxiedNotes) and one release file, and stores uploads; any other call
// gets 407. With rotate set, the password it accepts becomes rotate once it
// has answered a capabilities call, as when the sandbox's password expires
// between an upload's capability check and the upload itself. It records
// the password each call carried and the body of each upload it accepted.
func relayProxy(t *testing.T, pass, rotate string) (proxy *url.URL, seen func() []string, uploads func() []string) {
	t.Helper()
	var mu sync.Mutex
	var passwords, bodies []string
	sum := sha256.Sum256([]byte(proxiedNotes))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := ""
		if raw, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Basic "); ok {
			if dec, err := base64.StdEncoding.DecodeString(raw); err == nil {
				_, got, _ = strings.Cut(string(dec), ":")
			}
		}
		mu.Lock()
		passwords = append(passwords, got)
		ok := got == pass
		if ok && rotate != "" && r.URL.Path == "/v1/capabilities" {
			pass = rotate
		}
		mu.Unlock()
		if !ok {
			w.Header().Set("Proxy-Authenticate", `Basic realm="egress"`)
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/capabilities":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"attachments":true}`))
		case r.Method == "GET" && r.URL.Path == "/v1/attachments/att1":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set(client.AttachmentSHA256Header, hex.EncodeToString(sum[:]))
			io.WriteString(w, proxiedNotes)
		case r.Method == "POST" && r.URL.Path == "/v1/attachments":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(body))
			mu.Unlock()
			s := sha256.Sum256(body)
			json.NewEncoder(w).Encode(client.UploadedAttachment{ID: "att2", Name: r.URL.Query().Get("name"), Size: int64(len(body)), SHA256: hex.EncodeToString(s[:])})
		case r.Method == "GET" && r.URL.Path == "/v1/dist/tincan_linux_amd64":
			io.WriteString(w, "release-binary")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	pu, _ := url.Parse(ts.URL)
	return pu, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(passwords)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(bodies)
		}
}

// proxiedRelay is a client for a relay reached through proxy with password
// pass, saved at a fresh client.json whose path it returns with the config.
func proxiedRelay(t *testing.T, proxy *url.URL, pass string) (*client.Relay, client.Config, string) {
	t.Helper()
	clearProxyEnv(t)
	path := filepath.Join(t.TempDir(), "client.json")
	cfg := client.Config{Relay: "http://tincan-relay", Proxy: "http://u:" + pass + "@" + proxy.Host, Agent: "muse"}
	if err := client.SaveConfigTo(path, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := client.NewRelayForFile(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	return r, cfg, path
}

// rewriteProxyPassword saves cfg at path with the proxy password pass, as a
// wrapper that mints fresh credentials does.
func rewriteProxyPassword(t *testing.T, cfg client.Config, path string, proxy *url.URL, pass string) {
	t.Helper()
	cfg.Proxy = "http://u:" + pass + "@" + proxy.Host
	if err := client.SaveConfigTo(path, cfg); err != nil {
		t.Fatal(err)
	}
}

// An attachment download refused with 407 reads the config again and, when
// a wrapper has written fresh credentials there, is made once more with
// them, like any other relay call.
func TestDownloadAttachmentProxy407RetriesWithRewrittenConfig(t *testing.T) {
	proxy, seen, _ := relayProxy(t, "fresh", "")
	r, cfg, path := proxiedRelay(t, proxy, "stale")
	rewriteProxyPassword(t, cfg, path, proxy, "fresh")
	var buf bytes.Buffer
	d, err := r.DownloadAttachment(t.Context(), "att1", &buf)
	if err != nil || buf.String() != proxiedNotes || !d.Verified {
		t.Fatalf("DownloadAttachment = %+v, %q, %v", d, buf.String(), err)
	}
	if got := seen(); !slices.Equal(got, []string{"stale", "fresh"}) {
		t.Fatalf("proxy saw passwords %q, want stale then fresh", got)
	}
}

// With nothing fresher in the config, a refused download ends with a
// ProxyAuthError that never shows the password, and writes nothing.
func TestDownloadAttachmentProxy407FailsClearly(t *testing.T) {
	proxy, seen, _ := relayProxy(t, "fresh", "")
	r, _, _ := proxiedRelay(t, proxy, "stale-secret")
	var buf bytes.Buffer
	_, err := r.DownloadAttachment(t.Context(), "att1", &buf)
	if !client.IsProxyAuth(err) {
		t.Fatalf("DownloadAttachment error = %v, want a proxy authentication error", err)
	}
	if strings.Contains(err.Error(), "stale-secret") {
		t.Fatalf("error leaks the proxy password: %q", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("refused download wrote %q", buf.String())
	}
	if got := seen(); len(got) != 1 {
		t.Fatalf("proxy saw %d calls (%q), want 1 with no retry", len(got), got)
	}
}

// An upload refused with 407 is sent again with the rewritten credentials,
// with its whole body: the retry starts the file over.
func TestUploadAttachmentProxy407RetriesWithRewrittenConfig(t *testing.T) {
	proxy, seen, uploads := relayProxy(t, "old", "fresh")
	r, cfg, path := proxiedRelay(t, proxy, "old")
	rewriteProxyPassword(t, cfg, path, proxy, "fresh")
	body := strings.Repeat("upload body ", 1000)
	up, err := r.UploadAttachment(t.Context(), "notes.txt", "text/plain", strings.NewReader(body), int64(len(body)))
	if err != nil || up.ID != "att2" || up.Size != int64(len(body)) {
		t.Fatalf("UploadAttachment = %+v, %v", up, err)
	}
	if got := uploads(); len(got) != 1 || got[0] != body {
		t.Fatalf("relay stored %d uploads, want the whole body once", len(got))
	}
	if got := seen(); !slices.Equal(got, []string{"old", "old", "fresh"}) {
		t.Fatalf("proxy saw passwords %q, want old (capabilities), old (refused upload), fresh", got)
	}
}

// A refused upload with nothing fresher ends with a ProxyAuthError that
// never shows the password.
func TestUploadAttachmentProxy407FailsClearly(t *testing.T) {
	proxy, seen, uploads := relayProxy(t, "old-secret", "fresh")
	r, _, _ := proxiedRelay(t, proxy, "old-secret")
	_, err := r.UploadAttachment(t.Context(), "notes.txt", "text/plain", strings.NewReader("hi"), 2)
	if !client.IsProxyAuth(err) {
		t.Fatalf("UploadAttachment error = %v, want a proxy authentication error", err)
	}
	if strings.Contains(err.Error(), "old-secret") {
		t.Fatalf("error leaks the proxy password: %q", err)
	}
	if len(uploads()) != 0 || len(seen()) != 2 {
		t.Fatalf("proxy saw %d calls and stored %d uploads, want capabilities and one refused upload", len(seen()), len(uploads()))
	}
}

// A refused upload whose body cannot be read again (a stream) is not sent
// a second time, even with fresh credentials in the config: it ends with a
// ProxyAuthError, and the next call uses the fresh credentials.
func TestUploadStreamProxy407IsNotResent(t *testing.T) {
	proxy, seen, uploads := relayProxy(t, "old", "fresh")
	r, cfg, path := proxiedRelay(t, proxy, "old")
	rewriteProxyPassword(t, cfg, path, proxy, "fresh")
	_, err := r.UploadAttachment(t.Context(), "notes.txt", "text/plain", io.MultiReader(strings.NewReader("hi")), 2)
	if !client.IsProxyAuth(err) {
		t.Fatalf("UploadAttachment error = %v, want a proxy authentication error", err)
	}
	if got := seen(); !slices.Equal(got, []string{"old", "old"}) || len(uploads()) != 0 {
		t.Fatalf("proxy saw passwords %q and stored %d uploads, want capabilities and one refused upload", got, len(uploads()))
	}
	if _, err := r.Capabilities(t.Context()); err != nil {
		t.Fatalf("next call: %v", err)
	}
}

// A release download for tincan upgrade goes through the proxy too, and
// picks up rewritten credentials the same way.
func TestDownloadDistProxy407RetriesWithRewrittenConfig(t *testing.T) {
	proxy, seen, _ := relayProxy(t, "fresh", "")
	r, cfg, path := proxiedRelay(t, proxy, "stale")
	rewriteProxyPassword(t, cfg, path, proxy, "fresh")
	var buf bytes.Buffer
	if err := r.DownloadDist(t.Context(), "tincan_linux_amd64", &buf); err != nil || buf.String() != "release-binary" {
		t.Fatalf("DownloadDist = %q, %v", buf.String(), err)
	}
	if got := seen(); !slices.Equal(got, []string{"stale", "fresh"}) {
		t.Fatalf("proxy saw passwords %q, want stale then fresh", got)
	}
}
