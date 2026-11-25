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

func agyMCPConfigPath(home string) string {
	return filepath.Join(home, ".gemini", "config", "mcp_config.json")
}

type agyServer struct {
	Command string            `json:"command"`
	URL     string            `json:"url"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// agyWakeEnv is what the gemini-cli wake reads to find agy's MCP configs
// and the servers the owner allowed: the owner's values kept in an
// installed service, else the wake's defaults.
type agyWakeEnv struct {
	home, global, workdir string
	allowed               []string
}

func newAgyWakeEnv(home string, owner []envVar) agyWakeEnv {
	e := agyWakeEnv{home: home, global: agyMCPConfigPath(home), workdir: filepath.Join(home, "tincan-gemini")}
	for _, v := range owner {
		switch v.Key {
		case "TINCAN_AGY_MCP_CONFIG":
			if v.Value != "" {
				e.global = v.Value
			}
		case "TINCAN_GEMINI_WORKDIR":
			if v.Value != "" {
				e.workdir = v.Value
			}
		case "TINCAN_WAKE_ALLOWED_SERVERS":
			e.allowed = strings.FieldsFunc(v.Value, func(r rune) bool { return r == ',' || r == ' ' })
		}
	}
	return e
}

// checkAgyEntry stops a gemini-cli install that the wake would refuse at
// its first run. Across agy's MCP config (its default file, plus the
// owner's TINCAN_AGY_MCP_CONFIG when that names another) and the workdir's
// .agents/mcp_config.json there must be exactly one agent-tincan server,
// pinned to this agent's TINCAN_CONFIG, and every other server must be one
// the owner allowed. A server is agent-tincan when its name contains tincan
// or its command is a tincan binary, the same rule as the wake library's
// identity check.
func checkAgyEntry(e agyWakeEnv, cfg, binary string) error {
	addCmd := fmt.Sprintf("agy mcp add -e TINCAN_CONFIG=%s agent-tincan %s mcp", shellQuote(cfg), shellQuote(binary))
	add := "Add it with: " + addCmd
	servers := map[string]agyServer{}
	var tincan, extra []string
	paths := []string{agyMCPConfigPath(e.home), filepath.Join(e.workdir, ".agents", "mcp_config.json")}
	if e.global != paths[0] {
		paths = append(paths, e.global)
	}
	found := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		found++
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
		for name, s := range doc.Servers {
			switch {
			case strings.Contains(strings.ToLower(name), "tincan") || filepath.Base(s.Command) == "tincan":
				tincan = append(tincan, name)
				servers[name] = s
			case !slices.Contains(e.allowed, name):
				extra = append(extra, name)
			}
		}
	}
	slices.Sort(tincan)
	slices.Sort(extra)
	switch {
	case found == 0:
		return fmt.Errorf("agy has no MCP config at %s, so a woken agy could not reach the team. %s Nothing was installed", e.global, add)
	case len(tincan) == 0:
		return fmt.Errorf("agy's MCP configs (%s) have no agent-tincan server, so a woken agy could not reach the team. %s Nothing was installed", strings.Join(paths, ", "), add)
	case len(tincan) > 1:
		return fmt.Errorf("agy's MCP configs have %d agent-tincan servers (%s); the wake needs exactly one, pinned to %s. Remove the others with agy mcp remove. Nothing was installed", len(tincan), strings.Join(tincan, ", "), cfg)
	}
	if len(extra) > 0 {
		return fmt.Errorf("agy's MCP configs have servers the wake would refuse to run with: %s. Every wake lets agy call any configured server's tools, so remove them with agy mcp remove, or list them in TINCAN_WAKE_ALLOWED_SERVERS in the listener's environment. Nothing was installed", strings.Join(extra, ", "))
	}
	pin := servers[tincan[0]].Env["TINCAN_CONFIG"]
	if pin == "" {
		return fmt.Errorf("the %s server in agy's MCP config sets no TINCAN_CONFIG, so it would run as this machine's default agent. Remove it with agy mcp remove %s, then %s Nothing was installed", tincan[0], tincan[0], addCmd)
	}
	if !sameConfigFile(pin, cfg) {
		return fmt.Errorf("the %s server in agy's MCP config runs tincan with TINCAN_CONFIG=%s, which is another agent's config, so a woken agy would act as that agent. Point it at %s. Nothing was installed", tincan[0], pin, cfg)
	}
	return nil
}

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
