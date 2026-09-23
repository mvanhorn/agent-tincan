package wake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// replied reports a reply to one of asker's requests, as the relay does.
func replied(w *Waker, asker string) {
	w.Replied(context.Background(), envelope.Request{ID: "q1", From: asker, To: "muse", Body: "SECRET call Joe's Garage at 555-0100"})
}

func unseen(n *atomic.Int32) func(string) int {
	return func(string) int { return int(n.Load()) }
}

func message(t *testing.T, raw string) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return body["message"]
}

// A reply the asker has not read by the end of the grace period wakes it
// with a count-only message.
func TestReplyNudgeFiresAfterGraceWhenUnseen(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, st, Options{ReplyGrace: 150 * time.Millisecond, UnseenReplies: unseen(&n), ReplyRetries: noRetries})
	replied(w, "hermes")
	time.Sleep(30 * time.Millisecond)
	if rc.count() != 0 {
		t.Fatal("reply nudge fired before the grace period ended")
	}
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want 1", rc.count())
	}
	if got := message(t, rc.bodies[0]); got != WaitingMessage(0, 1) || !strings.Contains(got, "1 reply") {
		t.Fatalf("message = %q", got)
	}
	if strings.Contains(rc.bodies[0], "SECRET") || strings.Contains(rc.bodies[0], "555") {
		t.Fatalf("wake body leaks the request: %s", rc.bodies[0])
	}
	if got := strings.Join(events(t, st), ","); got != "woke" {
		t.Fatalf("audit = %s", got)
	}
}

// An asker that read the reply inline within the grace period is not woken.
func TestReplyNudgeSuppressedWhenReadWithinGrace(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil, Options{ReplyGrace: 50 * time.Millisecond, UnseenReplies: unseen(&n), ReplyRetries: noRetries})
	replied(w, "hermes")
	n.Store(0) // the asker's inline wait read it
	w.Flush()
	if rc.count() != 0 {
		t.Fatalf("wakes = %d, want none for a reply already read", rc.count())
	}
}

// The asker's session may have ended seconds before the reply, so a recent
// poll must not suppress a reply wake.
func TestReplyNudgeIgnoresOnline(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil,
		Options{ReplyGrace: time.Millisecond, UnseenReplies: unseen(&n), ReplyRetries: noRetries, Online: func(string) bool { return true }})
	replied(w, "hermes")
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want 1 even though the asker looks online", rc.count())
	}
}

// A request and a reply close together become one nudge that counts both,
// whichever comes first, and a burst of replies is one nudge.
func TestReplyAndRequestCoalesceIntoOneNudge(t *testing.T) {
	for _, order := range []string{"reply-first", "request-first", "reply-burst"} {
		t.Run(order, func(t *testing.T) {
			var rc recorder
			ts := rc.server(t)
			var n atomic.Int32
			w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil,
				Options{Debounce: 50 * time.Millisecond, ReplyGrace: 200 * time.Millisecond, UnseenReplies: unseen(&n), ReplyRetries: noRetries})
			want := WaitingMessage(1, 1)
			switch order {
			case "reply-first":
				n.Store(1)
				replied(w, "hermes")
				queued(w, "hermes", 1)
			case "request-first":
				queued(w, "hermes", 1)
				n.Store(1)
				replied(w, "hermes")
			case "reply-burst":
				for range 3 {
					n.Add(1)
					replied(w, "hermes")
				}
				want = WaitingMessage(0, 3)
			}
			w.Flush()
			if rc.count() != 1 {
				t.Fatalf("wakes = %d, want 1", rc.count())
			}
			if got := message(t, rc.bodies[0]); got != want {
				t.Fatalf("message = %q, want %q", got, want)
			}
		})
	}
}

// A pending request does not wait out the reply grace period: the nudge goes
// at the request's debounce and carries the reply count along.
func TestRequestDoesNotWaitForReplyGrace(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil,
		Options{Debounce: 10 * time.Millisecond, ReplyGrace: time.Hour, UnseenReplies: unseen(&n), ReplyRetries: noRetries})
	replied(w, "hermes")
	start := time.Now()
	queued(w, "hermes", 1)
	w.Flush()
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("nudge took %v; the request waited out the reply grace", took)
	}
	if rc.count() != 1 || message(t, rc.bodies[0]) != WaitingMessage(1, 1) {
		t.Fatalf("wakes = %d %v", rc.count(), rc.bodies)
	}
}

// Reply wakes count against the same hourly budget as request wakes.
func TestReplyNudgeRespectsHourlyCap(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	now := time.Unix(1_790_000_000, 0)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL, MaxPerHour: 1}}, st,
		Options{Debounce: time.Millisecond, ReplyGrace: time.Millisecond, UnseenReplies: unseen(&n), ReplyRetries: noRetries, Now: func() time.Time { return now }})
	queued(w, "hermes", 1)
	w.Flush()
	replied(w, "hermes")
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want 1 under a cap of 1", rc.count())
	}
	if got := events(t, st); got[len(got)-1] != "wake_skipped" {
		t.Fatalf("audit = %v", got)
	}
}

// Only relay-side methods get reply wakes; agent-side ones see replies on
// their own poll or channel.
func TestReplyNudgeOnlyForRelaySideMethods(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"muse": {Method: Wait}, "claude-code": {Method: Channel}, "codex": {Method: Command}}, nil,
		Options{ReplyGrace: time.Millisecond, UnseenReplies: unseen(&n), ReplyRetries: noRetries, AgentMailAPI: ts.URL})
	for _, asker := range []string{"muse", "claude-code", "codex", "chatgpt"} {
		replied(w, asker)
	}
	w.Flush()
	if rc.count() != 0 {
		t.Fatalf("wakes = %d, want none", rc.count())
	}
}

// Email reply wakes keep the subject the standing instructions look for and
// carry only counts.
func TestReplyNudgeByEmail(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var n atomic.Int32
	n.Store(2)
	w := New(Config{"instinct": {Method: Email, EmailTo: "agent@example.com", AgentMailFrom: "bot@agentmail.to", AgentMailKey: "k"}}, nil,
		Options{ReplyGrace: time.Millisecond, UnseenReplies: unseen(&n), ReplyRetries: noRetries, AgentMailAPI: ts.URL + "/v0"})
	replied(w, "instinct")
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want 1", rc.count())
	}
	var body map[string]string
	json.Unmarshal([]byte(rc.bodies[0]), &body)
	if body["subject"] != "Agent Tincan: requests waiting" || body["text"] != WaitingMessage(0, 2) || strings.Contains(rc.bodies[0], "SECRET") {
		t.Fatalf("email body = %s", rc.bodies[0])
	}
}

func TestWaitingMessage(t *testing.T) {
	for _, tc := range []struct {
		requests, replies int
		want              string
	}{
		{2, 0, Message(2)},
		{0, 1, "Agent Tincan: 1 reply to your request is waiting. Run check_inbox (or `tincan inbox`) to read it."},
		{0, 3, "Agent Tincan: 3 replies to your requests are waiting. Run check_inbox (or `tincan inbox`) to read them."},
		{1, 2, "Agent Tincan: 1 request from your teammates and 2 replies to your requests waiting. Run check_inbox (or `tincan inbox`) to read the replies and pick up the request, then reply to it."},
	} {
		if got := WaitingMessage(tc.requests, tc.replies); got != tc.want {
			t.Errorf("WaitingMessage(%d, %d) = %q, want %q", tc.requests, tc.replies, got, tc.want)
		}
	}
}

// noRetries turns off reply follow-ups for tests that count a single nudge.
var noRetries = []time.Duration{}

// A reply that stays unseen after its nudge is nudged again on each step of
// the retry schedule, then the retries stop.
func TestReplyRetriesWhileUnseenThenStop(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, st,
		Options{ReplyGrace: time.Millisecond, ReplyRetries: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}, UnseenReplies: unseen(&n)})
	replied(w, "hermes")
	w.Flush()
	if rc.count() != 3 {
		t.Fatalf("wakes = %d, want the first nudge plus 2 retries", rc.count())
	}
	for i, b := range rc.bodies {
		if got := message(t, b); got != WaitingMessage(0, 1) || strings.Contains(b, "SECRET") {
			t.Fatalf("wake %d message = %q", i, got)
		}
	}
	if got := strings.Join(events(t, st), ","); got != "woke,woke,woke" {
		t.Fatalf("audit = %s", got)
	}
}

// Once the asker reads the reply, the pending retry finds nothing unseen and
// the schedule ends.
func TestReplyRetriesStopWhenSeen(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil,
		Options{ReplyGrace: time.Millisecond, ReplyRetries: []time.Duration{300 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}, UnseenReplies: unseen(&n)})
	replied(w, "hermes")
	deadline := time.Now().Add(5 * time.Second)
	for rc.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	n.Store(0) // the woken session read it
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want 1: the reply was read before the first retry", rc.count())
	}
}

// Retries count against the hourly budget like every other relay-side wake.
func TestReplyRetriesRespectHourlyCap(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	now := time.Unix(1_790_000_000, 0)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL, MaxPerHour: 2}}, st,
		Options{ReplyGrace: time.Millisecond, ReplyRetries: []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond},
			UnseenReplies: unseen(&n), Now: func() time.Time { return now }})
	replied(w, "hermes")
	w.Flush()
	if rc.count() != 2 {
		t.Fatalf("wakes = %d, want 2 under a cap of 2", rc.count())
	}
	if got := strings.Join(events(t, st), ","); got != "woke,woke,wake_skipped,wake_skipped" {
		t.Fatalf("audit = %s", got)
	}
}

// A request that lands while a retry is pending coalesces with it into one
// nudge counting both, and the schedule carries on after it.
func TestReplyRetryCoalescesWithRequest(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil,
		Options{Debounce: 5 * time.Millisecond, ReplyGrace: time.Millisecond, ReplyRetries: []time.Duration{time.Hour, 10 * time.Millisecond},
			UnseenReplies: unseen(&n)})
	replied(w, "hermes")
	deadline := time.Now().Add(5 * time.Second)
	for rc.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	queued(w, "hermes", 1) // pulls the hour-long retry in to the debounce
	w.Flush()
	if rc.count() != 3 {
		t.Fatalf("wakes = %d, want reply, request+reply, final retry", rc.count())
	}
	if got := message(t, rc.bodies[1]); got != WaitingMessage(1, 1) {
		t.Fatalf("coalesced message = %q", got)
	}
	if got := message(t, rc.bodies[2]); got != WaitingMessage(0, 1) {
		t.Fatalf("last retry message = %q", got)
	}
}

func TestDefaultReplyRetries(t *testing.T) {
	w := New(Config{}, nil, Options{})
	want := []time.Duration{5 * time.Minute, 20 * time.Minute, time.Hour}
	if len(w.opts.ReplyRetries) != len(want) {
		t.Fatalf("retries = %v, want %v", w.opts.ReplyRetries, want)
	}
	for i := range want {
		if w.opts.ReplyRetries[i] != want[i] {
			t.Fatalf("retries = %v, want %v", w.opts.ReplyRetries, want)
		}
	}
}

// A fresh reply that lands while an earlier nudge is still being sent earns
// the full follow-up schedule: the earlier nudge's stale follow-up must not
// push it past its first step.
func TestFreshReplyDuringSlowSendKeepsFirstFollowUp(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release // the first send is slow
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ts.Close)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil,
		Options{ReplyGrace: 100 * time.Millisecond, ReplyRetries: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}, UnseenReplies: unseen(&n)})
	replied(w, "hermes")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first nudge never sent")
	}
	n.Store(2)
	replied(w, "hermes") // a fresh reply while the first send is in flight
	close(release)
	w.Flush()
	// First nudge, the fresh reply's nudge, then both of its follow-ups.
	if got := calls.Load(); got != 4 {
		t.Fatalf("wakes = %d, want 4: the fresh reply lost a follow-up step", got)
	}
}

// A nudge whose webhook send fails still schedules the follow-up, which
// reaches the agent once the webhook recovers.
func TestFailedReplyNudgeStillFollowsUp(t *testing.T) {
	var rc recorder
	rc.fail.Store(2) // the send and its single retry both fail
	ts := rc.server(t)
	st := auditStore(t)
	var n atomic.Int32
	n.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, st,
		Options{ReplyGrace: time.Millisecond, RetryDelay: time.Millisecond, ReplyRetries: []time.Duration{10 * time.Millisecond}, UnseenReplies: unseen(&n)})
	replied(w, "hermes")
	w.Flush()
	if rc.count() != 3 {
		t.Fatalf("webhook calls = %d, want 2 failed attempts plus the follow-up", rc.count())
	}
	if got := strings.Join(events(t, st), ","); got != "wake_failed,woke" {
		t.Fatalf("audit = %s", got)
	}
}
