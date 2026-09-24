package history

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	codexA = "01a0c000-0000-7000-8000-00000000000a" // Codex Desktop, two turns
	codexB = "01a0c000-0000-7000-8000-00000000000b" // codex-tui
	codexC = "01a0c000-0000-7000-8000-00000000000c" // codex_exec wake, not in index
	codexD = "01a0c000-0000-7000-8000-00000000000d" // Desktop thread in the scratch dir
	codexE = "01a0c000-0000-7000-8000-00000000000e" // 33 days old
	codexG = "01a0c000-0000-7000-8000-000000000010" // legacy user_message format
	codexH = "01a0c000-0000-7000-8000-000000000011" // in the index, rollout pruned
)

func codexFixture(t *testing.T) *Codex {
	t.Helper()
	home := copyTree(t, "codex")
	sess := filepath.Join(home, "sessions")
	setMtime(t, filepath.Join(sess, "2026/09/22/rollout-2026-09-22T01-00-00-"+codexC+".jsonl"), "2026-09-22T08:01:01Z")
	setMtime(t, filepath.Join(sess, "2026/09/22/rollout-2026-09-22T02-00-00-"+codexD+".jsonl"), "2026-09-22T09:00:05Z")
	setMtime(t, filepath.Join(sess, "2026/09/21/rollout-2026-09-21T02-00-00-"+codexA+".jsonl"), "2026-09-21T10:00:41Z")
	setMtime(t, filepath.Join(sess, "2026/09/19/rollout-2026-09-19T08-00-00-"+codexB+".jsonl"), "2026-09-19T15:05:01Z")
	setMtime(t, filepath.Join(sess, "2026/09/15/rollout-2026-09-15T05-00-00-"+codexG+".jsonl"), "2026-09-15T12:00:03Z")
	setMtime(t, filepath.Join(sess, "2026/08/20/rollout-2026-08-20T02-00-00-"+codexE+".jsonl"), "2026-08-20T09:00:05Z")
	gen := filepath.Join(home, "generated_images", codexA)
	// fakegen1 (yellow) was written during turn 1's reply, fakegen2 (orange)
	// during turn 2's.
	setMtime(t, filepath.Join(gen, "call_fakegen1.png"), "2026-09-21T09:00:40Z")
	setMtime(t, filepath.Join(gen, "call_fakegen2.png"), "2026-09-21T10:00:30Z")
	return &Codex{Home: home, ScratchDir: fixtureScratch, Now: func() time.Time { return fixtureNow }}
}

func ids(convs []Conversation) string {
	var out []string
	for _, c := range convs {
		out = append(out, c.ID)
	}
	return strings.Join(out, ",")
}

func userTurn(t *testing.T, c Conversation) Message {
	t.Helper()
	for _, m := range c.Messages {
		if m.Role == RoleUser {
			return m
		}
	}
	t.Fatalf("conversation %s has no user message: %+v", c.ID, c.Messages)
	return Message{}
}

func TestCodexLatestIsMattsNewestDesktopPromptWithTurnImages(t *testing.T) {
	r := codexFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeLatest, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d conversations, want 1: %+v", len(got), got)
	}
	c := got[0]
	// The newer codex_exec wake (C) and the scratch dir thread (D) are skipped.
	if c.ID != codexA {
		t.Fatalf("latest = %s, want Desktop thread A", c.ID)
	}
	if c.Title != "Fox logo for landing page" {
		t.Fatalf("title = %q, want the newest index name", c.Title)
	}
	if c.Cwd != "/Users/matt/Documents/Codex/fox-logo" || c.Originator != "Codex Desktop" || c.Automated {
		t.Fatalf("thread meta = cwd %q originator %q automated %v", c.Cwd, c.Originator, c.Automated)
	}
	u := userTurn(t, c)
	if u.Text != "make the ears bigger like this sketch" {
		t.Fatalf("prompt = %q", u.Text)
	}
	if u.Time.IsZero() {
		t.Fatal("prompt time missing")
	}
	// Images on the selected turn: green (attached) and orange (generated in
	// the reply). Red, blue and yellow belong to turn 1.
	var all []Image
	for _, m := range c.Messages {
		all = append(all, m.Images...)
	}
	if got := colors(t, all); got != "green,orange" {
		t.Fatalf("images = %s, want green,orange", got)
	}
	if len(c.Messages) != 2 || c.Messages[1].Role != RoleAssistant || c.Messages[1].Text != "Ears enlarged." {
		t.Fatalf("messages = %+v", c.Messages)
	}
}

func TestCodexLatestWithoutWantImagesReturnsNoImageBytes(t *testing.T) {
	r := codexFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeLatest}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got[0].Messages {
		if len(m.Images) != 0 {
			t.Fatalf("images returned without want_images: %+v", m.Images)
		}
	}
}

func TestCodexLatestWithAllIncludesExecWakeButNeverScratch(t *testing.T) {
	r := codexFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeLatest}, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != codexC || !got[0].Automated || got[0].Originator != "codex_exec" {
		t.Fatalf("latest with --all = %+v, want exec wake C", got)
	}
}

func TestCodexExecRolloutListedInIndexIsStillExcluded(t *testing.T) {
	r := codexFixture(t)
	f, err := os.OpenFile(filepath.Join(r.Home, "session_index.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":"` + codexC + `","thread_name":"Wake","updated_at":"2026-09-22T08:01:01.000000Z"}` + "\n")
	_ = f.Close()
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeLatest}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != codexA {
		t.Fatalf("latest = %s, want A (exec originator excluded even when indexed)", ids(got))
	}
	list, _ := r.List(context.Background(), 20, Options{})
	if strings.Contains(ids(list), codexC) {
		t.Fatalf("list without --all includes exec run: %s", ids(list))
	}
}

func TestCodexLatestCountAcrossSessionsAndDays(t *testing.T) {
	r := codexFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeLatest, Count: 4}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var prompts []string
	for _, c := range got {
		prompts = append(prompts, userTurn(t, c).Text)
	}
	want := "make the ears bigger like this sketch|draw a fox logo for the landing page|fix the flaky relay test|legacy hello with a picture"
	if strings.Join(prompts, "|") != want {
		t.Fatalf("prompts = %q", prompts)
	}
}

func TestCodexSearchSelectsMatchingTurnAndItsReplyImages(t *testing.T) {
	r := codexFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeSearch, Terms: []string{"Fox", "logo"}, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Exec wake C mentions fox, D is scratch, E is 33 days old.
	if ids(got) != codexA {
		t.Fatalf("search ids = %s, want only A", ids(got))
	}
	c := got[0]
	if u := userTurn(t, c); u.Text != "draw a fox logo for the landing page" {
		t.Fatalf("matched prompt = %q", u.Text)
	}
	var all []Image
	for _, m := range c.Messages {
		all = append(all, m.Images...)
	}
	// red: attached to turn 1; blue: image_generation_call in the reply;
	// yellow: generated_images file written during the reply. Green and
	// orange belong to turn 2.
	if got := colors(t, all); got != "blue,red,yellow" {
		t.Fatalf("images = %s, want blue,red,yellow", got)
	}
	if c.Messages[len(c.Messages)-1].Text != "Here is the fox logo." {
		t.Fatalf("reply = %+v", c.Messages)
	}
}

func TestCodexSearchByTitleFallsBackToLatestTurn(t *testing.T) {
	r := codexFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeSearch, Terms: []string{"landing page"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// "landing page" is in turn 1's text, so turn 1 wins over the title.
	if ids(got) != codexA || userTurn(t, got[0]).Text != "draw a fox logo for the landing page" {
		t.Fatalf("got %+v", got)
	}
	// H's rollout was pruned; its index title still matches, with no turns.
	got, err = r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeSearch, Terms: []string{"pruned"}}, Options{})
	if err != nil || ids(got) != codexH || len(got[0].Messages) != 0 {
		t.Fatalf("title search = %+v, %v", got, err)
	}
}

func TestCodexSearchRespectsRecencyWindow(t *testing.T) {
	r := codexFixture(t)
	// E matches but is 33 days old.
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeSearch, Terms: []string{"mascot"}}, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("33 day old match returned: %+v", got)
	}
	// With a window of one conversation, B (second newest) is just outside.
	r.Window = Window{Max: 1, MaxAge: DefaultWindow().MaxAge}
	got, err = r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeSearch, Terms: []string{"flaky"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("match outside the window returned: %+v", got)
	}
	r.Window = Window{Max: 2, MaxAge: DefaultWindow().MaxAge}
	got, _ = r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeSearch, Terms: []string{"flaky"}}, Options{})
	if ids(got) != codexB {
		t.Fatalf("match inside a window of 2 = %s", ids(got))
	}
}

func TestCodexLegacyUserMessageFormat(t *testing.T) {
	r := codexFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeSearch, Terms: []string{"legacy"}, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != codexG {
		t.Fatalf("ids = %s", ids(got))
	}
	u := userTurn(t, got[0])
	if u.Text != "legacy hello with a picture" || colors(t, u.Images) != "purple" {
		t.Fatalf("legacy turn = %q images %d", u.Text, len(u.Images))
	}
}

func TestCodexListShowsThreadsWithCwd(t *testing.T) {
	r := codexFixture(t)
	got, err := r.List(context.Background(), 20, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join([]string{codexA, codexB, codexH, codexG}, ","); ids(got) != want {
		t.Fatalf("list = %s, want %s", ids(got), want)
	}
	if got[0].Cwd != "/Users/matt/Documents/Codex/fox-logo" || got[0].Title != "Fox logo for landing page" || got[0].UpdatedAt.IsZero() {
		t.Fatalf("A = %+v", got[0])
	}
	if got[1].Cwd != "/Users/matt/code/agent-tincan" || got[1].Originator != "codex-tui" {
		t.Fatalf("B = %+v", got[1])
	}
	if got[2].Cwd != "" || got[2].Title != "Pruned thread" {
		t.Fatalf("H (no rollout) = %+v", got[2])
	}
	for _, c := range got {
		if len(c.Messages) != 0 {
			t.Fatalf("list should carry no messages: %+v", c)
		}
	}
	got, _ = r.List(context.Background(), 2, Options{})
	if ids(got) != codexA+","+codexB {
		t.Fatalf("list 2 = %s", ids(got))
	}
}

func TestCodexListAllIncludesExecRunsNotScratch(t *testing.T) {
	r := codexFixture(t)
	got, err := r.List(context.Background(), 20, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join([]string{codexC, codexA, codexB, codexH, codexG}, ","); ids(got) != want {
		t.Fatalf("list --all = %s, want %s", ids(got), want)
	}
	c := got[0]
	if c.Cwd != "/Users/matt/tincan-codex" || !c.Automated || c.Originator != "codex_exec" {
		t.Fatalf("exec entry = %+v", c)
	}
	if !strings.Contains(c.Title, "tincan wake") {
		t.Fatalf("exec title = %q, want its first prompt", c.Title)
	}
}

func TestCodexConversationByID(t *testing.T) {
	r := codexFixture(t)
	got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeConversation, ConversationID: codexA, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
	var roles []string
	for _, m := range got[0].Messages {
		roles = append(roles, string(m.Role)+":"+m.Text)
	}
	want := "user:draw a fox logo for the landing page|assistant:Here is the fox logo.|user:make the ears bigger like this sketch|assistant:Ears enlarged."
	if strings.Join(roles, "|") != want {
		t.Fatalf("messages = %q", roles)
	}
	if got[0].Cwd == "" {
		t.Fatal("cwd missing")
	}
	// Exec runs need --all; scratch threads are never shown.
	if _, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeConversation, ConversationID: codexC}, Options{}); err == nil {
		t.Fatal("exec conversation returned without --all")
	}
	if got, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeConversation, ConversationID: codexC}, Options{All: true}); err != nil || ids(got) != codexC {
		t.Fatalf("exec conversation with --all = %+v, %v", got, err)
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeConversation, ConversationID: codexD}, Options{All: true}); err == nil {
		t.Fatal("scratch conversation returned")
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeConversation, ConversationID: "../../x"}, Options{}); err == nil {
		t.Fatal("path-like id accepted")
	}
}

func TestCodexMissingHomeIsAClearError(t *testing.T) {
	r := &Codex{Home: filepath.Join(t.TempDir(), "nope"), Now: func() time.Time { return fixtureNow }}
	_, err := r.Read(context.Background(), Query{Source: SourceCodex, Mode: ModeLatest}, Options{})
	if err == nil || !strings.Contains(err.Error(), "session_index.jsonl") {
		t.Fatalf("err = %v", err)
	}
}

func TestCodexSourceAndInterface(t *testing.T) {
	var r Reader = &Codex{}
	if r.Source() != SourceCodex {
		t.Fatal(r.Source())
	}
}
