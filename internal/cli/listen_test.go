package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// syncBuffer is a log sink safe to write from the presence goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fastListen shortens listen's presence interval, presence cap and
// cooldown, and captures its log.
func fastListen(t *testing.T, every, presenceCap, cooldown time.Duration) *syncBuffer {
	t.Helper()
	oldE, oldC, oldD, oldL := listenPresenceEvery, listenPresenceCap, listenCooldown, listenLog
	log := &syncBuffer{}
	listenPresenceEvery, listenPresenceCap, listenCooldown, listenLog = every, presenceCap, cooldown, log
	t.Cleanup(func() {
		listenPresenceEvery, listenPresenceCap, listenCooldown, listenLog = oldE, oldC, oldD, oldL
	})
	return log
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The #60 bug: while the --exec command ran, listen made no relay call and
// the agent showed offline. It now keeps its presence fresh with peeks
// that claim nothing, so a request queued meanwhile stays queued.
func TestListenKeepsPresenceWhileCommandRuns(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	fastListen(t, 10*time.Millisecond, time.Minute, time.Second)
	m := testrelay.New(t, relay.Config{PollHold: 2 * time.Second})
	muse := m.Client(t, "muse")
	grokbot := m.Client(t, "grokbot")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := grokbot.Send(ctx, "muse", "call Joe's Garage", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	running := filepath.Join(t.TempDir(), "running")
	release := filepath.Join(t.TempDir(), "release")
	done := make(chan error, 1)
	go func() {
		done <- listen(ctx, muse, `touch `+running+`; while [ ! -e `+release+` ]; do sleep 0.01; done`, true)
	}()
	waitForFile(t, running)
	start := time.Now()
	second, err := grokbot.Send(ctx, "muse", "and the dentist", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	agents, err := grokbot.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lastPoll time.Time
	var online bool
	for _, a := range agents {
		if a.Name == "muse" {
			lastPoll, online = a.LastPoll, a.Online
		}
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !lastPoll.After(start) || !online {
		t.Fatalf("muse last poll %s (online %v) is not after the command began (%s): no presence while it ran", lastPoll, online, start)
	}
	for _, id := range []string{first.ID, second.ID} {
		got, err := grokbot.Get(ctx, id, 0)
		if err != nil || got.Status != envelope.StatusQueued {
			t.Fatalf("request %s: %s %v (want still queued)", id, got.Status, err)
		}
	}
}

// fakeListenRelay answers listen's long peek with one waiting request and
// records every presence peek (hold=0). presenceStatus, when set, fails
// the presence peeks with that status. After the first long peek the next
// one calls onSecondPeek and returns nothing.
type fakeListenRelay struct {
	mu             sync.Mutex
	presence       []time.Time
	longPeeks      int
	presenceStatus int
	onSecondPeek   func()
}

func (f *fakeListenRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/poll" || r.URL.Query().Get("peek") != "1" {
		http.Error(w, "unexpected "+r.URL.String(), http.StatusTeapot)
		return
	}
	f.mu.Lock()
	if r.URL.Query().Get("hold") == "0" {
		f.presence = append(f.presence, time.Now())
		status := f.presenceStatus
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"error":"relay down"}`, status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	f.longPeeks++
	n := f.longPeeks
	hook := f.onSecondPeek
	f.mu.Unlock()
	if n > 1 {
		if hook != nil {
			hook()
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"waiting":1,"queued":1}`))
}

func (f *fakeListenRelay) presencePeeks() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.presence...)
}

func newFakeListenRelay(t *testing.T, f *fakeListenRelay) *client.Relay {
	t.Helper()
	return newFakeListenRelayHandler(t, f.ServeHTTP)
}

// newFakeListenRelayHandler is a client of a relay made of h alone.
func newFakeListenRelayHandler(t *testing.T, h http.HandlerFunc) *client.Relay {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	r, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// With --once, presence stops when listen returns: no refresh goroutine
// outlives it.
func TestListenOnceStopsPresence(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	fastListen(t, 10*time.Millisecond, time.Minute, time.Second)
	f := &fakeListenRelay{}
	r := newFakeListenRelay(t, f)
	if err := listen(t.Context(), r, "sleep 0.2", true); err != nil {
		t.Fatal(err)
	}
	if len(f.presencePeeks()) == 0 {
		t.Fatal("no presence peeks while the command ran")
	}
	// A peek already in flight when presence stopped can still reach the
	// server after listen returns; let it land before taking the baseline.
	time.Sleep(50 * time.Millisecond)
	during := len(f.presencePeeks())
	time.Sleep(100 * time.Millisecond)
	if after := len(f.presencePeeks()); after != during {
		t.Fatalf("presence peeks went on after listen returned: %d then %d", during, after)
	}
}

// The cooldown after a nudge keeps presence too: the agent is still
// working on what it was nudged about.
func TestListenKeepsPresenceDuringCooldown(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	fastListen(t, 10*time.Millisecond, time.Minute, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var atSecond int
	f := &fakeListenRelay{}
	f.onSecondPeek = func() {
		atSecond = len(f.presencePeeks())
		cancel()
	}
	r := newFakeListenRelay(t, f)
	if err := listen(ctx, r, "true", false); err != context.Canceled {
		t.Fatalf("listen = %v, want context.Canceled", err)
	}
	if atSecond < 5 {
		t.Fatalf("%d presence peeks during a 200ms cooldown at 10ms, want several", atSecond)
	}
}

// A hung command must fall offline again: presence stops at the cap and
// says so in the log.
func TestListenPresenceStopsAtCap(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	log := fastListen(t, 10*time.Millisecond, 100*time.Millisecond, time.Second)
	f := &fakeListenRelay{}
	r := newFakeListenRelay(t, f)
	start := time.Now()
	if err := listen(t.Context(), r, "sleep 0.6", true); err != nil {
		t.Fatal(err)
	}
	peeks := f.presencePeeks()
	if len(peeks) == 0 {
		t.Fatal("no presence peeks before the cap")
	}
	if last := peeks[len(peeks)-1]; last.Sub(start) > 400*time.Millisecond {
		t.Fatalf("presence peek %s after the command began, past the 100ms cap", last.Sub(start))
	}
	if !strings.Contains(log.String(), "stopped keeping this agent online after 100ms") {
		t.Fatalf("no cap line in the log: %q", log.String())
	}
}

// A relay that fails presence peeks is logged and does not touch the
// command's run.
func TestListenPresenceErrorsAreLoggedOnly(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	log := fastListen(t, 10*time.Millisecond, time.Minute, time.Second)
	f := &fakeListenRelay{presenceStatus: http.StatusBadGateway}
	r := newFakeListenRelay(t, f)
	out := filepath.Join(t.TempDir(), "nudged")
	if err := listen(t.Context(), r, `sleep 0.1; echo "$TINCAN_WAITING" > `+out, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("command saw TINCAN_WAITING=%q", got)
	}
	if !strings.Contains(log.String(), "tincan listen: presence:") || !strings.Contains(log.String(), "relay down") {
		t.Fatalf("presence errors not logged: %q", log.String())
	}
}

func TestListenContinuesAfterPingFailure(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	for _, phase := range []string{"claim", "reply"} {
		for _, status := range []int{409, 503} {
			t.Run(fmt.Sprintf("%s/%d", phase, status), func(t *testing.T) {
				fastListen(t, time.Second, time.Minute, time.Second)
				oldRetry := client.PongRetry
				client.PongRetry = nil
				t.Cleanup(func() { client.PongRetry = oldRetry })
				var peeks, failures atomic.Int32
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/v1/poll":
						if r.URL.Query().Get("hold") == "0" {
							w.WriteHeader(204)
							return
						}
						if peeks.Add(1) == 1 {
							fmt.Fprint(w, `{"waiting":1,"pings":1,"pending":[{"id":"ping","kind":"ping"}]}`)
						} else {
							fmt.Fprint(w, `{"waiting":1,"pending":[{"id":"work","kind":"ask"}]}`)
						}
					case strings.HasSuffix(r.URL.Path, "/"+phase):
						failures.Add(1)
						http.Error(w, `{"error":"ping failed"}`, status)
					default:
						fmt.Fprint(w, `{}`)
					}
				}))
				defer ts.Close()
				relay, err := client.NewRelay(ts.URL, "")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				marker := filepath.Join(t.TempDir(), "nudged")
				if err := listen(ctx, relay, "touch "+marker, true); err != nil {
					t.Fatal(err)
				}
				if failures.Load() != 1 || peeks.Load() < 2 {
					t.Fatalf("failures=%d peeks=%d", failures.Load(), peeks.Load())
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestInboxPingFailurePreservesWork(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/poll":
			fmt.Fprint(w, `{"requests":[{"id":"ping","kind":"ping"},{"id":"work","kind":"ask","body":"real work"}]}`)
		case "/v1/requests/ping/claim":
			http.Error(w, `{"error":"ping failed"}`, http.StatusServiceUnavailable)
		case "/v1/requests/work/claim":
			fmt.Fprint(w, `{"id":"work","kind":"ask","body":"real work"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer ts.Close()
	relay, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, jsonOutput := range []bool{false, true} {
		var out, errOut bytes.Buffer
		check := checkInbox
		if jsonOutput {
			check = checkInboxJSON
		}
		if err := check(t.Context(), relay, 0, &out, &errOut); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "real work") {
			t.Fatalf("output: %s", &out)
		}
	}
	in, _, err := waitForInbox(t.Context(), relay, time.Second, client.RepliesNone)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].ID != "work" {
		t.Fatalf("wait: %+v, %v", in, err)
	}
}

func TestListenDrainsTruncatedPingBatchWithoutNudging(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var answered, peeks atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/poll":
			if r.URL.Query().Get("hold") == "0" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			peeks.Add(1)
			done := int(answered.Load())
			waiting := client.Waiting{Total: 60 - done, Queued: 60 - done, Pings: 60 - done}
			for i := done; i < min(done+relay.MaxPeekPending, 60); i++ {
				waiting.Pending = append(waiting.Pending, envelope.Pending{ID: fmt.Sprintf("ping-%d", i), Kind: envelope.KindPing})
			}
			if err := json.NewEncoder(w).Encode(waiting); err != nil {
				t.Errorf("encode peek: %v", err)
			}
			if done == 60 {
				cancel()
			}
		case strings.HasSuffix(r.URL.Path, "/claim"):
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/reply"):
			answered.Add(1)
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer ts.Close()
	r, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "nudged")
	err = listen(ctx, r, "touch "+marker, true)
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("exec command ran or marker could not be checked: %v", statErr)
	}
	if err != context.Canceled {
		t.Fatalf("listen = %v, want context.Canceled", err)
	}
	if answered.Load() != 60 || peeks.Load() != 3 {
		t.Fatalf("answered=%d peeks=%d, want 60 answers and 3 peeks", answered.Load(), peeks.Load())
	}
}

func TestListenPingFailureDoesNotBlockWaitingWork(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	fastListen(t, time.Second, time.Minute, time.Second)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/poll" {
			waiting := client.Waiting{Total: 51, Queued: 51, Pings: 50}
			for i := range 50 {
				waiting.Pending = append(waiting.Pending, envelope.Pending{ID: fmt.Sprintf("ping-%d", i), Kind: envelope.KindPing})
			}
			if err := json.NewEncoder(w).Encode(waiting); err != nil {
				t.Error(err)
			}
			return
		}
		http.Error(w, `{"error":"ping failed"}`, http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	relay, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "waiting")
	if err := listen(ctx, relay, `printf %s "$TINCAN_WAITING" > `+marker, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "1" {
		t.Fatalf("waiting=%q: %v", got, err)
	}
}

// A pong that keeps failing retries in the background: work that arrives
// meanwhile still runs the command at once, and --once waits for the retry
// before returning instead of dropping it.
func TestListenPongRetryNeitherDelaysWorkNorIsDropped(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	fastListen(t, time.Second, time.Minute, time.Millisecond)
	oldRetry := client.PongRetry
	client.PongRetry = []time.Duration{300 * time.Millisecond}
	t.Cleanup(func() { client.PongRetry = oldRetry })
	var peeks, replies atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/poll":
			if r.URL.Query().Get("hold") == "0" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if peeks.Add(1) == 1 {
				fmt.Fprint(w, `{"waiting":2,"pings":1,"pending":[{"id":"ping","kind":"ping"},{"id":"work","kind":"ask"}]}`)
			} else {
				fmt.Fprint(w, `{"waiting":1,"pending":[{"id":"work","kind":"ask"}]}`)
			}
		case strings.HasSuffix(r.URL.Path, "/reply"):
			if replies.Add(1) == 1 {
				http.Error(w, `{"error":"busy"}`, http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer ts.Close()
	relay, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "nudged")
	start := time.Now()
	nudged := make(chan time.Duration, 1)
	go func() {
		waitForFile(t, marker)
		nudged <- time.Since(start)
	}()
	if err := listen(t.Context(), relay, "touch "+marker, true); err != nil {
		t.Fatal(err)
	}
	if d := <-nudged; d >= 300*time.Millisecond {
		t.Fatalf("command ran after %s, behind the pong retry", d)
	}
	if replies.Load() != 2 {
		t.Fatalf("pong replies = %d, want the retry to finish before listen returned", replies.Load())
	}
}

// Windows has no sh, so a command that runs through it says so and points at
// what does work there, instead of failing on every nudge.
func TestNeedsShellRefusesOnWindows(t *testing.T) {
	err := needsShell("windows", "tincan listen")
	if err == nil {
		t.Fatal("want a refusal on Windows")
	}
	for _, want := range []string{"tincan listen", "Windows", "sh -c", "tincan wait"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal lacks %q: %v", want, err)
		}
	}
	for _, goos := range []string{"linux", "darwin"} {
		if err := needsShell(goos, "tincan listen"); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
	}
}

func TestListenAndRelayWatchRefuseOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("exercises the refusal on Windows; TestNeedsShellRefusesOnWindows covers its text everywhere")
	}
	useConfig(t, client.Config{})
	if _, err := run(t, listenCmd(), "--exec", "echo hi"); err == nil || !strings.Contains(err.Error(), "sh -c") {
		t.Fatalf("listen: %v", err)
	}
	if _, err := run(t, relayWatchCmd(), "--alert-cmd", "echo hi"); err == nil || !strings.Contains(err.Error(), "sh -c") {
		t.Fatalf("relay-watch: %v", err)
	}
}

// The relay needs unix file and socket behavior, so Windows stops at a clear
// message rather than a failure deep in startup.
func TestRelayRefusesOnWindows(t *testing.T) {
	err := relayHostErr("windows")
	if err == nil || !strings.Contains(err.Error(), "Linux and macOS") {
		t.Fatalf("windows: %v", err)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if err := relayHostErr(goos); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
	}
}
