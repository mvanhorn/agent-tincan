package pass

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// passRig is a mesh with the pass service (kind pass) on muse's machine,
// chatgpt-web (kind chatgpt-web) on instinct's, every client calling the
// relay as tincan version, and the owner's claude.ai history holding one
// chat, abc-3, updated now.
type passRig struct {
	mesh   *testrelay.Mesh
	web    *client.Relay
	svc    *Service
	reader *fakeReader
}

const tomatoes = "plant the tomatoes in May"

func newPassRig(t *testing.T, version string) *passRig {
	t.Helper()
	old := client.Version
	client.Version = version
	t.Cleanup(func() { client.Version = old })
	m := testrelay.New(t, relay.Config{})
	me := m.JoinOnMachineOf(t, "muse", "pass")
	web := m.JoinOnMachineOf(t, "instinct", "chatgpt-web")
	admin := m.Client(t, "admin")
	for name, kind := range map[string]string{"pass": "pass", "chatgpt-web": "chatgpt-web"} {
		if err := admin.SetKind(t.Context(), name, kind); err != nil {
			t.Fatal(err)
		}
	}
	// The destination calls the relay once, so the roster knows its build.
	if _, err := web.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	c := chat(3)
	c.Source, c.ID, c.UpdatedAt = history.SourceClaudeAI, "abc-3", time.Now()
	c.Messages[len(c.Messages)-1].Text = tomatoes
	rd := &fakeReader{src: history.SourceClaudeAI, convs: []history.Conversation{c}}
	svc := &Service{
		Relay: me,
		Reader: func(src history.Source) (history.Reader, bool) {
			return rd, src == history.SourceClaudeAI
		},
		Allowlist:        history.StaticAllowlist(history.AllowAll),
		HistoryAllowlist: history.StaticAllowlist(history.AllowAll),
		ReplyWait:        10 * time.Second,
		RenewEvery:       time.Second,
		Log:              testLog{t},
	}
	return &passRig{mesh: m, web: web, svc: svc, reader: rd}
}

// ask sends body to pass from asker, approves it as the owner, and has
// the service handle it, returning the asker's view of the request.
func (r *passRig) ask(t *testing.T, asker, body string) client.Result {
	t.Helper()
	c := r.mesh.Client(t, asker)
	req, err := c.Send(t.Context(), "pass", body, envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != envelope.StatusHeld {
		t.Fatalf("pass ask status %q, want held", req.Status)
	}
	if r.reader.reads != 0 {
		t.Fatal("history was read before the owner approved")
	}
	if err := r.mesh.Client(t, "admin").Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := r.svc.PollOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("PollOnce = %d, %v", n, err)
	}
	res, err := c.Get(t.Context(), req.ID, 0)
	if err != nil || res.Reply == nil {
		t.Fatalf("no reply: %+v %v", res, err)
	}
	return res
}

// fakeWeb plays the destination web agent for one request: it claims it,
// runs before (if set), and replies with what reply returns. It sends the
// request it took on the returned channel.
func fakeWeb(t *testing.T, c *client.Relay, before func(envelope.Request), reply func(envelope.Request) (string, envelope.Status)) <-chan envelope.Request {
	t.Helper()
	got := make(chan envelope.Request, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			in, err := c.Poll(ctx, time.Second)
			if err != nil || len(in.Requests) == 0 {
				continue
			}
			req := in.Requests[0]
			if _, err := c.Claim(ctx, req.ID); err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if before != nil {
				before(req)
			}
			body, status := reply(req)
			if _, err := c.Reply(ctx, req.ID, body, status); err != nil && ctx.Err() == nil {
				t.Errorf("reply: %v", err)
			}
			got <- req
			return
		}
	}()
	return got
}

// Covers AE5. An approved ask to pass reads the named chat and sends one
// seed to the destination, a child of the ask grouped pass-<id> whose
// chain carries the asker; the asker gets the new conversation's id and
// none of the chat's text, even when the destination's answer echoes it.
func TestPassApprovedSendsOneSeedAndRepliesIDOnly(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	got := fakeWeb(t, r.web, nil, func(envelope.Request) (string, envelope.Status) {
		return "I have it: " + tomatoes + "\n\nChatGPT conversation: newconv-7", envelope.StatusAnswered
	})
	res := r.ask(t, "grokbot", "from: https://claude.ai/chat/abc-3\nto: chatgpt-web")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "ChatGPT conversation: newconv-7") {
		t.Fatalf("reply: %s %q", res.Status, res.Reply.Body)
	}
	if strings.Contains(res.Reply.Body, "tomatoes") || strings.Contains(res.Reply.Body, "prompt") {
		t.Fatalf("the reply carries the chat's text: %q", res.Reply.Body)
	}
	seed := <-got
	if !strings.HasPrefix(seed.Body, "new chat\n") || !strings.Contains(seed.Body, tomatoes) {
		t.Fatalf("seed = %q", seed.Body)
	}
	if seed.From != "pass" || seed.ParentID != res.Request.ID || seed.Group != "pass-"+res.Request.ID || !slices.Contains(seed.Chain, "grokbot") {
		t.Fatalf("seed from %s parent %s group %s chain %v", seed.From, seed.ParentID, seed.Group, seed.Chain)
	}
	members, err := r.svc.Relay.GroupMembers(t.Context(), seed.Group)
	if err != nil || len(members) != 1 {
		t.Fatalf("seeds sent = %+v %v, want one", members, err)
	}
}

// A malformed body, or one asking for "latest", fails naming the form,
// and reads and sends nothing.
func TestPassRefusesMalformedAndLatest(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	for _, body := range []string{"pass my latest claude chat to chatgpt-web", "from: claude-ai latest\nto: chatgpt-web"} {
		res := r.ask(t, "grokbot", body)
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "from: <chat URL>") {
			t.Fatalf("%q: %s %q", body, res.Status, res.Reply.Body)
		}
	}
	if r.reader.reads != 0 || r.mesh.Server.QueuedCount("chatgpt-web") != 0 {
		t.Fatalf("reads %d, queued %d", r.reader.reads, r.mesh.Server.QueuedCount("chatgpt-web"))
	}
}

// An asker that is not on pass-allow.txt, or whose chain is not all on
// history-allow.txt, is declined before anything is read.
func TestPassChecksBothAllowlists(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	body := "from: claude-ai abc-3\nto: chatgpt-web"
	r.svc.Allowlist = history.StaticAllowlist("muse")
	if res := r.ask(t, "grokbot", body); res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "pass allowlist") {
		t.Fatalf("pass allowlist: %s %q", res.Status, res.Reply.Body)
	}
	r.svc.Allowlist = history.StaticAllowlist(history.AllowAll)
	r.svc.HistoryAllowlist = history.StaticAllowlist("muse")
	if res := r.ask(t, "grokbot", body); res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "history allowlist") {
		t.Fatalf("history allowlist: %s %q", res.Status, res.Reply.Body)
	}
	if r.reader.reads != 0 {
		t.Fatalf("history read %d times for declined asks", r.reader.reads)
	}
}

// A chat last updated outside the owner's history window is refused, and
// nothing is sent.
func TestPassRefusesChatOutsideWindow(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	r.reader.convs[0].UpdatedAt = time.Now().Add(-60 * 24 * time.Hour)
	res := r.ask(t, "grokbot", "from: claude-ai abc-3\nto: chatgpt-web")
	if res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "outside the owner's history window") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if r.mesh.Server.QueuedCount("chatgpt-web") != 0 {
		t.Fatal("a seed was sent for a chat outside the window")
	}
}

// A destination running a build older than the continuation refusal is
// refused before anything is read.
func TestPassRefusesOldDestination(t *testing.T) {
	r := newPassRig(t, "0.17.0")
	res := r.ask(t, "grokbot", "from: claude-ai abc-3\nto: chatgpt-web")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "runs tincan 0.17.0") || !strings.Contains(res.Reply.Body, GuardVersion) {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if r.reader.reads != 0 {
		t.Fatalf("history read %d times for an old destination", r.reader.reads)
	}
}

// A seed whose destination runs out of messages is never moved, even
// with another web chat teammate free: its handback is refused, and the
// pass fails with the destination's limit reason.
func TestPassSeedNeverFailsOver(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	other := r.mesh.JoinOnMachineOf(t, "grokbot", "claude-web")
	if err := r.mesh.Client(t, "admin").SetKind(t.Context(), "claude-web", "claude-web"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Poll(t.Context(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	var handback error
	got := fakeWeb(t, r.web, func(req envelope.Request) {
		if err := r.web.ReportWebStatus(t.Context(), envelope.WebStatus{Site: "chatgpt.com", State: envelope.WebLimited, Until: until, ObservedAt: time.Now()}); err != nil {
			t.Error(err)
		}
		_, handback = r.web.Handback(t.Context(), req.ID, until)
	}, func(envelope.Request) (string, envelope.Status) {
		return "Sorry, ChatGPT is out of messages until 14:00 UTC.", envelope.StatusFailed
	})
	res := r.ask(t, "grokbot", "from: claude-ai abc-3\nto: chatgpt-web")
	seed := <-got
	if handback == nil {
		t.Fatal("the relay took back a pass seed")
	}
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "out of messages") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	after, err := r.svc.Relay.Get(t.Context(), seed.ID, 0)
	if err != nil || after.Request.To != "chatgpt-web" || len(after.Request.MovedFrom) != 0 {
		t.Fatalf("seed after the limit: to %s moved %v (%v)", after.Request.To, after.Request.MovedFrom, err)
	}
}

// pass serve refuses to run unless the relay stores kind pass for it,
// and claims nothing.
func TestPassServeRefusesWithoutPassKind(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	me := m.JoinOnMachineOf(t, "muse", "pass")
	req, err := m.Client(t, "grokbot").Send(t.Context(), "pass", "from: claude-ai abc-3\nto: chatgpt-web", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Relay: me, Log: testLog{t}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = svc.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "refuses to run") || !strings.Contains(err.Error(), "tincan kind pass pass") {
		t.Fatalf("Run = %v", err)
	}
	res, err := m.Client(t, "grokbot").Get(t.Context(), req.ID, 0)
	if err != nil || res.Status != envelope.StatusQueued {
		t.Fatalf("the ask after a refused start: %s %v", res.Status, err)
	}
}

// A destination that fails after the seed went out did not confirm the
// new chat, but the chat was sent: the reply says so, with the status and
// the destination's reason, and never "Nothing was passed".
func TestPassUnconfirmedSeedSaysItWasSent(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	fakeWeb(t, r.web, nil, func(envelope.Request) (string, envelope.Status) {
		return "Sorry, ChatGPT did not finish answering in time. The message was sent; the conversation is newconv-9.", envelope.StatusFailed
	})
	res := r.ask(t, "grokbot", "from: claude-ai abc-3\nto: chatgpt-web")
	body := res.Reply.Body
	if res.Status != envelope.StatusFailed || strings.Contains(body, "Nothing was passed") || !strings.Contains(body, "was sent to chatgpt-web") ||
		!strings.Contains(body, "did not confirm") || !strings.Contains(body, "(failed)") || !strings.Contains(body, "did not finish answering in time") {
		t.Fatalf("%s %q", res.Status, body)
	}
}

// A redelivered ask whose seed already went out waits for that seed:
// it reads nothing, sends no second seed, and answers from the first.
func TestPassRedeliveryReusesSentSeed(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	c := r.mesh.Client(t, "grokbot")
	req, err := c.Send(t.Context(), "pass", "from: claude-ai abc-3\nto: chatgpt-web", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.mesh.Client(t, "admin").Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	// The first delivery sent its seed before the service stopped.
	claimed, err := r.svc.Relay.Claim(t.Context(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Relay.SendGrouped(t.Context(), "chatgpt-web", "new chat\nthe first seed", envelope.KindAsk, req.ID, "pass-"+req.ID); err != nil {
		t.Fatal(err)
	}
	got := fakeWeb(t, r.web, nil, func(envelope.Request) (string, envelope.Status) {
		return "ChatGPT conversation: newconv-8", envelope.StatusAnswered
	})
	r.svc.Handle(t.Context(), claimed)
	res, err := c.Get(t.Context(), req.ID, 0)
	if err != nil || res.Status != envelope.StatusAnswered || res.Reply == nil || !strings.Contains(res.Reply.Body, "newconv-8") {
		t.Fatalf("reply: %+v %v", res, err)
	}
	if seed := <-got; seed.Body != "new chat\nthe first seed" {
		t.Fatalf("the destination got %q, not the first seed", seed.Body)
	}
	members, err := r.svc.Relay.GroupMembers(t.Context(), "pass-"+req.ID)
	if err != nil || len(members) != 1 || r.reader.reads != 0 {
		t.Fatalf("seeds %+v (%v), reads %d; want one seed and no read", members, err, r.reader.reads)
	}
}

// A destination that does not finish within ReplyWait leaves the pass
// "Not passed yet", naming the seed the owner can check.
func TestPassReplyWaitTimeout(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	r.svc.ReplyWait = time.Second
	res := r.ask(t, "grokbot", "from: claude-ai abc-3\nto: chatgpt-web")
	if res.Status != envelope.StatusFailed || !strings.HasPrefix(res.Reply.Body, "Not passed yet: chatgpt-web did not open the chat within 1s") || !strings.Contains(res.Reply.Body, "tincan get ") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if r.mesh.Server.QueuedCount("chatgpt-web") != 1 {
		t.Fatal("the seed was not sent")
	}
}

// An answer that names no conversation fails: the asker gets no id.
func TestPassAnsweredWithoutConversationFails(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	fakeWeb(t, r.web, nil, func(envelope.Request) (string, envelope.Status) {
		return "Done, I read it all.", envelope.StatusAnswered
	})
	res := r.ask(t, "grokbot", "from: claude-ai abc-3\nto: chatgpt-web")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "answered without naming its conversation") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

// A history window file that is not valid fails the pass closed: nothing
// is read or sent.
func TestPassBadWindowFileFailsClosed(t *testing.T) {
	r := newPassRig(t, GuardVersion)
	r.svc.WindowFile = filepath.Join(t.TempDir(), "history-window.json")
	if err := os.WriteFile(r.svc.WindowFile, []byte(`{"days": "thirty"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := r.ask(t, "grokbot", "from: claude-ai abc-3\nto: chatgpt-web")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "Nothing was passed") || !strings.Contains(res.Reply.Body, "history-window.json") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if r.reader.reads != 0 || r.mesh.Server.QueuedCount("chatgpt-web") != 0 {
		t.Fatalf("reads %d, queued %d", r.reader.reads, r.mesh.Server.QueuedCount("chatgpt-web"))
	}
}

// Every log line starts with the service clock's local time, to the
// millisecond, so it lines up with the relay's UTC trace.
func TestLogLinesAreTimestampedWithTheServiceClock(t *testing.T) {
	at := time.Date(2026, 11, 8, 21, 55, 7, 137_000_000, time.FixedZone("PST", -8*3600))
	var log strings.Builder
	svc := &Service{Log: &log, Now: func() time.Time { return at }}
	svc.logf("request %s: declined", "r1")
	if got, want := log.String(), "2026-11-08T21:55:07.137-08:00 tincan pass: request r1: declined\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}
