package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestWebInstallWritesDefinitionWithoutLoading(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service definition on " + runtime.GOOS)
	}
	home := t.TempDir()
	setHome(t, home)
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
	setHome(t, home)
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

// shortNativeDir is a native host directory short enough for a unix
// socket path, with HOME moved so nothing touches the real config.
func shortNativeDir(t *testing.T) string {
	t.Helper()
	skipOnWindows(t, "the native host socket is a unix socket")
	dir, err := os.MkdirTemp("/tmp", "tcw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TINCAN_HISTORY_NATIVE_DIR", dir)
	setHome(t, t.TempDir())
	return dir
}

// fakeHostStatus serves the native host socket in dir and answers
// host.status with status(), as a native host would.
func fakeHostStatus(t *testing.T, dir string, status func() history.ExtensionStatus) {
	t.Helper()
	ln, err := history.ListenSocket(filepath.Join(dir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			b, err := history.ReadMessage(conn, 1<<20)
			var req history.NativeRequest
			if err == nil && json.Unmarshal(b, &req) == nil {
				resp := history.NativeResponse{ID: req.ID, Error: &history.NativeError{Code: "bad_request", Message: "unexpected op"}}
				if req.Op == history.OpHostStatus {
					res, _ := json.Marshal(status())
					resp = history.NativeResponse{ID: req.ID, OK: true, Result: res}
				}
				_ = history.WriteMessage(conn, resp, 1<<20)
			}
			conn.Close()
		}
	}()
}

// The extension is connected and says chatgpt.com is not granted: web
// serve does not exit (the service manager would restart it every few
// seconds, forever). It logs the missing grant once, naming the options
// page, makes no poll while it waits, and starts serving once the grant
// appears.
func TestWebServeWaitsForAMissingGrant(t *testing.T) {
	dir := shortNativeDir(t)
	old := webGrantRecheck
	webGrantRecheck = 50 * time.Millisecond
	t.Cleanup(func() { webGrantRecheck = old })
	var granted atomic.Bool
	fakeHostStatus(t, dir, func() history.ExtensionStatus {
		if granted.Load() {
			return history.ExtensionStatus{Hello: true, Granted: []string{"claudeai", "chatgpt"}}
		}
		return history.ExtensionStatus{Hello: true, Granted: []string{"claudeai"}}
	})
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
	cmd.SetArgs([]string{"web", "serve", "--site", "chatgpt", "--config", serveConfig(t, srv.URL, "chatgpt-web"), "--allowlist", filepath.Join(t.TempDir(), "allow.txt")})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	time.Sleep(400 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("web serve exited on a missing grant: %v\n%s", err, out.String())
	default:
	}
	log := out.String()
	if n := strings.Count(log, "no access to chatgpt.com"); n != 1 || !strings.Contains(log, "Extension options") {
		t.Fatalf("want the missing grant logged once, naming the options page:\n%s", log)
	}
	if n := polls.Load(); n != 0 {
		t.Fatalf("%d polls while the grant is missing", n)
	}
	granted.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for polls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if polls.Load() == 0 {
		t.Fatalf("web serve did not start serving after the grant:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "serving chatgpt") {
		t.Fatalf("no serving line:\n%s", out.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("web serve did not stop")
	}
}

// launchd starts web serve at login, often before Chrome: with no
// extension connected it starts, holds one long poll (no tight loop), and
// stops cleanly.
func TestWebServeWithoutExtensionStartsAndDoesNotLoop(t *testing.T) {
	shortNativeDir(t)
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
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"web", "serve", "--site", "chatgpt", "--config", serveConfig(t, srv.URL, "chatgpt-web"), "--allowlist", filepath.Join(t.TempDir(), "allow.txt")})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	time.Sleep(700 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("web serve exited without an extension: %v\n%s", err, out.String())
	default:
	}
	if n := polls.Load(); n != 1 {
		t.Fatalf("%d polls in 700ms; want one held long poll", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("web serve did not stop")
	}
}

// The dots site needs its thread: web serve and web install refuse it
// without a valid --thread, and install keeps the thread in the service;
// no other site takes --thread.
func TestWebDotsNeedsThread(t *testing.T) {
	t.Setenv("TINCAN_RELAY", "")
	allow := filepath.Join(t.TempDir(), "allow.txt")
	cfg := filepath.Join(t.TempDir(), "dot-web.json")
	for _, args := range [][]string{
		{"web", "serve", "--site", "dots", "--config", cfg, "--allowlist", allow},
		{"web", "serve", "--site", "dots", "--thread", "not/a/thread", "--config", cfg, "--allowlist", allow},
		{"web", "serve", "--site", "chatgpt", "--thread", "0d0d0d0d-1111-7222-8333-000000000001", "--config", cfg, "--allowlist", allow},
	} {
		_, err := run(t, Root(), args...)
		if err == nil || !strings.Contains(err.Error(), "--thread") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return
	}
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PATH", t.TempDir())
	// Without --thread, install asks the extension for the primary dot;
	// when it cannot answer, install fails naming the reason and --thread.
	for _, fail := range []error{
		&history.UnavailableError{Source: history.SourceDots, Kind: history.ErrExtensionNotConnected},
		&history.UnavailableError{Source: history.SourceDots, Kind: history.ErrRejected, Detail: `unknown operation "dots.primary"`},
	} {
		stubDotPrimary(t, history.DotPrimary{}, fail)
		_, err := run(t, Root(), "web", "install", "--site", "dots", "--binary", "/opt/tincan/tincan")
		if err == nil || !strings.Contains(err.Error(), "--thread") || !strings.Contains(err.Error(), fail.Error()) || !strings.Contains(err.Error(), "update") {
			t.Fatalf("install without --thread, %v: %v", fail, err)
		}
	}
	if _, err := run(t, Root(), "web", "install", "--site", "grok", "--thread", "0d0d0d0d-1111-7222-8333-000000000001", "--binary", "/opt/tincan/tincan"); err == nil {
		t.Fatal("install --site grok --thread accepted")
	}
	// An explicit --thread never asks the extension.
	stubDotPrimary(t, history.DotPrimary{}, errors.New("asked the extension"))
	out, err := run(t, Root(), "web", "install", "--site", "dots", "--thread", "0d0d0d0d-1111-7222-8333-000000000001", "--binary", "/opt/tincan/tincan")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	def := filepath.Join(home, ".config", "systemd", "user", "tincan-dot-web.service")
	want := "--thread 0d0d0d0d-1111-7222-8333-000000000001"
	if runtime.GOOS == "darwin" {
		def = filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.web.dots.plist")
		want = "<string>--thread</string>\n    <string>0d0d0d0d-1111-7222-8333-000000000001</string>"
	}
	b, err := os.ReadFile(def)
	if err != nil || !strings.Contains(string(b), want) || !strings.Contains(string(b), "dot-web.json") {
		t.Fatalf("definition %s (%v):\n%s", def, err, b)
	}
}

// Without --thread, install takes the thread of the account's primary
// dot from the extension and says which dot it used.
func TestWebInstallDotsFindsThePrimaryDotsThread(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service definition on " + runtime.GOOS)
	}
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PATH", t.TempDir())
	const thread = "0d0d0d0d-1111-7222-8333-00000000000f"
	stubDotPrimary(t, history.DotPrimary{Thread: thread, Name: "Orbit"}, nil)
	out, err := run(t, Root(), "web", "install", "--site", "dots", "--binary", "/opt/tincan/tincan")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, `using dot "Orbit" thread `+thread) {
		t.Fatalf("output does not name the dot:\n%s", out)
	}
	def := filepath.Join(home, ".config", "systemd", "user", "tincan-dot-web.service")
	if runtime.GOOS == "darwin" {
		def = filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.web.dots.plist")
	}
	if b, err := os.ReadFile(def); err != nil || !strings.Contains(string(b), thread) {
		t.Fatalf("definition %s (%v) lacks the thread:\n%s", def, err, b)
	}
}

// stubDotPrimary replaces the extension's primary-dot answer for one test.
func stubDotPrimary(t *testing.T, p history.DotPrimary, err error) {
	t.Helper()
	saved := dotPrimary
	dotPrimary = func(context.Context) (history.DotPrimary, error) { return p, err }
	t.Cleanup(func() { dotPrimary = saved })
}

// --teach is for the dot's web agent only.
func TestWebTeachIsDotsOnly(t *testing.T) {
	t.Setenv("TINCAN_RELAY", "")
	allow := filepath.Join(t.TempDir(), "allow.txt")
	cfg := filepath.Join(t.TempDir(), "chatgpt-web.json")
	_, err := run(t, Root(), "web", "serve", "--site", "chatgpt", "--teach", "--config", cfg, "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), "--teach applies only to --site dots") {
		t.Fatalf("--teach on chatgpt: %v", err)
	}
}

// The relay holds requests to the dot for the owner's approval only when it
// knows the agent's kind is dot-web; web serve --site dots refuses to start
// as an agent of any other kind (an invite without --kind leaves it empty),
// and gets past the check for kind dot-web.
func TestWebServeDotsNeedsDotWebKind(t *testing.T) {
	shortNativeDir(t)
	setHome(t, t.TempDir())
	for _, kind := range []string{"", "codex", "dot-web"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/whoami":
				_ = json.NewEncoder(w).Encode(map[string]string{"name": "dot-web", "kind": kind})
			case "/v1/poll":
				<-r.Context().Done()
			default:
				http.NotFound(w, r)
			}
		}))
		ctx, cancel := context.WithCancel(context.Background())
		cmd := Root()
		var out syncBuffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"web", "serve", "--site", "dots", "--thread", "0d0d0d0d-1111-7222-8333-000000000001", "--config", serveConfig(t, srv.URL, "dot-web"), "--allowlist", filepath.Join(t.TempDir(), "allow.txt")})
		done := make(chan error, 1)
		go func() { done <- cmd.ExecuteContext(ctx) }()
		if kind != "dot-web" {
			select {
			case err := <-done:
				shown := kind
				if shown == "" {
					shown = "none"
				}
				if err == nil || !strings.Contains(err.Error(), `kind on the relay is "`+shown+`"`) || !strings.Contains(err.Error(), "tincan kind dot-web dot-web") {
					t.Fatalf("kind %q: %v\n%s", kind, err, out.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("kind %q: web serve started\n%s", kind, out.String())
			}
		} else {
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(out.String(), "serving dots") && time.Now().Before(deadline) {
				select {
				case err := <-done:
					t.Fatalf("kind dot-web: web serve exited: %v\n%s", err, out.String())
				default:
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !strings.Contains(out.String(), "serving dots") {
				t.Fatalf("kind dot-web: web serve did not start:\n%s", out.String())
			}
		}
		cancel()
		if kind == "dot-web" {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("web serve did not stop")
			}
		}
		srv.Close()
	}
}

func TestWebInstallCustomNameAndConfig(t *testing.T) {
	skipOnWindows(t, skipNoService)
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PATH", t.TempDir())
	cfg := filepath.Join(home, "my configs", "dayward.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"agent":"dayward","relay":"http://relay"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, Root(), "web", "install", "--site", "claude-ai", "--name", "dayward", "--config", cfg, "--binary", "/opt/tincan")
	if err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(home, ".config", "systemd", "user", "tincan-dayward.service")
	if runtime.GOOS == "darwin" {
		def = filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.web.claude-ai.dayward.plist")
	}
	b, err := os.ReadFile(def)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{cfg, "--name", "dayward"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q: %s", want, b)
		}
	}
	if !strings.Contains(out, cfg) {
		t.Fatal(out)
	}
	if _, err := run(t, Root(), "web", "install", "--site", "claude-ai", "--config", cfg, "--binary", "/opt/tincan"); err == nil {
		t.Fatal("installed as wrong name")
	}
}

// A Duet channel has no primary to find, so install needs --thread; a
// channel the extension refuses (shared, archived, not the owner's) is
// never installed, and an unreachable extension only warns.
// web serve refuses a Duet channel the extension refuses (shared with
// someone else) before it serves anything.
func TestWebServeDuetRefusesASharedChannel(t *testing.T) {
	shortNativeDir(t)
	setHome(t, t.TempDir())
	saved := checkWebThread
	checkWebThread = func(context.Context, history.Source, string) error {
		return &history.UnavailableError{Source: history.SourceDuet, Kind: history.ErrRefused, Detail: "the Duet channel #team is shared"}
	}
	t.Cleanup(func() { checkWebThread = saved })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "duet-web", "kind": "duet-web"})
		case "/v1/poll":
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := Root()
	var out syncBuffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"web", "serve", "--site", "duet", "--thread", "chan_0000000000000000chan01", "--config", serveConfig(t, srv.URL, "duet-web"), "--allowlist", filepath.Join(t.TempDir(), "allow.txt")})
	err := cmd.ExecuteContext(ctx)
	if !errors.Is(err, history.ErrRefused) || !strings.Contains(err.Error(), "shared") || strings.Contains(out.String(), "serving duet") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestWebInstallDuetChecksTheChannel(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service definition on " + runtime.GOOS)
	}
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PATH", t.TempDir())
	const channel = "chan_0000000000000000chan01"
	stub := func(err error) {
		saved := checkWebThread
		checkWebThread = func(context.Context, history.Source, string) error { return err }
		t.Cleanup(func() { checkWebThread = saved })
	}
	stub(nil)
	if _, err := run(t, Root(), "web", "install", "--site", "duet", "--binary", "/opt/tincan/tincan"); err == nil || !strings.Contains(err.Error(), "--thread") {
		t.Fatalf("install without --thread: %v", err)
	}
	stub(&history.UnavailableError{Source: history.SourceDuet, Kind: history.ErrRefused, Detail: "the Duet channel #team is shared"})
	if _, err := run(t, Root(), "web", "install", "--site", "duet", "--thread", channel, "--binary", "/opt/tincan/tincan"); err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("install of a refused channel: %v", err)
	}
	stub(&history.UnavailableError{Source: history.SourceDuet, Kind: history.ErrExtensionNotConnected})
	out, err := run(t, Root(), "web", "install", "--site", "duet", "--thread", "https://claude.ai/messages/"+channel, "--binary", "/opt/tincan/tincan")
	if err != nil || !strings.Contains(out, "web serve checks it again") {
		t.Fatalf("install without the extension: %v\n%s", err, out)
	}
	def := filepath.Join(home, ".config", "systemd", "user", "tincan-duet-web.service")
	if runtime.GOOS == "darwin" {
		def = filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.web.duet.plist")
	}
	if b, err := os.ReadFile(def); err != nil || !strings.Contains(string(b), channel) || strings.Contains(string(b), "https://") || !strings.Contains(string(b), "duet-web.json") {
		t.Fatalf("definition %s (%v):\n%s", def, err, b)
	}
}
