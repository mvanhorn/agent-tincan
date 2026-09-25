package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// Hermes keeps its MCP servers in config.yaml. doctor reads the flat shape
// Hermes documents, with args as a flow or a block list, and a disabled
// server as such.
func TestYamlServers(t *testing.T) {
	es := yamlServers(`
# Hermes config
model: gpt
mcp_servers:
  agent-tincan:
    command: "tincan"   # on PATH
    args: ["mcp"]
    enabled: true
    env:
      TINCAN_CONFIG: "/Users/hermes/.hermes/tincan-hermes.json"
  'old tincan':
    command: /usr/local/bin/tincan
    args:
      - mcp
      - --channel
    enabled: false
  github:
    command: gh
other: true
`)
	if len(es) != 2 {
		t.Fatalf("got %d entries %+v, want the two tincan servers", len(es), es)
	}
	if es[0].Name != "agent-tincan" || es[0].Command != "tincan" || strings.Join(es[0].Args, " ") != "mcp" || es[0].broken {
		t.Fatalf("first entry %+v", es[0])
	}
	if es[1].Name != "old tincan" || es[1].Command != "/usr/local/bin/tincan" || strings.Join(es[1].Args, " ") != "mcp --channel" || !es[1].broken {
		t.Fatalf("second entry %+v, want the block-list args and disabled", es[1])
	}
}

// Keys in a sub-block under a server (env here) belong to that block, so
// an enabled: false there does not disable the server. A comment after
// mcp_servers: does not hide the block.
func TestYamlServersNestedKeysAndComments(t *testing.T) {
	es := yamlServers(`
mcp_servers:   # MCP servers
  agent-tincan:
    command: tincan
    env:
      enabled: false
      command: other
      args: [x]
    args:
      - mcp
`)
	if len(es) != 1 {
		t.Fatalf("got %d entries %+v, want the tincan server", len(es), es)
	}
	if es[0].Command != "tincan" || strings.Join(es[0].Args, " ") != "mcp" || es[0].broken {
		t.Fatalf("entry %+v, want tincan mcp and not disabled", es[0])
	}
}

// A YAML config passed with --config (or the Hermes one) is read as YAML,
// not judged as broken JSON.
func TestFindMCPConfigsReadsYAML(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tincan")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("mcp_servers:\n  agent-tincan:\n    command: "+exe+"\n    args: [\"mcp\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mine []mcpConfigEntry
	for _, e := range findMCPConfigs([]string{cfg}) {
		if e.File == cfg {
			mine = append(mine, e)
		}
	}
	if len(mine) != 1 || mine[0].Name != "agent-tincan" || mine[0].broken {
		t.Fatalf("yaml entries = %+v", mine)
	}
	if c := configCheck(mine, exe); c.Status != "ok" {
		t.Fatalf("yaml entry: got %+v", c)
	}
}

// Claude Code keeps a server map per project in ~/.claude.json. A server
// added in two directories lives in two maps that never load together, so
// it is not a duplicate; a server in the map loaded everywhere plus one in
// a project's map is.
func TestConfigDuplicatesCountPerScope(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tincan")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := `{"command":"` + exe + `","args":["mcp","--channel"]}`
	twoProjects := filepath.Join(dir, "two-projects.json")
	if err := os.WriteFile(twoProjects, []byte(`{"projects":{"/a":{"mcpServers":{"agent-tincan":`+entry+`}},"/b":{"mcpServers":{"agent-tincan":`+entry+`}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	userAndProject := filepath.Join(dir, "user-and-project.json")
	if err := os.WriteFile(userAndProject, []byte(`{"mcpServers":{"tincan":`+entry+`},"projects":{"/a":{"mcpServers":{"agent-tincan":`+entry+`}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// OpenClaw keeps its servers at mcp.servers, a map it loads everywhere.
	openclaw := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(openclaw, []byte(`{"mcp":{"servers":{"tincan":`+entry+`}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	byFile := map[string][]mcpConfigEntry{}
	for _, e := range findMCPConfigs([]string{twoProjects, userAndProject, openclaw}) {
		byFile[e.File] = append(byFile[e.File], e)
	}
	es := byFile[twoProjects]
	if len(es) != 2 || es[0].broken || es[1].broken {
		t.Fatalf("two project entries judged: %+v", es)
	}
	scopes := []string{es[0].Scope, es[1].Scope}
	if (scopes[0] != "projects./a" || scopes[1] != "projects./b") && (scopes[0] != "projects./b" || scopes[1] != "projects./a") {
		t.Fatalf("scopes = %v", scopes)
	}
	if c := configCheck(es, exe); c.Status != "ok" {
		t.Fatalf("two projects: got %+v", c)
	}
	es = byFile[userAndProject]
	if len(es) != 2 || !es[0].broken || !es[1].broken || !strings.Contains(strings.Join(es[0].Problems, " "), "one of 2") {
		t.Fatalf("user plus project entries judged: %+v", es)
	}
	es = byFile[openclaw]
	if len(es) != 1 || es[0].Scope != "" || es[0].broken {
		t.Fatalf("openclaw entry judged: %+v", es)
	}
}

// With TINCAN_RELAY set the client never writes the config file, so the
// key the relay hands out is not saved. doctor used to blame an old relay
// for that; it says what is going on instead.
func TestDoctorRelayMovesUnderTincanRelayOverride(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	relayMoves := func() check {
		t.Helper()
		for _, c := range runDoctor(context.Background(), "", nil).Checks {
			if c.Name == "relay moves" {
				return c
			}
		}
		t.Fatal("no relay moves check")
		return check{}
	}
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	if c := relayMoves(); c.Status != "ok" {
		t.Fatalf("with a saved config: %+v", c)
	}
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	t.Setenv("TINCAN_RELAY", m.URL("grokbot"))
	c := relayMoves()
	if c.Status != "warn" || !strings.Contains(c.Detail, "TINCAN_RELAY") || strings.Contains(c.Detail, "older than") || !strings.Contains(c.Fix, "rejoin") {
		t.Fatalf("under TINCAN_RELAY: %+v", c)
	}
}

// The other two reasons a relay key is missing: a relay too old to hand it
// out, and a config file the key could not be written to.
func TestDoctorRelayMovesOldRelayAndSaveFailure(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	relayMoves := func() check {
		t.Helper()
		for _, c := range runDoctor(context.Background(), "", nil).Checks {
			if c.Name == "relay moves" {
				return c
			}
		}
		t.Fatal("no relay moves check")
		return check{}
	}

	// A relay older than 0.5.0-rc12 leaves relay_key out of whoami.
	h := m.Server.Handler()
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "100.0.0.2:1" // grokbot's machine
		if r.URL.Path != "/v1/whoami" {
			h.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Error(err)
		}
		delete(body, "relay_key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Code)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(old.Close)
	useConfig(t, client.Config{Relay: old.URL, Agent: "grokbot"})
	if c := relayMoves(); c.Status != "warn" || !strings.Contains(c.Detail, "older than") {
		t.Fatalf("old relay: %+v", c)
	}

	// A current relay, but the config directory is read-only.
	if os.Getuid() == 0 {
		t.Skip("root writes to read-only directories")
	}
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	dir := filepath.Dir(client.ConfigPath())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if c := relayMoves(); c.Status != "warn" || !strings.Contains(c.Detail, "could not be saved") || strings.Contains(c.Detail, "older than") {
		t.Fatalf("read-only config: %+v", c)
	}
}

// In Claude Code a project server with the same name as a user-level one
// overrides it, which is the path `--scope user` advice creates: not a
// duplicate. A differently named pair still loads together.
func TestConfigSameNameUserAndProjectIsAnOverride(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tincan")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := `{"command":"` + exe + `","args":["mcp","--channel"]}`
	same := filepath.Join(dir, "same.json")
	diff := filepath.Join(dir, "diff.json")
	_ = os.WriteFile(same, []byte(`{"mcpServers":{"agent-tincan":`+entry+`},"projects":{"/a":{"mcpServers":{"agent-tincan":`+entry+`}}}}`), 0o600)
	_ = os.WriteFile(diff, []byte(`{"mcpServers":{"agent-tincan":`+entry+`},"projects":{"/a":{"mcpServers":{"tincan":`+entry+`}}}}`), 0o600)
	byFile := map[string][]mcpConfigEntry{}
	for _, e := range findMCPConfigs([]string{same, diff}) {
		byFile[e.File] = append(byFile[e.File], e)
	}
	for _, e := range byFile[same] {
		if e.broken {
			t.Fatalf("same-name override flagged: %+v", e)
		}
	}
	flagged := 0
	for _, e := range byFile[diff] {
		if e.broken {
			flagged++
		}
	}
	if flagged != 2 {
		t.Fatalf("differently named user and project servers: %d flagged, want 2: %+v", flagged, byFile[diff])
	}
}

// PyYAML reads YAML 1.1 booleans, so Hermes' enabled: False / no / off all
// turn a server off; an entry whose shape tincan cannot read warns instead
// of failing.
func TestYamlServersBooleansAndUnreadEntries(t *testing.T) {
	for _, v := range []string{"False", "no", "off", "NO"} {
		es := yamlServers("mcp_servers:\n  tincan:\n    command: tincan\n    args: [mcp]\n    enabled: " + v + "\n")
		if len(es) != 1 || !es[0].broken {
			t.Fatalf("enabled: %s not read as off: %+v", v, es)
		}
	}
	es := yamlServers("mcp_servers:\n  tincan:\n    command: tincan\n    args: [mcp]\n    enabled: yes\n")
	if len(es) != 1 || es[0].broken {
		t.Fatalf("enabled: yes read as off: %+v", es)
	}
	es = yamlServers("mcp_servers:\n  tincan:\n    url: something-tincan-reads-no-command-from\n")
	if len(es) != 1 || !es[0].unread {
		t.Fatalf("unreadable entry: %+v", es)
	}
	if c := configCheck(es, ""); c.Status != "warn" {
		t.Fatalf("unreadable entry should warn, got %+v", c)
	}
}
