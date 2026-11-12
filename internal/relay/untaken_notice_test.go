package relay

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// untakenHarness is noticeHarness with muse on a Claude Code channel and
// the store on the fake clock, so the sweep and the store agree on time.
func untakenHarness(t *testing.T) (*harness, *fakeClock, *fakeWaker) {
	t.Helper()
	h, clk, fw, _ := noticeHarness(t)
	h.st.SetClock(clk.Now)
	fw.methods["muse"] = "channel"
	return h, clk, fw
}

// untakenNotices polls addr and returns the untaken notices it was
// delivered.
func untakenNotices(t *testing.T, h *harness, addr string) []envelope.Request {
	t.Helper()
	var out []envelope.Request
	for _, r := range inbox(t, h, addr) {
		if r.From == "relay" && strings.Contains(r.Body, "has not taken this request") {
			out = append(out, r)
		}
	}
	return out
}

// An ask to a channel agent that no session takes tells its asker once,
// after a full grace: nothing is working on it, busy or online on the
// roster does not mean otherwise, and a channel reaches only an idle
// session started with channels. Later sweeps and a restarted relay do not
// tell again.
func TestUntakenNoticeTellsAskerOnce(t *testing.T) {
	h, clk, fw := untakenHarness(t)
	ctx := context.Background()
	ask := h.send(instinctAddr, "muse", "review the deploy plan")

	clk.advance(5 * time.Minute)
	pollAll(t, h, grokAddr, strangerAddr)
	h.srv.Sweep(ctx)
	if got := untakenNotices(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told before a full grace: %+v", got)
	}

	clk.advance(DefaultWakeGrace)
	pollAll(t, h, grokAddr, strangerAddr)
	h.srv.Sweep(ctx)
	got := untakenNotices(t, h, instinctAddr)
	if len(got) != 1 {
		t.Fatalf("asker notices = %+v, want one", got)
	}
	n := got[0]
	if n.Kind != envelope.KindNotify || !strings.Contains(n.Body, ask.ID) || !strings.Contains(n.Body, "review the deploy plan") {
		t.Fatalf("notice = %+v", n)
	}
	for _, want := range []string{
		"muse has not taken this request.",
		"It is still queued and nothing is working on it yet",
		"muse showing as busy or online does not mean it is working on this request.",
		"a Claude Code session sees a channel notice only while it is idle and was started with channels.",
		"cancel it and ask a teammate who is online and good at this: grokbot, scout.",
	} {
		if !strings.Contains(n.Body, want) {
			t.Errorf("notice lacks %q: %s", want, n.Body)
		}
	}
	var res envelope.Result
	h.do(instinctAddr, "GET", "/v1/requests/"+ask.ID, "", http.StatusOK, &res)
	if res.Status != envelope.StatusQueued || res.RelayNote == nil || !strings.Contains(res.RelayNote.Note, "has not taken this request") {
		t.Fatalf("asker's get = status %s, note %+v", res.Status, res.RelayNote)
	}

	// A notify the asker polled but never took comes back to it, so a
	// second sight of the same notice is not a second notice.
	clk.advance(DefaultWakeGrace)
	h.srv.Sweep(ctx)
	h.srv.Sweep(ctx)
	for _, r := range untakenNotices(t, h, instinctAddr) {
		if r.ID != n.ID {
			t.Fatalf("told again on a later sweep: %s", r.Body)
		}
	}

	srv := New(identity.NewDirectory(h.st, h.who, identity.Config{Admins: []string{"macbook-pro-44"}}), h.st, Config{Now: clk.Now})
	srv.SetWakeNamer(fw)
	h2 := &harness{t: t, srv: srv, h: srv.Handler(), st: h.st, who: h.who}
	srv.Sweep(ctx)
	for _, r := range untakenNotices(t, h2, instinctAddr) {
		if r.ID != n.ID {
			t.Fatalf("told again after a restart: %s", r.Body)
		}
	}
	if c := untakenNoteEvents(t, h.st); c != 1 {
		t.Fatalf("untaken relay notes recorded = %d, want 1", c)
	}
}

// A wait agent gets the note without the channel sentence, and an urgent
// ask is told after the urgent grace.
func TestUntakenNoticeUrgentAndNotChannel(t *testing.T) {
	h, clk, fw := untakenHarness(t)
	fw.methods["muse"] = "wait"
	var urgent envelope.Request
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"muse","body":"now","urgent":true}`, http.StatusCreated, &urgent)
	clk.advance(DefaultUrgentWakeGrace + time.Second)
	h.srv.Sweep(context.Background())
	got := untakenNotices(t, h, instinctAddr)
	if len(got) != 1 || !strings.Contains(got[0].Body, urgent.ID) {
		t.Fatalf("urgent asker notices = %+v", got)
	}
	if strings.Contains(got[0].Body, "channel") {
		t.Fatalf("a wait agent's note talks about channels: %s", got[0].Body)
	}
}

// No note for an ask the relay is holding for approval; once approved, the
// grace runs from the approval.
func TestUntakenNoticeNotWhileHeld(t *testing.T) {
	h, clk, _ := untakenHarness(t)
	h.srv.SetPreparer(holdingPreparer{})
	ctx := context.Background()
	held := h.send(instinctAddr, "muse", "x")
	if held.Status != envelope.StatusHeld {
		t.Fatalf("ask = %+v, want held", held)
	}
	clk.advance(2 * DefaultWakeGrace)
	h.srv.Sweep(ctx)
	if got := untakenNotices(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told about a held ask: %+v", got)
	}

	h.do(macAddr, "POST", "/v1/admin/requests/"+held.ID+"/approve", "", http.StatusOK, nil)
	clk.advance(DefaultWakeGrace / 2)
	h.srv.Sweep(ctx)
	if got := untakenNotices(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told inside a grace from the approval: %+v", got)
	}
	clk.advance(DefaultWakeGrace)
	h.srv.Sweep(ctx)
	if got := untakenNotices(t, h, instinctAddr); len(got) != 1 {
		t.Fatalf("approved ask notices = %+v, want one", got)
	}
}

// holdingPreparer holds every request for an hour.
type holdingPreparer struct{ newChain }

func (p holdingPreparer) Prepare(ctx context.Context, req *envelope.Request) error {
	if err := p.newChain.Prepare(ctx, req); err != nil {
		return err
	}
	req.Status, req.HoldTTL = envelope.StatusHeld, time.Hour
	return nil
}

// An ask TellAskers already noted, and a self-ask, get no untaken note.
func TestUntakenNoticeSkipsWakeNotedAndSelf(t *testing.T) {
	h, clk, _ := untakenHarness(t)
	ctx := context.Background()
	ask := h.send(instinctAddr, "muse", "x")
	if _, err := h.st.AddRelayNote(ctx, ask.ID, store.NoteWake, 0, "muse was woken", clk.Now()); err != nil {
		t.Fatal(err)
	}
	self, err := h.st.Enqueue(ctx, envelope.Request{From: "muse", To: "muse", Kind: envelope.KindAsk, Hop: 1, Chain: []string{"muse"}, Body: "note to self"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * DefaultWakeGrace)
	h.srv.Sweep(ctx)
	if got := untakenNotices(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told about an ask already noted: %+v", got)
	}
	if n, err := h.st.RelayNote(ctx, self.ID); err != nil || n != nil {
		t.Fatalf("self-ask note = %+v, %v", n, err)
	}
}

// Agents the relay wakes, and schedule agents, have their own follow-ups:
// the sweep tells nobody about them.
func TestUntakenNoticeOnlyForAgentSideWakes(t *testing.T) {
	h, clk, fw := untakenHarness(t)
	fw.methods["scout"] = "schedule"
	h.send(instinctAddr, "grokbot", "webhook agent")
	h.send(instinctAddr, "scout", "schedule agent")
	clk.advance(2 * DefaultWakeGrace)
	h.srv.Sweep(context.Background())
	if got := untakenNotices(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told about a relay-woken or schedule agent: %+v", got)
	}
}

// untakenNoteEvents counts the relay_note audit events of kind untaken.
func untakenNoteEvents(t *testing.T, st *store.Store) int {
	t.Helper()
	events, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Event == "relay_note" && strings.Contains(e.Detail, `"kind":"untaken"`) {
			n++
		}
	}
	return n
}
