package history

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeCodexScript records its arguments (NUL-separated), working
// directory, directory listing, stdin and output schema, then writes
// $FAKE_CODEX_REPLY to the --output-last-message file.
const fakeCodexScript = `#!/bin/sh
log=$FAKE_CODEX_LOG
: > "$log/args"
for a in "$@"; do printf '%s\0' "$a" >> "$log/args"; done
pwd -P > "$log/pwd"
printf '%s' "${TINCAN_CONFIG:-}" > "$log/tincan_config"
ls -A > "$log/ls"
cat > "$log/stdin"
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out=$a; fi
  if [ "$prev" = "--output-schema" ]; then cp "$a" "$log/schema"; fi
  prev=$a
done
if [ -n "$FAKE_CODEX_EXIT" ]; then
  echo "fake codex failing" >&2
  exit "$FAKE_CODEX_EXIT"
fi
printf '%s' "$FAKE_CODEX_REPLY" > "$out"
`

// fakeCodex puts a fake codex binary first on PATH and returns its log dir
// and an extractor whose scratch dir is a fresh temp dir.
func fakeCodex(t *testing.T, reply string) (logDir string, x *CodexExtractor) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake codex is a shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fakeCodexScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	logDir = t.TempDir()
	t.Setenv("FAKE_CODEX_LOG", logDir)
	t.Setenv("FAKE_CODEX_REPLY", reply)
	t.Setenv("TINCAN_CONFIG", "/should/not/leak.json")
	scratch := filepath.Join(t.TempDir(), "history-scratch")
	return logDir, &CodexExtractor{Binary: "codex", ScratchDir: scratch}
}

func readLog(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// hasSeq reports whether want occurs in args as a contiguous run.
func hasSeq(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func TestCodexExtractorFlagsAndInput(t *testing.T) {
	logDir, x := fakeCodex(t, `{"source":"chatgpt","mode":"latest","terms":[],"conversation_id":"","count":1,"want_images":true}`)
	question := "what was the last thing Matt asked ChatGPT? send the image"
	q, err := x.Extract(context.Background(), question)
	if err != nil {
		t.Fatal(err)
	}
	if q.Source != SourceChatGPT || q.Mode != ModeLatest || !q.WantImages || q.Count != 1 {
		t.Fatalf("query = %+v", q)
	}

	args := strings.Split(strings.TrimSuffix(readLog(t, logDir, "args"), "\x00"), "\x00")
	if args[0] != "exec" {
		t.Fatalf("first arg %q, want exec", args[0])
	}
	for _, seq := range [][]string{
		{"--sandbox", "read-only"},
		{"-c", "approval_policy=never"},
		{"-c", "mcp_servers={}"},
		{"-c", "web_search=disabled"},
		{"--ignore-user-config"},
		{"--ephemeral"},
		{"--skip-git-repo-check"},
		{"--disable", "plugins"},
		{"--disable", "apps"},
		{"--disable", "shell_tool"},
		{"--disable", "browser_use"},
		{"--disable", "computer_use"},
		{"--disable", "image_generation"},
	} {
		if !hasSeq(args, seq...) {
			t.Errorf("args missing %v:\n%q", seq, args)
		}
	}
	for _, bad := range []string{"workspace-write", "danger-full-access", "--dangerously-bypass-approvals-and-sandbox", "--add-dir", "--search"} {
		if slices.Contains(args, bad) {
			t.Errorf("args contain %q", bad)
		}
	}

	// It runs from an empty scratch dir under the scratch path the readers
	// exclude, and that dir is gone afterwards.
	cd := argAfter(args, "--cd")
	scratch, _ := filepath.EvalSymlinks(filepath.Dir(x.ScratchDir))
	scratch = filepath.Join(scratch, filepath.Base(x.ScratchDir))
	pwd := strings.TrimSpace(readLog(t, logDir, "pwd"))
	if !inDir(pwd, scratch) || pwd == scratch {
		t.Fatalf("cwd %q is not a subdir of the scratch dir %q", pwd, scratch)
	}
	if !inDir(cd, x.ScratchDir) {
		t.Fatalf("--cd %q not under scratch %q", cd, x.ScratchDir)
	}
	if ls := readLog(t, logDir, "ls"); ls != "" {
		t.Fatalf("scratch cwd not empty: %q", ls)
	}
	if _, err := os.Stat(cd); !os.IsNotExist(err) {
		t.Fatalf("run dir %s not removed: %v", cd, err)
	}
	if left, _ := os.ReadDir(x.ScratchDir); len(left) != 0 {
		t.Fatalf("scratch dir left behind %v", left)
	}

	// Structured output: a schema file naming every query field.
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal([]byte(readLog(t, logDir, "schema")), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, f := range []string{"source", "mode", "terms", "conversation_id", "count", "want_images"} {
		if !slices.Contains(schema.Required, f) {
			t.Errorf("schema does not require %q", f)
		}
	}
	if argAfter(args, "--output-last-message") == "" {
		t.Fatal("no --output-last-message")
	}

	// Only the question reaches the model: it is stdin, and the prompt
	// argument is the fixed instructions.
	if got := readLog(t, logDir, "stdin"); got != question {
		t.Fatalf("stdin = %q, want only the question", got)
	}
	if env := readLog(t, logDir, "tincan_config"); env != "" {
		t.Fatalf("TINCAN_CONFIG leaked into the extractor run: %q", env)
	}
	if last := args[len(args)-1]; last != extractInstructions || strings.Contains(last, question) {
		t.Fatalf("prompt argument is not the fixed instructions: %q", last)
	}
}

func TestCodexExtractorRejectsBadOutput(t *testing.T) {
	cases := map[string]string{
		"not json":       `I think they asked about cats`,
		"unknown source": `{"source":"myspace","mode":"latest","terms":[],"conversation_id":"","count":1,"want_images":false}`,
		"unclear":        `{"source":"unknown","mode":"latest","terms":[],"conversation_id":"","count":1,"want_images":false}`,
		"count too big":  `{"source":"codex","mode":"latest","terms":[],"conversation_id":"","count":500,"want_images":false}`,
		"bad id":         `{"source":"codex","mode":"conversation","terms":[],"conversation_id":"../../etc/passwd","count":1,"want_images":false}`,
		"search no term": `{"source":"codex","mode":"search","terms":[],"conversation_id":"","count":1,"want_images":false}`,
		"extra field":    `{"source":"codex","mode":"latest","terms":[],"conversation_id":"","count":1,"want_images":false,"shell":"rm -rf"}`,
		"two objects":    `{"source":"codex","mode":"latest"} {"source":"chatgpt"}`,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			_, x := fakeCodex(t, reply)
			_, err := x.Extract(context.Background(), "what did Matt ask?")
			if !errors.Is(err, ErrUnclearQuestion) {
				t.Fatalf("err = %v, want ErrUnclearQuestion", err)
			}
		})
	}
}

func TestCodexExtractorFailureIsNotUnclear(t *testing.T) {
	_, x := fakeCodex(t, "")
	t.Setenv("FAKE_CODEX_EXIT", "3")
	_, err := x.Extract(context.Background(), "what did Matt ask Codex?")
	if err == nil || errors.Is(err, ErrUnclearQuestion) {
		t.Fatalf("err = %v, want an extractor failure", err)
	}
	if left, _ := os.ReadDir(x.ScratchDir); len(left) != 0 {
		t.Fatalf("scratch dir left behind after failure: %v", left)
	}
}

func TestCodexExtractorRefusesEmptyOrHugeQuestion(t *testing.T) {
	logDir, x := fakeCodex(t, `{}`)
	for _, q := range []string{"", "   ", strings.Repeat("a", MaxQuestionBytes+1)} {
		if _, err := x.Extract(context.Background(), q); !errors.Is(err, ErrUnclearQuestion) {
			t.Fatalf("question of %d bytes: err = %v", len(q), err)
		}
	}
	if _, err := os.Stat(filepath.Join(logDir, "args")); !os.IsNotExist(err) {
		t.Fatal("codex ran for an empty or oversized question")
	}
}

func TestParseExtractionTolerance(t *testing.T) {
	q, err := parseExtraction([]byte("```json\n{\"source\":\"claude-code\",\"mode\":\"search\",\"terms\":[\"relay\"],\"conversation_id\":\"\",\"count\":0,\"want_images\":false}\n```\n"))
	if err != nil {
		t.Fatal(err)
	}
	if q.Source != SourceClaudeCode || q.Mode != ModeSearch || len(q.Terms) != 1 || q.Terms[0] != "relay" {
		t.Fatalf("query = %+v", q)
	}
}
