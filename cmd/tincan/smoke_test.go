package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// TestBuiltBinarySmoke builds the release binary and drives it as a teammate
// would, as a separate process against an in-process relay: join, ask, inbox
// and an MCP initialize. It is the acceptance check for the Windows client,
// where no real machine runs it, and it needs no skip on any platform.
func TestBuiltBinarySmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	exe := filepath.Join(t.TempDir(), "tincan")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", exe, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	m := testrelay.New(t, relay.Config{})
	home := t.TempDir()
	env := append(os.Environ(),
		"TINCAN_CONFIG="+filepath.Join(home, "client.json"),
		"TINCAN_RELAY=", "TINCAN_PROXY=",
		"HOME="+home, "USERPROFILE="+home, "AppData="+home, "LOCALAPPDATA="+home,
		"XDG_CONFIG_HOME="+home,
	)
	tincan := func(args ...string) string {
		t.Helper()
		c := exec.CommandContext(ctx, exe, args...)
		c.Env = env
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("tincan %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	// join: the machine has its own tailnet address, as a real one does.
	out := tincan("join", m.Invite(t, "winbot"), "--relay", m.URL("stranger"))
	if !strings.Contains(out, `Joined as "winbot"`) {
		t.Fatalf("join: %s", out)
	}

	// ask: muse answers in-process while tincan waits for the reply.
	muse := m.Client(t, "muse")
	answered := make(chan error, 1)
	go func() {
		in, err := muse.Poll(ctx, 20*time.Second)
		if err == nil && len(in.Requests) == 0 {
			in, err = muse.Poll(ctx, 20*time.Second)
		}
		if err != nil {
			answered <- err
			return
		}
		for _, q := range in.Requests {
			_, err = muse.Reply(ctx, q.ID, "pong from muse", envelope.StatusAnswered)
		}
		answered <- err
	}()
	if out := tincan("ask", "muse", "ping"); !strings.Contains(out, "pong from muse") {
		t.Fatalf("ask: %s", out)
	}
	if err := <-answered; err != nil {
		t.Fatal(err)
	}

	// inbox: a teammate's request reaches the exe.
	if _, err := muse.Send(ctx, "winbot", "please check the build", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	if out := tincan("inbox", "--wait", "5s"); !strings.Contains(out, "please check the build") {
		t.Fatalf("inbox: %s", out)
	}

	// mcp: initialize over stdio, as an agent's app starts it.
	mcp := exec.CommandContext(ctx, exe, "mcp")
	mcp.Env = env
	stdin, err := mcp.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := mcp.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	mcp.Stderr = &stderr
	if err := mcp.Start(); err != nil {
		t.Fatal(err)
	}
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}`
	if _, err := io.WriteString(stdin, initialize+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		t.Fatalf("mcp initialize: %v\n%s", err, stderr.String())
	}
	var reply struct {
		ID     int `json:"id"`
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("mcp reply %q: %v", line, err)
	}
	if reply.ID != 1 || reply.Result.ServerInfo.Name == "" {
		t.Fatalf("mcp reply: %s", line)
	}
	// An app closing the pipe is how tincan mcp stops.
	stdin.Close()
	if err := mcp.Wait(); err != nil {
		t.Fatalf("mcp exit: %v\n%s", err, stderr.String())
	}
}
