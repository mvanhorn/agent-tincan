package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

func TestWebInstallWritesDefinitionWithoutLoading(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service definition on " + runtime.GOOS)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A PATH with no launchctl or systemctl proves install never runs them.
	t.Setenv("PATH", t.TempDir())
	for _, site := range []string{"chatgpt", "claude-ai"} {
		out, err := run(t, Root(), "web", "install", "--site", site, "--binary", "/opt/tincan/tincan")
		if err != nil {
			t.Fatalf("%s: %v\n%s", site, err, out)
		}
		agent := history.WebAgentName(history.Source(site))
		def := filepath.Join(home, ".config", "systemd", "user", "tincan-"+agent+".service")
		if runtime.GOOS == "darwin" {
			def = filepath.Join(home, "Library", "LaunchAgents", history.WebServiceLabel(history.Source(site))+".plist")
		}
		b, err := os.ReadFile(def)
		if err != nil {
			t.Fatalf("%s: definition not written: %v\n%s", site, err, out)
		}
		for _, want := range []string{"/opt/tincan/tincan", site, agent + ".json"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s: definition missing %q", site, want)
			}
		}
		for _, want := range []string{def, "(not started)", "TINCAN_CONFIG=~/.config/tincan/" + agent + ".json tincan join"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output missing %q:\n%s", site, want, out)
			}
		}
	}
	if _, err := run(t, Root(), "web", "install", "--site", "codex"); err == nil {
		t.Fatal("site codex accepted")
	}
	if _, err := run(t, Root(), "web", "install"); err == nil {
		t.Fatal("missing --site accepted")
	}
}

func TestWebServeRefusesAnotherAgentAndMissingConfig(t *testing.T) {
	t.Setenv("TINCAN_RELAY", "")
	allow := filepath.Join(t.TempDir(), "allow.txt")
	_, err := run(t, Root(), "web", "serve", "--site", "chatgpt", "--config", filepath.Join(t.TempDir(), "chatgpt-web.json"), "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), "no relay configured") {
		t.Fatalf("no config: %v", err)
	}
	// Config joined as codex: refused before any poll or claim.
	srv := whoamiRelay(t, "codex")
	_, err = run(t, Root(), "web", "serve", "--site", "chatgpt", "--config", serveConfig(t, srv.URL, "codex"), "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), `"codex"`) || !strings.Contains(err.Error(), "chatgpt-web") {
		t.Fatalf("codex config: %v", err)
	}
	// No agent in the config: the relay's answer decides.
	_, err = run(t, Root(), "web", "serve", "--site", "claude-ai", "--config", serveConfig(t, srv.URL, ""), "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), `"codex"`) || !strings.Contains(err.Error(), "claude-web") {
		t.Fatalf("whoami codex: %v", err)
	}
	if err := os.WriteFile(allow, []byte("bad/name\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = run(t, Root(), "web", "serve", "--site", "chatgpt", "--config", serveConfig(t, srv.URL, "chatgpt-web"), "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("bad allowlist: %v", err)
	}
}

func TestHistoryInstallExtensionDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TINCAN_HISTORY_NATIVE_DIR", filepath.Join(home, "native"))
	ext, err := filepath.Abs(filepath.Join("..", "..", "extension"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, Root(), "history", "install", "--binary", "/opt/tincan/tincan", "--no-service", "--extension-dir", ext)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	w, err := os.ReadFile(filepath.Join(home, "native", "native-host"))
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	if err != nil || !strings.Contains(string(w), history.ExtensionDirEnv+"='"+ext+"'") || !strings.Contains(out, "reloads itself") {
		t.Fatalf("wrapper:\n%s\noutput:\n%s", w, out)
	}
	if _, err := run(t, Root(), "history", "install", "--binary", "/opt/tincan/tincan", "--no-service", "--extension-dir", t.TempDir()); err == nil {
		t.Fatal("dir without a manifest accepted")
	}
}
