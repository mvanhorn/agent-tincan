package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	fastListen(t, 10*time.Millisecond, time.Minute, time.Second)
	m := testrelay.New(t, relay.Config{PollHold: 2 * time.Second})
	muse := m.Client(t, "muse")
	grokbot := m.Client(t, "grokbot")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := grokbot.Send(ctx, "muse", "call Joe's Garage", envelope.KindAsk, "")
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
	second, err := grokbot.Send(ctx, "muse", "and the dentist", envelope.KindAsk, "")
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
	ts := httptest.NewServer(f)
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
