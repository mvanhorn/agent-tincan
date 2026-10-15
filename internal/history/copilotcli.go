package history

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CopilotCLI reads local GitHub Copilot CLI sessions:
// <home>/session-state/<session id>/ holds workspace.yaml (id, cwd, name,
// created_at, updated_at) and events.jsonl, one event per line. The
// owner's prompts are the user.message events' content (what was typed,
// not the transformedContent Copilot sends the model); the reply is the
// last non-empty assistant.message before the next prompt. Other files
// there (checkpoints, rewind snapshots, session-store.db) are ignored.
//
// Wake runs are sessions whose ids a wake recorded in a
// <name>.wake-sessions list in WakeDir, or whose cwd is a wake's workdir
// or home; sessions in ScratchDir are never reported.
type CopilotCLI struct {
	// Home is $COPILOT_HOME or ~/.copilot.
	Home       string
	WakeDir    string
	ScratchDir string
	Window     Window
	Now        func() time.Time
}

// NewCopilotCLI returns a reader for $COPILOT_HOME or ~/.copilot.
func NewCopilotCLI() *CopilotCLI {
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".copilot")
		}
	}
	return &CopilotCLI{Home: home, WakeDir: configPath("", ""), ScratchDir: DefaultScratchDir()}
}

// Source implements Reader.
func (c *CopilotCLI) Source() Source { return SourceCopilotCLI }

func (c *CopilotCLI) store() cliStore {
	return cliStore{source: SourceCopilotCLI, wakeDir: c.WakeDir, scratchDir: c.ScratchDir, window: c.Window, now: c.Now, sessions: fromCandidates(c.candidates)}
}

// candidates returns the sessions newest first.
func (c *CopilotCLI) candidates(ctx context.Context) ([]cliSession, error) {
	root := filepath.Join(c.Home, "session-state")
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, noHistory("Copilot CLI", root)
	}
	if err != nil {
		return nil, fmt.Errorf("copilot-cli: read sessions: %w", err)
	}
	var out []cliSession
	for _, s := range entries {
		if !s.IsDir() || !idPattern.MatchString(s.Name()) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := filepath.Join(root, s.Name())
		events := filepath.Join(dir, "events.jsonl")
		st, err := os.Stat(events)
		if err != nil {
			continue
		}
		ws := readSimpleYAML(filepath.Join(dir, "workspace.yaml"))
		e := cliSession{id: s.Name(), title: ws["name"], cwd: ws["cwd"], updated: st.ModTime()}
		for _, k := range []string{"updated_at", "created_at"} {
			if t := parseTime(ws[k]); !t.IsZero() {
				e.updated = t
				break
			}
		}
		updated := e.updated
		e.load = func() ([]turn, string, error) { return copilotCLITurns(events, updated) }
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, noHistory("Copilot CLI", root)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].updated.After(out[j].updated) })
	return out, nil
}

var (
	copilotUserMessage      = []byte("user.message")
	copilotAssistantMessage = []byte("assistant.message")
)

// copilotCLITurns reads one events.jsonl.
func copilotCLITurns(path string, updated time.Time) ([]turn, string, error) {
	var turns []turn
	err := scanFile(path, func(line []byte) bool {
		// Most events are tool calls and other bookkeeping; skip them
		// without decoding.
		if !bytes.Contains(line, copilotUserMessage) && !bytes.Contains(line, copilotAssistantMessage) {
			return true
		}
		var ev struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Data      struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return true
		}
		text := strings.TrimSpace(ev.Data.Content)
		switch ev.Type {
		case "user.message":
			if text == "" {
				return true
			}
			at := parseTime(ev.Timestamp)
			if at.IsZero() {
				at = updated
			}
			turns = append(turns, turn{prompt: Message{Role: RoleUser, Text: text, Time: at}})
		case "assistant.message":
			if text != "" && len(turns) > 0 {
				turns[len(turns)-1].reply = Message{Role: RoleAssistant, Text: text}
			}
		}
		return true
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}
	return turns, "", nil
}

// readSimpleYAML reads the top-level "key: scalar" lines of a small YAML
// file; anything else (nesting, block scalars) is skipped. A missing file
// is an empty map.
func readSimpleYAML(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = strings.ReplaceAll(v[1:len(v)-1], "''", "'")
		case len(v) >= 2 && v[0] == '"':
			if u, err := strconv.Unquote(v); err == nil {
				v = u
			}
		case v == "|" || v == ">" || strings.HasPrefix(v, "|-") || strings.HasPrefix(v, ">-"):
			continue
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// List implements Reader.
func (c *CopilotCLI) List(ctx context.Context, count int, opts Options) (Page, error) {
	return c.store().List(ctx, count, opts)
}

// Read implements Reader.
func (c *CopilotCLI) Read(ctx context.Context, q Query, opts Options) (Page, error) {
	return c.store().Read(ctx, q, opts)
}
