package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const testMessagingToken = "tok-secret-0123456789"

// claudeSockFixture is a fake Claude Code session: a short socket path and a
// config dir holding the session registry for parent pid 4242.
type claudeSockFixture struct {
	socket, configDir string
	ppid              int
}

func newClaudeSockFixture(t *testing.T) claudeSockFixture {
	t.Helper()
	skipOnWindows(t, skipSocketWake)
	// macOS caps unix socket paths near 104 bytes, so the socket lives in a
	// short directory under /tmp rather than t.TempDir.
	dir, err := os.MkdirTemp("/tmp", "tccs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return claudeSockFixture{socket: filepath.Join(dir, "s.sock"), configDir: filepath.Join(dir, "cfg"), ppid: 4242}
}

func (f claudeSockFixture) writeRegistry(t *testing.T, pid int, socket string) {
	t.Helper()
	sessions := filepath.Join(f.configDir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"pid": pid, "sessionId": "sess-1", "status": "idle",
		"procStart": "Sat Oct 17 04:55:23 2026", "messagingSocketPath": socket,
	})
	if err := os.WriteFile(filepath.Join(sessions, "4242.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f claudeSockFixture) getenv(token string) func(string) string {
	return func(k string) string {
		switch k {
		case "CLAUDE_CODE_MESSAGING_SOCKET":
			return f.socket
		case "CLAUDE_CODE_MESSAGING_TOKEN":
			return token
		case "CLAUDE_CONFIG_DIR":
			return f.configDir
		}
		return ""
	}
}

// listen accepts one connection and sends back every line it reads.
func (f claudeSockFixture) listen(t *testing.T) (net.Listener, <-chan []string) {
	t.Helper()
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var lines []string
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		got <- lines
	}()
	return ln, got
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestClaudeSocketPushWritesAuthThenUser(t *testing.T) {
	f := newClaudeSockFixture(t)
	f.writeRegistry(t, f.ppid, f.socket)
	_, got := f.listen(t)
	p, err := newClaudeSocketPusher(f.getenv(testMessagingToken), f.ppid)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !closed(p.Ready()) {
		t.Fatal("Ready not closed after verification passed")
	}
	req := envelope.Request{From: "alice", Body: "please rotate the staging keys"}
	content := channelNotice(1, []string{req.From})
	if err := p.Push(context.Background(), content, map[string]string{"count": "1"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	var lines []string
	select {
	case lines = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("listener got nothing")
	}
	if len(lines) != 2 {
		t.Fatalf("got %d frames, want 2: %q", len(lines), lines)
	}
	var auth struct{ Type, Token string }
	if err := json.Unmarshal([]byte(lines[0]), &auth); err != nil || auth.Type != "auth" || auth.Token != testMessagingToken {
		t.Fatalf("auth frame = %s (%v)", lines[0], err)
	}
	var user struct {
		Type    string
		Message struct{ Role, Content string }
	}
	if err := json.Unmarshal([]byte(lines[1]), &user); err != nil || user.Type != "user" || user.Message.Role != "user" {
		t.Fatalf("user frame = %s (%v)", lines[1], err)
	}
	if user.Message.Content != content {
		t.Fatalf("content = %q, want %q", user.Message.Content, content)
	}
	if strings.Contains(lines[1], req.Body) {
		t.Fatalf("user frame carries the request body: %s", lines[1])
	}
}

func TestClaudeSocketPushOmitsAuthWithoutToken(t *testing.T) {
	f := newClaudeSockFixture(t)
	f.writeRegistry(t, f.ppid, f.socket)
	_, got := f.listen(t)
	p, err := newClaudeSocketPusher(f.getenv(""), f.ppid)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := p.Push(context.Background(), channelNotice(2, nil), nil); err != nil {
		t.Fatalf("push: %v", err)
	}
	lines := <-got
	if len(lines) != 1 || !strings.Contains(lines[0], `"type":"user"`) {
		t.Fatalf("frames = %q, want one user frame", lines)
	}
}

func TestClaudeSocketVerifyRefusesMismatch(t *testing.T) {
	cases := map[string]func(f claudeSockFixture, t *testing.T){
		"no registry file": func(claudeSockFixture, *testing.T) {},
		"other socket path": func(f claudeSockFixture, t *testing.T) {
			f.writeRegistry(t, f.ppid, f.socket+".other")
		},
		"other pid": func(f claudeSockFixture, t *testing.T) {
			f.writeRegistry(t, f.ppid+1, f.socket)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newClaudeSockFixture(t)
			setup(f, t)
			p, err := newClaudeSocketPusher(f.getenv(testMessagingToken), f.ppid)
			if err == nil {
				t.Fatal("verification passed, want refusal")
			}
			if strings.Contains(err.Error(), testMessagingToken) {
				t.Fatalf("error leaks the token: %v", err)
			}
			if closed(p.Ready()) {
				t.Fatal("Ready closed although verification failed")
			}
		})
	}
}

func TestClaudeSocketVerifyNeedsEnv(t *testing.T) {
	f := newClaudeSockFixture(t)
	f.writeRegistry(t, f.ppid, f.socket)
	p, err := newClaudeSocketPusher(func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return f.configDir
		}
		return ""
	}, f.ppid)
	if err == nil || closed(p.Ready()) {
		t.Fatalf("verification without a socket env var: err=%v", err)
	}
}

func TestClaudeSocketPushErrorNamesPathNotToken(t *testing.T) {
	f := newClaudeSockFixture(t)
	f.writeRegistry(t, f.ppid, f.socket)
	ln, _ := f.listen(t)
	p, err := newClaudeSocketPusher(f.getenv(testMessagingToken), f.ppid)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	ln.Close()
	err = p.Push(context.Background(), channelNotice(1, nil), nil)
	if err == nil {
		t.Fatal("push to a closed listener succeeded")
	}
	if !strings.Contains(err.Error(), f.socket) {
		t.Fatalf("error %q does not name the socket path", err)
	}
	if strings.Contains(err.Error(), testMessagingToken) {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestClaudeSessionStatusRereadsRegistry(t *testing.T) {
	f := newClaudeSockFixture(t)
	f.writeRegistry(t, f.ppid, f.socket)
	p, err := newClaudeSocketPusher(f.getenv(testMessagingToken), f.ppid)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	s, err := p.session()
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "idle" || s.SessionID != "sess-1" || s.ProcStart != "Sat Oct 17 04:55:23 2026" {
		t.Fatalf("session = %+v", s)
	}
	path := filepath.Join(f.configDir, "sessions", "4242.json")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), `"idle"`, `"busy"`, 1)), 0o600)
	if s, err = p.session(); err != nil || s.Status != "busy" {
		t.Fatalf("after change: %+v, %v", s, err)
	}
}

// fakeHostReporter records host status reports and can play an older relay
// or one that fails with err.
type fakeHostReporter struct {
	mu          sync.Mutex
	sent        []bool
	unsupported bool
	err         error
}

func (f *fakeHostReporter) ReportHostStatus(_ context.Context, busy bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, busy)
	if f.unsupported {
		return client.ErrHostStatusUnsupported
	}
	return f.err
}

// reports is a copy of the reports sent so far.
func (f *fakeHostReporter) reports() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// The forwarder reports the session's state at once when it changes, and
// again every keepalive while it stays the same, so a long turn or a relay
// restart does not lapse to idle on the roster. An unreadable registry
// sends nothing.
func TestHostStatusForwarder(t *testing.T) {
	rep := &fakeHostReporter{}
	busy, ok := false, true
	f := &hostStatusForwarder{r: rep, busy: func() (bool, bool) { return busy, ok }}
	now := time.Unix(1_790_000_000, 0)
	f.check(t.Context(), now)
	busy = true
	f.check(t.Context(), now.Add(5*time.Second))
	f.check(t.Context(), now.Add(10*time.Second))
	if want := []bool{false, true}; !slices.Equal(rep.sent, want) {
		t.Fatalf("on change: sent %v, want %v", rep.sent, want)
	}
	f.check(t.Context(), now.Add(5*time.Second+sessionKeepalive))
	if want := []bool{false, true, true}; !slices.Equal(rep.sent, want) {
		t.Fatalf("after a keepalive: sent %v, want %v", rep.sent, want)
	}
	ok = false
	f.check(t.Context(), now.Add(time.Hour))
	if len(rep.sent) != 3 {
		t.Fatalf("unreadable registry sent %v", rep.sent)
	}
}

// An older relay without the route turns the forwarder off quietly.
func TestHostStatusForwarderOlderRelay(t *testing.T) {
	rep := &fakeHostReporter{unsupported: true}
	busy := false
	f := &hostStatusForwarder{r: rep, busy: func() (bool, bool) { return busy, true }}
	now := time.Unix(1_790_000_000, 0)
	f.check(t.Context(), now)
	busy = true
	f.check(t.Context(), now.Add(time.Second))
	f.check(t.Context(), now.Add(time.Hour))
	if len(rep.sent) != 1 {
		t.Fatalf("kept reporting to an older relay: %v", rep.sent)
	}
}

// A relay that fails to take a report is retried with a growing backoff,
// not every check, and the failure is logged once per streak.
func TestHostStatusForwarderBacksOff(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	rep := &fakeHostReporter{err: errors.New("relay unreachable")}
	busy := true
	f := &hostStatusForwarder{r: rep, busy: func() (bool, bool) { return busy, true }}
	now := time.Unix(1_790_000_000, 0)
	for i := 0; i <= 60; i += 5 {
		f.check(t.Context(), now.Add(time.Duration(i)*time.Second))
	}
	if n := len(rep.reports()); n != 3 {
		t.Fatalf("sent %d reports in a minute of failures, want 3: %v", n, rep.reports())
	}
	if n := strings.Count(logs.String(), "host status"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, logs.String())
	}
	rep.err = nil
	f.check(t.Context(), now.Add(70*time.Second))
	if n := len(rep.reports()); n != 4 {
		t.Fatalf("no retry once the backoff passed: %v", rep.reports())
	}
	rep.err = errors.New("relay unreachable")
	busy = false
	f.check(t.Context(), now.Add(75*time.Second))
	if n := strings.Count(logs.String(), "host status"); n != 2 {
		t.Fatalf("a new failure streak logged %d times in all, want 2:\n%s", n, logs.String())
	}
}

// When the server stops while it last reported busy, it reports not busy
// once on the way out, so the roster does not show it busy for
// SessionGrace after the window closed.
func TestStartHostStatusClearsBusyOnStop(t *testing.T) {
	rep := &fakeHostReporter{}
	f := &hostStatusForwarder{r: rep, busy: func() (bool, bool) { return true, true }}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		startHostStatus(ctx, f)
		close(done)
	}()
	for deadline := time.Now().Add(5 * time.Second); len(rep.reports()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("no first report")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if got, want := rep.reports(), []bool{true, false}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
}

// The Claude Code forwarder reads busy from the parent session's registry
// file, and only when the file names the parent.
func TestClaudeHostStatusReadsRegistry(t *testing.T) {
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return dir
		}
		return ""
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(pid int, status string) {
		b, _ := json.Marshal(map[string]any{"pid": pid, "status": status})
		if err := os.WriteFile(filepath.Join(dir, "sessions", "4242.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := newClaudeHostStatus(&fakeHostReporter{}, getenv, 4242)
	if _, ok := f.busy(); ok {
		t.Fatal("no registry file: want not ok")
	}
	write(4242, "busy")
	if busy, ok := f.busy(); !busy || !ok {
		t.Fatalf("busy registry: %v %v", busy, ok)
	}
	write(4242, "idle")
	if busy, ok := f.busy(); busy || !ok {
		t.Fatalf("idle registry: %v %v", busy, ok)
	}
	write(99, "busy")
	if _, ok := f.busy(); ok {
		t.Fatal("registry for another pid: want not ok")
	}
}
