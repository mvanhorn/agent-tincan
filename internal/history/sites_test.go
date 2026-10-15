package history

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Every op constant belongs to the site its prefix names.
func TestOpsResolveToTheirOwnSite(t *testing.T) {
	cases := map[Op]Source{
		OpChatGPTList: SourceChatGPT, OpChatGPTDetail: SourceChatGPT, OpChatGPTFile: SourceChatGPT,
		OpChatGPTSend: SourceChatGPT, OpChatGPTClose: SourceChatGPT,
		OpClaudeAIList: SourceClaudeAI, OpClaudeAIDetail: SourceClaudeAI, OpClaudeAIFile: SourceClaudeAI,
		OpClaudeAISend: SourceClaudeAI, OpClaudeAIClose: SourceClaudeAI,
		OpGrokList: SourceGrok, OpGrokDetail: SourceGrok, OpGrokFile: SourceGrok,
		OpGrokSend: SourceGrok, OpGrokClose: SourceGrok,
		OpGeminiList: SourceGemini, OpGeminiDetail: SourceGemini, OpGeminiFile: SourceGemini,
		OpGeminiSend: SourceGemini, OpGeminiClose: SourceGemini,
		OpPerplexityDetail: SourcePerplexity, OpPerplexitySend: SourcePerplexity, OpPerplexityClose: SourcePerplexity,
		OpCopilotList: SourceCopilot, OpCopilotDetail: SourceCopilot,
		OpCopilotSend: SourceCopilot, OpCopilotClose: SourceCopilot,
	}
	for op, want := range cases {
		if got := op.source(); got != want {
			t.Errorf("%s: source %q, want %q", op, got, want)
		}
	}
}

// An op with an unknown prefix, or an unknown verb on a known prefix, is
// not attributed to ChatGPT, and the client refuses it without sending.
func TestUnknownOpIsRejectedNotChatGPT(t *testing.T) {
	for _, op := range []Op{"bard.list", "perplexity.list", "perplexity.file", "chatgpt.delete", "list", "", OpExtensionReload} {
		if got := op.source(); got != "" {
			t.Errorf("%q: source %q, want none", op, got)
		}
	}
	sent := 0
	c := &Client{Channel: channelFunc(func(context.Context, NativeRequest, func(NativeResponse) (bool, error)) error {
		sent++
		return nil
	}), Cooldown: &SiteCooldown{}}
	_, err := c.Request(context.Background(), "bard.list", OpArgs{Count: 1})
	if !errors.Is(err, ErrRejected) || strings.Contains(err.Error(), "chatgpt") {
		t.Fatalf("err = %v, want a rejection not attributed to chatgpt", err)
	}
	if sent != 0 {
		t.Fatalf("%d requests sent for an unknown op", sent)
	}
}

// Send and Close to a site that is not in the table fail instead of
// going to ChatGPT.
func TestSendAndCloseUnknownSiteFail(t *testing.T) {
	var mu sync.Mutex
	var ops []Op
	c := &Client{Channel: channelFunc(func(_ context.Context, req NativeRequest, _ func(NativeResponse) (bool, error)) error {
		mu.Lock()
		ops = append(ops, req.Op)
		mu.Unlock()
		return nil
	}), Cooldown: &SiteCooldown{}}
	if _, err := c.Send(context.Background(), "bard", "hi", "", true); err == nil {
		t.Fatal("send to an unknown site succeeded")
	}
	if err := c.Close(context.Background(), "bard", "abc-1"); err == nil {
		t.Fatal("close on an unknown site succeeded")
	}
	if len(ops) != 0 {
		t.Fatalf("ops sent: %v", ops)
	}
}

func TestParseWebSiteUnknownListsKnownSites(t *testing.T) {
	for _, s := range WebSites {
		if got, err := ParseWebSite(string(s)); err != nil || got != s {
			t.Fatalf("ParseWebSite(%q) = %q, %v", s, got, err)
		}
	}
	_, err := ParseWebSite("bard")
	if err == nil || err.Error() != `unknown site "bard" (want chatgpt, claude-ai, grok, gemini, perplexity, copilot or dots)` {
		t.Fatalf("err = %v", err)
	}
}

// A cooldown noted for one site holds back only that site.
func TestCooldownsAreIndependentPerSite(t *testing.T) {
	for _, limited := range WebSites {
		cd := &SiteCooldown{}
		cd.Note(limited, time.Minute)
		c := &Client{Channel: frames(), Cooldown: cd}
		for _, s := range WebSites {
			left := c.CooldownRemaining(s)
			if (s == limited) != (left > 0) {
				t.Fatalf("cooldown on %s: %s has %s left", limited, s, left)
			}
		}
		// A site with one thread (dots) always sends into it.
		conv, newChat := "", true
		if siteFor(limited).oneThread {
			conv, newChat = "0d0d0d0d-1111-7222-8333-000000000001", false
		}
		_, err := c.Send(context.Background(), limited, "hi", conv, newChat)
		if !errors.Is(err, ErrRateLimited) || err.Error() != "source unavailable: "+string(limited)+": "+siteLimiter(limited)+" is rate-limiting this account right now; try again later" {
			t.Fatalf("send to %s during its cooldown: %v", limited, err)
		}
	}
}

// A web agent for a site outside the table fails its requests instead of
// acting as ChatGPT, and sends nothing.
func TestWebAgentUnknownSiteFails(t *testing.T) {
	rig := newWebRig(t)
	rig.agent.Site = "bard"
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, `unknown site "bard"`) {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if n := rig.browser.exchanges(); n != 0 {
		t.Fatalf("%d extension requests for an unknown site", n)
	}
}

// Only claude.ai has the text-stability rule, with the default windows;
// the agent's overrides change the windows but never give ChatGPT or
// Grok (which mark a finished answer explicitly) one.
func TestStabilityRuleIsClaudeOnly(t *testing.T) {
	for _, s := range []Source{SourceChatGPT, SourceGrok} {
		if r := (&WebAgent{Site: s, ClaudeStablePolls: 2, ClaudeStableFor: time.Second}).stableRule(); r != nil {
			t.Fatalf("%s has a stability rule: %+v", s, *r)
		}
	}
	r := (&WebAgent{Site: SourceClaudeAI}).stableRule()
	if r == nil || r.polls != 4 || r.span != 10*time.Second {
		t.Fatalf("claude.ai rule = %+v", r)
	}
	r = (&WebAgent{Site: SourceClaudeAI, ClaudeStablePolls: 2, ClaudeStableFor: time.Second}).stableRule()
	if r == nil || r.polls != 2 || r.span != time.Second {
		t.Fatalf("claude.ai rule with overrides = %+v", r)
	}
	if siteFor(SourceClaudeAI).stable.polls != DefaultClaudeStablePolls {
		t.Fatal("an override changed the table")
	}
}

// The table keeps each site's names and forms as they were.
func TestSiteTableNames(t *testing.T) {
	if WebSiteNames() != "chatgpt, claude-ai, grok, gemini, perplexity, copilot or dots" || WebAgentNames() != "chatgpt-web, claude-web, grok-web, gemini-web, perplexity-web, copilot-web or dot-web" || LiveSourcesLabel() != "ChatGPT, claude.ai, Grok, Gemini and Copilot" {
		t.Fatalf("names %q, agents %q, labels %q", WebSiteNames(), WebAgentNames(), LiveSourcesLabel())
	}
	if SourceNames() != "chatgpt, claude-ai, grok, gemini, copilot, codex, claude-code, grok-cli, cursor-cli, cursor-ide, copilot-cli, gemini-cli or opencode" {
		t.Fatalf("sources %q", SourceNames())
	}
	want := map[Source][3]string{
		SourceChatGPT:  {"chatgpt-web", "ChatGPT", "chatgpt.com"},
		SourceClaudeAI: {"claude-web", "claude.ai", "claude.ai"},
		SourceGrok:     {"grok-web", "Grok", "grok.com"},
		SourceGemini:   {"gemini-web", "Gemini", "gemini.google.com"},
		SourceCopilot:  {"copilot-web", "Copilot", "copilot.com"},
	}
	for src, w := range want {
		if got := [3]string{WebAgentName(src), siteLabel(src), siteOf(src)}; got != w {
			t.Fatalf("%s: %v, want %v", src, got, w)
		}
		if !IsLiveSource(src) {
			t.Fatalf("%s is not live", src)
		}
		r, ok := NewLiveReader(src, &Client{}, nil)
		if !ok || r.Source() != src {
			t.Fatalf("%s reader: %v %v", src, r, ok)
		}
	}
	if got := [3]string{WebAgentName(SourcePerplexity), siteLabel(SourcePerplexity), siteOf(SourcePerplexity)}; got != [3]string{"perplexity-web", "Perplexity", "www.perplexity.ai"} {
		t.Fatalf("perplexity: %v", got)
	}
	if got := [3]string{WebAgentName(SourceDots), siteLabel(SourceDots), siteOf(SourceDots)}; got != [3]string{"dot-web", "your dot", "chatgpt.com"} || IsLiveSource(SourceDots) {
		t.Fatalf("dots: %v", got)
	}
	for _, src := range []Source{SourceCodex, SourceClaudeCode, "bard"} {
		if IsLiveSource(src) || WebAgentName(src) != "" {
			t.Fatalf("%s treated as a live site", src)
		}
		if _, ok := NewLiveReader(src, &Client{}, nil); ok {
			t.Fatalf("%s has a live reader", src)
		}
	}
	if joinList([]string{"a", "b", "c"}, "or") != "a, b or c" || joinList([]string{"a"}, "or") != "a" {
		t.Fatal("joinList")
	}
}

// Conversation URLs are read on every site's host and no other.
func TestConversationRefHosts(t *testing.T) {
	for ref, want := range map[string]string{
		"https://chatgpt.com/c/abc-1":                                           "abc-1",
		"https://chatgpt.com/g/g-x/c/abc-2":                                     "abc-2",
		"https://claude.ai/chat/abc-3":                                          "abc-3",
		"https://example.com/c/abc-4":                                           "",
		"http://chatgpt.com/c/abc-5":                                            "",
		"https://claude.ai.example.com/chat/x":                                  "",
		"https://grok.com/c/0e1d0000-0000-4000-8000-000000000001":               "0e1d0000-0000-4000-8000-000000000001",
		"https://grok.com/chat/abc-6":                                           "",
		"https://grok.com.example.com/c/abc-7":                                  "",
		"https://www.perplexity.ai/search/0e1d0000-0000-4000-8000-0000000000a1": "0e1d0000-0000-4000-8000-0000000000a1",
		"https://perplexity.ai/search/abc-8":                                    "",
		"https://www.perplexity.ai/c/abc-9":                                     "",
	} {
		got, ok := conversationRef(ref)
		if ok != (want != "") || got != want {
			t.Errorf("%s: %q %v, want %q", ref, got, ok, want)
		}
	}
}

// Perplexity fronts a web agent only: it is not a history source, has no
// list or file operation, and the history prose leaves it out.
func TestPerplexityIsNotAHistorySource(t *testing.T) {
	want := []Source{SourceChatGPT, SourceClaudeAI, SourceGrok, SourceGemini, SourceCopilot, SourceCodex, SourceClaudeCode, SourceGrokCLI,
		SourceCursorCLI, SourceCursorIDE, SourceCopilotCLI, SourceGeminiCLI, SourceOpenCode}
	if len(Sources) != len(want) {
		t.Fatalf("Sources = %v", Sources)
	}
	for i := range want {
		if Sources[i] != want[i] {
			t.Fatalf("Sources = %v", Sources)
		}
	}
	if IsLiveSource(SourcePerplexity) {
		t.Fatal("perplexity is a live history source")
	}
	if _, ok := NewLiveReader(SourcePerplexity, &Client{}, nil); ok {
		t.Fatal("perplexity has a history reader")
	}
	if err := (Query{Source: SourcePerplexity, Mode: ModeLatest}).Validate(); err == nil {
		t.Fatal("a perplexity history query validates")
	}
	if strings.Contains(SourceNames(), "perplexity") || strings.Contains(LiveSourcesLabel(), "Perplexity") {
		t.Fatalf("history names list perplexity: %q, %q", SourceNames(), LiveSourcesLabel())
	}
	if !slices.Contains(WebSites, SourcePerplexity) {
		t.Fatalf("perplexity is not a web site: %v", WebSites)
	}
	for _, op := range []Op{"perplexity.list", "perplexity.file"} {
		if err := ValidateOp(op, OpArgs{Count: 1}); err == nil {
			t.Errorf("%s validates", op)
		}
	}
	if err := ValidateOp(OpPerplexityDetail, OpArgs{ID: "0e1d0000-0000-4000-8000-0000000000a1"}); err != nil {
		t.Fatalf("perplexity.detail: %v", err)
	}
}
