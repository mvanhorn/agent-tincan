package relay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

const (
	instinctMail = "instinct@example.com"
	relayInbox   = "relay@agentmail.to"
	mailKey      = "am_SECRETKEY"
)

// fakeMail is an AgentMail inbox: it lists, gets and replies, and records
// every call, so a test can see what the relay read and wrote.
type fakeMail struct {
	t          *testing.T
	mu         sync.Mutex
	msgs       []wake.MailMessage
	lists      []string // the query of each list call
	gets       []string // message ids fetched in full
	replies    []sentReply
	writes     []string        // any other call: the relay must never make one
	auths      []string        // every Authorization header seen
	urls       []string        // every request URL seen
	fail       int             // reply calls left to fail with 500
	failedKeys []string        // idempotency keys of the failed reply calls
	broken     map[string]bool // message ids whose get fails
	limited    int             // list calls left to answer 429
	pageSize   int
}

type sentReply struct {
	MessageID, Key, Text string
}

func newFakeMail(t *testing.T) (*fakeMail, *httptest.Server) {
	f := &fakeMail{t: t, pageSize: 2}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /inboxes/{inbox}/messages", f.list)
	mux.HandleFunc("GET /inboxes/{inbox}/messages/{id}", f.get)
	mux.HandleFunc("POST /inboxes/{inbox}/messages/{id}/reply", f.reply)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		http.Error(w, "not here", http.StatusNotFound)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.urls = append(f.urls, r.URL.String())
		f.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/inboxes/"+relayInbox+"/") {
			f.mu.Lock()
			f.writes = append(f.writes, "wrong inbox "+r.URL.Path)
			f.mu.Unlock()
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// add puts a received message in the inbox and returns its id. A nil text
// is a message AgentMail has no reply-stripped text for.
func (f *fakeMail) add(from, subject string, text *string, at time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("<m%d.%d@mail.example>", len(f.msgs)+1, at.UnixNano())
	f.msgs = append(f.msgs, wake.MailMessage{MessageID: id, ThreadID: "t" + id, From: from, Subject: subject, Labels: []string{"received"}, Timestamp: at.UTC(), ExtractedText: text})
	return id
}

func (f *fakeMail) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	f.lists = append(f.lists, r.URL.RawQuery)
	if f.limited > 0 {
		f.limited--
		w.Header().Set("Retry-After", "2")
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	if q.Get("labels") != "received" || q.Has("include_spam") || q.Has("include_unauthenticated") {
		f.t.Errorf("list query %q: want labels=received and no spam or unauthenticated mail", r.URL.RawQuery)
	}
	after, err := time.Parse(time.RFC3339, q.Get("after"))
	if err != nil {
		f.t.Errorf("list after %q: %v", q.Get("after"), err)
	}
	var match []wake.MailMessage
	for _, m := range f.msgs {
		if m.Timestamp.After(after) {
			m.ExtractedText = nil // only a get carries text
			match = append(match, m)
		}
	}
	start, _ := strconv.Atoi(q.Get("page_token"))
	end := min(start+f.pageSize, len(match))
	page := map[string]any{"count": end - start, "messages": match[start:end]}
	if end < len(match) {
		page["next_page_token"] = strconv.Itoa(end)
	}
	json.NewEncoder(w).Encode(page)
}

func (f *fakeMail) get(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := r.PathValue("id")
	f.gets = append(f.gets, id)
	if f.broken[id] {
		http.Error(w, "boom", http.StatusBadGateway)
		return
	}
	for _, m := range f.msgs {
		if m.MessageID == id {
			json.NewEncoder(w).Encode(m)
			return
		}
	}
	http.NotFound(w, r)
}

func (f *fakeMail) reply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		f.failedKeys = append(f.failedKeys, r.Header.Get("Idempotency-Key"))
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	f.replies = append(f.replies, sentReply{MessageID: r.PathValue("id"), Key: r.Header.Get("Idempotency-Key"), Text: body.Text})
	json.NewEncoder(w).Encode(map[string]string{"message_id": "<out@agentmail.to>", "thread_id": "t"})
}

func (f *fakeMail) sent() []sentReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.replies)
}

func (f *fakeMail) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.gets)
}

func (f *fakeMail) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lists)
}

// replyEvents records reply wakes for askers.
type replyEvents struct {
	mu      sync.Mutex
	replied []envelope.Request
}

func (e *replyEvents) Queued(context.Context, envelope.Request) {}
func (e *replyEvents) Replied(_ context.Context, req envelope.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replied = append(e.replied, req)
}

type emailEnv struct {
	t      *testing.T
	m      *testrelay.Mesh
	mail   *fakeMail
	events *replyEvents
	box    *wake.AgentMail
	waits  []time.Duration
}

// newEmailEnv is a mesh where instinct is opted in to request emails, its
// inbox served by a fake AgentMail.
func newEmailEnv(t *testing.T, cfg relay.Config) *emailEnv {
	t.Helper()
	m := testrelay.New(t, cfg)
	m.Server.SetEmailTagKey([]byte("0123456789abcdef0123456789abcdef"))
	ev := &replyEvents{}
	m.Server.SetEvents(ev)
	f, srv := newFakeMail(t)
	e := &emailEnv{t: t, m: m, mail: f, events: ev}
	e.box = wake.NewAgentMail(srv.URL, relayInbox, mailKey, srv.Client())
	e.box.Sleep = func(ctx context.Context, d time.Duration) error {
		e.waits = append(e.waits, d)
		return nil
	}
	m.Server.SetEmailInboxes([]relay.EmailInbox{{Agent: "instinct", Address: instinctMail, Mail: e.box}})
	return e
}

// ask has muse ask instinct and returns the stored request.
func (e *emailEnv) ask(body string) envelope.Request {
	e.t.Helper()
	req, err := e.m.Client(e.t, "muse").Send(e.t.Context(), "instinct", body, envelope.KindAsk, "", false)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.stored(req.ID)
}

func (e *emailEnv) stored(id string) envelope.Request {
	e.t.Helper()
	req, _, err := e.m.Store.Request(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return req
}

// subject is the reply subject of req's current request email.
func (e *emailEnv) subject(req envelope.Request) string {
	return fmt.Sprintf("Re: Agent Tincan: request from %s [tincan %s.%s]", req.From, req.ID, e.m.Server.RequestEmailTag(req))
}

func (e *emailEnv) poll() { e.m.Server.PollEmailReplies(e.t.Context()) }

func (e *emailEnv) get(id string) envelope.Result {
	e.t.Helper()
	res, err := e.m.Client(e.t, "muse").Get(e.t.Context(), id, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// AE1: instinct answers a request by replying to its email. The answer is
// stored as instinct's, reaches the asker as any reply does, is audited
// without mail details, and instinct is told it was recorded in the
// thread. Nothing is written to the inbound message.
func TestEmailReplyIsRecorded(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("find the generator quote")
	id := e.mail.add("Instinct <"+strings.ToUpper("instinct")+"@Example.com>", e.subject(req),
		new("The quote is $4,200 from Acme.\n\nOn Mon, relay wrote:\n> Agent Tincan: request from muse [tincan "+req.ID+".deadbeef]"), time.Now())
	e.poll()

	res := e.get(req.ID)
	if res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.From != "instinct" {
		t.Fatalf("result = %+v, want answered by instinct", res)
	}
	if strings.Contains(res.Reply.Body, "[tincan ") || !strings.Contains(res.Reply.Body, "$4,200 from Acme") {
		t.Fatalf("stored reply = %q, want the answer without tag lines", res.Reply.Body)
	}
	e.events.mu.Lock()
	replied := slices.Clone(e.events.replied)
	e.events.mu.Unlock()
	if len(replied) != 1 || replied[0].ID != req.ID || replied[0].From != "muse" {
		t.Fatalf("reply wakes = %+v, want one for muse", replied)
	}
	sent := e.mail.sent()
	if len(sent) != 1 || sent[0].MessageID != id || sent[0].Key == "" || !strings.HasPrefix(sent[0].Text, "Agent Tincan: recorded") {
		t.Fatalf("responses = %+v, want one recorded reply in the thread", sent)
	}
	if strings.Contains(sent[0].Text, "4,200") || strings.Contains(sent[0].Text, "generator") {
		t.Fatalf("response %q carries request or reply text", sent[0].Text)
	}
	events, err := e.m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Event == "replied" && ev.RequestID == req.ID {
			found = true
			if ev.Actor != "instinct" || !strings.Contains(ev.Detail, `"via":"email"`) || !strings.Contains(ev.Detail, `"status":"answered"`) {
				t.Fatalf("audit = %+v, want replied by instinct via email", ev)
			}
		}
		for _, secret := range []string{e.m.Server.RequestEmailTag(req), "example.com", "Example.com", "4,200", id} {
			if strings.Contains(ev.Detail, secret) {
				t.Fatalf("audit %+v carries %q", ev, secret)
			}
		}
	}
	if !found {
		t.Fatal("no replied audit row")
	}
	if len(e.mail.writes) != 0 {
		t.Fatalf("relay wrote to the inbox: %v", e.mail.writes)
	}
	for _, a := range e.mail.auths {
		if a != "Bearer "+mailKey {
			t.Fatalf("Authorization = %q", a)
		}
	}
	for _, u := range e.mail.urls {
		if strings.Contains(u, mailKey) {
			t.Fatalf("key in URL %q", u)
		}
	}
}

// claim has instinct take req over tincan, under a live lease.
func (e *emailEnv) claim(req envelope.Request) {
	e.t.Helper()
	inst := e.m.Client(e.t, "instinct")
	if _, err := inst.Poll(e.t.Context(), 0); err != nil {
		e.t.Fatal(err)
	}
	if _, err := inst.Claim(e.t.Context(), req.ID); err != nil {
		e.t.Fatal(err)
	}
}

// wantResponses checks the response emails sent so far, by text prefix.
func (e *emailEnv) wantResponses(prefixes ...string) {
	e.t.Helper()
	sent := e.mail.sent()
	if len(sent) != len(prefixes) {
		e.t.Fatalf("responses = %+v, want %d", sent, len(prefixes))
	}
	for i, p := range prefixes {
		if !strings.HasPrefix(sent[i].Text, p) {
			e.t.Fatalf("response %d = %q, want %q", i, sent[i].Text, p)
		}
	}
}

func (e *emailEnv) decided(id string) bool {
	e.t.Helper()
	ok, err := e.m.Store.EmailDecided(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
}

// AE2: a request already answered over tincan is left as it is, and the
// email gets "not recorded: already answered".
func TestEmailReplyToAnsweredRequest(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("find the quote")
	e.claim(req)
	if _, err := e.m.Client(t, "instinct").Reply(t.Context(), req.ID, "over tincan", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	e.mail.add(instinctMail, e.subject(req), new("by email"), time.Now())
	e.poll()
	if res := e.get(req.ID); res.Reply == nil || res.Reply.Body != "over tincan" {
		t.Fatalf("result = %+v, want the tincan answer kept", res)
	}
	e.wantResponses("Agent Tincan: not recorded: this request was already answered")
}

// AE3: a valid tag from another sender is not recorded and gets no email.
func TestEmailReplyFromAnotherSender(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("find the quote")
	id := e.mail.add("Someone <someone@else.example>", e.subject(req), new("forged answer"), time.Now())
	e.poll()
	if res := e.get(req.ID); res.Status != envelope.StatusQueued || res.Reply != nil {
		t.Fatalf("result = %+v, want still queued", res)
	}
	e.wantResponses()
	if got := e.mail.fetched(); len(got) != 0 {
		t.Fatalf("fetched %v in full, want nothing", got)
	}
	if !e.decided(id) {
		t.Fatal("wrong-sender message has no email_replies row")
	}
}

// KTD8: a request the sandbox claimed over tincan is not answered by email
// while the claim's lease is live; once it lapses and the request is
// requeued, a fresh email reply is recorded.
func TestEmailReplyWhileClaimed(t *testing.T) {
	e := newEmailEnv(t, relay.Config{ClaimLease: time.Minute})
	req := e.ask("call the plumber")
	e.claim(req)
	e.mail.add(instinctMail, e.subject(req), new("done"), time.Now())
	e.poll()
	e.wantResponses("Agent Tincan: not recorded: this request is being handled over tincan")
	if res := e.get(req.ID); res.Status != envelope.StatusClaimed {
		t.Fatalf("status = %s, want claimed", res.Status)
	}

	later := time.Now().Add(2 * time.Minute)
	e.m.Store.SetClock(func() time.Time { return later })
	e.m.Server.Sweep(t.Context())
	if res := e.get(req.ID); res.Status != envelope.StatusQueued {
		t.Fatalf("status after lease = %s, want queued", res.Status)
	}
	e.mail.add(instinctMail, e.subject(req), new("done, booked for Tue"), time.Now())
	e.poll()
	if res := e.get(req.ID); res.Status != envelope.StatusAnswered || res.Reply.Body != "done, booked for Tue" {
		t.Fatalf("result = %+v, want the second email recorded", res)
	}
	e.wantResponses("Agent Tincan: not recorded: this request is being handled", "Agent Tincan: recorded")
}

// AE4: an email from before a clarification round no longer verifies.
func TestEmailReplyFromEarlierRound(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("book the house")
	round0 := e.subject(req)
	e.claim(req)
	if _, err := e.m.Client(t, "instinct").Reply(t.Context(), req.ID, "which house?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Client(t, "muse").Answer(t.Context(), req.ID, "the lake house"); err != nil {
		t.Fatal(err)
	}
	e.mail.add(instinctMail, round0, new("booked"), time.Now())
	e.poll()
	if res := e.get(req.ID); res.Status != envelope.StatusQueued {
		t.Fatalf("status = %s, want queued", res.Status)
	}
	e.wantResponses("Agent Tincan: not recorded: this email is out of date")

	// The current round's email still works.
	e.mail.add(instinctMail, e.subject(e.stored(req.ID)), new("booked the lake house"), time.Now())
	e.poll()
	if res := e.get(req.ID); res.Status != envelope.StatusAnswered {
		t.Fatalf("status = %s, want answered", res.Status)
	}
}

// A "failed:" or "declined:" first line sets the status, and the marker is
// not part of the stored reply.
func TestEmailReplyFailedAndDeclined(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	a, b := e.ask("find the quote"), e.ask("buy it")
	e.mail.add(instinctMail, e.subject(a), new("failed: quote not found"), time.Now())
	e.mail.add(instinctMail, e.subject(b), new("Declined: over budget\nask the owner"), time.Now())
	e.poll()
	if res := e.get(a.ID); res.Status != envelope.StatusFailed || res.Reply.Body != "quote not found" {
		t.Fatalf("a = %s %+v, want failed with %q", res.Status, res.Reply, "quote not found")
	}
	if res := e.get(b.ID); res.Status != envelope.StatusDeclined || res.Reply.Body != "over budget\nask the owner" {
		t.Fatalf("b = %s %+v, want declined", res.Status, res.Reply)
	}
	e.wantResponses("Agent Tincan: recorded", "Agent Tincan: recorded")
}

// Email cannot ask a clarifying question, and a message with no text of its
// own has nothing to record: each gets "not supported by email" once.
func TestEmailReplyUnsupported(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	reqs := []envelope.Request{e.ask("one"), e.ask("two"), e.ask("three"), e.ask("four"), e.ask("five")}
	e.mail.add(instinctMail, e.subject(reqs[0]), new("needs input: which house?"), time.Now())
	e.mail.add(instinctMail, e.subject(reqs[1]), new("  \n"), time.Now())
	e.mail.add(instinctMail, e.subject(reqs[2]), nil, time.Now())
	e.mail.add(instinctMail, e.subject(reqs[3]), new("\n> Re: Agent Tincan: request from muse [tincan "+reqs[3].ID+".x]\n"), time.Now())
	e.mail.add(instinctMail, e.subject(reqs[4]), new(""), time.Now()) // only an attachment
	e.mail.mu.Lock()
	e.mail.msgs[len(e.mail.msgs)-1].Attachments = []wake.MailAttachment{{AttachmentID: "a1"}}
	e.mail.mu.Unlock()
	e.poll()
	for _, r := range reqs {
		if res := e.get(r.ID); res.Status != envelope.StatusQueued {
			t.Fatalf("%s status = %s, want queued", r.Body, res.Status)
		}
	}
	const u = "Agent Tincan: not recorded: not supported by email"
	e.wantResponses(u, u, u, u, u)
}

// A reply over the reply limit is not recorded.
func TestEmailReplyTooLong(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("write it all")
	e.mail.add(instinctMail, e.subject(req), new(strings.Repeat("x", envelope.DefaultMaxBody+1)), time.Now())
	e.poll()
	e.wantResponses("Agent Tincan: not recorded: too long")
}

// The same message is decided once: on a second poll, and again by a
// restarted relay whose cursor starts over.
func TestEmailReplyDecidedOnce(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("find the quote")
	e.mail.add(instinctMail, e.subject(req), new("$4,200"), time.Now())
	e.poll()
	e.poll()

	restarted := relay.New(e.m.Dir, e.m.Store, relay.Config{})
	restarted.SetEmailTagKey([]byte("0123456789abcdef0123456789abcdef"))
	restarted.SetEmailInboxes([]relay.EmailInbox{{Agent: "instinct", Address: instinctMail, Mail: e.box}})
	restarted.PollEmailReplies(t.Context())

	e.wantResponses("Agent Tincan: recorded")
	events, err := e.m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events {
		if ev.Event == "replied" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("replied audit rows = %d, want 1", n)
	}
	if lists := e.mail.listCalls(); lists != 3 {
		t.Fatalf("list calls = %d, want 3", lists)
	}
}

// A response email that fails is sent on the next poll with the same
// idempotency key, and the reply is recorded once.
func TestEmailResponseRetried(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("find the quote")
	e.mail.fail = 1
	e.mail.add(instinctMail, e.subject(req), new("$4,200"), time.Now())
	e.poll()
	if res := e.get(req.ID); res.Status != envelope.StatusAnswered {
		t.Fatalf("status = %s, want answered before the response is sent", res.Status)
	}
	e.wantResponses()
	e.poll()
	e.wantResponses("Agent Tincan: recorded")
	if len(e.mail.failedKeys) != 1 || e.mail.failedKeys[0] != e.mail.sent()[0].Key {
		t.Fatalf("keys: failed %v, sent %q, want the same", e.mail.failedKeys, e.mail.sent()[0].Key)
	}
	if owed, _ := e.m.Store.UnsentEmailResponses(t.Context(), "instinct"); len(owed) != 0 {
		t.Fatalf("still owed: %+v", owed)
	}
}

// A reply that arrived while the relay was down, two hours before it
// started, is seen by the first poll.
func TestEmailReplyFromBeforeStart(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("find the quote")
	e.mail.add(instinctMail, e.subject(req), new("$4,200"), time.Now().Add(-2*time.Hour))
	e.poll()
	if res := e.get(req.ID); res.Status != envelope.StatusAnswered {
		t.Fatalf("status = %s, want answered", res.Status)
	}
}

// Other mail in a shared inbox, and spam carrying a valid tag, are never
// fetched in full, answered or recorded.
func TestEmailOtherMailUntouched(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("find the quote")
	ids := []string{
		e.mail.add("friend@example.org", "Lunch?", new("tomorrow?"), time.Now()),
		e.mail.add(instinctMail, "Re: Agent Tincan: requests waiting", new("on it"), time.Now()),
		e.mail.add("grok@example.org", "status", new("ok"), time.Now()),
	}
	spam := e.mail.add(instinctMail, e.subject(req), new("forged"), time.Now())
	e.mail.mu.Lock()
	e.mail.msgs[len(e.mail.msgs)-1].Labels = []string{"received", "spam"}
	e.mail.mu.Unlock()
	e.poll()
	if got := e.mail.fetched(); len(got) != 0 {
		t.Fatalf("fetched %v, want nothing", got)
	}
	e.wantResponses()
	for _, id := range append(ids, spam) {
		if e.decided(id) {
			t.Fatalf("%s has an email_replies row", id)
		}
	}
	if res := e.get(req.ID); res.Status != envelope.StatusQueued {
		t.Fatalf("status = %s, want queued", res.Status)
	}
	if lists := e.mail.listCalls(); lists != 2 {
		t.Fatalf("list calls = %d, want 2 pages of 2", lists)
	}
}

// A 429 is waited out for its Retry-After, and the poll carries on.
func TestEmailPollWaitsOutRateLimit(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	req := e.ask("find the quote")
	e.mail.limited = 1
	e.mail.add(instinctMail, e.subject(req), new("$4,200"), time.Now())
	e.poll()
	if len(e.waits) != 1 || e.waits[0] != 2*time.Second {
		t.Fatalf("waits = %v, want one of 2s", e.waits)
	}
	if res := e.get(req.ID); res.Status != envelope.StatusAnswered {
		t.Fatalf("status = %s, want answered after the wait", res.Status)
	}
}

// The inbox is polled only while the agent has an open ask or for 24 hours
// after its last ask closed.
func TestEmailPollWindow(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(d) }
	e := newEmailEnv(t, relay.Config{Now: clock})
	e.m.Store.SetClock(clock)
	e.poll()
	if n := e.mail.listCalls(); n != 0 {
		t.Fatalf("list calls with no ask ever = %d, want 0", n)
	}
	req := e.ask("find the quote")
	e.poll()
	if n := e.mail.listCalls(); n != 1 {
		t.Fatalf("list calls with an open ask = %d, want 1", n)
	}
	if err := e.m.Client(t, "muse").Cancel(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	advance(23 * time.Hour)
	e.poll()
	if n := e.mail.listCalls(); n != 2 {
		t.Fatalf("list calls 23h after the ask closed = %d, want 2", n)
	}
	advance(2 * time.Hour)
	e.poll()
	if n := e.mail.listCalls(); n != 2 {
		t.Fatalf("list calls 25h after the ask closed = %d, want still 2", n)
	}
}

// Run polls on its own ticker.
func TestEmailPollRuns(t *testing.T) {
	e := newEmailEnv(t, relay.Config{EmailPollEvery: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); e.m.Server.Run(ctx) }()
	defer func() { cancel(); <-done }()
	req := e.ask("find the quote")
	e.mail.add(instinctMail, e.subject(req), new("$4,200"), time.Now())
	deadline := time.Now().Add(5 * time.Second)
	for e.get(req.ID).Status != envelope.StatusAnswered {
		if time.Now().After(deadline) {
			t.Fatal("Run never recorded the email reply")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A message that cannot be fetched does not hold up the rest of the poll,
// and is decided on a later poll once it can be.
func TestEmailReplyFetchFailureRetried(t *testing.T) {
	e := newEmailEnv(t, relay.Config{})
	a, b := e.ask("one"), e.ask("two")
	stuck := e.mail.add(instinctMail, e.subject(a), new("answer one"), time.Now())
	e.mail.add(instinctMail, e.subject(b), new("answer two"), time.Now())
	e.mail.broken = map[string]bool{stuck: true}
	e.poll()
	if res := e.get(a.ID); res.Status != envelope.StatusQueued {
		t.Fatalf("a = %s, want queued while its message cannot be fetched", res.Status)
	}
	if res := e.get(b.ID); res.Status != envelope.StatusAnswered {
		t.Fatalf("b = %s, want answered", res.Status)
	}
	e.mail.mu.Lock()
	e.mail.broken = nil
	e.mail.mu.Unlock()
	e.poll()
	if res := e.get(a.ID); res.Status != envelope.StatusAnswered {
		t.Fatalf("a = %s, want answered on the next poll", res.Status)
	}
	e.wantResponses("Agent Tincan: recorded", "Agent Tincan: recorded")
}
