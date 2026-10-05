package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// noticeHarness is wakeHarness plus scout, a fourth agent, and a recorder
// for queue events.
func noticeHarness(t *testing.T) (*harness, *fakeClock, *fakeWaker, *queuedRecorder) {
	t.Helper()
	h, clk, fw := wakeHarness(t)
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"scout"}`, http.StatusOK, &inv)
	h.do(strangerAddr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	rec := &queuedRecorder{}
	h.srv.SetEvents(rec)
	return h, clk, fw, rec
}

// pollAll has each address poll once, so it counts as online now.
func pollAll(t *testing.T, h *harness, addrs ...string) {
	t.Helper()
	for _, a := range addrs {
		inbox(t, h, a)
	}
}

// inbox polls addr once and returns the requests it was delivered.
func inbox(t *testing.T, h *harness, addr string) []envelope.Request {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/poll?hold=0", nil)
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	switch rec.Code {
	case http.StatusNoContent:
		return nil
	case http.StatusOK:
	default:
		t.Fatalf("poll from %s: status %d: %s", addr, rec.Code, rec.Body.String())
	}
	var got pollResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Requests
}

func TestWakeNoticeTellsAskerOnce(t *testing.T) {
	h, clk, fw, rec := noticeHarness(t)
	ask := h.send(instinctAddr, "grokbot", "book the 3pm slot, codes expire soon")
	woke := clk.Now()
	fw.set("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})

	clk.advance(5 * time.Minute)
	pollAll(t, h, museAddr, strangerAddr, instinctAddr)
	h.srv.TellAskers("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	if got := inbox(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told before a full grace: %+v", got)
	}

	clk.advance(DefaultWakeGrace)
	pollAll(t, h, museAddr, strangerAddr, instinctAddr)
	h.srv.TellAskers("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	got := inbox(t, h, instinctAddr)
	if len(got) != 1 {
		t.Fatalf("asker inbox = %+v, want one notice", got)
	}
	n := got[0]
	if n.From != "relay" || n.Kind != envelope.KindNotify || !strings.Contains(n.Body, ask.ID) || !strings.Contains(n.Body, "book the 3pm slot") {
		t.Fatalf("notice = %+v", n)
	}
	for _, want := range []string{"grokbot was woken at", "has not checked in (webhook ok)", "The request is still queued.", "cancel it and ask a teammate who is online and good at this: muse, scout."} {
		if !strings.Contains(n.Body, want) {
			t.Errorf("notice lacks %q: %s", want, n.Body)
		}
	}
	rec.mu.Lock()
	if len(rec.to) != 2 || rec.to[1] != "instinct" { // the ask itself, then the notice
		t.Errorf("queue events = %v, want the ask and then the asker woken once", rec.to)
	}
	rec.mu.Unlock()

	var res envelope.Result
	h.do(instinctAddr, "GET", "/v1/requests/"+ask.ID, "", http.StatusOK, &res)
	if res.Status != envelope.StatusQueued || res.RelayNote == nil || res.RelayNote.By != "relay" || !strings.Contains(res.RelayNote.Note, "muse, scout") {
		t.Fatalf("asker's get = status %s, note %+v", res.Status, res.RelayNote)
	}
	var target envelope.Result
	h.do(grokAddr, "GET", "/v1/requests/"+ask.ID, "", http.StatusOK, &target)
	if target.RelayNote != nil {
		t.Fatalf("target sees the note: %+v", target.RelayNote)
	}

	clk.advance(DefaultWakeGrace)
	h.srv.TellAskers("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	if got := inbox(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told twice: %+v", got)
	}

	// A restarted relay has the same store and does not tell again.
	srv := New(identity.NewDirectory(h.st, h.who, identity.Config{Admins: []string{"macbook-pro-44"}}), h.st, Config{Now: clk.Now})
	srv.SetWakeNamer(fw)
	srv.SetEvents(rec)
	h2 := &harness{t: t, srv: srv, h: srv.Handler(), st: h.st, who: h.who}
	srv.TellAskers("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	if got := inbox(t, h2, instinctAddr); len(got) != 0 {
		t.Fatalf("told again after a restart: %+v", got)
	}
}

// An agent that polled after its wake has checked in: nobody is told.
func TestWakeNoticeNotAfterPoll(t *testing.T) {
	h, clk, fw, _ := noticeHarness(t)
	ask := h.send(instinctAddr, "grokbot", "x")
	woke := clk.Now()
	fw.set("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	clk.advance(time.Minute)
	h.do(grokAddr, "GET", "/v1/poll?hold=0&peek=1", "", http.StatusOK, nil)
	clk.advance(DefaultWakeGrace)
	h.srv.TellAskers("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	if got := inbox(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told after the target polled: %+v", got)
	}
	if n, err := h.st.RelayNote(context.Background(), ask.ID); err != nil || n != nil {
		t.Fatalf("note = %+v, %v", n, err)
	}
}

// An urgent ask is noticed after the urgent grace; a routine one queued
// alongside it waits its full grace, and a notify is never noticed. With no
// one else online, the notice says so. A failed wake shows its error.
func TestWakeNoticeUrgentGraceAndNoOneOnline(t *testing.T) {
	h, clk, fw, _ := noticeHarness(t)
	var urgent envelope.Request
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"grokbot","body":"now","urgent":true}`, http.StatusCreated, &urgent)
	routine := h.send(museAddr, "grokbot", "later")
	h.do(strangerAddr, "POST", "/v1/send", `{"to":"grokbot","body":"fyi","kind":"notify"}`, http.StatusCreated, nil)
	woke := clk.Now()
	wk := store.Wake{At: woke, Result: "hooks.example returned 502 Bad Gateway"}
	fw.set("grokbot", wk)

	clk.advance(DefaultUrgentWakeGrace + time.Second)
	h.srv.TellAskers("grokbot", wk)
	got := inbox(t, h, instinctAddr)
	if len(got) != 1 || !strings.Contains(got[0].Body, urgent.ID) {
		t.Fatalf("urgent asker inbox = %+v", got)
	}
	for _, want := range []string{"(webhook failed: hooks.example returned 502 Bad Gateway)", "none is online right now"} {
		if !strings.Contains(got[0].Body, want) {
			t.Errorf("notice lacks %q: %s", want, got[0].Body)
		}
	}
	if got := inbox(t, h, museAddr); len(got) != 0 {
		t.Fatalf("routine asker told inside its grace: %+v", got)
	}
	clk.advance(DefaultWakeGrace)
	h.srv.TellAskers("grokbot", wk)
	if got := inbox(t, h, museAddr); len(got) != 1 || !strings.Contains(got[0].Body, routine.ID) {
		t.Fatalf("routine asker inbox = %+v", got)
	}
	if got := inbox(t, h, strangerAddr); len(got) != 0 {
		t.Fatalf("notify sender told: %+v", got)
	}
}

// Only relay-woken agents are judged: a wait agent's silence tells nobody.
func TestWakeNoticeOnlyForRelayWoken(t *testing.T) {
	h, clk, fw, _ := noticeHarness(t)
	h.send(instinctAddr, "muse", "x")
	wk := store.Wake{At: clk.Now(), Result: envelope.WakeOK}
	fw.set("muse", wk)
	clk.advance(time.Hour)
	h.srv.TellAskers("muse", wk)
	if got := inbox(t, h, instinctAddr); len(got) != 0 {
		t.Fatalf("told about a wait agent: %+v", got)
	}
}
