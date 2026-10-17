package relay_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// browserKeys is a push subscription's key pair as a browser makes it.
type browserKeys struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newBrowserKeys(t *testing.T) browserKeys {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	return browserKeys{priv, auth}
}

func (k browserKeys) json() map[string]string {
	return map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(k.priv.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(k.auth)}
}

// decrypt opens an aes128gcm Web Push body (RFC 8291) as the browser
// would.
func (k browserKeys) decrypt(t *testing.T, body []byte) string {
	t.Helper()
	if len(body) < 21 || binary.BigEndian.Uint32(body[16:20]) == 0 {
		t.Fatalf("push body is not aes128gcm: %d bytes", len(body))
	}
	salt, idlen := body[:16], int(body[20])
	serverPub, ct := body[21:21+idlen], body[21+idlen:]
	pub, err := ecdh.P256().NewPublicKey(serverPub)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := k.priv.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	info := append(append([]byte("WebPush: info\x00"), k.priv.PublicKey().Bytes()...), serverPub...)
	ikm, _ := hkdf.Key(sha256.New, secret, k.auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("decrypt push: %v", err)
	}
	plain = bytes.TrimRight(plain, "\x00")
	return string(plain[:len(plain)-1]) // drop the 0x02 delimiter
}

// pushService records what a fake push service receives and answers with
// status (after hang, if set).
type pushService struct {
	mu     sync.Mutex
	bodies [][]byte
	heads  []http.Header
	paths  []string
	status int
	hang   chan struct{}
	srv    *httptest.Server
}

func newPushService(t *testing.T, status int) *pushService {
	t.Helper()
	p := &pushService{status: status}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.bodies, p.heads, p.paths = append(p.bodies, b), append(p.heads, r.Header.Clone()), append(p.paths, r.URL.Path)
		hang := p.hang
		p.mu.Unlock()
		if hang != nil {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(p.status)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *pushService) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bodies)
}

func (p *pushService) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("push service got %d pushes, want %d", p.count(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pushMesh is a gated mesh (asks to muse are held, instinct is notified)
// with push on. The pusher accepts endpoints on loopback test servers and
// may dial them; the timeout is short.
func pushMesh(t *testing.T, timeout time.Duration) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	a := writeApproval(t, `{"gate":{"muse":{"from":"*"}},"notify":"instinct"}`)
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	key := make([]byte, 32)
	rand.Read(key)
	p, err := relay.NewPusher(key)
	if err != nil {
		t.Fatal(err)
	}
	relay.LoosenPusherForTest(p, timeout)
	m.Server.SetPusher(p)
	t.Cleanup(m.Server.Stop)
	return m
}

func writeApproval(t *testing.T, body string) *policy.Approval {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// subscribe adds a push subscription for endpoint from the admin's page.
func subscribe(t *testing.T, m *testrelay.Mesh, endpoint string, keys map[string]string) (int, string) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"endpoint": endpoint, "keys": keys})
	code, body, _ := browserCall(t, "PUT", hardened(t, m, "admin"), "/v1/admin/push/subscription", "127.0.0.1", string(in), browserJSON)
	return code, body
}

func auditEvents(t *testing.T, m *testrelay.Mesh, event string) []store.AuditEvent {
	t.Helper()
	all, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AuditEvent
	for _, e := range all {
		if e.Event == event {
			out = append(out, e)
		}
	}
	return out
}

func waitAudit(t *testing.T, m *testrelay.Mesh, event string, n int) []store.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := auditEvents(t, m, event); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %d %s audit events", n, event)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A held ask sends one push to each subscription. The decrypted payload
// says only that a request is waiting, with a count, and the audit names
// the subscription by id, never its endpoint or keys.
func TestPushOnHeldAsk(t *testing.T) {
	m := pushMesh(t, 2*time.Second)
	svc := newPushService(t, http.StatusCreated)
	k1, k2 := newBrowserKeys(t), newBrowserKeys(t)
	if code, body := subscribe(t, m, svc.srv.URL+"/push/one", k1.json()); code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	if code, body := subscribe(t, m, svc.srv.URL+"/push/two", k2.json()); code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	if subs := waitAudit(t, m, "push_subscribed", 2); strings.Contains(subs[0].Detail, svc.srv.URL) || !strings.Contains(subs[0].Detail, "macbook-pro-44") {
		t.Fatalf("push_subscribed detail %q", subs[0].Detail)
	}

	holdOne(t, m)
	svc.waitFor(t, 2)
	time.Sleep(100 * time.Millisecond)
	if svc.count() != 2 {
		t.Fatalf("pushes = %d, want one per subscription", svc.count())
	}
	keys := map[string]browserKeys{"/push/one": k1, "/push/two": k2}
	for i, b := range svc.bodies {
		h := svc.heads[i]
		if h.Get("Urgency") != "high" || h.Get("TTL") == "" || h.Get("TTL") == "0" || !strings.HasPrefix(h.Get("Authorization"), "vapid ") {
			t.Errorf("push headers: %v", h)
		}
		payload := keys[svc.paths[i]].decrypt(t, b)
		if strings.Contains(payload, "bank") || strings.Contains(payload, "grokbot") || strings.Contains(payload, "muse") {
			t.Fatalf("push payload carries request content: %s", payload)
		}
		var msg struct {
			Body  string `json:"body"`
			Count int    `json:"count"`
			URL   string `json:"url"`
		}
		if err := json.Unmarshal([]byte(payload), &msg); err != nil || msg.Body != "A request is waiting for your approval" || msg.Count != 1 || msg.URL != "/phone/#held" {
			t.Fatalf("push payload %s: %+v %v", payload, msg, err)
		}
	}
	for _, e := range waitAudit(t, m, "push_sent", 2) {
		if strings.Contains(e.Detail, svc.srv.URL) || strings.Contains(e.Detail, k1.json()["auth"]) || !strings.Contains(e.Detail, `"status":201`) {
			t.Fatalf("push_sent detail %q", e.Detail)
		}
	}
}

// A push service that answers 410 has dropped the subscription, so the
// relay deletes it.
func TestPushGoneDeletesSubscription(t *testing.T) {
	m := pushMesh(t, 2*time.Second)
	svc := newPushService(t, http.StatusGone)
	if code, body := subscribe(t, m, svc.srv.URL+"/push/gone", newBrowserKeys(t).json()); code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	holdOne(t, m)
	waitAudit(t, m, "push_unsubscribed", 1)
	if subs, err := m.Store.PushSubscriptions(t.Context()); err != nil || len(subs) != 0 {
		t.Fatalf("subscriptions after 410: %+v %v", subs, err)
	}
}

// A push service that hangs neither delays the send that held the ask nor
// changes the hold, and the approval notify teammate is still told.
func TestPushHangDoesNotStallSend(t *testing.T) {
	m := pushMesh(t, 300*time.Millisecond)
	svc := newPushService(t, http.StatusCreated)
	svc.hang = make(chan struct{})
	t.Cleanup(func() { close(svc.hang) })
	if code, body := subscribe(t, m, svc.srv.URL+"/push/slow", newBrowserKeys(t).json()); code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	start := time.Now()
	req := holdOne(t, m)
	if took := time.Since(start); took > 250*time.Millisecond {
		t.Fatalf("send took %s with a hanging push service", took)
	}
	svc.waitFor(t, 1)
	sent := waitAudit(t, m, "push_sent", 1)
	if !strings.Contains(sent[0].Detail, "timeout") {
		t.Fatalf("push_sent detail %q, want a timeout", sent[0].Detail)
	}
	if _, st, err := m.Store.Request(t.Context(), req.ID); err != nil || st != envelope.StatusHeld {
		t.Fatalf("request after the push timed out: %s %v", st, err)
	}
	in, err := m.Client(t, "instinct").Poll(t.Context(), time.Millisecond)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].From != "relay" || !strings.Contains(in.Requests[0].Body, req.ID) {
		t.Fatalf("notify teammate inbox: %+v %v", in.Requests, err)
	}
}

// Only https endpoints on the known push services are accepted, with
// well-formed browser keys, and only from an admin device.
func TestPushSubscriptionValidation(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	key := make([]byte, 32)
	rand.Read(key)
	p, err := relay.NewPusher(key)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPusher(p)
	t.Cleanup(m.Server.Stop)
	keys := newBrowserKeys(t).json()
	for _, ep := range []string{
		"https://web.push.apple.com/QGuQ", "https://fcm.googleapis.com/fcm/send/abc", "https://updates.push.services.mozilla.com/wpush/v2/x", "https://wns2-par02p.notify.windows.com/w/?token=x",
	} {
		if code, body := subscribe(t, m, ep, keys); code != http.StatusOK {
			t.Errorf("subscribe %s: %d %s", ep, code, body)
		}
	}
	for _, ep := range []string{
		"http://web.push.apple.com/QGuQ", "https://evil.example/push", "https://web.push.apple.com.evil.example/x", "https://notify.windows.com.evil.example/x",
		"https://user@web.push.apple.com/x", "https://web.push.apple.com:8443/x", "https://100.64.0.1/push", "ftp://fcm.googleapis.com/x", "",
	} {
		if code, _ := subscribe(t, m, ep, keys); code != http.StatusBadRequest {
			t.Errorf("subscribe %q: %d, want 400", ep, code)
		}
	}
	if code, _ := subscribe(t, m, "https://web.push.apple.com/k", map[string]string{"p256dh": "short", "auth": keys["auth"]}); code != http.StatusBadRequest {
		t.Errorf("bad p256dh: %d, want 400", code)
	}
	in, _ := json.Marshal(map[string]any{"endpoint": "https://web.push.apple.com/z", "keys": keys})
	if code, _, _ := browserCall(t, "PUT", hardened(t, m, "grokbot"), "/v1/admin/push/subscription", "127.0.0.1", string(in), browserJSON); code != http.StatusForbidden {
		t.Errorf("a joined agent subscribing: %d, want 403", code)
	}
	if code, body, _ := browserCall(t, "GET", hardened(t, m, "admin"), "/v1/admin/push/key", "127.0.0.1", "", sameSite); code != http.StatusOK || !strings.Contains(body, "public_key") {
		t.Errorf("public key: %d %s", code, body)
	}
	if code, _, _ := browserCall(t, "GET", hardened(t, m, "stranger"), "/v1/admin/push/key", "127.0.0.1", "", sameSite); code != http.StatusForbidden {
		t.Errorf("stranger reading the push key: %d, want 403", code)
	}
	del, _ := json.Marshal(map[string]string{"endpoint": "https://web.push.apple.com/QGuQ"})
	if code, body, _ := browserCall(t, "DELETE", hardened(t, m, "admin"), "/v1/admin/push/subscription", "127.0.0.1", string(del), browserJSON); code != http.StatusOK {
		t.Errorf("unsubscribe: %d %s", code, body)
	}
	if len(auditEvents(t, m, "push_unsubscribed")) != 1 {
		t.Error("unsubscribe not audited")
	}
}

// The relay follows no redirect from a push service, and refuses to dial
// a private, loopback or tailnet address whatever name resolved to it.
func TestPushRefusesRedirectsAndPrivateAddresses(t *testing.T) {
	m := pushMesh(t, 2*time.Second)
	target := newPushService(t, http.StatusCreated)
	redirector := httptest.NewServer(http.RedirectHandler(target.srv.URL+"/push/x", http.StatusTemporaryRedirect))
	t.Cleanup(redirector.Close)
	if code, body := subscribe(t, m, redirector.URL+"/push/r", newBrowserKeys(t).json()); code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	holdOne(t, m)
	sent := waitAudit(t, m, "push_sent", 1)
	if !strings.Contains(sent[0].Detail, `"status":307`) || target.count() != 0 {
		t.Fatalf("redirect: %q, %d pushes reached the redirect target", sent[0].Detail, target.count())
	}

	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "100.64.0.1", "100.101.102.103", "100.127.255.254", "169.254.1.1", "0.0.0.0", "::1", "fd7a:115c:a1e0::1", "fe80::1", "::ffff:127.0.0.1", "224.0.0.1"} {
		if !relay.PushAddrRefused(ip) {
			t.Errorf("%s is not refused", ip)
		}
	}
	for _, ip := range []string{"17.253.144.10", "142.250.72.10", "2a00:1450:4001::200e", "100.128.0.1"} {
		if relay.PushAddrRefused(ip) {
			t.Errorf("%s is refused", ip)
		}
	}
	u, _ := url.Parse(target.srv.URL)
	if err := relay.DialPushForTest(t.Context(), u.Host); err == nil {
		t.Error("the real push dialer reached a loopback push service")
	}
}
