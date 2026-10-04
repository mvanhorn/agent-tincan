package wake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
	auth   []string
	sigs   []string
	fail   atomic.Int32 // fail this many requests first
	seq    atomic.Int32
	// holdFirst, if set, parks request 1 until the channel is closed, then
	// answers 502. firstBegin closes when it is parked; firstDone when it has
	// answered.
	holdFirst  chan struct{}
	firstBegin chan struct{}
	firstDone  chan struct{}
}

func (rc *recorder) server(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.bodies = append(rc.bodies, string(raw))
		rc.paths = append(rc.paths, r.URL.Path)
		rc.auth = append(rc.auth, r.Header.Get("Authorization"))
		rc.sigs = append(rc.sigs, r.Header.Get("X-Hub-Signature-256"))
		rc.mu.Unlock()
		n := rc.seq.Add(1)
		if n == 1 && rc.holdFirst != nil {
			close(rc.firstBegin)
			<-rc.holdFirst
			http.Error(w, "down", http.StatusBadGateway)
			if rc.firstDone != nil {
				close(rc.firstDone)
			}
			return
		}
		if rc.fail.Load() > 0 {
			rc.fail.Add(-1)
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (rc *recorder) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.bodies)
}

func (rc *recorder) body(i int) string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.bodies[i]
}

func (rc *recorder) path(i int) string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.paths[i]
}

func queued(w *Waker, to string, n int) {
	for i := range n {
		w.Queued(context.Background(), envelope.Request{ID: "r" + string(rune('a'+i)), To: to, Body: "SECRET call Joe's Garage at 555-0100"})
	}
}

func auditStore(t *testing.T) *store.Store {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func events(t *testing.T, st *store.Store) []string {
	evs, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Event)
	}
	return out
}

func TestBurstGivesOneWebhookWithoutRequestText(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL, BearerToken: "tok"}}, st, Options{Debounce: 50 * time.Millisecond})
	queued(w, "grokbot", 5)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	var body map[string]string
	json.Unmarshal([]byte(rc.bodies[0]), &body)
	if !strings.Contains(body["message"], "5 requests") || strings.Contains(rc.bodies[0], "SECRET") || strings.Contains(rc.bodies[0], "555") {
		t.Fatalf("wake body = %s", rc.bodies[0])
	}
	if rc.auth[0] != "Bearer tok" {
		t.Fatalf("auth = %q", rc.auth[0])
	}
	if got := strings.Join(events(t, st), ","); got != "woke" {
		t.Fatalf("audit = %s", got)
	}
}

func TestWebhookRetriesOnceThenAuditsFailure(t *testing.T) {
	var rc recorder
	rc.fail.Store(1)
	ts := rc.server(t)
	st := auditStore(t)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{Debounce: time.Millisecond, RetryDelay: time.Millisecond})
	queued(w, "grokbot", 1)
	w.Flush()
	if rc.count() != 2 || strings.Join(events(t, st), ",") != "woke" {
		t.Fatalf("calls=%d audit=%v", rc.count(), events(t, st))
	}
	rc.fail.Store(2)
	queued(w, "grokbot", 1)
	w.Flush()
	if got := events(t, st); got[len(got)-1] != "wake_failed" {
		t.Fatalf("audit = %v, want wake_failed last", got)
	}
}

// The generic format carries the count in both message and text, for
// runtimes that read either field.
func TestWebhookBodyMirrorsTextWithBearerOnly(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"openclaw": {Method: Webhook, URL: ts.URL, BearerToken: "tok"}}, nil, Options{Debounce: time.Millisecond})
	queued(w, "openclaw", 2)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(rc.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["source"] != "agent-tincan" || body["message"] != Message(2) || body["text"] != body["message"] {
		t.Fatalf("wake body = %s", rc.bodies[0])
	}
	if rc.auth[0] != "Bearer tok" || rc.sigs[0] != "" {
		t.Fatalf("auth = %q sig = %q", rc.auth[0], rc.sigs[0])
	}
}

// Hermes verifies webhooks with the GitHub HMAC scheme.
func TestWebhookHMACSignsExactBody(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL, HMACSecret: "hush"}}, nil, Options{Debounce: time.Millisecond})
	queued(w, "hermes", 1)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	sign := func(secret string) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(rc.bodies[0]))
		return "sha256=" + hex.EncodeToString(m.Sum(nil))
	}
	if rc.sigs[0] != sign("hush") {
		t.Fatalf("sig = %q, want %q", rc.sigs[0], sign("hush"))
	}
	if hmac.Equal([]byte(rc.sigs[0]), []byte(sign("wrong"))) {
		t.Fatal("signature verified with the wrong secret")
	}
	if rc.auth[0] != "" {
		t.Fatalf("auth = %q, want none", rc.auth[0])
	}
	if strings.Contains(rc.bodies[0], "SECRET") || strings.Contains(rc.bodies[0], "555") || strings.Contains(rc.bodies[0], "hush") {
		t.Fatalf("wake body leaks: %s", rc.bodies[0])
	}
}

// Instinct wakes on email; the relay sends it through Grok Bot's AgentMail.
func TestEmailWakeUsesAgentMail(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"instinct": {Method: Email, EmailTo: "agent@example.com", AgentMailFrom: "bot@agentmail.to", AgentMailKey: "am_key"}},
		nil, Options{Debounce: time.Millisecond, AgentMailAPI: ts.URL + "/v0"})
	queued(w, "instinct", 2)
	w.Flush()
	if rc.count() != 1 || rc.paths[0] != "/v0/inboxes/bot@agentmail.to/messages/send" || rc.auth[0] != "Bearer am_key" {
		t.Fatalf("paths=%v auth=%v", rc.paths, rc.auth)
	}
	var body map[string]string
	json.Unmarshal([]byte(rc.bodies[0]), &body)
	if body["to"] != "agent@example.com" || !strings.Contains(body["text"], "2 requests") || strings.Contains(rc.bodies[0], "SECRET") {
		t.Fatalf("email body = %s", rc.bodies[0])
	}
}

func TestBudgetStopsWakesButNotQueueing(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	now := time.Unix(1_790_000_000, 0)
	w := New(Config{"instinct": {Method: Webhook, URL: ts.URL, MaxPerHour: 2}}, st, Options{Debounce: time.Millisecond, Now: func() time.Time { return now }})
	for range 3 {
		queued(w, "instinct", 1)
		w.Flush()
	}
	if rc.count() != 2 {
		t.Fatalf("wakes = %d, want 2", rc.count())
	}
	if got := events(t, st); got[len(got)-1] != "wake_skipped" {
		t.Fatalf("audit = %v", got)
	}
	now = now.Add(61 * time.Minute)
	queued(w, "instinct", 1)
	w.Flush()
	if rc.count() != 3 {
		t.Fatalf("after the hour, wakes = %d, want 3", rc.count())
	}
}

func TestAgentSideMethodsAndOnlineAgentsNeedNoRelayWake(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{
		"muse":        {Method: Wait},
		"claude-code": {Method: Channel},
		"grokbot":     {Method: Webhook, URL: ts.URL},
	}, nil, Options{Debounce: time.Millisecond, Online: func(a string) bool { return a == "grokbot" }})
	queued(w, "muse", 1)
	queued(w, "claude-code", 1)
	queued(w, "grokbot", 1) // online: its poller has it
	queued(w, "chatgpt", 1) // not configured: none
	w.Flush()
	if rc.count() != 0 {
		t.Fatalf("unexpected wakes: %d", rc.count())
	}
	for agent, want := range map[string]string{"muse": Wait, "claude-code": Channel, "grokbot": Webhook, "chatgpt": None} {
		if got := w.WakeMethod(agent); got != want {
			t.Errorf("%s wake = %s, want %s", agent, got, want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	if c, err := LoadConfig(filepath.Join(dir, "missing.json")); err != nil || len(c) != 0 {
		t.Fatalf("missing file: %v %v", c, err)
	}
	p := filepath.Join(dir, "wake.json")
	os.WriteFile(p, []byte(`{"grokbot":{"method":"webhook","url":"http://x"}}`), 0o644)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable config should be refused: %v", err)
	}
	os.Chmod(p, 0o600)
	if c, err := LoadConfig(p); err != nil || c["grokbot"].Method != Webhook {
		t.Fatalf("load: %v %v", c, err)
	}
	os.WriteFile(p, []byte(`{"hermes":{"method":"webhook","url":"http://x","hmac_secret":"hush"}}`), 0o600)
	if c, err := LoadConfig(p); err != nil || c["hermes"].HMACSecret != "hush" {
		t.Fatalf("hmac_secret: %v %v", c, err)
	}
	for _, bad := range []string{`{"a":{"method":"webhook"}}`, `{"a":{"method":"email","email_to":"x"}}`, `{"a":{"method":"smoke-signal"}}`} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("config %s should be rejected", bad)
		}
	}
}

// A requeue happens because the agent's own claim or delivery timed out, so a
// recent poll does not mean a poller is holding the request: wake anyway.
func TestRequeuedWakesEvenWhenRecentlyOnline(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{
		"hermes": {Method: Webhook, URL: ts.URL},
		"muse":   {Method: Wait},
	}, nil, Options{Debounce: time.Millisecond, Online: func(string) bool { return true }})
	w.Requeued(context.Background(), envelope.Request{ID: "r1", To: "hermes"})
	w.Requeued(context.Background(), envelope.Request{ID: "r2", To: "muse"}) // agent-side method: no relay wake
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want 1 for the requeued webhook agent", rc.count())
	}
}

// A webhook agent counts as online for a few seconds after its last poll, so
// a request queued just as its session ends skips the wake. Nobody is left to
// take it, so the waker checks again later and wakes it if it is still queued.
func TestOnlineSkipRechecksAndWakesWhenStillQueued(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var stillQueued atomic.Int32
	stillQueued.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil, Options{
		Debounce:      time.Millisecond,
		OnlineRecheck: 20 * time.Millisecond,
		WakeGrace:     skipFollowUp,
		Online:        func(string) bool { return true },
		Queued:        func(string) int { return int(stillQueued.Load()) },
	})
	queued(w, "hermes", 1) // session polled a moment ago, then ended
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want 1: the request is still queued after the recheck", rc.count())
	}
	if !strings.Contains(rc.bodies[0], "1") {
		t.Errorf("nudge should count the waiting request: %s", rc.bodies[0])
	}

	// A poller that really held it leaves nothing queued: no wake.
	stillQueued.Store(0)
	queued(w, "hermes", 1)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want still 1 once the poller took the request", rc.count())
	}
}

func TestUrgentImmediateOnlineAndBudget(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	w := New(Config{"target": {Method: Webhook, URL: ts.URL, MaxPerHour: 1}}, st, Options{Debounce: time.Hour, Online: func(string) bool { return true }})
	w.Queued(t.Context(), envelope.Request{To: "target", Urgent: true})
	done := make(chan struct{})
	go func() { w.Flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("urgent wake waited for debounce or online recheck")
	}
	if rc.count() != 1 {
		t.Fatalf("wakes = %d", rc.count())
	}
	w.Queued(t.Context(), envelope.Request{To: "target", Urgent: true})
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("urgent exceeded hourly budget: %d", rc.count())
	}
	if got := strings.Join(events(t, st), ","); got != "woke,wake_skipped" {
		t.Fatalf("audit = %s", got)
	}
}

func TestUrgentPullsPendingWakeForward(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"target": {Method: Webhook, URL: ts.URL}}, auditStore(t), Options{Debounce: time.Hour})
	w.Queued(t.Context(), envelope.Request{To: "target"})
	w.Queued(t.Context(), envelope.Request{To: "target", Urgent: true})
	done := make(chan struct{})
	go func() { w.Flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("urgent request did not pull in pending wake")
	}
	if rc.count() != 1 {
		t.Fatalf("wakes = %d", rc.count())
	}
}

// Stop drops scheduled nudges, cuts short one waiting to retry, and
// schedules nothing afterwards, so a stopping relay is not held up by its
// wakes.
func TestStopDropsAndEndsNudges(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	cfg := Config{"grokbot": {Method: Webhook, URL: ts.URL}, "muse": {Method: Webhook, URL: ts.URL}}
	w := New(cfg, st, Options{Debounce: time.Hour, RetryDelay: time.Hour})
	queued(w, "grokbot", 1) // waits out the hour-long debounce
	rc.fail.Store(1)
	w.Queued(context.Background(), envelope.Request{ID: "ru", To: "muse", Urgent: true}) // fails, then waits an hour to retry
	deadline := time.Now().Add(5 * time.Second)
	for rc.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	w.Stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Stop took %v", took)
	}
	queued(w, "grokbot", 1)
	w.ReplyWaiting("muse")
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want only the failed urgent one", rc.count())
	}
	if got := strings.Join(events(t, st), ","); got != "wake_failed" {
		t.Fatalf("audit = %s, want the cut-short nudge recorded as failed", got)
	}
}

// A restarted relay re-arms the request wakes the old process lost. The
// nudge counts the queued requests when it fires, so one a poller took in
// the meantime wakes nobody, and agents woken by their own side are skipped.
func TestRequestsWaitingCountsAtFireTime(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var queuedN atomic.Int32
	queuedN.Store(2)
	cfg := Config{"grokbot": {Method: Webhook, URL: ts.URL}, "muse": {Method: Command}}
	w := New(cfg, nil, Options{Debounce: time.Millisecond, WakeGrace: skipFollowUp, Queued: func(string) int { return int(queuedN.Load()) }})
	w.RequestsWaiting("grokbot")
	w.RequestsWaiting("muse")
	w.Flush()
	if rc.count() != 1 || !strings.Contains(rc.bodies[0], "2 requests") {
		t.Fatalf("wakes = %q, want one for grokbot's 2 requests", rc.bodies)
	}
	queuedN.Store(0) // a poller took them before the nudge fired
	w.RequestsWaiting("grokbot")
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("woke %d times for requests already taken", rc.count())
	}
}

// A request queued while a re-armed wake is pending does not hide the older
// backlog: the nudge names every request still waiting.
func TestRequestsWaitingCountsWholeBacklog(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, nil, Options{Debounce: 50 * time.Millisecond, WakeGrace: skipFollowUp, Queued: func(string) int { return 3 }})
	w.RequestsWaiting("grokbot")
	queued(w, "grokbot", 1)
	w.Flush()
	if rc.count() != 1 || !strings.Contains(rc.bodies[0], "3 requests") {
		t.Fatalf("wakes = %q, want one naming all 3 requests", rc.bodies)
	}
}

// A schedule agent checks its inbox on its own cron: wake.json declares the
// interval, which must be a positive Go duration.
func TestLoadConfigSchedule(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wake.json")
	os.WriteFile(p, []byte(`{"fo":{"method":"schedule","every":"5m"}}`), 0o600)
	c, err := LoadConfig(p)
	if err != nil || c["fo"].Method != Schedule || c["fo"].Interval() != 5*time.Minute {
		t.Fatalf("schedule load: %+v %v", c, err)
	}
	for _, bad := range []string{
		`{"fo":{"method":"schedule"}}`,
		`{"fo":{"method":"schedule","every":"0s"}}`,
		`{"fo":{"method":"schedule","every":"-5m"}}`,
		`{"fo":{"method":"schedule","every":"often"}}`,
	} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "fo") {
			t.Errorf("config %s: err = %v, want a rejection naming fo", bad, err)
		}
	}
}

// The relay never wakes a schedule agent; it only reports its interval.
func TestScheduleAgentGetsNoRelayWake(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{
		"fo":      {Method: Schedule, Every: "5m"},
		"grokbot": {Method: Webhook, URL: ts.URL},
	}, nil, Options{Debounce: time.Millisecond, ReplyGrace: time.Millisecond, ReplyRetries: []time.Duration{}})
	queued(w, "fo", 2)
	w.Replied(context.Background(), envelope.Request{ID: "x", From: "fo", To: "grokbot"})
	w.RequestsWaiting("fo")
	w.Flush()
	if rc.count() != 0 {
		t.Fatalf("schedule agent was woken %d times", rc.count())
	}
	if got := w.WakeMethod("fo"); got != Schedule {
		t.Fatalf("fo wake = %q, want schedule", got)
	}
	if got := w.CheckEvery("fo"); got != 5*time.Minute {
		t.Fatalf("fo check every = %v, want 5m", got)
	}
	for _, a := range []string{"grokbot", "nobody"} {
		if got := w.CheckEvery(a); got != 0 {
			t.Errorf("%s check every = %v, want 0", a, got)
		}
	}
}

// The waker remembers each agent's last real wake send: its time and "ok",
// or the error once the send and its retry both failed.
func TestLastWakeRecordsSendResult(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	now := time.Unix(1_790_000_000, 0)
	var poll time.Time
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}, "muse": {Method: Wait}}, st,
		Options{Debounce: time.Millisecond, RetryDelay: time.Millisecond, Now: func() time.Time { return now }, LastPoll: func(string) time.Time { return poll }})
	if _, ok := w.LastWake("grokbot"); ok {
		t.Fatal("last wake before any send")
	}
	queued(w, "grokbot", 1)
	w.Flush()
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(now) || got.Result != "ok" {
		t.Fatalf("after ok send: %+v %v", got, ok)
	}
	poll = now // a check-in starts a new episode; a later failure records its own time
	now = now.Add(time.Minute)
	rc.fail.Store(2)
	queued(w, "grokbot", 1)
	w.Flush()
	got, ok := w.LastWake("grokbot")
	if !ok || !got.At.Equal(now) || !strings.Contains(got.Result, "502") {
		t.Fatalf("after failed send: %+v %v", got, ok)
	}
	if stored, err := st.LastWakes(context.Background()); err != nil || stored["grokbot"] != got {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	queued(w, "muse", 1)
	w.Flush()
	if _, ok := w.LastWake("muse"); ok {
		t.Fatal("agent-side method has a last wake")
	}
}

// A waker over a reopened store still knows the last wake.
func TestLastWakeLoadsFromStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(1_790_000_000_000)
	if err := st.SetLastWake(context.Background(), "grokbot", store.Wake{At: at, Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := New(Config{"grokbot": {Method: Webhook, URL: "http://127.0.0.1:1/hook"}}, st, Options{})
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(at) || got.Result != "ok" {
		t.Fatalf("loaded = %+v %v", got, ok)
	}
}

// A wake the hourly cap skips is not a send, so it leaves the last real
// send in place; a debounced burst records one send.
func TestLastWakeIgnoresSkippedWakes(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	start := time.Unix(1_790_000_000, 0)
	now := start
	w := New(Config{"instinct": {Method: Webhook, URL: ts.URL, MaxPerHour: 1}}, st, Options{Debounce: 20 * time.Millisecond, Now: func() time.Time { return now }})
	queued(w, "instinct", 3)
	w.Flush()
	now = now.Add(time.Minute)
	queued(w, "instinct", 1)
	w.Flush()
	if got := strings.Join(events(t, st), ","); got != "woke,wake_skipped" {
		t.Fatalf("audit = %s", got)
	}
	if got, ok := w.LastWake("instinct"); !ok || !got.At.Equal(start) || got.Result != "ok" {
		t.Fatalf("last wake = %+v %v, want the first send", got, ok)
	}
}

// A wake that fails before any response (connection refused here) keeps a
// result that names the host but never the URL's userinfo, path or query,
// nor the AgentMail key: wake_result reaches every joined agent.
func TestLastWakeTransportErrorHidesURL(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	st := auditStore(t)
	secrets := []string{"hunter2", "tok-secret", "path-secret", "am-key-secret", "from-secret", "user:"}
	w := New(Config{
		"grokbot":  {Method: Webhook, URL: "http://user:hunter2@127.0.0.1:1/hook/path-secret?token=tok-secret"},
		"instinct": {Method: Email, EmailTo: "i@example.com", AgentMailFrom: "from-secret", AgentMailKey: "am-key-secret"},
	}, st, Options{Debounce: time.Millisecond, RetryDelay: time.Millisecond, AgentMailAPI: "http://127.0.0.1:1/v0?api_key=am-key-secret"})
	queued(w, "grokbot", 1)
	queued(w, "instinct", 1)
	w.Flush()
	for _, agent := range []string{"grokbot", "instinct"} {
		got, ok := w.LastWake(agent)
		if !ok || got.Result == envelope.WakeOK || !strings.Contains(got.Result, "127.0.0.1:1") {
			t.Fatalf("%s last wake = %+v %v, want a failure naming the host", agent, got, ok)
		}
		for _, s := range secrets {
			if strings.Contains(got.Result, s) {
				t.Errorf("%s wake result %q leaks %q", agent, got.Result, s)
			}
		}
	}
	evs, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		for _, s := range secrets {
			if strings.Contains(e.Detail, s) {
				t.Errorf("audit %s detail %q leaks %q", e.Event, e.Detail, s)
			}
		}
	}
	for _, s := range secrets {
		if strings.Contains(logged.String(), s) {
			t.Errorf("relay log %q leaks %q", logged.String(), s)
		}
	}
}

// The waker keeps the newer of two wakes whichever is recorded last, in
// memory and in the store.
func TestRememberKeepsNewerWake(t *testing.T) {
	st := auditStore(t)
	w := New(Config{"grokbot": {Method: Webhook, URL: "http://127.0.0.1:1/hook"}}, st, Options{})
	t1 := time.UnixMilli(1_790_000_000_000)
	t2 := t1.Add(time.Minute)
	w.remember(context.Background(), "grokbot", store.Wake{At: t2, Result: "ok"})
	w.remember(context.Background(), "grokbot", store.Wake{At: t1, Result: "boom"})
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(t2) || got.Result != "ok" {
		t.Fatalf("in memory = %+v %v", got, ok)
	}
	if stored, err := st.LastWakes(context.Background()); err != nil || !stored["grokbot"].At.Equal(t2) {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
}

// skipFollowUp turns off request follow-up so Flush is not held by the
// wake-grace timer. New treats 0 as DefaultWakeGrace.
const skipFollowUp = time.Duration(-1)

func waitCalls(t *testing.T, rc *recorder, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for rc.count() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rc.count() != n {
		t.Fatalf("webhook calls = %d, want %d", rc.count(), n)
	}
}

func drainFollowUp(t *testing.T, w *Waker, queued *atomic.Int32) {
	t.Helper()
	queued.Store(0)
	w.Flush()
}

// atomicTime is a clock or last-poll stamp tests mutate while a nudge timer
// may be firing.
type atomicTime struct{ v atomic.Value }

func (a *atomicTime) set(t time.Time) { a.v.Store(t) }

func (a *atomicTime) get() time.Time {
	if v := a.v.Load(); v != nil {
		return v.(time.Time)
	}
	return time.Time{}
}

func TestDefaultWakeGrace(t *testing.T) {
	w := New(Config{}, nil, Options{})
	if w.opts.WakeGrace != DefaultWakeGrace {
		t.Fatalf("WakeGrace = %v, want %v", w.opts.WakeGrace, DefaultWakeGrace)
	}
}

// AE1: a 2xx request wake that stays queued with no poll is POSTed again after
// WakeGrace, and last wake time stays the first send.
func TestRequestFollowUpAfterSilent2xx(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 20 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return time.Time{} },
		Now:       now.get,
	})
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 1)
	now.set(start.Add(time.Minute))
	waitCalls(t, &rc, 2)
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(start) || got.Result != envelope.WakeOK {
		t.Fatalf("last wake = %+v %v, want first send at %v", got, ok, start)
	}
	if !strings.Contains(rc.body(1), "1 request") || strings.Contains(rc.body(1), "SECRET") {
		t.Fatalf("follow-up body = %s", rc.body(1))
	}
	drainFollowUp(t, w, &queuedN)
}

// AE2: a poll after the first follow-up stops further request POSTs.
func TestRequestFollowUpStopsAfterPoll(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	start := time.Unix(1_790_000_000, 0)
	var now, poll atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, nil, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 20 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return poll.get() },
		Now:       now.get,
	})
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 2)
	poll.set(now.get())
	w.Flush()
	if rc.count() != 2 {
		t.Fatalf("webhook calls = %d, want 2 after the poll", rc.count())
	}
}

// An empty queue without a poll also stops the follow-up.
func TestRequestFollowUpStopsWhenQueueDrains(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, nil, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 20 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return time.Time{} },
	})
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 1)
	queuedN.Store(0)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1 once the queue drained", rc.count())
	}
}

// AE3: a follow-up under a spent hourly cap is skipped with no POST, last
// wake unchanged, and a later fire after the hour window POSTs again.
func TestRequestFollowUpRespectsHourlyCap(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"instinct": {Method: Webhook, URL: ts.URL, MaxPerHour: 1}}, st, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 20 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return time.Time{} },
		Now:       now.get,
	})
	queued(w, "instinct", 1)
	waitCalls(t, &rc, 1)
	deadline := time.Now().Add(5 * time.Second)
	for strings.Join(events(t, st), ",") != "woke,wake_skipped" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Join(events(t, st), ","); got != "woke,wake_skipped" {
		t.Fatalf("audit = %s", got)
	}
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1 under a cap of 1", rc.count())
	}
	if got, ok := w.LastWake("instinct"); !ok || !got.At.Equal(start) || got.Result != envelope.WakeOK {
		t.Fatalf("last wake = %+v %v, want the first send", got, ok)
	}
	now.set(start.Add(61 * time.Minute))
	waitCalls(t, &rc, 2)
	drainFollowUp(t, w, &queuedN)
}

// AE7: a failed follow-up records the error, keeps the first wake time, and
// still re-arms.
func TestRequestFollowUpFailedKeepsWokenAt(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{
		Debounce:   time.Millisecond,
		RetryDelay: time.Millisecond,
		WakeGrace:  20 * time.Millisecond,
		Queued:     func(string) int { return int(queuedN.Load()) },
		LastPoll:   func(string) time.Time { return time.Time{} },
		Now:        now.get,
	})
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 1)
	rc.fail.Store(2)
	now.set(start.Add(time.Minute))
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok := w.LastWake("grokbot")
		if ok && strings.Contains(got.Result, "502") {
			if !got.At.Equal(start) {
				t.Fatalf("last wake = %+v, want time %v", got, start)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("last wake = %+v %v, want a 502 keeping %v", got, ok, start)
		}
		time.Sleep(5 * time.Millisecond)
	}
	rc.fail.Store(0)
	waitCalls(t, &rc, 4) // first ok, two failed attempts, then a recovered follow-up
	deadline = time.Now().Add(5 * time.Second)
	for {
		got, ok := w.LastWake("grokbot")
		if ok && got.Result == envelope.WakeOK {
			if !got.At.Equal(start) {
				t.Fatalf("recovered last wake = %+v, want time %v", got, start)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("last wake = %+v %v, want ok keeping %v", got, ok, start)
		}
		time.Sleep(5 * time.Millisecond)
	}
	drainFollowUp(t, w, &queuedN)
}

// A failed follow-up is what a reopened store loads: same woken_at, new result.
func TestRequestFollowUpFailedResultSurvivesReload(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{
		Debounce:   time.Millisecond,
		RetryDelay: time.Millisecond,
		WakeGrace:  20 * time.Millisecond,
		Queued:     func(string) int { return int(queuedN.Load()) },
		LastPoll:   func(string) time.Time { return time.Time{} },
		Now:        now.get,
	})
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 1)
	rc.fail.Store(100)
	now.set(start.Add(time.Minute))
	deadline := time.Now().Add(5 * time.Second)
	var fail store.Wake
	for {
		got, ok := w.LastWake("grokbot")
		if ok && strings.Contains(got.Result, "502") {
			if !got.At.Equal(start) {
				t.Fatalf("last wake = %+v, want time %v", got, start)
			}
			fail = got
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("last wake = %+v %v, want a 502 keeping %v", got, ok, start)
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Stop()
	st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w = New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{WakeGrace: skipFollowUp})
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(fail.At) || got.Result != fail.Result {
		t.Fatalf("reloaded = %+v %v, want %+v", got, ok, fail)
	}
}

// A later 2xx while still silent keeps the first woken_at and stores ok, including across reload.
func TestRequestFollowUpRecoveredResultKeepsWokenAt(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{
		Debounce:   time.Millisecond,
		RetryDelay: time.Millisecond,
		WakeGrace:  20 * time.Millisecond,
		Queued:     func(string) int { return int(queuedN.Load()) },
		LastPoll:   func(string) time.Time { return time.Time{} },
		Now:        now.get,
	})
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 1)
	rc.fail.Store(2)
	now.set(start.Add(time.Minute))
	waitCalls(t, &rc, 4)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok := w.LastWake("grokbot")
		if ok && got.Result == envelope.WakeOK {
			if !got.At.Equal(start) {
				t.Fatalf("recovered last wake = %+v, want time %v", got, start)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("last wake = %+v %v, want ok keeping %v", got, ok, start)
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Stop()
	st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w = New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{WakeGrace: skipFollowUp})
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(start) || got.Result != envelope.WakeOK {
		t.Fatalf("reloaded = %+v %v, want ok at %v", got, ok, start)
	}
}

// A slow first send that fails after a later send succeeded must not take
// the successful wake's time or replace ok, including across a store reload.
func TestLateFailedSendDoesNotReplaceNewerOK(t *testing.T) {
	var rc recorder
	rc.holdFirst = make(chan struct{})
	rc.firstBegin = make(chan struct{})
	rc.firstDone = make(chan struct{})
	ts := rc.server(t)
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{
		Debounce:   time.Millisecond,
		RetryDelay: 200 * time.Millisecond,
		WakeGrace:  skipFollowUp,
		Queued:     func(string) int { return int(queuedN.Load()) },
		LastPoll:   func(string) time.Time { return time.Time{} },
		Now:        now.get,
	})
	queued(w, "grokbot", 1)
	select {
	case <-rc.firstBegin:
	case <-time.After(5 * time.Second):
		t.Fatal("first send did not start")
	}
	second := start.Add(time.Second)
	now.set(second)
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 2)
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(second) || got.Result != envelope.WakeOK {
		t.Fatalf("after second send = %+v %v, want ok at %v", got, ok, second)
	}
	close(rc.holdFirst)
	select {
	case <-rc.firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first send did not finish")
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		got, ok := w.LastWake("grokbot")
		if !ok || got.Result != envelope.WakeOK || !got.At.Equal(second) {
			t.Fatalf("late failure overwrote the newer ok: %+v %v", got, ok)
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Stop()
	st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w = New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{WakeGrace: skipFollowUp})
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(second) || got.Result != envelope.WakeOK {
		t.Fatalf("reloaded = %+v %v, want ok at %v", got, ok, second)
	}
}

// Forget then Joined (remove and re-join under the same name) still
// schedules a wake-grace follow-up when the first send stays unanswered.
func TestJoinedAllowsRequestFollowUp(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, nil, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 20 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return time.Time{} },
	})
	w.Forget("grokbot")
	w.Joined("grokbot")
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 2)
	drainFollowUp(t, w, &queuedN)
}

// An in-flight send that finishes after Forget and Joined is the former
// agent's and is not kept as the new agent's last wake.
func TestInFlightSendAfterRejoinIsNotRemembered(t *testing.T) {
	var rc recorder
	rc.holdFirst = make(chan struct{})
	rc.firstBegin = make(chan struct{})
	rc.firstDone = make(chan struct{})
	ts := rc.server(t)
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, nil, Options{
		Debounce:   time.Millisecond,
		RetryDelay: time.Second,
		WakeGrace:  skipFollowUp,
		Queued:     func(string) int { return int(queuedN.Load()) },
		LastPoll:   func(string) time.Time { return time.Time{} },
		Now:        now.get,
	})
	queued(w, "grokbot", 1)
	select {
	case <-rc.firstBegin:
	case <-time.After(5 * time.Second):
		t.Fatal("first send did not start")
	}
	now.set(start.Add(time.Second))
	w.Forget("grokbot")
	now.set(start.Add(2 * time.Second))
	w.Joined("grokbot")
	close(rc.holdFirst)
	select {
	case <-rc.firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first send did not finish")
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if wk, ok := w.LastWake("grokbot"); ok {
			t.Fatalf("in-flight send after rejoin was remembered: %+v", wk)
		}
		time.Sleep(5 * time.Millisecond)
	}
	now.set(start.Add(3 * time.Second))
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 2)
	got, ok := w.LastWake("grokbot")
	if !ok || !got.At.Equal(start.Add(3*time.Second)) || got.Result != envelope.WakeOK {
		t.Fatalf("new agent's wake = %+v %v", got, ok)
	}
}

// AE6: an email target gets a second AgentMail send with the count-only body
// and the same subject.
func TestRequestFollowUpByEmail(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"instinct": {Method: Email, EmailTo: "agent@example.com", AgentMailFrom: "bot@agentmail.to", AgentMailKey: "am_key"}},
		nil, Options{
			Debounce:     time.Millisecond,
			WakeGrace:    20 * time.Millisecond,
			AgentMailAPI: ts.URL + "/v0",
			Queued:       func(string) int { return int(queuedN.Load()) },
			LastPoll:     func(string) time.Time { return time.Time{} },
		})
	queued(w, "instinct", 1)
	waitCalls(t, &rc, 2)
	if rc.path(1) != "/v0/inboxes/bot@agentmail.to/messages/send" {
		t.Fatalf("follow-up path = %q", rc.path(1))
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(rc.body(1)), &body); err != nil {
		t.Fatal(err)
	}
	if body["subject"] != "Agent Tincan: requests waiting" || body["text"] != Message(1) || strings.Contains(rc.body(1), "SECRET") {
		t.Fatalf("follow-up email = %s", rc.body(1))
	}
	drainFollowUp(t, w, &queuedN)
}

// AE4: wait, channel, command and schedule targets get no request follow-up.
func TestRequestFollowUpOnlyForRelaySideMethods(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{
		"muse":        {Method: Wait},
		"claude-code": {Method: Channel},
		"codex":       {Method: Command},
		"fo":          {Method: Schedule, Every: "5m"},
	}, nil, Options{
		Debounce:     time.Millisecond,
		WakeGrace:    20 * time.Millisecond,
		Queued:       func(string) int { return int(queuedN.Load()) },
		LastPoll:     func(string) time.Time { return time.Time{} },
		AgentMailAPI: ts.URL,
	})
	for _, to := range []string{"muse", "claude-code", "codex", "fo"} {
		queued(w, to, 1)
		w.RequestsWaiting(to)
	}
	w.Flush()
	if rc.count() != 0 {
		t.Fatalf("unexpected wakes: %d", rc.count())
	}
}

// Forget drops a pending request follow-up.
func TestForgetDropsRequestFollowUp(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, nil, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 50 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return time.Time{} },
	})
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 1)
	w.Forget("grokbot")
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1 after Forget", rc.count())
	}
}

// After a poll, a new Queued still sends, and a pending follow-up does not.
func TestQueuedAfterPollSendsAndFollowUpDoesNot(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var now, poll atomicTime
	now.set(time.Unix(1_790_000_000, 0))
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, nil, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 30 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return poll.get() },
		Now:       now.get,
	})
	queued(w, "grokbot", 1)
	waitCalls(t, &rc, 1)
	poll.set(now.get().Add(time.Second))
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("follow-up after poll: calls = %d, want 1", rc.count())
	}
	queued(w, "grokbot", 1)
	w.Flush()
	if rc.count() != 2 {
		t.Fatalf("new Queued after poll: calls = %d, want 2", rc.count())
	}
}

// AE5: a restart RequestsWaiting 2xx does not move woken_at while still silent.
func TestRequestsWaitingDoesNotResetUnansweredClock(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	first := time.UnixMilli(1_790_000_000_000)
	if err := st.SetLastWake(context.Background(), "grokbot", store.Wake{At: first, Result: envelope.WakeOK}); err != nil {
		t.Fatal(err)
	}
	now := first.Add(time.Hour)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{
		Debounce:  time.Millisecond,
		WakeGrace: skipFollowUp,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return time.Time{} },
		Now:       func() time.Time { return now },
	})
	w.RequestsWaiting("grokbot")
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(first) || got.Result != envelope.WakeOK {
		t.Fatalf("last wake = %+v %v, want the persisted first send", got, ok)
	}
}
