package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// relayHelperEnv names the state dir of a relay the test binary runs as a
// child process, so the shutdown tests can signal a real relay process.
const relayHelperEnv = "TINCAN_TEST_RELAY_STATE"

// relayHelperArgsEnv carries extra relay flags, one per line, to the relay
// TestRelayProcessHelper runs.
const relayHelperArgsEnv = "TINCAN_TEST_RELAY_ARGS"

// loopbackTailnet stands in for tailscaled: every caller is the one admin
// machine "host", which is always online.
type loopbackTailnet struct{ url string }

func (l loopbackTailnet) WhoIs(context.Context, string) (identity.Node, error) {
	return identity.Node{ID: "nHOST", Name: "host", User: "owner@example.com"}, nil
}

func (loopbackTailnet) NodeOnline(context.Context, string) (bool, bool, error) {
	return true, true, nil
}

func (l loopbackTailnet) SelfURLs(context.Context, int) []string { return []string{l.url} }

// SelfNames is the loopback address the relay serves on: the only name a
// browser-hardened test relay accepts in Host.
func (loopbackTailnet) SelfNames(context.Context) []string { return []string{"127.0.0.1"} }

// TestRelayProcessHelper is the relay process the shutdown tests start. It
// runs only when the parent test sets relayHelperEnv. It serves the real
// relay command on loopback and writes its URL to the state dir.
func TestRelayProcessHelper(t *testing.T) {
	dir := os.Getenv(relayHelperEnv)
	if dir == "" {
		t.Skip("run by the relay shutdown tests")
	}
	relayDrainTimeout = relayDrainTimeoutForTest
	openRelayNet = func(context.Context, relayFlags, string) (net.Listener, relayResolver, func(), error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, nil, err
		}
		url := "http://" + ln.Addr().String()
		if err := os.WriteFile(filepath.Join(dir, "url"), []byte(url), 0o600); err != nil {
			return nil, nil, nil, err
		}
		if os.Getenv(relayHelperTLSEnv) != "" {
			return ln, loopbackTLSTailnet{loopbackTailnet{url}, dir}, func() {}, nil
		}
		return ln, loopbackTailnet{url}, func() {}, nil
	}
	cmd := relayCmd()
	args := []string{"--state-dir", dir, "--admin", "host"}
	if extra := os.Getenv(relayHelperArgsEnv); extra != "" {
		args = append(args, strings.Split(extra, "\n")...)
	}
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "relay:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// lockedBuffer collects a child process's output while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// relayProcess is a relay running as a child process.
type relayProcess struct {
	cmd  *exec.Cmd
	dir  string
	url  string
	out  *lockedBuffer
	done chan error
}

// relayStateDir makes a relay state dir. The admin socket lives in it, and
// unix socket paths are length-limited, so it sits directly under /tmp.
func relayStateDir(t *testing.T) string {
	t.Helper()
	skipOnWindows(t, skipRelayHost)
	dir, err := os.MkdirTemp("/tmp", "tincan-relay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func startRelayProcess(t *testing.T) *relayProcess {
	t.Helper()
	return startRelayProcessIn(t, relayStateDir(t))
}

// startRelayProcessIn starts a relay on the state dir dir, which may hold
// the state of an earlier relay, with any extra relay flags.
func startRelayProcessIn(t *testing.T, dir string, extra ...string) *relayProcess {
	t.Helper()
	p, err := tryRelayProcessIn(t, dir, extra...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// tryRelayProcessIn is startRelayProcessIn returning the failure, so a
// caller can retry a relay that exited at startup.
func tryRelayProcessIn(t *testing.T, dir string, extra ...string) (*relayProcess, error) {
	t.Helper()
	skipOnWindows(t, skipRelayHost)
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	os.Remove(filepath.Join(dir, "url"))
	p := &relayProcess{dir: dir, out: &lockedBuffer{}, done: make(chan error, 1)}
	p.cmd = exec.Command(os.Args[0], "-test.run=^TestRelayProcessHelper$")
	p.cmd.Env = append(os.Environ(), relayHelperEnv+"="+dir, relayHelperArgsEnv+"="+strings.Join(extra, "\n"))
	p.cmd.Stdout = p.out
	p.cmd.Stderr = p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		p.cmd.Process.Kill()
		<-p.done
	})
	deadline := time.Now().Add(30 * time.Second)
	for p.url == "" || p.get("/v1/hello") != nil {
		select {
		case err := <-p.done:
			p.done <- err // for cleanup
			return nil, fmt.Errorf("relay exited at startup (%v):\n%s", err, p.out)
		default:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("relay did not start:\n%s", p.out)
		}
		time.Sleep(20 * time.Millisecond)
		if raw, err := os.ReadFile(filepath.Join(dir, "url")); err == nil {
			p.url = string(raw)
		}
	}
	return p, nil
}

func (p *relayProcess) get(path string) error {
	resp, err := http.Get(p.url + path)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// agent joins name and returns its client.
func (p *relayProcess) agent(t *testing.T, name string) *client.Relay {
	t.Helper()
	admin, err := client.NewRelayFor(client.Config{Relay: p.url})
	if err != nil {
		t.Fatal(err)
	}
	code, err := admin.Invite(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := client.NewRelayFor(client.Config{Relay: p.url, Agent: name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(t.Context(), code); err != nil {
		t.Fatal(err)
	}
	return r
}

// waitExit waits up to limit for the relay to exit and returns how long it
// took and its exit error.
func (p *relayProcess) waitExit(t *testing.T, since time.Time, limit time.Duration) (time.Duration, error) {
	t.Helper()
	select {
	case err := <-p.done:
		p.done <- err // for cleanup
		return time.Since(since), err
	case <-time.After(limit):
		t.Fatalf("relay still running %s after the signal:\n%s", limit, p.out)
		return 0, nil
	}
}

// A relay told to stop ends its held long polls and waits at once with their
// normal "nothing yet" answers, exits promptly, and leaves a store that opens
// cleanly. SIGINT and SIGTERM behave the same.
func TestRelayProcessExitsPromptlyOnSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a relay process")
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			p := startRelayProcess(t)
			asker := p.agent(t, "asker")
			p.agent(t, "worker")
			req, err := asker.Send(t.Context(), "worker", "are you there?", envelope.KindAsk, "", false)
			if err != nil {
				t.Fatal(err)
			}

			var wg sync.WaitGroup
			var pollErr, getErr error
			var inbox client.Inbox
			var res client.Result
			held := time.Now()
			wg.Go(func() { inbox, pollErr = asker.Poll(context.Background(), client.DefaultPollHold) })
			wg.Go(func() { res, getErr = asker.Get(context.Background(), req.ID, client.DefaultPollHold) })
			waitOnline(t, asker, "asker")
			time.Sleep(200 * time.Millisecond) // let the wait reach its hold too

			sent := time.Now()
			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			took, err := p.waitExit(t, sent, 10*time.Second)
			if err != nil {
				t.Fatalf("relay exit: %v\n%s", err, p.out)
			}
			wg.Wait()
			t.Logf("%s: relay exited %s after the signal; held calls returned after %s", sig, took.Round(time.Millisecond), time.Since(held).Round(time.Millisecond))
			if took > 5*time.Second {
				t.Fatalf("relay took %s to exit with only held calls open:\n%s", took, p.out)
			}
			if pollErr != nil || len(inbox.Requests) != 0 {
				t.Fatalf("held poll: %d requests, err %v; want a normal empty answer", len(inbox.Requests), pollErr)
			}
			if getErr != nil || res.Request.ID != req.ID || res.Done() {
				t.Fatalf("held wait: request %q status %q, err %v; want the pending request", res.Request.ID, res.Status, getErr)
			}
			if !strings.Contains(p.out.String(), "tincan relay stopped") {
				t.Fatalf("relay did not finish its shutdown steps:\n%s", p.out)
			}

			st, err := store.Open(filepath.Join(p.dir, "relay.db"))
			if err != nil {
				t.Fatalf("reopen store after shutdown: %v", err)
			}
			defer st.Close()
			if _, err := st.VerifyAudit(t.Context()); err != nil {
				t.Fatalf("audit chain after shutdown: %v", err)
			}
		})
	}
}

// A client that never finishes its request cannot hold the relay past the
// drain bound, and a second signal ends the drain at once.
func TestRelayProcessDrainIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a relay process")
	}
	for _, second := range []bool{false, true} {
		t.Run(fmt.Sprintf("second_signal=%v", second), func(t *testing.T) {
			p := startRelayProcess(t)
			stalled := stallRequest(t, p.url)
			defer stalled.Close()

			sent := time.Now()
			if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if second {
				time.Sleep(300 * time.Millisecond)
				if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				took, err := p.waitExit(t, sent, 10*time.Second)
				t.Logf("second SIGTERM: relay exited %s after the first (%v)", took.Round(time.Millisecond), err)
				if took > time.Second+300*time.Millisecond {
					t.Fatalf("a second signal took %s to stop the relay:\n%s", took, p.out)
				}
				if err == nil {
					t.Fatalf("a forced exit reported success:\n%s", p.out)
				}
				return
			}
			took, err := p.waitExit(t, sent, 10*time.Second)
			t.Logf("stalled client: relay exited %s after SIGTERM (%v)", took.Round(time.Millisecond), err)
			if err != nil {
				t.Fatalf("relay exit: %v\n%s", err, p.out)
			}
			if took < relayDrainTimeoutForTest || took > relayDrainTimeoutForTest+3*time.Second {
				t.Fatalf("relay exited %s after SIGTERM with a stalled client, want about the %s drain bound:\n%s", took, relayDrainTimeoutForTest, p.out)
			}
		})
	}
}

// relayDrainTimeoutForTest is the drain bound TestRelayProcessHelper sets.
const relayDrainTimeoutForTest = 2 * time.Second

// stallRequest opens a request to the relay whose body never arrives, so the
// relay has a connection that is neither idle nor done.
func stallRequest(t *testing.T, url string) io.Closer {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST /v1/send HTTP/1.1\r\nHost: relay\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	time.Sleep(200 * time.Millisecond) // let the relay start reading the body
	return conn
}

// waitOnline waits until the relay reports name as polling.
func waitOnline(t *testing.T, r *client.Relay, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		agents, err := r.Agents(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range agents {
			if a.Name == name && a.Online {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never showed as polling", name)
}

// A request sent to a webhook agent just before the relay stops still wakes
// that agent: the restarted relay schedules the nudge the old process never
// sent.
func TestRelayProcessRestartWakesForQueuedRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a relay process")
	}
	var mu sync.Mutex
	var bodies []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer hook.Close()
	woken := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(bodies)
	}

	dir := relayStateDir(t)
	cfg := `{"worker": {"method": "webhook", "url": "` + hook.URL + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "wake.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startRelayProcessIn(t, dir)
	asker := p.agent(t, "asker")
	p.agent(t, "worker")
	if _, err := asker.Send(t.Context(), "worker", "are you there?", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	// Stop inside the wake debounce, before the nudge goes out.
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if _, err := p.waitExit(t, time.Now(), 10*time.Second); err != nil {
		t.Fatalf("relay exit: %v\n%s", err, p.out)
	}
	if got := woken(); len(got) != 0 {
		t.Fatalf("worker woken before the restart: %q", got)
	}

	p = startRelayProcessIn(t, dir)
	deadline := time.Now().Add(15 * time.Second)
	for len(woken()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the restarted relay never woke worker for its queued request:\n%s", p.out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := woken(); len(got) != 1 || !strings.Contains(got[0], "1 request") {
		t.Fatalf("wakes after the restart: %q", got)
	}
}
