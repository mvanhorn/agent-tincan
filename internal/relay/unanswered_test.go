package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// fakeWaker names wake methods and reports last wakes set by the test.
type fakeWaker struct {
	mu      sync.Mutex
	methods map[string]string
	wakes   map[string]store.Wake
}

func (f *fakeWaker) WakeMethod(agent string) string {
	if m := f.methods[agent]; m != "" {
		return m
	}
	return "none"
}

func (f *fakeWaker) LastWake(agent string) (store.Wake, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.wakes[agent]
	return w, ok
}

func (f *fakeWaker) Forget(agent string) {
	f.mu.Lock()
	delete(f.wakes, agent)
	f.mu.Unlock()
}

func (f *fakeWaker) Unforget(string) {}

func (f *fakeWaker) Joined(string) {}

func (f *fakeWaker) set(agent string, w store.Wake) {
	f.mu.Lock()
	f.wakes[agent] = w
	f.mu.Unlock()
}

// wakeHarness has grokbot on a webhook, instinct on email and muse on wait,
// on a fake clock that starts now, one second past the joins.
func wakeHarness(t *testing.T) (*harness, *fakeClock, *fakeWaker) {
	t.Helper()
	clk := &fakeClock{t: time.Now()}
	h := newHarness(t, Config{Now: clk.Now})
	fw := &fakeWaker{methods: map[string]string{"grokbot": "webhook", "instinct": "email", "muse": "wait"}, wakes: map[string]store.Wake{}}
	h.srv.SetWakeNamer(fw)
	clk.advance(time.Second)
	return h, clk, fw
}

// AE1, AE2: a webhook agent woken and silent for longer than the grace is
// unanswered; its first poll after the wake clears it.
func TestRosterUnansweredAfterGrace(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	woke := clk.Now()
	fw.set("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	g := agentInfo(t, h, macAddr, "grokbot")
	if !g.WokenAt.Equal(woke) || g.WakeResult != "ok" || g.Unanswered {
		t.Fatalf("just woken: %+v", g)
	}
	clk.advance(DefaultWakeGrace)
	if g := agentInfo(t, h, macAddr, "grokbot"); g.Unanswered {
		t.Fatalf("exactly the grace later: want not unanswered yet: %+v", g)
	}
	clk.advance(2 * time.Minute)
	if g := agentInfo(t, h, macAddr, "grokbot"); !g.Unanswered {
		t.Fatalf("12m without a poll: want unanswered: %+v", g)
	}
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	if g := agentInfo(t, h, macAddr, "grokbot"); g.Unanswered || !g.WokenAt.Equal(woke) {
		t.Fatalf("after a poll: %+v", g)
	}
}

// A shorter --wake-grace flags sooner.
func TestRosterWakeGraceConfigurable(t *testing.T) {
	clk := &fakeClock{t: time.Now()}
	h := newHarness(t, Config{Now: clk.Now, WakeGrace: time.Minute})
	fw := &fakeWaker{methods: map[string]string{"grokbot": "webhook"}, wakes: map[string]store.Wake{}}
	h.srv.SetWakeNamer(fw)
	clk.advance(time.Second)
	fw.set("grokbot", store.Wake{At: clk.Now(), Result: envelope.WakeOK})
	clk.advance(2 * time.Minute)
	if g := agentInfo(t, h, macAddr, "grokbot"); !g.Unanswered {
		t.Fatalf("2m with a 1m grace: %+v", g)
	}
}

// AE3: a failed send is unanswered at once, with the error.
func TestRosterFailedWakeUnansweredAtOnce(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	fw.set("instinct", store.Wake{At: clk.Now(), Result: "api.agentmail.to returned 502 Bad Gateway"})
	in := agentInfo(t, h, macAddr, "instinct")
	if !in.Unanswered || in.WakeResult != "api.agentmail.to returned 502 Bad Gateway" {
		t.Fatalf("failed wake: %+v", in)
	}
}

// AE5: an agent the relay does not wake never carries the wake fields, even
// with a stale record from an earlier webhook config.
func TestRosterAgentSideMethodsHaveNoWakeFields(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	fw.set("muse", store.Wake{At: clk.Now(), Result: "boom"})
	clk.advance(time.Hour)
	rec := h.do(macAddr, "GET", "/v1/agents", "", http.StatusOK, nil)
	var raw struct{ Agents []map[string]any }
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, a := range raw.Agents {
		for _, k := range []string{"woken_at", "wake_result", "unanswered"} {
			if _, ok := a[k]; ok {
				t.Errorf("%s roster entry has %s: %v", a["name"], k, a)
			}
		}
	}
}

// Only a poll is a check-in: a send or a get from the woken agent after the
// wake leaves it unanswered, in the roster and in a send response.
func TestActivityIsNotACheckIn(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	fw.set("grokbot", store.Wake{At: clk.Now(), Result: envelope.WakeOK})
	clk.advance(time.Minute)
	var sent envelope.SendResponse
	h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"hi"}`, http.StatusCreated, &sent)
	h.do(grokAddr, "GET", "/v1/requests/"+sent.ID, "", http.StatusOK, nil)
	h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, nil)
	clk.advance(DefaultWakeGrace)
	if g := agentInfo(t, h, macAddr, "grokbot"); !g.Unanswered {
		t.Fatalf("send and get after the wake, no poll: want unanswered: %+v", g)
	}
	var out envelope.SendResponse
	h.do(museAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusCreated, &out)
	if out.Target == nil || !out.Target.Unanswered {
		t.Fatalf("send target: %+v", out.Target)
	}
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil)
	if g := agentInfo(t, h, macAddr, "grokbot"); g.Unanswered {
		t.Fatalf("after a poll: %+v", g)
	}
}

// The relay keeps its last poll per agent in the store. After a restart, a
// poll persisted after the wake keeps the agent answered; without one (other
// activity does not count) it is unanswered once the grace has passed since
// the later of the wake and the restart.
func TestRosterUnansweredAcrossRestart(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	fw.set("grokbot", store.Wake{At: clk.Now(), Result: envelope.WakeOK})
	fw.set("instinct", store.Wake{At: clk.Now(), Result: envelope.WakeOK})
	clk.advance(time.Minute)
	h.do(instinctAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"hi"}`, http.StatusCreated, nil)

	clk.advance(time.Hour)
	srv := New(identity.NewDirectory(h.st, h.who, identity.Config{Admins: []string{"macbook-pro-44"}}), h.st, Config{Now: clk.Now})
	srv.SetWakeNamer(fw)
	h2 := &harness{t: t, srv: srv, h: srv.Handler(), st: h.st, who: h.who}
	if g := agentInfo(t, h2, macAddr, "grokbot"); g.Unanswered {
		t.Fatalf("just restarted: %+v", g)
	}
	clk.advance(DefaultWakeGrace + time.Minute)
	if g := agentInfo(t, h2, macAddr, "grokbot"); !g.Unanswered {
		t.Fatalf("grace past the restart, no poll since the wake: %+v", g)
	}
	if in := agentInfo(t, h2, macAddr, "instinct"); in.Unanswered {
		t.Fatalf("polled after the wake, before the restart: %+v", in)
	}
	var out envelope.SendResponse
	h2.do(museAddr, "POST", "/v1/send", `{"to":"instinct","body":"hi"}`, http.StatusCreated, &out)
	if out.Target == nil || out.Target.Unanswered {
		t.Fatalf("send to instinct after the restart: %+v", out.Target)
	}
}

// Every poll by a relay-woken agent is written to the store, so a restart
// cannot lose the check-in that answered a wake. An agent the relay does not
// wake polls constantly and is never judged unanswered, so its polls are not
// written at all.
func TestPollPersistence(t *testing.T) {
	h, clk, _ := wakeHarness(t)
	for range 3 {
		h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
		h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
		polled := clk.Now().Truncate(time.Millisecond)
		if p, err := h.st.AgentLastPoll(t.Context(), "grokbot"); err != nil || !p.Equal(polled) {
			t.Fatalf("persisted webhook poll = %v, %v; want %v", p, err, polled)
		}
		if p, err := h.st.AgentLastPoll(t.Context(), "muse"); err != nil || !p.IsZero() {
			t.Fatalf("persisted wait poll = %v, %v; want none", p, err)
		}
		clk.advance(5 * time.Second)
	}
}

// An empty long poll by a relay-woken agent writes its last poll once, at
// the start that proves it checked in, not again when the hold runs out.
func TestEmptyLongPollPersistsOnce(t *testing.T) {
	h := newHarness(t, Config{PollHold: 50 * time.Millisecond})
	h.srv.SetWakeNamer(&fakeWaker{methods: map[string]string{"grokbot": "webhook"}, wakes: map[string]store.Wake{}})
	if _, err := h.st.DB().Exec(`CREATE TABLE poll_writes (n INTEGER);
CREATE TRIGGER count_poll_writes AFTER UPDATE OF last_poll_at ON agents BEGIN INSERT INTO poll_writes VALUES (1); END`); err != nil {
		t.Fatal(err)
	}
	h.do(grokAddr, "GET", "/v1/poll", "", http.StatusNoContent, nil)
	var n int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM poll_writes`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("last poll writes = %d, %v; want 1", n, err)
	}
}

// A poll that lands while a wake is still being sent (the waker records the
// wake, stamped with when its attempt started, only after the send returns)
// is persisted, so after a restart the agent reads as answered.
func TestPollDuringWakeSurvivesRestart(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	fw.set("grokbot", store.Wake{At: clk.Now(), Result: envelope.WakeOK})
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	clk.advance(5 * time.Second)
	attempt := clk.Now() // a second wake starts sending
	clk.advance(5 * time.Second)
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	clk.advance(time.Second)
	fw.set("grokbot", store.Wake{At: attempt, Result: envelope.WakeOK}) // the send returns

	srv := New(identity.NewDirectory(h.st, h.who, identity.Config{Admins: []string{"macbook-pro-44"}}), h.st, Config{Now: clk.Now})
	srv.SetWakeNamer(fw)
	h2 := &harness{t: t, srv: srv, h: srv.Handler(), st: h.st, who: h.who}
	clk.advance(DefaultWakeGrace + time.Minute)
	if g := agentInfo(t, h2, macAddr, "grokbot"); g.Unanswered {
		t.Fatalf("polled while the wake was sending, then restarted: %+v", g)
	}
}

// AE4: a send to an unanswered relay-woken agent returns the wake facts in
// target, so the asker can be told.
func TestSendResponseCarriesUnansweredWake(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	var out envelope.SendResponse
	h.do(museAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusCreated, &out)
	if out.Target != nil {
		t.Fatalf("never woken: target %+v", out.Target)
	}
	woke := clk.Now()
	fw.set("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	clk.advance(12 * time.Minute)
	h.do(museAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi again"}`, http.StatusCreated, &out)
	if out.Target == nil || !out.Target.Unanswered || !out.Target.WokenAt.Equal(woke) || out.Target.WakeResult != "ok" || out.Target.CheckEverySeconds != 0 {
		t.Fatalf("unanswered grokbot: target %+v", out.Target)
	}
	fw.set("muse", store.Wake{At: woke, Result: "boom"})
	rec := h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"hi"}`, http.StatusCreated, nil)
	if strings.Contains(rec.Body.String(), `"target"`) {
		t.Fatalf("send to a wait agent has a target: %s", rec.Body.String())
	}
}

// The real waker satisfies WakeReporter, so SetWakeNamer picks it up.
var _ WakeReporter = (*wake.Waker)(nil)

// A wake from before the agent joined (an agent removed and invited again
// under the same name) is not the current agent's: no wake fields until the
// relay wakes it again.
func TestWakeBeforeJoinIsIgnored(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	fw.set("grokbot", store.Wake{At: clk.Now().Add(-time.Hour), Result: "hooks.example returned 502 Bad Gateway"})
	clk.advance(DefaultWakeGrace + time.Minute)
	if g := agentInfo(t, h, macAddr, "grokbot"); g.Unanswered || !g.WokenAt.IsZero() || g.WakeResult != "" {
		t.Fatalf("wake before the join: %+v", g)
	}
	var out envelope.SendResponse
	h.do(museAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusCreated, &out)
	if out.Target != nil {
		t.Fatalf("send target for a wake before the join: %+v", out.Target)
	}
}

// Removing an agent drops its last wake from the store and from the waker,
// so nothing about it outlives the agent.
func TestRemoveForgetsLastWake(t *testing.T) {
	h := newHarness(t, Config{})
	if err := h.st.SetLastWake(t.Context(), "grokbot", store.Wake{At: time.Now(), Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	w := wake.New(wake.Config{"grokbot": {Method: wake.Webhook, URL: "http://127.0.0.1:1/hook"}}, h.st, wake.Options{})
	h.srv.SetWakeNamer(w)
	if _, ok := w.LastWake("grokbot"); !ok {
		t.Fatal("no last wake before the remove")
	}
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"grokbot"}`, http.StatusOK, nil)
	if wk, ok := w.LastWake("grokbot"); ok {
		t.Fatalf("waker still has a last wake: %+v", wk)
	}
	if ws, err := h.st.LastWakes(t.Context()); err != nil || len(ws) != 0 {
		t.Fatalf("stored wakes = %+v, %v", ws, err)
	}
}

// A wake still being sent when its agent is removed does not bring the
// agent's last wake back when it finishes, and a nudge still waiting for its
// debounce is dropped. A wake that starts after the removal, for an agent
// invited again under the same name, is kept as usual.
func TestRemoveDuringWakeKeepsItForgotten(t *testing.T) {
	h := newHarness(t, Config{})
	arrived := make(chan struct{}, 4)
	release := make(chan struct{})
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
	}))
	defer hook.Close()
	w := wake.New(wake.Config{
		"grokbot":  {Method: wake.Webhook, URL: hook.URL},
		"instinct": {Method: wake.Webhook, URL: hook.URL},
	}, h.st, wake.Options{HTTP: hook.Client(), Debounce: time.Second})
	defer w.Stop()
	h.srv.SetWakeNamer(w)

	w.Queued(t.Context(), envelope.Request{To: "grokbot", Urgent: true})
	<-arrived // grokbot's wake is in flight
	// instinct's nudge waits out its debounce.
	w.Queued(t.Context(), envelope.Request{To: "instinct"})
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"grokbot"}`, http.StatusOK, nil)
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"instinct"}`, http.StatusOK, nil)
	close(release)
	w.Flush()
	select {
	case <-arrived:
		t.Fatal("the debounced nudge for a removed agent was sent")
	default:
	}
	for _, a := range []string{"grokbot", "instinct"} {
		if wk, ok := w.LastWake(a); ok {
			t.Fatalf("%s: waker has a last wake after the remove: %+v", a, wk)
		}
	}
	if ws, err := h.st.LastWakes(t.Context()); err != nil || len(ws) != 0 {
		t.Fatalf("stored wakes = %+v, %v", ws, err)
	}

	time.Sleep(time.Millisecond) // the next wake starts after the removal
	w.Queued(t.Context(), envelope.Request{To: "grokbot", Urgent: true})
	w.Flush()
	if wk, ok := w.LastWake("grokbot"); !ok || wk.Result != envelope.WakeOK {
		t.Fatalf("wake after the removal: %+v, %v", wk, ok)
	}
}

// A removal whose directory delete fails leaves the agent joined, so the
// waker gets back what Forget dropped: its last wake from the store, wakes
// recorded again, and a wake for the replies it has not read.
func TestFailedRemoveKeepsWakeState(t *testing.T) {
	h := newHarness(t, Config{})
	h.answer(museAddr, h.send(grokAddr, "muse", "call the garage"), "Tue 3pm") // grokbot has an unseen reply
	stored := store.Wake{At: time.UnixMilli(time.Now().Add(-time.Minute).UnixMilli()), Result: "ok"}
	if err := h.st.SetLastWake(t.Context(), "grokbot", stored); err != nil {
		t.Fatal(err)
	}
	hits := make(chan struct{}, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits <- struct{}{} }))
	defer hook.Close()
	// A frozen clock: a wake sent after the removal is stamped with the
	// removal's own time, so only a cleared marker lets it be recorded.
	now := time.Now()
	w := wake.New(wake.Config{"grokbot": {Method: wake.Webhook, URL: hook.URL}}, h.st, wake.Options{
		HTTP: hook.Client(), ReplyGrace: time.Millisecond, ReplyRetries: []time.Duration{},
		WakeGrace: -1, UnseenReplies: h.srv.UnseenReplies, Queued: h.srv.QueuedCount, Now: func() time.Time { return now },
	})
	defer w.Stop()
	h.srv.SetWakeNamer(w)
	if _, err := h.st.DB().Exec(`CREATE TRIGGER fail_remove BEFORE DELETE ON agents BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}

	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"grokbot"}`, http.StatusBadRequest, nil)
	if wk, ok := w.LastWake("grokbot"); !ok || !wk.At.Equal(stored.At) || wk.Result != stored.Result {
		t.Fatalf("last wake after a failed remove = %+v, %v; want %+v", wk, ok, stored)
	}
	w.Flush()
	select {
	case <-hits:
	default:
		t.Fatal("no wake re-armed for the unseen reply")
	}
	if wk, ok := w.LastWake("grokbot"); !ok || !wk.At.Equal(stored.At) || wk.Result != stored.Result {
		t.Fatalf("silent 2xx after a failed remove moved last wake: %+v, %v; want %+v", wk, ok, stored)
	}
}

// A long poll that is open when a wake is recorded and ends after it counts
// as the check-in both in memory and in the store, so a restart agrees.
func TestWakeDuringOpenPollSurvivesRestart(t *testing.T) {
	clk := &fakeClock{t: time.Now()}
	h := newHarness(t, Config{Now: clk.Now, PollHold: 300 * time.Millisecond})
	fw := &fakeWaker{methods: map[string]string{"grokbot": "webhook"}, wakes: map[string]store.Wake{}}
	h.srv.SetWakeNamer(fw)
	clk.advance(time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.do(grokAddr, "GET", "/v1/poll", "", http.StatusNoContent, nil)
	}()
	time.Sleep(100 * time.Millisecond)
	clk.advance(time.Second)
	fw.set("grokbot", store.Wake{At: clk.Now(), Result: envelope.WakeOK}) // woken while the poll waits
	clk.advance(time.Second)
	<-done
	if g := agentInfo(t, h, macAddr, "grokbot"); g.Unanswered {
		t.Fatalf("before restart: %+v", g)
	}
	srv := New(identity.NewDirectory(h.st, h.who, identity.Config{Admins: []string{"macbook-pro-44"}}), h.st, Config{Now: clk.Now})
	srv.SetWakeNamer(fw)
	h2 := &harness{t: t, srv: srv, h: srv.Handler(), st: h.st, who: h.who}
	clk.advance(DefaultWakeGrace + time.Minute)
	if g := agentInfo(t, h2, macAddr, "grokbot"); g.Unanswered {
		t.Fatalf("after restart, the poll that outlived the wake is lost: %+v", g)
	}
}

// A send whose in-memory last poll is already at or after the wake must not
// read the store: closing it would panic if LastPoll still queried.
func TestLastPollSkipsStoreWhenMemoryIsAtOrAfterWake(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	woke := clk.Now()
	fw.set("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	clk.advance(time.Second)
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	st := h.srv.store
	h.srv.store = nil
	t.Cleanup(func() { h.srv.store = st })
	if last := h.srv.LastPoll("grokbot"); last.IsZero() {
		t.Fatal("LastPoll dropped the in-memory poll")
	}
	tgt := h.srv.recipientWake(t.Context(), "grokbot")
	if tgt == nil || tgt.Unanswered || tgt.WakeResult != envelope.WakeOK {
		t.Fatalf("send wake from memory: %+v", tgt)
	}
}

// When this process's last poll is older than the wake, the store still
// answers if it has a later poll, as after a restart that left memory stale.
func TestLastPollUsesStoreWhenMemoryIsOlderThanWake(t *testing.T) {
	h, clk, fw := wakeHarness(t)
	woke := clk.Now()
	fw.set("grokbot", store.Wake{At: woke, Result: envelope.WakeOK})
	clk.advance(time.Second)
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	h.srv.mu.Lock()
	h.srv.lastPoll["grokbot"] = woke.Add(-time.Second)
	h.srv.mu.Unlock()
	clk.advance(DefaultWakeGrace + time.Minute)
	var out envelope.SendResponse
	h.do(museAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusCreated, &out)
	if out.Target == nil || out.Target.Unanswered {
		t.Fatalf("store poll after the wake must win: %+v", out.Target)
	}
}

// After remove and join under the same name, a silent request wake still
// gets a wake-grace follow-up.
func TestRequestFollowUpAfterRemoveAndRejoin(t *testing.T) {
	h := newHarness(t, Config{})
	hits := make(chan struct{}, 8)
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits <- struct{}{}
	}))
	t.Cleanup(hook.Close)
	w := wake.New(wake.Config{"grokbot": {Method: wake.Webhook, URL: hook.URL}}, h.st, wake.Options{
		HTTP:      hook.Client(),
		Debounce:  time.Millisecond,
		WakeGrace: 20 * time.Millisecond,
		Queued:    h.srv.QueuedCount,
		LastPoll:  h.srv.LastPoll,
	})
	t.Cleanup(w.Stop)
	h.srv.SetWakeNamer(w)
	h.srv.SetEvents(w)

	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"grokbot"}`, http.StatusOK, nil)
	h.joinAt(grokAddr, "grokbot")
	h.do(museAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusCreated, nil)

	deadline := time.Now().Add(5 * time.Second)
	n := 0
	for n < 2 && time.Now().Before(deadline) {
		select {
		case <-hits:
			n++
		case <-time.After(20 * time.Millisecond):
		}
	}
	if n != 2 {
		t.Fatalf("webhook calls = %d, want 2 (first wake and follow-up after rejoin)", n)
	}
}

// An in-flight wake that finishes after remove and join under the same name
// is the former agent's. The new agent's later unanswered wake is still
// visible on the roster.
func TestInFlightWakeAfterRejoinDoesNotHideUnanswered(t *testing.T) {
	h := newHarness(t, Config{WakeGrace: 30 * time.Millisecond})
	arrived := make(chan struct{}, 8)
	release := make(chan struct{})
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		arrived <- struct{}{}
		<-release
	}))
	t.Cleanup(hook.Close)
	w := wake.New(wake.Config{"grokbot": {Method: wake.Webhook, URL: hook.URL}}, h.st, wake.Options{
		HTTP:       hook.Client(),
		Debounce:   time.Millisecond,
		RetryDelay: time.Second,
		WakeGrace:  -1,
		Queued:     h.srv.QueuedCount,
		LastPoll:   h.srv.LastPoll,
	})
	t.Cleanup(w.Stop)
	h.srv.SetWakeNamer(w)
	h.srv.SetEvents(w)

	w.Queued(t.Context(), envelope.Request{To: "grokbot", Urgent: true})
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("first send did not start")
	}
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"grokbot"}`, http.StatusOK, nil)
	h.joinAt(grokAddr, "grokbot")
	joined, found, err := h.srv.dir.Agent(t.Context(), "grokbot")
	if err != nil || !found {
		t.Fatalf("rejoined grokbot: found %v, %v", found, err)
	}
	close(release)
	w.Flush()
	if wk, ok := w.LastWake("grokbot"); ok {
		t.Fatalf("pre-join send was remembered: %+v", wk)
	}

	h.do(museAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hi"}`, http.StatusCreated, nil)
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("wake after rejoin did not send")
	}
	time.Sleep(80 * time.Millisecond)
	g := agentInfo(t, h, macAddr, "grokbot")
	if !g.Unanswered || g.WokenAt.IsZero() || g.WokenAt.Before(joined.JoinedAt) {
		t.Fatalf("new agent's unanswered wake hidden: %+v, joined %v", g, joined.JoinedAt)
	}
}
