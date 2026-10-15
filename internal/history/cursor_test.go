package history

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCursorCLISession adds a Cursor CLI session under home/chats: a
// meta.json and a store.db whose blobs table holds the messages and whose
// root blob lists them in order, the layout cursor-agent writes.
func writeCursorCLISession(t *testing.T, home, cwd, id, name string, updated time.Time, turns ...fixtureTurn) {
	t.Helper()
	sum := md5.Sum([]byte(cwd))
	dir := filepath.Join(home, "chats", hex.EncodeToString(sum[:]), id)
	mkdirs(t, dir)
	meta, _ := json.Marshal(map[string]any{"schemaVersion": 1, "createdAtMs": updated.Add(-time.Hour).UnixMilli(), "updatedAtMs": updated.UnixMilli(), "hasConversation": true, "cwd": cwd})
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	msgs = append(msgs, map[string]any{"role": "system", "content": "You are a coding agent."})
	msgs = append(msgs, map[string]any{"role": "user", "content": "<user_info>\nOS Version: darwin\n</user_info>"})
	for _, tr := range turns {
		stamp := tr.at.In(time.FixedZone("", -7*3600)).Format("Monday, Jan 2, 2006, 3:04 PM") + " (UTC-7)"
		msgs = append(msgs, map[string]any{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "<system_reminder>\nplan mode is off\n</system_reminder>"},
			{"type": "text", "text": "<timestamp>" + stamp + "</timestamp>\n<user_query>\n" + tr.prompt + "\n</user_query>"},
		}})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{{"type": "reasoning", "text": "thinking"}, {"type": "text", "text": "Let me look."}}})
		msgs = append(msgs, map[string]any{"role": "tool", "content": []map[string]any{{"type": "tool-result", "toolName": "read", "result": "package main"}}})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": tr.reply}}})
	}
	stmts := []string{"CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB)", "CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT)"}
	var root []byte
	for _, m := range msgs {
		raw, _ := json.Marshal(m)
		h := sha256.Sum256(raw)
		stmts = append(stmts, "INSERT OR IGNORE INTO blobs VALUES ("+sqlQuote(hex.EncodeToString(h[:]))+", CAST("+sqlQuote(string(raw))+" AS BLOB))")
		root = append(root, 0x0a, 0x20) // field 1, 32 bytes: a message blob id
		root = append(root, h[:]...)
	}
	root = append(root, 0xd0, 0x01, 0x01) // field 26 varint, which the reader skips
	rh := sha256.Sum256(root)
	stmts = append(stmts, "INSERT INTO blobs VALUES ("+sqlQuote(hex.EncodeToString(rh[:]))+", X'"+hex.EncodeToString(root)+"')")
	m, _ := json.Marshal(map[string]any{"agentId": id, "latestRootBlobId": hex.EncodeToString(rh[:]), "name": name, "mode": "default", "createdAt": updated.Add(-time.Hour).UnixMilli()})
	stmts = append(stmts, "INSERT INTO meta VALUES ('0', "+sqlQuote(hex.EncodeToString(m))+")")
	fixtureDB(t, filepath.Join(dir, "store.db"), stmts...)
}

const (
	cursorCLI1 = "0c0c0000-0000-4000-8000-000000000001" // owner, newest
	cursorCLI2 = "0c0c0000-0000-4000-8000-000000000002" // owner, untitled
	cursorCLI3 = "0c0c0000-0000-4000-8000-000000000003" // wake run, recorded id
	cursorCLI4 = "0c0c0000-0000-4000-8000-000000000004" // history's own scratch run
	cursorCLI5 = "0c0c0000-0000-4000-8000-000000000005" // forty days old
)

func cursorCLIFixture(t *testing.T) (*CursorCLI, string) {
	t.Helper()
	home, wakeDir, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	writeCursorCLISession(t, home, "/Users/matt/code/x", cursorCLI1, "Makefile cleanup", day(21, 10),
		fixtureTurn{"tidy the Makefile", "Removed the dead targets.", day(21, 9)},
		fixtureTurn{"now add a lint target", "Added make lint.", day(21, 9).Add(50 * time.Minute)})
	writeCursorCLISession(t, home, "/Users/matt/code/y", cursorCLI2, "", day(20, 10),
		fixtureTurn{"rename the config loader", "Renamed it to loadConfig.", day(20, 9)})
	writeCursorCLISession(t, home, "/Users/matt/code/x", cursorCLI3, "wake", day(21, 11),
		fixtureTurn{"check your tincan inbox", "Done.", day(21, 11)})
	writeCursorCLISession(t, home, fixtureScratch+"/run-1", cursorCLI4, "", day(21, 12),
		fixtureTurn{"classify this question", "{}", day(21, 12)})
	writeCursorCLISession(t, home, "/Users/matt/code/x", cursorCLI5, "old", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		fixtureTurn{"an old question", "an old answer", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)})
	if err := os.WriteFile(filepath.Join(wakeDir, "cursor-cli.wake-sessions"), []byte("# Cursor CLI sessions started by the cursor-cli wake\n"+cursorCLI3+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &CursorCLI{Home: home, WakeDir: wakeDir, ScratchDir: fixtureScratch, TempDir: tmp, Now: func() time.Time { return fixtureNow }}, tmp
}

func TestCursorCLIListNewestFirstWithoutWakeOrScratchRuns(t *testing.T) {
	r, tmp := cursorCLIFixture(t)
	page, err := r.List(context.Background(), 20, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page.Conversations); got != cursorCLI1+","+cursorCLI2 {
		t.Fatalf("list = %s, want 1,2", got)
	}
	c := page.Conversations[0]
	if c.Source != SourceCursorCLI || c.Title != "Makefile cleanup" || c.Cwd != "/Users/matt/code/x" || c.Automated {
		t.Fatalf("first = %+v", c)
	}
	if page.Conversations[1].Title != "rename the config loader" {
		t.Fatalf("untitled session title = %q, want its first prompt", page.Conversations[1].Title)
	}
	if page.Limited != LimitAge {
		t.Fatalf("limited = %q, want age", page.Limited)
	}
	page, err = r.List(context.Background(), 20, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	// The scratch run is never reported; the wake run is, marked, with all.
	if got := ids(page.Conversations); got != cursorCLI3+","+cursorCLI1+","+cursorCLI2 || !page.Conversations[0].Automated {
		t.Fatalf("list --all = %s %+v", got, page.Conversations)
	}
	emptyDir(t, tmp)
}

func TestCursorCLILatestReadsTypedPromptsOnly(t *testing.T) {
	r, _ := cursorCLIFixture(t)
	got, err := convsOf(r.Read(context.Background(), Query{Source: SourceCursorCLI, Mode: ModeLatest, Count: 2}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != cursorCLI1 || got[1].ID != cursorCLI1 {
		t.Fatalf("latest 2 = %s", ids(got))
	}
	m := got[0].Messages
	if len(m) != 2 || m[0].Text != "now add a lint target" || m[1].Text != "Added make lint." {
		t.Fatalf("newest turn = %+v", m)
	}
	if want := time.Date(2026, 9, 21, 9, 50, 0, 0, time.UTC); !m[0].Time.Equal(want) {
		t.Fatalf("prompt time = %v, want %v from the timestamp block", m[0].Time, want)
	}
	for _, c := range got {
		for _, msg := range c.Messages {
			for _, bad := range []string{"<user_info>", "system_reminder", "user_query", "coding agent", "Let me look", "package main"} {
				if strings.Contains(msg.Text, bad) {
					t.Fatalf("injected or tool text %q leaked into %q", bad, msg.Text)
				}
			}
		}
	}
	got, err = convsOf(r.Read(context.Background(), Query{Source: SourceCursorCLI, Mode: ModeSearch, Terms: []string{"config", "loader"}}, Options{}))
	if err != nil || ids(got) != cursorCLI2 {
		t.Fatalf("search = %s, %v", ids(got), err)
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceCursorCLI, Mode: ModeConversation, ConversationID: cursorCLI3}, Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wake run by id: err = %v, want not found", err)
	}
}

// Cursor holds its store open, so a recent chat may be only in the -wal
// file. The reader copies the WAL with the database and still sees it.
func TestCursorCLIReadsAChatOnlyInTheWAL(t *testing.T) {
	home := t.TempDir()
	writeCursorCLISession(t, home, "/Users/matt/code/w", cursorCLI1, "", time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
		fixtureTurn{"walonly prompt about the parser", "walonly reply", time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)})
	stores, _ := filepath.Glob(filepath.Join(home, "chats", "*", cursorCLI1, "store.db"))
	if len(stores) != 1 {
		t.Fatalf("stores = %v", stores)
	}
	main, err := os.ReadFile(stores[0])
	if err != nil {
		t.Fatal(err)
	}
	wal, err := os.ReadFile(stores[0] + "-wal")
	if err != nil || bytes.Contains(main, []byte("walonly")) || !bytes.Contains(wal, []byte("walonly")) {
		t.Fatalf("fixture: the chat must be only in the WAL (wal err %v)", err)
	}
	r := &CursorCLI{Home: home, TempDir: t.TempDir(), Now: func() time.Time { return fixtureNow }}
	got, err := convsOf(r.Read(context.Background(), Query{Source: SourceCursorCLI, Mode: ModeLatest}, Options{}))
	if err != nil || len(got) != 1 || got[0].Messages[0].Text != "walonly prompt about the parser" {
		t.Fatalf("latest = %+v, %v", got, err)
	}
	// The read never touched the original's files.
	after, _ := os.ReadFile(stores[0] + "-wal")
	if !bytes.Equal(after, wal) {
		t.Fatal("the original WAL changed")
	}
}

// A store that is not a database fails the read, and the copy made for it
// is removed all the same.
func TestCursorCLILeavesNoCopyBehindOnError(t *testing.T) {
	r, tmp := cursorCLIFixture(t)
	stores, _ := filepath.Glob(filepath.Join(r.Home, "chats", "*", cursorCLI1, "store.db"))
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(stores[0] + suffix)
	}
	if err := os.WriteFile(stores[0], bytes.Repeat([]byte("not a database "), 512), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceCursorCLI, Mode: ModeLatest}, Options{}); err == nil {
		t.Fatal("read of a corrupt store succeeded")
	}
	emptyDir(t, tmp)
}

func TestCursorCLINoHistory(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".cursor")
	r := &CursorCLI{Home: home}
	_, err := r.Read(context.Background(), Query{Source: SourceCursorCLI, Mode: ModeLatest}, Options{})
	if want := "no Cursor CLI history found in " + filepath.Join(home, "chats"); !errors.Is(err, ErrNoHistory) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if got := readFailure(Query{Source: SourceCursorCLI}, err); got != "No Cursor CLI history was found on this machine." {
		t.Fatalf("reply = %q", got)
	}
}

// NewCursorCLI reads cursor-agent's config dir: $CURSOR_CONFIG_DIR, else
// $XDG_CONFIG_HOME/cursor, else ~/.cursor.
func TestNewCursorCLIHonorsConfigDir(t *testing.T) {
	t.Setenv("CURSOR_CONFIG_DIR", "/tmp/somewhere/cursor")
	if r := NewCursorCLI(); r.Home != "/tmp/somewhere/cursor" || r.Source() != SourceCursorCLI {
		t.Fatalf("reader = %+v", r)
	}
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if r := NewCursorCLI(); r.Home != "/tmp/xdg/cursor" {
		t.Fatalf("xdg home = %q", r.Home)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	h, _ := os.UserHomeDir()
	if r := NewCursorCLI(); r.Home != filepath.Join(h, ".cursor") {
		t.Fatalf("default home = %q", r.Home)
	}
}

// cursorIDEFixtureComposer returns the statements that add one composer
// (an IDE chat) to the cursorDiskKV table: composerData:<id> with its
// bubble headers, and one bubbleId:<id>:<bubble> row per message.
func cursorIDEFixtureComposer(id, name string, updated time.Time, turns ...fixtureTurn) []string {
	var headers []map[string]any
	var stmts []string
	for i, tr := range turns {
		for j, b := range []map[string]any{
			{"_v": 3, "type": 1, "text": tr.prompt, "createdAt": tr.at.Format(time.RFC3339Nano)},
			{"_v": 3, "type": 2, "text": "", "createdAt": tr.at.Add(time.Second).Format(time.RFC3339Nano)},
			{"_v": 3, "type": 2, "text": tr.reply, "createdAt": tr.at.Add(2 * time.Second).Format(time.RFC3339Nano)},
		} {
			bid := strings.Repeat("b", 8) + "-" + string(rune('a'+i)) + string(rune('0'+j))
			b["bubbleId"] = bid
			headers = append(headers, map[string]any{"bubbleId": bid, "type": b["type"]})
			raw, _ := json.Marshal(b)
			stmts = append(stmts, "INSERT INTO cursorDiskKV VALUES ("+sqlQuote("bubbleId:"+id+":"+bid)+", "+sqlQuote(string(raw))+")")
		}
	}
	d := map[string]any{"_v": 18, "composerId": id, "fullConversationHeadersOnly": headers, "createdAt": updated.Add(-time.Hour).UnixMilli(), "lastUpdatedAt": updated.UnixMilli()}
	if name != "" {
		d["name"] = name
	}
	raw, _ := json.Marshal(d)
	return append(stmts, "INSERT INTO cursorDiskKV VALUES ("+sqlQuote("composerData:"+id)+", "+sqlQuote(string(raw))+")")
}

const (
	cursorIDE1 = "1d1d0000-0000-4000-8000-000000000001"
	cursorIDE2 = "1d1d0000-0000-4000-8000-000000000002"
	cursorIDE3 = "1d1d0000-0000-4000-8000-000000000003" // recorded in a wake list
	cursorIDE4 = "1d1d0000-0000-4000-8000-000000000004" // forty days old
)

func cursorIDEFixture(t *testing.T) (*CursorIDE, string) {
	t.Helper()
	dir, wakeDir, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	stmts := []string{
		"CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		"CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		"INSERT INTO cursorDiskKV VALUES ('composerData:empty-state-draft', '{\"_v\":18,\"composerId\":\"empty-state-draft\",\"fullConversationHeadersOnly\":[],\"isDraft\":true}')",
	}
	stmts = append(stmts, cursorIDEFixtureComposer(cursorIDE1, "Gemfile bump", day(21, 10),
		fixtureTurn{"bump the Gemfile", "Bumped rails.", day(21, 9)},
		fixtureTurn{"and run bundle", "Bundle installed.", day(21, 9).Add(30 * time.Minute)})...)
	stmts = append(stmts, cursorIDEFixtureComposer(cursorIDE2, "", day(20, 10), fixtureTurn{"explain the router", "It maps paths to handlers.", day(20, 9)})...)
	stmts = append(stmts, cursorIDEFixtureComposer(cursorIDE3, "", day(21, 11), fixtureTurn{"automated", "ok", day(21, 11)})...)
	stmts = append(stmts, cursorIDEFixtureComposer(cursorIDE4, "", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), fixtureTurn{"old", "old", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)})...)
	fixtureDB(t, filepath.Join(dir, "state.vscdb"), stmts...)
	if err := os.WriteFile(filepath.Join(wakeDir, "cursor-ide.wake-sessions"), []byte(cursorIDE3+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &CursorIDE{Dir: dir, WakeDir: wakeDir, TempDir: tmp, Now: func() time.Time { return fixtureNow }}, tmp
}

func TestCursorIDEListNewestFirstAndLatest(t *testing.T) {
	r, tmp := cursorIDEFixture(t)
	page, err := r.List(context.Background(), 20, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page.Conversations); got != cursorIDE1+","+cursorIDE2 || page.Limited != LimitAge {
		t.Fatalf("list = %s (limited %q), want 1,2 limited by age", got, page.Limited)
	}
	if c := page.Conversations[0]; c.Source != SourceCursorIDE || c.Title != "Gemfile bump" {
		t.Fatalf("first = %+v", c)
	}
	if page.Conversations[1].Title != "explain the router" {
		t.Fatalf("untitled = %q", page.Conversations[1].Title)
	}
	got, err := convsOf(r.Read(context.Background(), Query{Source: SourceCursorIDE, Mode: ModeLatest}, Options{}))
	if err != nil || len(got) != 1 {
		t.Fatalf("latest = %+v, %v", got, err)
	}
	m := got[0].Messages
	if len(m) != 2 || m[0].Text != "and run bundle" || m[1].Text != "Bundle installed." || !m[0].Time.Equal(time.Date(2026, 9, 21, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("latest turn = %+v", m)
	}
	page, err = r.List(context.Background(), 20, Options{All: true})
	if err != nil || ids(page.Conversations) != cursorIDE3+","+cursorIDE1+","+cursorIDE2 || !page.Conversations[0].Automated {
		t.Fatalf("list --all = %s, %v", ids(page.Conversations), err)
	}
	emptyDir(t, tmp)
}

func TestCursorIDENoHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "globalStorage")
	tmp := t.TempDir()
	r := &CursorIDE{Dir: dir, TempDir: tmp}
	_, err := r.List(context.Background(), 5, Options{})
	if want := "no Cursor IDE history found in " + filepath.Join(dir, "state.vscdb"); !errors.Is(err, ErrNoHistory) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
	emptyDir(t, tmp)
}
