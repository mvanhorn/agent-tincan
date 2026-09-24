package history

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// codexExecOriginator marks unattended `codex exec` runs, including every
// Tincan wake. They are not "what Matt asked".
const codexExecOriginator = "codex_exec"

// Codex reads local Codex history: session_index.jsonl lists Matt's
// threads (Desktop and interactive CLI), rollouts under
// sessions/YYYY/MM/DD hold content, images and cwd, and
// generated_images/<thread id>/ holds images Codex generated.
type Codex struct {
	// Home is the Codex home, normally ~/.codex.
	Home string
	// ScratchDir is the history service's own working directory; threads
	// whose cwd is inside it are never reported.
	ScratchDir string
	Window     Window
	Now        func() time.Time
}

// NewCodex returns a reader for $CODEX_HOME or ~/.codex.
func NewCodex() *Codex {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	return &Codex{Home: home, ScratchDir: DefaultScratchDir()}
}

// Source implements Reader.
func (c *Codex) Source() Source { return SourceCodex }

func (c *Codex) now() time.Time { return orNow(c.Now) }

// codexEntry is one candidate thread.
type codexEntry struct {
	id      string
	name    string
	updated time.Time
	inIndex bool
	rollout string
}

// candidates returns threads newest first: every index thread, plus with
// all every rollout on disk that the index does not list (exec runs).
func (c *Codex) candidates(ctx context.Context, all bool) ([]codexEntry, error) {
	byID := map[string]*codexEntry{}
	var order []*codexEntry
	err := scanFile(filepath.Join(c.Home, "session_index.jsonl"), func(line []byte) bool {
		var rec struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
			UpdatedAt  string `json:"updated_at"`
		}
		if json.Unmarshal(line, &rec) != nil || !idPattern.MatchString(rec.ID) {
			return true
		}
		e := byID[rec.ID]
		if e == nil {
			e = &codexEntry{id: rec.ID, inIndex: true}
			byID[rec.ID] = e
			order = append(order, e)
		}
		// Later lines are renames and updates of the same thread.
		if rec.ThreadName != "" {
			e.name = rec.ThreadName
		}
		if t := parseTime(rec.UpdatedAt); t.After(e.updated) {
			e.updated = t
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("codex: read session_index.jsonl: %w", err)
	}
	rollouts, err := c.rollouts(ctx)
	if err != nil {
		return nil, err
	}
	for id, path := range rollouts {
		if e := byID[id]; e != nil {
			e.rollout = path
			continue
		}
		if !all {
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		e := &codexEntry{id: id, updated: st.ModTime(), rollout: path}
		byID[id] = e
		order = append(order, e)
	}
	out := make([]codexEntry, 0, len(order))
	for _, e := range order {
		out = append(out, *e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].updated.After(out[j].updated) })
	return out, nil
}

// rollouts maps thread id to rollout path from the file names
// (rollout-<timestamp>-<uuid>.jsonl) without opening any file.
func (c *Codex) rollouts(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	root := filepath.Join(c.Home, "sessions")
	years, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex: read sessions: %w", err)
	}
	for _, y := range years {
		months, _ := os.ReadDir(filepath.Join(root, y.Name()))
		for _, m := range months {
			days, _ := os.ReadDir(filepath.Join(root, y.Name(), m.Name()))
			for _, d := range days {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				dir := filepath.Join(root, y.Name(), m.Name(), d.Name())
				files, _ := os.ReadDir(dir)
				for _, f := range files {
					name := f.Name()
					if !f.Type().IsRegular() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
						continue
					}
					base := strings.TrimSuffix(name, ".jsonl")
					if len(base) < 36 {
						continue
					}
					id := base[len(base)-36:]
					if idPattern.MatchString(id) {
						out[id] = filepath.Join(dir, name)
					}
				}
			}
		}
	}
	return out, nil
}

// codexContent is one content item in a response_item message or an
// item_completed item.
type codexContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
	Path     string          `json:"path"`
}

func (cc codexContent) imageURL() string {
	var s string
	if json.Unmarshal(cc.ImageURL, &s) == nil {
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(cc.ImageURL, &obj)
	return obj.URL
}

// codexPayload is the union of the payload shapes the reader uses.
type codexPayload struct {
	Type    string         `json:"type"`
	Role    string         `json:"role"`
	Content []codexContent `json:"content"`
	// session_meta
	ID         string `json:"id"`
	Cwd        string `json:"cwd"`
	Originator string `json:"originator"`
	// event_msg item_completed
	Item *struct {
		Type    string         `json:"type"`
		Content []codexContent `json:"content"`
	} `json:"item"`
	// legacy event_msg user_message
	Message     string   `json:"message"`
	Images      []string `json:"images"`
	LocalImages []string `json:"local_images"`
	// response_item image_generation_call
	Result string `json:"result"`
}

type codexMeta struct {
	id, cwd, originator string
}

// codexParse controls how much of a rollout is read.
type codexParse struct {
	metaOnly    bool // stop after session_meta
	firstPrompt bool // stop after the first prompt
	images      bool // decode images
}

// parseRollout streams a rollout into its meta and turns. Malformed lines
// are skipped.
func (c *Codex) parseRollout(path string, p codexParse) (codexMeta, []turn, error) {
	var meta codexMeta
	// Current-format turns come from item_completed UserMessage items;
	// older rollouts only have event_msg user_message. Both are collected
	// and the current format wins when present.
	var lists [2][]turn
	cur := -1
	var pending []string
	current := func() *turn {
		if cur < 0 || len(lists[cur]) == 0 {
			return nil
		}
		return &lists[cur][len(lists[cur])-1]
	}
	addTurn := func(list int, ts time.Time, text string, dataURLs, localPaths []string) {
		t := turn{prompt: Message{Role: RoleUser, Text: strings.TrimSpace(text), Time: ts}}
		if p.images {
			for _, u := range dataURLs {
				if b, ok := decodeDataURL(u); ok {
					if img, ok := newImage(b, ""); ok {
						t.prompt.Images = appendImage(t.prompt.Images, img)
					}
				}
			}
			if len(t.prompt.Images) == 0 {
				for _, lp := range localPaths {
					if img, ok := readImageFile(lp); ok {
						t.prompt.Images = appendImage(t.prompt.Images, img)
					}
				}
			}
		}
		if t.prompt.Text == "" && len(dataURLs) == 0 && len(localPaths) == 0 {
			return
		}
		lists[list] = append(lists[list], t)
		cur = list
	}
	stop := false
	err := scanFile(path, func(line []byte) bool {
		var rec struct {
			Timestamp string       `json:"timestamp"`
			Type      string       `json:"type"`
			Payload   codexPayload `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return true
		}
		pl := rec.Payload
		ts := parseTime(rec.Timestamp)
		switch rec.Type {
		case "session_meta":
			if meta.id == "" {
				meta = codexMeta{id: pl.ID, cwd: pl.Cwd, originator: pl.Originator}
			}
			if p.metaOnly {
				return false
			}
		case "response_item":
			switch pl.Type {
			case "message":
				switch pl.Role {
				case "user":
					for _, ci := range pl.Content {
						if ci.Type == "input_image" {
							pending = append(pending, ci.imageURL())
						}
					}
				case "assistant":
					if t := current(); t != nil {
						var parts []string
						for _, ci := range pl.Content {
							if ci.Type == "output_text" && ci.Text != "" {
								parts = append(parts, ci.Text)
							}
						}
						if len(parts) > 0 {
							t.reply = Message{Role: RoleAssistant, Text: strings.Join(parts, "\n"), Time: ts}
						}
					}
				}
			case "image_generation_call":
				if t := current(); t != nil && p.images && pl.Result != "" {
					if b, ok := decodeBase64(pl.Result); ok {
						if img, ok := newImage(b, pl.ID); ok {
							t.replyImages = appendImage(t.replyImages, img)
						}
					}
				}
			}
		case "event_msg":
			switch {
			case pl.Type == "item_completed" && pl.Item != nil && pl.Item.Type == "UserMessage":
				var texts, locals []string
				for _, ci := range pl.Item.Content {
					switch ci.Type {
					case "text":
						texts = append(texts, ci.Text)
					case "local_image":
						locals = append(locals, ci.Path)
					}
				}
				addTurn(0, ts, strings.Join(texts, "\n"), pending, locals)
				pending = nil
				stop = p.firstPrompt && len(lists[0]) > 0
			case pl.Type == "user_message":
				imgs := pl.Images
				if len(imgs) == 0 {
					imgs = pending
				}
				addTurn(1, ts, pl.Message, imgs, pl.LocalImages)
				pending = nil
				stop = p.firstPrompt && len(lists[1]) > 0
			}
		}
		return !stop
	})
	if err != nil {
		return meta, nil, err
	}
	turns := lists[0]
	if len(turns) == 0 {
		turns = lists[1]
	}
	if p.images && meta.id != "" && idPattern.MatchString(meta.id) {
		c.attachGenerated(meta.id, turns)
	}
	return meta, turns, nil
}

// attachGenerated adds files from generated_images/<id>/ to the reply of
// the turn during which they were written.
func (c *Codex) attachGenerated(id string, turns []turn) {
	if len(turns) == 0 {
		return
	}
	dir := filepath.Join(c.Home, "generated_images", id)
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if !f.Type().IsRegular() {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime()
		idx := -1
		for i := range turns {
			if !mt.Before(turns[i].prompt.Time) {
				idx = i
			}
		}
		if idx < 0 {
			continue
		}
		if img, ok := readImageFile(filepath.Join(dir, f.Name())); ok {
			turns[idx].replyImages = appendImage(turns[idx].replyImages, img)
		}
	}
}

// load parses one candidate and applies the exclusions: the scratch dir
// always, codex_exec unless all.
func (c *Codex) load(e codexEntry, all bool, p codexParse) (thread, bool, error) {
	conv := Conversation{Source: SourceCodex, ID: e.id, Title: e.name, UpdatedAt: e.updated}
	if e.rollout == "" {
		// Listed in the index but the rollout was pruned from disk.
		return thread{conv: conv}, true, nil
	}
	meta, turns, err := c.parseRollout(e.rollout, p)
	if err != nil {
		if os.IsNotExist(err) {
			return thread{conv: conv}, true, nil
		}
		return thread{}, false, fmt.Errorf("codex: read rollout %s: %w", e.id, err)
	}
	if inDir(meta.cwd, c.ScratchDir) {
		return thread{}, false, nil
	}
	conv.Cwd = meta.cwd
	conv.Originator = meta.originator
	conv.Automated = meta.originator == codexExecOriginator
	if conv.Automated && !all {
		return thread{}, false, nil
	}
	if conv.Title == "" && len(turns) > 0 {
		conv.Title = titleFrom(turns[0].prompt.Text)
	}
	return thread{conv: conv, turns: turns}, true, nil
}

// List implements Reader.
func (c *Codex) List(ctx context.Context, count int, opts Options) ([]Conversation, error) {
	if err := checkListCount(count); err != nil {
		return nil, err
	}
	cands, err := c.candidates(ctx, opts.All)
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
		p := codexParse{metaOnly: true}
		if !e.inIndex {
			p = codexParse{firstPrompt: true}
		}
		th, ok, err := c.load(e, opts.All, p)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, th.conv)
		}
	}
	return out, nil
}

// Read implements Reader.
func (c *Codex) Read(ctx context.Context, q Query, opts Options) ([]Conversation, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if err := checkSource(SourceCodex, q); err != nil {
		return nil, err
	}
	p := codexParse{images: q.WantImages}
	if q.Mode == ModeConversation {
		cands, err := c.candidates(ctx, true)
		if err != nil {
			return nil, err
		}
		for _, e := range cands {
			if e.id != q.ConversationID {
				continue
			}
			th, ok, err := c.load(e, opts.All, p)
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
			return []Conversation{conversationMessages(th, q.WantImages)}, nil
		}
		return nil, fmt.Errorf("codex: %w: %s", ErrNotFound, q.ConversationID)
	}
	cands, err := c.candidates(ctx, opts.All)
	if err != nil {
		return nil, err
	}
	return pick(q, c.Window, c.now(), len(cands),
		func(i int) time.Time { return cands[i].updated },
		func(i int) (thread, bool, error) {
			if err := ctx.Err(); err != nil {
				return thread{}, false, err
			}
			return c.load(cands[i], opts.All, p)
		})
}
