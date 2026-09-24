package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
)

func TestLaunchCheck(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	cases := []struct {
		name   string
		ls     []mcpserver.Launch
		status string
		want   string
	}{
		{"never started", nil, "warn", "not running tincan at all"},
		{"stopped on error", []mcpserver.Launch{{Started: ago(time.Minute), Client: "host 1", Ended: ago(time.Minute), Error: "not a joined agent"}}, "fail", "not a joined agent"},
		{"closed before initialize", []mcpserver.Launch{{Started: ago(time.Minute), Ended: ago(time.Minute)}}, "fail", "without ever sending initialize"},
		{"connected, no tools/list", []mcpserver.Launch{{Started: ago(time.Minute), Client: "host 1", Initialized: ago(time.Minute), Framing: "content-length"}}, "fail", "never asked for the tool list"},
		{"healthy", []mcpserver.Launch{{Started: ago(time.Minute), Client: "host 1", Initialized: ago(time.Minute), ToolsListed: ago(time.Minute), Version: Version}}, "ok", "listed the tools"},
		{"old build still running", []mcpserver.Launch{{Started: ago(time.Hour), Initialized: ago(time.Hour), ToolsListed: ago(time.Hour), Version: "0.1.0"}}, "warn", "running tincan 0.1.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := launchCheck(c.ls, now)
			if got.Status != c.status || !strings.Contains(got.Detail, c.want) {
				t.Fatalf("got %s %q, want %s containing %q", got.Status, got.Detail, c.status, c.want)
			}
		})
	}
}

func TestTomlServers(t *testing.T) {
	es := tomlServers(`
[mcp_servers.agent-tincan]
command = "/usr/local/bin/tincan"
args = ["mcp", "--channel"]

[mcp_servers.agent-tincan.env]
TINCAN_CONFIG = "~/.config/tincan/codex.json"

[mcp_servers."other tincan"]
command = "tincan mcp"
enabled = false

[mcp_servers.github]
command = "gh"
`)
	if len(es) != 2 {
		t.Fatalf("got %d entries %+v, want 2 (the env sub-table is not a server)", len(es), es)
	}
	if es[0].Name != "agent-tincan" || es[0].Command != "/usr/local/bin/tincan" || strings.Join(es[0].Args, " ") != "mcp --channel" {
		t.Fatalf("first entry %+v", es[0])
	}
	if es[1].Name != "other tincan" || !es[1].broken {
		t.Fatalf("second entry %+v, want the quoted name and disabled", es[1])
	}
}

func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tincan")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "mcp.json")
	raw := `{"mcpServers":{"user-tincan mcp":{"command":"` + exe + ` mcp"},"github":{"command":"gh"}},
	"projects":{"/x":{"mcpServers":{"tincan":{"command":"` + exe + `","args":["mcp"]}}}}}`
	if err := os.WriteFile(cfg, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	es := findMCPConfigs([]string{cfg})
	var mine []mcpConfigEntry
	for _, e := range es {
		if e.File == cfg {
			mine = append(mine, e)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("got %+v, want the two tincan entries", mine)
	}
	c := configCheck(mine, exe)
	if c.Status != "fail" {
		t.Fatalf("got %+v, want fail", c)
	}
	var bad mcpConfigEntry
	for _, e := range mine {
		if e.Name == "user-tincan mcp" {
			bad = e
		}
	}
	all := strings.Join(bad.Problems, "; ")
	for _, want := range []string{"space", "command holds arguments", `args must start with "mcp"`, "one of 2"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems %q lack %q", all, want)
		}
	}

	good := []mcpConfigEntry{{File: cfg, Name: "tincan", Command: exe, Args: []string{"mcp"}}}
	if c := configCheck(good, exe); c.Status != "ok" {
		t.Fatalf("clean entry: got %+v", c)
	}
}
