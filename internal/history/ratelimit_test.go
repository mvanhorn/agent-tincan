package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// fakeClock is a web agent clock whose sleeps return at once and move its
// time forward, so hours of polling run in milliseconds.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 24, 7, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return nil
}

// clockRig is a scriptRig on a fake clock that records, in fake time, when
// the message was sent and when each detail read happened.
type clockRig struct {
	*scriptRig
	clock  *fakeClock
	mu     sync.Mutex
	sentAt time.Time
	reads  []time.Time
}

func newClockRig(t *testing.T, site Source, detail func(n int, sentAt time.Time) (json.RawMessage, *NativeError)) *clockRig {
	t.Helper()
	fc := newFakeClock()
	r := &clockRig{scriptRig: newScriptRig(t, site, detail), clock: fc}
	inner := r.agent.Native.Channel
	r.agent.Native = &Client{Cooldown: &SiteCooldown{Now: fc.Now}, Channel: channelFunc(func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		r.mu.Lock()
		switch req.Op {
		case OpChatGPTSend, OpClaudeAISend:
			r.sentAt = fc.Now()
		case OpChatGPTDetail, OpClaudeAIDetail:
			r.reads = append(r.reads, fc.Now())
		}
		r.mu.Unlock()
		return inner.Exchange(ctx, req, recv)
	})}
	r.agent.clock = fc
	// The real cadence, not the fast test one.
	r.agent.PollInterval = 0
	return r
}

func (r *clockRig) gaps() (first time.Duration, gaps []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reads) == 0 {
		return 0, nil
	}
	first = r.reads[0].Sub(r.sentAt)
	for i := 1; i < len(r.reads); i++ {
		gaps = append(gaps, r.reads[i].Sub(r.reads[i-1]))
	}
	return first, gaps
}

func (r *clockRig) count(op Op) int {
	r.scriptRig.mu.Lock()
	defer r.scriptRig.mu.Unlock()
	n := 0
	for _, o := range r.ops {
		if o == op {
			n++
		}
	}
	return n
}

func inProgressChain(at time.Time) json.RawMessage {
	return cgChain(scriptConv,
		cgm{id: "u1", role: "user", text: "hi", at: at, status: "finished_successfully"},
		cgm{id: "a1", role: "assistant", text: "still writ", at: at.Add(time.Second), status: "in_progress"},
	)
}

func finishedChain(at time.Time) json.RawMessage {
	return cgChain(scriptConv,
		cgm{id: "u1", role: "user", text: "hi", at: at, status: "finished_successfully"},
		cgm{id: "a1", role: "assistant", text: "Hello back.", at: at.Add(time.Second), status: "finished_successfully", done: true},
	)
}

func rateLimitedErr(retryAfter int) *NativeError {
	return &NativeError{Code: "rate_limited", Message: "HTTP 429 from /backend-api/conversation/" + scriptConv, RetryAfter: retryAfter}
}

// The first detail read waits about 5s after the send, then the reads
// back off (5s, 8s, 12s, then every 20s): never the old every-2s hammer.
func TestWebPollCadenceBacksOff(t *testing.T) {
	rig := newClockRig(t, SourceChatGPT, func(_ int, at time.Time) (json.RawMessage, *NativeError) {
		return inProgressChain(at), nil
	})
	rig.agent.RequestTimeout = 3 * time.Minute
	res := rig.ask(t, "grokbot", "new chat\nhi")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "did not finish answering in time") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	first, gaps := rig.gaps()
	if first != DefaultWebPollSchedule[0] {
		t.Fatalf("first read %s after the send, want %s", first, DefaultWebPollSchedule[0])
	}
	for i, g := range gaps {
		want := DefaultWebPollSchedule[min(i+1, len(DefaultWebPollSchedule)-1)]
		if g != want {
			t.Fatalf("gap %d = %s, want %s (gaps %v)", i, g, want, gaps)
		}
		if g < 5*time.Second || g > 20*time.Second {
			t.Fatalf("gap %d = %s outside [5s, 20s]", i, g)
		}
	}
	// 3 minutes at the old 2s cadence was 90 reads.
	if n := len(gaps) + 1; n > 12 || n < 8 {
		t.Fatalf("%d detail reads in 3 minutes (gaps %v)", n, gaps)
	}
	if c := rig.closed(); len(c) != 1 {
		t.Fatalf("closes = %v", c)
	}
}

// A 429 with Retry-After waits exactly that long before the next read.
func TestWebRateLimitWaitsRetryAfter(t *testing.T) {
	rig := newClockRig(t, SourceChatGPT, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		if n == 0 {
			return nil, rateLimitedErr(90)
		}
		return finishedChain(at), nil
	})
	res := rig.ask(t, "grokbot", "new chat\nhi")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "Hello back.") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if _, gaps := rig.gaps(); len(gaps) != 1 || gaps[0] != 90*time.Second {
		t.Fatalf("gaps = %v, want [1m30s]", gaps)
	}
	if left := rig.agent.Native.CooldownRemaining(SourceChatGPT); left != 0 {
		t.Fatalf("cooldown still %s after waiting it out", left)
	}
}

// A 429 without Retry-After backs off exponentially from 30s.
func TestWebRateLimitBacksOffExponentially(t *testing.T) {
	rig := newClockRig(t, SourceChatGPT, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		if n < 3 {
			return nil, rateLimitedErr(0)
		}
		return finishedChain(at), nil
	})
	res := rig.ask(t, "grokbot", "new chat\nhi")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	_, gaps := rig.gaps()
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}
	if len(gaps) != len(want) {
		t.Fatalf("gaps = %v, want %v", gaps, want)
	}
	for i := range want {
		if gaps[i] != want[i] {
			t.Fatalf("gaps = %v, want %v", gaps, want)
		}
	}
	if d := rateLimitBackoff(20); d != RateLimitBackoffMax {
		t.Fatalf("backoff cap = %s", d)
	}
}

// When the rate limit outlasts the request's budget the request fails with
// the rate-limit message, and while the cooldown runs the next request
// fails at once without touching the site.
func TestWebRateLimitBudgetAndCooldown(t *testing.T) {
	for _, site := range []Source{SourceChatGPT, SourceClaudeAI} {
		rig := newClockRig(t, site, func(int, time.Time) (json.RawMessage, *NativeError) {
			return nil, rateLimitedErr(0)
		})
		rig.agent.RequestTimeout = 3 * time.Minute
		res := rig.ask(t, "grokbot", "new chat\nhi")
		// The message was already sent: the reply says so, names the
		// conversation, and does not invite sending it again.
		sent := "Sorry, " + siteLabel(site) + " is rate-limiting this account right now. The message was sent to " +
			siteLabel(site) + " (conversation " + scriptConv + "); ask for the reply later instead of sending it again."
		if res.Status != envelope.StatusFailed || res.Reply.Body != sent {
			t.Fatalf("%s: %s %q\nwant %q", site, res.Status, res.Reply.Body, sent)
		}
		want := siteLabel(site) + " is rate-limiting this account right now; try again later"
		// Reads at 5s, 35s, 95s; the next (at 215s) is past the budget.
		if _, gaps := rig.gaps(); len(gaps) != 2 {
			t.Fatalf("%s: gaps = %v", site, gaps)
		}
		if c := rig.closed(); len(c) != 1 {
			t.Fatalf("%s: closes = %v", site, c)
		}
		if left := rig.agent.Native.CooldownRemaining(site); left <= 0 {
			t.Fatalf("%s: no cooldown after the rate limit", site)
		}
		sendOp, detailOp := OpChatGPTSend, OpChatGPTDetail
		if site == SourceClaudeAI {
			sendOp, detailOp = OpClaudeAISend, OpClaudeAIDetail
		}
		sends, details := rig.count(sendOp), rig.count(detailOp)
		res = rig.ask(t, "grokbot", "new chat\nhi again")
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, want) {
			t.Fatalf("%s: second request: %s %q", site, res.Status, res.Reply.Body)
		}
		if rig.count(sendOp) != sends || rig.count(detailOp) != details {
			t.Fatalf("%s: the second request reached the site during the cooldown", site)
		}
	}
}

// An HTTP 5xx on a detail read backs off too instead of retrying at the
// normal cadence.
func TestWebServerErrorBacksOff(t *testing.T) {
	rig := newClockRig(t, SourceChatGPT, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		if n < 2 {
			return nil, &NativeError{Code: "http_error", Message: "HTTP 502 from /backend-api/conversation/x"}
		}
		return finishedChain(at), nil
	})
	res := rig.ask(t, "grokbot", "new chat\nhi")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	_, gaps := rig.gaps()
	if len(gaps) != 2 || gaps[0] != 10*time.Second || gaps[1] != 20*time.Second {
		t.Fatalf("gaps = %v, want [10s 20s]", gaps)
	}
}

// While a web request waits for its reply, the agent keeps its relay
// presence fresh (a peek that claims nothing), so it does not show
// offline and a request queued meanwhile stays queued.
func TestWebPresenceDuringLongWait(t *testing.T) {
	rig := newWebRig(t)
	rig.agent.PresenceInterval = 10 * time.Millisecond
	rig.browser.inProgress = 40
	codex := rig.mesh.Client(t, "codex")
	fb := rig.browser
	var (
		mu       sync.Mutex
		reads    int
		start    time.Time
		lastPoll time.Time
		queuedID string
	)
	rig.agent.Native = &Client{Channel: channelFunc(func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		if req.Op == OpChatGPTDetail {
			mu.Lock()
			n := reads
			reads++
			mu.Unlock()
			switch n {
			case 0:
				start = time.Now()
				q, err := codex.Send(ctx, "chatgpt-web", "a second question", envelope.KindAsk, "")
				if err != nil {
					t.Error(err)
				}
				queuedID = q.ID
			case 30:
				agents, err := codex.Agents(ctx)
				if err != nil {
					t.Error(err)
				}
				for _, a := range agents {
					if a.Name == "chatgpt-web" {
						lastPoll = a.LastPoll
					}
				}
			}
		}
		return fb.Exchange(ctx, req, recv)
	})}
	res := rig.ask(t, "grokbot", "a long one")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if !lastPoll.After(start) {
		t.Fatalf("last poll %s is not after the wait began (%s): no presence during the wait", lastPoll, start)
	}
	got, err := codex.Get(t.Context(), queuedID, 0)
	if err != nil || got.Status != envelope.StatusQueued {
		t.Fatalf("the request queued during the wait: %s %v (want still queued)", got.Status, err)
	}
}

// A live history read that hits a 429 fails at once with the rate-limit
// message, and later reads of that site fail without reaching it while
// the cooldown runs. Another site is not held back.
func TestLiveReadRateLimitFailsFast(t *testing.T) {
	var mu sync.Mutex
	calls := map[Source]int{}
	ch := channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		mu.Lock()
		calls[req.Op.source()]++
		mu.Unlock()
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "rate_limited", Message: "HTTP 429 from /backend-api/conversations", RetryAfter: 60}})
		return err
	})
	c := &Client{Channel: ch, Cooldown: &SiteCooldown{}}
	_, err := NewChatGPT(c).List(context.Background(), 5, Options{})
	if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "ChatGPT is rate-limiting this account right now; try again later") {
		t.Fatalf("err = %v", err)
	}
	if calls[SourceChatGPT] != 1 {
		t.Fatalf("%d calls; a 429 must not be retried", calls[SourceChatGPT])
	}
	if left := c.CooldownRemaining(SourceChatGPT); left <= 50*time.Second || left > time.Minute {
		t.Fatalf("cooldown %s, want the Retry-After", left)
	}
	_, err = NewChatGPT(c).Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeConversation, ConversationID: "abc-1"}, Options{})
	if !errors.Is(err, ErrRateLimited) || calls[SourceChatGPT] != 1 {
		t.Fatalf("during the cooldown: %v after %d calls", err, calls[SourceChatGPT])
	}
	if got := readFailure(Query{Source: SourceChatGPT}, err); got != "Sorry, ChatGPT is rate-limiting this account right now; try again later." {
		t.Fatalf("readFailure = %q", got)
	}
	if _, err := NewClaudeAI(c).List(context.Background(), 5, Options{}); !errors.Is(err, ErrRateLimited) || calls[SourceClaudeAI] != 1 {
		t.Fatalf("claude.ai held back by ChatGPT's cooldown: %v %d", err, calls[SourceClaudeAI])
	}
	// Closing a tab does not touch the site and still runs.
	if err := c.Close(context.Background(), SourceChatGPT, "abc-1"); errors.Is(err, ErrRateLimited) && calls[SourceChatGPT] == 1 {
		t.Fatal("close was held back by the cooldown")
	}
}

func TestFromNativeErrorRateLimited(t *testing.T) {
	for _, code := range []string{"rate_limited", "http_429"} {
		err := fromNativeError(SourceChatGPT, &NativeError{Code: code, Message: "HTTP 429 from /x", RetryAfter: 42})
		after, ok := rateLimited(err)
		if !ok || after != 42*time.Second || !errors.Is(err, ErrRateLimited) {
			t.Fatalf("%s: %v %s %v", code, err, after, ok)
		}
	}
	err := fromNativeError(SourceClaudeAI, &NativeError{Code: "rate_limited", RetryAfter: 1 << 30})
	if after, _ := rateLimited(err); after != maxRetryAfter {
		t.Fatalf("huge Retry-After not capped: %s", after)
	}
	if !strings.Contains(err.Error(), "claude.ai is rate-limiting this account") {
		t.Fatalf("%v", err)
	}
	if !serverError(fromNativeError(SourceChatGPT, &NativeError{Code: "http_error", Message: "HTTP 503 from /x"})) {
		t.Fatal("503 is not a server error")
	}
	if serverError(fromNativeError(SourceChatGPT, &NativeError{Code: "http_error", Message: "HTTP 400 from /x"})) {
		t.Fatal("400 is a server error")
	}
}

// The native host is shared by every reader and agent: after a 429 it
// answers that site's requests itself until the cooldown ends, without
// asking the extension. Other sites and tab closes still go through.
func TestNativeHostRateLimitCooldown(t *testing.T) {
	sock := filepath.Join(shortDir(t), "n", "host.sock")
	chromeToHostR, chromeToHostW := io.Pipe()
	hostToChromeR, hostToChromeW := io.Pipe()
	seen := make(chan NativeRequest, 16)
	fakeExtension(t, hostToChromeR, chromeToHostW, seen, func(req NativeRequest) []NativeResponse {
		switch req.Op {
		case OpChatGPTDetail:
			return []NativeResponse{{Error: &NativeError{Code: "rate_limited", Message: "HTTP 429 from /backend-api/conversation/abc-1", RetryAfter: 60}}}
		case OpChatGPTClose:
			return []NativeResponse{{OK: true, Result: json.RawMessage(`{"closed":0}`)}}
		}
		return []NativeResponse{{OK: true, Result: json.RawMessage(`[]`)}}
	})
	host := &NativeHost{SocketPath: sock, In: chromeToHostR, Out: hostToChromeW, RequestTimeout: 5 * time.Second}
	go func() { _ = host.Run(context.Background()) }()
	defer chromeToHostW.Close()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })
	// A fresh client each time, so only the host's cooldown can hold a
	// request back.
	client := func() *Client {
		return &Client{Channel: &SocketChannel{Path: sock, ChromeRunning: func() bool { return true }}, Timeout: 5 * time.Second, Cooldown: &SiteCooldown{}}
	}
	_, err := client().Request(context.Background(), OpChatGPTDetail, OpArgs{ID: "abc-1"})
	if after, ok := rateLimited(err); !ok || after != time.Minute {
		t.Fatalf("first: %v", err)
	}
	<-seen
	_, err = client().Request(context.Background(), OpChatGPTList, OpArgs{Count: 1})
	if after, ok := rateLimited(err); !ok || after <= 0 || after > time.Minute {
		t.Fatalf("during the cooldown: %v", err)
	}
	select {
	case r := <-seen:
		t.Fatalf("the extension was asked during the cooldown: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := client().Request(context.Background(), OpClaudeAIList, OpArgs{Count: 1}); err != nil {
		t.Fatalf("claude.ai held back: %v", err)
	}
	<-seen
	if err := client().Close(context.Background(), SourceChatGPT, "abc-1"); err != nil {
		t.Fatalf("close held back: %v", err)
	}
	<-seen
}

// A ChatGPT image turn: a visible tool message with the image, hidden tool
// messages, a visible reasoning recap with end_turn false, then a hidden
// empty assistant text with end_turn true. It is finished, and the reply
// is the image with no text.
func TestChatGPTImageTurnFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "chatgpt", "image-turn-detail.json"))
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := chatgptNodes(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := progressOf(nodes, replyAnchor{message: "Tincan test: generate a tiny simple image: a red circle on a white background. No text."})
	if p.userID != "u-img-0001" || !p.found || !p.finished || p.orphaned {
		t.Fatalf("progress = %+v", p)
	}
	// Without the final end_turn it is not finished yet.
	partial := strings.Replace(string(raw), `"end_turn": true`, `"end_turn": null`, 1)
	partial = strings.Replace(partial, `"finish_details"`, `"no_finish_details"`, 1)
	nodes, err = chatgptNodes(json.RawMessage(partial))
	if err != nil {
		t.Fatal(err)
	}
	if p := progressOf(nodes, replyAnchor{message: "Tincan test: generate a tiny simple image: a red circle on a white background. No text."}); !p.found || p.finished {
		t.Fatalf("partial progress = %+v", p)
	}
	th, err := parseChatGPTDetail("6ab4cc22-0000-4000-8000-00000000c0de", raw)
	if err != nil || len(th.turns) != 1 || th.turns[0].reply.Text != "" || len(th.turns[0].replyImages) != 1 {
		t.Fatalf("parse: %+v %v", th.turns, err)
	}
}

// The web agent answers an image turn of that shape with the image and a
// short note instead of waiting until the request times out.
func TestWebChatGPTImageTurnCompletes(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "chatgpt", "image-turn-detail.json"))
	if err != nil {
		t.Fatal(err)
	}
	const prompt = "Tincan test: generate a tiny simple image: a red circle on a white background. No text."
	rig := newScriptRig(t, SourceChatGPT, func(_ int, at time.Time) (json.RawMessage, *NativeError) {
		var d map[string]any
		_ = json.Unmarshal(fixture, &d)
		d["conversation_id"] = scriptConv
		mapping := d["mapping"].(map[string]any)
		for _, n := range mapping {
			if m, ok := n.(map[string]any)["message"].(map[string]any); ok {
				m["create_time"] = float64(at.UnixMilli())/1000 + 1
			}
		}
		b, _ := json.Marshal(d)
		return b, nil
	})
	img := fakePNG(1500)
	inner := rig.agent.Native.Channel
	rig.agent.Native = &Client{Channel: channelFunc(func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		if req.Op == OpChatGPTFile {
			if req.Args.FileID != "file_00000000aaaa0000bbbb0000cccc0001" {
				t.Errorf("file op for %q", req.Args.FileID)
			}
			for _, fr := range chunkFrames(img, "image/png", 1<<10) {
				fr.ID = req.ID
				if done, err := recv(fr); done || err != nil {
					return err
				}
			}
			return nil
		}
		return inner.Exchange(ctx, req, recv)
	})}
	rig.agent.RequestTimeout = 5 * time.Second
	res := rig.ask(t, "grokbot", "new chat\n"+prompt)
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "replied with an image and no text") || !strings.Contains(body, "1 image attached.") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if len(res.Reply.Attachments) != 1 {
		t.Fatalf("attachments = %+v", res.Reply.Attachments)
	}
	data, _, err := rig.mesh.Client(t, "grokbot").FetchAttachment(t.Context(), res.Reply.Attachments[0].ID)
	if err != nil || !bytes.Equal(data, img) {
		t.Fatalf("attachment: %d bytes %v", len(data), err)
	}
}

// While connected, the native host re-checks the extension files on disk
// every RecheckInterval, and asks for a reload once they drift from what
// the extension loaded (once per cooldown).
func TestNativeHostRechecksExtensionFiles(t *testing.T) {
	extDir := t.TempDir()
	loaded := writeExtensionDir(t, extDir, "0.2.0", map[string]string{"ops.js": "same"})
	sock := filepath.Join(shortDir(t), "n", "host.sock")
	chromeToHostR, chromeToHostW := io.Pipe()
	hostToChromeR, hostToChromeW := io.Pipe()
	var logBuf lockedBuffer
	host := &NativeHost{SocketPath: sock, In: chromeToHostR, Out: hostToChromeW, ExtensionDir: extDir, Log: &logBuf, RecheckInterval: 30 * time.Millisecond}
	go func() { _ = host.Run(context.Background()) }()
	defer chromeToHostW.Close()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })
	fromHost := make(chan NativeRequest, 8)
	go func() {
		for {
			b, err := ReadMessage(hostToChromeR, MaxHostMessage)
			if err != nil {
				return
			}
			var r NativeRequest
			_ = json.Unmarshal(b, &r)
			fromHost <- r
		}
	}()
	if err := WriteMessage(chromeToHostW, map[string]any{"id": 0, "hello": loaded}, MaxChromeMessage); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-fromHost:
		t.Fatalf("matching files: host sent %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
	// An update lands on disk while the extension stays connected.
	if err := os.WriteFile(filepath.Join(extDir, "ops.js"), []byte("updated"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-fromHost:
		if r.Op != OpExtensionReload {
			t.Fatalf("host sent %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no reload after the files changed (log: %s)", logBuf.String())
	}
	// Later checks toward the same files respect the cooldown.
	select {
	case r := <-fromHost:
		t.Fatalf("asked again inside the cooldown: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
}

// Before anything is sent, a rate limit keeps the bare message: nothing
// went through, so trying again later is the right advice.
func TestSendFailureRateLimitKeepsBareMessage(t *testing.T) {
	w := &WebAgent{Site: SourceChatGPT}
	err := fromNativeError(SourceChatGPT, rateLimitedErr(30))
	if got := w.sendFailure(err, scriptConv); got != "ChatGPT is rate-limiting this account right now; try again later." {
		t.Fatalf("sendFailure = %q", got)
	}
	if got := w.waitFailure(err, scriptConv); !strings.Contains(got, "The message was sent to ChatGPT (conversation "+scriptConv+")") || strings.Contains(got, "try again later") {
		t.Fatalf("waitFailure = %q", got)
	}
}

func TestClampRetryAfterSeconds(t *testing.T) {
	for _, c := range []struct {
		in   int
		want time.Duration
	}{{-5, 0}, {0, 0}, {42, 42 * time.Second}, {3600, time.Hour}, {1 << 30, maxRetryAfter}, {int(^uint(0) >> 1), maxRetryAfter}} {
		if got := clampRetryAfterSeconds(c.in); got != c.want {
			t.Errorf("clampRetryAfterSeconds(%d) = %s, want %s", c.in, got, c.want)
		}
	}
}

// A drift that a reload cannot clear (the loaded copy lives elsewhere) is
// not re-requested by the periodic recheck, however old the record: only
// the files changing again (a new fingerprint) or a reconnect asks again.
func TestNativeHostRecheckDoesNotRepeatRecordedDrift(t *testing.T) {
	extDir := t.TempDir()
	loaded := writeExtensionDir(t, extDir, "0.2.0", map[string]string{"ops.js": "same"})
	// The loaded copy never matches the disk.
	loaded.Files["ops.js"] = "elsewhere"
	statePath := filepath.Join(t.TempDir(), "reload-state.json")
	sock := filepath.Join(shortDir(t), "n", "host.sock")
	chromeToHostR, chromeToHostW := io.Pipe()
	hostToChromeR, hostToChromeW := io.Pipe()
	var logBuf lockedBuffer
	host := &NativeHost{SocketPath: sock, In: chromeToHostR, Out: hostToChromeW, ExtensionDir: extDir, ReloadStatePath: statePath, Log: &logBuf, RecheckInterval: 20 * time.Millisecond}
	go func() { _ = host.Run(context.Background()) }()
	defer chromeToHostW.Close()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })
	fromHost := make(chan NativeRequest, 64)
	go func() {
		for {
			b, err := ReadMessage(hostToChromeR, MaxHostMessage)
			if err != nil {
				return
			}
			var r NativeRequest
			_ = json.Unmarshal(b, &r)
			fromHost <- r
		}
	}()
	expectReload := func(what string) {
		t.Helper()
		select {
		case r := <-fromHost:
			if r.Op != OpExtensionReload {
				t.Fatalf("%s: host sent %+v", what, r)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: no reload (log: %s)", what, logBuf.String())
		}
	}
	expectQuiet := func(what string) {
		t.Helper()
		select {
		case r := <-fromHost:
			t.Fatalf("%s: host asked again: %+v (log: %s)", what, r, logBuf.String())
		case <-time.After(300 * time.Millisecond):
		}
	}
	// age moves the recorded reload hours into the past, past every
	// cooldown, as if the drift had persisted that long.
	age := func() {
		t.Helper()
		b, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		var st reloadState
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		st.At = st.At.Add(-7 * time.Hour)
		b, _ = json.Marshal(st)
		if err := os.WriteFile(statePath, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteMessage(chromeToHostW, map[string]any{"id": 0, "hello": loaded}, MaxChromeMessage); err != nil {
		t.Fatal(err)
	}
	expectReload("first hello with drift")
	age()
	expectQuiet("recheck, same drift")
	// The files change again: a new fingerprint, so one more reload.
	if err := os.WriteFile(filepath.Join(extDir, "ops.js"), []byte("updated"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectReload("files changed")
	age()
	expectQuiet("recheck, new drift already asked for")
	// A real reconnect hello still gets its once-per-cooldown reload.
	if err := WriteMessage(chromeToHostW, map[string]any{"id": 0, "hello": loaded}, MaxChromeMessage); err != nil {
		t.Fatal(err)
	}
	expectReload("reconnect past the cooldown")
}
