package wake

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// asksFake stands in for the relay's open-asks query.
type asksFake struct {
	mu   sync.Mutex
	asks []envelope.Request
}

func (f *asksFake) open(string) ([]envelope.Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asks), nil
}

func (f *asksFake) add(r envelope.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks = append(f.asks, r)
}

// fakeTag stands in for the relay's tag minting.
func fakeTag(req envelope.Request) string { return "tag" + req.ID }

// optedIn is an email agent with request emails turned on.
func optedIn() Target {
	return Target{Method: Email, EmailTo: "agent@example.com", AgentMailFrom: "bot@agentmail.to", AgentMailKey: "am_key", IncludeRequests: true}
}

type sentMail struct{ To, Subject, Text string }

// mail decodes AgentMail send i.
func (rc *recorder) mail(t *testing.T, i int) sentMail {
	t.Helper()
	var m sentMail
	if err := json.Unmarshal([]byte(rc.body(i)), &m); err != nil {
		t.Fatalf("send %d: %v", i, err)
	}
	return m
}

func ask(id, from, body string, at time.Time) envelope.Request {
	return envelope.Request{ID: id, From: from, To: "instinct", Kind: envelope.KindAsk, Body: body, CreatedAt: at, Status: envelope.StatusQueued}
}

// subjectFor is the subject a request email for id from asker carries.
func subjectFor(asker, id string) string {
	return "Agent Tincan: request from " + asker + " [tincan " + id + ".tag" + id + "]"
}

// Opted in with two asks open: one AgentMail send per ask, each with its own
// id and tag in the subject and only its own text, the urgent one first and
// saying so, with the reply instructions. The fire is one woke row; no log
// line or audit detail carries a body, a tag or the AgentMail key.
func TestRequestEmailPerAsk(t *testing.T) {
	var logged lockedBuffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	at := time.Unix(1_790_000_000, 0)
	asks := &asksFake{asks: []envelope.Request{
		ask("ra", "claude-code", "SECRET-A find a generator quote", at),
		ask("rb", "muse", "SECRET-B book the boat", at.Add(time.Second)),
	}}
	asks.asks[0].Urgent = true
	w := New(Config{"instinct": optedIn()}, st, Options{Debounce: time.Millisecond, WakeGrace: skipFollowUp, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag})
	for _, r := range asks.asks {
		w.Queued(context.Background(), r)
	}
	w.Flush()
	if rc.count() != 2 {
		t.Fatalf("AgentMail sends = %d, want one per ask", rc.count())
	}
	for i := range 2 {
		if rc.path(i) != "/v0/inboxes/bot@agentmail.to/messages/send" || rc.auth[i] != "Bearer am_key" {
			t.Fatalf("send %d: path %q auth %q", i, rc.path(i), rc.auth[i])
		}
	}
	a, b := rc.mail(t, 0), rc.mail(t, 1)
	if a.To != "agent@example.com" || a.Subject != subjectFor("claude-code", "ra") || b.Subject != subjectFor("muse", "rb") {
		t.Fatalf("subjects = %q, %q", a.Subject, b.Subject)
	}
	for _, want := range []string{"SECRET-A find a generator quote", "ra", "claude-code", "URGENT"} {
		if !strings.Contains(a.Text, want) {
			t.Errorf("first email lacks %q:\n%s", want, a.Text)
		}
	}
	if strings.Contains(a.Text, "SECRET-B") || strings.Contains(b.Text, "SECRET-A") || strings.Contains(b.Text, "URGENT") {
		t.Fatalf("emails mix requests:\n%s\n---\n%s", a.Text, b.Text)
	}
	for _, m := range []sentMail{a, b} {
		for _, want := range []string{"Reply to this email", "keep the subject", "failed:", "declined:", "cannot ask a clarifying question"} {
			if !strings.Contains(m.Text, want) {
				t.Errorf("email lacks reply instruction %q:\n%s", want, m.Text)
			}
		}
	}
	if got := strings.Join(events(t, st), ","); got != "woke" {
		t.Fatalf("audit = %s, want one woke for the fire", got)
	}
	evs, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET", "tagra", "tagrb", "am_key", "agent@example.com"} {
		for _, e := range evs {
			if strings.Contains(e.Detail, secret) {
				t.Errorf("audit %s detail %q leaks %q", e.Event, e.Detail, secret)
			}
		}
		if strings.Contains(logged.String(), secret) {
			t.Errorf("relay log leaks %q: %s", secret, logged.String())
		}
	}
}

// Held requests, pings and notifies never reach a request email: with no
// ask open the agent gets today's count-only email, and with one open it gets
// that ask's email plus the count-only email for the rest.
func TestRequestEmailSkipsHeldAndPing(t *testing.T) {
	for _, withAsk := range []bool{false, true} {
		name := "no ask"
		if withAsk {
			name = "with ask"
		}
		t.Run(name, func(t *testing.T) {
			var rc recorder
			ts := rc.server(t)
			st := auditStore(t)
			ctx := context.Background()
			enqueue := func(req envelope.Request) envelope.Request {
				t.Helper()
				req.From, req.To, req.Hop, req.Chain = "muse", "instinct", 1, []string{"muse"}
				out, err := st.Enqueue(ctx, req, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			enqueue(envelope.Request{Kind: envelope.KindAsk, Body: "HELDTEXT", Status: envelope.StatusHeld, HoldTTL: time.Hour})
			var woken []envelope.Request
			woken = append(woken, enqueue(envelope.Request{Kind: envelope.KindPing, Body: "PINGTEXT"}))
			if withAsk {
				woken = append(woken, enqueue(envelope.Request{Kind: envelope.KindAsk, Body: "ASKTEXT"}))
			}
			w := New(Config{"instinct": optedIn()}, st, Options{
				Debounce: time.Millisecond, WakeGrace: skipFollowUp, AgentMailAPI: ts.URL + "/v0",
				OpenAsks:   func(a string) ([]envelope.Request, error) { return st.OpenAsks(ctx, a) },
				Queued:     func(a string) int { n, _ := st.CountQueued(ctx, a); return n },
				RequestTag: fakeTag,
			})
			for _, r := range woken {
				w.Queued(ctx, r)
			}
			w.Flush()
			var countOnly, requestMails []sentMail
			for i := range rc.count() {
				m := rc.mail(t, i)
				for _, leak := range []string{"HELDTEXT", "PINGTEXT"} {
					if strings.Contains(m.Text, leak) || strings.Contains(m.Subject, leak) {
						t.Fatalf("email carries %s: %+v", leak, m)
					}
				}
				if m.Subject == "Agent Tincan: requests waiting" {
					countOnly = append(countOnly, m)
				} else {
					requestMails = append(requestMails, m)
				}
			}
			if len(countOnly) != 1 || countOnly[0].Text != Message(1) {
				t.Fatalf("count-only emails = %+v, want one for the ping", countOnly)
			}
			if !withAsk {
				if len(requestMails) != 0 {
					t.Fatalf("request emails without an open ask: %+v", requestMails)
				}
				return
			}
			if len(requestMails) != 1 || !strings.Contains(requestMails[0].Text, "ASKTEXT") || requestMails[0].Subject != subjectFor("muse", woken[1].ID) {
				t.Fatalf("request emails = %+v, want one for the ask", requestMails)
			}
		})
	}
}

// With include_requests off the email and webhook payloads are byte for byte
// today's, even with the open-asks and tag hooks wired.
func TestRequestEmailOffKeepsCountOnly(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "SECRET call Joe's Garage at 555-0100", time.Unix(1_790_000_000, 0))}}
	off := optedIn()
	off.IncludeRequests = false
	w := New(Config{"instinct": off, "grokbot": {Method: Webhook, URL: ts.URL + "/hook"}}, nil, Options{
		Debounce: time.Millisecond, WakeGrace: skipFollowUp, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
	})
	queued(w, "instinct", 2)
	w.Flush()
	queued(w, "grokbot", 1)
	w.Flush()
	wantMail, _ := json.Marshal(map[string]any{"to": "agent@example.com", "subject": "Agent Tincan: requests waiting", "text": Message(2)})
	wantHook, _ := json.Marshal(map[string]string{"source": "agent-tincan", "message": Message(1), "text": Message(1)})
	if rc.count() != 2 || rc.body(0) != string(wantMail) || rc.body(1) != string(wantHook) {
		t.Fatalf("bodies = %q, want %s and %s", rc.bodies, wantMail, wantHook)
	}
}

// A request email names how many attachments there are and to fetch them
// with tincan, never their names, and carries earlier clarification rounds.
func TestRequestEmailAttachmentsAndExchanges(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	r := ask("ra", "muse", "quote the generator", time.Unix(1_790_000_000, 0))
	r.Attachments = []envelope.Attachment{{ID: "att1", Name: "SECRETNAME-quote.pdf"}, {ID: "att2", Name: "SECRETNAME-photo.png"}}
	r.Exchanges = []envelope.Exchange{{Question: "which house?", Answer: "the lake house"}}
	one := ask("rb", "muse", "and the other one", time.Unix(1_790_000_001, 0))
	one.Attachments = []envelope.Attachment{{ID: "att3", Name: "SECRETNAME-list.txt"}}
	asks := &asksFake{asks: []envelope.Request{r, one}}
	w := New(Config{"instinct": optedIn()}, nil, Options{Debounce: time.Millisecond, WakeGrace: skipFollowUp, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag})
	w.Queued(context.Background(), r)
	w.Flush()
	if rc.count() != 2 {
		t.Fatalf("sends = %d, want 2", rc.count())
	}
	m, n := rc.mail(t, 0), rc.mail(t, 1)
	for _, want := range []string{"2 attachments: fetch with tincan", "which house?", "the lake house"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("email lacks %q:\n%s", want, m.Text)
		}
	}
	if !strings.Contains(n.Text, "1 attachment: fetch with tincan") || strings.Contains(n.Text, "which house?") {
		t.Errorf("second email:\n%s", n.Text)
	}
	for i := range 2 {
		if strings.Contains(rc.body(i), "SECRETNAME") {
			t.Fatalf("email names an attachment: %s", rc.body(i))
		}
	}
}

// A body over 64 KiB is cut there with a note to fetch the rest with
// tincan; a smaller one is sent whole.
func TestRequestEmailCapsBody(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	big := ask("ra", "muse", strings.Repeat("~", 100<<10)+"TAILMARK", time.Unix(1_790_000_000, 0))
	small := ask("rb", "muse", strings.Repeat("~", 10<<10), time.Unix(1_790_000_001, 0))
	asks := &asksFake{asks: []envelope.Request{big, small}}
	w := New(Config{"instinct": optedIn()}, nil, Options{Debounce: time.Millisecond, WakeGrace: skipFollowUp, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag})
	w.Queued(context.Background(), big)
	w.Flush()
	m, n := rc.mail(t, 0), rc.mail(t, 1)
	if got := strings.Count(m.Text, "~"); got != 64<<10 || strings.Contains(m.Text, "TAILMARK") || !strings.Contains(m.Text, "fetch the rest with `tincan get ra`") {
		t.Fatalf("big body: %d of it sent, tail %v, note %v", got, strings.Contains(m.Text, "TAILMARK"), strings.Contains(m.Text, "fetch the rest"))
	}
	if got := strings.Count(n.Text, "~"); got != 10<<10 || strings.Contains(n.Text, "fetch the rest") {
		t.Fatalf("small body: %d of it sent, note %v", got, strings.Contains(n.Text, "fetch the rest"))
	}
}

// include_requests is for an agent whose primary path is email; on any other
// method or on a fallback the config is refused, naming the agent.
func TestLoadConfigIncludeRequests(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wake.json")
	good := `{"instinct":{"method":"email","email_to":"a@b","agentmail_inbox":"c@d","agentmail_key":"k","include_requests":true,
		"fallback":[{"method":"webhook","url":"http://y"}]}}`
	os.WriteFile(p, []byte(good), 0o600)
	c, err := LoadConfig(p)
	if err != nil || !c["instinct"].IncludeRequests {
		t.Fatalf("load: %+v %v", c, err)
	}
	for _, bad := range []string{
		`{"grokbot":{"method":"webhook","url":"http://x","include_requests":true}}`,
		`{"grokbot":{"method":"schedule","every":"5m","include_requests":true}}`,
		`{"grokbot":{"method":"email","email_to":"a@b","agentmail_inbox":"c@d","agentmail_key":"k","fallback":[{"method":"email","email_to":"a@b","agentmail_inbox":"c@d","agentmail_key":"k","include_requests":true}]}}`,
		`{"grokbot":{"method":"email","email_to":"a@b","agentmail_inbox":"c@d","agentmail_key":"k","include_requests":true,"fallback":[{"method":"webhook","url":"http://y","include_requests":true}]}}`,
		`{"grokbot":{"method":"webhook","url":"http://x","fallback":[{"method":"email","email_to":"a@b","agentmail_inbox":"c@d","agentmail_key":"k","include_requests":true}]}}`,
	} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "grokbot") || !strings.Contains(err.Error(), "include_requests") {
			t.Errorf("config %s: err = %v, want a rejection naming grokbot and include_requests", bad, err)
		}
	}
}

// pendingDue is when agent's pending nudge fires, and whether it is a
// request follow-up.
func pendingDue(w *Waker, agent string) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.pending[agent]
	if p == nil {
		return time.Time{}, false
	}
	return p.due, p.followUp
}

// One ask open with its follow-up armed: a second ask gets its first email
// on the debounce, not at the follow-up, and the follow-up keeps its due
// time.
func TestNewAskGetsEmailWhileFollowUpPending(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	start := time.Unix(1_790_000_000, 0)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "first", start)}}
	w := New(Config{"instinct": optedIn()}, nil, Options{
		Debounce: time.Millisecond, WakeGrace: time.Hour, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued:   func(string) int { return 1 },
		LastPoll: func(string) time.Time { return time.Time{} },
		Now:      fixedClock(start),
	})
	defer w.Stop()
	w.Queued(context.Background(), asks.asks[0])
	waitCalls(t, &rc, 1)
	waitFollowUpDue(t, w, "instinct")
	due, _ := pendingDue(w, "instinct")
	second := ask("rb", "claude-code", "second", start.Add(time.Second))
	asks.add(second)
	w.Queued(context.Background(), second)
	waitCalls(t, &rc, 2)
	if m := rc.mail(t, 1); m.Subject != subjectFor("claude-code", "rb") || !strings.Contains(m.Text, "second") {
		t.Fatalf("second email = %+v", m)
	}
	if got, followUp := pendingDue(w, "instinct"); !followUp || !got.Equal(due) {
		t.Fatalf("follow-up due %v (follow-up %v), want it kept at %v", got, followUp, due)
	}
}

// Within a wake, asks never emailed go first, then re-sends oldest first,
// and only of asks last emailed at least a wake grace ago.
func TestRequestEmailOrderNewBeforeResends(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "first", start), ask("rb", "muse", "second", start.Add(time.Second))}}
	w := New(Config{"instinct": optedIn()}, nil, Options{
		Debounce: time.Millisecond, WakeGrace: time.Hour, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued:   func(a string) int { open, _ := asks.open(a); return len(open) },
		LastPoll: func(string) time.Time { return time.Time{} },
		Now:      now.get,
	})
	defer w.Stop()
	w.Queued(context.Background(), asks.asks[0])
	waitCalls(t, &rc, 2)
	waitFollowUpDue(t, w, "instinct")
	now.set(start.Add(time.Hour))
	third := ask("rc", "claude-code", "third", start.Add(2*time.Second))
	asks.add(third)
	w.Queued(context.Background(), third)
	waitCalls(t, &rc, 5)
	var got []string
	for i := 2; i < 5; i++ {
		got = append(got, rc.mail(t, i).Subject)
	}
	want := []string{subjectFor("claude-code", "rc"), subjectFor("muse", "ra"), subjectFor("muse", "rb")}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %q, want %q", got, want)
	}
}

// max_per_hour counts wake fires, not emails: two asks re-sent on three
// follow-ups use four of five fires, and a third ask still gets its first
// email on the debounce.
func TestRequestEmailBudgetCountsFires(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(2)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "first", start), ask("rb", "muse", "second", start.Add(time.Second))}}
	cfg := optedIn()
	cfg.MaxPerHour = 5
	w := New(Config{"instinct": cfg}, st, Options{
		Debounce: time.Millisecond, WakeGrace: 20 * time.Millisecond, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued:   func(string) int { return int(queuedN.Load()) },
		LastPoll: func(string) time.Time { return time.Time{} },
		Now:      now.get,
	})
	w.Queued(context.Background(), asks.asks[0])
	waitCalls(t, &rc, 2)
	for i := range 3 {
		now.set(start.Add(time.Duration(i+1) * time.Minute))
		waitCalls(t, &rc, 4+2*i)
	}
	third := ask("rc", "claude-code", "third", start.Add(2*time.Second))
	asks.add(third)
	queuedN.Store(3)
	w.Queued(context.Background(), third)
	waitCalls(t, &rc, 9)
	if m := rc.mail(t, 8); m.Subject != subjectFor("claude-code", "rc") {
		t.Fatalf("ninth email = %q, want the third ask's first", m.Subject)
	}
	drainFollowUp(t, w, &queuedN)
	if got := strings.Join(events(t, st), ","); got != "woke,woke,woke,woke,woke" {
		t.Fatalf("audit = %s, want five fires and no skip", got)
	}
}

// A follow-up on a webhook fallback sends today's count-only message there,
// never request text; request emails go only to the primary email path.
func TestRequestEmailFallbackStepStaysCountOnly(t *testing.T) {
	var mail, hook recorder
	mts, hts := mail.server(t), hook.server(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "SECRET call Joe's Garage", time.Unix(1_790_000_000, 0))}}
	cfg := optedIn()
	cfg.Fallback = []Target{{Method: Webhook, URL: hts.URL}}
	w := New(Config{"instinct": cfg}, nil, Options{
		Debounce: time.Millisecond, WakeGrace: 20 * time.Millisecond, AgentMailAPI: mts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued:   func(string) int { return int(queuedN.Load()) },
		LastPoll: func(string) time.Time { return time.Time{} },
	})
	w.Queued(context.Background(), asks.asks[0])
	waitCalls(t, &hook, 1)
	drainFollowUp(t, w, &queuedN)
	if m := mail.mail(t, 0); m.Subject != subjectFor("muse", "ra") {
		t.Fatalf("first send = %+v, want the request email on the primary", m)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(hook.body(0)), &body); err != nil {
		t.Fatal(err)
	}
	if body["message"] != Message(1) || strings.Contains(hook.body(0), "SECRET") {
		t.Fatalf("fallback webhook = %s", hook.body(0))
	}
}

// An ask whose clarification was answered since its last email gets a new
// request email on the next wake, even inside its wake grace: the old email's
// tag no longer verifies, so the agent needs one with the current round.
func TestClarifiedAskIsEmailedAgain(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	start := time.Unix(1_790_000_000, 0)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "find the quote", start)}}
	w := New(Config{"instinct": optedIn()}, nil, Options{
		Debounce: time.Millisecond, WakeGrace: time.Hour, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued:   func(string) int { return 1 },
		LastPoll: func(string) time.Time { return time.Time{} },
		Now:      fixedClock(start),
	})
	defer w.Stop()
	w.Queued(context.Background(), asks.asks[0])
	waitCalls(t, &rc, 1)

	asks.mu.Lock()
	asks.asks[0].Exchanges = []envelope.Exchange{{Question: "which house?", Answer: "the Seattle one", At: start}}
	clarified := asks.asks[0]
	asks.mu.Unlock()
	w.Requeued(context.Background(), clarified)
	waitCalls(t, &rc, 2)
	if m := rc.mail(t, 1); !strings.Contains(m.Text, "the Seattle one") {
		t.Fatalf("second email = %+v, want the request with its answered clarification", m)
	}
}

// A request email that fails twice (send and its retry) is recorded as a
// failed wake, and the ask counts as never emailed, so the next wake sends
// it again without waiting out a wake grace.
func TestFailedRequestEmailIsSentAgain(t *testing.T) {
	var rc recorder
	rc.fail.Store(2)
	ts := rc.server(t)
	st := auditStore(t)
	start := time.Unix(1_790_000_000, 0)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "find the quote", start)}}
	w := New(Config{"instinct": optedIn()}, st, Options{
		Debounce: time.Millisecond, RetryDelay: time.Millisecond, WakeGrace: time.Hour, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued:   func(string) int { return 1 },
		LastPoll: func(string) time.Time { return time.Time{} },
		Now:      fixedClock(start),
	})
	defer w.Stop()
	w.Queued(context.Background(), asks.asks[0])
	waitCalls(t, &rc, 2)
	waitEvents(t, st, "wake_failed")
	w.Requeued(context.Background(), asks.asks[0])
	waitCalls(t, &rc, 3)
	if m := rc.mail(t, 2); m.Subject != subjectFor("muse", "ra") {
		t.Fatalf("third send = %q, want the failed ask's request email again", m.Subject)
	}
}

// With the hourly budget used up, a wake records wake_skipped and sends
// nothing; the asks it would have emailed stay never emailed and go out on
// the first wake the budget allows.
func TestBudgetSkippedRequestEmailWaits(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "first", start)}}
	cfg := optedIn()
	cfg.MaxPerHour = 1
	w := New(Config{"instinct": cfg}, st, Options{
		Debounce: time.Millisecond, WakeGrace: time.Hour, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued:   func(string) int { open, _ := asks.open(""); return len(open) },
		LastPoll: func(string) time.Time { return time.Time{} },
		Now:      now.get,
	})
	defer w.Stop()
	w.Queued(context.Background(), asks.asks[0])
	waitCalls(t, &rc, 1)
	second := ask("rb", "claude-code", "second", start.Add(time.Second))
	asks.add(second)
	w.Queued(context.Background(), second)
	waitEvents(t, st, "woke,wake_skipped")
	if n := rc.count(); n != 1 {
		t.Fatalf("sends with the budget spent = %d, want 1", n)
	}
	// After the window the skipped ask goes first, as never emailed, then
	// the first ask's re-send, now a wake grace old.
	now.set(start.Add(61 * time.Minute))
	w.Queued(context.Background(), second)
	waitCalls(t, &rc, 3)
	if m := rc.mail(t, 1); m.Subject != subjectFor("claude-code", "rb") {
		t.Fatalf("first send after the window = %q, want the skipped ask", m.Subject)
	}
	if m := rc.mail(t, 2); m.Subject != subjectFor("muse", "ra") {
		t.Fatalf("second send after the window = %q, want the first ask's re-send", m.Subject)
	}
}

// waitEvents waits up to 5s for the audit events to read want.
func waitEvents(t *testing.T, st *store.Store, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		if got = strings.Join(events(t, st), ","); got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("audit = %s, want %s", got, want)
}

// A new ask whose first email fails while a follow-up is pending still rides
// that follow-up: when the agent checks in before the follow-up fires, the
// follow-up turns into a fresh wake and the ask gets its email then.
func TestNewAskEmailFailureRidesFollowUp(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	start := time.Unix(1_790_000_000, 0)
	var now atomicTime
	now.set(start)
	var polled atomicTime
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "first", start)}}
	w := New(Config{"instinct": optedIn()}, nil, Options{
		Debounce: time.Millisecond, RetryDelay: time.Millisecond, WakeGrace: 50 * time.Millisecond, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued:   func(string) int { open, _ := asks.open(""); return len(open) },
		LastPoll: func(string) time.Time { return polled.get() },
		Now:      now.get,
	})
	defer w.Stop()
	w.Queued(context.Background(), asks.asks[0])
	waitCalls(t, &rc, 1)
	waitFollowUpDue(t, w, "instinct")
	second := ask("rb", "claude-code", "second", start.Add(time.Second))
	asks.add(second)
	rc.fail.Store(2) // the second ask's email and its retry
	w.Queued(context.Background(), second)
	waitCalls(t, &rc, 3)
	// The agent checks in after the failed email, still inside the first
	// ask's wake grace, so only the second ask is due.
	polled.set(start.Add(5 * time.Millisecond))
	now.set(start.Add(10 * time.Millisecond))
	waitCalls(t, &rc, 4)
	if m := rc.mail(t, 3); m.Subject != subjectFor("claude-code", "rb") {
		t.Fatalf("fourth send = %q, want the second ask's email after the check-in", m.Subject)
	}
}

// When the request emails go out but the count-only email for the rest
// fails, the wake is recorded failed and says one of its two emails failed,
// never "0 of 2".
func TestCountOnlyFailureIsCounted(t *testing.T) {
	var sends atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m sentMail
		_ = json.NewDecoder(r.Body).Decode(&m)
		sends.Add(1)
		if m.Subject == countOnlySubject {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	st := auditStore(t)
	start := time.Unix(1_790_000_000, 0)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "find the quote", start)}}
	w := New(Config{"instinct": optedIn()}, st, Options{
		Debounce: time.Millisecond, RetryDelay: time.Millisecond, WakeGrace: skipFollowUp, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued: func(string) int { return 2 }, // the ask plus one ping
	})
	w.Queued(context.Background(), asks.asks[0])
	w.Flush()
	evs, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, ev := range evs {
		if ev.Event == "wake_failed" {
			detail = ev.Detail
		}
	}
	if !strings.Contains(detail, "1 of 2 emails failed") {
		t.Fatalf("wake_failed detail = %q, want it to say 1 of 2 emails failed", detail)
	}
}

// When the agent checks in and finishes every ask before a pending
// follow-up fires, the follow-up sends nothing: no request is left to
// announce, so a count-only email would only spend the hourly budget.
func TestFollowUpAfterFinishedWorkSendsNothing(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	start := time.Unix(1_790_000_000, 0)
	var now, polled atomicTime
	now.set(start)
	asks := &asksFake{asks: []envelope.Request{ask("ra", "muse", "first", start)}}
	w := New(Config{"instinct": optedIn()}, nil, Options{
		Debounce: time.Millisecond, WakeGrace: 50 * time.Millisecond, AgentMailAPI: ts.URL + "/v0", OpenAsks: asks.open, RequestTag: fakeTag,
		Queued:   func(string) int { open, _ := asks.open(""); return len(open) },
		LastPoll: func(string) time.Time { return polled.get() },
		Now:      now.get,
	})
	w.Queued(context.Background(), asks.asks[0])
	waitCalls(t, &rc, 1)
	waitFollowUpDue(t, w, "instinct")
	second := ask("rb", "claude-code", "second", start.Add(time.Second))
	asks.add(second)
	w.Queued(context.Background(), second)
	waitCalls(t, &rc, 2)
	// The agent checks in and finishes both asks before the follow-up.
	asks.mu.Lock()
	asks.asks = nil
	asks.mu.Unlock()
	polled.set(start.Add(5 * time.Millisecond))
	now.set(start.Add(10 * time.Millisecond))
	w.Flush()
	w.Stop()
	if n := rc.count(); n != 2 {
		t.Fatalf("sends = %d, want 2: nothing after the agent finished its asks (last: %+v)", n, rc.mail(t, n-1))
	}
}
