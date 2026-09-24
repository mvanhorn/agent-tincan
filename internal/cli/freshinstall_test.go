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

	// A saved relay (an admin device that is also a joined agent) is known.
	useConfig(t, client.Config{Relay: "http://tincan-relay"})
	out, err = run(t, inviteCmd(), "codex", "--socket", sock)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--relay http://tincan-relay\n") {
		t.Fatalf("invite with saved relay output = %q", out)
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

// --listen is checked before the relay creates its database.
func TestRelayRejectsBadListenBeforeCreatingState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	err := runRelay(context.Background(), relayFlags{listen: "127.0.0.1", stateDir: dir, port: 8787})
	if err == nil || !strings.Contains(err.Error(), "--listen must be a tailnet 100.x address") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "relay.db")); !os.IsNotExist(err) {
		t.Fatalf("relay.db was created before --listen was checked: %v", err)
	}
}
