package client_test

import (
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// clearProxyEnv unsets every proxy variable DialProxy may read, so the
// machine running the tests cannot lend its own credentials.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "TINCAN_PROXY"} {
		t.Setenv(k, "")
	}
}

// Muse mints a new egress-proxy password for every shell. With
// credentials-from-environment on, the saved tunnel proxy (no password)
// takes the current shell's password from the default proxy on the same
// host, keeping its own port.
func TestDialProxyBorrowsSameHostCredentials(t *testing.T) {
	cases := []struct {
		name string
		cfg  client.Config
		env  map[string]string
		want string
	}{
		{
			name: "flag on borrows from HTTPS_PROXY",
			cfg:  client.Config{Proxy: "http://h:3130", ProxyCredentialsFromEnv: true},
			env:  map[string]string{"HTTPS_PROXY": "http://u:new@h:3128"},
			want: "http://u:new@h:3130",
		},
		{
			name: "flag off leaves the saved URL alone",
			cfg:  client.Config{Proxy: "http://h:3130"},
			env:  map[string]string{"HTTPS_PROXY": "http://u:new@h:3128"},
			want: "http://h:3130",
		},
		{
			name: "flag off keeps a saved password",
			cfg:  client.Config{Proxy: "http://u:old@h:3130"},
			env:  map[string]string{"HTTPS_PROXY": "http://u:new@h:3128"},
			want: "http://u:old@h:3130",
		},
		{
			name: "flag on never borrows from another host",
			cfg:  client.Config{Proxy: "http://h:3130", ProxyCredentialsFromEnv: true},
			env:  map[string]string{"HTTPS_PROXY": "http://u:new@elsewhere:3128"},
			want: "http://h:3130",
		},
		{
			name: "flag on skips another host for a later same-host variable",
			cfg:  client.Config{Proxy: "http://h:3130", ProxyCredentialsFromEnv: true},
			env:  map[string]string{"HTTPS_PROXY": "http://u:new@elsewhere:3128", "ALL_PROXY": "http://v:all@h:3128"},
			want: "http://v:all@h:3130",
		},
		{
			// A wrapper that rewrote client.json with fresh credentials wins
			// over this process's environment, which cannot have changed.
			name: "flag on keeps credentials written into the config",
			cfg:  client.Config{Proxy: "http://u:wrapper@h:3130", ProxyCredentialsFromEnv: true},
			env:  map[string]string{"HTTPS_PROXY": "http://u:new@h:3128"},
			want: "http://u:wrapper@h:3130",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearProxyEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := tc.cfg.DialProxy(); got != tc.want {
				t.Fatalf("DialProxy() = %q, want %q", got, tc.want)
			}
		})
	}
}
