package relay

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func sessionHeader(s string) map[string]string {
	if s == "" {
		return nil
	}
	return map[string]string{client.SessionHeader: s}
}

// sendFrom sends body from addr in session and returns the stored request.
func (h *harness) sendFrom(addr, session, to, body string) envelope.Request {
	h.t.Helper()
	rec := h.raw(addr, "POST", "/v1/send", strings.NewReader(`{"to":"`+to+`","body":"`+body+`"}`), -1, sessionHeader(session))
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("send from session %q: status %d: %s", session, rec.Code, rec.Body)
	}
	var req envelope.Request
	if err := json.Unmarshal(rec.Body.Bytes(), &req); err != nil {
		h.t.Fatal(err)
	}
	return req
}

// pollReplies polls addr's replies in session and returns their request ids.
func (h *harness) pollReplies(addr, session, mode string) []string {
	h.t.Helper()
	rec := h.raw(addr, "GET", "/v1/poll?replies="+mode+"&hold=0", nil, 0, sessionHeader(session))
	if rec.Code == http.StatusNoContent {
		return []string{}
	}
	if rec.Code != http.StatusOK {
		h.t.Fatalf("poll from session %q: status %d: %s", session, rec.Code, rec.Body)
	}
	var got repliesPoll
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		h.t.Fatal(err)
	}
	ids := []string{}
	for _, r := range got.Replies {
		ids = append(ids, r.Request.ID)
	}
	return ids
}

// twoAnsweredSessions has grokbot send one ask from session A and one from
// session B, and muse answer both.
func twoAnsweredSessions(t *testing.T) (h *harness, a, b envelope.Request) {
	h = newHarness(t, Config{})
	a = h.sendFrom(grokAddr, "session-a", "muse", "from A")
	b = h.sendFrom(grokAddr, "session-b", "muse", "from B")
	for _, id := range []string{a.ID, b.ID} {
		h.do(museAddr, "POST", "/v1/requests/"+id+"/claim", "", http.StatusOK, nil)
		h.do(museAddr, "POST", "/v1/requests/"+id+"/reply", `{"body":"done"}`, http.StatusOK, nil)
	}
	return h, a, b
}

// Two sessions share one agent name. Each sends its own request and each
// gets an answer. A poll from session A returns only A's reply, and once A
// has taken it, B still gets its own.
func TestSessionPollReturnsOnlyItsOwnReplies(t *testing.T) {
	h, a, b := twoAnsweredSessions(t)
	if got := h.pollReplies(grokAddr, "session-a", client.RepliesKeep); !slices.Equal(got, []string{a.ID}) {
		t.Fatalf("session A poll returned replies %v, want only %s (B's is %s)", got, a.ID, b.ID)
	}
	if got := h.pollReplies(grokAddr, "session-a", client.RepliesTake); !slices.Equal(got, []string{a.ID}) {
		t.Fatalf("session A take = %v, want [%s]", got, a.ID)
	}
	h.do(grokAddr, "POST", "/v1/replies/ack", `{"ids":["`+a.ID+`"]}`, http.StatusNoContent, nil)
	if got := h.pollReplies(grokAddr, "session-b", client.RepliesTake); !slices.Equal(got, []string{b.ID}) {
		t.Fatalf("session B take = %v, want [%s]", got, b.ID)
	}
}

// A caller that names no session (tincan inbox) sees every reply, and the
// relay's agent-wide count, which wake uses, counts them all.
func TestSessionlessPollSeesEveryReply(t *testing.T) {
	h, a, b := twoAnsweredSessions(t)
	got := h.pollReplies(grokAddr, "", client.RepliesKeep)
	slices.Sort(got)
	want := []string{a.ID, b.ID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("sessionless poll = %v, want %v", got, want)
	}
	if n := h.srv.UnseenReplies("grokbot"); n != 2 {
		t.Fatalf("agent-wide unseen = %d, want 2", n)
	}
}

// A peek from a session reports only that session's replies, so the
// channel notice does not fire in a session nothing is waiting for.
func TestSessionPeekCountsOnlyItsOwnReplies(t *testing.T) {
	h := newHarness(t, Config{})
	a := h.sendFrom(grokAddr, "session-a", "muse", "from A")
	h.do(museAddr, "POST", "/v1/requests/"+a.ID+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", "/v1/requests/"+a.ID+"/reply", `{"body":"done"}`, http.StatusOK, nil)
	for session, want := range map[string]int{"session-a": 1, "session-b": 0, "": 1} {
		rec := h.raw(grokAddr, "GET", "/v1/poll?peek=1&replies=keep&hold=0", nil, 0, sessionHeader(session))
		var got peekResult
		if rec.Code == http.StatusNoContent { // nothing waiting
			if want != 0 {
				t.Errorf("peek from session %q: nothing waiting, want %d", session, want)
			}
			continue
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("peek from %q: status %d: %s", session, rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Waiting != want {
			t.Errorf("peek from session %q: waiting %d, want %d", session, got.Waiting, want)
		}
	}
}

// A reply stays readable by id from any session: a session only narrows
// what a poll hands out.
func TestSessionGetByIDIsUnscoped(t *testing.T) {
	h, a, _ := twoAnsweredSessions(t)
	rec := h.raw(grokAddr, "GET", "/v1/requests/"+a.ID, nil, 0, sessionHeader("session-b"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done"`) {
		t.Fatalf("get A's reply from session B: %d %s", rec.Code, rec.Body)
	}
}

// A malformed session is refused rather than ignored.
func TestSessionHeaderValidated(t *testing.T) {
	h := newHarness(t, Config{})
	for _, bad := range []string{strings.Repeat("x", 65), "a/b", "a b"} {
		hdr := sessionHeader(bad)
		if rec := h.raw(grokAddr, "POST", "/v1/send", strings.NewReader(`{"to":"muse","body":"hi"}`), -1, hdr); rec.Code != http.StatusBadRequest {
			t.Errorf("send with session %q: status %d, want 400", bad, rec.Code)
		}
		if rec := h.raw(grokAddr, "GET", "/v1/poll?hold=0", nil, 0, hdr); rec.Code != http.StatusBadRequest {
			t.Errorf("poll with session %q: status %d, want 400", bad, rec.Code)
		}
	}
	h.sendFrom(grokAddr, strings.Repeat("x", 64), "muse", "longest allowed")
	h.sendFrom(grokAddr, "Mcp-1.2_3", "muse", "every allowed character")
}

// A reply to a session that stops calling the relay (its window closed, or
// its MCP server restarted and took a new id) is not stranded: once that
// session has been quiet for SessionGrace, the next session to poll gets
// it. While the asking session keeps calling, its reply stays its own.
func TestReplyToGoneSessionOpensAfterGrace(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{})
	clk.advance(SessionGrace) // past the just-started window
	a := h.sendFrom(grokAddr, "session-a", "muse", "from A")
	h.do(museAddr, "POST", "/v1/requests/"+a.ID+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", "/v1/requests/"+a.ID+"/reply", `{"body":"done"}`, http.StatusOK, nil)

	clk.advance(SessionGrace - time.Minute)
	if got := h.pollReplies(grokAddr, "session-b", client.RepliesKeep); len(got) != 0 {
		t.Fatalf("session B saw %v while A was still live", got)
	}
	h.pollReplies(grokAddr, "session-a", client.RepliesKeep) // A is still running
	clk.advance(SessionGrace - time.Minute)
	if got := h.pollReplies(grokAddr, "session-b", client.RepliesKeep); len(got) != 0 {
		t.Fatalf("session B saw %v though A polled %s ago", got, SessionGrace-time.Minute)
	}

	clk.advance(2 * time.Minute) // A has now been quiet past SessionGrace
	if got := h.pollReplies(grokAddr, "session-b", client.RepliesTake); !slices.Equal(got, []string{a.ID}) {
		t.Fatalf("session B take = %v after A went quiet, want [%s]", got, a.ID)
	}
}

// Session activity is in memory only, so right after the relay starts it
// cannot tell a quiet session from a gone one: for SessionGrace every other
// session's replies stay with that session.
func TestReplyScopeKeepsEverySessionRightAfterStart(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{})
	if got := h.srv.replyScope("grokbot", "session-b"); !got.AllLive {
		t.Fatalf("scope just after start = %+v, want AllLive", got)
	}
	if got := h.srv.replyScope("grokbot", ""); got.AllLive || got.Session != "" {
		t.Fatalf("sessionless scope = %+v, want every reply", got)
	}
	h.srv.touchSession("grokbot", "session-a")
	clk.advance(SessionGrace)
	h.srv.touchSession("grokbot", "session-c")
	got := h.srv.replyScope("grokbot", "session-b")
	if got.AllLive || !slices.Equal(got.Live, []string{"session-c"}) {
		t.Fatalf("scope after grace = %+v, want only session-c live", got)
	}
}
