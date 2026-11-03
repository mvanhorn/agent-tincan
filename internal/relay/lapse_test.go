package relay

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// A claimant that claims and never replies has its ask requeued once, fresh
// (same id, sender and chain, the lapse mark cleared) with one note to the
// asker, and offered to it again on its next poll.
func TestLapsedClaimRequeuedAndReoffered(t *testing.T) {
	h, clk, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "summarize this paper")
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(DefaultClaimLease + time.Second)
	h.srv.Sweep(t.Context())

	if st := statusOf(t, h, grokAddr, sent.ID); st != envelope.StatusQueued {
		t.Fatalf("status after the lapse = %s, want queued", st)
	}
	if lapsed, err := h.st.ClaimLapsed(t.Context(), sent.ID); err != nil || lapsed {
		t.Fatalf("claim_lapsed = %v, %v; want cleared", lapsed, err)
	}
	if a := agentInfo(t, h, macAddr, "muse"); a.StatusReason == "claim lapsed" {
		t.Fatalf("still stuck on the lapse: %+v", a)
	}
	got := inbox(t, h, grokAddr)
	if len(got) != 1 || got[0].From != "relay" || !strings.Contains(got[0].Body, sent.ID) ||
		!strings.Contains(got[0].Body, "muse claimed this but did not answer; requeued") {
		t.Fatalf("asker inbox = %+v", got)
	}
	h.srv.Sweep(t.Context())
	if got := inbox(t, h, grokAddr); len(got) != 0 {
		t.Fatalf("told twice: %+v", got)
	}

	offered := inbox(t, h, museAddr)
	if len(offered) != 1 || offered[0].ID != sent.ID || offered[0].From != "grokbot" {
		t.Fatalf("muse was offered %+v, want %s from grokbot again", offered, sent.ID)
	}
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	if st := statusOf(t, h, grokAddr, sent.ID); st != envelope.StatusClaimed {
		t.Fatalf("status after reclaim = %s", st)
	}
}

// A second lapse fails the ask to its asker with a typed reply, the agent is
// not left stuck on it, and it returns to idle.
func TestSecondLapseFailsToSender(t *testing.T) {
	h, clk, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "summarize this paper")
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	clk.advance(DefaultClaimLease + time.Second)
	h.srv.Sweep(t.Context())
	if st := statusOf(t, h, grokAddr, sent.ID); st != envelope.StatusQueued {
		t.Fatalf("first lapse: status %s, want queued", st)
	}
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	claim2 := clk.Now().Truncate(time.Millisecond).UTC()
	clk.advance(DefaultClaimLease + time.Second)
	h.srv.Sweep(t.Context())

	var res envelope.Result
	h.do(grokAddr, "GET", "/v1/requests/"+sent.ID, "", http.StatusOK, &res)
	want := "failed: claim_lapsed (muse claimed at " + claim2.Format(time.RFC3339) + ", no reply by " + claim2.Add(DefaultClaimLease).Format(time.RFC3339) + ")"
	if res.Status != envelope.StatusFailed || res.Reply == nil || res.Reply.Status != envelope.StatusFailed || res.Reply.Body != want {
		t.Fatalf("result = %s %+v, want failed with %q", res.Status, res.Reply, want)
	}
	listen(h, museAddr)
	if a := agentInfo(t, h, macAddr, "muse"); a.Status != client.StatusIdle || a.StatusRequest != "" {
		t.Fatalf("muse after the second lapse: %+v, want idle", a)
	}
}

// A claim that reported progress before it lapsed may be partly done, so it
// fails at the first lapse rather than being requeued.
func TestLapseAfterProgressFails(t *testing.T) {
	h, clk, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "long job")
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/progress", `{"note":"started"}`, http.StatusOK, nil)
	clk.advance(DefaultClaimLease + time.Second)
	h.srv.Sweep(t.Context())
	var res envelope.Result
	h.do(grokAddr, "GET", "/v1/requests/"+sent.ID, "", http.StatusOK, &res)
	if res.Status != envelope.StatusFailed || res.Reply == nil || !strings.HasPrefix(res.Reply.Body, "failed: claim_lapsed (muse claimed at ") {
		t.Fatalf("result = %s %+v", res.Status, res.Reply)
	}
}

// A stuck agent's roster entry carries the id of the request holding it up,
// and so does a busy agent's.
func TestRosterNamesStuckAndClaimedRequest(t *testing.T) {
	h, clk, _ := statusHarness(t)
	sent := h.send(grokAddr, "muse", "draft the note")
	if a := agentInfo(t, h, macAddr, "muse"); a.Status != client.StatusStuck || a.StatusRequest != sent.ID ||
		!strings.Contains(a.StatusField(clk.Now()), " request="+sent.ID) {
		t.Fatalf("stuck muse = %+v", a)
	}
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", http.StatusOK, nil)
	if a := agentInfo(t, h, macAddr, "muse"); a.Status != client.StatusBusy || a.StatusRequest != sent.ID ||
		!strings.Contains(a.StatusField(clk.Now()), " request="+sent.ID) {
		t.Fatalf("busy muse = %+v", a)
	}
}
