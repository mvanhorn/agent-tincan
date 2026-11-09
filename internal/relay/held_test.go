package relay

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// sweepHarness is a harness whose relay and store share a fake clock, with a
// recorder for queue events.
func sweepHarness(t *testing.T, cfg Config) (*harness, *fakeClock, *queuedRecorder) {
	t.Helper()
	clk := &fakeClock{t: time.Now()}
	cfg.Now = clk.Now
	h := newHarness(t, cfg)
	h.st.SetClock(clk.Now)
	rec := &queuedRecorder{}
	h.srv.SetEvents(rec)
	return h, clk, rec
}

func heldOf(t *testing.T, rec *http.Response) []envelope.Held {
	t.Helper()
	v := rec.Header.Get(client.HeldHeader)
	if v == "" {
		return nil
	}
	var held []envelope.Held
	if err := json.Unmarshal([]byte(v), &held); err != nil {
		t.Fatalf("held header %q: %v", v, err)
	}
	return held
}

// An urgent claim gets the urgent lease and each progress note renews it by
// that lease; a routine claim keeps ClaimLease. When the urgent claim runs
// out it is requeued and the asker is told once; the routine one later.
func TestUrgentClaimLeaseAndStaleClaimNotice(t *testing.T) {
	h, clk, rec := sweepHarness(t, Config{})
	var urgent envelope.Request
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"grokbot","body":"use the code before it expires","urgent":true}`, http.StatusCreated, &urgent)
	routine := h.send(museAddr, "grokbot", "whenever")
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil)
	h.do(grokAddr, "POST", "/v1/requests/"+urgent.ID+"/claim", "", http.StatusOK, nil)
	h.do(grokAddr, "POST", "/v1/requests/"+routine.ID+"/claim", "", http.StatusOK, nil)

	clk.advance(9 * time.Minute)
	h.do(grokAddr, "POST", "/v1/requests/"+urgent.ID+"/progress", `{"note":"on it"}`, http.StatusOK, nil)
	clk.advance(9 * time.Minute)
	h.srv.Sweep(t.Context())
	if st := statusOf(t, h, instinctAddr, urgent.ID); st != envelope.StatusClaimed {
		t.Fatalf("18m, progress at 9m: urgent status %s, want claimed", st)
	}
	clk.advance(2 * time.Minute)
	h.srv.Sweep(t.Context())
	if st := statusOf(t, h, instinctAddr, urgent.ID); st != envelope.StatusQueued {
		t.Fatalf("20m, 11m since progress: urgent status %s, want queued", st)
	}
	if st := statusOf(t, h, museAddr, routine.ID); st != envelope.StatusClaimed {
		t.Fatalf("20m: routine status %s, want still claimed", st)
	}
	got := inbox(t, h, instinctAddr)
	if len(got) != 1 || got[0].From != "relay" || !strings.Contains(got[0].Body, urgent.ID) ||
		!strings.Contains(got[0].Body, "grokbot claimed this 20m ago and did not reply or post progress before its claim lease ran out; it has been requeued.") ||
		!strings.HasSuffix(got[0].Body, " Sent by the relay at "+client.UTCClock(clk.Now(), clk.Now())+". No reply needed.") {
		t.Fatalf("asker inbox = %+v", got)
	}
	rec.mu.Lock()
	if n := len(rec.to); n < 2 || rec.to[n-2] != "instinct" || rec.to[n-1] != "grokbot" {
		t.Errorf("queue events = %v, want the asker told and the target woken again", rec.to)
	}
	rec.mu.Unlock()
	var res envelope.Result
	h.do(instinctAddr, "GET", "/v1/requests/"+urgent.ID, "", http.StatusOK, &res)
	if res.RelayNote == nil || !strings.Contains(res.RelayNote.Note, "claimed this 20m ago") {
		t.Fatalf("relay note = %+v", res.RelayNote)
	}
	h.srv.Sweep(t.Context())
	if got := inbox(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told twice: %+v", got)
	}

	clk.advance(11 * time.Minute)
	h.srv.Sweep(t.Context())
	if got := inbox(t, h, museAddr); len(got) != 1 || !strings.Contains(got[0].Body, routine.ID) || !strings.Contains(got[0].Body, "claimed this 31m ago") {
		t.Fatalf("routine asker inbox = %+v", got)
	}
}

// A request that expires unanswered tells its asker once who held it. A
// notify that expires tells nobody.
func TestExpiredRequestNotice(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{RequestTTL: time.Hour})
	ask := h.send(instinctAddr, "grokbot", "anyone?")
	claimed := h.send(museAddr, "grokbot", "take this")
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"grokbot","body":"fyi","kind":"notify"}`, http.StatusCreated, nil)
	h.do(grokAddr, "POST", "/v1/requests/"+claimed.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(61 * time.Minute)
	h.srv.Sweep(t.Context())

	// Expiry notices are not linked to their request, so nothing closes them.
	for _, n := range append(relayNotices(h, "instinct"), relayNotices(h, "muse")...) {
		if n.NoticeFor != "" || n.NoticeKind != "" {
			t.Fatalf("expiry notice linked: %+v", n)
		}
	}
	got := inbox(t, h, instinctAddr)
	if len(got) != 1 || !strings.Contains(got[0].Body, ask.ID) || !strings.Contains(got[0].Body, "This request expired after 1h1m with no reply. grokbot never picked it up.") {
		t.Fatalf("asker inbox = %+v", got)
	}
	var res envelope.Result
	h.do(instinctAddr, "GET", "/v1/requests/"+ask.ID, "", http.StatusOK, &res)
	if res.Status != envelope.StatusExpired || res.RelayNote == nil || !strings.Contains(res.RelayNote.Note, "never picked it up") {
		t.Fatalf("get = %s %+v", res.Status, res.RelayNote)
	}
	got = inbox(t, h, museAddr)
	if len(got) != 1 || !strings.Contains(got[0].Body, "grokbot claimed it 1h1m ago and never replied.") {
		t.Fatalf("claimed asker inbox = %+v", got)
	}
	clk.advance(25 * time.Hour) // the notices themselves expire; nobody is told about those
	h.srv.Sweep(t.Context())
	for _, a := range []string{instinctAddr, museAddr} {
		if got := inbox(t, h, a); len(got) != 0 {
			t.Fatalf("told again: %+v", got)
		}
	}
}

// The held header lists the caller's claimed, unreplied asks on any of its
// calls, is computed after the call's own work, and is absent for others.
func TestHeldHeader(t *testing.T) {
	h, _, _ := sweepHarness(t, Config{})
	var urgent envelope.Request
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"grokbot","body":"now","urgent":true}`, http.StatusCreated, &urgent)
	if held := heldOf(t, h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil).Result()); held != nil {
		t.Fatalf("delivered, not claimed: held %+v", held)
	}
	if held := heldOf(t, h.do(grokAddr, "POST", "/v1/requests/"+urgent.ID+"/claim", "", http.StatusOK, nil).Result()); len(held) != 1 {
		t.Fatalf("claim response held %+v, want the claim itself", held)
	}
	held := heldOf(t, h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, nil).Result())
	if len(held) != 1 || held[0].ID != urgent.ID || held[0].From != "instinct" || !held[0].Urgent || held[0].ClaimedAt.IsZero() {
		t.Fatalf("held = %+v", held)
	}
	if held := heldOf(t, h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil).Result()); len(held) != 1 {
		t.Fatalf("204 poll held = %+v", held)
	}
	if held := heldOf(t, h.do(instinctAddr, "GET", "/v1/agents", "", http.StatusOK, nil).Result()); held != nil {
		t.Fatalf("asker sees held %+v", held)
	}
	if held := heldOf(t, h.do(grokAddr, "POST", "/v1/requests/"+urgent.ID+"/reply", `{"body":"done"}`, http.StatusOK, nil).Result()); held != nil {
		t.Fatalf("reply response held %+v, want none", held)
	}
}

// lapsedClaim sends instinct's ask to grokbot, has grokbot claim it and
// lets the claim lease run out, so the sweep requeues it and queues the
// stale-claim notice for instinct.
func lapsedClaim(t *testing.T, h *harness, clk *fakeClock) envelope.Request {
	t.Helper()
	req := h.send(instinctAddr, "grokbot", "find the code")
	h.do(grokAddr, "POST", "/v1/requests/"+req.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(31 * time.Minute)
	h.srv.Sweep(t.Context())
	if n := relayNotices(h, "instinct"); len(n) != 1 || !strings.Contains(n[0].Body, "claim lease ran out") {
		t.Fatalf("queued notices = %+v", n)
	}
	return req
}

// superseded reports whether the audit log shows notice id closed unsent.
func superseded(t *testing.T, h *harness, id string) bool {
	t.Helper()
	events, err := h.st.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Event == "notice_superseded" && e.RequestID == id {
			return true
		}
	}
	return false
}

// The target replies after its claim lapsed and before the asker polls:
// the asker gets the reply and never the stale-claim notice.
func TestStaleClaimNoticeSupersededByReply(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{})
	req := lapsedClaim(t, h, clk)
	notice := relayNotices(h, "instinct")[0]
	h.do(grokAddr, "POST", "/v1/requests/"+req.ID+"/reply", `{"body":"4417"}`, http.StatusOK, nil)
	if got := inbox(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("asker inbox after the reply = %+v", got)
	}
	var res envelope.Result
	h.do(instinctAddr, "GET", "/v1/requests/"+req.ID, "", http.StatusOK, &res)
	if res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.Body != "4417" {
		t.Fatalf("get = %s %+v", res.Status, res.Reply)
	}
	if !superseded(t, h, notice.ID) {
		t.Fatal("no notice_superseded audit event")
	}
}

// An asker that polls before the reply reads the notice, which says when
// the relay made it, and still gets the reply after.
func TestStaleClaimNoticeDeliveredBeforeReply(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{})
	req := lapsedClaim(t, h, clk)
	got := inbox(t, h, instinctAddr)
	if len(got) != 1 {
		t.Fatalf("asker inbox = %+v", got)
	}
	want := " Sent by the relay at " + client.UTCClock(got[0].CreatedAt, got[0].CreatedAt) + ". No reply needed."
	if !strings.HasSuffix(got[0].Body, want) {
		t.Fatalf("notice body = %q, want suffix %q", got[0].Body, want)
	}
	h.do(grokAddr, "POST", "/v1/requests/"+req.ID+"/reply", `{"body":"4417"}`, http.StatusOK, nil)
	if st := statusOf(t, h, instinctAddr, req.ID); st != envelope.StatusAnswered {
		t.Fatalf("status %s, want answered", st)
	}
	if superseded(t, h, got[0].ID) {
		t.Fatal("a delivered notice was marked superseded")
	}
}

// Cancelling the ask before polling closes its queued notice.
func TestStaleClaimNoticeClosedByCancel(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{})
	req := lapsedClaim(t, h, clk)
	notice := relayNotices(h, "instinct")[0]
	h.do(instinctAddr, "POST", "/v1/requests/"+req.ID+"/cancel", "", http.StatusOK, nil)
	if got := inbox(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("asker inbox after cancel = %+v", got)
	}
	if !superseded(t, h, notice.ID) {
		t.Fatal("no notice_superseded audit event")
	}
}

// A reclaim and a progress note after the lapse, with no reply yet, leave
// the notice to be delivered.
func TestStaleClaimNoticeSurvivesReclaim(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{})
	req := lapsedClaim(t, h, clk)
	h.do(grokAddr, "POST", "/v1/requests/"+req.ID+"/claim", "", http.StatusOK, nil)
	h.do(grokAddr, "POST", "/v1/requests/"+req.ID+"/progress", `{"note":"back on it"}`, http.StatusOK, nil)
	if got := inbox(t, h, instinctAddr); len(got) != 1 || !strings.Contains(got[0].Body, req.ID) {
		t.Fatalf("asker inbox = %+v", got)
	}
}

// A reply that lands between the sweep's requeue and the notice being
// built means no notice is queued.
func TestStaleClaimNoticeSkippedWhenReplyRaces(t *testing.T) {
	h, clk, _ := sweepHarness(t, Config{})
	req := h.send(instinctAddr, "grokbot", "find the code")
	h.do(grokAddr, "POST", "/v1/requests/"+req.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(31 * time.Minute)
	trs, err := h.st.Sweep(t.Context())
	if err != nil || len(trs) != 1 {
		t.Fatalf("sweep %+v %v", trs, err)
	}
	h.do(grokAddr, "POST", "/v1/requests/"+req.ID+"/reply", `{"body":"4417"}`, http.StatusOK, nil)
	h.srv.tellSwept(t.Context(), trs[0])
	if n := relayNotices(h, "instinct"); len(n) != 0 {
		t.Fatalf("queued notices = %+v", n)
	}
	var res envelope.Result
	h.do(instinctAddr, "GET", "/v1/requests/"+req.ID, "", http.StatusOK, &res)
	if res.RelayNote != nil {
		t.Fatalf("relay note = %+v", res.RelayNote)
	}
}
