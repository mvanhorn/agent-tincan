package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// fakeWindow is one Claude Code window to pushSocket: it records pushes,
// and its idle state and write failures are set by the test.
type fakeWindow struct {
	*recordPusher
	busy atomic.Bool
	fail atomic.Bool
}

func newFakeWindow() *fakeWindow { return &fakeWindow{recordPusher: newRecordPusher()} }

func (w *fakeWindow) idle() bool { return !w.busy.Load() }

func (w *fakeWindow) Push(ctx context.Context, content string, meta map[string]string) error {
	if w.fail.Load() {
		return errors.New("socket closed")
	}
	return w.recordPusher.Push(ctx, content, meta)
}

func (w *fakeWindow) count() int { return len(w.pushes()) }

// startSocket runs pushSocket for one window until the test ends.
func startSocket(t *testing.T, r *client.Relay, w socketTarget, shared socketShared) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pushSocket(ctx, r, w, shared, map[string]bool{}) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

// museWindow is a muse client with its own session, as one window's MCP
// server has.
func museWindow(t *testing.T, m *testrelay.Mesh, session string) *client.Relay {
	r := m.Client(t, "muse")
	r.SetSession(session)
	return r
}

func totalPushes(ws ...*fakeWindow) int {
	n := 0
	for _, w := range ws {
		n += w.count()
	}
	return n
}

func settle() { time.Sleep(300 * time.Millisecond) }

// One teammate request starts one turn on the machine, however many
// windows are idle.
func TestSocketWakeOneWindowPerRequest(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, 50*time.Millisecond)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	ws := []*fakeWindow{newFakeWindow(), newFakeWindow(), newFakeWindow()}
	for i, w := range ws {
		startSocket(t, museWindow(t, m, "mcp-"+string(rune('a'+i))), w, shared)
	}
	sent, _ := m.Client(t, "grokbot").Send(t.Context(), "muse", "call the dentist", envelope.KindAsk, "", false)
	settle()
	if n := totalPushes(ws...); n != 1 {
		t.Fatalf("pushes across windows = %d, want 1", n)
	}
	for _, w := range ws {
		for _, p := range w.pushes() {
			if p["request_ids"] != sent.ID {
				t.Fatalf("push = %v, want request %s", p, sent.ID)
			}
		}
	}
}

// A reply to an ask a window sent goes to that window only.
func TestSocketWakeOwnReplyToAskingWindow(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, 50*time.Millisecond)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	a, b := newFakeWindow(), newFakeWindow()
	ra, rb := museWindow(t, m, "mcp-a"), museWindow(t, m, "mcp-b")
	startSocket(t, ra, a, shared)
	startSocket(t, rb, b, shared)
	res, err := rb.Ask(t.Context(), "grokbot", "is the build green?", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	grok := m.Client(t, "grokbot")
	in, _ := grok.Poll(t.Context(), 0)
	if len(in.Requests) != 1 {
		t.Fatalf("grokbot inbox = %+v", in)
	}
	if _, err := grok.Reply(t.Context(), in.Requests[0].ID, "green", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	settle()
	if a.count() != 0 || b.count() != 1 {
		t.Fatalf("pushes a=%d b=%d, want 0 and 1", a.count(), b.count())
	}
	if got := b.pushes()[0]["request_ids"]; got != res.Request.ID {
		t.Fatalf("b pushed %q, want %s", got, res.Request.ID)
	}
}

// A reply to a request sent with no session is shown to every window, and
// still wakes only one.
func TestSocketWakeSessionlessReplyWakesOne(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, 50*time.Millisecond)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	if _, err := m.Client(t, "muse").Ask(t.Context(), "grokbot", "is the build green?", "", 0, false); err != nil {
		t.Fatal(err)
	}
	grok := m.Client(t, "grokbot")
	in, _ := grok.Poll(t.Context(), 0)
	if _, err := grok.Reply(t.Context(), in.Requests[0].ID, "green", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	ws := []*fakeWindow{newFakeWindow(), newFakeWindow(), newFakeWindow()}
	for i, w := range ws {
		startSocket(t, museWindow(t, m, "mcp-"+string(rune('a'+i))), w, shared)
	}
	settle()
	if n := totalPushes(ws...); n != 1 {
		t.Fatalf("pushes across windows = %d, want 1", n)
	}
}

// A busy window never holds a request back from an idle one.
func TestSocketWakeBusyWindowDoesNotBlockIdle(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, 50*time.Millisecond)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	busy, idle := newFakeWindow(), newFakeWindow()
	busy.busy.Store(true)
	startSocket(t, museWindow(t, m, "mcp-a"), busy, shared)
	startSocket(t, museWindow(t, m, "mcp-b"), idle, shared)
	m.Client(t, "grokbot").Send(t.Context(), "muse", "call the dentist", envelope.KindAsk, "", false)
	settle()
	if busy.count() != 0 || idle.count() != 1 {
		t.Fatalf("pushes busy=%d idle=%d, want 0 and 1", busy.count(), idle.count())
	}
}

// A request is delivered once: the window that pushed it may go away, and
// nobody announces it again, even after reannounceAfter.
func TestSocketWakeDeliversOnce(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, 50*time.Millisecond)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	first := newFakeWindow()
	stop := startSocket(t, museWindow(t, m, "mcp-a"), first, shared)
	m.Client(t, "grokbot").Send(t.Context(), "muse", "call the dentist", envelope.KindAsk, "", false)
	settle()
	if first.count() != 1 {
		t.Fatalf("first window pushes = %d, want 1", first.count())
	}
	stop()
	second := newFakeWindow()
	startSocket(t, museWindow(t, m, "mcp-b"), second, shared)
	settle()
	if second.count() != 0 || first.count() != 1 {
		t.Fatalf("pushes after the first window left: first=%d second=%d", first.count(), second.count())
	}
}

// While another window holds the lock nothing is pushed, not even a reply
// to this window's own ask: an older relay shows that reply to every
// window, so the record has to say who delivered it. Once the lock frees,
// the window's own reply goes first, and the request waits for the next
// recheck rather than landing in a session that just started a turn.
func TestSocketWakeOwnReplyWithoutLock(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, 50*time.Millisecond)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	release, ok, err := shared.tryLock()
	if err != nil || !ok {
		t.Fatalf("test lock: ok=%v err=%v", ok, err)
	}
	rb := museWindow(t, m, "mcp-b")
	if _, err := rb.Ask(t.Context(), "grokbot", "is the build green?", "", 0, false); err != nil {
		t.Fatal(err)
	}
	grok := m.Client(t, "grokbot")
	in, _ := grok.Poll(t.Context(), 0)
	grok.Reply(t.Context(), in.Requests[0].ID, "green", envelope.StatusAnswered)
	req, _ := grok.Send(t.Context(), "muse", "call the dentist", envelope.KindAsk, "", false)
	b := newFakeWindow()
	startSocket(t, rb, b, shared)
	settle()
	if got := b.pushes(); len(got) != 0 {
		t.Fatalf("pushes while locked out = %v, want none", got)
	}
	release()
	got := waitPushes(t, b.recordPusher, 2)
	if len(got) != 2 || got[0]["kind"] != "reply" || strings.Contains(got[0]["request_ids"], req.ID) || got[1]["request_ids"] != req.ID {
		t.Fatalf("pushes after the lock frees = %v, want the own reply, then the request", got)
	}
}

// Upgrade notices never go through the socket: each socket message spends
// a turn, and the notice reaches the agent on its next tool call anyway.
func TestSocketWakeSkipsUpgradeNotice(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	old := client.Version
	client.Version = "0.5.4"
	t.Cleanup(func() { client.Version = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"waiting": 0, "upgrade_available": "93.0.0"})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	w := newFakeWindow()
	startSocket(t, r, w, socketShared{dir: t.TempDir(), agent: "muse"})
	settle()
	if w.count() != 0 {
		t.Fatalf("pushes = %v, want none", w.pushes())
	}
}

// A socket that keeps failing ends the loop, so the agent stops peeking and
// stops looking online; socketWake then keeps the session live without
// polling until the socket checks out again. Its peeks advertise socket
// wake and the keepalive after them does not.
func TestSocketWakeStopsPeekingWhenSocketFails(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 10*time.Millisecond, time.Hour)
	var polls, whoami atomic.Int32
	var peekFeatures, keepFeatures atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/poll":
			peekFeatures.Store(r.Header.Get(client.FeaturesHeader))
			polls.Add(1)
			n := polls.Load()
			_ = json.NewEncoder(w).Encode(map[string]any{"waiting": 1, "queued": 1, "pending": []map[string]string{{"id": "q" + string(rune('0'+n%10)), "from": "grokbot"}}})
		case "/v1/whoami":
			keepFeatures.Store(r.Header.Get(client.FeaturesHeader))
			whoami.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"agent": "muse"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	w := newFakeWindow()
	w.fail.Store(true)
	var mu sync.Mutex
	reverified := 0
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		socketWake(ctx, r, w, socketShared{dir: t.TempDir(), agent: "muse"}, socketWakeTiming{keepalive: 20 * time.Millisecond, retry: time.Hour}, func() (socketTarget, error) {
			mu.Lock()
			reverified++
			mu.Unlock()
			return nil, errors.New("still failing")
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for whoami.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	before := polls.Load()
	time.Sleep(200 * time.Millisecond)
	after := polls.Load()
	cancel()
	<-done
	if whoami.Load() == 0 {
		t.Fatal("socketWake never fell back to the keepalive")
	}
	if after != before {
		t.Fatalf("peeks went on after the socket failed: %d then %d", before, after)
	}
	if before > socketFailLimit {
		t.Fatalf("peeks before giving up = %d, want at most %d", before, socketFailLimit)
	}
	if got := peekFeatures.Load(); got != "ping,socket-wake" {
		t.Fatalf("peek features = %v, want ping,socket-wake", got)
	}
	if got := keepFeatures.Load(); got != "ping" {
		t.Fatalf("keepalive features = %v, want ping", got)
	}
}

// The idle gate reads the session registry each time and ignores how long
// ago the status last changed: a window idle for hours is the one to wake.
func TestClaudeSocketIdleIgnoresStatusAge(t *testing.T) {
	f := newClaudeSockFixture(t)
	sock, cfg := f.socket, f.configDir
	pid := os.Getpid() // a live process, as the gate checks
	reg := filepath.Join(cfg, "sessions")
	os.MkdirAll(reg, 0o700)
	write := func(status string) {
		b, _ := json.Marshal(map[string]any{
			"pid": pid, "status": status, "procStart": "Sat Oct 17 04:55:23 2026",
			"statusUpdatedAt": time.Now().Add(-13 * time.Hour).UnixMilli(), "messagingSocketPath": sock,
		})
		os.WriteFile(filepath.Join(reg, strconv.Itoa(pid)+".json"), b, 0o600)
	}
	write("idle")
	env := map[string]string{"CLAUDE_CODE_MESSAGING_SOCKET": sock, "CLAUDE_CONFIG_DIR": cfg}
	p, err := newClaudeSocketPusher(func(k string) string { return env[k] }, pid)
	if err != nil {
		t.Fatal(err)
	}
	if !p.idle() {
		t.Fatal("idle session 13 hours since its last status change is not idle")
	}
	write("busy")
	if p.idle() {
		t.Fatal("busy session reads idle")
	}
}

// A relay that predates pending lists gives only a count, so a new request
// that brings the count back to one it already announced looks the same as
// the old one. The count key is not kept past socketRecordKeep even while
// it is listed, and a change in the count drops it at once, so such a
// request is not kept from its turn for good.
func TestSocketWakeCountOnlyRelayAnnouncesAgain(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 10*time.Millisecond, time.Hour)
	old := socketRecordKeep
	socketRecordKeep = 150 * time.Millisecond
	t.Cleanup(func() { socketRecordKeep = old })
	var queued atomic.Int32
	queued.Store(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := queued.Load()
		_ = json.NewEncoder(w).Encode(map[string]any{"waiting": n, "queued": n})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	w := newFakeWindow()
	startSocket(t, r, w, socketShared{dir: t.TempDir(), agent: "muse"})
	if got := waitPushes(t, w.recordPusher, 1); len(got) != 1 {
		t.Fatalf("first pushes = %v", got)
	}
	queued.Store(2)
	if got := waitPushes(t, w.recordPusher, 2); len(got) != 2 {
		t.Fatalf("pushes after the count rose = %v", got)
	}
	queued.Store(1)
	if got := waitPushes(t, w.recordPusher, 3); len(got) != 3 {
		t.Fatalf("pushes after the count came back to one = %v", got)
	}
}

// Which wake path tincan mcp runs: --channel wins whatever --wake-socket
// says, --wake-socket=false keeps only the keepalive and never reads the
// session socket, and a socket that does not check out falls back to the
// keepalive so the window still keeps its replies.
func TestPickWake(t *testing.T) {
	verified := &claudeSocketPusher{}
	for _, c := range []struct {
		name                string
		channel, wakeSocket bool
		env                 *claudeSocketPusher
		want                wakePath
		wantRead            bool
	}{
		{"channel wins", true, true, verified, wakeByChannel, false},
		{"socket wake off", false, false, verified, wakeByKeepalive, false},
		{"socket checks out", false, true, verified, wakeBySocket, true},
		{"no session socket", false, true, nil, wakeByKeepalive, true},
	} {
		read := false
		route, p := pickWake(c.channel, c.wakeSocket, func() *claudeSocketPusher { read = true; return c.env })
		if route != c.want || read != c.wantRead {
			t.Errorf("%s: route %v (read socket %v), want %v (read %v)", c.name, route, read, c.want, c.wantRead)
		}
		if (route == wakeBySocket) != (p != nil) {
			t.Errorf("%s: route %v with pusher %v", c.name, route, p)
		}
	}
}

func TestWakeSocketOnByDefault(t *testing.T) {
	f := mcpCmd().Flags().Lookup("wake-socket")
	if f == nil || f.DefValue != "true" {
		t.Fatalf("--wake-socket flag = %+v, want one that defaults to true", f)
	}
}

// Once a fresh check of the socket passes, socketWake pushes again and the
// relay hears socket wake again.
func TestSocketWakeResumesAfterReverify(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 10*time.Millisecond, time.Hour)
	var features sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/poll" {
			features.Store(r.Header.Get(client.FeaturesHeader), true)
			_ = json.NewEncoder(w).Encode(map[string]any{"waiting": 1, "queued": 1, "pending": []map[string]string{{"id": "q1", "from": "grokbot"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"agent": "muse"})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	failing, healthy := newFakeWindow(), newFakeWindow()
	failing.fail.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		socketWake(ctx, r, failing, socketShared{dir: t.TempDir(), agent: "muse"}, socketWakeTiming{keepalive: 10 * time.Millisecond, retry: 50 * time.Millisecond}, func() (socketTarget, error) {
			return healthy, nil
		})
	}()
	got := waitPushes(t, healthy.recordPusher, 1)
	cancel()
	<-done
	if len(got) != 1 || got[0]["request_ids"] != "q1" {
		t.Fatalf("pushes after the socket checked out again = %v", got)
	}
	if _, ok := features.Load("ping," + client.SocketWakeFeature); !ok {
		t.Fatal("resumed peeks did not advertise socket wake")
	}
}

// A request queued again after a clarification is answered is the same
// request, but it is new work: it gets its own turn once it has left the
// queue in between.
func TestSocketWakeAnnouncesRequeuedRequest(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, time.Hour)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	w := newFakeWindow()
	muse := museWindow(t, m, "mcp-a")
	startSocket(t, muse, w, shared)
	grok := m.Client(t, "grokbot")
	req, _ := grok.Send(t.Context(), "muse", "book dinner", envelope.KindAsk, "", false)
	if got := waitPushes(t, w.recordPusher, 1); len(got) != 1 {
		t.Fatalf("first pushes = %v", got)
	}
	if _, err := muse.Claim(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(t.Context(), req.ID, "which restaurant?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	// A real answer comes after a person reads the question, well past one
	// poll hold, so a peek sees the request out of the queue first.
	time.Sleep(1500 * time.Millisecond)
	if _, err := grok.Answer(t.Context(), req.ID, "Nopa, 2 people"); err != nil {
		t.Fatal(err)
	}
	got := waitPushes(t, w.recordPusher, 2)
	if len(got) != 2 || got[1]["request_ids"] != req.ID {
		t.Fatalf("pushes after the answer = %v, want the request announced again", got)
	}
}

// Each answer to an ask is its own reply generation and gets its own turn.
func TestSocketWakeAnnouncesNewReplyGeneration(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, time.Hour)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	w := newFakeWindow()
	muse := museWindow(t, m, "mcp-a")
	startSocket(t, muse, w, socketShared{dir: t.TempDir(), agent: "muse"})
	res, err := muse.Ask(t.Context(), "grokbot", "book dinner", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	grok := m.Client(t, "grokbot")
	if _, err := grok.Claim(t.Context(), res.Request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := grok.Reply(t.Context(), res.Request.ID, "which restaurant?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	if got := waitPushes(t, w.recordPusher, 1); len(got) != 1 {
		t.Fatalf("pushes after the question = %v", got)
	}
	if _, err := muse.Answer(t.Context(), res.Request.ID, "Nopa"); err != nil {
		t.Fatal(err)
	}
	if _, err := grok.Claim(t.Context(), res.Request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := grok.Reply(t.Context(), res.Request.ID, "booked for 7", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if got := waitPushes(t, w.recordPusher, 2); len(got) != 2 {
		t.Fatalf("pushes after the final answer = %v, want a second turn", got)
	}
}

// A delivery the record cannot save would be sent again on the next peek,
// so it counts as a failed write and ends socket wake after the limit.
func TestSocketWakeUnsavedRecordCountsAsFailure(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 10*time.Millisecond, time.Hour)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	dir := t.TempDir()
	shared := socketShared{dir: filepath.Join(dir, "rec"), agent: "muse"}
	release, ok, err := shared.tryLock()
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	release()
	if err := os.Chmod(shared.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(shared.dir, 0o700) })
	m.Client(t, "grokbot").Send(t.Context(), "muse", "call the dentist", envelope.KindAsk, "", false)
	w := newFakeWindow()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := pushSocket(ctx, museWindow(t, m, "mcp-a"), w, shared, map[string]bool{}); !errors.Is(err, errSocketFailing) {
		t.Fatalf("pushSocket = %v, want it to give up on an unsavable record", err)
	}
	if n := w.count(); n != socketFailLimit {
		t.Fatalf("pushes = %d, want %d before giving up", n, socketFailLimit)
	}
}

// The own-reply record outlives one pushSocket run, so a socket that
// recovers does not announce a reply it already delivered.
func TestSocketWakeOwnReplySurvivesRestart(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, time.Hour)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	muse := museWindow(t, m, "mcp-a")
	res, _ := muse.Ask(t.Context(), "grokbot", "is the build green?", "", 0, false)
	grok := m.Client(t, "grokbot")
	grok.Claim(t.Context(), res.Request.ID)
	grok.Reply(t.Context(), res.Request.ID, "green", envelope.StatusAnswered)
	own := map[string]bool{}
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	run := func() int {
		w := newFakeWindow()
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		pushSocket(ctx, muse, w, shared, own)
		return w.count()
	}
	if n := run(); n != 1 {
		t.Fatalf("first run pushes = %d, want 1", n)
	}
	if n := run(); n != 0 {
		t.Fatalf("restarted run pushes = %d, want 0", n)
	}
}

// A peek taken before another window saved its delivery must not delete
// that delivery: the older peek simply had not seen the request yet.
func TestSocketWakeOlderPeekKeepsNewerDelivery(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	peekedAt := time.Now()
	saved := peekedAt.Add(time.Second)
	if err := os.MkdirAll(shared.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := shared.write(map[string]time.Time{pendingKey("r1"): saved}); err != nil {
		t.Fatal(err)
	}
	w := newFakeWindow()
	if _, err := pushShared(t.Context(), w, client.Waiting{}, shared, peekedAt); err != nil {
		t.Fatal(err)
	}
	if _, ok := shared.read()[pendingKey("r1")]; !ok {
		t.Fatal("an empty peek taken before the delivery deleted it")
	}
	if _, err := pushShared(t.Context(), w, client.Waiting{}, shared, saved.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok := shared.read()[pendingKey("r1")]; ok {
		t.Fatal("an empty peek taken after the delivery kept it")
	}
}

// A relay that predates per-session replies shows a reply to every window
// of the agent. The window that asked and the others must not each push it:
// one reply starts one turn on the machine. (Seen live against an older
// relay, where both windows woke for one reply.)
func TestSocketWakeOwnReplyOnceAcrossWindowsOnOlderRelay(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/send":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "q1", "from": "muse", "to": "grokbot", "status": "queued"})
		case "/v1/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{"waiting": 1, "queued": 0, "replies": []map[string]any{{
				"request": map[string]any{"id": "q1", "from": "muse", "to": "grokbot"},
				"status":  "answered",
				"reply":   map[string]any{"generation": 1, "from": "grokbot", "status": "answered", "body": "green"},
			}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()
	asker, _ := client.NewRelay(srv.URL, "")
	asker.SetSession("mcp-a")
	if _, err := asker.Ask(t.Context(), "grokbot", "is the build green?", "", 0, false); err != nil {
		t.Fatal(err)
	}
	other, _ := client.NewRelay(srv.URL, "")
	other.SetSession("mcp-b")
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	a, b := newFakeWindow(), newFakeWindow()
	startSocket(t, asker, a, shared)
	startSocket(t, other, b, shared)
	settle()
	if n := totalPushes(a, b); n != 1 {
		t.Fatalf("pushes for one reply across windows = %d (asker %d, other %d), want 1", n, a.count(), b.count())
	}
}

// A lock that cannot be taken at all (a config directory that cannot be
// written) means nothing can be delivered, so it counts as a failed write
// and ends socket wake instead of advertising a wake that never comes.
func TestSocketWakeLockErrorCountsAsFailure(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 10*time.Millisecond, time.Hour)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	shared := socketShared{dir: filepath.Join(blocker, "socket-wake"), agent: "muse"}
	m.Client(t, "grokbot").Send(t.Context(), "muse", "call the dentist", envelope.KindAsk, "", false)
	w := newFakeWindow()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := pushSocket(ctx, museWindow(t, m, "mcp-a"), w, shared, map[string]bool{}); !errors.Is(err, errSocketFailing) {
		t.Fatalf("pushSocket = %v, want it to give up when the lock cannot be taken", err)
	}
	if n := w.count(); n != 0 {
		t.Fatalf("pushes = %d, want none without the lock", n)
	}
}

// A peek that left out a request naming another project has not seen the
// whole queue, so the push the matching window made must stand: without this
// that window would push the same request again after every such peek.
func TestSocketWakeHiddenRequestKeepsDelivery(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	if err := os.MkdirAll(shared.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	saved := time.Now()
	if err := shared.write(map[string]time.Time{pendingKey("r1"): saved}); err != nil {
		t.Fatal(err)
	}
	var peek client.Waiting
	if err := json.Unmarshal([]byte(`{"waiting":0,"queued":0,"hidden":1}`), &peek); err != nil {
		t.Fatal(err)
	}
	w := newFakeWindow()
	later := saved.Add(time.Second)
	if _, err := pushShared(t.Context(), w, peek, shared, later); err != nil {
		t.Fatal(err)
	}
	if _, ok := shared.read()[pendingKey("r1")]; !ok {
		t.Fatal("a peek that hid a request deleted its delivery")
	}
	if _, err := pushShared(t.Context(), w, client.Waiting{}, shared, later); err != nil {
		t.Fatal(err)
	}
	if _, ok := shared.read()[pendingKey("r1")]; ok {
		t.Fatal("a peek of the whole queue kept a request that left it")
	}
}

// A peek that names what it hid knows the whole queue: the hidden request's
// push stands, and a request that is in neither list has left. One hidden
// request it cannot name leaves the record alone, as a capped list does.
func TestSocketWakeHiddenIDsSeparateDeliveries(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	if err := os.MkdirAll(shared.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	saved := time.Now()
	if err := shared.write(map[string]time.Time{pendingKey("theirs"): saved, pendingKey("gone"): saved}); err != nil {
		t.Fatal(err)
	}
	w := newFakeWindow()
	later := saved.Add(time.Second)
	capped := client.Waiting{Hidden: 2, HiddenIDs: []string{"theirs"}}
	if _, err := pushShared(t.Context(), w, capped, shared, later); err != nil {
		t.Fatal(err)
	}
	if _, ok := shared.read()[pendingKey("gone")]; !ok {
		t.Fatal("a peek with a capped hidden list deleted a delivery")
	}
	whole := client.Waiting{Hidden: 1, HiddenIDs: []string{"theirs"}}
	if _, err := pushShared(t.Context(), w, whole, shared, later); err != nil {
		t.Fatal(err)
	}
	rec := shared.read()
	if _, ok := rec[pendingKey("theirs")]; !ok {
		t.Fatal("a peek deleted the delivery of a request it hid")
	}
	if _, ok := rec[pendingKey("gone")]; ok {
		t.Fatal("a peek that named every queued request kept one that left")
	}
	if n := w.count(); n != 0 {
		t.Fatalf("pushes = %d, want a hidden request left to its own window", n)
	}
}

// The other half of once-only delivery: however many empty peeks another
// project's window makes, the matching window pushes its request once.
func TestSocketWakeHiddenRequestPushedOnce(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, time.Hour)
	m := testrelay.New(t, relay.Config{PollHold: 200 * time.Millisecond})
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	alpha, beta := museWindow(t, m, "mcp-a"), museWindow(t, m, "mcp-b")
	alpha.SetProject("alpha")
	beta.SetProject("beta")
	wa, wb := newFakeWindow(), newFakeWindow()
	startSocket(t, alpha, wa, shared)
	startSocket(t, beta, wb, shared)
	if _, err := m.Client(t, "grokbot").Send(client.WithProject(t.Context(), "alpha"), "muse", "book dinner", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	if got := waitPushes(t, wa.recordPusher, 1); len(got) != 1 {
		t.Fatalf("first pushes = %v", got)
	}
	time.Sleep(time.Second)
	if a, b := wa.count(), wb.count(); a != 1 || b != 0 {
		t.Fatalf("pushes = alpha %d, beta %d, want alpha once", a, b)
	}
}

// A window's own request leaves the queue while another project's request
// sits there unseen. Its record must still go, or the request's return after
// a clarification would look already delivered and the idle window would
// never hear of it.
func TestSocketWakeAnnouncesRequeuedRequestBesideHidden(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	fastChannel(t, 20*time.Millisecond, time.Hour)
	m := testrelay.New(t, relay.Config{PollHold: time.Second})
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	alpha, beta := museWindow(t, m, "mcp-a"), museWindow(t, m, "mcp-b")
	alpha.SetProject("alpha")
	beta.SetProject("beta")
	wa, wb := newFakeWindow(), newFakeWindow()
	wb.busy.Store(true)
	startSocket(t, alpha, wa, shared)
	startSocket(t, beta, wb, shared)
	grok := m.Client(t, "grokbot")
	req, err := grok.Send(client.WithProject(t.Context(), "alpha"), "muse", "book dinner", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPushes(t, wa.recordPusher, 1); len(got) != 1 {
		t.Fatalf("first pushes = %v", got)
	}
	if _, err := grok.Send(client.WithProject(t.Context(), "beta"), "muse", "fix the build", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	// The push started a turn, so alpha is busy until it has asked its
	// question.
	wa.busy.Store(true)
	if _, err := alpha.Claim(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := alpha.Reply(t.Context(), req.ID, "which restaurant?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	wa.busy.Store(false)
	// Longer than one poll hold, so alpha's idle peek sees its request gone.
	time.Sleep(1500 * time.Millisecond)
	if _, err := grok.Answer(t.Context(), req.ID, "Nopa, 2 people"); err != nil {
		t.Fatal(err)
	}
	got := waitPushes(t, wa.recordPusher, 2)
	if len(got) != 2 || got[1]["request_ids"] != req.ID {
		t.Fatalf("pushes after the answer = %v, want the request announced again", got)
	}
}

// A return to the queue is announced even when the hidden list is capped
// and so cannot show the old record is stale.
func TestSocketWakeAnnouncesRequeuedRequestPastHiddenCap(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	if err := shared.write(map[string]time.Time{pendingKey("r1"): time.Now(), revKey(envelope.Pending{ID: "r1", Rev: 100}): time.Now()}); err != nil {
		t.Fatal(err)
	}
	hidden := make([]string, relay.MaxPeekPending)
	for i := range hidden {
		hidden[i] = fmt.Sprintf("hidden-%d", i)
	}
	w := client.Waiting{
		Total:     1,
		Queued:    1,
		Pending:   []envelope.Pending{{ID: "r1", From: "grokbot", Kind: envelope.KindAsk, Rev: 200}},
		Hidden:    len(hidden) + 1,
		HiddenIDs: hidden,
	}
	p := newFakeWindow()
	wrote, err := pushShared(t.Context(), p, w, shared, time.Now())
	if err != nil || !wrote {
		t.Fatalf("pushShared = %v, %v", wrote, err)
	}
	if got := p.pushes(); len(got) != 1 || got[0]["request_ids"] != "r1" {
		t.Fatalf("pushes = %v", got)
	}
	// The same return is one delivery.
	wrote, err = pushShared(t.Context(), p, w, shared, time.Now())
	if err != nil || wrote {
		t.Fatalf("second pushShared = %v, %v", wrote, err)
	}
}

// A hidden request's rev key stands too, since the hidden list names only
// its id: without it a later return could not be told from an old window's
// delivery.
func TestSocketWakeHiddenRecordKeepsRev(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	shared := socketShared{dir: t.TempDir(), agent: "muse"}
	if err := shared.write(map[string]time.Time{revKey(envelope.Pending{ID: "r1", Rev: 100}): time.Now().Add(-socketRecordKeep * 3 / 4)}); err != nil {
		t.Fatal(err)
	}
	w := client.Waiting{Hidden: 1, HiddenIDs: []string{"r1"}}
	if _, err := pushShared(t.Context(), newFakeWindow(), w, shared, time.Now()); err != nil {
		t.Fatal(err)
	}
	if at, ok := shared.read()[revKey(envelope.Pending{ID: "r1", Rev: 100})]; !ok || time.Since(at) > time.Minute {
		t.Fatalf("rev key of a hidden request went stale: %v, %v", at, ok)
	}
}

// Windows from before rev share the record during an upgrade. A delivery
// either side made holds for the other, and neither drops the other's key.
func TestSocketWakeMixedVersionsAnnounceOnce(t *testing.T) {
	skipOnWindows(t, skipSocketWake)
	queued := client.Waiting{
		Total:   1,
		Queued:  1,
		Pending: []envelope.Pending{{ID: "r1", From: "grokbot", Kind: envelope.KindAsk, Rev: 200}},
	}
	t.Run("old window delivered", func(t *testing.T) {
		shared := socketShared{dir: t.TempDir(), agent: "muse"}
		if err := shared.write(map[string]time.Time{pendingKey("r1"): time.Now()}); err != nil {
			t.Fatal(err)
		}
		p := newFakeWindow()
		for range 3 {
			if wrote, err := pushShared(t.Context(), p, queued, shared, time.Now()); err != nil || wrote {
				t.Fatalf("pushShared = %v, %v", wrote, err)
			}
		}
		if _, ok := shared.read()[pendingKey("r1")]; !ok {
			t.Fatal("old window's key was dropped")
		}
	})
	t.Run("new window delivered", func(t *testing.T) {
		shared := socketShared{dir: t.TempDir(), agent: "muse"}
		if wrote, err := pushShared(t.Context(), newFakeWindow(), queued, shared, time.Now()); err != nil || !wrote {
			t.Fatalf("pushShared = %v, %v", wrote, err)
		}
		// An old window finds its own key, the only one it reads.
		if _, ok := shared.read()[pendingKey("r1")]; !ok {
			t.Fatalf("record = %v, want the request key", shared.read())
		}
	})
}
