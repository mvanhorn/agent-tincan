package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/pass"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// passReaderStub serves one fixed conversation per source and counts
// reads, so a test sees whether the chat was read at all.
type passReaderStub struct {
	src   history.Source
	convs []history.Conversation
	reads *int
}

func (p passReaderStub) Source() history.Source { return p.src }

func (p passReaderStub) List(_ context.Context, count int, _ history.Options) (history.Page, error) {
	*p.reads++
	var page history.Page
	for _, c := range p.convs[:min(count, len(p.convs))] {
		c.Messages = nil
		page.Conversations = append(page.Conversations, c)
	}
	return page, nil
}

func (p passReaderStub) Read(_ context.Context, q history.Query, _ history.Options) (history.Page, error) {
	*p.reads++
	for _, c := range p.convs {
		if c.ID == q.ConversationID {
			return history.Page{Conversations: []history.Conversation{c}}, nil
		}
	}
	return history.Page{}, history.ErrNotFound
}

// passMesh is a mesh with chatgpt-web (kind chatgpt-web) on muse's
// machine and the owner's CLI joined as "owner", every agent running
// pass.GuardVersion. The owner's claude.ai history holds convs; reads
// counts every history read.
func passMesh(t *testing.T, convs ...history.Conversation) (m *testrelay.Mesh, web *client.Relay, reads *int) {
	t.Helper()
	return passMeshAt(t, pass.GuardVersion, convs...)
}

// passMeshAt is passMesh with every agent running tincan version.
func passMeshAt(t *testing.T, version string, convs ...history.Conversation) (m *testrelay.Mesh, web *client.Relay, reads *int) {
	t.Helper()
	oldVersion := client.Version
	client.Version = version
	t.Cleanup(func() { client.Version = oldVersion })
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	setHome(t, t.TempDir())
	m = testrelay.New(t, relay.Config{})
	web = m.JoinOnMachineOf(t, "muse", "chatgpt-web")
	if err := m.Client(t, "admin").SetKind(t.Context(), "chatgpt-web", "chatgpt-web"); err != nil {
		t.Fatal(err)
	}
	// The destination calls the relay once, so the roster knows its build.
	if _, err := web.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.JoinOnMachineOf(t, "admin", "owner")
	cfg := filepath.Join(t.TempDir(), "owner.json")
	b, _ := json.Marshal(client.Config{Relay: m.URL("owner"), Agent: "owner"})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_CONFIG", cfg)
	reads = new(int)
	old := passReader
	passReader = func(src history.Source) (history.Reader, bool) {
		if src != history.SourceClaudeAI {
			return nil, false
		}
		return passReaderStub{src: src, convs: convs, reads: reads}, true
	}
	t.Cleanup(func() { passReader = old })
	return m, web, reads
}

func claudeChat(id string, turns int) history.Conversation {
	c := history.Conversation{Source: history.SourceClaudeAI, ID: id, Title: "Garden plan", UpdatedAt: time.Now()}
	for i := range turns {
		c.Messages = append(c.Messages,
			history.Message{Role: history.RoleUser, Text: "prompt " + string(rune('a'+i))},
			history.Message{Role: history.RoleAssistant, Text: "reply " + string(rune('a'+i))})
	}
	c.Messages[len(c.Messages)-1].Text = "plant the tomatoes in May"
	return c
}

// tincan pass reads the claude.ai chat a URL names, sends chatgpt-web one
// ask, grouped as a pass, whose body opens a new chat and carries the
// chat's last turn, and prints the conversation the destination opened.
func TestPassSendsSeedAndNamesNewChat(t *testing.T) {
	_, web, _ := passMesh(t, claudeChat("abc-3", 3))
	got := fakeCouncil(t, web, nil, "Got it.\n\nChatGPT conversation: newchat-1", envelope.StatusAnswered)
	out, _, err := runSplit(t, passCmd(), "https://claude.ai/chat/abc-3", "--to", "chatgpt-web")
	if err != nil {
		t.Fatalf("pass: %v\n%s", err, out)
	}
	req := <-got
	if !strings.HasPrefix(req.Body, "new chat\n") || !strings.Contains(req.Body, "plant the tomatoes in May") {
		t.Fatalf("seed = %q", req.Body)
	}
	if req.From != "owner" || !strings.HasPrefix(req.Group, "pass-") {
		t.Fatalf("seed from %s in group %q", req.From, req.Group)
	}
	if !strings.Contains(out, "ChatGPT conversation: newchat-1") || strings.Contains(out, "Got it.") {
		t.Fatalf("output = %q, want the footer and not the answer", out)
	}
}

// pass serve refuses to run when the relay does not store kind pass for
// it, saying how to fix it.
func TestPassServeRefusesWithoutPassKind(t *testing.T) {
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	m := testrelay.New(t, relay.Config{})
	m.JoinOnMachineOf(t, "muse", "pass")
	cfg := filepath.Join(t.TempDir(), "pass.json")
	b, _ := json.Marshal(client.Config{Relay: m.URL("pass"), Agent: "pass"})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	none := filepath.Join(t.TempDir(), "none.txt")
	_, _, err := runSplit(t, passCmd(), "serve", "--config", cfg, "--allowlist", none, "--history-allowlist", none, "--window-file", none)
	if err == nil || !strings.Contains(err.Error(), "refuses to run") || !strings.Contains(err.Error(), "tincan kind pass pass") {
		t.Fatalf("serve = %v, want a refusal naming the kind fix", err)
	}
}

// A bare source passes its newest conversation.
func TestPassBareSourceTakesNewest(t *testing.T) {
	newest, older := claudeChat("newest", 2), claudeChat("older", 2)
	newest.Messages[len(newest.Messages)-1].Text = "water the newest beds"
	older.Messages[len(older.Messages)-1].Text = "water the older beds"
	older.UpdatedAt = newest.UpdatedAt.Add(-time.Hour)
	_, web, _ := passMesh(t, newest, older)
	got := fakeCouncil(t, web, nil, "ok\n\nChatGPT conversation: c2", envelope.StatusAnswered)
	if out, _, err := runSplit(t, passCmd(), "claude-ai", "--to", "chatgpt-web"); err != nil {
		t.Fatalf("pass: %v\n%s", err, out)
	}
	if req := <-got; !strings.Contains(req.Body, "water the newest beds") || strings.Contains(req.Body, "water the older beds") {
		t.Fatalf("seed = %q, want the newest chat's last turn only", req.Body)
	}
}

// A destination that is not a web chat teammate, or is signed out, is
// refused before any history is read; a source with no conversations
// says so.
func TestPassRefusals(t *testing.T) {
	m, web, reads := passMesh(t)
	m.JoinOnMachineOf(t, "grokbot", "codex")
	if err := m.Client(t, "admin").SetKind(t.Context(), "codex", "codex"); err != nil {
		t.Fatal(err)
	}
	_, _, err := runSplit(t, passCmd(), "claude-ai", "--to", "codex")
	if err == nil || !strings.Contains(err.Error(), "not a web chat teammate") {
		t.Fatalf("--to codex = %v", err)
	}
	_, _, err = runSplit(t, passCmd(), "claude-ai", "--to", "chatgpt-web")
	if err == nil || !strings.Contains(err.Error(), "no claude.ai conversations") {
		t.Fatalf("empty source = %v", err)
	}
	*reads = 0
	if err := web.ReportWebStatus(t.Context(), envelope.WebStatus{Site: "chatgpt.com", State: "signed_out", ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, _, err = runSplit(t, passCmd(), "claude-ai", "--to", "chatgpt-web")
	if err == nil || !strings.Contains(err.Error(), "signed out") {
		t.Fatalf("signed out = %v", err)
	}
	if *reads != 0 {
		t.Fatalf("history read %d times before the refusal", *reads)
	}
	_, _, err = runSplit(t, passCmd(), "https://www.perplexity.ai/search/abc", "--to", "chatgpt-web")
	if err == nil || !strings.Contains(err.Error(), "Perplexity") {
		t.Fatalf("perplexity = %v", err)
	}
}

// A destination running a build older than pass.GuardVersion is refused
// before the chat is read, with the pass service's reason, so it never
// receives a pass it does not record as passed.
func TestPassRefusesOldDestination(t *testing.T) {
	_, _, reads := passMeshAt(t, "0.17.0", claudeChat("abc-3", 3))
	_, _, err := runSplit(t, passCmd(), "https://claude.ai/chat/abc-3", "--to", "chatgpt-web", "--wait", "1s")
	want := "chatgpt-web runs tincan 0.17.0, which does not refuse agents' continuations of a passed chat. Upgrade it to " + pass.GuardVersion + " or later"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("old destination = %v, want %q", err, want)
	}
	if *reads != 0 {
		t.Fatalf("history read %d times for an old destination", *reads)
	}
}
