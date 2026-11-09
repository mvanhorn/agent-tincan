package notes

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"
)

// fakeStateEnv names the fake helper's state dir. When it is set, the test
// binary runs as the fake agent-notes helper instead of the tests, so the
// service drives a real executable without the Agent Notes app.
const fakeStateEnv = "NOTES_FAKE_HELPER_STATE"

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeStateEnv); dir != "" {
		os.Exit(fakeHelperMain(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeNote is one note in the fake library: a JSON file per note in the
// library root.
type fakeNote struct {
	ID     string            `json:"id"`
	Title  string            `json:"title"`
	Body   string            `json:"body"`
	Tags   []string          `json:"tags"`
	State  string            `json:"state"`
	Pinned bool              `json:"pinned"`
	Props  map[string]string `json:"props"`
}

// fakeCall is one recorded helper invocation.
type fakeCall struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

// fakeHelperMain emulates the agent-notes helper contract: one JSON line on
// stdout, exit 0 on ok and 1 on failure. Controls, in the state dir:
// fail-create holds an error code every create returns, and
// fail-word-search one every single-word search returns; the library root
// missing gives unauthorized_library; a session-context.json in the
// Application Support root gives turn_context_invalid; old-helper makes
// create behave like a build without idempotency keys.
func fakeHelperMain(state string, args []string) int {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "AGENT_NOTES_") {
			env[k] = v
		}
	}
	if b, err := json.Marshal(fakeCall{Args: args, Env: env}); err == nil {
		if f, err := os.OpenFile(filepath.Join(state, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	}
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	opts := map[string]string{}
	flags := map[string]bool{}
	for i := 1; i < len(args); i++ {
		name, ok := strings.CutPrefix(args[i], "--")
		if !ok {
			continue
		}
		if name == "background" {
			flags[name] = true
			continue
		}
		if i+1 < len(args) {
			opts[name] = args[i+1]
			i++
		}
	}
	fail := func(code, msg string) int {
		out, _ := json.Marshal(map[string]any{"ok": false, "command": command, "error": map[string]any{"code": code, "message": msg}})
		fmt.Println(string(out))
		return 1
	}
	succeed := func(v map[string]any) int {
		v["ok"], v["command"] = true, command
		out, _ := json.Marshal(v)
		fmt.Println(string(out))
		return 0
	}
	lib := env["AGENT_NOTES_LIBRARY_ROOT"]
	if lib == "" {
		return fail("missing_authorization", "Run this bundled helper from an Agent Notes session.")
	}
	if st, err := os.Stat(lib); err != nil || !st.IsDir() {
		return fail("unauthorized_library", "The helper is authorized for a different library root.")
	}
	if sup := env["AGENT_NOTES_APPLICATION_SUPPORT_ROOT"]; sup != "" {
		if _, err := os.Stat(filepath.Join(sup, "session-context.json")); err == nil {
			return fail("turn_context_invalid", "The Agent Notes turn context is invalid.")
		}
	}
	notes := loadFakeNotes(lib)
	summary := func(n fakeNote) map[string]any {
		return map[string]any{"id": n.ID, "title": n.Title, "state": n.State, "pinned": n.Pinned, "tags": n.Tags, "hash": "h-" + n.ID}
	}
	switch command {
	case "create":
		if b, err := os.ReadFile(filepath.Join(state, "fail-create")); err == nil {
			return fail(strings.TrimSpace(string(b)), "injected failure")
		}
		if _, err := os.Stat(filepath.Join(state, "slow-create")); err == nil {
			busy := filepath.Join(state, "busy")
			if f, err := os.OpenFile(busy, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err != nil {
				_ = os.WriteFile(filepath.Join(state, "overlap"), nil, 0o600)
			} else {
				_ = f.Close()
				time.Sleep(150 * time.Millisecond)
				_ = os.Remove(busy)
			}
		}
		title := opts["title"]
		if title == "" {
			return fail("missing_option", "Missing required --title option.")
		}
		for _, s := range append([]string{title}, strings.Split(opts["tags"], ",")...) {
			if strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }) >= 0 {
				return fail("invalid_metadata", "Metadata contains an unsupported control character.")
			}
		}
		props := map[string]string{}
		if p, ok := opts["properties"]; ok {
			if err := json.Unmarshal([]byte(p), &props); err != nil {
				return fail("invalid_metadata", "bad properties")
			}
		}
		// An Agent Notes build older than the idempotency feature ignores
		// the key and its create response has no "existed" field.
		_, statErr := os.Stat(filepath.Join(state, "old-helper"))
		oldHelper := statErr == nil
		key := opts["idempotency-key"]
		if oldHelper {
			key = ""
		}
		if key != "" {
			props["remote.request-id"] = key
			for _, n := range notes {
				if n.Props["remote.request-id"] == key {
					return succeed(map[string]any{"note": map[string]any{"summary": summary(n), "body": n.Body}, "existed": true})
				}
			}
		}
		var tags []string
		if opts["tags"] != "" {
			tags = strings.Split(opts["tags"], ",")
		}
		n := fakeNote{ID: fakeUUID(), Title: title, Body: opts["body"], Tags: tags, State: "active", Props: props}
		saveFakeNote(lib, n)
		resp := map[string]any{"note": map[string]any{"summary": summary(n), "body": n.Body}}
		if !oldHelper {
			resp["existed"] = false
		}
		return succeed(resp)
	case "search":
		q := strings.ToLower(opts["query"])
		if q == "" {
			return fail("missing_option", "Missing required --query option.")
		}
		if b, err := os.ReadFile(filepath.Join(state, "fail-word-search")); err == nil && !strings.Contains(q, " ") {
			return fail(strings.TrimSpace(string(b)), "injected failure")
		}
		if b, err := os.ReadFile(filepath.Join(state, "slow-search")); err == nil {
			if d, err := time.ParseDuration(strings.TrimSpace(string(b))); err == nil {
				time.Sleep(d)
			}
		}
		// As the app's helper: the whole query, or with two or more words
		// every word, each a lowercase substring of the title, body or a tag.
		in := func(n fakeNote, w string) bool {
			return strings.Contains(strings.ToLower(n.Title), w) || strings.Contains(strings.ToLower(n.Body), w) ||
				slices.ContainsFunc(n.Tags, func(t string) bool { return strings.Contains(strings.ToLower(t), w) })
		}
		words := strings.Fields(q)
		out := []map[string]any{}
		for _, n := range notes {
			all := len(words) > 1
			for _, w := range words {
				all = all && in(n, w)
			}
			if in(n, q) || all {
				s := summary(n)
				s["snippet"] = firstRunes(n.Body, 160)
				out = append(out, s)
			}
		}
		return succeed(map[string]any{"notes": out})
	case "read":
		id := opts["id"]
		if !uuidPattern.MatchString(id) {
			return fail("invalid_identifier", "The note ID must be a UUID.")
		}
		for _, n := range notes {
			if n.ID == id {
				return succeed(map[string]any{"note": map[string]any{"summary": summary(n), "body": n.Body}})
			}
		}
		return fail("operation_failed", "The requested note no longer exists.")
	}
	return fail("unknown_command", "Supported commands: create, search, read.")
}

var uuidPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func fakeUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func loadFakeNotes(lib string) []fakeNote {
	paths, _ := filepath.Glob(filepath.Join(lib, "*.json"))
	sort.Strings(paths)
	var out []fakeNote
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var n fakeNote
		if json.Unmarshal(b, &n) == nil {
			out = append(out, n)
		}
	}
	return out
}

func saveFakeNote(lib string, n fakeNote) {
	b, _ := json.Marshal(n)
	_ = os.WriteFile(filepath.Join(lib, n.ID+".json"), b, 0o600)
}

// fakeHelper is a test's handle on one fake helper install.
type fakeHelper struct {
	path  string // the helper executable (a wrapper script)
	state string
	lib   string
}

// newFakeHelper installs a wrapper at a path with spaces, like the real
// one inside the app bundle, that runs this test binary as the helper.
func newFakeHelper(t *testing.T, lib string) *fakeHelper {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	helpers := filepath.Join(root, "Agent Notes.app", "Contents", "Helpers")
	for _, d := range []string{state, helpers} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(helpers, "agent-notes")
	script := fmt.Sprintf("#!/bin/sh\n%s=%s exec %s \"$@\"\n", fakeStateEnv, shellQuote(state), shellQuote(exe))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeHelper{path: path, state: state, lib: lib}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// slowSearches makes every search take d (0 stops).
func (h *fakeHelper) slowSearches(t *testing.T, d time.Duration) {
	t.Helper()
	p := filepath.Join(h.state, "slow-search")
	if d == 0 {
		_ = os.Remove(p)
		return
	}
	if err := os.WriteFile(p, []byte(d.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// failWordSearches makes every single-word search return code.
func (h *fakeHelper) failWordSearches(t *testing.T, code string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.state, "fail-word-search"), []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
}

// failCreates makes every create return code ("" stops failing).
func (h *fakeHelper) failCreates(t *testing.T, code string) {
	t.Helper()
	p := filepath.Join(h.state, "fail-create")
	if code == "" {
		_ = os.Remove(p)
		return
	}
	if err := os.WriteFile(p, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
}

// calls returns every recorded invocation so far.
func (h *fakeHelper) calls(t *testing.T) []fakeCall {
	t.Helper()
	f, err := os.Open(filepath.Join(h.state, "calls.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []fakeCall
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var c fakeCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// callsOf returns the recorded invocations of command.
func (h *fakeHelper) callsOf(t *testing.T, command string) []fakeCall {
	var out []fakeCall
	for _, c := range h.calls(t) {
		if len(c.Args) > 0 && c.Args[0] == command {
			out = append(out, c)
		}
	}
	return out
}

// notes returns the notes in the library.
func (h *fakeHelper) notes() []fakeNote { return loadFakeNotes(h.lib) }

// put adds a note directly to the library.
func (h *fakeHelper) put(t *testing.T, n fakeNote) fakeNote {
	t.Helper()
	if n.ID == "" {
		n.ID = fakeUUID()
	}
	if n.State == "" {
		n.State = "active"
	}
	saveFakeNote(h.lib, n)
	return n
}

// setState changes a note's state, as the owner archiving or trashing it.
func (h *fakeHelper) setState(t *testing.T, id, state string) {
	t.Helper()
	for _, n := range h.notes() {
		if n.ID == id {
			n.State = state
			saveFakeNote(h.lib, n)
			return
		}
	}
	t.Fatalf("no note %s", id)
}

// setTitle renames note id in the fake library, as the owner would.
func (h *fakeHelper) setTitle(t *testing.T, id, title string) {
	t.Helper()
	for _, n := range h.notes() {
		if n.ID == id {
			n.Title = title
			saveFakeNote(h.lib, n)
			return
		}
	}
	t.Fatalf("no note %s", id)
}

// argValue returns the value after --name in args.
func argValue(args []string, name string) (string, bool) {
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--"+name {
			return args[i+1], true
		}
	}
	return "", false
}
