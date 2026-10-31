package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// leaseHarness is a harness on a fake clock shared with its store, cut to
// the milliseconds the store keeps.
func leaseHarness(t *testing.T) (*harness, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Now().Truncate(time.Millisecond)}
	h := newHarness(t, Config{Now: clock.Now})
	h.st.SetClock(clock.Now)
	return h, clock
}

func acquire(h *harness, addr, body string, want int) client.Lease {
	h.t.Helper()
	var out client.Lease
	h.do(addr, "POST", "/v1/leases", body, want, &out)
	return out
}

func listLeases(h *harness, addr string) []client.Lease {
	h.t.Helper()
	var out struct {
		Leases []client.Lease `json:"leases"`
	}
	h.do(addr, "GET", "/v1/leases", "", http.StatusOK, &out)
	return out.Leases
}

// The holder is the authenticated caller, a new lease answers 201, a
// renewal 200, every joined agent can read the list, and each change is an
// audit row that names the resource and never carries the note.
func TestLeaseAcquireRenewAndList(t *testing.T) {
	h, clock := leaseHarness(t)
	got := acquire(h, grokAddr, `{"resource":"repo/internal/relay/*","ttl_seconds":3600,"note":"private-note-text"}`, http.StatusCreated)
	if got.Holder != "grokbot" || got.Renewed || !got.ExpiresAt.Equal(clock.Now().Add(time.Hour)) {
		t.Fatalf("acquire = %+v", got)
	}
	got = acquire(h, grokAddr, `{"resource":"repo/internal/relay/*","ttl_seconds":7200}`, http.StatusOK)
	if !got.Renewed || got.Note != "private-note-text" || !got.ExpiresAt.Equal(clock.Now().Add(2*time.Hour)) {
		t.Fatalf("renew = %+v", got)
	}
	for _, addr := range []string{grokAddr, museAddr, macAddr} {
		if l := listLeases(h, addr); len(l) != 1 || l[0].Holder != "grokbot" || l[0].Note != "private-note-text" {
			t.Fatalf("list from %s = %+v", addr, l)
		}
	}
	h.do(strangerAddr, "GET", "/v1/leases", "", http.StatusForbidden, nil)
	h.do(strangerAddr, "POST", "/v1/leases", `{"resource":"x"}`, http.StatusForbidden, nil)
	for _, ev := range []string{"lease_acquired", "lease_renewed"} {
		rows := auditEvents(h, ev)
		if len(rows) != 1 || rows[0].Actor != "grokbot" || !strings.Contains(rows[0].Detail, "repo/internal/relay/*") {
			t.Fatalf("%s = %+v", ev, rows)
		}
		if strings.Contains(rows[0].Detail, "private-note-text") {
			t.Fatalf("%s leaks the note: %s", ev, rows[0].Detail)
		}
	}
}

// A missing ttl_seconds means one hour.
func TestLeaseDefaultTTL(t *testing.T) {
	h, clock := leaseHarness(t)
	got := acquire(h, grokAddr, `{"resource":"a"}`, http.StatusCreated)
	if !got.ExpiresAt.Equal(clock.Now().Add(time.Hour)) {
		t.Fatalf("expires = %v", got.ExpiresAt)
	}
}

// A conflict answers 409, naming the holder and the expiry in the message
// and in fields of their own.
func TestLeaseConflictNamesHolderAndExpiry(t *testing.T) {
	h, _ := leaseHarness(t)
	held := acquire(h, museAddr, `{"resource":"a/*"}`, http.StatusCreated)
	var out struct {
		Error     string    `json:"error"`
		Resource  string    `json:"resource"`
		Holder    string    `json:"holder"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	h.do(grokAddr, "POST", "/v1/leases", `{"resource":"a/b"}`, http.StatusConflict, &out)
	if out.Holder != "muse" || out.Resource != "a/*" || !out.ExpiresAt.Equal(held.ExpiresAt) {
		t.Fatalf("conflict = %+v", out)
	}
	if !strings.Contains(out.Error, "muse") || !strings.Contains(out.Error, held.ExpiresAt.UTC().Format(time.RFC3339)) {
		t.Fatalf("error = %q", out.Error)
	}
	if rows := auditEvents(h, "lease_acquired"); len(rows) != 1 {
		t.Fatalf("a refused acquire changed nothing, audit = %+v", rows)
	}
}

// Bad input never reaches the table.
func TestLeaseRefusesBadInput(t *testing.T) {
	h, _ := leaseHarness(t)
	long := strings.Repeat("x", 121)
	for _, body := range []string{
		`{}`,
		`{"resource":"   "}`,
		`{"resource":"a\nb"}`,
		`{"resource":"` + long + `"}`,
		`{"resource":"a","note":"line\nbreak"}`,
		`{"resource":"a","note":"` + long + `"}`,
		`{"resource":"a","ttl_seconds":-1}`,
		`{"resource":"a","ttl_seconds":86401}`,
		`{"resource":"a","ttl_seconds":90000}`,
		`{"resource":"a","ttl_seconds":9223372036854775807}`,
		`{"resource":"a","holder":"muse"}`,
		`{"resource":"a"} {"resource":"b"}`,
	} {
		h.do(grokAddr, "POST", "/v1/leases", body, http.StatusBadRequest, nil)
	}
	h.do(grokAddr, "DELETE", "/v1/leases", `{}`, http.StatusBadRequest, nil)
	if l := listLeases(h, grokAddr); len(l) != 0 {
		t.Fatalf("leases = %+v", l)
	}
	acquire(h, grokAddr, `{"resource":"a","ttl_seconds":86400}`, http.StatusCreated)
}

// The 33rd live lease is refused.
func TestLeaseCapPerAgent(t *testing.T) {
	h, _ := leaseHarness(t)
	for i := range LeaseMaxPerAgent {
		acquire(h, grokAddr, fmt.Sprintf(`{"resource":"r/%d"}`, i), http.StatusCreated)
	}
	h.do(grokAddr, "POST", "/v1/leases", `{"resource":"r/last"}`, http.StatusConflict, nil)
	acquire(h, museAddr, `{"resource":"m"}`, http.StatusCreated)
}

// Only the holder or an admin releases. The admin's is audited as that,
// from the device that did it.
func TestLeaseRelease(t *testing.T) {
	h, _ := leaseHarness(t)
	acquire(h, museAddr, `{"resource":"a"}`, http.StatusCreated)
	h.do(grokAddr, "DELETE", "/v1/leases", `{"resource":"a"}`, http.StatusForbidden, nil)
	h.do(strangerAddr, "DELETE", "/v1/leases", `{"resource":"a"}`, http.StatusForbidden, nil)
	if l := listLeases(h, grokAddr); len(l) != 1 {
		t.Fatalf("refused release dropped it: %+v", l)
	}
	var out client.Lease
	h.do(museAddr, "DELETE", "/v1/leases", `{"resource":"a"}`, http.StatusOK, &out)
	if out.Holder != "muse" || out.Resource != "a" {
		t.Fatalf("release = %+v", out)
	}
	h.do(museAddr, "DELETE", "/v1/leases", `{"resource":"a"}`, http.StatusNotFound, nil)

	acquire(h, museAddr, `{"resource":"b"}`, http.StatusCreated)
	h.do(macAddr, "DELETE", "/v1/leases", `{"resource":"b"}`, http.StatusForbidden, nil)
	h.do(macAddr, "DELETE", "/v1/leases", `{"resource":"b","admin":true}`, http.StatusOK, nil)
	rows := auditEvents(h, "lease_released")
	if len(rows) != 2 {
		t.Fatalf("lease_released = %+v", rows)
	}
	if rows[0].Actor != "muse" || !strings.Contains(rows[0].Detail, `"reason":"holder"`) {
		t.Fatalf("holder release row = %+v", rows[0])
	}
	if rows[1].Actor == "muse" || !strings.Contains(rows[1].Detail, `"reason":"admin"`) || !strings.Contains(rows[1].Detail, `"device":"macbook-pro-44"`) || !strings.Contains(rows[1].Detail, `"holder":"muse"`) {
		t.Fatalf("admin release row = %+v", rows[1])
	}
}

// An agent on an admin device releases as itself: a teammate's lease is
// refused and its own is audited under its name. The admin path takes
// "admin": true, which gives nothing to a caller that is not an admin.
func TestLeaseReleaseAgentOnAdminDevice(t *testing.T) {
	h, _ := leaseHarness(t)
	h.joinAt(macAddr, "laptop")
	acquire(h, museAddr, `{"resource":"theirs"}`, http.StatusCreated)
	h.doAs(macAddr, "laptop", "POST", "/v1/leases", `{"resource":"mine"}`, http.StatusCreated, nil)

	h.doAs(macAddr, "laptop", "DELETE", "/v1/leases", `{"resource":"theirs"}`, http.StatusForbidden, nil)
	h.do(macAddr, "DELETE", "/v1/leases", `{"resource":"theirs"}`, http.StatusForbidden, nil)
	h.do(grokAddr, "DELETE", "/v1/leases", `{"resource":"theirs","admin":true}`, http.StatusForbidden, nil)
	if l := listLeases(h, museAddr); len(l) != 2 {
		t.Fatalf("a refused release dropped a lease: %+v", l)
	}

	h.doAs(macAddr, "laptop", "DELETE", "/v1/leases", `{"resource":"mine"}`, http.StatusOK, nil)
	h.doAs(macAddr, "laptop", "DELETE", "/v1/leases", `{"resource":"theirs","admin":true}`, http.StatusOK, nil)
	rows := auditEvents(h, "lease_released")
	if len(rows) != 2 {
		t.Fatalf("lease_released = %+v", rows)
	}
	if rows[0].Actor != "laptop" || !strings.Contains(rows[0].Detail, `"reason":"holder"`) {
		t.Fatalf("own release row = %+v", rows[0])
	}
	if !strings.Contains(rows[1].Detail, `"reason":"admin"`) || !strings.Contains(rows[1].Detail, `"holder":"muse"`) {
		t.Fatalf("admin release row = %+v", rows[1])
	}
}

// The relay host's admin socket reads and releases too.
func TestLeaseOverAdminSocket(t *testing.T) {
	h, _ := leaseHarness(t)
	acquire(h, museAddr, `{"resource":"a"}`, http.StatusCreated)
	local := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/leases", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.srv.AdminHandler().ServeHTTP(rec, req)
		return rec
	}
	rec := local("GET", "")
	var listed struct {
		Leases []client.Lease `json:"leases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || rec.Code != http.StatusOK || len(listed.Leases) != 1 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	if rec := local("DELETE", `{"resource":"a"}`); rec.Code != http.StatusOK {
		t.Fatalf("release = %d %s", rec.Code, rec.Body)
	}
}

// A lease lapses with no call from its holder. The sweep clears it, and the
// lapse is audited once, also when a teammate's acquire cleared the row.
func TestLeaseExpiryIsAudited(t *testing.T) {
	h, clock := leaseHarness(t)
	acquire(h, museAddr, `{"resource":"swept","ttl_seconds":60}`, http.StatusCreated)
	acquire(h, museAddr, `{"resource":"taken","ttl_seconds":60}`, http.StatusCreated)
	clock.advance(time.Minute)
	if l := listLeases(h, grokAddr); len(l) != 0 {
		t.Fatalf("lapsed leases listed: %+v", l)
	}
	if got := acquire(h, grokAddr, `{"resource":"taken"}`, http.StatusCreated); got.Holder != "grokbot" {
		t.Fatalf("take over = %+v", got)
	}
	h.srv.Sweep(t.Context())
	h.srv.Sweep(t.Context())
	rows := auditEvents(h, "lease_expired")
	if len(rows) != 2 {
		t.Fatalf("lease_expired = %+v", rows)
	}
	for _, e := range rows {
		if e.Actor != "relay" || !strings.Contains(e.Detail, `"holder":"muse"`) {
			t.Fatalf("row = %+v", e)
		}
	}
}

// tincan remove clears the agent's leases, audited under its name.
func TestRemoveClearsLeases(t *testing.T) {
	h, _ := leaseHarness(t)
	acquire(h, museAddr, `{"resource":"a"}`, http.StatusCreated)
	acquire(h, grokAddr, `{"resource":"b"}`, http.StatusCreated)
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"muse"}`, http.StatusOK, nil)
	l := listLeases(h, grokAddr)
	if len(l) != 1 || l[0].Holder != "grokbot" {
		t.Fatalf("leases = %+v", l)
	}
	rows := auditEvents(h, "lease_released")
	if len(rows) != 1 || !strings.Contains(rows[0].Detail, `"reason":"removed"`) || !strings.Contains(rows[0].Detail, `"holder":"muse"`) {
		t.Fatalf("lease_released = %+v", rows)
	}
	acquire(h, grokAddr, `{"resource":"a"}`, http.StatusCreated)
}

func TestCapabilitiesAdvertiseLeases(t *testing.T) {
	h, _ := leaseHarness(t)
	var caps client.Capabilities
	h.do(grokAddr, "GET", "/v1/capabilities", "", http.StatusOK, &caps)
	if !caps.Leases {
		t.Fatal("capabilities lack leases")
	}
}
