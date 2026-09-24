package onboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The OpenClaw block gives the owner OpenClaw's real commands: the MCP
// server goes in with openclaw mcp add (config key mcp.servers, not
// mcpServers), hooks are enabled through openclaw config, the gateway is
// restarted, and wake.json uses the openclaw webhook format with the hook
// token as bearer_token. Secret values are only ever named.
func TestOpenClawSetupUsesRealCommands(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "claw", Kind: KindOpenClaw}}})
	a := block(t, k, "claw")
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{
		"openclaw mcp add agent-tincan --command tincan --arg mcp --env TINCAN_CONFIG=",
		"~/.config/tincan/claw.json",
		"openclaw mcp doctor agent-tincan --probe",
		"openclaw config patch --file",
		"examples/openclaw/openclaw-snippet.json",
		"hooks.enabled",
		"hooks.token",
		"hooks.allowedAgentIds",
		"openclaw gateway restart",
		`"format": "openclaw"`,
		"agent_id",
		"bearer_token",
		"18789",
		"~/.openclaw/workspace/AGENTS.md",
		"docs/adapters/openclaw.md",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("openclaw setup missing %q:\n%s", want, setup)
		}
	}
	for _, stale := range []string{"mcpServers", "/hooks/wake URL", "mcporter"} {
		if strings.Contains(setup, stale) {
			t.Errorf("openclaw setup still says %q:\n%s", stale, setup)
		}
	}
	// No secret-looking literal: the token is named, never shown.
	if regexp.MustCompile(`bearer_token"?\s*[:=]\s*"[^<]`).MatchString(setup) {
		t.Errorf("openclaw setup prints a bearer token value:\n%s", setup)
	}
	if !strings.Contains(a.Instructions, "Agent Tincan:") || !strings.Contains(a.Instructions, "/hooks/agent") {
		t.Errorf("openclaw instructions should name the /hooks/agent wake and its prefix:\n%s", a.Instructions)
	}
	if !strings.Contains(a.Instructions, "check_inbox") || !strings.Contains(a.Instructions, "tincan inbox") {
		t.Errorf("openclaw instructions should name both check_inbox and the tincan inbox fallback:\n%s", a.Instructions)
	}
}

var repoRoot = filepath.Join("..", "..")

// examples/openclaw/openclaw-snippet.json is a config patch for
// openclaw config patch --file: plain JSON (a subset of OpenClaw's JSON5),
// with the MCP server under mcp.servers and the hook block OpenClaw's
// gateway expects.
func TestOpenClawExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, "examples", "openclaw", "openclaw-snippet.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
		MCP        struct {
			Servers map[string]struct {
				Command string            `json:"command"`
				Args    []string          `json:"args"`
				Env     map[string]string `json:"env"`
			} `json:"servers"`
		} `json:"mcp"`
		Hooks struct {
			Enabled                bool     `json:"enabled"`
			Token                  string   `json:"token"`
			Path                   string   `json:"path"`
			AllowedAgentIDs        []string `json:"allowedAgentIds"`
			AllowRequestSessionKey *bool    `json:"allowRequestSessionKey"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("openclaw-snippet.json: %v", err)
	}
	if cfg.MCPServers != nil {
		t.Error("OpenClaw reads MCP servers from mcp.servers, not mcpServers")
	}
	s, ok := cfg.MCP.Servers["agent-tincan"]
	if !ok || s.Command != "tincan" || len(s.Args) != 1 || s.Args[0] != "mcp" {
		t.Errorf("mcp.servers.agent-tincan = %+v", cfg.MCP.Servers)
	}
	if !strings.Contains(s.Env["TINCAN_CONFIG"], ".config/tincan/") {
		t.Errorf("agent-tincan env TINCAN_CONFIG = %q", s.Env["TINCAN_CONFIG"])
	}
	h := cfg.Hooks
	if !h.Enabled || !strings.HasPrefix(h.Token, "<") || h.Path != "/hooks" || len(h.AllowedAgentIDs) == 0 {
		t.Errorf("hooks = %+v", h)
	}
	if h.AllowRequestSessionKey == nil || *h.AllowRequestSessionKey {
		t.Errorf("hooks.allowRequestSessionKey should be explicitly false: %+v", h)
	}
}

// The skill's SKILL.md has AgentSkills front matter (name, description) and
// a metadata.openclaw block, written as one line of JSON so it parses as
// both YAML and JSON5, that gates the skill on the tincan binary.
func TestOpenClawExampleSkillParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, "examples", "openclaw", "skills", "agent-tincan", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatal("SKILL.md must start with --- front matter")
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		t.Fatal("SKILL.md front matter is not closed")
	}
	front, body := text[4:4+end], text[4+end+5:]
	fields := map[string]string{}
	for line := range strings.SplitSeq(front, "\n") {
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") {
			t.Fatalf("front matter line %q is not a top-level key: value pair", line)
		}
		fields[key] = strings.TrimSpace(val)
	}
	if fields["name"] != "agent-tincan" {
		t.Errorf("name = %q, want agent-tincan (it must match the skill directory)", fields["name"])
	}
	if len(fields["description"]) < 20 {
		t.Errorf("description = %q", fields["description"])
	}
	var meta struct {
		OpenClaw struct {
			Requires struct {
				Bins []string `json:"bins"`
			} `json:"requires"`
			Homepage string `json:"homepage"`
		} `json:"openclaw"`
	}
	if err := json.Unmarshal([]byte(fields["metadata"]), &meta); err != nil {
		t.Fatalf("metadata %q: %v", fields["metadata"], err)
	}
	if len(meta.OpenClaw.Requires.Bins) != 1 || meta.OpenClaw.Requires.Bins[0] != "tincan" {
		t.Errorf("metadata.openclaw.requires.bins = %v", meta.OpenClaw.Requires.Bins)
	}
	for _, want := range []string{"tincan inbox", "tincan reply", "tincan ask", "tincan get <request-id>", "tincan agents", "/hooks/agent", "Agent Tincan:"} {
		if !strings.Contains(body, want) {
			t.Errorf("SKILL.md body missing %q", want)
		}
	}
}
