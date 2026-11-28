package t3bridge

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

// ProviderState describes the Agent Tincan MCP wiring status of one T3 provider config.
type ProviderState string

const (
	ProviderWired       ProviderState = "wired"
	ProviderSharedOther ProviderState = "shared_other"
	ProviderMissing     ProviderState = "missing"
	ProviderError       ProviderState = "error"
)

// ProviderTarget represents one MCP config file used by a T3 Code provider instance.
type ProviderTarget struct {
	InstanceID  string `json:"instance_id"`
	Driver      string `json:"driver"`
	DisplayName string `json:"display_name,omitempty"`
	ConfigPath  string `json:"config_path"`
	Format      string `json:"format"` // "mcpServers", "opencode", "codex"
}

// ProviderStatus is the inspection/wiring outcome for one ProviderTarget.
type ProviderStatus struct {
	ProviderTarget
	State          ProviderState `json:"state"`
	ExistingConfig string        `json:"existing_config,omitempty"`
	Action         string        `json:"action,omitempty"`
	Detail         string        `json:"detail,omitempty"`
}

// WireOptions configures WireProviders.
type WireOptions struct {
	Home           string
	T3Home         string
	TincanBinary   string
	TincanConfig   string
	OverrideShared bool
}

type t3SettingsFile struct {
	ProviderInstances map[string]struct {
		Driver      string `json:"driver"`
		DisplayName string `json:"displayName"`
		Enabled     *bool  `json:"enabled"`
		Config      struct {
			HomePath string `json:"homePath"`
		} `json:"config"`
	} `json:"providerInstances"`
}

// DiscoverProviders inspects ~/.t3/userdata/settings.json and T3's isolated
// provider directories to locate every MCP config file T3 Code loads.
func DiscoverProviders(home, t3Home string) ([]ProviderTarget, error) {
	if t3Home == "" {
		t3Home = DefaultT3Home(home)
	}
	var targets []ProviderTarget
	seenPaths := map[string]bool{}
	add := func(t ProviderTarget) {
		clean := filepath.Clean(t.ConfigPath)
		if seenPaths[clean] {
			return
		}
		seenPaths[clean] = true
		t.ConfigPath = clean
		targets = append(targets, t)
	}

	settingsPath := filepath.Join(t3Home, "userdata", "settings.json")
	if raw, err := os.ReadFile(settingsPath); err == nil {
		var sf t3SettingsFile
		if err := json.Unmarshal(raw, &sf); err != nil {
			return nil, fmt.Errorf("parse T3 settings %s: %w", settingsPath, err)
		}
		ids := make([]string, 0, len(sf.ProviderInstances))
		for id := range sf.ProviderInstances {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			inst := sf.ProviderInstances[id]
			if inst.Enabled != nil && !*inst.Enabled {
				continue
			}
			drv := strings.ToLower(strings.TrimSpace(inst.Driver))
			switch drv {
			case "claudeagent", "claude":
				base := home
				if hp := strings.TrimSpace(inst.Config.HomePath); hp != "" {
					base = expandHomeDir(hp, home)
				}
				add(ProviderTarget{
					InstanceID:  id,
					Driver:      inst.Driver,
					DisplayName: inst.DisplayName,
					ConfigPath:  filepath.Join(base, ".claude.json"),
					Format:      "mcpServers",
				})
			case "opencode":
				add(ProviderTarget{
					InstanceID:  id,
					Driver:      inst.Driver,
					DisplayName: inst.DisplayName,
					ConfigPath:  filepath.Join(home, ".config", "opencode", "opencode.json"),
					Format:      "opencode",
				})
			case "codex":
				codexHome := filepath.Join(home, ".codex")
				if hp := strings.TrimSpace(inst.Config.HomePath); hp != "" {
					codexHome = expandHomeDir(hp, home)
				}
				add(ProviderTarget{
					InstanceID:  id,
					Driver:      inst.Driver,
					DisplayName: inst.DisplayName,
					ConfigPath:  filepath.Join(codexHome, "config.toml"),
					Format:      "codex",
				})
			case "antigravity":
				// Antigravity runs under T3-isolated profile directories.
				for _, p := range discoverAntigravityMCPConfigs(t3Home) {
					add(ProviderTarget{
						InstanceID:  id,
						Driver:      inst.Driver,
						DisplayName: inst.DisplayName,
						ConfigPath:  p,
						Format:      "mcpServers",
					})
				}
			}
		}
	}

	// Always include any T3-isolated Antigravity profiles even when settings.json omits providerInstances.
	for _, p := range discoverAntigravityMCPConfigs(t3Home) {
		add(ProviderTarget{
			InstanceID:  "antigravity",
			Driver:      "antigravity",
			DisplayName: "Antigravity (T3)",
			ConfigPath:  p,
			Format:      "mcpServers",
		})
	}

	// Include default Claude/OpenCode/Codex files if settings.json did not exist and they are present.
	if _, err := os.Stat(settingsPath); errors.Is(err, fs.ErrNotExist) {
		for _, cand := range []ProviderTarget{
			{InstanceID: "claudeAgent", Driver: "claudeAgent", ConfigPath: filepath.Join(home, ".claude.json"), Format: "mcpServers"},
			{InstanceID: "opencode", Driver: "opencode", ConfigPath: filepath.Join(home, ".config", "opencode", "opencode.json"), Format: "opencode"},
			{InstanceID: "codex", Driver: "codex", ConfigPath: filepath.Join(home, ".codex", "config.toml"), Format: "codex"},
		} {
			if _, err := os.Stat(cand.ConfigPath); err == nil {
				add(cand)
			}
		}
	}

	return targets, nil
}

func discoverAntigravityMCPConfigs(t3Home string) []string {
	pattern := filepath.Join(t3Home, "userdata", "providers", "antigravity", "*", "config", "mcp_config.json")
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		// Also find profile directories where config/ exists even if mcp_config.json has not been created yet.
		dirs, _ := filepath.Glob(filepath.Join(t3Home, "userdata", "providers", "antigravity", "*", "config"))
		for _, d := range dirs {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				matches = append(matches, filepath.Join(d, "mcp_config.json"))
			}
		}
	}
	slices.Sort(matches)
	return matches
}

// InspectProviders returns the current Agent Tincan MCP wiring status of each discovered T3 provider.
func InspectProviders(home, t3Home, tincanConfig string) ([]ProviderStatus, error) {
	targets, err := DiscoverProviders(home, t3Home)
	if err != nil {
		return nil, err
	}
	wantCfg := cleanAbs(tincanConfig)
	out := make([]ProviderStatus, 0, len(targets))
	for _, tgt := range targets {
		out = append(out, inspectOneProvider(tgt, wantCfg))
	}
	return out, nil
}

// WireProviders adds agent-tincan (pinned to opts.TincanConfig) to each T3
// provider config that does not already point to another standalone teammate.
func WireProviders(opts WireOptions) ([]ProviderStatus, error) {
	if strings.TrimSpace(opts.TincanBinary) == "" {
		return nil, errors.New("WireProviders requires TincanBinary")
	}
	if strings.TrimSpace(opts.TincanConfig) == "" {
		return nil, errors.New("WireProviders requires TincanConfig")
	}
	targets, err := DiscoverProviders(opts.Home, opts.T3Home)
	if err != nil {
		return nil, err
	}
	wantCfg := cleanAbs(opts.TincanConfig)
	results := make([]ProviderStatus, 0, len(targets))
	for _, tgt := range targets {
		st := inspectOneProvider(tgt, wantCfg)
		switch st.State {
		case ProviderError:
			st.Action = "error"
			results = append(results, st)
			continue
		case ProviderWired:
			st.Action = "unchanged"
			results = append(results, st)
			continue
		case ProviderSharedOther:
			if !opts.OverrideShared {
				st.Action = "skipped"
				st.Detail = fmt.Sprintf("kept existing agent-tincan (%s); pass --override-shared to switch to %s", nonEmpty(st.ExistingConfig, "default config"), wantCfg)
				results = append(results, st)
				continue
			}
		}
		if err := writeProviderConfig(tgt, opts.TincanBinary, wantCfg); err != nil {
			st.State = ProviderError
			st.Action = "error"
			st.Detail = err.Error()
		} else {
			st.State = ProviderWired
			st.Action = "wired"
			st.ExistingConfig = wantCfg
			st.Detail = "added agent-tincan pinned to " + wantCfg
		}
		results = append(results, st)
	}
	return results, nil
}

func inspectOneProvider(tgt ProviderTarget, wantCfg string) ProviderStatus {
	st := ProviderStatus{ProviderTarget: tgt}
	raw, err := os.ReadFile(tgt.ConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		st.State = ProviderMissing
		st.Detail = "config file does not exist yet"
		return st
	}
	if err != nil {
		st.State = ProviderError
		st.Detail = err.Error()
		return st
	}
	switch tgt.Format {
	case "mcpServers":
		return inspectJSONMap(st, raw, "mcpServers", "env", wantCfg)
	case "opencode":
		return inspectJSONMap(st, raw, "mcp", "environment", wantCfg)
	case "codex":
		return inspectCodexTOML(st, string(raw), wantCfg)
	default:
		st.State = ProviderError
		st.Detail = "unsupported provider format " + tgt.Format
		return st
	}
}

func inspectJSONMap(st ProviderStatus, raw []byte, topKey, envKey, wantCfg string) ProviderStatus {
	if len(strings.TrimSpace(string(raw))) == 0 {
		st.State = ProviderMissing
		return st
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		st.State = ProviderError
		st.Detail = fmt.Sprintf("invalid JSON in %s: %v", st.ConfigPath, err)
		return st
	}
	servers, _ := doc[topKey].(map[string]any)
	if len(servers) == 0 {
		st.State = ProviderMissing
		return st
	}
	for name, v := range servers {
		m, ok := v.(map[string]any)
		if !ok || !entryIsTincan(name, m) {
			continue
		}
		envMap, _ := m[envKey].(map[string]any)
		cfgStr, _ := envMap["TINCAN_CONFIG"].(string)
		st.ExistingConfig = cfgStr
		if cfgStr != "" && cleanAbs(cfgStr) == wantCfg {
			st.State = ProviderWired
		} else {
			st.State = ProviderSharedOther
		}
		return st
	}
	st.State = ProviderMissing
	return st
}

func inspectCodexTOML(st ProviderStatus, raw, wantCfg string) ProviderStatus {
	lines := strings.Split(raw, "\n")
	inTincan := false
	found := false
	existingCfg := ""
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			sec := strings.Trim(t, "[] ")
			if sec == "mcp_servers.agent-tincan" || sec == "mcp_servers.agent-tincan.env" || (strings.HasPrefix(sec, "mcp_servers.") && strings.Contains(strings.ToLower(sec), "tincan")) {
				inTincan = true
				found = true
			} else {
				inTincan = false
			}
			continue
		}
		if inTincan && strings.Contains(t, "TINCAN_CONFIG") {
			if _, rhs, ok := strings.Cut(t, "="); ok {
				existingCfg = strings.Trim(strings.TrimSpace(rhs), "\"'")
			}
		}
	}
	if !found {
		st.State = ProviderMissing
		return st
	}
	st.ExistingConfig = existingCfg
	if existingCfg != "" && cleanAbs(existingCfg) == wantCfg {
		st.State = ProviderWired
	} else {
		st.State = ProviderSharedOther
	}
	return st
}

func writeProviderConfig(tgt ProviderTarget, binary, tincanCfg string) error {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(tgt.ConfigPath); err == nil {
		mode = fi.Mode().Perm()
		if raw, err := os.ReadFile(tgt.ConfigPath); err == nil {
			_ = os.WriteFile(tgt.ConfigPath+".bak-tincan-t3", raw, mode)
		}
	}
	switch tgt.Format {
	case "mcpServers":
		return writeJSONProvider(tgt.ConfigPath, mode, "mcpServers", map[string]any{
			"command": binary,
			"args":    []string{"mcp"},
			"env": map[string]string{
				"TINCAN_CONFIG": tincanCfg,
			},
		})
	case "opencode":
		return writeJSONProvider(tgt.ConfigPath, mode, "mcp", map[string]any{
			"type":    "local",
			"command": []string{binary, "mcp"},
			"environment": map[string]string{
				"TINCAN_CONFIG": tincanCfg,
			},
			"enabled": true,
		})
	case "codex":
		return writeCodexProvider(tgt.ConfigPath, mode, binary, tincanCfg)
	default:
		return fmt.Errorf("unsupported provider format %s", tgt.Format)
	}
}

func writeJSONProvider(path string, mode os.FileMode, topKey string, entry map[string]any) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	servers, _ := doc[topKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["agent-tincan"] = entry
	doc[topKey] = servers

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return writeFileAtomicWithMode(path, out, mode)
}

func writeCodexProvider(path string, mode os.FileMode, binary, tincanCfg string) error {
	var raw string
	if b, err := os.ReadFile(path); err == nil {
		raw = string(b)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Strip any existing [mcp_servers.agent-tincan] and [mcp_servers.agent-tincan.env] sections.
	lines := strings.Split(raw, "\n")
	var kept []string
	skip := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			sec := strings.Trim(t, "[] ")
			skip = sec == "mcp_servers.agent-tincan" || sec == "mcp_servers.agent-tincan.env"
		}
		if !skip {
			kept = append(kept, line)
		}
	}
	base := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	block := fmt.Sprintf("\n\n[mcp_servers.agent-tincan]\ncommand = %q\nargs = [\"mcp\"]\n\n[mcp_servers.agent-tincan.env]\nTINCAN_CONFIG = %q\n", binary, tincanCfg)
	var final string
	if base == "" {
		final = strings.TrimLeft(block, "\n")
	} else {
		final = base + block
	}
	return writeFileAtomicWithMode(path, []byte(final), mode)
}

func writeFileAtomicWithMode(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tincan-t3-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func entryIsTincan(name string, m map[string]any) bool {
	if strings.Contains(strings.ToLower(name), "tincan") {
		return true
	}
	switch cmd := m["command"].(type) {
	case string:
		return filepath.Base(cmd) == "tincan"
	case []any:
		if len(cmd) > 0 {
			if s, ok := cmd[0].(string); ok && filepath.Base(s) == "tincan" {
				return true
			}
		}
	}
	return false
}

func expandHomeDir(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func cleanAbs(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}
