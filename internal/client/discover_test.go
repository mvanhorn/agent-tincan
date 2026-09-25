package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeRelay answers hello with a proof under key, and agents.
func fakeRelay(t *testing.T, key string) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
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
	if r.Base() != moved {
		t.Fatalf("base %s, want %s (the peer that proved the key, not the impostor)", r.Base(), moved)
	}
	c, err := LoadConfig()
	if err != nil || c.Relay != moved || c.RelayKey != key {
		t.Fatalf("saved config %+v %v", c, err)
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
