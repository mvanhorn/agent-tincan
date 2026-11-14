package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// A profile name the extension's options page would refuse is refused by
// web serve and web install too.
func TestWebProfileFlagRefusesABadName(t *testing.T) {
	setHome(t, t.TempDir())
	for _, args := range [][]string{
		{"web", "serve", "--site", "chatgpt", "--profile", "Work!"},
		{"web", "install", "--site", "chatgpt", "--profile", "Work!", "--binary", "/opt/tincan"},
		{"web", "install", "--site", "chatgpt", "--profile", "../x", "--binary", "/opt/tincan"},
	} {
		out, err := run(t, Root(), args...)
		if err == nil || !strings.Contains(err.Error(), "lowercase letters") {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
}

// web install --profile keeps the profile in the service it writes.
func TestWebInstallKeepsTheProfile(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service definition on " + runtime.GOOS)
	}
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PATH", t.TempDir())
	out, err := run(t, Root(), "web", "install", "--site", "claude-ai", "--name", "claude-web-1", "--profile", "work", "--binary", "/opt/tincan/tincan")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	def := filepath.Join(home, ".config", "systemd", "user", "tincan-claude-web-1.service")
	want := "--profile work"
	if runtime.GOOS == "darwin" {
		def = filepath.Join(home, "Library", "LaunchAgents", history.WebServiceLabel(history.SourceClaudeAI)+".claude-web-1.plist")
		want = "<string>--profile</string>\n    <string>work</string>"
	}
	b, err := os.ReadFile(def)
	if err != nil {
		t.Fatalf("definition: %v\n%s", err, out)
	}
	if !strings.Contains(string(b), want) {
		t.Fatalf("definition lacks %q:\n%s", want, b)
	}
}

// web serve --profile waits at startup for that profile's native host,
// saying so once, and starts when it answers. The unnamed profile's host
// answering meanwhile does not count.
func TestWebServeProfileWaitsForItsHost(t *testing.T) {
	dir := shortNativeDir(t)
	old := webProfileRecheck
	webProfileRecheck = 50 * time.Millisecond
	t.Cleanup(func() { webProfileRecheck = old })
	connected := func() history.ExtensionStatus {
		return history.ExtensionStatus{Hello: true, Granted: []string{"chatgpt", "claudeai"}}
	}
	fakeHostStatus(t, dir, connected)
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "chatgpt-web"})
		case "/v1/poll":
			polls.Add(1)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := Root()
	var out syncBuffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"web", "serve", "--site", "chatgpt", "--profile", "work", "--config", serveConfig(t, srv.URL, "chatgpt-web"), "--allowlist", filepath.Join(t.TempDir(), "allow.txt")})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	time.Sleep(400 * time.Millisecond)
	if n := polls.Load(); n != 0 {
		t.Fatalf("%d polls before the work profile's host answered:\n%s", n, out.String())
	}
	if log := out.String(); strings.Count(log, "waiting for Chrome profile work") != 1 || !strings.Contains(log, "host-work.sock") {
		t.Fatalf("want the wait logged once, naming the socket:\n%s", log)
	}
	fakeHostStatusAt(t, filepath.Join(dir, "host-work.sock"), connected)
	deadline := time.Now().Add(5 * time.Second)
	for polls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if polls.Load() == 0 {
		t.Fatalf("web serve did not start once the profile's host answered:\n%s", out.String())
	}
	if log := out.String(); !strings.Contains(log, "Chrome profile work is connected") || !strings.Contains(log, "through Chrome profile work") {
		t.Fatalf("log:\n%s", log)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("web serve did not stop")
	}
}

// Once running, a request for a profile whose host went away fails as
// not connected; it is never sent through the unnamed profile's host,
// which is still answering.
func TestProfileClientNeverFallsBackToAnotherSocket(t *testing.T) {
	dir := shortNativeDir(t)
	var defaultAsked atomic.Int32
	fakeHostStatus(t, dir, func() history.ExtensionStatus {
		defaultAsked.Add(1)
		return history.ExtensionStatus{Hello: true, Granted: []string{"chatgpt"}}
	})
	c := history.NewClientFor("work")
	c.Channel.(*history.SocketChannel).ChromeRunning = func() bool { return true }
	_, err := c.Request(context.Background(), history.OpChatGPTList, history.OpArgs{Count: 1})
	if err == nil || !strings.Contains(err.Error(), "extension is not connected") {
		t.Fatalf("request for a gone profile: %v", err)
	}
	if _, err := c.ExtensionStatus(context.Background()); err == nil {
		t.Fatal("status for a gone profile answered")
	}
	if n := defaultAsked.Load(); n != 0 {
		t.Fatalf("the unnamed profile's host was asked %d times", n)
	}
}

// web install --profile asks that profile's browser, not the unnamed
// one, for the dot's thread and for a Duet channel check.
func TestWebInstallAsksThroughTheProfile(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service definition on " + runtime.GOOS)
	}
	setHome(t, t.TempDir())
	t.Setenv("PATH", t.TempDir())
	var asked []string
	savedDot, savedCheck := dotPrimary, checkWebThread
	t.Cleanup(func() { dotPrimary, checkWebThread = savedDot, savedCheck })
	dotPrimary = func(_ context.Context, profile string) (history.DotPrimary, error) {
		asked = append(asked, "dot:"+profile)
		return history.DotPrimary{Name: "Orbit", Thread: "01a0efac-8fd4-71f2-8aa9-0004fbd78034"}, nil
	}
	checkWebThread = func(_ context.Context, _ history.Source, _ string, profile string) error {
		asked = append(asked, "duet:"+profile)
		return nil
	}
	if out, err := run(t, Root(), "web", "install", "--site", "dots", "--profile", "work", "--binary", "/opt/tincan"); err != nil {
		t.Fatalf("dots: %v\n%s", err, out)
	}
	if out, err := run(t, Root(), "web", "install", "--site", "duet", "--thread", "chan_0000000000000000chan01", "--profile", "work", "--binary", "/opt/tincan"); err != nil {
		t.Fatalf("duet: %v\n%s", err, out)
	}
	if strings.Join(asked, ",") != "dot:work,duet:work" {
		t.Fatalf("asked %v", asked)
	}
}
