package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// watchedRelay is a relay that proves key on hello and answers whoami with
// status code.
func watchedRelay(t *testing.T, key string, code int) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			_ = json.NewEncoder(w).Encode(map[string]string{"service": client.HelloService, "proof": client.HelloProof(key, r.URL.Query().Get("nonce"))})
		case "/v1/whoami":
			if code != http.StatusOK {
				http.Error(w, `{"error":"not a joined agent"}`, code)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "hermes", "relay_key": key})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func goneURL(t *testing.T) string {
	ts := httptest.NewServer(http.NotFoundHandler())
	u := ts.URL
	ts.Close()
	return u
}

func watchRelay(t *testing.T, c client.Config) *client.Relay {
	t.Helper()
	t.Setenv("TINCAN_RELAY", "")
	// Not t.TempDir: after a move the client refreshes relay info in the
	// background and saves the config again, which can land while
	// t.TempDir's cleanup is removing the folder.
	dir, err := os.MkdirTemp("", "relaywatch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 50 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	path := filepath.Join(dir, "hermes.json")
	if err := client.SaveConfigTo(path, c); err != nil {
		t.Fatal(err)
	}
	r, err := client.NewRelayForFile(c, path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func noNetmap(t *testing.T) {
	none := func(context.Context) ([]string, error) { return nil, nil }
	t.Cleanup(client.SwapNetmapLookups(none, none))
}

func TestRelayWatchProbe(t *testing.T) {
	noNetmap(t)
	const key = "k-relay"

	if err := relayProbe(watchRelay(t, client.Config{Relay: watchedRelay(t, key, 200), Agent: "hermes", RelayKey: key}))(t.Context()); err != nil {
		t.Fatalf("relay answering: probe %v", err)
	}
	// A relay that answers with an error is still up.
	if err := relayProbe(watchRelay(t, client.Config{Relay: watchedRelay(t, key, http.StatusForbidden), Agent: "hermes", RelayKey: key}))(t.Context()); err != nil {
		t.Fatalf("relay answering 403: probe %v", err)
	}
	// A proxy answering for a relay that is down is not the relay, and a
	// relay that cannot check callers serves no one.
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if err := relayProbe(watchRelay(t, client.Config{Relay: watchedRelay(t, key, code), Agent: "hermes", RelayKey: key}))(t.Context()); err == nil {
			t.Fatalf("relay answering %d: probe reported up", code)
		}
	}
	if err := relayProbe(watchRelay(t, client.Config{Relay: goneURL(t), Agent: "hermes", RelayKey: key}))(t.Context()); err == nil {
		t.Fatal("nothing listening: probe reported up")
	}
}

func TestRelayWatchProbeFindsMovedRelay(t *testing.T) {
	noNetmap(t)
	const key = "k-relay"
	moved := watchedRelay(t, key, 200)
	r := watchRelay(t, client.Config{Relay: goneURL(t), Agent: "hermes", RelayKey: key, RelayURLs: []string{moved}})
	if err := relayProbe(r)(t.Context()); err != nil {
		t.Fatalf("relay moved to an address discovery finds: probe %v", err)
	}
	if r.Base() != moved {
		t.Fatalf("base %s, want %s", r.Base(), moved)
	}
}

func TestRelayWatchRefusesBadFlags(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "hermes.json")
	if err := client.SaveConfigTo(cfg, client.Config{Relay: "http://tincan-relay", Agent: "hermes"}); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "none.json")
	for name, args := range map[string][]string{
		"no alert command": {"relay-watch", "--config", cfg},
		"zero after":       {"relay-watch", "--config", cfg, "--alert-cmd", "true", "--after", "0"},
		"zero every":       {"relay-watch", "--config", cfg, "--alert-cmd", "true", "--every", "0"},
		"no relay":         {"relay-watch", "--config", empty, "--alert-cmd", "true"},
	} {
		root := Root()
		root.SetArgs(args)
		root.SetOut(new(strings.Builder))
		root.SetErr(new(strings.Builder))
		if err := root.ExecuteContext(t.Context()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRelayWatchInstall(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service definition here")
	}
	home := t.TempDir()
	setHome(t, home)
	cfg := filepath.Join(t.TempDir(), "hermes.json")
	if err := client.SaveConfigTo(cfg, client.Config{Relay: "http://tincan-relay", Agent: "hermes"}); err != nil {
		t.Fatal(err)
	}
	root := Root()
	var out strings.Builder
	root.SetOut(&out)
	root.SetArgs([]string{"relay-watch", "install", "--config", cfg, "--alert-cmd", "TINCAN_ALERT_TO=you@example.com /bin/imessage-alert.sh", "--binary", "/usr/local/bin/tincan"})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.relaywatch.plist")
	start := "launchctl bootstrap"
	if runtime.GOOS == "linux" {
		path = filepath.Join(home, ".config", "systemd", "user", "tincan-relay-watch.service")
		start = "systemctl --user"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no service definition: %v\n%s", err, out.String())
	}
	for _, want := range []string{cfg, "TINCAN_ALERT_TO=you@example.com /bin/imessage-alert.sh", "10m0s", "1m0s"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("service definition lacks %q:\n%s", want, b)
		}
	}
	if !strings.Contains(out.String(), path) || !strings.Contains(out.String(), start) {
		t.Fatalf("install output %q should name %s and how to start it", out.String(), path)
	}
}
