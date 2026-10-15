package history

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	geminiCLI1 = "0g0g0000-0000-4000-8000-000000000001" // owner, rewound and patched
	geminiCLI2 = "0g0g0000-0000-4000-8000-000000000002" // owner, legacy .json file
	geminiCLI3 = "0g0g0000-0000-4000-8000-000000000003" // wake run, recorded id
	geminiCLI4 = "0g0g0000-0000-4000-8000-000000000004" // history's own scratch run
	geminiCLI5 = "0g0g0000-0000-4000-8000-000000000005" // forty days old
	geminiCLI6 = "0g0g0000-0000-4000-8000-000000000006" // a subagent's session
)

// geminiCLIFixture copies testdata/gemini-cli: tmp/ is the owner's
// ~/.gemini/tmp, tincan/ holds the wake's list.
func geminiCLIFixture(t *testing.T) *GeminiCLI {
	t.Helper()
	root := copyTree(t, "gemini-cli")
	return &GeminiCLI{Home: root, WakeDir: filepath.Join(root, "tincan"), ScratchDir: fixtureScratch, Now: func() time.Time { return fixtureNow }}
}

func TestGeminiCLIListNewestFirstWithoutWakeOrScratchRuns(t *testing.T) {
	r := geminiCLIFixture(t)
	page, err := r.List(context.Background(), 20, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// 6 is a subagent's session, which is never listed.
	if got := ids(page.Conversations); got != geminiCLI1+","+geminiCLI2 || page.Limited != LimitAge {
		t.Fatalf("list = %s (limited %q), want 1,2 limited by age", got, page.Limited)
	}
	c := page.Conversations[0]
	if c.Source != SourceGeminiCLI || c.Title != "Makefile rebuilds" || c.Cwd != "/Users/matt/code/x" || c.Automated || !c.UpdatedAt.Equal(time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("first = %+v", c)
	}
	if c := page.Conversations[1]; c.Title != "list the rent receipts script options" || c.Cwd != "/Users/matt/code/y" {
		t.Fatalf("legacy = %+v", c)
	}
	page, err = r.List(context.Background(), 20, Options{All: true})
	if err != nil || ids(page.Conversations) != geminiCLI3+","+geminiCLI1+","+geminiCLI2 || !page.Conversations[0].Automated {
		t.Fatalf("list --all = %s, %v", ids(page.Conversations), err)
	}
}

// The session file is replayed: a rewind drops what came after it, a
// patch replaces a message's content, and the prompt shown is what the
// owner typed (displayContent) rather than the expanded text.
func TestGeminiCLIConversationReplaysTheLog(t *testing.T) {
	r := geminiCLIFixture(t)
	got, err := convsOf(r.Read(context.Background(), Query{Source: SourceGeminiCLI, Mode: ModeConversation, ConversationID: geminiCLI1}, Options{}))
	if err != nil || len(got) != 1 {
		t.Fatalf("conversation = %+v, %v", got, err)
	}
	var texts []string
	for _, m := range got[0].Messages {
		texts = append(texts, m.Text)
	}
	want := "why does the Makefile rebuild everything|The all target has no prerequisites list, so it always runs.|fix it in @Makefile|Added the prerequisites."
	if strings.Join(texts, "|") != want {
		t.Fatalf("messages = %q\nwant %q", strings.Join(texts, "|"), want)
	}
	latest, err := convsOf(r.Read(context.Background(), Query{Source: SourceGeminiCLI, Mode: ModeLatest}, Options{}))
	if err != nil || len(latest) != 1 || !latest[0].Messages[0].Time.Equal(time.Date(2026, 9, 21, 9, 50, 0, 0, time.UTC)) {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceGeminiCLI, Mode: ModeConversation, ConversationID: geminiCLI3}, Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wake run by id: err = %v, want not found", err)
	}
}

// Session files modified before the window are not replayed for a list
// or a latest read, but a lookup by id still finds an old session.
func TestGeminiCLISkipsFilesOlderThanTheWindow(t *testing.T) {
	r := geminiCLIFixture(t)
	chats := filepath.Join(r.Home, "tmp", "x", "chats")
	setMtime(t, filepath.Join(chats, "session-2026-08-01T00-00-0g0g0000.jsonl"), "2026-08-01T00:00:05Z")
	// An old file that is not JSON and cannot be opened: replaying it would
	// fail the read.
	bad := filepath.Join(chats, "session-2026-07-01T00-00-deadbeef.jsonl")
	if err := os.WriteFile(bad, []byte("not json {"), 0o644); err != nil {
		t.Fatal(err)
	}
	setMtime(t, bad, "2026-07-01T00:00:00Z")
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })
	if f, err := os.Open(bad); err == nil {
		_ = f.Close()
		t.Skip("running with permission to read a mode 0 file")
	}
	page, err := r.List(context.Background(), 20, Options{})
	if err != nil || ids(page.Conversations) != geminiCLI1+","+geminiCLI2 || page.Limited != LimitAge {
		t.Fatalf("list = %s (limited %q), %v; want 1,2 limited by age", ids(page.Conversations), page.Limited, err)
	}
	if _, err := convsOf(r.Read(context.Background(), Query{Source: SourceGeminiCLI, Mode: ModeLatest}, Options{})); err != nil {
		t.Fatalf("latest: %v", err)
	}
	got, err := convsOf(r.Read(context.Background(), Query{Source: SourceGeminiCLI, Mode: ModeConversation, ConversationID: geminiCLI5}, Options{}))
	if err != nil || len(got) != 1 || got[0].ID != geminiCLI5 || len(got[0].Messages) == 0 {
		t.Fatalf("old session by id = %+v, %v", got, err)
	}
}

func TestGeminiCLINoHistory(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	r := &GeminiCLI{Home: home}
	_, err := r.Read(context.Background(), Query{Source: SourceGeminiCLI, Mode: ModeLatest}, Options{})
	if want := "no Gemini CLI history found in " + filepath.Join(home, "tmp"); !errors.Is(err, ErrNoHistory) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// NewGeminiCLI reads $GEMINI_CLI_HOME/.gemini, ~/.gemini by default.
func TestNewGeminiCLIHonorsGeminiCLIHome(t *testing.T) {
	t.Setenv("GEMINI_CLI_HOME", "/tmp/somewhere")
	if r := NewGeminiCLI(); r.Home != "/tmp/somewhere/.gemini" || r.Source() != SourceGeminiCLI {
		t.Fatalf("reader = %+v", r)
	}
	t.Setenv("GEMINI_CLI_HOME", "")
	h, _ := os.UserHomeDir()
	if r := NewGeminiCLI(); r.Home != filepath.Join(h, ".gemini") {
		t.Fatalf("default home = %q", r.Home)
	}
}

// AE7: the query step keeps a bare "Gemini" or "Copilot" on the web
// sources and sends only questions that say CLI (or terminal) to the CLI
// sources; the schema and the structured-query help name every source.
func TestExtractorRoutesCLISourcesApartFromWebSources(t *testing.T) {
	for _, want := range []string{
		`"gemini" for Gemini or gemini.google.com`,
		`"gemini-cli" for the Gemini CLI`,
		`"copilot" for Microsoft Copilot`,
		`"copilot-cli" for the GitHub Copilot CLI`,
		`"cursor-cli" for the Cursor CLI`,
		`"cursor-ide" for Cursor, the editor`,
		`"opencode" for OpenCode`,
		`A bare "Gemini" or "Copilot" is always the web source`,
	} {
		if !strings.Contains(extractInstructions, want) {
			t.Errorf("extractor instructions lack %q", want)
		}
	}
	for _, s := range []Source{SourceCursorCLI, SourceCursorIDE, SourceCopilotCLI, SourceGeminiCLI, SourceOpenCode} {
		if !strings.Contains(extractSchema, `"`+string(s)+`"`) || !strings.Contains(structuredHelp, string(s)) {
			t.Errorf("schema or structured help lacks %q", s)
		}
	}
	// "what did I ask Gemini about rent" and "what did I ask gemini cli
	// about the Makefile", as the query step answers them.
	for src, want := range map[string]Source{"gemini": SourceGemini, "gemini-cli": SourceGeminiCLI, "copilot": SourceCopilot, "copilot-cli": SourceCopilotCLI} {
		q, err := parseExtraction([]byte(`{"source":"` + src + `","mode":"search","terms":["rent"],"conversation_id":"","count":1,"want_images":false,"with_images":false}`))
		if err != nil || q.Source != want {
			t.Fatalf("%s extraction = %+v, %v", src, q, err)
		}
	}
}
