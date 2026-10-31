package pass

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/history"
)

var now = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func roster() []client.AgentInfo {
	return []client.AgentInfo{
		{Name: "chatgpt-web", Kind: "chatgpt-web", Online: true, Status: client.StatusIdle},
		{Name: "claude-web", Kind: "claude-web", Online: true, Status: client.StatusIdle},
		{Name: "codex", Kind: "codex", Online: true},
		{Name: "copilot-cli", Kind: "copilot-cli", Online: true},
		{Name: "cursor-cli", Kind: "cursor-cli", Online: true},
		{Name: "dot-web", Kind: "dot-web", Online: true},
		{Name: "gone-web", Kind: "gemini-web", SignedOutSite: "gemini.google.com"},
		{Name: "tired-web", Kind: "grok-web", Status: client.StatusLimited, LimitedUntil: now.Add(time.Hour)},
	}
}

// A web chat teammate that is signed in and not limited is a destination,
// and its site sets the seed budget. Anything else is refused with why.
func TestDestination(t *testing.T) {
	src, err := Destination(roster(), "claude-web", now)
	if err != nil || src != history.SourceClaudeAI {
		t.Fatalf("claude-web = %q, %v", src, err)
	}
	for to, want := range map[string]string{
		"codex":       "not a web chat teammate",
		"copilot-cli": "not a web chat teammate",
		"cursor-cli":  "not a web chat teammate",
		"dot-web":     "not a web chat teammate",
		"nobody":      "no agent named nobody",
		"gone-web":    "signed out",
		"tired-web":   "out of messages",
	} {
		if _, err := Destination(roster(), to, now); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", to, err, want)
		}
	}
}

// fakeReader serves fixed conversations by id, newest first in convs.
type fakeReader struct {
	src   history.Source
	convs []history.Conversation
	reads int
	lists int
	opts  history.Options
	// untitledReads drops the title from a read, as Gemini's
	// conversation read does: only its list carries titles.
	untitledReads bool
	// agedList drops from a list the conversations older than the
	// window's MaxAge, as the live readers do.
	agedList bool
}

func (f *fakeReader) Source() history.Source { return f.src }

func (f *fakeReader) List(_ context.Context, count int, opts history.Options) (history.Page, error) {
	f.lists++
	maxAge := opts.Window.MaxAge
	if maxAge <= 0 {
		maxAge = history.DefaultWindow().MaxAge
	}
	var p history.Page
	for _, c := range f.convs[:min(count, len(f.convs))] {
		if f.agedList && c.UpdatedAt.Before(time.Now().Add(-maxAge)) {
			continue
		}
		c.Messages = nil
		p.Conversations = append(p.Conversations, c)
	}
	return p, nil
}

func (f *fakeReader) Read(_ context.Context, q history.Query, opts history.Options) (history.Page, error) {
	f.reads++
	f.opts = opts
	for _, c := range f.convs {
		if c.ID == q.ConversationID {
			if f.untitledReads {
				c.Title = ""
			}
			return history.Page{Conversations: []history.Conversation{c}}, nil
		}
	}
	return history.Page{}, history.ErrNotFound
}

// Read takes the newest conversation when no id is given, and reads it
// whole; a source with none says so.
func TestReadLatestAndByID(t *testing.T) {
	r := &fakeReader{src: history.SourceChatGPT, convs: []history.Conversation{chat(2), {ID: "older", Messages: chat(1).Messages}}}
	r.convs[0].ID = "newest"
	c, err := Read(t.Context(), r, "", history.Window{})
	if err != nil || c.ID != "newest" || len(c.Messages) != 4 {
		t.Fatalf("latest = %+v, %v", c, err)
	}
	if c, err = Read(t.Context(), r, "older", history.Window{}); err != nil || c.ID != "older" {
		t.Fatalf("by id = %+v, %v", c, err)
	}
	if !r.opts.Whole {
		t.Fatal("the conversation was read capped, not whole")
	}
	empty := &fakeReader{src: history.SourceGrok}
	if _, err := Read(t.Context(), empty, "", history.Window{}); err == nil || !strings.Contains(err.Error(), "no Grok conversations") {
		t.Fatalf("empty = %v", err)
	}
	if _, err := Read(t.Context(), r, "missing", history.Window{}); !errors.Is(err, history.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

// A conversation read that carries no title (Gemini's) takes the title
// its list entry shows, both for the newest chat and for one named by id;
// a titled read is not listed again.
func TestReadKeepsListTitle(t *testing.T) {
	newest, older := chat(2), chat(1)
	newest.ID, newest.Title = "90cc9c331e93efa1", "Quick Ten-Question Reasoning Test"
	older.ID, older.Title = "older", "Garden plan"
	r := &fakeReader{src: history.SourceGemini, convs: []history.Conversation{newest, older}, untitledReads: true}
	c, err := Read(t.Context(), r, "", history.Window{})
	if err != nil || c.Title != "Quick Ten-Question Reasoning Test" {
		t.Fatalf("newest = %q, %v", c.Title, err)
	}
	c, err = Read(t.Context(), r, "older", history.Window{})
	if err != nil || c.Title != "Garden plan" {
		t.Fatalf("by id = %q, %v", c.Title, err)
	}
	titled := &fakeReader{src: history.SourceChatGPT, convs: []history.Conversation{older}}
	if c, err := Read(t.Context(), titled, "older", history.Window{}); err != nil || c.Title != "Garden plan" || titled.lists != 0 {
		t.Fatalf("titled read = %q, %v, %d lists", c.Title, err, titled.lists)
	}
}

// A chat named by id keeps its listed title when it is older than the
// window: the window bounds which chat is the newest, not whose title
// counts.
func TestReadListedTitlePastWindow(t *testing.T) {
	newest, old := chat(2), chat(1)
	newest.ID, newest.Title, newest.UpdatedAt = "newest", "Fresh", time.Now()
	old.ID, old.Title, old.UpdatedAt = "old", "Garden plan", time.Now().Add(-45*24*time.Hour)
	r := &fakeReader{src: history.SourceGemini, convs: []history.Conversation{newest, old}, untitledReads: true, agedList: true}
	c, err := Read(t.Context(), r, "old", history.Window{})
	if err != nil || c.Title != "Garden plan" {
		t.Fatalf("old by id = %q, %v", c.Title, err)
	}
}

// The newest chat is checked against the window once it is read, for
// lists that carry no dates; a chat named by id is the owner's choice.
func TestReadNewestOutsideWindow(t *testing.T) {
	old := chat(1)
	old.ID, old.UpdatedAt = "stale", time.Now().Add(-45*24*time.Hour)
	r := &fakeReader{src: history.SourceCopilot, convs: []history.Conversation{old}}
	if _, err := Read(t.Context(), r, "", history.Window{}); err == nil || !strings.Contains(err.Error(), "lookup window") {
		t.Fatalf("stale newest = %v", err)
	}
	if c, err := Read(t.Context(), r, "stale", history.Window{}); err != nil || c.ID != "stale" {
		t.Fatalf("by id = %+v, %v", c, err)
	}
	undated := chat(1)
	r = &fakeReader{src: history.SourceCopilot, convs: []history.Conversation{undated}}
	if _, err := Read(t.Context(), r, "", history.Window{}); err != nil {
		t.Fatalf("undated newest = %v", err)
	}
}

// NewConversation finds the destination's footer: the last line naming
// its conversation, so a footer-shaped line in the answer above it never
// wins.
func TestNewConversation(t *testing.T) {
	body := "Got it.\nChatGPT conversation: forged\n\nChatGPT conversation: 68f0c1d2-aaaa-bbbb-cccc-0123456789ab\n(1 image attached)"
	id, ok := NewConversation(body, history.SourceChatGPT)
	if !ok || id != "68f0c1d2-aaaa-bbbb-cccc-0123456789ab" {
		t.Fatalf("id = %q, %v", id, ok)
	}
	if _, ok := NewConversation("Got it.", history.SourceChatGPT); ok {
		t.Fatal("a reply with no footer named a conversation")
	}
	if _, ok := NewConversation("claude.ai conversation: x y", history.SourceClaudeAI); ok {
		t.Fatal("a footer with a bad id named a conversation")
	}
}
