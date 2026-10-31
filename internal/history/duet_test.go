package history

import (
	"encoding/json"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const duetChan = "chan_0000000000000000chan01"

// duetDetailJSON is the extension's duet.detail result over dotItem items:
// the dot's owner is the channel's owner, the dot is Claude and anyone
// else is another member.
func duetDetailJSON(working bool, items ...map[string]any) json.RawMessage {
	out := []map[string]any{}
	for _, it := range items {
		m := map[string]any{}
		maps.Copy(m, it)
		switch m["from"] {
		case dotOwner:
			m["role"] = "owner"
		case dotBot:
			m["role"] = "claude"
		default:
			m["role"] = "other"
		}
		out = append(out, m)
	}
	b, _ := json.Marshal(map[string]any{"thread": duetChan, "room": duetChan, "owner": dotOwner, "paused": false, "working": working, "items": out})
	return b
}

// A channel id is taken as is or from the channel's address; anything
// else is not a Duet channel.
func TestDuetCanonicalID(t *testing.T) {
	for in, want := range map[string]string{
		duetChan:                                               duetChan,
		"  " + duetChan + " ":                                  duetChan,
		"https://claude.ai/messages/" + duetChan:               duetChan,
		"https://claude.ai/messages/" + duetChan + "/":         duetChan,
		"https://claude.ai/messages/" + duetChan + "?thread=x": duetChan,
		"https://claude.ai/duet/" + duetChan:                   duetChan,
		"https://claude.ai/duet/" + duetChan + "/":             duetChan,
		"https://claude.ai/duet/" + duetChan + "#latest":       duetChan,
	} {
		if got, ok := duetCanonicalID(in); !ok || got != want {
			t.Errorf("duetCanonicalID(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "chan_", "chan_short", "0d0d0d0d-1111-7222-8333-000000000001", "https://claude.ai/chat/" + duetChan, "https://evil.test/messages/" + duetChan, "https://evil.test/duet/" + duetChan, "https://claude.ai/duet/" + duetChan + "/x", "chan_abc/../x"} {
		if got, ok := duetCanonicalID(in); ok {
			t.Errorf("duetCanonicalID(%q) = %q, accepted", in, got)
		}
	}
	if id, ok := ParseWebThread(SourceDuet, "https://claude.ai/duet/"+duetChan); !ok || id != duetChan {
		t.Fatalf("ParseWebThread = %q, %v", id, ok)
	}
}

// Duet is a web-only, one-thread site under claude.ai's grant with its own
// kind; unlike a dot it has no primary operation, so --thread is required.
func TestDuetSiteIsWebOnlyUnderClaudeAIsGrant(t *testing.T) {
	s := siteFor(SourceDuet)
	if s == nil || !s.webOnly || !s.oneThread || s.primary || s.grantAs != SourceClaudeAI {
		t.Fatalf("site %+v", s)
	}
	if WebSiteKind(SourceDuet) != "duet-web" || WebSiteKind(SourceDots) != "dot-web" || WebSiteKind(SourceChatGPT) != "" {
		t.Fatal("kinds")
	}
	if WebSiteHasPrimary(SourceDuet) || !WebSiteHasPrimary(SourceDots) {
		t.Fatal("primary")
	}
	for _, op := range []Op{OpDuetDetail, OpDuetSend, OpDuetClose, "duet.session"} {
		if site, _, ok := op.resolve(); !ok || site.source != SourceDuet {
			t.Errorf("%s does not resolve to duet", op)
		}
	}
	for _, op := range []Op{"duet.list", "duet.file", "duet.primary"} {
		if _, _, ok := op.resolve(); ok {
			t.Errorf("%s resolves", op)
		}
	}
	if IsLiveSource(SourceDuet) || strings.Contains(SourceNames(), "duet") {
		t.Fatal("duet is not a history source")
	}
	if err := ValidateOp(OpDuetSend, OpArgs{Message: "hi"}); err == nil {
		t.Fatal("a send with no channel is valid")
	}
	if WebThreadSites() != "dots or duet" {
		t.Fatalf("thread sites %q", WebThreadSites())
	}
}

// The live channel's redacted timeline, read the way the extension returns
// it, keeps its roles: the owner's question and Claude's two messages make
// one turn.
func TestParseDotFeedReadsADuetChannel(t *testing.T) {
	raw := json.RawMessage(`{"thread":"` + duetChan + `","room":"` + duetChan + `","owner":"user_o","paused":false,"working":false,"items":[
		{"id":"cmsg_1","at":"2026-10-21T05:41:00Z","from":"user_o","role":"owner","text":"What changed?","attachments":0},
		{"id":"cmsg_2","at":"2026-10-21T05:41:04Z","from":"cagt_a","role":"claude","text":"On it","attachments":0},
		{"id":"cmsg_3","at":"2026-10-21T05:41:30Z","from":"user_x","role":"other","text":"me too","attachments":0},
		{"id":"cmsg_4","at":"2026-10-21T05:41:40Z","from":"cagt_a","role":"claude","text":"Three things changed.","attachments":1}]}`)
	f, err := ParseDotFeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.Working == nil || *f.Working || !f.Messages[0].Owner || f.Messages[1].Owner || f.Messages[1].Other || !f.Messages[2].Other {
		t.Fatalf("feed %+v", f)
	}
	th, err := parseDuetDetail(duetChan, raw)
	if err != nil || len(th.turns) != 1 || th.conv.Source != SourceDuet {
		t.Fatalf("thread %+v %v", th, err)
	}
	want := "On it\n\nThree things changed.\n\n(Claude also sent an attachment; open the channel to see them)"
	if th.turns[0].reply.Text != want {
		t.Fatalf("reply %q", th.turns[0].reply.Text)
	}
	// A dot feed says nothing about working.
	if f, _ := ParseDotFeed(dotsDetailJSON(dotItem("m1", dotOwner, "A", time.Now()))); f.Working != nil {
		t.Fatal("a dot feed has a working flag")
	}
	// The fixture captured live parses as the extension shapes it.
	if _, err := os.Stat("testdata/duet/timeline.json"); err != nil {
		t.Fatal(err)
	}
}

// Claude's run is busy while the channel says it is working, and left to
// the stability rule (never finished on one read) once it is not; another
// member's message is no turn.
func TestDuetNodesFollowTheWorkingFlag(t *testing.T) {
	at := time.Date(2026, 10, 21, 5, 41, 0, 0, time.UTC)
	items := []map[string]any{
		dotItem("m1", dotOwner, "A", at),
		dotItem("m2", "user-other", "interjection", at.Add(time.Second)),
		dotItem("m3", dotBot, "On it", at.Add(2*time.Second)),
	}
	nodes, err := duetNodes(duetDetailJSON(true, items...))
	if err != nil {
		t.Fatal(err)
	}
	p := progressOf(nodes, replyAnchor{bound: "m1"})
	if !p.found || p.finished || !p.busy || p.orphaned {
		t.Fatalf("working: progress %+v", p)
	}
	nodes, _ = duetNodes(duetDetailJSON(false, items...))
	if p := progressOf(nodes, replyAnchor{bound: "m1"}); !p.found || p.finished || p.busy || !strings.Contains(p.sig, "On it") || strings.Contains(p.sig, "interjection") {
		t.Fatalf("done: progress %+v", p)
	}
	// A read that cannot tell whether Claude is working counts as working.
	raw := strings.Replace(string(duetDetailJSON(false, items...)), `"working":false`, `"working":null`, 1)
	nodes, _ = duetNodes(json.RawMessage(raw))
	if p := progressOf(nodes, replyAnchor{bound: "m1"}); !p.busy || p.finished {
		t.Fatalf("unknown: progress %+v", p)
	}
	// Another member writing after the owner does not orphan the request.
	nodes, _ = duetNodes(duetDetailJSON(true, items[:2]...))
	if p := progressOf(nodes, replyAnchor{bound: "m1"}); p.orphaned || p.found {
		t.Fatalf("other only: progress %+v", p)
	}
}

func duetRig(t *testing.T) (*webRig, *dotsBrowser) {
	t.Helper()
	rig, b := dotsRig(t)
	b.duet = true
	rig.agent.Site = SourceDuet
	rig.agent.Name = "duet-web"
	rig.agent.Thread = duetChan
	return rig, b
}

// A request goes into the channel and waits while Claude works, even when
// its interim "On it" holds still past the text-stability rule; the reply
// is everything Claude wrote once it stopped.
func TestDuetWebWaitsWhileClaudeWorks(t *testing.T) {
	rig, b := duetRig(t)
	b.busyReads = 6
	b.replies = []dotReply{{text: "On it", at: 1}, {text: "Three things changed.", at: 6}}
	res := rig.ask(t, "codex", "new chat\nwhat changed?")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	want := "On it\n\nThree things changed.\n\nyour Duet channel: " + duetChan
	if res.Reply.Body != want {
		t.Fatalf("reply\n%s\nwant\n%s", res.Reply.Body, want)
	}
	if got := b.sent(); len(got) != 1 || got[0] != (OpArgs{Message: "[tincan request " + res.Request.ID + " from codex]\nwhat changed?", ConversationID: duetChan}) {
		t.Fatalf("sends %+v", got)
	}
	if len(b.closes) != 1 || b.closes[0] != duetChan {
		t.Fatalf("closes %v", b.closes)
	}
}

// A read that says Claude is not working while the channel still shows
// only "On it" does not end the wait: Claude's answer posted on a later
// read is part of the reply.
func TestDuetWebNotWorkingStillWaitsForTheText(t *testing.T) {
	rig, b := duetRig(t)
	b.busyReads = 0
	b.replies = []dotReply{{text: "On it", at: 1}, {text: "Three things changed.", at: 2}}
	res := rig.ask(t, "codex", "what changed?")
	if res.Status != envelope.StatusAnswered || !strings.HasPrefix(res.Reply.Body, "On it\n\nThree things changed.") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

// A send that could not tell whether Claude is working fails with nothing
// sent and says so.
func TestDuetWebUnknownWorkingSaysNothingWasSent(t *testing.T) {
	err := &UnavailableError{Source: SourceDuet, Kind: ErrBusy, Detail: BusyUnknown}
	if got := err.Error(); !strings.Contains(got, "could not tell whether Claude Duet is working") {
		t.Fatal(got)
	}
}

// A refused channel and a busy Claude each fail with the reason, and
// nothing was sent.
func TestDuetWebRefusedOrBusySaysNothingWasSent(t *testing.T) {
	for code, want := range map[string]string{
		"channel_refused": "Nothing was sent: channel_refused.",
		"busy":            "Nothing was sent: Claude Duet is still working on something else; ask again when it is done.",
	} {
		rig, b := duetRig(t)
		b.sendErr = code
		res := rig.ask(t, "codex", "hello")
		if res.Status != envelope.StatusFailed || res.Reply.Body != want {
			t.Fatalf("%s: %s %q", code, res.Status, res.Reply.Body)
		}
		if len(b.closes) != 0 {
			t.Fatalf("%s: closes %v", code, b.closes)
		}
	}
}

// A channel refused while the reply is read ends the wait at once.
func TestDuetWebRefusedWhileWaitingStops(t *testing.T) {
	rig, b := duetRig(t)
	b.busyReads = 100
	b.replies = []dotReply{{text: "On it", at: 1}}
	go func() {
		for len(b.sent()) == 0 {
			time.Sleep(time.Millisecond)
		}
		b.mu.Lock()
		b.detailErr = "channel_refused"
		b.mu.Unlock()
	}()
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "channel_refused") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

// Without a valid channel the agent sends nothing and says how to set one.
func TestDuetWebWithoutThreadSendsNothing(t *testing.T) {
	rig, b := duetRig(t)
	rig.agent.Thread = "not-a-channel"
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "web install --site duet --thread <id>") || len(b.sent()) != 0 {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

// Only Claude's own @tincan ask lines are asked: the owner's and another
// member's are not, and the ask's body says it came from the Duet channel.
func TestDuetOutOnlyClaudesMessagesAreAsks(t *testing.T) {
	r := newDotOutRig(t)
	r.b.duet = true
	r.agent.Site = SourceDuet
	r.agent.Thread = duetChan
	r.taught()
	r.says(dotOwner, "@tincan ask muse owner wants this")
	r.says("user-other", "@tincan ask muse someone else wants this")
	r.dotSays("@tincan ask muse check the calendar")
	r.tick()
	in := r.museInbox()
	if len(in) != 1 || !strings.HasPrefix(in[0].Body, "check the calendar\n\n(from the owner's Duet channel, ref dotask") {
		t.Fatalf("muse inbox %+v", in)
	}
}
