package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// runStdio runs the real command tree with the process's stdout and stderr
// swapped for pipes, the way a shell sees it, and returns what each got.
func runStdio(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	outR, outW, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	errR, errW, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	outc, errc := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outc <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errc <- string(b) }()
	func() {
		defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
		root := Root()
		root.SetArgs(args)
		err = root.ExecuteContext(context.Background())
	}()
	outW.Close()
	errW.Close()
	return <-outc, <-errc, err
}

// A customer runs `tincan onboard > kit.txt` and `v=$(tincan version)`:
// command output belongs on stdout, not stderr.
func TestCommandOutputGoesToStdout(t *testing.T) {
	useConfig(t, client.Config{})
	out, errOut, err := runStdio(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != Version || errOut != "" {
		t.Fatalf("version: stdout %q, stderr %q", out, errOut)
	}
	out, errOut, err = runStdio(t, "onboard", "--offline")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "tincan") || errOut != "" {
		t.Fatalf("onboard --offline: stdout %d bytes, stderr %q", len(out), errOut)
	}
}

// The invite output fills in the relay URL it used, so the join line can be
// pasted as is. Over the admin socket with no saved relay it cannot know the
// URL and keeps the placeholder.
func TestInvitePrintsRelayURL(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	url := m.URL("admin")
	out, err := run(t, inviteCmd(), "codex", "--relay", url)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--relay "+url+"\n") || strings.Contains(out, "<relay URL>") {
		t.Fatalf("invite output = %q", out)
	}

	sock := adminSocket(t, m)
	out, err = run(t, inviteCmd(), "codex", "--socket", sock)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--relay <relay URL>") {
		t.Fatalf("invite over socket output = %q", out)
	}

	// Over the socket a saved relay is not trusted: it may name a
	// different relay than the one the socket serves, so the placeholder
	// stays unless --relay says which.
	useConfig(t, client.Config{Relay: "http://other-relay"})
	out, err = run(t, inviteCmd(), "codex", "--socket", sock)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--relay <relay URL>") || strings.Contains(out, "other-relay") {
		t.Fatalf("invite over socket with a saved relay output = %q", out)
	}
	out, err = run(t, inviteCmd(), "codex", "--socket", sock, "--relay", "http://tincan-relay")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--relay http://tincan-relay\n") {
		t.Fatalf("invite over socket with --relay output = %q", out)
	}

	// Without --socket the invite went to the saved relay, so that one is
	// printed.
	useConfig(t, client.Config{Relay: url})
	out, err = run(t, inviteCmd(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--relay "+url+"\n") {
		t.Fatalf("invite through the saved relay output = %q", out)
	}
}

// With no saved relay and neither --relay nor --socket, tincan agents says
// how to point it at the relay instead of failing somewhere deeper.
func TestAgentsWithoutRelayExplains(t *testing.T) {
	useConfig(t, client.Config{})
	_, err := run(t, Root(), "agents")
	if err == nil || !strings.Contains(err.Error(), "no relay configured: on an admin device pass --relay") {
		t.Fatalf("agents with no relay: %v", err)
	}
}

// An admin device never joins, so it has no saved config. tincan agents
// takes --relay and --socket like the other admin commands.
func TestAgentsOnAdminDeviceWithoutJoin(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	out, err := run(t, Root(), "agents", "--relay", m.URL("admin"))
	if err != nil {
		t.Fatalf("agents --relay: %v", err)
	}
	for _, name := range []string{"grokbot", "instinct", "muse"} {
		if !strings.Contains(out, name) {
			t.Fatalf("agents --relay output = %q", out)
		}
	}
	out, err = run(t, Root(), "agents", "--socket", adminSocket(t, m))
	if err != nil || !strings.Contains(out, "muse") {
		t.Fatalf("agents --socket = %q, %v", out, err)
	}
}

// adminSocket serves the mesh's admin handler on a unix socket, as the relay
// does in its state dir. The directory is short so the path fits the unix
// socket limit.
func adminSocket(t *testing.T, m *testrelay.Mesh) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: m.Server.AdminHandler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

// A fresh admin who never joined gets onboard's own actionable error, not a
// rebuilt-machine rejoin hint.
func TestOnboardWithNoConfigHasNoRejoinHint(t *testing.T) {
	useConfig(t, client.Config{})
	_, err := run(t, Root(), "onboard")
	if err == nil || !strings.Contains(err.Error(), "--offline") {
		t.Fatalf("onboard with no config: %v", err)
	}
	if strings.Contains(err.Error(), "rejoin") {
		t.Fatalf("fresh admin should not be told to rejoin: %v", err)
	}
}

// --listen is fully parsed before the relay creates its state dir or
// database.
func TestRelayRejectsBadListenBeforeCreatingState(t *testing.T) {
	for _, listen := range []string{"127.0.0.1", "100.not-an-ip", "100.64.1", "100.64.1.2.3", "100.64.1.2:notaport", "100.64.1.2:0", "100.64.1.2:70000", "[::1]:80", "::1", "tincan-relay"} {
		dir := filepath.Join(t.TempDir(), "state")
		err := runRelay(context.Background(), relayFlags{listen: listen, stateDir: dir, port: 8787})
		if err == nil || !strings.Contains(err.Error(), "--listen must be a tailnet 100.x address") {
			t.Fatalf("%q: err = %v", listen, err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%q: the state dir was created before --listen was checked: %v", listen, err)
		}
	}
}

func TestListenAddr(t *testing.T) {
	for in, want := range map[string]string{
		"100.64.1.2":      "100.64.1.2:8787",
		" 100.64.1.2 ":    "100.64.1.2:8787",
		"100.101.102.103": "100.101.102.103:8787",
		"100.64.1.2:9000": "100.64.1.2:9000",
	} {
		got, err := listenAddr(in, 8787)
		if err != nil || got != want {
			t.Errorf("listenAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
