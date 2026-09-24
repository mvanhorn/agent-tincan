package history

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	ccS1 = "11111111-1111-4111-8111-111111111111" // cli, two human prompts, tool results
	ccS2 = "22222222-2222-4222-8222-222222222222" // sdk-cli, newer
	ccS3 = "33333333-3333-4333-8333-333333333333" // cli in the scratch dir, newest
	ccS4 = "44444444-4444-4444-8444-444444444444" // cli, fox
	ccS5 = "55555555-5555-4555-8555-555555555555" // cli, 36 days old
	ccS6 = "66666666-6666-4666-8666-666666666666" // cli, plain string prompt
)

func claudeFixture(t *testing.T) *ClaudeCode {
	t.Helper()
	root := filepath.Join(copyTree(t, "claudecode"), "projects")
	x := filepath.Join(root, "-Users-matt-code-x")
	y := filepath.Join(root, "-Users-matt-code-y")
	setMtime(t, filepath.Join(x, ccS1+".jsonl"), "2026-09-21T11:00:30Z")
	setMtime(t, filepath.Join(x, ccS2+".jsonl"), "2026-09-22T07:00:05Z")
	setMtime(t, filepath.Join(x, ccS1, "subagents", "agent-fake.jsonl"), "2026-09-22T08:00:00Z")
	setMtime(t, filepath.Join(root, "-Users-matt--config-tincan-history-scratch", ccS3+".jsonl"), "2026-09-22T09:00:00Z")
	setMtime(t, filepath.Join(y, ccS4+".jsonl"), "2026-09-10T09:00:05Z")
	setMtime(t, filepath.Join(y, ccS5+".jsonl"), "2026-08-17T09:00:05Z")
	setMtime(t, filepath.Join(y, ccS6+".jsonl"), "2026-09-12T09:00:05Z")
	return &ClaudeCode{Root: root, ScratchDir: fixtureScratch, Now: func() time.Time { return fixtureNow }}
}

func TestClaudeCodeLatestSkipsSDKSubagentsScratchAndToolResults(t *testing.T) {
	r := claudeFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeLatest, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	c := got[0]
	if c.ID != ccS1 || c.Title != "Relay design" || c.Cwd != "/Users/matt/code/x" || c.Automated {
		t.Fatalf("latest = %+v", c)
	}
	u := userTurn(t, c)
	// Tool results, /model, local command output and task notifications
	// that came after the first prompt are not Matt's prompts.
	if u.Text != "now write tests for the relay" || len(u.Images) != 0 {
		t.Fatalf("prompt = %q images %d", u.Text, len(u.Images))
	}
	if len(c.Messages) != 2 || c.Messages[1].Text != "Tests written." {
		t.Fatalf("messages = %+v", c.Messages)
	}
}

func TestClaudeCodeSearchDecodesImagesOnSelectedTurnOnly(t *testing.T) {
	r := claudeFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeSearch, Terms: []string{"diagram"}, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != ccS1 {
		t.Fatalf("ids = %s", ids(got))
	}
	var all []Image
	for _, m := range got[0].Messages {
		all = append(all, m.Images...)
	}
	// red is attached to the prompt; orange is inside a tool result and is
	// not Matt's image.
	if colors(t, all) != "red" {
		t.Fatalf("images = %s, want red", colors(t, all))
	}
	if got[0].Messages[len(got[0].Messages)-1].Text != "The relay is a single Go binary." {
		t.Fatalf("reply = %+v", got[0].Messages)
	}
}

func TestClaudeCodeSearchExclusionsAndAll(t *testing.T) {
	r := claudeFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeSearch, Terms: []string{"fox"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// S2 is sdk-cli, the subagent and S3 (scratch) are never shown, S5 is
	// 36 days old.
	if ids(got) != ccS4 {
		t.Fatalf("search fox = %s, want S4", ids(got))
	}
	got, err = r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeSearch, Terms: []string{"fox"}}, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != ccS2+","+ccS4 {
		t.Fatalf("search fox --all = %s", ids(got))
	}
	if !got[0].Automated || got[0].Originator != "sdk-cli" {
		t.Fatalf("sdk session = %+v", got[0])
	}
}

func TestClaudeCodeRecencyWindow(t *testing.T) {
	r := claudeFixture(t)
	got, _ := r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeSearch, Terms: []string{"pelicans"}}, Options{})
	if ids(got) != ccS6 {
		t.Fatalf("pelicans = %s", ids(got))
	}
	r.Window = Window{Max: 1, MaxAge: DefaultWindow().MaxAge}
	got, _ = r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeSearch, Terms: []string{"pelicans"}}, Options{})
	if len(got) != 0 {
		t.Fatalf("match outside the window returned: %s", ids(got))
	}
}

func TestClaudeCodeList(t *testing.T) {
	r := claudeFixture(t)
	got, err := r.List(context.Background(), 10, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join([]string{ccS1, ccS6, ccS4}, ","); ids(got) != want {
		t.Fatalf("list = %s, want %s", ids(got), want)
	}
	if got[0].Title != "Relay design" || got[1].Title == "" {
		t.Fatalf("titles = %q %q", got[0].Title, got[1].Title)
	}
	got, _ = r.List(context.Background(), 10, Options{All: true})
	if want := strings.Join([]string{ccS2, ccS1, ccS6, ccS4}, ","); ids(got) != want {
		t.Fatalf("list --all = %s, want %s", ids(got), want)
	}
}

func TestClaudeCodeConversationByID(t *testing.T) {
	r := claudeFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeConversation, ConversationID: ccS1}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range got[0].Messages {
		roles = append(roles, string(m.Role)+":"+m.Text)
	}
	want := "user:[Image #1] summarize the relay design in this diagram|assistant:The relay is a single Go binary.|user:now write tests for the relay|assistant:Tests written."
	if strings.Join(roles, "|") != want {
		t.Fatalf("messages = %q", roles)
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeConversation, ConversationID: ccS2}, Options{}); err == nil {
		t.Fatal("sdk-cli session returned without --all")
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeConversation, ConversationID: ccS3}, Options{All: true}); err == nil {
		t.Fatal("scratch session returned")
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeConversation, ConversationID: "agent-fake"}, Options{All: true}); err == nil {
		t.Fatal("subagent transcript returned")
	}
}

func TestClaudeCodeMissingRootIsAClearError(t *testing.T) {
	r := &ClaudeCode{Root: filepath.Join(t.TempDir(), "nope"), Now: func() time.Time { return fixtureNow }}
	if _, err := r.Read(context.Background(), Query{Source: SourceClaudeCode, Mode: ModeLatest}, Options{}); err == nil {
		t.Fatal("missing root: want error")
	}
	var reader Reader = r
	if reader.Source() != SourceClaudeCode {
		t.Fatal(reader.Source())
	}
}

func TestClaudeCodeSlashCommandWithArgsIsAPrompt(t *testing.T) {
	text, ok := claudePromptText("<command-message>ce-plan is running</command-message>\n<command-name>/ce-plan</command-name>\n<command-args>add a fox mode</command-args>")
	if !ok || text != "/ce-plan add a fox mode" {
		t.Fatalf("slash command = %q %v", text, ok)
	}
	for _, s := range []string{
		"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>",
		"<local-command-stdout>x</local-command-stdout>",
		"<task-notification>x</task-notification>",
		"<system-reminder>x</system-reminder>",
		"[Request interrupted by user]",
		"   ",
	} {
		if got, ok := claudePromptText(s); ok {
			t.Errorf("claudePromptText(%q) = %q, want skipped", s, got)
		}
	}
}
