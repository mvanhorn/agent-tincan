package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GeminiCLI reads local Gemini CLI sessions: <home>/tmp/<project>/chats/
// holds one session-<time>-<id>.jsonl per session (older versions wrote
// session-*.json, one JSON object), and <home>/tmp/<project>/.project_root
// names the project's directory. A .jsonl file is a log: a metadata
// record (sessionId, startTime, lastUpdated, summary, kind), message
// records (id, timestamp, type user or gemini or info, content), and
// $set, $patch and $rewindTo records that update, replace or drop what
// came before. Subagent sessions (chats/<parent id>/, kind subagent) are
// not the owner's and are never read. Antigravity's store is not read.
//
// Wake runs are sessions whose ids a wake recorded in a
// <name>.wake-sessions list in WakeDir, or whose project is a wake's
// workdir or home; sessions in ScratchDir are never reported.
type GeminiCLI struct {
	// Home is the Gemini CLI dir: $GEMINI_CLI_HOME/.gemini or ~/.gemini.
	Home       string
	WakeDir    string
	ScratchDir string
	Window     Window
	Now        func() time.Time
}

// NewGeminiCLI returns a reader for $GEMINI_CLI_HOME/.gemini or ~/.gemini.
func NewGeminiCLI() *GeminiCLI {
	base := os.Getenv("GEMINI_CLI_HOME")
	if base == "" {
		if h, err := os.UserHomeDir(); err == nil {
			base = h
		}
	}
	home := ""
	if base != "" {
		home = filepath.Join(base, ".gemini")
	}
	return &GeminiCLI{Home: home, WakeDir: configPath("", ""), ScratchDir: DefaultScratchDir()}
}

// Source implements Reader.
func (g *GeminiCLI) Source() Source { return SourceGeminiCLI }

func (g *GeminiCLI) store(opts Options, id string) cliStore {
	sel := geminiSelect{id: id}
	if id == "" {
		w := effectiveWindow(g.Window, opts)
		sel.cutoff = orNow(g.Now).Add(-w.MaxAge)
	}
	return cliStore{source: SourceGeminiCLI, wakeDir: g.WakeDir, scratchDir: g.ScratchDir, window: g.Window, now: g.Now, sessions: fromCandidates(func(ctx context.Context) ([]cliSession, error) {
		return g.candidates(ctx, sel)
	})}
}

// geminiSelect says which session files a lookup needs replayed: with id
// set, only files whose name carries that id's first eight characters;
// otherwise only files modified at or after cutoff. A file's mtime is an
// upper bound on its last activity, so a file modified before cutoff
// holds no session inside the window.
type geminiSelect struct {
	id     string
	cutoff time.Time
}

// geminiFileID is the short session id in a session-<start>-<id8> file
// name, or "" when the name does not carry one.
func geminiFileID(name string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".jsonl"), ".json")
	if i := strings.LastIndexByte(base, '-'); i >= 0 && len(base)-i-1 == 8 {
		return base[i+1:]
	}
	return ""
}

// candidates replays the main session files sel needs for their metadata
// and time, newest first. Files left out because they are older than the
// window are stood in for by one session, updated at the newest such
// file's mtime, so a scan still stops at the window's edge.
func (g *GeminiCLI) candidates(ctx context.Context, sel geminiSelect) ([]cliSession, error) {
	root := filepath.Join(g.Home, "tmp")
	projects, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, noHistory("Gemini CLI", root)
	}
	if err != nil {
		return nil, fmt.Errorf("gemini-cli: read sessions: %w", err)
	}
	byID := map[string]cliSession{}
	var stale *cliSession
	found := false
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := filepath.Join(root, p.Name())
		cwd := ""
		if raw, err := ReadCapped(filepath.Join(dir, ".project_root"), 64<<10); err == nil {
			cwd = strings.TrimSpace(string(raw))
		}
		files, _ := os.ReadDir(filepath.Join(dir, "chats"))
		for _, f := range files {
			name := f.Name()
			isLog := strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".json")
			if !f.Type().IsRegular() || !strings.HasPrefix(name, "session-") || !isLog {
				continue
			}
			found = true
			path := filepath.Join(dir, "chats", name)
			var mtime time.Time
			if st, err := f.Info(); err == nil {
				mtime = st.ModTime()
			}
			load := func() ([]turn, string, error) {
				log, err := readGeminiLog(path)
				if err != nil {
					return nil, "", err
				}
				return log.turns(), "", nil
			}
			if sel.id != "" {
				if short := geminiFileID(name); short != "" && !strings.HasPrefix(sel.id, short) {
					continue
				}
			} else if !mtime.IsZero() && mtime.Before(sel.cutoff) {
				if stale == nil || mtime.After(stale.updated) {
					stale = &cliSession{cwd: cwd, updated: mtime, load: load}
				}
				continue
			}
			log, err := readGeminiLog(path)
			if err != nil {
				return nil, fmt.Errorf("gemini-cli: read session %s: %w", name, err)
			}
			if log.meta.Kind == "subagent" || !idPattern.MatchString(log.meta.SessionID) {
				continue
			}
			e := cliSession{id: log.meta.SessionID, title: log.meta.Summary, cwd: cwd, updated: log.updated(), load: load}
			if e.updated.IsZero() {
				e.updated = mtime
			}
			// A session resumed from a legacy .json is rewritten as .jsonl;
			// keep the newer copy.
			if old, ok := byID[e.id]; !ok || e.updated.After(old.updated) {
				byID[e.id] = e
			}
		}
	}
	if !found {
		return nil, noHistory("Gemini CLI", root)
	}
	out := make([]cliSession, 0, len(byID)+1)
	for _, e := range byID {
		out = append(out, e)
	}
	if stale != nil {
		out = append(out, *stale)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].updated.Equal(out[j].updated) {
			return out[i].updated.After(out[j].updated)
		}
		return out[i].id < out[j].id
	})
	return out, nil
}

// geminiMeta is the session metadata a log's records set.
type geminiMeta struct {
	SessionID   string `json:"sessionId"`
	StartTime   string `json:"startTime"`
	LastUpdated string `json:"lastUpdated"`
	Summary     string `json:"summary"`
	Kind        string `json:"kind"`
}

// merge applies the fields a record sets.
func (m *geminiMeta) merge(raw json.RawMessage) {
	var n geminiMeta
	if json.Unmarshal(raw, &n) != nil {
		return
	}
	for _, f := range []struct {
		dst *string
		v   string
	}{
		{&m.SessionID, n.SessionID}, {&m.StartTime, n.StartTime}, {&m.LastUpdated, n.LastUpdated}, {&m.Summary, n.Summary}, {&m.Kind, n.Kind},
	} {
		if f.v != "" {
			*f.dst = f.v
		}
	}
}

// geminiMessage is one message record.
type geminiMessage struct {
	ID             string          `json:"id"`
	Timestamp      string          `json:"timestamp"`
	Type           string          `json:"type"`
	Content        json.RawMessage `json:"content"`
	DisplayContent json.RawMessage `json:"displayContent"`
}

// geminiLog is a session file replayed.
type geminiLog struct {
	meta  geminiMeta
	order []string
	msgs  map[string]*geminiMessage
}

func (l *geminiLog) add(m geminiMessage) {
	if _, ok := l.msgs[m.ID]; !ok {
		l.order = append(l.order, m.ID)
	}
	l.msgs[m.ID] = &m
}

func (l *geminiLog) remove(id string) {
	if _, ok := l.msgs[id]; !ok {
		return
	}
	delete(l.msgs, id)
	for i, o := range l.order {
		if o == id {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
}

func (l *geminiLog) reset() {
	l.order, l.msgs = nil, map[string]*geminiMessage{}
}

// addAll adds a messages array (a legacy file's, or a $set's).
func (l *geminiLog) addAll(raw json.RawMessage) {
	var msgs []geminiMessage
	_ = json.Unmarshal(raw, &msgs)
	for _, m := range msgs {
		if m.ID != "" && m.Type != "" {
			l.add(m)
		}
	}
}

// apply replays one .jsonl record.
func (l *geminiLog) apply(line []byte) {
	var rec map[string]json.RawMessage
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	switch {
	case rec["$rewindTo"] != nil:
		var id string
		_ = json.Unmarshal(rec["$rewindTo"], &id)
		for i, o := range l.order {
			if o == id {
				for _, gone := range l.order[i:] {
					delete(l.msgs, gone)
				}
				l.order = l.order[:i]
				return
			}
		}
		l.reset()
	case rec["$patch"] != nil:
		var p struct {
			ID        string          `json:"id"`
			Content   json.RawMessage `json:"content"`
			RemoveIDs []string        `json:"removeIds"`
			Updates   []struct {
				ID      string          `json:"id"`
				Content json.RawMessage `json:"content"`
			} `json:"updates"`
		}
		if json.Unmarshal(rec["$patch"], &p) != nil {
			return
		}
		patch := func(id string, content json.RawMessage) {
			if m := l.msgs[id]; m != nil && content != nil {
				m.Content = content
			}
		}
		patch(p.ID, p.Content)
		for _, u := range p.Updates {
			patch(u.ID, u.Content)
		}
		for _, id := range p.RemoveIDs {
			l.remove(id)
		}
	case rec["$set"] != nil:
		var set map[string]json.RawMessage
		if json.Unmarshal(rec["$set"], &set) != nil {
			return
		}
		if set["messages"] != nil {
			l.reset()
			l.addAll(set["messages"])
		}
		l.meta.merge(rec["$set"])
	case rec["id"] != nil && rec["type"] != nil:
		var m geminiMessage
		if json.Unmarshal(line, &m) == nil && m.ID != "" {
			l.add(m)
		}
	case rec["sessionId"] != nil:
		l.meta.merge(line)
		if rec["messages"] != nil {
			l.addAll(rec["messages"])
		}
	}
}

// readGeminiLog reads a .jsonl log, or a legacy .json record.
func readGeminiLog(path string) (*geminiLog, error) {
	l := &geminiLog{msgs: map[string]*geminiMessage{}}
	if strings.HasSuffix(path, ".json") {
		raw, err := ReadCapped(path, maxLine)
		if err != nil {
			return nil, err
		}
		l.apply(raw)
		return l, nil
	}
	err := scanFile(path, func(line []byte) bool {
		l.apply(line)
		return true
	})
	return l, err
}

// updated is the later of lastUpdated and the newest message's time.
func (l *geminiLog) updated() time.Time {
	t := parseTime(l.meta.LastUpdated)
	for _, id := range l.order {
		if mt := parseTime(l.msgs[id].Timestamp); mt.After(t) {
			t = mt
		}
	}
	if t.IsZero() {
		t = parseTime(l.meta.StartTime)
	}
	return t
}

// turns pairs each user message with the last gemini text after it. The
// prompt is what the owner typed (displayContent when the CLI expanded
// it, such as an @file reference).
func (l *geminiLog) turns() []turn {
	var turns []turn
	for _, id := range l.order {
		m := l.msgs[id]
		switch m.Type {
		case "user":
			text := geminiText(m.DisplayContent)
			if text == "" {
				text = geminiText(m.Content)
			}
			if text == "" {
				continue
			}
			turns = append(turns, turn{prompt: Message{Role: RoleUser, Text: text, Time: parseTime(m.Timestamp)}})
		case "gemini":
			if text := geminiText(m.Content); text != "" && len(turns) > 0 {
				turns[len(turns)-1].reply = Message{Role: RoleAssistant, Text: text}
			}
		}
	}
	return turns
}

// geminiText flattens a PartListUnion: a string, a part, or a list of
// them; only text parts count.
func geminiText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	type part struct {
		Text string `json:"text"`
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		parts = []json.RawMessage{raw}
	}
	var texts []string
	for _, p := range parts {
		var ps string
		var pt part
		switch {
		case json.Unmarshal(p, &ps) == nil:
			texts = append(texts, ps)
		case json.Unmarshal(p, &pt) == nil && pt.Text != "":
			texts = append(texts, pt.Text)
		}
	}
	return strings.TrimSpace(strings.Join(texts, ""))
}

// List implements Reader.
func (g *GeminiCLI) List(ctx context.Context, count int, opts Options) (Page, error) {
	return g.store(opts, "").List(ctx, count, opts)
}

// Read implements Reader.
func (g *GeminiCLI) Read(ctx context.Context, q Query, opts Options) (Page, error) {
	id := ""
	if q.Mode == ModeConversation {
		id = q.ConversationID
	}
	return g.store(opts, id).Read(ctx, q, opts)
}
