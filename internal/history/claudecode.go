package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// claudeSDKEntrypoint marks Claude Code sessions started through the SDK
// (unattended agents), not typed by Matt.
const claudeSDKEntrypoint = "sdk-cli"

// ClaudeCode reads local Claude Code transcripts:
// <projects>/<project>/<session>.jsonl. Subagent transcripts live one level
// deeper (<session>/subagents/) and are never read.
type ClaudeCode struct {
	// Root is the projects directory, normally ~/.claude/projects.
	Root string
	// ScratchDir is the history service's own working directory; sessions
	// whose cwd is inside it are never reported.
	ScratchDir string
	Window     Window
	Now        func() time.Time
}

// NewClaudeCode returns a reader for $CLAUDE_CONFIG_DIR/projects or
// ~/.claude/projects.
func NewClaudeCode() *ClaudeCode {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(h, ".claude")
		}
	}
	return &ClaudeCode{Root: filepath.Join(dir, "projects"), ScratchDir: DefaultScratchDir()}
}

// Source implements Reader.
func (c *ClaudeCode) Source() Source { return SourceClaudeCode }

func (c *ClaudeCode) now() time.Time { return orNow(c.Now) }

type claudeEntry struct {
	id      string
	path    string
	updated time.Time
}

// candidates returns session transcripts newest (by mtime) first.
func (c *ClaudeCode) candidates(ctx context.Context) ([]claudeEntry, error) {
	projects, err := os.ReadDir(c.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, noHistory("Claude Code", c.Root)
	}
	if err != nil {
		return nil, fmt.Errorf("claude-code: read projects: %w", err)
	}
	var out []claudeEntry
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := filepath.Join(c.Root, p.Name())
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			name := f.Name()
			if !f.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if !idPattern.MatchString(id) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			out = append(out, claudeEntry{id: id, path: filepath.Join(dir, name), updated: info.ModTime()})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].updated.After(out[j].updated) })
	return out, nil
}

type claudeBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

type claudeRecord struct {
	Type                      string `json:"type"`
	Timestamp                 string `json:"timestamp"`
	Entrypoint                string `json:"entrypoint"`
	Cwd                       string `json:"cwd"`
	IsMeta                    bool   `json:"isMeta"`
	IsSidechain               bool   `json:"isSidechain"`
	IsCompactSummary          bool   `json:"isCompactSummary"`
	IsVisibleInTranscriptOnly bool   `json:"isVisibleInTranscriptOnly"`
	PromptSource              string `json:"promptSource"`
	Origin                    *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	AITitle     string `json:"aiTitle"`
	CustomTitle string `json:"customTitle"`
}

// content returns the message content as blocks; a plain string becomes
// one text block.
func (r claudeRecord) content() []claudeBlock {
	if r.Message == nil || len(r.Message.Content) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(r.Message.Content, &s) == nil {
		return []claudeBlock{{Type: "text", Text: s}}
	}
	var blocks []claudeBlock
	_ = json.Unmarshal(r.Message.Content, &blocks)
	return blocks
}

// humanTyped reports whether a user record could be something Matt typed,
// before looking at its text.
func (r claudeRecord) humanTyped() bool {
	if r.Type != "user" || r.IsMeta || r.IsSidechain || r.IsCompactSummary || r.IsVisibleInTranscriptOnly {
		return false
	}
	if r.Origin != nil && r.Origin.Kind != "human" {
		return false
	}
	return r.PromptSource != "system"
}

// claudeBuiltins are Claude Code's own slash commands; running one is not
// a question to the model.
var claudeBuiltins = map[string]bool{
	"/add-dir": true, "/agents": true, "/clear": true, "/compact": true, "/config": true,
	"/context": true, "/cost": true, "/doctor": true, "/exit": true, "/export": true,
	"/help": true, "/hooks": true, "/ide": true, "/init": true, "/login": true,
	"/logout": true, "/mcp": true, "/memory": true, "/model": true, "/permissions": true,
	"/release-notes": true, "/resume": true, "/rewind": true, "/status": true,
	"/theme": true, "/upgrade": true, "/usage": true, "/vim": true,
}

// claudeInjected are prefixes of user-role text that Claude Code writes
// itself. The Desktop app's scheduled tasks and Create PR button are
// among them: their records look typed (origin human, promptSource sdk).
var claudeInjected = []string{
	"<local-command-", "<task-notification", "<system-reminder>", "<bash-",
	"<fork-boilerplate>", "<user-prompt-submit-hook>", "[Request interrupted",
	"Caveat: The messages below", "<scheduled-task", "<create-pr-command",
}

// openTag matches the start of an XML element and captures its name.
var openTag = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9_-]*)[\s>]`)

// injectedElement reports whether s is one XML element, <name>...</name>
// with nothing outside it, whose name has a '-' or '_'. Everything Claude
// Code writes into the user role itself has that shape, and the Desktop
// app keeps adding kinds, so the claudeInjected prefixes alone would
// report each new kind as something Matt typed until it was listed.
// People do wrap prompts in tags like <instructions> or paste HTML and
// SVG, so a plain one-word name is never taken as injected, and the
// first closing tag must be the one at the end.
func injectedElement(s string) bool {
	m := openTag.FindStringSubmatch(s)
	if m == nil || !strings.ContainsAny(m[1], "-_") {
		return false
	}
	end := "</" + m[1] + ">"
	return strings.HasSuffix(s, end) && strings.Index(s, end) == len(s)-len(end)
}

// claudePromptText turns user-role text into Matt's prompt, or reports
// false for text Claude Code injected. A custom slash command becomes
// "/name args". sdk says the record came in with promptSource "sdk", the
// way the Desktop app's injections arrive; only those can be dropped as an
// unlisted injected element, so a prompt Matt typed or queued never is.
func claudePromptText(s string, sdk bool) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if strings.Contains(s, "<command-name>") {
		name := strings.TrimSpace(between(s, "<command-name>", "</command-name>"))
		args := strings.TrimSpace(between(s, "<command-args>", "</command-args>"))
		if name == "" || claudeBuiltins[name] {
			return "", false
		}
		if args != "" {
			name += " " + args
		}
		return name, true
	}
	for _, p := range claudeInjected {
		if strings.HasPrefix(s, p) {
			return "", false
		}
	}
	if sdk && injectedElement(s) {
		return "", false
	}
	return s, true
}

func between(s, open, closing string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	s = s[i+len(open):]
	if before, _, ok := strings.Cut(s, closing); ok {
		return before
	}
	return s
}

// parse streams one transcript into a thread, applying the exclusions.
func (c *ClaudeCode) parse(e claudeEntry, all, images bool) (thread, bool, error) {
	conv := Conversation{Source: SourceClaudeCode, ID: e.id, UpdatedAt: e.updated}
	var turns []turn
	var aiTitle, customTitle string
	excluded := false
	err := scanFile(e.path, func(line []byte) bool {
		var r claudeRecord
		if json.Unmarshal(line, &r) != nil {
			return true
		}
		if conv.Originator == "" && r.Entrypoint != "" {
			conv.Originator = r.Entrypoint
			conv.Automated = r.Entrypoint == claudeSDKEntrypoint
			if conv.Automated && !all {
				excluded = true
				return false
			}
		}
		if conv.Cwd == "" && r.Cwd != "" {
			conv.Cwd = r.Cwd
			if inDir(r.Cwd, c.ScratchDir) {
				excluded = true
				return false
			}
		}
		switch r.Type {
		case "ai-title":
			aiTitle = r.AITitle
		case "custom-title":
			customTitle = r.CustomTitle
		case "user":
			if !r.humanTyped() {
				return true
			}
			var texts []string
			var imgs []Image
			nImages := 0
			for _, b := range r.content() {
				switch b.Type {
				case "tool_result":
					return true
				case "text":
					texts = append(texts, b.Text)
				case "image":
					nImages++
					if images && b.Source != nil && b.Source.Type == "base64" {
						if data, ok := decodeBase64(b.Source.Data); ok {
							if img, ok := newImage(data, ""); ok {
								imgs = appendImage(imgs, img)
							}
						}
					}
				}
			}
			text, ok := claudePromptText(strings.Join(texts, "\n"), r.PromptSource == "sdk")
			if !ok && nImages == 0 {
				return true
			}
			turns = append(turns, turn{prompt: Message{Role: RoleUser, Text: text, Time: parseTime(r.Timestamp), Images: imgs}})
		case "assistant":
			if len(turns) == 0 {
				return true
			}
			var parts []string
			for _, b := range r.content() {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					parts = append(parts, b.Text)
				}
			}
			if len(parts) > 0 {
				turns[len(turns)-1].reply = Message{Role: RoleAssistant, Text: strings.Join(parts, "\n"), Time: parseTime(r.Timestamp)}
			}
		}
		return true
	})
	if err != nil {
		return thread{}, false, fmt.Errorf("claude-code: read session %s: %w", e.id, err)
	}
	if excluded {
		return thread{}, false, nil
	}
	switch {
	case customTitle != "":
		conv.Title = customTitle
	case aiTitle != "":
		conv.Title = aiTitle
	case len(turns) > 0:
		conv.Title = titleFrom(turns[0].prompt.Text)
	}
	return thread{conv: conv, turns: turns}, true, nil
}

// List implements Reader.
func (c *ClaudeCode) List(ctx context.Context, count int, opts Options) ([]Conversation, error) {
	if err := checkListCount(count); err != nil {
		return nil, err
	}
	cands, err := c.candidates(ctx)
	if err != nil {
		return nil, err
	}
	w, now := c.Window.orDefault(), c.now()
	var out []Conversation
	for _, e := range cands {
		if len(out) >= count || !w.fresh(e.updated, now) {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		th, ok, err := c.parse(e, opts.All, false)
		if err != nil {
			return nil, err
		}
		if ok && len(th.turns) > 0 {
			out = append(out, th.conv)
		}
	}
	return out, nil
}

// Read implements Reader.
func (c *ClaudeCode) Read(ctx context.Context, q Query, opts Options) ([]Conversation, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if err := checkSource(SourceClaudeCode, q); err != nil {
		return nil, err
	}
	cands, err := c.candidates(ctx)
	if err != nil {
		return nil, err
	}
	if q.Mode == ModeConversation {
		for _, e := range cands {
			if e.id != q.ConversationID {
				continue
			}
			th, ok, err := c.parse(e, opts.All, q.wantsImages())
			if err != nil {
				return nil, err
			}
			if ok {
				return []Conversation{conversationMessages(th, q.wantsImages())}, nil
			}
		}
		return nil, fmt.Errorf("claude-code: %w: %s", ErrNotFound, q.ConversationID)
	}
	return pick(q, c.Window, c.now(), len(cands),
		func(i int) time.Time { return cands[i].updated },
		func(i int) (thread, bool, error) {
			if err := ctx.Err(); err != nil {
				return thread{}, false, err
			}
			th, ok, err := c.parse(cands[i], opts.All, q.wantsImages())
			// A transcript with no prompts from Matt does not use up a
			// window slot.
			return th, ok && len(th.turns) > 0, err
		})
}
