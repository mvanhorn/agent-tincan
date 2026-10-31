package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/paths"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

// fakeRelay answers hello with a proof under key, and agents.
func fakeRelay(t *testing.T, key string) string {
	t.Helper()
	ts := httptest.NewServer(helloHandler(key))
	t.Cleanup(ts.Close)
	return ts.URL
}

// deadURL is an address where nothing listens any more.
func deadURL(t *testing.T) string {
	ts := httptest.NewServer(http.NotFoundHandler())
	u := ts.URL
	ts.Close()
	return u
}

func savedConfig(t *testing.T, c Config) {
	t.Helper()
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "client.json"))
	t.Setenv("TINCAN_RELAY", "")
	if err := SaveConfig(c); err != nil {
		t.Fatal(err)
	}
}

func TestRelayMovedIsFoundAndSaved(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	moved := fakeRelay(t, key)
	impostor := fakeRelay(t, "k-other")
	savedConfig(t, Config{Relay: old, Agent: "muse", RelayKey: key})
	r, err := NewRelayFor(Config{Relay: old, Agent: "muse", RelayKey: key})
	if err != nil {
		t.Fatal(err)
	}
	r.findRelays = func(context.Context, string) []string { return []string{impostor, moved} }

	agents, err := r.Agents(t.Context())
	if err != nil || len(agents) != 1 {
		t.Fatalf("call after the move: %v %v", agents, err)
	}
	findIdle(t, r) // the relay info refresh runs after the retry
	if r.Base() != moved {
		t.Fatalf("base %s, want %s (the peer that proved the key, not the impostor)", r.Base(), moved)
	}
	c, err := LoadConfig()
	if err != nil || c.Relay != moved || c.RelayKey != key {
		t.Fatalf("saved config %+v %v", c, err)
	}
	if len(c.RelayURLs) != 1 || c.RelayURLs[0] != "http://tincan-relay.example.ts.net" || c.RelayInfoAt.IsZero() {
		t.Fatalf("after the move, advertised URLs should be the live whoami list, got %+v", c)
	}
}

func TestRelayNotFollowedWithoutProof(t *testing.T) {
	old := deadURL(t)
	impostor := fakeRelay(t, "k-other")
	savedConfig(t, Config{Relay: old, RelayKey: "k-real"})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: "k-real"})
	r.findRelays = func(context.Context, string) []string { return []string{impostor} }
	if _, err := r.Agents(t.Context()); err == nil {
		t.Fatal("followed a peer that cannot prove the relay key")
	}
	if r.Base() != old {
		t.Fatalf("base changed to %s", r.Base())
	}
}

func TestRelayNotSearchedWithoutKey(t *testing.T) {
	old := deadURL(t)
	savedConfig(t, Config{Relay: old})
	r, _ := NewRelayFor(Config{Relay: old})
	searched := false
	r.findRelays = func(context.Context, string) []string { searched = true; return nil }
	_, _ = r.Agents(t.Context())
	if searched {
		t.Fatal("searched the tailnet without a relay key")
	}
}

func TestRelayErrorIsNotAMove(t *testing.T) {
	if unreachable(&APIError{Code: 403, Message: "not a joined agent"}) {
		t.Fatal("an answer from the relay is not a move")
	}
}

func TestLearnRelayKeySavesIt(t *testing.T) {
	url := fakeRelay(t, "k-learned")
	savedConfig(t, Config{Relay: url, Agent: "muse"})
	r, _ := NewRelayFor(Config{Relay: url, Agent: "muse"})
	LearnRelayKey(t.Context(), r)
	raw, _ := os.ReadFile(ConfigPath())
	var c Config
	_ = json.Unmarshal(raw, &c)
	if c.RelayKey != "k-learned" || c.Relay != url {
		t.Fatalf("config %+v", c)
	}
}

func TestRelayFoundAtItsAdvertisedNameWithoutTailscale(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	named := fakeRelay(t, key)
	savedConfig(t, Config{Relay: old, RelayKey: key, RelayURLs: []string{named}})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: key, RelayURLs: []string{named}})
	r.findRelays = func(context.Context, string) []string { return nil } // a proxy-only sandbox
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.Base() != named {
		t.Fatalf("base %s, want the advertised %s", r.Base(), named)
	}
	findIdle(t, r)
}

func TestProxyGatewayErrorsMeanUnreachable(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		if !unreachable(&APIError{Code: code}) {
			t.Errorf("%d through a proxy should count as the relay not answering", code)
		}
	}
}

func TestLearnRelayInfoSavesURLsAndRefreshes(t *testing.T) {
	url := fakeRelay(t, "k")
	savedConfig(t, Config{Relay: url})
	r, _ := NewRelayFor(Config{Relay: url})
	LearnRelayKey(t.Context(), r)
	c, _ := LoadConfig()
	if c.RelayKey != "k" || c.RelayInfoAt.IsZero() || NeedsRelayInfo(c) {
		t.Fatalf("config %+v", c)
	}
	c.RelayInfoAt = c.RelayInfoAt.Add(-2 * relayInfoEvery)
	if !NeedsRelayInfo(c) {
		t.Fatal("day-old relay info should be refreshed")
	}
}

// readConfig reads a config file as saved, with no environment overrides.
func readConfig(t *testing.T, path string) Config {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// A service loads its config from --config, not ConfigPath(). The key it
// learns and the address of a moved relay go to that file, and the default
// config (another agent's) is never touched.
func TestNewRelayForFileWritesToItsOwnFile(t *testing.T) {
	const key = "k-svc"
	url := fakeRelay(t, key)
	savedConfig(t, Config{Relay: url, Agent: "codex"}) // ConfigPath(): another agent
	own := filepath.Join(t.TempDir(), "history.json")
	if err := SaveConfigTo(own, Config{Relay: url, Agent: "history"}); err != nil {
		t.Fatal(err)
	}
	r, err := NewRelayForFile(Config{Relay: url, Agent: "history"}, own)
	if err != nil {
		t.Fatal(err)
	}
	LearnRelayKey(t.Context(), r)
	if r.key != key {
		t.Fatalf("key learned %q", r.key)
	}
	if c := readConfig(t, own); c.RelayKey != key || c.Agent != "history" || len(c.RelayURLs) == 0 || c.RelayInfoAt.IsZero() {
		t.Fatalf("own config %+v", c)
	}
	if c := readConfig(t, ConfigPath()); c.RelayKey != "" || c.Agent != "codex" {
		t.Fatalf("ConfigPath() was written: %+v", c)
	}

	// The relay moves. Both files name the old address, so the old code
	// (which always wrote ConfigPath()) would have rewritten the wrong one.
	old := deadURL(t)
	if err := SaveConfig(Config{Relay: old, Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfigTo(own, Config{Relay: old, Agent: "history", RelayKey: key}); err != nil {
		t.Fatal(err)
	}
	r, _ = NewRelayForFile(Config{Relay: old, Agent: "history", RelayKey: key}, own)
	r.findRelays = func(context.Context, string) []string { return []string{url} }
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	findIdle(t, r) // the relay info refresh runs after the retry
	if c := readConfig(t, own); c.Relay != url || c.RelayKey != key {
		t.Fatalf("own config after the move %+v", c)
	}
	if got := readConfig(t, own); len(got.RelayURLs) != 1 || got.RelayURLs[0] != "http://tincan-relay.example.ts.net" {
		t.Fatalf("own advertised URLs after the move %+v", got)
	}
	if c := readConfig(t, ConfigPath()); c.Relay != old {
		t.Fatalf("ConfigPath() followed the move for another agent: %+v", c)
	}
}

// TINCAN_RELAY overrides the saved relay for one process only: nothing is
// written to any file, whichever constructor built the client.
func TestEnvRelayOverrideNeverWrites(t *testing.T) {
	const key = "k-env"
	url := fakeRelay(t, key)
	savedConfig(t, Config{Relay: url, Agent: "muse"})
	own := filepath.Join(t.TempDir(), "history.json")
	if err := SaveConfigTo(own, Config{Relay: url, Agent: "history"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_RELAY", url)
	for name, build := range map[string]func() (*Relay, error){
		"NewRelayFor":     func() (*Relay, error) { return NewRelayFor(Config{Relay: url, Agent: "muse"}) },
		"NewRelayForFile": func() (*Relay, error) { return NewRelayForFile(Config{Relay: url, Agent: "history"}, own) },
	} {
		r, err := build()
		if err != nil {
			t.Fatal(err)
		}
		LearnRelayKey(t.Context(), r)
		if r.key != key {
			t.Fatalf("%s: the key is still handed out under TINCAN_RELAY, got %q", name, r.key)
		}
		if c := readConfig(t, ConfigPath()); c.RelayKey != "" {
			t.Fatalf("%s: ConfigPath() written under TINCAN_RELAY: %+v", name, c)
		}
		if c := readConfig(t, own); c.RelayKey != "" {
			t.Fatalf("%s: --config file written under TINCAN_RELAY: %+v", name, c)
		}
	}

	// A move is followed in memory but not written either.
	old := deadURL(t)
	t.Setenv("TINCAN_RELAY", old)
	if err := SaveConfigTo(own, Config{Relay: old, Agent: "history", RelayKey: key}); err != nil {
		t.Fatal(err)
	}
	r, _ := NewRelayForFile(Config{Relay: old, Agent: "history", RelayKey: key}, own)
	r.findRelays = func(context.Context, string) []string { return []string{url} }
	if _, err := r.Agents(t.Context()); err != nil || r.Base() != url {
		t.Fatalf("move: %v, base %s", err, r.Base())
	}
	if c := readConfig(t, own); c.Relay != old {
		t.Fatalf("--config file rewritten under TINCAN_RELAY: %+v", c)
	}
}

func TestIPv4sFromStatusJSONIncludesOfflineAndSkipsNames(t *testing.T) {
	raw := []byte(`{
		"Self": {"TailscaleIPs": ["100.64.0.30", "fd7a:115c:a1e0::1"], "HostName": "self-host", "Online": true},
		"Peer": {
			"n1": {"TailscaleIPs": ["100.64.0.10", "fd7a:115c:a1e0::2"], "HostName": "old-host", "Online": false},
			"n2": {"TailscaleIPs": ["100.64.0.20"], "HostName": "live-host", "Online": false},
			"n3": {"TailscaleIPs": ["100.1.2.3"], "HostName": "later-name", "Online": true},
			"n4": {"TailscaleIPs": ["fd7a:115c:a1e0::9"], "HostName": "v6-only", "Online": true}
		}
	}`)
	got, err := ipv4sFromStatusJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"100.64.0.30", "100.64.0.10", "100.64.0.20", "100.1.2.3"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want every IPv4 including offline, not host names or IPv6", got)
	}
}

func TestPeerURLsUseBasePortAndSkipCurrentHost(t *testing.T) {
	got := peerURLs("http://100.64.0.10:8787", []string{"100.64.0.10", "100.64.0.20", "100.1.2.3", "100.64.0.20"})
	want := []string{"http://100.64.0.20:8787", "http://100.1.2.3:8787"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNetmapPrefersLocalAPIWhenCLIMissing(t *testing.T) {
	t.Cleanup(SwapNetmapLookups(
		func(context.Context) ([]string, error) { return []string{"100.64.0.20"}, nil },
		func(context.Context) ([]string, error) { return nil, errors.New("no cli") },
	))
	ips, src := netmapIPv4s(t.Context())
	if src != "localapi" || !slices.Equal(ips, []string{"100.64.0.20"}) {
		t.Fatalf("netmap %v via %s", ips, src)
	}
}

func TestNetmapFallsBackToCLI(t *testing.T) {
	t.Cleanup(SwapNetmapLookups(
		func(context.Context) ([]string, error) { return nil, errors.New("no localapi") },
		func(context.Context) ([]string, error) { return []string{"100.64.0.20", "100.1.2.3"}, nil },
	))
	ips, src := netmapIPv4s(t.Context())
	if src != "cli" || !slices.Equal(ips, []string{"100.64.0.20", "100.1.2.3"}) {
		t.Fatalf("netmap %v via %s", ips, src)
	}
}

func TestNetmapEmptyWithoutLocalAPIOrCLI(t *testing.T) {
	t.Cleanup(SwapNetmapLookups(
		func(context.Context) ([]string, error) { return nil, errors.New("no localapi") },
		func(context.Context) ([]string, error) { return nil, errors.New("no cli") },
	))
	ips, src := netmapIPv4s(t.Context())
	if len(ips) != 0 || src != "" {
		t.Fatalf("netmap %v via %q", ips, src)
	}
}

func TestRelayMovedFromStaleIPFollowsProvingPeer(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	staleName := deadURL(t)
	moved := fakeRelay(t, key)
	other := fakeRelay(t, "k-other")
	savedConfig(t, Config{Relay: old, Agent: "muse", RelayKey: key, RelayURLs: []string{staleName}})
	r, err := NewRelayFor(Config{Relay: old, Agent: "muse", RelayKey: key, RelayURLs: []string{staleName}})
	if err != nil {
		t.Fatal(err)
	}
	r.findRelays = func(context.Context, string) []string { return []string{other, moved} }
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.Base() != moved {
		t.Fatalf("base %s, want the proving peer", r.Base())
	}
	findIdle(t, r) // the relay info refresh runs after the retry
	c, _ := LoadConfig()
	if c.Relay != moved || slices.Contains(c.RelayURLs, staleName) {
		t.Fatalf("saved %+v still names the dead advertised URL", c)
	}
}

// hangingURL is a relay address that accepts calls and never answers.
func hangingURL(t *testing.T) string {
	t.Helper()
	stop := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); ts.Close() })
	return ts.URL
}

// waitFor fails t unless ok turns true within 5s.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A call that runs out its own time (tincan wait --timeout, a timed-out
// inbox check) returns at its deadline. The search it set off goes on by
// itself, once, and the next call uses what it found.
func TestTimedOutCallReturnsAtDeadlineAndSearchStillLands(t *testing.T) {
	const key = "k-real"
	old := hangingURL(t)
	moved := fakeRelay(t, key)
	savedConfig(t, Config{Relay: old, RelayKey: key})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: key})
	var searches atomic.Int32
	r.findRelays = func(ctx context.Context, _ string) []string {
		searches.Add(1)
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
		return []string{moved}
	}
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		start := time.Now()
		_, err := r.Agents(ctx)
		cancel()
		if err == nil {
			t.Fatal("the hanging relay answered")
		}
		if d := time.Since(start); d > 700*time.Millisecond {
			t.Fatalf("call took %s past its 200ms deadline; the search held it", d)
		}
	}
	waitFor(t, "the background search to move the client", func() bool { return r.Base() == moved })
	findIdle(t, r) // the refresh after the move writes the config file
	if n := searches.Load(); n != 1 {
		t.Fatalf("%d searches, want one shared by both timed-out calls", n)
	}
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatalf("next call after the search: %v", err)
	}
	if c, _ := LoadConfig(); c.Relay != moved {
		t.Fatalf("saved relay %s, want %s", c.Relay, moved)
	}
}

// A cancelled caller (a stopping service) ends the search instead of
// holding shutdown for it.
func TestCancelledCallerStopsSearch(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	savedConfig(t, Config{Relay: old, RelayKey: key})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: key})
	started, stopped := make(chan struct{}), make(chan struct{})
	r.findRelays = func(ctx context.Context, _ string) []string {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := r.Agents(ctx); done <- err }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the call waited for the search after its caller was cancelled")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the search kept running after its caller was cancelled")
	}
}

// A caller already cancelled does not start a search at all.
func TestCancelledCallerDoesNotSearch(t *testing.T) {
	old := deadURL(t)
	savedConfig(t, Config{Relay: old, RelayKey: "k-real"})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: "k-real"})
	searched := false
	r.findRelays = func(context.Context, string) []string { searched = true; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r.relocate(ctx, &net.OpError{Op: "dial", Err: errors.New("connection refused")}) || searched {
		t.Fatal("a cancelled caller searched")
	}
}

// The whoami refresh after a move gets its own few seconds, not what is
// left of the search.
func TestRefreshAfterMoveHasItsOwnDeadline(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	moved := fakeRelay(t, key)
	savedConfig(t, Config{Relay: old, RelayKey: key})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: key})
	r.findRelays = func(context.Context, string) []string { return []string{moved} }
	oldFor, oldLearn := relocateFor, learnAfterMove
	t.Cleanup(func() { relocateFor, learnAfterMove = oldFor, oldLearn })
	relocateFor = 300 * time.Millisecond
	left := make(chan time.Duration, 1)
	learnAfterMove = func(ctx context.Context, r *Relay) {
		d, ok := ctx.Deadline()
		if !ok || ctx.Err() != nil {
			left <- 0
			return
		}
		left <- time.Until(d)
		LearnRelayKey(ctx, r)
	}
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d := <-left; d < refreshFor-time.Second {
		t.Fatalf("refresh had %s, want about %s of its own", d, refreshFor)
	}
	findIdle(t, r)
}

func TestIPv4sFromLocalStatusIncludesOffline(t *testing.T) {
	st := &ipnstate.Status{
		Self: &ipnstate.PeerStatus{
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.30"), netip.MustParseAddr("fd7a:115c:a1e0::1")},
			Online:       true,
		},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.10")}, Online: false, HostName: "old-host"},
			key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.20")}, Online: false, HostName: "live-host"},
			key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.1.2.3")}, Online: true, HostName: "later-name"},
			key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::9")}, Online: true},
		},
	}
	got := ipv4sFromLocalStatus(st)
	want := []string{"100.64.0.30", "100.64.0.10", "100.64.0.20", "100.1.2.3"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want every IPv4 including offline, not host names or IPv6", got)
	}
}

// swapLocalStatus makes localAPINetmap's LocalAPI call report whether it
// ran instead of dialing tailscaled.
func swapLocalStatus(t *testing.T, platform string) *bool {
	t.Helper()
	asked := new(bool)
	oldGOOS, oldStatus, oldProc, oldHome := goos, localStatus, procRoot, userHome
	goos = platform
	localStatus = func(context.Context, *local.Client) (*ipnstate.Status, error) {
		*asked = true
		return &ipnstate.Status{}, nil
	}
	// No userspace tailscaled on this machine counts either.
	empty := t.TempDir()
	procRoot, userHome = filepath.Join(empty, "proc"), func() (string, error) { return empty, nil }
	t.Cleanup(func() { goos, localStatus, procRoot, userHome = oldGOOS, oldStatus, oldProc, oldHome })
	return asked
}

// TS_SOCKET naming a missing file means no tailscaled there: fail at once
// so a proxy 502 retry is not stalled on LocalAPI's dial timeout.
func TestLocalAPINetmapFailsFastWithMissingTSSocket(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		asked := swapLocalStatus(t, platform)
		t.Setenv("TS_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
		if _, err := localAPINetmap(t.Context()); err == nil || *asked {
			t.Fatalf("%s: err %v, asked LocalAPI %v; want an immediate miss", platform, err, *asked)
		}
	}
}

// On Linux the default socket is the only way to tailscaled, so a missing
// one fails at once too.
func TestLocalAPINetmapFailsFastWithoutLinuxSocket(t *testing.T) {
	if _, err := os.Stat(paths.DefaultTailscaledSocket()); err == nil {
		t.Skip("this machine has a tailscaled socket")
	}
	asked := swapLocalStatus(t, "linux")
	t.Setenv("TS_SOCKET", "")
	if _, err := localAPINetmap(t.Context()); err == nil || *asked {
		t.Fatalf("err %v, asked LocalAPI %v; want an immediate miss", err, *asked)
	}
}

// The macOS Tailscale app has no socket file; LocalAPI is a localhost TCP
// port with a token, which the local client finds by itself. A launchd
// service must still ask it rather than stop at the missing socket.
func TestLocalAPINetmapOnDarwinDoesNotNeedSocketFile(t *testing.T) {
	asked := swapLocalStatus(t, "darwin")
	t.Setenv("TS_SOCKET", "")
	if _, err := localAPINetmap(t.Context()); err != nil || !*asked {
		t.Fatalf("err %v, asked LocalAPI %v; want the darwin default to reach LocalAPI", err, *asked)
	}
}

// Production FindRelay path: LocalAPI IPs on the saved URL's port, no
// findRelays hook, follow only the peer that proves the key.
func TestRelayMovedFollowsLocalAPIAddresses(t *testing.T) {
	const key = "k-real"
	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(liveLn.Addr().(*net.TCPAddr).Port)
	impLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		liveLn.Close()
		t.Skip("cannot bind 127.0.0.2")
	}
	serveHello(t, liveLn, key)
	serveHello(t, impLn, "k-other")
	old := "http://" + net.JoinHostPort("127.0.0.3", port)
	want := "http://" + net.JoinHostPort("127.0.0.1", port)
	savedConfig(t, Config{Relay: old, Agent: "muse", RelayKey: key})
	t.Cleanup(SwapNetmapLookups(
		func(context.Context) ([]string, error) {
			return []string{"127.0.0.3", "127.0.0.2", "127.0.0.1"}, nil
		},
		func(context.Context) ([]string, error) { return nil, errors.New("no cli") },
	))
	r, err := NewRelayFor(Config{Relay: old, Agent: "muse", RelayKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.Base() != want {
		t.Fatalf("base %s, want the LocalAPI peer that proved the key", r.Base())
	}
	listed, src, _ := r.LastFind()
	if src != "localapi" || listed != 2 {
		t.Fatalf("LastFind listed=%d source=%s, want 2 via localapi (current host skipped)", listed, src)
	}
	findIdle(t, r)
}

func serveHello(t *testing.T, ln net.Listener, key string) {
	t.Helper()
	srv := &http.Server{Handler: helloHandler(key)}
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()
}

func helloHandler(key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			_ = json.NewEncoder(w).Encode(map[string]string{"service": HelloService, "proof": HelloProof(key, r.URL.Query().Get("nonce"))})
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "muse", "relay_key": key, "relay_urls": []string{"http://tincan-relay.example.ts.net"}})
		case "/v1/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []AgentInfo{{Name: "muse"}}})
		default:
			http.NotFound(w, r)
		}
	})
}

// A live relay that is slow to answer a call (a long poll that outran the
// client's timeout) still proves the key at its address. The client stays
// put rather than moving to another address the same relay advertises.
func TestSlowLiveRelayIsNotAMove(t *testing.T) {
	const key = "k-real"
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		helloHandler(key).ServeHTTP(w, r)
	}))
	t.Cleanup(func() { close(release); slow.Close() })
	other := fakeRelay(t, key) // the same relay's other advertised address
	savedConfig(t, Config{Relay: slow.URL, RelayKey: key, RelayURLs: []string{other}})
	r, _ := NewRelayFor(Config{Relay: slow.URL, RelayKey: key, RelayURLs: []string{other}})
	r.api = &http.Client{Timeout: 200 * time.Millisecond}
	r.findRelays = func(context.Context, string) []string { return nil }
	_, err := r.Agents(t.Context())
	if r.Base() != slow.URL {
		t.Fatalf("moved to %s, but the relay at %s still proves the key", r.Base(), slow.URL)
	}
	if err == nil {
		t.Fatal("the slow call answered")
	}
	if c, _ := LoadConfig(); c.Relay != slow.URL {
		t.Fatalf("saved relay rewritten to %s", c.Relay)
	}
}

// A large netmap is probed a bounded number of addresses at a time.
func TestFindRelayBoundsProbes(t *testing.T) {
	var now, most atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := now.Add(1)
		defer now.Add(-1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(50 * time.Millisecond)
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	var peers []string
	for i := range 3 * probeWorkers {
		peers = append(peers, ts.URL+"/p"+strconv.Itoa(i))
	}
	r := NewRelayHTTP(deadURL(t), ts.Client())
	r.key = "k-real"
	r.findRelays = func(context.Context, string) []string { return peers }
	if got := r.FindRelay(t.Context()); got != "" {
		t.Fatalf("found %s", got)
	}
	if m := most.Load(); m > probeWorkers {
		t.Fatalf("%d probes at once, want at most %d", m, probeWorkers)
	}
}

// findIdle waits until no search (or post-move refresh) is running on r.
func findIdle(t *testing.T, r *Relay) {
	t.Helper()
	waitFor(t, "the search to end", func() bool {
		r.findMu.Lock()
		defer r.findMu.Unlock()
		return r.finding == nil
	})
}

// Cancelling the caller that started a search does not stop it while
// another caller still waits on it; that caller gets the found relay.
func TestCancelledStarterDoesNotStopSharedSearch(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	moved := fakeRelay(t, key)
	savedConfig(t, Config{Relay: old, RelayKey: key})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: key})
	started, release := make(chan struct{}), make(chan struct{})
	r.findRelays = func(ctx context.Context, _ string) []string {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil
		}
		return []string{moved}
	}
	refused := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan bool, 1)
	go func() { first <- r.relocate(ctx, refused) }()
	<-started
	second := make(chan bool, 1)
	go func() { second <- r.relocate(t.Context(), refused) }()
	waitJoined(t, r, 2)
	cancel()
	if <-first {
		t.Fatal("the cancelled caller reported a move")
	}
	close(release)
	select {
	case ok := <-second:
		if !ok || r.Base() != moved {
			t.Fatalf("second caller got %v, base %s; want the found relay %s", ok, r.Base(), moved)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second caller never heard back")
	}
	findIdle(t, r)
}

// A search stopped because its callers were cancelled does not count
// against findEvery: the next caller searches at once.
func TestCancelledSearchDoesNotThrottle(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	moved := fakeRelay(t, key)
	savedConfig(t, Config{Relay: old, RelayKey: key})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: key})
	var searches atomic.Int32
	r.findRelays = func(ctx context.Context, _ string) []string {
		if searches.Add(1) == 1 {
			<-ctx.Done()
			return nil
		}
		return []string{moved}
	}
	refused := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan bool, 1)
	go func() { done <- r.relocate(ctx, refused) }()
	waitFor(t, "the first search", func() bool { return searches.Load() == 1 })
	cancel()
	<-done
	findIdle(t, r)
	if !r.relocate(t.Context(), refused) || r.Base() != moved {
		t.Fatalf("after a cancelled search: base %s, %d searches; want a new search to find %s", r.Base(), searches.Load(), moved)
	}
	findIdle(t, r)
}

// Once the relay is found and saved, waiting callers retry at once; a slow
// whoami refresh runs afterwards and does not hold them.
func TestSlowRefreshDoesNotDelayRetry(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	moved := fakeRelay(t, key)
	savedConfig(t, Config{Relay: old, RelayKey: key})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: key})
	r.findRelays = func(context.Context, string) []string { return []string{moved} }
	oldLearn := learnAfterMove
	t.Cleanup(func() { learnAfterMove = oldLearn })
	release := make(chan struct{})
	refreshed := make(chan struct{})
	learnAfterMove = func(ctx context.Context, r *Relay) {
		defer close(refreshed)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := r.Agents(ctx); err != nil {
		t.Fatalf("retry waited on the refresh: %v", err)
	}
	if c, _ := LoadConfig(); c.Relay != moved {
		t.Fatalf("saved relay %s, want %s before the refresh ends", c.Relay, moved)
	}
	close(release)
	<-refreshed
	findIdle(t, r)
}

// waitJoined waits until n callers wait on r's running search.
func waitJoined(t *testing.T, r *Relay, n int) {
	t.Helper()
	waitFor(t, "callers to join the search", func() bool {
		r.findMu.Lock()
		defer r.findMu.Unlock()
		return r.finding != nil && r.finding.live == n
	})
}

// unixSocket listens on a short-path unix socket and returns its path.
func unixSocket(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tailscaled socket discovery is for Linux sandboxes; Windows talks to Tailscale over its LocalAPI pipe")
	}
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Skipf("cannot listen on a unix socket: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return p
}

func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestSocketFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"/usr/sbin/tailscaled", "--tun=userspace-networking", "--socket=/home/sandbox/.tailscale-x/tailscaled.sock"}, "/home/sandbox/.tailscale-x/tailscaled.sock"},
		{[]string{"tailscaled", "--socket", "/tmp/ts.sock", "--state=/x"}, "/tmp/ts.sock"},
		{[]string{"tailscaled", "-socket=/tmp/a.sock"}, "/tmp/a.sock"},
		{[]string{"tailscaled", "--state=/x"}, ""},
		{[]string{"tailscaled", "--socket"}, ""},
		{[]string{"/bin/sh", "--socket=/tmp/ts.sock"}, ""},
		{nil, ""},
	} {
		if got := socketFlag(tc.args); got != tc.want {
			t.Errorf("socketFlag(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// A tailscaled started in userspace with its socket away from the default
// path is found from its command line, and LocalAPI is asked on it. This
// is how a sandbox agent finds a moved relay with TS_SOCKET unset.
func TestLocalAPINetmapFindsUserspaceSocketFromProcess(t *testing.T) {
	if _, err := os.Stat(paths.DefaultTailscaledSocket()); err == nil {
		t.Skip("this machine has a tailscaled socket at the default path")
	}
	dir := shortTemp(t)
	sock := unixSocket(t, dir, "ts/tailscaled.sock")
	proc := filepath.Join(dir, "proc")
	for pid, cmd := range map[string]string{
		"1":    "/sbin/init\x00",
		"42":   "/usr/sbin/tailscaled\x00--tun=userspace-networking\x00--socks5-server=localhost:1055\x00--socket=" + sock + "\x00",
		"self": "",
	} {
		if err := os.MkdirAll(filepath.Join(proc, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proc, pid, "cmdline"), []byte(cmd), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldProc, oldHome := procRoot, userHome
	procRoot, userHome = proc, func() (string, error) { return filepath.Join(dir, "nohome"), nil }
	t.Cleanup(func() { procRoot, userHome = oldProc, oldHome })
	var used []string
	oldGOOS, oldStatus := goos, localStatus
	goos = "linux"
	localStatus = func(_ context.Context, lc *local.Client) (*ipnstate.Status, error) {
		used = append(used, lc.Socket)
		return &ipnstate.Status{}, nil
	}
	t.Cleanup(func() { goos, localStatus = oldGOOS, oldStatus })
	t.Setenv("TS_SOCKET", "")

	if _, err := localAPINetmap(t.Context()); err != nil || !slices.Equal(used, []string{sock}) {
		t.Fatalf("err %v, asked LocalAPI on %q; want %q", err, used, sock)
	}
}

// With no tailscaled process to read, the usual userspace socket places
// under the home folder are tried.
func TestUserspaceSocketFromHome(t *testing.T) {
	dir := shortTemp(t)
	sock := unixSocket(t, dir, ".tailscale-agentcookie/tailscaled.sock")
	os.WriteFile(filepath.Join(dir, ".tailscale-notasocket"), []byte("x"), 0o644)
	oldProc, oldHome := procRoot, userHome
	procRoot, userHome = filepath.Join(dir, "noproc"), func() (string, error) { return dir, nil }
	t.Cleanup(func() { procRoot, userHome = oldProc, oldHome })
	cache := unixSocket(t, dir, ".cache/tailscale/tailscaled.sock")
	if got := userspaceSockets(t.Context()); !slices.Equal(got, []string{sock, cache}) {
		t.Fatalf("userspaceSockets = %q, want %q", got, []string{sock, cache})
	}
	os.Remove(sock)
	os.Remove(cache)
	if got := userspaceSockets(t.Context()); len(got) != 0 {
		t.Fatalf("userspaceSockets with nothing listening = %q, want none", got)
	}
	// A search whose deadline has passed stops looking.
	unixSocket(t, dir, ".config/tailscale/tailscaled.sock")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := userspaceSockets(ctx); len(got) != 0 {
		t.Fatalf("userspaceSockets after its deadline = %q, want none", got)
	}
}

// Two userspace tailscaleds, perhaps on different tailnets: both are
// listed, so the relay's node is among the candidates whichever one it is
// on; the hello proof picks it.
func TestLocalAPINetmapListsEveryUserspaceTailnet(t *testing.T) {
	if _, err := os.Stat(paths.DefaultTailscaledSocket()); err == nil {
		t.Skip("this machine has a tailscaled socket at the default path")
	}
	dir := shortTemp(t)
	a := unixSocket(t, dir, ".tailscale-a/tailscaled.sock")
	b := unixSocket(t, dir, ".tailscale-b/tailscaled.sock")
	oldProc, oldHome := procRoot, userHome
	procRoot, userHome = filepath.Join(dir, "noproc"), func() (string, error) { return dir, nil }
	t.Cleanup(func() { procRoot, userHome = oldProc, oldHome })
	oldGOOS, oldStatus := goos, localStatus
	goos = "linux"
	peers := map[string]string{a: "100.64.0.1", b: "100.100.0.9"}
	localStatus = func(_ context.Context, lc *local.Client) (*ipnstate.Status, error) {
		ip := netip.MustParseAddr(peers[lc.Socket])
		return &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): {TailscaleIPs: []netip.Addr{ip}}}}, nil
	}
	t.Cleanup(func() { goos, localStatus = oldGOOS, oldStatus })
	t.Setenv("TS_SOCKET", "")

	got, err := localAPINetmap(t.Context())
	if err != nil || !slices.Equal(got, []string{"100.64.0.1", "100.100.0.9"}) {
		t.Fatalf("got %v, %v; want the peers of both tailnets", got, err)
	}
}

// A relay found at one of its advertised addresses is followed at once,
// without waiting on a slow tailnet listing.
func TestFindRelayAdvertisedDoesNotWaitOnListing(t *testing.T) {
	const key = "k-real"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveHello(t, ln, key)
	live := "http://" + ln.Addr().String()
	slow := func(ctx context.Context) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(SwapNetmapLookups(slow, slow))
	r, err := NewRelayFor(Config{Relay: "http://127.0.0.1:1", RelayKey: key, RelayURLs: []string{live}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if got := r.FindRelay(t.Context()); got != live {
		t.Fatalf("FindRelay = %q, want the advertised %q", got, live)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v; want the advertised address at once", d)
	}
}

// A userspace socket that never answers does not hold up the others: they
// are asked at once, and the listing returns within the caller's deadline
// with the peers of the one that answered.
func TestLocalAPINetmapDeadSocketDoesNotBlock(t *testing.T) {
	if _, err := os.Stat(paths.DefaultTailscaledSocket()); err == nil {
		t.Skip("this machine has a tailscaled socket at the default path")
	}
	dir := shortTemp(t)
	dead := unixSocket(t, dir, ".tailscale-a/tailscaled.sock")
	live := unixSocket(t, dir, ".tailscale-b/tailscaled.sock")
	oldProc, oldHome := procRoot, userHome
	procRoot, userHome = filepath.Join(dir, "noproc"), func() (string, error) { return dir, nil }
	t.Cleanup(func() { procRoot, userHome = oldProc, oldHome })
	oldGOOS, oldStatus := goos, localStatus
	goos = "linux"
	localStatus = func(ctx context.Context, lc *local.Client) (*ipnstate.Status, error) {
		if lc.Socket == dead {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &ipnstate.Status{Self: &ipnstate.PeerStatus{TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.7")}}}, nil
	}
	t.Cleanup(func() { goos, localStatus = oldGOOS, oldStatus })
	t.Setenv("TS_SOCKET", "")
	_ = live

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := localAPINetmap(ctx)
	if err != nil || !slices.Equal(got, []string{"100.64.0.7"}) {
		t.Fatalf("got %v, %v; want the live socket's peer", got, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v; want the caller's deadline to bound the dead socket", d)
	}
}

// Stale advertised addresses that hang do not cost the tailnet search its
// time: a moved relay the netmap lists is followed while they still wait.
func TestFindRelayNetmapDoesNotWaitOnStaleAdvertised(t *testing.T) {
	const key = "k-real"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveHello(t, ln, key)
	live := "http://" + ln.Addr().String()
	// An advertised address that accepts but never answers.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(hang.Close)
	r, err := NewRelayFor(Config{Relay: "http://127.0.0.1:1", RelayKey: key, RelayURLs: []string{hang.URL}})
	if err != nil {
		t.Fatal(err)
	}
	r.findRelays = func(context.Context, string) []string { return []string{live} }
	start := time.Now()
	if got := r.FindRelay(t.Context()); got != live {
		t.Fatalf("FindRelay = %q, want the netmap peer %q", got, live)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v; want the netmap peer found without waiting on the stale address", d)
	}
}

// fakeLocalAPI serves a tailscaled LocalAPI status on a unix socket at sock
// and counts the connections it accepts. stop closes the listener and every
// connection, as a tailscaled restart would.
func fakeLocalAPI(t *testing.T, sock string) (conns *atomic.Int32, stop func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tailscaled socket discovery is for Linux sandboxes; Windows talks to Tailscale over its LocalAPI pipe")
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	conns = new(atomic.Int32)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/localapi/v0/status" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(&ipnstate.Status{Self: &ipnstate.PeerStatus{TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")}}})
		}),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				conns.Add(1)
			}
		},
	}
	go func() { _ = srv.Serve(ln) }()
	stop = func() { _ = srv.Close() }
	t.Cleanup(stop)
	return conns, stop
}

// localAPISocket is a fresh socket path short enough for macOS's 104-byte
// unix socket limit, which t.TempDir paths can exceed.
func localAPISocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tsl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func wantLocalAPINetmap(t *testing.T) {
	t.Helper()
	ips, err := localAPINetmap(t.Context())
	if err != nil || !slices.Equal(ips, []string{"100.64.0.1"}) {
		t.Fatalf("localAPINetmap = %v, %v; want [100.64.0.1]", ips, err)
	}
}

// Each relay search asks LocalAPI for the netmap. A client per call left its
// keep-alive connection open in a pool nothing used again, so a process
// searching for an unreachable relay leaked one connection every 30s until
// the machine ran out of ephemeral ports. Searches must reuse one connection.
func TestLocalAPINetmapReusesConnection(t *testing.T) {
	sock := localAPISocket(t)
	conns, _ := fakeLocalAPI(t, sock)
	t.Setenv("TS_SOCKET", sock)
	for range 50 {
		wantLocalAPINetmap(t)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("50 netmap lookups opened %d LocalAPI connections, want 1", n)
	}
}

// Each socket keeps its own client: lookups against one tailscaled never
// reuse, or add to, another's connections.
func TestLocalAPINetmapClientPerSocket(t *testing.T) {
	a, b := localAPISocket(t), localAPISocket(t)
	connsA, _ := fakeLocalAPI(t, a)
	connsB, _ := fakeLocalAPI(t, b)
	for range 3 {
		t.Setenv("TS_SOCKET", a)
		wantLocalAPINetmap(t)
		t.Setenv("TS_SOCKET", b)
		wantLocalAPINetmap(t)
	}
	if na, nb := connsA.Load(), connsB.Load(); na != 1 || nb != 1 {
		t.Fatalf("connections a=%d b=%d, want 1 each", na, nb)
	}
}

// A reused client must not stick to a dead connection: after tailscaled
// restarts on the same socket, the next lookup dials it again.
func TestLocalAPINetmapAfterTailscaledRestart(t *testing.T) {
	sock := localAPISocket(t)
	_, stop := fakeLocalAPI(t, sock)
	t.Setenv("TS_SOCKET", sock)
	wantLocalAPINetmap(t)
	stop()
	_ = os.Remove(sock)
	conns, _ := fakeLocalAPI(t, sock)
	wantLocalAPINetmap(t)
	if n := conns.Load(); n != 1 {
		t.Fatalf("restarted LocalAPI accepted %d connections, want 1", n)
	}
}

// Concurrent lookups may each hold a connection while in flight, but once
// they finish, later lookups reuse the pool instead of opening more.
func TestLocalAPINetmapConcurrentLookups(t *testing.T) {
	sock := localAPISocket(t)
	conns, _ := fakeLocalAPI(t, sock)
	t.Setenv("TS_SOCKET", sock)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := localAPINetmap(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for range 20 {
		wantLocalAPINetmap(t)
	}
	// A count taken after wg.Wait could be low: a lookup that got a freed
	// connection leaves its own dial to finish in the background.
	if n := conns.Load(); n > 20 {
		t.Fatalf("connections: %d after 20 concurrent and 20 sequential lookups, want at most 20", n)
	}
}

func swapTailnetSources(t *testing.T, status func() (*ipnstate.Status, error), prefs func() (*ipn.Prefs, error), cli func(args []string) ([]byte, error)) {
	t.Helper()
	oldGOOS, oldStatus, oldPrefs, oldCLI := goos, localStatus, localPrefs, tailscaleOutput
	goos = "darwin" // one default socket, whatever runs the test
	t.Setenv("TS_SOCKET", "")
	localStatus = func(context.Context, *local.Client) (*ipnstate.Status, error) { return status() }
	localPrefs = func(context.Context, *local.Client) (*ipn.Prefs, error) { return prefs() }
	tailscaleOutput = func(_ context.Context, args ...string) ([]byte, error) { return cli(args) }
	t.Cleanup(func() { goos, localStatus, localPrefs, tailscaleOutput = oldGOOS, oldStatus, oldPrefs, oldCLI })
}

func TestTailnetSelvesLocalAPI(t *testing.T) {
	tags := views.SliceOf([]string{"tag:agent"})
	swapTailnetSources(t,
		func() (*ipnstate.Status, error) {
			return &ipnstate.Status{Version: "1.104.4-tabc1234-gdef5678", BackendState: "Running", Self: &ipnstate.PeerStatus{Tags: &tags}}, nil
		},
		func() (*ipn.Prefs, error) { return &ipn.Prefs{RunSSH: true}, nil },
		func([]string) ([]byte, error) {
			t.Error("the CLI ran although LocalAPI answered")
			return nil, errors.New("no")
		})
	got := TailnetSelves(t.Context())
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	s := got[0]
	if s.Version != "1.104.4-tabc1234-gdef5678" || !s.Running || !slices.Equal(s.Tags, []string{"tag:agent"}) || s.SSH == nil || !*s.SSH {
		t.Fatalf("got %+v", s)
	}
}

// Prefs are a separate call that can be refused; the rest still reports.
func TestTailnetSelvesPrefsRefused(t *testing.T) {
	swapTailnetSources(t,
		func() (*ipnstate.Status, error) {
			return &ipnstate.Status{Version: "1.104.4", BackendState: "NeedsLogin"}, nil
		},
		func() (*ipn.Prefs, error) { return nil, errors.New("prefs access denied") },
		nil)
	got := TailnetSelves(t.Context())
	if len(got) != 1 || got[0].SSH != nil || got[0].Running || len(got[0].Tags) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestTailnetSelvesCLIFallback(t *testing.T) {
	var ran []string
	swapTailnetSources(t,
		func() (*ipnstate.Status, error) { return nil, errors.New("no LocalAPI") },
		func() (*ipn.Prefs, error) { t.Error("prefs read after status failed"); return nil, errors.New("no") },
		func(args []string) ([]byte, error) {
			ran = append(ran, strings.Join(args, " "))
			if slices.Contains(args, "status") {
				return []byte(`{"Version":"1.104.3-tabc1234","BackendState":"Running","Self":{"Tags":["tag:agent"]}}`), nil
			}
			return []byte(`{"RunSSH":true}`), nil
		})
	got := TailnetSelves(t.Context())
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	s := got[0]
	if s.Version != "1.104.3-tabc1234" || !s.Running || !slices.Equal(s.Tags, []string{"tag:agent"}) || s.SSH == nil || !*s.SSH {
		t.Fatalf("got %+v", s)
	}
	if !slices.Equal(ran, []string{"status --json", "debug prefs"}) {
		t.Fatalf("ran %q", ran)
	}
}

func TestTailnetSelvesCLIWithoutPrefs(t *testing.T) {
	swapTailnetSources(t,
		func() (*ipnstate.Status, error) { return nil, errors.New("no LocalAPI") },
		nil,
		func(args []string) ([]byte, error) {
			if slices.Contains(args, "debug") {
				return nil, errors.New("unknown subcommand")
			}
			return []byte(`{"Version":"1.104.4","BackendState":"Stopped"}`), nil
		})
	got := TailnetSelves(t.Context())
	if len(got) != 1 || got[0].SSH != nil || got[0].Running {
		t.Fatalf("got %+v", got)
	}
}

func TestTailnetSelvesNone(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  []byte
		err  error
	}{
		{"no CLI", nil, errors.New("tailscale CLI not found")},
		{"bad output", []byte("not json"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapTailnetSources(t,
				func() (*ipnstate.Status, error) { return nil, errors.New("no LocalAPI") },
				nil,
				func([]string) ([]byte, error) { return tc.out, tc.err })
			if got := TailnetSelves(t.Context()); len(got) != 0 {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

// With no socket at all, LocalAPI is not dialed and the CLI still gets a turn.
func TestTailnetSelvesMissingSocket(t *testing.T) {
	swapTailnetSources(t,
		func() (*ipnstate.Status, error) {
			t.Error("LocalAPI asked with no socket")
			return nil, errors.New("no")
		},
		nil,
		func(args []string) ([]byte, error) {
			if slices.Contains(args, "--socket") {
				t.Errorf("args %q", args)
			}
			if slices.Contains(args, "debug") {
				return nil, errors.New("no")
			}
			return []byte(`{"Version":"1.104.4"}`), nil
		})
	t.Setenv("TS_SOCKET", filepath.Join(t.TempDir(), "gone.sock"))
	if got := TailnetSelves(t.Context()); len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// linuxHost makes this a Linux host with the given default socket, running
// tailscaleds with the given command lines.
func linuxHost(t *testing.T, dir, def string, cmdlines ...string) {
	t.Helper()
	swapTailnetSources(t,
		func() (*ipnstate.Status, error) {
			return &ipnstate.Status{Version: "1.104.4", BackendState: "Running"}, nil
		},
		func() (*ipn.Prefs, error) { return &ipn.Prefs{}, nil },
		nil)
	goos = "linux" // swapTailnetSources puts it back
	proc := filepath.Join(dir, "proc")
	for i, cmd := range cmdlines {
		pid := filepath.Join(proc, strconv.Itoa(i+1))
		if err := os.MkdirAll(pid, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pid, "cmdline"), []byte(cmd), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldProc, oldHome, oldDef := procRoot, userHome, defaultSocket
	procRoot, userHome = proc, func() (string, error) { return filepath.Join(dir, "nohome"), nil }
	defaultSocket = func() string { return def }
	t.Cleanup(func() { procRoot, userHome, defaultSocket = oldProc, oldHome, oldDef })
}

// A userspace tailscaled beside the one on the default socket reports too,
// though the search for a moved relay stops at the default socket.
func TestTailnetSelvesUserspaceBesideDefault(t *testing.T) {
	dir := shortTemp(t)
	def := unixSocket(t, dir, "default.sock")
	user := unixSocket(t, dir, "ts/tailscaled.sock")
	linuxHost(t, dir, def, "/usr/sbin/tailscaled\x00--tun=userspace-networking\x00--socket="+user+"\x00")
	got := TailnetSelves(t.Context())
	if len(got) != 2 || got[0].Socket != def || got[1].Socket != user || !got[0].Running || !got[1].Running {
		t.Fatalf("got %+v, want %q then %q", got, def, user)
	}
	if socks, err := tailscaledSockets(t.Context()); err != nil || !slices.Equal(socks, []string{def}) {
		t.Fatalf("tailscaledSockets = %q, %v; want only %q", socks, err, def)
	}
}

// The stock service names the default socket through /run where the default
// path says /var/run. That is one tailscaled, not a second one to count.
func TestTailnetSelvesDefaultSocketBySecondPath(t *testing.T) {
	dir := shortTemp(t)
	actual := unixSocket(t, dir, "run/ts.sock")
	if err := os.Symlink(filepath.Join(dir, "run"), filepath.Join(dir, "varrun")); err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(dir, "varrun", "ts.sock")
	linuxHost(t, dir, def, "/usr/sbin/tailscaled\x00--socket="+actual+"\x00")
	if got := TailnetSelves(t.Context()); len(got) != 1 || got[0].Socket != def {
		t.Fatalf("got %+v, want one tailscaled on %q", got, def)
	}
}

// A proxy on another host is what the relay sees. One on loopback, or none,
// leaves it seeing this machine, and a proxy from the environment counts.
func TestThroughRemoteProxy(t *testing.T) {
	for _, tc := range []struct {
		name, saved, env string
		want             bool
	}{
		{name: "no proxy"},
		{name: "saved remote", saved: "http://hatch-egress-proxy:3130", want: true},
		{name: "saved remote address", saved: "http://100.64.0.9:3130", want: true},
		{name: "saved localhost", saved: "http://localhost:1055"},
		{name: "saved loopback", saved: "http://127.0.0.1:1055"},
		{name: "saved loopback v6", saved: "http://[::1]:1055"},
		{name: "env remote", env: "http://proxy.corp:3128", want: true},
		{name: "env loopback", env: "http://localhost:1055"},
		{name: "saved beats env", saved: "http://localhost:1055", env: "http://proxy.corp:3128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := envProxy
			envProxy = func(*http.Request) (*url.URL, error) {
				if tc.env == "" {
					return nil, nil
				}
				return url.Parse(tc.env)
			}
			t.Cleanup(func() { envProxy = old })
			r, err := NewRelay("http://tincan-relay", tc.saved)
			if err != nil {
				t.Fatal(err)
			}
			if got := r.ThroughRemoteProxy(); got != tc.want {
				t.Fatalf("ThroughRemoteProxy = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every tailscaled on the host reports, each named by its socket.
func TestTailnetSelvesCLISocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "tailscaled.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	swapTailnetSources(t,
		func() (*ipnstate.Status, error) { return nil, errors.New("no LocalAPI") },
		nil,
		func(args []string) ([]byte, error) {
			if len(args) < 2 || args[0] != "--socket" || args[1] != sock {
				t.Errorf("args %q", args)
			}
			return []byte(`{"Version":"1.104.4"}`), nil
		})
	t.Setenv("TS_SOCKET", sock)
	got := TailnetSelves(t.Context())
	if len(got) != 1 || got[0].Socket != sock {
		t.Fatalf("got %+v", got)
	}
}
