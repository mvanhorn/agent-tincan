package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// agyMCPConfigPath is the MCP config "agy mcp add" writes, which the
// gemini-cli wake reads by default.
func agyMCPConfigPath(home string) string {
	return filepath.Join(home, ".gemini", "config", "mcp_config.json")
}

type agyServer struct {
	Command string            `json:"command"`
	URL     string            `json:"url"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// checkAgyEntry stops a gemini-cli install that the wake would refuse at
// its first run: agy's MCP config must hold exactly one agent-tincan
// server, pinned to this agent's TINCAN_CONFIG. A server is agent-tincan
// when its name contains tincan or its command is a tincan binary, the
// same rule as the wake library's identity check.
func checkAgyEntry(path, cfg, binary string) error {
	add := fmt.Sprintf("Add it with: agy mcp add -e TINCAN_CONFIG=%s agent-tincan %s mcp", shellQuote(cfg), shellQuote(binary))
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("agy has no MCP config at %s, so a woken agy could not reach the team. %s Nothing was installed", path, add)
	}
	if err != nil {
		return fmt.Errorf("read agy's MCP config: %w. Nothing was installed", err)
	}
	var doc struct {
		Servers map[string]agyServer `json:"mcpServers"`
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("agy's MCP config %s is not valid JSON (%v). Nothing was installed", path, err)
		}
	}
	var names []string
	for name, s := range doc.Servers {
		if strings.Contains(strings.ToLower(name), "tincan") || filepath.Base(s.Command) == "tincan" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	switch len(names) {
	case 0:
		return fmt.Errorf("agy's MCP config %s has no agent-tincan server, so a woken agy could not reach the team. %s Nothing was installed", path, add)
	case 1:
	default:
		return fmt.Errorf("agy's MCP config %s has %d agent-tincan servers (%s); the wake needs exactly one, pinned to %s. Remove the others with agy mcp remove. Nothing was installed", path, len(names), strings.Join(names, ", "), cfg)
	}
	pin := doc.Servers[names[0]].Env["TINCAN_CONFIG"]
	if pin == "" {
		return fmt.Errorf("the %s server in %s sets no TINCAN_CONFIG, so it would run as this machine's default agent. Remove it with agy mcp remove %s, then %s Nothing was installed", names[0], path, names[0], strings.TrimPrefix(add, "Add it with: "))
	}
	if !sameConfigFile(pin, cfg) {
		return fmt.Errorf("the %s server in %s runs tincan with TINCAN_CONFIG=%s, which is another agent's config, so a woken agy would act as that agent. Point it at %s. Nothing was installed", names[0], path, pin, cfg)
	}
	return nil
}

// sameConfigFile reports whether a and b name the same file once their
// directories' symlinks are resolved, as the wake compares them.
func sameConfigFile(a, b string) bool {
	if a == b {
		return true
	}
	resolve := func(p string) string {
		if d, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			return filepath.Join(d, filepath.Base(p))
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}
