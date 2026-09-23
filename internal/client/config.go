package client

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Config is an agent's saved connection to its relay.
type Config struct {
	Relay string `json:"relay"`           // relay base URL, e.g. http://tincan-relay
	Proxy string `json:"proxy,omitempty"` // proxy for relay traffic (Muse: its tailnet tunnel proxy)
	Agent string `json:"agent,omitempty"` // the agent name this config joined as; sent on every relay call
}

// ConfigPath is where the agent config lives. A second agent on the same
// machine sets TINCAN_CONFIG to its own file for join, MCP, and listen. A
// leading ~ is expanded, since MCP config files pass the value unexpanded.
func ConfigPath() string {
	if p := os.Getenv("TINCAN_CONFIG"); p != "" {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			if home, err := os.UserHomeDir(); err == nil {
				return filepath.Join(home, rest)
			}
		}
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "tincan", "client.json")
}

// LoadConfig reads the saved config, then applies TINCAN_RELAY and
// TINCAN_PROXY overrides.
func LoadConfig() (Config, error) {
	var c Config
	raw, err := os.ReadFile(ConfigPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, err
	default:
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, err
		}
	}
	if v := os.Getenv("TINCAN_RELAY"); v != "" {
		c.Relay = v
	}
	if v := os.Getenv("TINCAN_PROXY"); v != "" {
		c.Proxy = v
	}
	return c, nil
}

// SaveConfig writes the config with owner-only permissions.
func SaveConfig(c Config) error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
