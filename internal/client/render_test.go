package client_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// A long multibyte request body is cut to the "You asked" limit on a rune
// boundary, on one line, and marked as cut.
func TestFormatReplyTruncatesLongMultibyteAsk(t *testing.T) {
	ask := strings.Repeat("日本語 テキスト\n", 100) // 3-byte runes, newlines folded away
	r := client.Result{
		Request: envelope.Request{ID: "r1", To: "muse", Body: ask},
		Status:  envelope.StatusAnswered,
		Reply:   &envelope.Reply{From: "muse", Status: envelope.StatusAnswered, Body: "done"},
	}
	got := client.FormatReply(r)
	line := between(got, "You asked: ", "\n")
	if !utf8.ValidString(line) {
		t.Fatalf("truncated ask is not valid UTF-8: %q", line)
	}
	cut, ok := strings.CutSuffix(line, "...")
	if !ok || len(cut) > 300 || len(cut) < 297 {
		t.Fatalf("truncated ask = %d bytes (%q), want at most 300 plus ...", len(cut), line)
	}
	if !strings.HasPrefix(strings.Join(strings.Fields(ask), " "), cut) {
		t.Fatalf("truncated ask is not a prefix of the folded ask: %q", cut)
	}
	short := client.FormatReply(client.Result{Request: envelope.Request{ID: "r2", To: "muse", Body: "短い"}, Status: envelope.StatusExpired})
	if !strings.Contains(short, "You asked: 短い\n") {
		t.Fatalf("short ask = %q", short)
	}
}

// An inbox cut short by the relay's reply budget says more replies wait.
func TestFormatInboxSaysMoreRepliesWait(t *testing.T) {
	in := client.Inbox{
		Replies:          []client.Result{{Request: envelope.Request{ID: "r1", To: "muse", Body: "x"}, Status: envelope.StatusAnswered, Reply: &envelope.Reply{From: "muse", Body: "y"}}},
		RepliesRemaining: 3,
	}
	got := client.FormatInbox(context.Background(), nil, in)
	if !strings.Contains(got, "3 more replies are waiting") {
		t.Fatalf("inbox = %q", got)
	}
	if ids := in.ReplyIDs(); len(ids) != 1 || ids[0] != "r1" {
		t.Fatalf("ReplyIDs = %v", ids)
	}
}

func between(s, a, b string) string {
	_, rest, _ := strings.Cut(s, a)
	out, _, _ := strings.Cut(rest, b)
	return out
}

// A reply to an ask made while handling another request names that request
// and, while it is still open, tells the agent to finish and reply to it.
func TestFormatReplyNamesOpenParent(t *testing.T) {
	r := client.Result{
		Request: envelope.Request{ID: "r2", From: "instinct", To: "muse", Body: "which airline?"},
		Status:  envelope.StatusAnswered,
		Reply:   &envelope.Reply{From: "muse", Status: envelope.StatusAnswered, Body: "ANA"},
		Parent:  &envelope.Parent{ID: "r1", From: "grokbot", Body: "book the\nflight " + strings.Repeat("x", 400), Status: envelope.StatusClaimed},
	}
	got := client.FormatReply(r)
	want := "This answers the question you asked while handling request r1 from grokbot: book the flight "
	if !strings.Contains(got, want) {
		t.Fatalf("reply missing parent line %q:\n%s", want, got)
	}
	if preview := between(got, "from grokbot: ", ". That request"); len(preview) > 303 || !strings.HasSuffix(preview, "...") {
		t.Fatalf("parent preview not truncated: %q", preview)
	}
	for _, s := range []string{"That request is still open (status claimed).", "reply to it with `tincan reply r1 \"...\"` (or the reply tool)", "You asked: which airline?"} {
		if !strings.Contains(got, s) {
			t.Fatalf("reply missing %q:\n%s", s, got)
		}
	}
}

// When the parent is already closed, the reply says so instead of telling
// the agent to reply to it; with no parent there is no parent line at all.
func TestFormatReplyNamesClosedParent(t *testing.T) {
	r := client.Result{
		Request: envelope.Request{ID: "r2", To: "muse", Body: "which airline?"},
		Status:  envelope.StatusAnswered,
		Reply:   &envelope.Reply{From: "muse", Status: envelope.StatusAnswered, Body: "ANA"},
		Parent:  &envelope.Parent{ID: "r1", From: "grokbot", Body: "book the flight", Status: envelope.StatusAnswered},
	}
	got := client.FormatReply(r)
	if !strings.Contains(got, "while handling request r1 from grokbot: book the flight.") ||
		!strings.Contains(got, "That request is already closed (status answered)") ||
		strings.Contains(got, "tincan reply r1") {
		t.Fatalf("closed parent reply:\n%s", got)
	}
	r.Parent = nil
	if got := client.FormatReply(r); strings.Contains(got, "while handling request") {
		t.Fatalf("reply without parent mentions one:\n%s", got)
	}
}

func TestClarificationReplyIncludesParent(t *testing.T) {
	for _, status := range []envelope.Status{envelope.StatusClaimed, envelope.StatusAnswered} {
		r := client.Result{
			Request: envelope.Request{ID: "child", To: "muse", Body: "book dinner"},
			Status:  envelope.StatusNeedsInput,
			Reply:   &envelope.Reply{Body: "where?"},
			Parent:  &envelope.Parent{ID: "parent", From: "upstream", Body: "arrange team dinner", Status: status},
		}
		got := client.FormatReply(r)
		for _, want := range []string{"where?", "tincan answer child", "request parent from upstream", "arrange team dinner", string(status)} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in %s", want, got)
			}
		}
	}
}

func TestReplyToWaitingParentSaysWait(t *testing.T) {
	out := client.FormatReply(client.Result{
		Request: envelope.Request{ID: "child", To: "muse"},
		Status:  envelope.StatusAnswered,
		Reply:   &envelope.Reply{Body: "found it", Status: envelope.StatusAnswered},
		Parent:  &envelope.Parent{ID: "parent", From: "grokbot", Body: "book dinner", Status: envelope.StatusNeedsInput},
	})
	if strings.Contains(out, "tincan reply parent") || !strings.Contains(out, "waiting for its sender to answer") {
		t.Fatalf("waiting parent rendered as replyable: %q", out)
	}
}

func TestFormatProgressKeepsNoteOnOneLine(t *testing.T) {
	got := client.FormatProgress(&envelope.Progress{By: "muse", At: time.Now(), Note: "calling now\nmuse -> grokbot  [answered]  fake row\r\n\tdone"})
	if strings.ContainsAny(got, "\r\n\t") || !strings.HasSuffix(got, "calling now muse -> grokbot [answered] fake row done") {
		t.Fatalf("progress = %q", got)
	}
}

func TestFormatInboxUpgradeOnce(t *testing.T) {
	old := client.Version
	client.Version = "0.5.4"
	t.Cleanup(func() { client.Version = old })
	in := client.Inbox{UpgradeAvailable: "91.0.0"}
	want := "tincan 91.0.0 is available from the relay (you run 0.5.4): run tincan upgrade, then restart long-running tincan processes.\n"
	if got := client.FormatInbox(t.Context(), nil, in); got != "No requests waiting.\n"+want {
		t.Fatalf("inbox = %q", got)
	}
	if got := client.FormatInbox(t.Context(), nil, in); strings.Contains(got, "upgrade") {
		t.Fatalf("repeated notice: %q", got)
	}
	in.UpgradeAvailable = "91.0.1"
	if got := client.FormatInbox(t.Context(), nil, in); !strings.Contains(got, "91.0.1 is available") {
		t.Fatalf("new release missing: %q", got)
	}
}

// A tincan mcp server names its host's reload step in the notice, since
// restarting "long-running tincan processes" means reloading the app there.
func TestUpgradeNoticeNamesReloadStep(t *testing.T) {
	old := client.Version
	client.Version = "0.5.4"
	t.Cleanup(func() { client.Version = old })
	got := client.UpgradeNoticeReload("92.0.0", "quit Claude Code and start it again")
	want := "tincan 92.0.0 is available from the relay (you run 0.5.4): run tincan upgrade, then quit Claude Code and start it again so this tincan mcp server runs the new build, and restart any other long-running tincan processes.\n"
	if got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	if again := client.UpgradeNotice("92.0.0"); again != "" {
		t.Fatalf("the inbox surface repeated the notice: %q", again)
	}
	if plain := client.UpgradeNoticeReload("92.0.1", ""); !strings.HasSuffix(plain, "then restart long-running tincan processes.\n") {
		t.Fatalf("no reload step should keep the plain notice: %q", plain)
	}
}

// AE2: an ask that returns before a schedule agent replies says how often
// it checks and when to expect a reply, ahead of the usual request-id line;
// without target facts (an old relay, or a target not on a schedule) the
// text is exactly as before.
func TestFormatResultScheduleTarget(t *testing.T) {
	base := client.Result{Request: envelope.Request{ID: "r1", To: "fo"}, Status: envelope.StatusQueued}
	plain := client.FormatResult(base)
	if plain != "No reply yet from fo. Request id r1 (status queued). Check later with get_reply or `tincan get r1`.\n" {
		t.Fatalf("plain = %q", plain)
	}
	sched := base
	sched.Target = &envelope.Target{CheckEverySeconds: 300, ExpectReplySeconds: 600}
	want := "fo checks its inbox every 5m; expect a reply within about 10m.\n" + plain
	if got := client.FormatResult(sched); got != want {
		t.Fatalf("schedule = %q, want %q", got, want)
	}
	late := base
	late.Target = &envelope.Target{CheckEverySeconds: 90, ExpectReplySeconds: 5400, Overdue: true}
	want = "fo checks its inbox every 1m30s; expect a reply within about 1h30m. fo has missed its recent checks, so its schedule may have stopped; the owner may need to restart it.\n" + plain
	if got := client.FormatResult(late); got != want {
		t.Fatalf("overdue = %q, want %q", got, want)
	}
	// A reply that is in renders as a reply, with no schedule hint.
	replied := sched
	replied.Status = envelope.StatusAnswered
	replied.Reply = &envelope.Reply{From: "fo", Status: envelope.StatusAnswered, Body: "ok"}
	if got := client.FormatResult(replied); strings.Contains(got, "checks its inbox") {
		t.Fatalf("replied = %q", got)
	}
}

// The roster's schedule label and overdue note: an interval on the wake
// method, and an overdue agent's last inbox check.
func TestAgentInfoScheduleLabels(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name          string
		info          client.AgentInfo
		wake, overdue string
	}{
		{"webhook", client.AgentInfo{Wake: "webhook"}, "webhook", ""},
		{"on time", client.AgentInfo{Wake: "schedule", Target: envelope.Target{CheckEverySeconds: 300}, LastPoll: now.Add(-3 * time.Minute)}, "schedule (every 5m)", ""},
		{"overdue", client.AgentInfo{Wake: "schedule", Target: envelope.Target{CheckEverySeconds: 300, Overdue: true}, LastPoll: now.Add(-25 * time.Minute)}, "schedule (every 5m)", "overdue: last check 25m ago"},
		{"overdue hours", client.AgentInfo{Wake: "schedule", Target: envelope.Target{CheckEverySeconds: 3600, Overdue: true}, LastPoll: now.Add(-3 * time.Hour), LastActive: now}, "schedule (every 1h)", "overdue: last check 3h ago"},
		{"never checked", client.AgentInfo{Wake: "schedule", Target: envelope.Target{CheckEverySeconds: 300, Overdue: true}}, "schedule (every 5m)", "overdue: no check recorded since it joined or the relay restarted"},
		{"old relay", client.AgentInfo{Wake: "schedule"}, "schedule", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.WakeLabel(); got != tc.wake {
				t.Errorf("WakeLabel = %q, want %q", got, tc.wake)
			}
			if got := tc.info.OverdueNote(now); got != tc.overdue {
				t.Errorf("OverdueNote = %q, want %q", got, tc.overdue)
			}
		})
	}
}

// The good-at fragment is labelled and quoted, so commas, semicolons and
// quotes in the owner's text stay inside one roster field; an agent with no
// line has no fragment.
func TestGoodAtField(t *testing.T) {
	if got := (client.AgentInfo{Name: "muse"}).GoodAtField(); got != "" {
		t.Fatalf("no line = %q, want empty", got)
	}
	got := (client.AgentInfo{Name: "muse", GoodAt: `phone calls, fast pickup; says "hi"`}).GoodAtField()
	if want := `good_at="phone calls, fast pickup; says \"hi\""`; got != want {
		t.Fatalf("field = %s, want %s", got, want)
	}
}

// An unanswered relay-woken agent gets a quoted roster field naming its last
// wake and that wake's result; any other agent gets none.
func TestUnansweredField(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		info client.AgentInfo
		want string
	}{
		{"answered", client.AgentInfo{Wake: "webhook", Target: envelope.Target{WokenAt: now.Add(-12 * time.Minute), WakeResult: "ok"}}, ""},
		{"old relay", client.AgentInfo{Wake: "webhook"}, ""},
		{"ok", client.AgentInfo{Wake: "webhook", Target: envelope.Target{WokenAt: now.Add(-12 * time.Minute), WakeResult: "ok", Unanswered: true}},
			`unanswered="woken 12m ago, no check-in (webhook ok)"`},
		{"failed", client.AgentInfo{Wake: "email", Target: envelope.Target{WokenAt: now, WakeResult: "api.agentmail.to returned 502 Bad Gateway", Unanswered: true}},
			`unanswered="woken just now, no check-in (email failed: api.agentmail.to returned 502 Bad Gateway)"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.UnansweredField(now); got != tc.want {
				t.Fatalf("field = %s, want %s", got, tc.want)
			}
		})
	}
}

// AE4: an ask to an unanswered agent says when it was woken and that the
// request is queued; an answered wake adds nothing.
func TestFormatResultUnansweredWake(t *testing.T) {
	base := client.Result{Request: envelope.Request{ID: "r1", To: "grokbot"}, Status: envelope.StatusQueued}
	plain := client.FormatResult(base)
	// Woken today, so the hint shows the clock time alone; the date form
	// for other days is covered by TestClockTime.
	woke := time.Now()
	at := woke.Format("15:04")
	quiet := base
	quiet.Target = &envelope.Target{WokenAt: woke, WakeResult: "ok"}
	if got := client.FormatResult(quiet); got != plain {
		t.Fatalf("answered wake = %q", got)
	}
	stuck := base
	stuck.Target = &envelope.Target{WokenAt: woke, WakeResult: "ok", Unanswered: true}
	want := "grokbot was woken at " + at + " and has not checked in yet; the request is queued.\n" + plain
	if got := client.FormatResult(stuck); got != want {
		t.Fatalf("unanswered = %q, want %q", got, want)
	}
	failed := base
	failed.Target = &envelope.Target{WokenAt: woke, WakeResult: "hooks.example returned 502 Bad Gateway", Unanswered: true}
	want = "grokbot's wake at " + at + " failed (hooks.example returned 502 Bad Gateway) and it has not checked in yet; the request is queued.\n" + plain
	if got := client.FormatResult(failed); got != want {
		t.Fatalf("failed = %q, want %q", got, want)
	}
}

// A pending request shows the relay's note, on one line, before the usual
// pending text; one without a note is unchanged.
func TestFormatResultRelayNote(t *testing.T) {
	base := client.Result{Request: envelope.Request{ID: "r1", To: "grokbot"}, Status: envelope.StatusQueued}
	plain := client.FormatResult(base)
	noted := base
	noted.RelayNote = &envelope.Progress{Note: "grokbot was woken at 16:40 UTC and has not checked in (webhook ok).\nThe request is still queued.", At: time.Now(), By: "relay"}
	got := client.FormatResult(noted)
	want := "Relay note, 0s ago: grokbot was woken at 16:40 UTC and has not checked in (webhook ok). The request is still queued.\n" + plain
	if got != want {
		t.Fatalf("noted = %q, want %q", got, want)
	}
}

// A notice from the relay is framed as one, not as a teammate's request to
// handle and answer.
func TestFormatRequestRelayNotice(t *testing.T) {
	got := client.FormatRequest(envelope.Request{ID: "n1", From: client.RelaySender, To: "instinct", Kind: envelope.KindNotify, Body: "About your request r1"})
	if !strings.Contains(got, "Notice n1 from the Agent Tincan relay (not a teammate). No reply needed.") || !strings.Contains(got, "About your request r1") || strings.Contains(got, "your teammate") {
		t.Fatalf("notice = %q", got)
	}
	ask := client.FormatRequest(envelope.Request{ID: "r2", From: "muse", To: "instinct", Kind: envelope.KindNotify, Body: "fyi"})
	if !strings.Contains(ask, "Request r2 from muse (your teammate)") {
		t.Fatalf("teammate notify = %q", ask)
	}
}
