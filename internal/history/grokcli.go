package history

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GrokWakeSandboxProfile is the sandbox profile the grok-cli wake
// (examples/grok-cli/grok-wake.sh) runs Grok Build with. A session
// started with it is a wake run wherever it was stored.
const GrokWakeSandboxProfile = "tincan-wake"

// grokWakeSessions is the suffix of the list, beside a grok-cli teammate's
// tincan config, where its wake records every session id it starts (one
// per line) and its working directory (a line "workdir <path>").
const grokWakeSessions = ".wake-sessions"

// grokWakeHome is the suffix of a grok-cli wake's default home directory
// beside its tincan config (<name>.wake, with .grok inside).
const grokWakeHome = ".wake"

// GrokCLI reads local Grok Build (Grok CLI) sessions:
// <home>/sessions/<url-encoded cwd>/<session id>/ holds chat_history.jsonl
// (one message per line) and summary.json (times, title, cwd), and
// <home>/sessions/<url-encoded cwd>/prompt_history.jsonl the time each
// prompt was sent. Anything else there is ignored.
//
// Wake runs are automated: sessions whose ids a grok-cli wake recorded,
// sessions in a wake's working directory or home (which covers a run the
// timeout killed before its id was recorded), and sessions started with
// the wake's sandbox profile or GROK_HOME. The wake runs Grok with its own
// GROK_HOME, so its sessions are normally not under the owner's at all.
type GrokCLI struct {
	// Home is the owner's Grok home, normally $GROK_HOME or ~/.grok.
	Home string
	// WakeDir holds the grok-cli teammates' tincan configs, their
	// <name>.wake-sessions lists and <name>.wake homes; normally
	// ~/.config/tincan.
	WakeDir string
	// ScratchDir is the history service's own working directory; sessions
	// whose cwd is inside it are never reported.
	ScratchDir string
	Window     Window
	Now        func() time.Time
}

// NewGrokCLI returns a reader for $GROK_HOME or ~/.grok.
func NewGrokCLI() *GrokCLI {
	home := os.Getenv("GROK_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".grok")
		}
	}
	return &GrokCLI{Home: home, WakeDir: configPath("", ""), ScratchDir: DefaultScratchDir()}
}

// Source implements Reader.
func (g *GrokCLI) Source() Source { return SourceGrokCLI }

func (g *GrokCLI) now() time.Time { return orNow(g.Now) }

func (g *GrokCLI) sessionsDir() string { return filepath.Join(g.Home, "sessions") }

// grokSummary is the part of summary.json the reader uses.
type grokSummary struct {
	Info struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"info"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	LastActiveAt   string `json:"last_active_at"`
	GeneratedTitle string `json:"generated_title"`
	SandboxProfile string `json:"sandbox_profile"`
	GrokHome       string `json:"grok_home"`
}

type grokEntry struct {
	id      string
	dir     string // the session directory
	cwdDir  string // its parent, the encoded-cwd directory
	cwd     string
	updated time.Time
	summary grokSummary
}

// candidates returns the sessions newest first. A directory counts as a
// session when its name is an id and it holds summary.json or
// chat_history.jsonl.
func (g *GrokCLI) candidates(ctx context.Context) ([]grokEntry, error) {
	root := g.sessionsDir()
	cwds, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, noHistory("Grok CLI", root)
	}
	if err != nil {
		return nil, fmt.Errorf("grok-cli: read sessions: %w", err)
	}
	var out []grokEntry
	for _, c := range cwds {
		if !c.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cwdDir := filepath.Join(root, c.Name())
		decoded, err := url.PathUnescape(c.Name())
		if err != nil {
			decoded = ""
		}
		sessions, _ := os.ReadDir(cwdDir)
		for _, s := range sessions {
			if !s.IsDir() || !idPattern.MatchString(s.Name()) {
				continue
			}
			dir := filepath.Join(cwdDir, s.Name())
			e := grokEntry{id: s.Name(), dir: dir, cwdDir: cwdDir, cwd: decoded}
			raw, serr := ReadCapped(filepath.Join(dir, "summary.json"), 1<<20)
			chat, cerr := os.Stat(filepath.Join(dir, "chat_history.jsonl"))
			if serr != nil && cerr != nil {
				continue
			}
			if serr == nil && json.Unmarshal(raw, &e.summary) == nil {
				if e.summary.Info.Cwd != "" {
					e.cwd = e.summary.Info.Cwd
				}
				for _, ts := range []string{e.summary.UpdatedAt, e.summary.LastActiveAt, e.summary.CreatedAt} {
					if t := parseTime(ts); t.After(e.updated) {
						e.updated = t
					}
				}
			}
			if e.updated.IsZero() && cerr == nil {
				e.updated = chat.ModTime()
			}
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, noHistory("Grok CLI", root)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].updated.After(out[j].updated) })
	return out, nil
}

// wakeLists is what the wakes on this machine left behind: recorded
// <name>.wake-sessions lists and wake homes, for every command-woken CLI.
type wakeLists struct {
	ids   map[string]bool
	dirs  []string // workdirs and wake homes
	homes []string // the wakes' GROK_HOMEs
}

// wakes reads every <name>.wake-sessions list and <name>.wake home in
// WakeDir. A missing directory or list means no wakes.
func (g *GrokCLI) wakes() wakeLists { return readWakes(g.WakeDir) }

// readWakes reads every <name>.wake-sessions list and <name>.wake home in
// dir, whichever command-woken CLI wrote them. A missing directory or list
// means no wakes.
func readWakes(dir string) wakeLists {
	w := wakeLists{ids: map[string]bool{}}
	if dir == "" {
		return w
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return w
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir() && strings.HasSuffix(e.Name(), grokWakeHome):
			w.dirs = append(w.dirs, p)
			w.homes = append(w.homes, filepath.Join(p, ".grok"))
		case e.Type().IsRegular() && strings.HasSuffix(e.Name(), grokWakeSessions):
			w.readList(p)
		}
	}
	return w
}

// readList adds the ids and workdir lines of one recorded list; other
// lines are ignored.
func (w *wakeLists) readList(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if dir, ok := strings.CutPrefix(line, "workdir "); ok {
			if dir = strings.TrimSpace(dir); filepath.IsAbs(dir) {
				w.dirs = append(w.dirs, dir)
			}
			continue
		}
		if idPattern.MatchString(line) {
			w.ids[line] = true
		}
	}
}

// automated reports whether e is a wake run.
func (w wakeLists) automated(e grokEntry) bool {
	if w.ids[e.id] || e.summary.SandboxProfile == GrokWakeSandboxProfile {
		return true
	}
	if w.inWakeDir(e.cwd) {
		return true
	}
	for _, h := range w.homes {
		if e.summary.GrokHome != "" && inDirResolved(e.summary.GrokHome, h) {
			return true
		}
	}
	return false
}

// inWakeDir reports whether cwd is a wake's working directory or inside a
// wake home.
func (w wakeLists) inWakeDir(cwd string) bool {
	for _, d := range w.dirs {
		if inDirResolved(cwd, d) {
			return true
		}
	}
	return false
}

// inDirResolved is inDir, also trying dir with its symlinks resolved
// (Grok records /private/tmp for /tmp on macOS).
func inDirResolved(path, dir string) bool {
	if inDir(path, dir) {
		return true
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil && r != dir {
		return inDir(path, r)
	}
	return false
}

// grokBlock is one content block of a chat_history.jsonl message.
type grokBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	URL  string `json:"url"`
}

// grokRecord is one chat_history.jsonl line.
type grokRecord struct {
	Type            string          `json:"type"`
	Content         json.RawMessage `json:"content"`
	SyntheticReason string          `json:"synthetic_reason"`
	PromptIndex     *int            `json:"prompt_index"`
}

// blocks returns the content as blocks; a plain string becomes one text
// block.
func (r grokRecord) blocks() []grokBlock {
	if len(r.Content) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(r.Content, &s) == nil {
		return []grokBlock{{Type: "text", Text: s}}
	}
	var bs []grokBlock
	_ = json.Unmarshal(r.Content, &bs)
	return bs
}

// grokPromptTimes maps session id to the times of its typed prompts, in
// order, from the encoded-cwd directory's prompt_history.jsonl.
type grokPrompt struct {
	text string
	at   time.Time
	used bool
}

func grokPromptTimes(cwdDir, id string) []grokPrompt {
	var out []grokPrompt
	_ = scanFile(filepath.Join(cwdDir, "prompt_history.jsonl"), func(line []byte) bool {
		var rec struct {
			Timestamp string `json:"timestamp"`
			SessionID string `json:"session_id"`
			Prompt    string `json:"prompt"`
			IsBash    bool   `json:"is_bash"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.SessionID != id || rec.IsBash {
			return true
		}
		out = append(out, grokPrompt{text: strings.TrimSpace(rec.Prompt), at: parseTime(rec.Timestamp)})
		return true
	})
	return out
}

// parse reads one session into a thread, applying the exclusions: the
// scratch dir always, wake runs unless all.
func (g *GrokCLI) parse(e grokEntry, w wakeLists, all, images bool) (thread, bool, error) {
	if inDir(e.cwd, g.ScratchDir) {
		return thread{}, false, nil
	}
	conv := Conversation{Source: SourceGrokCLI, ID: e.id, Title: e.summary.GeneratedTitle, UpdatedAt: e.updated, Cwd: e.cwd}
	conv.Automated = w.automated(e)
	if conv.Automated && !all {
		return thread{}, false, nil
	}
	var turns []turn
	err := scanFile(filepath.Join(e.dir, "chat_history.jsonl"), func(line []byte) bool {
		var r grokRecord
		if json.Unmarshal(line, &r) != nil {
			return true
		}
		switch r.Type {
		case "user":
			if r.SyntheticReason != "" {
				return true
			}
			var texts []string
			var imgs []Image
			nImages := 0
			for _, b := range r.blocks() {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "image":
					nImages++
					if images {
						if img, ok := grokImage(b.URL); ok {
							imgs = appendImage(imgs, img)
						}
					}
				}
			}
			text := strings.TrimSpace(strings.Join(texts, "\n"))
			// Grok opens each session with a context block (<user_info>)
			// in the user role; typed prompts carry a prompt_index.
			if r.PromptIndex == nil && injectedElement(text) {
				return true
			}
			if text == "" && nImages == 0 {
				return true
			}
			turns = append(turns, turn{prompt: Message{Role: RoleUser, Text: text, Images: imgs}})
		case "assistant":
			if len(turns) == 0 {
				return true
			}
			var s string
			if json.Unmarshal(r.Content, &s) == nil && strings.TrimSpace(s) != "" {
				turns[len(turns)-1].reply = Message{Role: RoleAssistant, Text: s}
			}
		}
		return true
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return thread{}, false, fmt.Errorf("grok-cli: read session %s: %w", e.id, err)
	}
	g.stamp(e, turns)
	if conv.Title == "" && len(turns) > 0 {
		conv.Title = titleFrom(turns[0].prompt.Text)
	}
	return thread{conv: conv, turns: turns}, true, nil
}

// stamp sets each prompt's time from prompt_history.jsonl, matching by
// text in order. chat_history.jsonl has no times, so a prompt with no
// match gets the session's start (the first) or its last update (the
// rest), which keeps the newest prompt newest.
func (g *GrokCLI) stamp(e grokEntry, turns []turn) {
	if len(turns) == 0 {
		return
	}
	times := grokPromptTimes(e.cwdDir, e.id)
	created := parseTime(e.summary.CreatedAt)
	if created.IsZero() {
		created = e.updated
	}
	for i := range turns {
		t := &turns[i]
		for j := range times {
			if !times[j].used && times[j].text == t.prompt.Text {
				times[j].used = true
				t.prompt.Time = times[j].at
				break
			}
		}
		if t.prompt.Time.IsZero() {
			if i == 0 {
				t.prompt.Time = created
			} else {
				t.prompt.Time = e.updated
			}
		}
	}
	last := &turns[len(turns)-1]
	if last.reply.Text != "" {
		last.reply.Time = e.updated
	}
}

// grokImage decodes an image block's url: a data: URL, or a local file
// (a file:// URL or an absolute path).
func grokImage(u string) (Image, bool) {
	if b, ok := decodeDataURL(u); ok {
		return newImage(b, "")
	}
	if p, ok := strings.CutPrefix(u, "file://"); ok {
		u = p
	}
	if filepath.IsAbs(u) {
		return readImageFile(u)
	}
	return Image{}, false
}

// List implements Reader.
func (g *GrokCLI) List(ctx context.Context, count int, opts Options) (Page, error) {
	if err := checkListCount(count); err != nil {
		return Page{}, err
	}
	cands, err := g.candidates(ctx)
	if err != nil {
		return Page{}, err
	}
	wk := g.wakes()
	w, now := effectiveWindow(g.Window, opts), g.now()
	page := Page{Window: w}
	for _, e := range cands {
		if len(page.Conversations) >= count {
			break
		}
		if !w.fresh(e.updated, now) {
			page.Limited = LimitAge
			break
		}
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		th, ok, err := g.parse(e, wk, opts.All, false)
		if err != nil {
			return Page{}, err
		}
		if ok && len(th.turns) > 0 {
			page.Conversations = append(page.Conversations, th.conv)
		}
	}
	return page, nil
}

// Read implements Reader.
func (g *GrokCLI) Read(ctx context.Context, q Query, opts Options) (Page, error) {
	if err := q.Validate(); err != nil {
		return Page{}, err
	}
	if err := checkSource(SourceGrokCLI, q); err != nil {
		return Page{}, err
	}
	cands, err := g.candidates(ctx)
	if err != nil {
		return Page{}, err
	}
	wk := g.wakes()
	if q.Mode == ModeConversation {
		for _, e := range cands {
			if e.id != q.ConversationID {
				continue
			}
			th, ok, err := g.parse(e, wk, opts.All, q.wantsImages())
			if err != nil {
				return Page{}, err
			}
			if ok {
				return Page{Conversations: []Conversation{conversationMessages(th, q.wantsImages())}, Window: effectiveWindow(g.Window, opts)}, nil
			}
		}
		return Page{}, fmt.Errorf("grok-cli: %w: %s", ErrNotFound, q.ConversationID)
	}
	return pick(q, effectiveWindow(g.Window, opts), g.now(), len(cands), false,
		func(i int) time.Time { return cands[i].updated },
		func(i int) (thread, bool, error) {
			if err := ctx.Err(); err != nil {
				return thread{}, false, err
			}
			th, ok, err := g.parse(cands[i], wk, opts.All, q.wantsImages())
			// A session with no typed prompt does not use up a window slot.
			return th, ok && len(th.turns) > 0, err
		})
}
