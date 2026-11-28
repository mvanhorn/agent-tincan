package t3bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWireProvidersNonClobberingAndOpenCode(t *testing.T) {
	home := t.TempDir()
	t3Home := filepath.Join(home, ".t3")
	tincanCfg := filepath.Join(home, ".config", "tincan", "t3.json")

	// Set up T3 settings.json mirroring Matt's real multi-provider setup.
	ud := filepath.Join(t3Home, "userdata")
	if err := os.MkdirAll(ud, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeGmailHome := filepath.Join(home, ".claude-gmail")
	settings := `{
  "providerInstances": {
    "claudeAgent": {"driver": "claudeAgent", "displayName": "Claude (main)", "enabled": true, "config": {}},
    "claude-gmail": {"driver": "claudeAgent", "displayName": "Claude (gmail)", "enabled": true, "config": {"homePath": "` + claudeGmailHome + `"}},
    "antigravity": {"driver": "antigravity", "displayName": "Antigravity", "enabled": true, "config": {}},
    "opencode": {"driver": "opencode", "displayName": "OpenCode", "enabled": true, "config": {}}
  }
}`
	if err := os.WriteFile(filepath.Join(ud, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	// Standalone ~/.claude.json already has agent-tincan for claude-code (must NOT be clobbered by default).
	standaloneClaude := `{"mcpServers":{"agent-tincan":{"command":"/usr/local/bin/tincan","args":["mcp","--channel"],"env":{}}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(standaloneClaude), 0o600); err != nil {
		t.Fatal(err)
	}
	// ~/.claude-gmail/.claude.json only has printing-press.
	if err := os.MkdirAll(claudeGmailHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeGmailHome, ".claude.json"), []byte(`{"mcpServers":{"printing-press":{"command":"pp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// T3 isolated Antigravity profile.
	agyDir := filepath.Join(ud, "providers", "antigravity", "abc123", "config")
	if err := os.MkdirAll(agyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	agyCfg := filepath.Join(agyDir, "mcp_config.json")
	if err := os.WriteFile(agyCfg, []byte(`{"mcpServers":{"browse":{"command":"browse"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// OpenCode config.
	ocDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(ocDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ocCfg := filepath.Join(ocDir, "opencode.json")
	if err := os.WriteFile(ocCfg, []byte(`{"$schema":"https://opencode.ai/config.json","model":"deepseek"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	statuses, err := WireProviders(WireOptions{
		Home:           home,
		T3Home:         t3Home,
		TincanBinary:   "/Users/test/bin/tincan",
		TincanConfig:   tincanCfg,
		OverrideShared: false,
	})
	if err != nil {
		t.Fatalf("WireProviders: %v", err)
	}
	if len(statuses) != 4 {
		t.Fatalf("expected 4 provider statuses, got %d: %+v", len(statuses), statuses)
	}

	// Verify ~/.claude.json was left byte-identical.
	gotClaude, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if string(gotClaude) != standaloneClaude {
		t.Fatalf("standalone ~/.claude.json was modified without --override-shared: %s", gotClaude)
	}

	// Verify claude-gmail preserved printing-press and added agent-tincan.
	var cg map[string]any
	rawCG, _ := os.ReadFile(filepath.Join(claudeGmailHome, ".claude.json"))
	if err := json.Unmarshal(rawCG, &cg); err != nil {
		t.Fatal(err)
	}
	cgServers := cg["mcpServers"].(map[string]any)
	if _, ok := cgServers["printing-press"]; !ok {
		t.Fatal("claude-gmail lost printing-press")
	}
	if _, ok := cgServers["agent-tincan"]; !ok {
		t.Fatal("claude-gmail missing agent-tincan")
	}

	// Verify T3 Antigravity profile got agent-tincan pinned to t3.json.
	var agy map[string]any
	rawAgy, _ := os.ReadFile(agyCfg)
	if err := json.Unmarshal(rawAgy, &agy); err != nil {
		t.Fatal(err)
	}
	agyAT := agy["mcpServers"].(map[string]any)["agent-tincan"].(map[string]any)
	if agyAT["env"].(map[string]any)["TINCAN_CONFIG"] != tincanCfg {
		t.Fatalf("antigravity TINCAN_CONFIG = %v, want %s", agyAT, tincanCfg)
	}

	// Verify OpenCode config preserved $schema/model and wrote OpenCode's mcp format.
	var oc map[string]any
	rawOC, _ := os.ReadFile(ocCfg)
	if err := json.Unmarshal(rawOC, &oc); err != nil {
		t.Fatal(err)
	}
	if oc["model"] != "deepseek" {
		t.Fatalf("opencode lost model: %+v", oc)
	}
	ocAT := oc["mcp"].(map[string]any)["agent-tincan"].(map[string]any)
	if ocAT["type"] != "local" || ocAT["environment"].(map[string]any)["TINCAN_CONFIG"] != tincanCfg {
		t.Fatalf("unexpected opencode mcp entry: %+v", ocAT)
	}

	// Re-running is idempotent.
	again, err := WireProviders(WireOptions{
		Home:         home,
		T3Home:       t3Home,
		TincanBinary: "/Users/test/bin/tincan",
		TincanConfig: tincanCfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range again {
		if st.InstanceID == "opencode" && st.Action != "unchanged" {
			t.Fatalf("second run was not idempotent for opencode: %+v", st)
		}
	}

	// With OverrideShared=true, ~/.claude.json is also updated and backed up.
	over, err := WireProviders(WireOptions{
		Home:           home,
		T3Home:         t3Home,
		TincanBinary:   "/Users/test/bin/tincan",
		TincanConfig:   tincanCfg,
		OverrideShared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var updatedClaude bool
	for _, st := range over {
		if st.InstanceID == "claudeAgent" && st.Action == "wired" {
			updatedClaude = true
		}
	}
	if !updatedClaude {
		t.Fatalf("OverrideShared did not wire claudeAgent: %+v", over)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json.bak-tincan-t3")); err != nil {
		t.Fatalf("backup file not written: %v", err)
	}
}

func TestWireProvidersCodexTOML(t *testing.T) {
	home := t.TempDir()
	t3Home := filepath.Join(home, ".t3")
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("model = \"o3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tincanCfg := filepath.Join(home, ".config", "tincan", "t3.json")
	res, err := WireProviders(WireOptions{
		Home:         home,
		T3Home:       t3Home,
		TincanBinary: "/Users/test/bin/tincan",
		TincanConfig: tincanCfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Action != "wired" {
		t.Fatalf("unexpected codex wire result: %+v", res)
	}
	raw, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), `model = "o3"`) || !strings.Contains(string(raw), `TINCAN_CONFIG = "`+tincanCfg+`"`) {
		t.Fatalf("unexpected codex config.toml:\n%s", raw)
	}
}

func TestWireProvidersCodexQuotedKeysAndComments(t *testing.T) {
	home := t.TempDir()
	t3Home := filepath.Join(home, ".t3")
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(codexDir, "config.toml")
	tincanCfg := filepath.Join(home, ".config", "tincan", "t3.json")

	// Existing commented header already wired to tincanCfg must be recognized as wired (no duplicate appended).
	initial := "model = \"o3\"\n\n[mcp_servers.agent-tincan] # existing\ncommand = \"/usr/local/bin/tincan\"\nargs = [\"mcp\"]\n\n[mcp_servers.agent-tincan.env] # env\nTINCAN_CONFIG = \"" + tincanCfg + "\" # pinned\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := WireProviders(WireOptions{
		Home:         home,
		T3Home:       t3Home,
		TincanBinary: "/Users/test/bin/tincan",
		TincanConfig: tincanCfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].State != ProviderWired || res[0].Action != "unchanged" {
		t.Fatalf("commented codex header not recognized as wired: %+v", res)
	}

	// Quoted table header pointing to another config must be stripped cleanly on OverrideShared=true,
	// while a separate server whose name happens to contain "tincan" (tincan-helper) is preserved untouched.
	quoted := "model = \"o3\"\n\n[mcp_servers.\"agent-tincan\"] # shared\ncommand = \"/usr/local/bin/tincan\"\nargs = [\"mcp\"]\n\n[mcp_servers.'agent-tincan'.env]\nTINCAN_CONFIG = \"/tmp/codex.json\"\n\n[mcp_servers.tincan-helper]\ncommand = \"/usr/local/bin/helper\"\n\n[mcp_servers.other]\ncommand = \"echo\"\n"
	if err := os.WriteFile(cfgPath, []byte(quoted), 0o600); err != nil {
		t.Fatal(err)
	}
	over, err := WireProviders(WireOptions{
		Home:           home,
		T3Home:         t3Home,
		TincanBinary:   "/Users/test/bin/tincan",
		TincanConfig:   tincanCfg,
		OverrideShared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(over) != 1 || over[0].Action != "wired" {
		t.Fatalf("expected wired on OverrideShared, got %+v", over)
	}
	outBytes, _ := os.ReadFile(cfgPath)
	out := string(outBytes)
	if strings.Contains(out, `"agent-tincan"`) || strings.Contains(out, `'agent-tincan'`) {
		t.Fatalf("quoted agent-tincan table was not stripped:\n%s", out)
	}
	if !strings.Contains(out, "[mcp_servers.tincan-helper]") || !strings.Contains(out, `command = "/usr/local/bin/helper"`) {
		t.Fatalf("unrelated tincan-helper server was removed:\n%s", out)
	}
	if !strings.Contains(out, "[mcp_servers.other]") || strings.Count(out, "[mcp_servers.agent-tincan]") != 1 {
		t.Fatalf("unexpected overridden codex config.toml:\n%s", out)
	}
}

func TestDiscoverAntigravityIncludesProfilesWithoutMCPConfigYet(t *testing.T) {
	home := t.TempDir()
	t3Home := filepath.Join(home, ".t3")
	profA := filepath.Join(t3Home, "userdata", "providers", "antigravity", "prof-a", "config")
	profB := filepath.Join(t3Home, "userdata", "providers", "antigravity", "prof-b", "config")
	if err := os.MkdirAll(profA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(profB, 0o700); err != nil {
		t.Fatal(err)
	}
	// Profile A already has mcp_config.json; Profile B only has its config/ directory.
	if err := os.WriteFile(filepath.Join(profA, "mcp_config.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	targets, err := DiscoverProviders(home, t3Home)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected both Antigravity profiles discovered, got %d: %+v", len(targets), targets)
	}
}
