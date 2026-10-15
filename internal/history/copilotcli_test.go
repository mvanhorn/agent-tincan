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
	copilotCLI1 = "0c0p0000-0000-4000-8000-000000000001" // owner, two prompts, named
	copilotCLI2 = "0c0p0000-0000-4000-8000-000000000002" // owner, unnamed
	copilotCLI3 = "0c0p0000-0000-4000-8000-000000000003" // wake run, recorded id
	copilotCLI4 = "0c0p0000-0000-4000-8000-000000000004" // history's own scratch run
	copilotCLI5 = "0c0p0000-0000-4000-8000-000000000005" // forty days old
)

// copilotCLIFixture copies testdata/copilot-cli: session-state/ is the
// owner's ~/.copilot/session-state, tincan/ holds the wake's list.
func copilotCLIFixture(t *testing.T) *CopilotCLI {
	t.Helper()
	root := copyTree(t, "copilot-cli")
	return &CopilotCLI{Home: root, WakeDir: filepath.Join(root, "tincan"), ScratchDir: fixtureScratch, Now: func() time.Time { return fixtureNow }}
}

func TestCopilotCLIListNewestFirstWithoutWakeOrScratchRuns(t *testing.T) {
	r := copilotCLIFixture(t)
	page, err := r.List(context.Background(), 20, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page.Conversations); got != copilotCLI1+","+copilotCLI2 || page.Limited != LimitAge {
		t.Fatalf("list = %s (limited %q), want 1,2 limited by age", got, page.Limited)
	}
	c := page.Conversations[0]
	if c.Source != SourceCopilotCLI || c.Title != "Fix the flaky test" || c.Cwd != "/Users/matt/code/x" || c.Automated {
		t.Fatalf("first = %+v", c)
	}
	if page.Conversations[1].Title != "summarize the onboarding doc" {
		t.Fatalf("unnamed title = %q", page.Conversations[1].Title)
	}
	page, err = r.List(context.Background(), 20, Options{All: true})
	if err != nil || ids(page.Conversations) != copilotCLI3+","+copilotCLI1+","+copilotCLI2 || !page.Conversations[0].Automated {
		t.Fatalf("list --all = %s, %v", ids(page.Conversations), err)
	}
}

func TestCopilotCLILatestReadsTypedPromptAndFinalReply(t *testing.T) {
	r := copilotCLIFixture(t)
	got, err := convsOf(r.Read(context.Background(), Query{Source: SourceCopilotCLI, Mode: ModeLatest, Count: 2}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != copilotCLI1 || got[1].ID != copilotCLI1 {
		t.Fatalf("latest 2 = %s", ids(got))
	}
	newest, older := got[0].Messages, got[1].Messages
	if len(newest) != 2 || newest[0].Text != "does it pass now" || newest[1].Text != "Yes, ten runs in a row." || !newest[0].Time.Equal(time.Date(2026, 9, 21, 9, 40, 0, 0, time.UTC)) {
		t.Fatalf("newest = %+v", newest)
	}
	if len(older) != 2 || older[0].Text != "fix the flaky relay test" || older[1].Text != "Added a retry around the dial." {
		t.Fatalf("older = %+v", older)
	}
	for _, c := range got {
		for _, m := range c.Messages {
			if strings.Contains(m.Text, "current_datetime") || strings.Contains(m.Text, "GitHub Copilot CLI") || strings.Contains(m.Text, "package main") {
				t.Fatalf("injected or tool text leaked into %q", m.Text)
			}
		}
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceCopilotCLI, Mode: ModeConversation, ConversationID: copilotCLI3}, Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wake run by id: err = %v, want not found", err)
	}
}

func TestCopilotCLINoHistory(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".copilot")
	r := &CopilotCLI{Home: home}
	_, err := r.Read(context.Background(), Query{Source: SourceCopilotCLI, Mode: ModeLatest}, Options{})
	if want := "no Copilot CLI history found in " + filepath.Join(home, "session-state"); !errors.Is(err, ErrNoHistory) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// NewCopilotCLI reads $COPILOT_HOME, ~/.copilot by default.
func TestNewCopilotCLIHonorsCopilotHome(t *testing.T) {
	t.Setenv("COPILOT_HOME", "/tmp/somewhere/copilot")
	if r := NewCopilotCLI(); r.Home != "/tmp/somewhere/copilot" || r.Source() != SourceCopilotCLI {
		t.Fatalf("reader = %+v", r)
	}
	t.Setenv("COPILOT_HOME", "")
	h, _ := os.UserHomeDir()
	if r := NewCopilotCLI(); r.Home != filepath.Join(h, ".copilot") {
		t.Fatalf("default home = %q", r.Home)
	}
}
