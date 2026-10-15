package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	openCode1 = "ses_0c0d00000000000000000001" // owner, two prompts
	openCode2 = "ses_0c0d00000000000000000002" // owner
	openCode3 = "ses_0c0d00000000000000000003" // wake run, recorded id
	openCode4 = "ses_0c0d00000000000000000004" // history's own scratch run
	openCode5 = "ses_0c0d00000000000000000005" // forty days old
	openCode6 = "ses_0c0d00000000000000000006" // a subagent task under 1
	openCode7 = "ses_0c0d00000000000000000007" // wake run in a recorded workdir, id not recorded
	openCode8 = "ses_0c0d00000000000000000008" // wake run under a <name>.wake home, id not recorded
)

// openCodeFixture builds an opencode.db in the layout OpenCode 1.x
// writes: session rows, message rows whose data JSON carries the role,
// and part rows whose data JSON carries the text.
func openCodeFixture(t *testing.T) (*OpenCode, string) {
	t.Helper()
	dataDir, wakeDir, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	stmts := []string{
		"CREATE TABLE `project` (`id` text PRIMARY KEY, `worktree` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `sandboxes` text NOT NULL)",
		"CREATE TABLE `session` (`id` text PRIMARY KEY, `project_id` text NOT NULL, `parent_id` text, `slug` text NOT NULL, `directory` text NOT NULL, `title` text NOT NULL, `version` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `time_archived` integer)",
		"CREATE TABLE `message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL)",
		"CREATE TABLE `part` (`id` text PRIMARY KEY, `message_id` text NOT NULL, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL)",
	}
	n := 0
	session := func(id, parent, dir, title string, updated time.Time, turns ...fixtureTurn) {
		p := "NULL"
		if parent != "" {
			p = sqlQuote(parent)
		}
		stmts = append(stmts, fmt.Sprintf("INSERT INTO session VALUES (%s, 'prj', %s, 'slug', %s, %s, '1.19.0', %d, %d, NULL)",
			sqlQuote(id), p, sqlQuote(dir), sqlQuote(title), updated.Add(-time.Hour).UnixMilli(), updated.UnixMilli()))
		msg := func(role string, at time.Time, parts ...map[string]any) {
			n++
			mid := fmt.Sprintf("msg_%04d", n)
			data, _ := json.Marshal(map[string]any{"role": role, "time": map[string]any{"created": at.UnixMilli()}})
			stmts = append(stmts, fmt.Sprintf("INSERT INTO message VALUES (%s, %s, %d, %d, %s)", sqlQuote(mid), sqlQuote(id), at.UnixMilli(), at.UnixMilli(), sqlQuote(string(data))))
			for i, pt := range parts {
				raw, _ := json.Marshal(pt)
				stmts = append(stmts, fmt.Sprintf("INSERT INTO part VALUES (%s, %s, %s, %d, %d, %s)", sqlQuote(fmt.Sprintf("prt_%04d_%d", n, i)), sqlQuote(mid), sqlQuote(id), at.UnixMilli(), at.UnixMilli(), sqlQuote(string(raw))))
			}
		}
		for _, tr := range turns {
			msg("user", tr.at,
				map[string]any{"type": "text", "text": tr.prompt},
				map[string]any{"type": "text", "text": "Called the Read tool with package main", "synthetic": true})
			msg("assistant", tr.at.Add(time.Second),
				map[string]any{"type": "reasoning", "text": "thinking"},
				map[string]any{"type": "text", "text": "Let me look."},
				map[string]any{"type": "tool", "tool": "read", "state": map[string]any{"output": "package main"}})
			msg("assistant", tr.at.Add(2*time.Second), map[string]any{"type": "text", "text": tr.reply})
		}
	}
	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	session(openCode1, "", "/Users/matt/code/x", "Opencode config", day(21, 10),
		fixtureTurn{"where does opencode keep its config", "In opencode.json.", day(21, 9)},
		fixtureTurn{"and the agents", "Under .opencode/agent.", day(21, 9).Add(45 * time.Minute)})
	session(openCode2, "", "/Users/matt/code/y", "New session - 2026-09-20T09:00:00.000Z", day(20, 10),
		fixtureTurn{"draft the release notes", "Drafted them.", day(20, 9)})
	// A large tool part on an owner message: the read must skip it.
	n++
	big, _ := json.Marshal(map[string]any{"type": "tool", "tool": "read", "state": map[string]any{"output": strings.Repeat("x", 1<<20)}})
	stmts = append(stmts,
		fmt.Sprintf("INSERT INTO message VALUES ('msg_big', %s, %d, %d, '{\"role\":\"assistant\"}')", sqlQuote(openCode2), day(20, 9).Add(3*time.Second).UnixMilli(), day(20, 9).Add(3*time.Second).UnixMilli()),
		fmt.Sprintf("INSERT INTO part VALUES ('prt_big', 'msg_big', %s, 0, 0, %s)", sqlQuote(openCode2), sqlQuote(string(big))))
	session(openCode3, "", "/Users/matt/code/x", "wake", day(21, 11), fixtureTurn{"check your tincan inbox", "Done.", day(21, 11)})
	session(openCode4, "", fixtureScratch+"/run-1", "", day(21, 12), fixtureTurn{"classify this question", "{}", day(21, 12)})
	session(openCode5, "", "/Users/matt/code/x", "old", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), fixtureTurn{"old", "old", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)})
	session(openCode6, openCode1, "/Users/matt/code/x", "subtask", day(21, 13), fixtureTurn{"explore the repo", "Explored.", day(21, 13)})
	session(openCode7, "", "/Users/matt/code/wakework/sub", "", day(21, 14), fixtureTurn{"check your tincan inbox", "Done.", day(21, 14)})
	wakeHome := filepath.Join(wakeDir, "opencode-helper.wake")
	mkdirs(t, wakeHome)
	session(openCode8, "", filepath.Join(wakeHome, "proj"), "", day(21, 15), fixtureTurn{"check your tincan inbox", "Done.", day(21, 15)})
	db := filepath.Join(dataDir, "opencode.db")
	fixtureDB(t, db, stmts...)
	if err := os.WriteFile(filepath.Join(wakeDir, "opencode.wake-sessions"), []byte(openCode3+"\nworkdir /Users/matt/code/wakework\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &OpenCode{DB: db, WakeDir: wakeDir, ScratchDir: fixtureScratch, TempDir: tmp, Now: func() time.Time { return fixtureNow }}, tmp
}

func TestOpenCodeListNewestFirstWithoutWakeScratchOrSubagentRuns(t *testing.T) {
	r, tmp := openCodeFixture(t)
	page, err := r.List(context.Background(), 20, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page.Conversations); got != openCode1+","+openCode2 || page.Limited != LimitAge {
		t.Fatalf("list = %s (limited %q), want 1,2 limited by age", got, page.Limited)
	}
	if c := page.Conversations[0]; c.Source != SourceOpenCode || c.Title != "Opencode config" || c.Cwd != "/Users/matt/code/x" || c.Automated {
		t.Fatalf("first = %+v", c)
	}
	page, err = r.List(context.Background(), 20, Options{All: true})
	if err != nil || ids(page.Conversations) != openCode8+","+openCode7+","+openCode6+","+openCode3+","+openCode1+","+openCode2 {
		t.Fatalf("list --all = %s %+v, %v", ids(page.Conversations), page.Conversations, err)
	}
	// 8 and 7 are wake runs by working directory alone (a <name>.wake home,
	// a recorded workdir line); 6 is a subagent; 3 has its id recorded.
	for i, c := range page.Conversations[:4] {
		if !c.Automated {
			t.Fatalf("list --all [%d] %s not automated", i, c.ID)
		}
	}
	emptyDir(t, tmp)
}

func TestOpenCodeLatestReadsTypedPromptAndFinalReply(t *testing.T) {
	r, tmp := openCodeFixture(t)
	got, err := convsOf(r.Read(context.Background(), Query{Source: SourceOpenCode, Mode: ModeLatest, Count: 2}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != openCode1 || got[1].ID != openCode1 {
		t.Fatalf("latest 2 = %s", ids(got))
	}
	m := got[0].Messages
	if len(m) != 2 || m[0].Text != "and the agents" || m[1].Text != "Under .opencode/agent." || !m[0].Time.Equal(time.Date(2026, 9, 21, 9, 45, 0, 0, time.UTC)) {
		t.Fatalf("newest = %+v", m)
	}
	for _, c := range got {
		for _, msg := range c.Messages {
			if strings.Contains(msg.Text, "package main") || strings.Contains(msg.Text, "Let me look") || strings.Contains(msg.Text, "thinking") {
				t.Fatalf("synthetic or tool text leaked into %q", msg.Text)
			}
		}
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceOpenCode, Mode: ModeConversation, ConversationID: openCode3}, Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wake run by id: err = %v, want not found", err)
	}
	emptyDir(t, tmp)
}

func TestOpenCodeNoHistory(t *testing.T) {
	db := filepath.Join(t.TempDir(), "opencode", "opencode.db")
	tmp := t.TempDir()
	r := &OpenCode{DB: db, TempDir: tmp}
	_, err := r.Read(context.Background(), Query{Source: SourceOpenCode, Mode: ModeLatest}, Options{})
	if want := "no OpenCode history found in " + db; !errors.Is(err, ErrNoHistory) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
	emptyDir(t, tmp)
}

// NewOpenCode reads $OPENCODE_DB when set (relative to the data dir), else
// opencode.db in $XDG_DATA_HOME/opencode or ~/.local/share/opencode.
func TestNewOpenCodeHonorsDataDirAndDBOverride(t *testing.T) {
	t.Setenv("OPENCODE_DB", "")
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg")
	if r := NewOpenCode(); r.DB != "/tmp/xdg/opencode/opencode.db" || r.Source() != SourceOpenCode {
		t.Fatalf("reader = %+v", r)
	}
	t.Setenv("OPENCODE_DB", "other.db")
	if r := NewOpenCode(); r.DB != "/tmp/xdg/opencode/other.db" {
		t.Fatalf("relative override = %q", r.DB)
	}
	t.Setenv("OPENCODE_DB", "/abs/oc.db")
	if r := NewOpenCode(); r.DB != "/abs/oc.db" {
		t.Fatalf("absolute override = %q", r.DB)
	}
	t.Setenv("OPENCODE_DB", "")
	t.Setenv("XDG_DATA_HOME", "")
	h, _ := os.UserHomeDir()
	if r := NewOpenCode(); r.DB != filepath.Join(h, ".local", "share", "opencode", "opencode.db") {
		t.Fatalf("default = %q", r.DB)
	}
}
