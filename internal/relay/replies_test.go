package relay

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

type replyRecorder struct {
	queuedRecorder
	replied []envelope.Request
}

func (q *replyRecorder) Replied(_ context.Context, req envelope.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.replied = append(q.replied, req)
}

type repliesPoll struct {
	Requests []envelope.Request `json:"requests"`
	Replies  []envelope.Result  `json:"replies"`
}

type peekResult struct {
	Waiting int               `json:"waiting"`
	Queued  int               `json:"queued"`
	Replies []envelope.Result `json:"replies"`
}

// answer has target claim and reply to req.
func (h *harness) answer(addr string, req envelope.Request, body string) {
	h.t.Helper()
	h.do(addr, "POST", "/v1/requests/"+req.ID+"/claim", "", http.StatusOK, nil)
	h.do(addr, "POST", "/v1/requests/"+req.ID+"/reply", `{"body":"`+body+`"}`, http.StatusOK, nil)
}

// A reply tells the Events listener who asked, so the waker can nudge the
// asker rather than the agent that answered.
func TestReplyFiresRepliedForAsker(t *testing.T) {
	h := newHarness(t, Config{})
	rec := &replyRecorder{}
	h.srv.SetEvents(rec)
	sent := h.send(grokAddr, "muse", "call the garage")
	h.answer(museAddr, sent, "Tue 3pm")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.replied) != 1 || rec.replied[0].ID != sent.ID || rec.replied[0].From != "grokbot" || rec.replied[0].To != "muse" {
		t.Fatalf("replied events = %+v, want one for grokbot's request", rec.replied)
	}
}

// Events without the Replier extension keep working when a reply lands.
func TestReplyWithoutReplierIsFine(t *testing.T) {
	h := newHarness(t, Config{})
	h.srv.SetEvents(&queuedRecorder{})
	h.answer(museAddr, h.send(grokAddr, "muse", "x"), "y")
}

// No poll marks a reply seen. Peek and replies=keep report an unseen reply;
// replies=take returns it until the caller acknowledges it.
func TestPollReturnsRepliesWithoutMarkingThem(t *testing.T) {
	h := newHarness(t, Config{})
	sent := h.send(grokAddr, "muse", "call the garage")
	h.answer(museAddr, sent, "Tue 3pm")
	if n := h.srv.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("unseen count = %d, want 1", n)
	}

	var pk peekResult
	h.do(grokAddr, "GET", "/v1/poll?peek=1&replies=keep&hold=0", "", http.StatusOK, &pk)
	if pk.Waiting != 1 || pk.Queued != 0 || len(pk.Replies) != 1 || pk.Replies[0].Request.ID != sent.ID || pk.Replies[0].Reply.Body != "Tue 3pm" {
		t.Fatalf("peek = %+v", pk)
	}
	var kept repliesPoll
	h.do(grokAddr, "GET", "/v1/poll?replies=keep&hold=0", "", http.StatusOK, &kept)
	if len(kept.Replies) != 1 || len(kept.Requests) != 0 {
		t.Fatalf("keep poll = %+v", kept)
	}
	h.do(grokAddr, "GET", "/v1/poll?replies=none&hold=0", "", http.StatusNoContent, nil)

	for range 2 { // taking without an ack leaves the reply for the next poll
		var took repliesPoll
		h.do(grokAddr, "GET", "/v1/poll?replies=take&hold=0", "", http.StatusOK, &took)
		if len(took.Replies) != 1 || took.Replies[0].Reply.From != "muse" || took.Replies[0].Status != envelope.StatusAnswered {
			t.Fatalf("take poll = %+v", took)
		}
	}
	if n := h.srv.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("unseen count after unacked take = %d, want 1", n)
	}
	h.do(grokAddr, "POST", "/v1/replies/ack", `{"ids":["`+sent.ID+`"]}`, http.StatusNoContent, nil)
	h.do(grokAddr, "GET", "/v1/poll?replies=take&hold=0", "", http.StatusNoContent, nil)
	h.do(grokAddr, "GET", "/v1/poll?peek=1&replies=keep&hold=0", "", http.StatusNoContent, nil)
	if n := h.srv.UnseenReplies("grokbot"); n != 0 {
		t.Fatalf("unseen count after ack = %d, want 0", n)
	}
}

// A client that predates replies polls with no replies param and decodes
// only requests. It must not be handed (and lose) replies: they stay unseen
// and out of its response, and a plain peek does not count them either, so
// an old listener does not nudge for something the old inbox never shows.
func TestPollWithoutRepliesParamLeavesRepliesAlone(t *testing.T) {
	h := newHarness(t, Config{})
	sent := h.send(grokAddr, "muse", "call the garage")
	h.answer(museAddr, sent, "Tue 3pm")
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	h.do(grokAddr, "GET", "/v1/poll?peek=1&hold=0", "", http.StatusNoContent, nil)

	incoming := h.send(instinctAddr, "grokbot", "summarize the report")
	var old struct {
		Requests []envelope.Request `json:"requests"`
	}
	rec := h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &old)
	if len(old.Requests) != 1 || old.Requests[0].ID != incoming.ID {
		t.Fatalf("old-style poll = %+v", old)
	}
	if strings.Contains(rec.Body.String(), `"replies"`) {
		t.Fatalf("old-style poll carried replies: %s", rec.Body.String())
	}
	if n := h.srv.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("unseen count after old-style poll = %d, want 1", n)
	}
}

func TestPollRejectsUnknownRepliesMode(t *testing.T) {
	h := newHarness(t, Config{})
	h.do(grokAddr, "GET", "/v1/poll?replies=all&hold=0", "", http.StatusBadRequest, nil)
	h.do(grokAddr, "GET", "/v1/poll?peek=1&replies=all&hold=0", "", http.StatusBadRequest, nil)
}

// An ack marks only the caller's own replies: ids it did not send, or that
// have no reply yet, are ignored.
func TestAckMarksOnlyCallersReplies(t *testing.T) {
	h := newHarness(t, Config{})
	mine := h.send(grokAddr, "muse", "call the garage")
	h.answer(museAddr, mine, "Tue 3pm")
	theirs := h.send(instinctAddr, "muse", "book a table")
	h.answer(museAddr, theirs, "7pm")
	pending := h.send(grokAddr, "muse", "still thinking")
	body := `{"ids":["` + mine.ID + `","` + theirs.ID + `","` + pending.ID + `","nope"]}`
	h.do(grokAddr, "POST", "/v1/replies/ack", body, http.StatusNoContent, nil)
	if n := h.srv.UnseenReplies("grokbot"); n != 0 {
		t.Fatalf("grokbot unseen = %d, want 0", n)
	}
	if n := h.srv.UnseenReplies("instinct"); n != 1 {
		t.Fatalf("instinct unseen = %d, want 1 (grokbot cannot ack its reply)", n)
	}
	h.answer(museAddr, pending, "done")
	if n := h.srv.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("grokbot unseen after late reply = %d, want 1 (an early ack does not count)", n)
	}
	h.do(grokAddr, "POST", "/v1/replies/ack", `{"ids":[]}`, http.StatusNoContent, nil)
	h.do(grokAddr, "POST", "/v1/replies/ack", `not json`, http.StatusBadRequest, nil)
	h.do(strangerAddr, "POST", "/v1/replies/ack", body, http.StatusForbidden, nil)
}

// A poll returns replies up to a byte budget, so a batch of large replies
// stays well under the client's response limit, and says how many are left.
func TestPollBoundsRepliesByBytes(t *testing.T) {
	h := newHarness(t, Config{})
	big := strings.Repeat("x", envelope.DefaultMaxBody-1024)
	const n = 8
	for range n {
		sent := h.send(grokAddr, "muse", big)
		h.answer(museAddr, sent, big)
	}
	var got struct {
		repliesPoll
		Remaining int `json:"replies_remaining"`
	}
	rec := h.do(grokAddr, "GET", "/v1/poll?replies=take&hold=0", "", http.StatusOK, &got)
	if len(got.Replies) == 0 || len(got.Replies) >= n || got.Remaining != n-len(got.Replies) {
		t.Fatalf("got %d replies, %d remaining; want a bounded batch", len(got.Replies), got.Remaining)
	}
	if rec.Body.Len() > MaxRepliesBytes+256<<10 {
		t.Fatalf("response is %d bytes", rec.Body.Len())
	}
}

// Requests and replies come back together in one poll.
func TestPollReturnsRequestsAndReplies(t *testing.T) {
	h := newHarness(t, Config{})
	sent := h.send(grokAddr, "muse", "call the garage")
	h.answer(museAddr, sent, "Tue 3pm")
	incoming := h.send(instinctAddr, "grokbot", "summarize the report")
	var got repliesPoll
	h.do(grokAddr, "GET", "/v1/poll?replies=take&hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != incoming.ID || len(got.Replies) != 1 || got.Replies[0].Request.ID != sent.ID {
		t.Fatalf("poll = %+v", got)
	}
}

// A poll held open for the asker returns as soon as a reply lands, in both
// taking and peek mode.
func TestPollHoldReturnsEarlyOnNewReply(t *testing.T) {
	for _, path := range []string{"/v1/poll?replies=take", "/v1/poll?peek=1&replies=keep"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t, Config{PollHold: 5 * time.Second})
			sent := h.send(grokAddr, "muse", "call the garage")
			h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
			var got repliesPoll
			var took time.Duration
			var wg sync.WaitGroup
			wg.Go(func() {
				start := time.Now()
				h.do(grokAddr, "GET", path, "", http.StatusOK, &got)
				took = time.Since(start)
			})
			time.Sleep(100 * time.Millisecond) // let the poll start waiting
			h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"Tue 3pm"}`, http.StatusOK, nil)
			wg.Wait()
			if len(got.Replies) != 1 || got.Replies[0].Request.ID != sent.ID {
				t.Fatalf("poll got %+v", got)
			}
			if took > time.Second {
				t.Fatalf("poll returned after %v, want under 1s after the reply", took)
			}
		})
	}
}

// The asker reading the reply through get-reply (or an inline ask wait)
// marks it seen, so the next poll does not repeat it.
func TestGetReplyMarksReplySeen(t *testing.T) {
	h := newHarness(t, Config{})
	sent := h.send(grokAddr, "muse", "call the garage")
	h.answer(museAddr, sent, "Tue 3pm")
	h.do(museAddr, "GET", "/v1/requests/"+sent.ID, "", http.StatusOK, nil) // the target's read does not count
	if n := h.srv.UnseenReplies("grokbot"); n != 1 {
		t.Fatalf("unseen after target get = %d, want 1", n)
	}
	h.do(grokAddr, "GET", "/v1/requests/"+sent.ID, "", http.StatusOK, nil)
	h.do(grokAddr, "GET", "/v1/poll?replies=take&hold=0", "", http.StatusNoContent, nil)
}

// A reply to an ask made with parent_id while holding that claim comes back
// from replies=take with a summary of the claimed parent. A reply to an ask
// with no parent carries none. (The implicit single-open-claim parent is
// covered end to end through testrelay in the cli package.)
func TestPollReplyCarriesClaimedParent(t *testing.T) {
	h := newHarness(t, Config{})
	h.srv.SetPreparer(chainPrep{h.st})
	parent := h.send(grokAddr, "instinct", "book the flight")
	h.do(instinctAddr, "POST", "/v1/requests/"+parent.ID+"/claim", "", http.StatusOK, nil)
	var child envelope.Request
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"muse","body":"which airline?","parent_id":"`+parent.ID+`"}`, http.StatusCreated, &child)
	h.answer(museAddr, child, "ANA")
	plain := h.send(museAddr, "grokbot", "unrelated")
	h.answer(grokAddr, plain, "fine")

	var took repliesPoll
	h.do(instinctAddr, "GET", "/v1/poll?replies=take&hold=0", "", http.StatusOK, &took)
	if len(took.Replies) != 1 {
		t.Fatalf("replies = %+v", took.Replies)
	}
	p := took.Replies[0].Parent
	if p == nil || p.ID != parent.ID || p.From != "grokbot" || p.Body != "book the flight" || p.Status != envelope.StatusClaimed {
		t.Fatalf("parent = %+v", p)
	}
	var other repliesPoll
	h.do(museAddr, "GET", "/v1/poll?replies=take&hold=0", "", http.StatusOK, &other)
	if len(other.Replies) != 1 || other.Replies[0].Parent != nil {
		t.Fatalf("reply without a parent = %+v", other.Replies)
	}
}
