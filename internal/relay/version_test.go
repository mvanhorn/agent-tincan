package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// whoamiAs calls whoami from addr as a client running the given build ("" for
// a client that predates the version header).
func whoamiAs(t *testing.T, h *harness, addr, version string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.RemoteAddr = addr
	if version != "" {
		req.Header.Set(client.VersionHeader, version)
	}
	req.Header.Set(client.PlatformHeader, "linux_amd64")
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami from %s: %d %s", addr, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The roster shows the build each agent last called with, taken from the
// client's version header, and the relay's own build beside it. A client
// that sends no header (one that predates it) leaves the agent's entry as it
// was, and a header that does not look like a build name is ignored.
func TestRosterShowsAgentAndRelayVersions(t *testing.T) {
	h := newHarness(t, Config{Version: "0.6.0"})
	h.send(grokAddr, "muse", "hi")
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "" {
		t.Fatalf("version with no header = %q", a.Version)
	}
	whoamiAs(t, h, grokAddr, "0.5.2")
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.2" {
		t.Fatalf("version = %q, want 0.5.2", a.Version)
	}
	for _, junk := range []string{"0.5.2 evil", "<script>", strings.Repeat("9", 65), " \t"} {
		whoamiAs(t, h, grokAddr, junk)
		if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.2" {
			t.Fatalf("after header %q version = %q, want 0.5.2 kept", junk, a.Version)
		}
	}
	h.send(grokAddr, "muse", "again")
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.2" {
		t.Fatalf("a later call without the header cleared the version: %q", a.Version)
	}
	whoamiAs(t, h, grokAddr, "0.5.2-3-gabcdef-dirty")
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.2-3-gabcdef-dirty" {
		t.Fatalf("a dev build name was not recorded: %q", a.Version)
	}
	if a := agentInfo(t, h, macAddr, "muse"); a.Version != "" {
		t.Fatalf("muse never called but has version %q", a.Version)
	}

	var roster struct {
		RelayVersion string `json:"relay_version"`
	}
	h.do(macAddr, "GET", "/v1/agents", "", http.StatusOK, &roster)
	if roster.RelayVersion != "0.6.0" {
		t.Fatalf("relay_version = %q", roster.RelayVersion)
	}
	if me := whoamiAs(t, h, grokAddr, ""); me["relay_version"] != "0.6.0" {
		t.Fatalf("whoami relay_version = %v", me["relay_version"])
	}

	// A relay that does not know its build says nothing about it.
	quiet := newHarness(t, Config{})
	rec := quiet.do(macAddr, "GET", "/v1/agents", "", http.StatusOK, nil)
	if strings.Contains(rec.Body.String(), "relay_version") {
		t.Fatalf("relay without a version reported one: %s", rec.Body.String())
	}
}

// The build is kept in the store, so a restarted relay's roster still shows
// it before the agent calls again.
func TestAgentVersionSurvivesRestart(t *testing.T) {
	h := newHarness(t, Config{})
	whoamiAs(t, h, grokAddr, "0.5.2")
	srv := New(identity.NewDirectory(h.st, h.who, identity.Config{Admins: []string{"macbook-pro-44"}}), h.st, Config{})
	h2 := &harness{t: t, srv: srv, h: srv.Handler(), st: h.st, who: h.who}
	if a := agentInfo(t, h2, macAddr, "grokbot"); a.Version != "0.5.2" {
		t.Fatalf("version after restart = %q", a.Version)
	}
}

// Removing an agent forgets what the relay remembered about the name, so an
// agent joined later under it starts clean.
func TestRemoveForgetsAgentVersionAndActivity(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_790_000_000, 0)}
	h := newHarness(t, Config{Now: clk.Now})
	whoamiAs(t, h, museAddr, "0.5.1")
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"muse"}`, http.StatusOK, nil)
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"muse"}`, http.StatusOK, &inv)
	h.do(museAddr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	a := agentInfo(t, h, macAddr, "muse")
	if a.Version != "" || !a.LastActive.IsZero() || !a.LastPoll.IsZero() || a.Online {
		t.Fatalf("rejoined muse still carries the removed agent's state: %+v", a)
	}
}

// Two builds calling under one name (an old listen or MCP process next to an
// upgraded CLI) write the store at most once a minute, not on every
// alternating call; the roster still shows the latest build at once.
func TestAlternatingBuildsThrottleVersionWrites(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_790_000_000, 0)}
	h := newHarness(t, Config{Now: clk.Now})
	stored := func() string {
		vs, err := h.st.AgentVersions(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return vs["grokbot"]
	}
	whoamiAs(t, h, grokAddr, "0.5.1")
	if got := stored(); got != "0.5.1" {
		t.Fatalf("first build not stored: %q", got)
	}
	whoamiAs(t, h, grokAddr, "0.5.3")
	whoamiAs(t, h, grokAddr, "0.5.1")
	whoamiAs(t, h, grokAddr, "0.5.3")
	if got := stored(); got != "0.5.1" {
		t.Fatalf("store rewritten within a minute: %q", got)
	}
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.3" {
		t.Fatalf("roster shows %q, want the latest build", a.Version)
	}
	clk.mu.Lock()
	clk.t = clk.t.Add(persistEvery)
	clk.mu.Unlock()
	whoamiAs(t, h, grokAddr, "0.5.3")
	if got := stored(); got != "0.5.3" {
		t.Fatalf("store did not catch up after a minute: %q", got)
	}
}

func TestUpgradeAvailable(t *testing.T) {
	h := newHarness(t, Config{})
	dir := t.TempDir()
	versionPath := filepath.Join(dir, "VERSION")
	if err := os.WriteFile(versionPath, []byte("0.5.5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tincan_linux_amd64"), []byte("binary"), 0600); err != nil {
		t.Fatal(err)
	}
	h.srv.SetDist(dir)
	for _, path := range []string{"/v1/whoami", "/v1/poll?hold=0", "/v1/poll?hold=0&peek=1"} {
		for _, version := range []string{"0.5.4", "0.5.5", "0.6.0", "", "dev", "0.5.4-rc.1", "0.5.4-3-gabcdef", "invalid"} {
			req := httptest.NewRequest("GET", path, nil)
			req.RemoteAddr = grokAddr
			req.Header.Set(client.VersionHeader, version)
			req.Header.Set(client.PlatformHeader, "linux_amd64")
			rec := httptest.NewRecorder()
			h.h.ServeHTTP(rec, req)
			var out map[string]any
			if rec.Code != http.StatusNoContent {
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
			}
			if version == "0.5.4" {
				if out["upgrade_available"] != "0.5.5" {
					t.Fatalf("%s: %s", path, rec.Body.String())
				}
				var legacy struct {
					Requests []client.Result `json:"requests"`
					Waiting  int             `json:"waiting"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &legacy); err != nil {
					t.Fatal(err)
				}
			} else if _, ok := out["upgrade_available"]; ok {
				t.Fatalf("%s version %q: unexpected notice %v", path, version, out)
			}
		}
	}
	h.send(museAddr, "grokbot", "hello")
	for _, path := range []string{"/v1/poll?hold=0&peek=1", "/v1/poll?hold=0"} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = grokAddr
		req.Header.Set(client.VersionHeader, "0.5.4")
		req.Header.Set(client.PlatformHeader, "linux_amd64")
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), `"upgrade_available":"0.5.5"`) {
			t.Fatalf("populated poll: %s", rec.Body.String())
		}
	}
	if err := os.WriteFile(versionPath, []byte("0.6.10"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := whoamiAs(t, h, grokAddr, "0.5.4")["upgrade_available"]; got != "0.6.10" {
		t.Fatalf("changed VERSION = %v", got)
	}

	if err := os.WriteFile(versionPath, []byte("0.6.11"), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(versionPath, later, later); err != nil {
		t.Fatal(err)
	}
	if got := whoamiAs(t, h, grokAddr, "0.5.4")["upgrade_available"]; got != "0.6.11" {
		t.Fatalf("mtime change = %v", got)
	}
	if err := os.WriteFile(versionPath, []byte("dev"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := whoamiAs(t, h, grokAddr, "0.5.4")["upgrade_available"]; got != nil {
		t.Fatalf("dev dist = %v", got)
	}
	if err := os.Remove(versionPath); err != nil {
		t.Fatal(err)
	}
	if got := whoamiAs(t, h, grokAddr, "0.5.4")["upgrade_available"]; got != nil {
		t.Fatalf("missing VERSION = %v", got)
	}
	h.srv.SetDist("")
	if got := whoamiAs(t, h, grokAddr, "0.5.4")["upgrade_available"]; got != nil {
		t.Fatalf("no dist = %v", got)
	}
}

func TestUpgradeAvailableNeedsPlatformBinaryAndSeesInPlaceEdits(t *testing.T) {
	h := newHarness(t, Config{})
	dir := t.TempDir()
	versionPath := filepath.Join(dir, "VERSION")
	if err := os.WriteFile(versionPath, []byte("0.5.5"), 0600); err != nil {
		t.Fatal(err)
	}
	h.srv.SetDist(dir)
	ask := func(platform string) any {
		req := httptest.NewRequest("GET", "/v1/whoami", nil)
		req.RemoteAddr = grokAddr
		req.Header.Set(client.VersionHeader, "0.5.4")
		if platform != "" {
			req.Header.Set(client.PlatformHeader, platform)
		}
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out["upgrade_available"]
	}
	if got := ask("darwin_arm64"); got != nil {
		t.Fatalf("VERSION staged before binaries = %v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "tincan_darwin_arm64"), []byte("binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ask("darwin_arm64"); got != "0.5.5" {
		t.Fatalf("staged binary = %v", got)
	}
	for _, platform := range []string{"", "linux_amd64", "windows_amd64", "windows_arm64", "../darwin_arm64"} {
		if got := ask(platform); got != nil {
			t.Fatalf("platform %q = %v", platform, got)
		}
	}
	// Windows clients send windows_<arch> and install the .exe.
	if err := os.WriteFile(filepath.Join(dir, "tincan_windows_amd64.exe"), []byte("binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ask("windows_amd64"); got != "0.5.5" {
		t.Fatalf("staged exe = %v", got)
	}
	for _, platform := range []string{"windows_arm64", "windows_amd64.exe"} {
		if got := ask(platform); got != nil {
			t.Fatalf("platform %q = %v", platform, got)
		}
	}
	// Same size, same mtime, rewritten in place: still seen.
	info, err := os.Stat(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versionPath, []byte("0.5.6"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(versionPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := ask("darwin_arm64"); got != "0.5.6" {
		t.Fatalf("in-place VERSION edit = %v", got)
	}
}
