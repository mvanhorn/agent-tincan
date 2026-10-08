package client

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is an agent's saved connection to its relay.
type Config struct {
	Relay string `json:"relay"`           // relay base URL, e.g. http://tincan-relay
	Proxy string `json:"proxy,omitempty"` // proxy for relay traffic (Muse: its tailnet tunnel proxy)
	Agent string `json:"agent,omitempty"` // the agent name this config joined as; sent on every relay call
	// RelayKey is the relay's secret, learned from whoami. With it the
	// client can find its relay again after the relay's address changes,
	// by proving IPv4 tailnet peers rather than walking a host-name list.
	RelayKey string `json:"relay_key,omitempty"`
	// RelayURLs are the addresses the relay last advertised for itself
	// (its tailnet name first), tried before searching the live netmap.
	RelayURLs []string `json:"relay_urls,omitempty"`
	// RelayInfoAt is when RelayKey and RelayURLs were last refreshed.
	RelayInfoAt time.Time `json:"relay_info_at,omitzero"`
	// ProxyCredentialsFromEnv says Proxy is saved without a password, and
	// each command takes the username and password from the environment's
	// proxy on the same host (see DialProxy). For sandboxes like Muse that
	// mint a new proxy password for every shell.
	ProxyCredentialsFromEnv bool `json:"proxy_credentials_from_env,omitempty"`
}

// proxyEnv lists the variables DialProxy borrows credentials from, in
// order, each with the lower-case spelling tools also accept.
var proxyEnv = [][2]string{{"HTTPS_PROXY", "https_proxy"}, {"HTTP_PROXY", "http_proxy"}, {"ALL_PROXY", "all_proxy"}}

// DialProxy is the proxy URL relay traffic goes through. It is Proxy as
// saved, except that with ProxyCredentialsFromEnv on and no credentials in
// Proxy, it takes them from the first of HTTPS_PROXY, HTTP_PROXY and
// ALL_PROXY whose host is Proxy's host, keeping Proxy's own port. Credentials
// written into the config (by a wrapper, or TINCAN_PROXY) win: they can be
// newer than this process's environment.
func (c Config) DialProxy() string {
	if !c.ProxyCredentialsFromEnv || c.Proxy == "" {
		return c.Proxy
	}
	saved, err := url.Parse(c.Proxy)
	if err != nil || saved.User != nil {
		return c.Proxy
	}
	for _, names := range proxyEnv {
		v := os.Getenv(names[0])
		if v == "" {
			v = os.Getenv(names[1])
		}
		if v == "" {
			continue
		}
		if !strings.Contains(v, "://") {
			v = "http://" + v // a bare host:port, as Go's proxy settings accept
		}
		env, err := url.Parse(v)
		if err != nil || env.User == nil || !strings.EqualFold(env.Hostname(), saved.Hostname()) {
			continue
		}
		saved.User = env.User
		return saved.String()
	}
	return c.Proxy
}

// WithoutProxyPassword returns proxy with its username and password removed,
// for saving a config whose credentials come from the environment.
func WithoutProxyPassword(proxy string) string {
	u, err := url.Parse(proxy)
	if err != nil || u.User == nil {
		return proxy
	}
	u.User = nil
	return u.String()
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
func LoadConfig() (Config, error) { return LoadConfigFrom(ConfigPath()) }

// LoadConfigFrom reads the config at path, then applies TINCAN_RELAY and
// TINCAN_PROXY overrides. A missing file is an empty config.
func LoadConfigFrom(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
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

// SaveConfig writes the config to ConfigPath() with owner-only permissions.
func SaveConfig(c Config) error { return SaveConfigTo(ConfigPath(), c) }

// SaveConfigTo writes the config to path with owner-only permissions,
// creating the directory as needed.
func SaveConfigTo(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, raw)
}
