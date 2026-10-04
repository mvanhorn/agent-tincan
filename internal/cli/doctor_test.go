package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
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

// Doctor names every tincan mcp still running a different build than this
// binary, with the reload step for the app that runs it. Ended launches and
// dead processes do not count.
func TestBuildCheck(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	alive := func(l mcpserver.Launch) bool { return l.PID != 99 }
	cur := Version
	ls := []mcpserver.Launch{
		{Started: now, PID: 10, Client: "claude-code 2.0.1", Version: "0.1.0"},
		{Started: now, PID: 11, Client: "codex-mcp-client 0.40", Version: "0.1.0"},
		{Started: now, PID: 12, Client: "claude-code 2.0.1", Version: cur},
		{Started: now, PID: 13, Client: "cursor-vscode 1", Version: "0.1.0", Ended: now},
		{Started: now, PID: 99, Client: "cursor-vscode 1", Version: "0.1.0"},
	}
	c := buildCheck(ls, alive)
	if c.Status != "warn" {
		t.Fatalf("status %s, want warn: %+v", c.Status, c)
	}
	for _, want := range []string{"pid 10", "pid 11", "0.1.0"} {
		if !strings.Contains(c.Detail, want) {
			t.Fatalf("detail lacks %q: %s", want, c.Detail)
		}
	}
	for _, not := range []string{"pid 12", "pid 13", "pid 99"} {
		if strings.Contains(c.Detail, not) {
			t.Fatalf("detail names %s: %s", not, c.Detail)
		}
	}
	for _, want := range []string{"Claude Code", "Codex"} {
		if !strings.Contains(c.Fix, want) {
			t.Fatalf("fix lacks the %s reload step: %s", want, c.Fix)
		}
	}
	if strings.Contains(c.Fix, "Cursor") {
		t.Fatalf("fix names a host with no stale server: %s", c.Fix)
	}
	if ok := buildCheck(ls[2:], alive); ok.Status != "ok" {
		t.Fatalf("no stale servers: got %+v", ok)
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

func TestDoctorProgressToolCount(t *testing.T) {
	for _, missingProgress := range []bool{false, true} {
		t.Run(fmt.Sprint(missingProgress), func(t *testing.T) {
			names := []string{"ask", "get_reply", "check_inbox", "claim", "reply", "answer", "cancel", "list_agents", "trace", "search", "onboard", "get_attachment"}
			if !missingProgress {
				names = append(names, "progress")
			}
			var tools []map[string]string
			for _, name := range names {
				tools = append(tools, map[string]string{"name": name})
			}
			raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{"tools": tools}})
			if err != nil {
				t.Fatal(err)
			}
			exe := filepath.Join(t.TempDir(), "mcp-probe")
			script := "#!/bin/sh\nread -r a\nread -r b\nread -r c\nprintf '%s\\n' '" + string(raw) + "'\n"
			if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			got := probeCheck(t.Context(), exe, false, true)
			if missingProgress {
				if got.Status != "fail" || !strings.Contains(got.Detail, "missing progress") {
					t.Fatalf("old tools: %+v", got)
				}
			} else if got.Status != "ok" || !strings.Contains(got.Detail, "lists all 13 tools") {
				t.Fatalf("tools: %+v", got)
			}
		})
	}
}

// setVersion runs the test as tincan version v.
func setVersion(t *testing.T, v string) {
	t.Helper()
	old := Version
	Version = v
	t.Cleanup(func() { Version = old })
}

// A binary that differs from the relay's release is only a failure when the
// relay's release is newer or the order is unknown. A newer client warns and
// tells the owner how to bring the relay up to it.
func TestVersionCheck(t *testing.T) {
	cases := []struct {
		name, client, relay, build string
		status                     string
		want, notWant              []string
	}{
		{"client newer", "0.8.0", "0.7.0", "other build", "warn",
			[]string{"0.8.0", "0.7.0", "relay-upgrade --from-github v0.8.0", "admin device"}, []string{"Run tincan upgrade"}},
		{"client newer with v prefix", "v0.8.0", "v0.7.0", "other build", "warn",
			[]string{"relay-upgrade --from-github v0.8.0"}, []string{"vv0.8.0"}},
		{"client older", "0.6.0", "0.7.0", "other build", "fail",
			[]string{"is not the relay's release 0.7.0", "Run tincan upgrade"}, []string{"relay-upgrade"}},
		{"same release", "0.7.0", "0.7.0", "old binary", "ok",
			[]string{"matches the relay's release"}, nil},
		{"development client", "0.0.1-dev", "0.7.0", "other build", "fail",
			[]string{"Run tincan upgrade"}, []string{"relay-upgrade"}},
		{"unknown relay version", "0.8.0", "", "other build", "fail",
			[]string{"Run tincan upgrade"}, []string{"relay-upgrade"}},
		{"unparsable relay version", "0.8.0", "nightly", "other build", "fail",
			[]string{"Run tincan upgrade"}, []string{"relay-upgrade"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setVersion(t, tc.client)
			files := map[string]string{platformFile: tc.build}
			if tc.relay != "" {
				files["VERSION"] = tc.relay
			}
			m := meshWithDist(t, files)
			exe, _ := fakeExe(t)
			c := versionCheck(t.Context(), m.Client(t, "muse"), exe)
			text := c.Detail + "\n" + c.Fix
			if c.Status != tc.status {
				t.Fatalf("status = %q, want %q: %s", c.Status, tc.status, text)
			}
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("missing %q in %q", w, text)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(text, w) {
					t.Errorf("unexpected %q in %q", w, text)
				}
			}
		})
	}
}

// The unanswered-wakes check lists every unanswered agent with its last wake
// result on an admin device, says all is well when none is, and is skipped
// on a device that is not an admin.
func TestUnansweredWakeCheck(t *testing.T) {
	now := time.Now()
	roster := fmt.Sprintf(`{"agents":[
{"name":"grokbot","wake":"webhook","woken_at":%q,"wake_result":"ok","unanswered":true},
{"name":"hermes","wake":"webhook","woken_at":%q,"wake_result":"ok"},
{"name":"instinct","wake":"email","woken_at":%q,"wake_result":"api.agentmail.to returned 502 Bad Gateway","unanswered":true}]}`,
		now.Add(-12*time.Minute).Format(time.RFC3339), now.Format(time.RFC3339), now.Add(-3*time.Minute).Format(time.RFC3339))
	for _, tc := range []struct {
		name, roster string
		admin        int
		status       string
		want         []string
	}{
		{"unanswered", roster, 200, "warn", []string{
			"grokbot woken 12m ago, no check-in (webhook ok); instinct woken 3m ago, no check-in (email failed: api.agentmail.to returned 502 Bad Gateway)",
			"same webhook or email",
			"next check-in"}},
		{"none", `{"agents":[{"name":"hermes","wake":"webhook"}]}`, 200, "ok", []string{"no webhook or email agent is waiting on an unanswered wake"}},
		{"not admin", roster, 403, "ok", []string{"skipped", "admin device"}},
		{"old relay", roster, 404, "ok", []string{"skipped"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/agents":
					fmt.Fprint(w, tc.roster)
				case "/v1/admin/held":
					w.WriteHeader(tc.admin)
					if tc.admin == 200 {
						fmt.Fprint(w, `[]`)
					} else {
						fmt.Fprint(w, `{"error":"no"}`)
					}
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			r, err := client.NewRelayFor(client.Config{Relay: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			c := unansweredCheck(t.Context(), r, now)
			text := c.Detail + "\n" + c.Fix
			if c.Name != "unanswered wakes" || c.Status != tc.status {
				t.Fatalf("check = %+v", c)
			}
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("missing %q in %q", w, text)
				}
			}
			if tc.admin != 200 && strings.Contains(text, "grokbot") {
				t.Errorf("non-admin check names agents: %q", text)
			}
		})
	}
}
