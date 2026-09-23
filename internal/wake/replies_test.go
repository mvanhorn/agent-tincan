package wake

import (
	"context"
	"encoding/json"
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
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, st, Options{ReplyGrace: 150 * time.Millisecond, UnseenReplies: unseen(&n)})
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
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil, Options{ReplyGrace: 50 * time.Millisecond, UnseenReplies: unseen(&n)})
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
		Options{ReplyGrace: time.Millisecond, UnseenReplies: unseen(&n), Online: func(string) bool { return true }})
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
				Options{Debounce: 50 * time.Millisecond, ReplyGrace: 200 * time.Millisecond, UnseenReplies: unseen(&n)})
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
		Options{Debounce: 10 * time.Millisecond, ReplyGrace: time.Hour, UnseenReplies: unseen(&n)})
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
		Options{Debounce: time.Millisecond, ReplyGrace: time.Millisecond, UnseenReplies: unseen(&n), Now: func() time.Time { return now }})
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
		Options{ReplyGrace: time.Millisecond, UnseenReplies: unseen(&n), AgentMailAPI: ts.URL})
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
		Options{ReplyGrace: time.Millisecond, UnseenReplies: unseen(&n), AgentMailAPI: ts.URL + "/v0"})
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
