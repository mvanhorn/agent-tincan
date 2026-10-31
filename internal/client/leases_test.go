package client_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// Acquire, list and release through a real relay; a conflict comes back as
// the relay's message, and a release from a teammate is refused.
func TestLeases(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	grok, muse := m.Client(t, "grokbot"), m.Client(t, "muse")
	got, err := client.AcquireLease(t.Context(), grok, "repo/*", "tests", 1800)
	if err != nil || got.Holder != "grokbot" || got.Renewed {
		t.Fatalf("acquire = %+v, %v", got, err)
	}
	if _, err := client.AcquireLease(t.Context(), muse, "repo/x", "", 0); !client.IsStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "grokbot") {
		t.Fatalf("conflict = %v", err)
	}
	if again, err := client.AcquireLease(t.Context(), grok, "repo/*", "", 3600); err != nil || !again.Renewed {
		t.Fatalf("renew = %+v, %v", again, err)
	}
	leases, err := client.ListLeases(t.Context(), muse)
	if err != nil || len(leases) != 1 {
		t.Fatalf("list = %+v, %v", leases, err)
	}
	if _, err := client.ReleaseLease(t.Context(), muse, "repo/*"); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("teammate release = %v", err)
	}
	if _, err := client.ReleaseLease(t.Context(), grok, "repo/*"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReleaseLease(t.Context(), grok, "repo/*"); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("second release = %v", err)
	}
}

// An older relay answers the new route 404 and advertises no leases, so
// each call fails with the upgrade message rather than a bare 404.
func TestLeasesOlderRelay(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"attachments":false}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer old.Close()
	r, err := client.NewRelay(old.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AcquireLease(t.Context(), r, "a", "", 0); !errors.Is(err, client.ErrLeasesUnsupported) {
		t.Fatalf("acquire = %v", err)
	}
	if _, err := client.ReleaseLease(t.Context(), r, "a"); !errors.Is(err, client.ErrLeasesUnsupported) {
		t.Fatalf("release = %v", err)
	}
	if _, err := client.ListLeases(t.Context(), r); !errors.Is(err, client.ErrLeasesUnsupported) {
		t.Fatalf("list = %v", err)
	}
}

// Resources and notes are teammates' text, so they print quoted, one line
// per lease.
func TestFormatLeases(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local)
	if got := client.FormatLeases(nil, now); !strings.Contains(got, "No leases") {
		t.Fatalf("empty = %q", got)
	}
	got := client.FormatLeases([]client.Lease{{Resource: "a/*", Holder: "muse", Note: "docs", ExpiresAt: now.Add(time.Hour)}, {Resource: "b", Holder: "codex", ExpiresAt: now.Add(time.Hour)}}, now)
	if lines := strings.Split(strings.TrimSpace(got), "\n"); len(lines) != 2 {
		t.Fatalf("lines = %q", got)
	}
	if !strings.Contains(got, `"a/*"`) || !strings.Contains(got, "muse") || !strings.Contains(got, `note="docs"`) {
		t.Fatalf("got = %q", got)
	}
}
