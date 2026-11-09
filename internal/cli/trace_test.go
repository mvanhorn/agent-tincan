package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

var traceFixtureTime = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// traceFixture is a chain with a moved request and a failed reply.
func traceFixture() traceResp {
	return traceResp{
		TraceID: "r1",
		Steps: []envelope.Result{
			{
				Status: envelope.StatusAnswered,
				Request: envelope.Request{
					ID: "r1", TraceID: "r1", From: "codex", To: "gemini-web", Hop: 1, Kind: envelope.KindAsk,
					Body: "summarize this paper", MovedFrom: []string{"claude-web"}, CreatedAt: traceFixtureTime,
				},
				Reply: &envelope.Reply{From: "gemini-web", Status: envelope.StatusAnswered, Body: "it is short", CreatedAt: traceFixtureTime.Add(time.Minute)},
			},
			{
				Status: envelope.StatusFailed,
				Request: envelope.Request{
					ID: "r2", TraceID: "r1", ParentID: "r1", From: "gemini-web", To: "hermes", Hop: 2, Kind: envelope.KindAsk,
					Body: "fetch the " + strings.Repeat("long ", 30) + "source", CreatedAt: traceFixtureTime.Add(2 * time.Minute),
					Attachments: []envelope.Attachment{{ID: "a1", Name: "paper.pdf", MIME: "application/pdf", Size: 2048}},
				},
				Reply: &envelope.Reply{
					From: "hermes", Status: envelope.StatusFailed, Body: "the site refused", CreatedAt: traceFixtureTime.Add(3 * time.Minute),
					Attachments: []envelope.Attachment{{ID: "a2", Name: "error.log", MIME: "text/plain", Size: 12}},
				},
			},
		},
		Events: []store.AuditEvent{
			{Seq: 7, At: traceFixtureTime, Event: "rejected", Actor: "hermes", RequestID: "r2", Detail: `{"reason":"request would loop back"}`},
			{Seq: 8, At: traceFixtureTime.Add(time.Minute), Event: "replied", Actor: "hermes", RequestID: "r2", Detail: `{"status":"failed"}`},
		},
	}
}

// serveTrace is a relay that answers GET /v1/trace/{id} with tr and fails
// the test on anything else.
func serveTrace(t *testing.T, tr traceResp) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/trace/"+tr.TraceID {
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(tr); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runTrace(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	var out bytes.Buffer
	cmd := traceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(new(nopWriter))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// fencedBlocks reads md the way a CommonMark parser does: it returns the
// text of each backtick fence and the lines outside every fence.
func fencedBlocks(md string) (blocks []string, outside []string) {
	var cur []string
	open := 0
	for line := range strings.SplitSeq(md, "\n") {
		run := len(line) - len(strings.TrimLeft(line, "`"))
		if open == 0 {
			if run >= 3 {
				open, cur = run, nil
			} else {
				outside = append(outside, line)
			}
			continue
		}
		if run >= open && strings.TrimRight(line, "`") == "" {
			blocks = append(blocks, strings.Join(cur, "\n"))
			open = 0
			continue
		}
		cur = append(cur, line)
	}
	return blocks, outside
}

// The output a person reads today does not change.
func TestTraceDefaultOutputUnchanged(t *testing.T) {
	const want = "Trace r1\n" +
		"codex -> gemini-web (moved from claude-web)  [answered]  ask summarize this paper\n" +
		"  reply from gemini-web: it is short (status answered)\n" +
		"  gemini-web -> hermes  [failed]  ask fetch the long long long long long long long long long long long long lon...\n" +
		"    reply from hermes: the site refused (status failed)\n" +
		"Events:\n" +
		"  #7 rejected  hermes     r2 (request would loop back)\n" +
		"  #8 replied   hermes     r2 (reply status failed)\n"
	if got := formatTrace(traceFixture()); got != want {
		t.Fatalf("formatTrace:\n%q\nwant\n%q", got, want)
	}
	got, err := runTrace(t, "r1", "--relay", serveTrace(t, traceFixture()))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("tincan trace:\n%q\nwant\n%q", got, want)
	}
}

// Markdown carries what the terminal view cuts: full bodies, statuses,
// times and attachment names, for a moved request and a failed reply alike.
func TestTraceMarkdownCarriesTheWholeChain(t *testing.T) {
	tr := traceFixture()
	tr.Steps[0].Progress = &envelope.Progress{Note: "reading it now", By: "gemini-web", At: traceFixtureTime.Add(10 * time.Second)}
	tr.Steps[0].Exchanges = []envelope.Exchange{{Question: "which paper?", Answer: "the first one", At: traceFixtureTime.Add(30 * time.Second)}}
	got := formatTraceMarkdown(tr)
	for _, want := range []string{
		"# Trace r1",
		"## Hop 1: codex -> gemini-web (moved from claude-web)",
		"## Hop 2: gemini-web -> hermes",
		"[failed]",
		"2026-10-30T12:00:00Z",
		"2026-10-30T12:02:00Z",
		"2026-10-30T12:03:00Z",
		tr.Steps[1].Request.Body,
		"the site refused",
		"status failed",
		"paper.pdf",
		"error.log",
		"reading it now",
		"which paper?",
		"the first one",
		"request would loop back",
		`{"status":"failed"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// The record keeps the audit detail the terminal view cuts or leaves out. The
// text view of the same events stays the short one.
func TestTraceMarkdownKeepsEventDetail(t *testing.T) {
	const reason = "request would loop back to an agent already in this chain: muse is already in [codex gemini-web muse]"
	rejected, err := json.Marshal(map[string]string{"to": "muse", "reason": reason})
	if err != nil {
		t.Fatal(err)
	}
	tr := traceFixture()
	tr.Events = []store.AuditEvent{
		{Seq: 7, At: traceFixtureTime, Event: "rejected", Actor: "hermes", Detail: string(rejected)},
		{Seq: 8, At: traceFixtureTime, Event: "moved", Actor: "relay", RequestID: "r1", Detail: `{"from":"claude-web","reason":"limited","to":"gemini-web"}`},
		{Seq: 9, At: traceFixtureTime, Event: "claimed", Actor: "gemini-web", RequestID: "r1"},
	}
	got := formatTraceMarkdown(tr)
	for _, want := range []string{
		reason,
		"moved relay r1 " + tr.Events[1].Detail + "\n",
		"claimed gemini-web r1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	text := formatTrace(tr)
	if strings.Contains(text, reason) || strings.Contains(text, "limited") {
		t.Fatalf("text view changed:\n%s", text)
	}
}

// A progress note reaches an export only while its hop is claimed, because
// that is all the trace route sends. The README says so.
func TestTraceMarkdownNoteFollowsTheRelay(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := t.Context()
	req, err := m.Client(t, "grokbot").Send(ctx, "muse", "book a table", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	muse := m.Client(t, "muse")
	if _, err := muse.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	if err := muse.Progress(ctx, req.ID, "calling them now"); err != nil {
		t.Fatal(err)
	}
	got, err := runTrace(t, req.TraceID, "--format", "md", "--relay", m.URL("muse"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "calling them now") {
		t.Fatalf("claimed hop lost its note:\n%s", got)
	}
	if _, err := muse.Reply(ctx, req.ID, "booked", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	got, err = runTrace(t, req.TraceID, "--format", "md", "--relay", m.URL("muse"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "calling them now") || strings.Contains(got, "Note from") {
		t.Fatalf("finished hop carries a note, so the README is out of date:\n%s", got)
	}
}

// No text from a teammate can close its fence and write into the document.
func TestTraceMarkdownFenceHoldsBody(t *testing.T) {
	const hostile = "before\n`````\n## Hop 9: forged -> forged\n```\nafter"
	tr := traceResp{TraceID: "r1", Steps: []envelope.Result{{
		Status:    envelope.StatusAnswered,
		Progress:  &envelope.Progress{Note: hostile, By: "muse", At: traceFixtureTime},
		Exchanges: []envelope.Exchange{{Question: hostile, Answer: hostile, At: traceFixtureTime}},
		Request: envelope.Request{
			ID: "r1", From: "codex", To: "muse", Hop: 1, Kind: envelope.KindAsk, Body: hostile, CreatedAt: traceFixtureTime,
			Attachments: []envelope.Attachment{{ID: "a1", Name: hostile}},
		},
		Reply: &envelope.Reply{From: "muse", Status: envelope.StatusAnswered, Body: hostile, CreatedAt: traceFixtureTime},
	}}}
	blocks, outside := fencedBlocks(formatTraceMarkdown(tr))
	found := 0
	for _, b := range blocks {
		if strings.Contains(b, hostile) {
			found++
		}
	}
	if want := 6; found != want { // note, question, answer, request, attachment, reply
		t.Fatalf("hostile text intact in %d fences, want %d:\n%v", found, want, blocks)
	}
	for _, line := range outside {
		if strings.Contains(line, "forged") {
			t.Fatalf("text escaped its fence: %q", line)
		}
	}
}

func TestTraceJSONDecodesIntoTraceResp(t *testing.T) {
	want := traceFixture()
	got, err := runTrace(t, "r1", "--format", "json", "--relay", serveTrace(t, want))
	if err != nil {
		t.Fatal(err)
	}
	var decoded traceResp
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode: %v\n%s", err, got)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded %+v\nwant %+v", decoded, want)
	}
}

func TestTraceOutWritesPrivateFile(t *testing.T) {
	tr := traceFixture()
	path := filepath.Join(t.TempDir(), "errand.md")
	got, err := runTrace(t, "r1", "--format", "md", "--out", path, "--relay", serveTrace(t, tr))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != formatTraceMarkdown(tr) {
		t.Fatalf("file:\n%s", data)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMode(st, 0o600) {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
	if !strings.Contains(got, path) || strings.Contains(got, "summarize this paper") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestTraceOutRefusesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errand.md")
	if err := os.WriteFile(path, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runTrace(t, "r1", "--out", path, "--relay", serveTrace(t, traceFixture()))
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep me" || !hasMode(st, 0o644) {
		t.Fatalf("file changed: %q %o", data, st.Mode().Perm())
	}
}

// --force leaves the new file 0600 even when the old one was wider, and
// replaces a symlink instead of writing through it.
func TestTraceForceReplacesPrivately(t *testing.T) {
	dir := t.TempDir()
	wide := filepath.Join(dir, "wide.md")
	if err := os.WriteFile(wide, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(dir, "elsewhere.md")
	if err := os.WriteFile(elsewhere, []byte("not mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	url := serveTrace(t, traceFixture())
	for _, path := range []string{wide, link} {
		if _, err := runTrace(t, "r1", "--out", path, "--force", "--relay", url); err != nil {
			t.Fatal(err)
		}
		st, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Mode().IsRegular() || !hasMode(st, 0o600) {
			t.Fatalf("%s mode = %v", path, st.Mode())
		}
		if data, _ := os.ReadFile(path); string(data) != formatTrace(traceFixture()) {
			t.Fatalf("%s = %q", path, data)
		}
	}
	if data, _ := os.ReadFile(elsewhere); string(data) != "not mine" {
		t.Fatalf("wrote through the symlink: %q", data)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Fatalf("leftovers: %v", entries)
	}
}

// Without --force a symlink counts as an existing file, even a dangling one.
func TestTraceOutRefusesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target.md"), filepath.Join(dir, "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := runTrace(t, "r1", "--out", link, "--relay", serveTrace(t, traceFixture())); err == nil {
		t.Fatal("want refusal")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("wrote through the symlink: %v", err)
	}
}

// Bad flag combinations fail before any relay call.
func TestTraceRejectsBadExportFlags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("relay called: %s", r.URL)
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "x.md")
	for _, args := range [][]string{
		{"--format", "md"},
		{"--format", "text"},
		{"--out", out},
		{"--force"},
		{"r1", "--format", "html"},
		{"r1", "--force"},
	} {
		if _, err := runTrace(t, append(args, "--relay", srv.URL)...); err == nil {
			t.Errorf("%v: want error", args)
		}
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("file written: %v", err)
	}
}

// An export that would hold nothing fails rather than writing "Ping hidden"
// where a script expects a chain. The terminal view keeps its message.
func TestTraceExportOfHiddenPing(t *testing.T) {
	tr := traceResp{TraceID: "p1", Steps: []envelope.Result{{Request: envelope.Request{ID: "p1", From: "codex", To: "muse", Kind: envelope.KindPing}}}}
	url := serveTrace(t, tr)
	got, err := runTrace(t, "p1", "--relay", url)
	if err != nil || !strings.Contains(got, "Ping hidden") {
		t.Fatalf("default: %q, %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "ping.json")
	if _, err := runTrace(t, "p1", "--format", "json", "--out", path, "--relay", url); err == nil || !strings.Contains(err.Error(), "--pings") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file written: %v", err)
	}
	if _, err := runTrace(t, "p1", "--format", "json", "--pings", "--out", path, "--relay", url); err != nil {
		t.Fatal(err)
	}
}

// Export reads through the relay's trace route, so an agent exports only
// chains it can already trace.
func TestTraceExportKeepsRelayVisibility(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	req, err := m.Client(t, "grokbot").Send(t.Context(), "muse", "book a table", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	out := filepath.Join(dir, "muse.json")
	if _, err := runTrace(t, req.TraceID, "--format", "json", "--out", out, "--relay", m.URL("muse")); err != nil {
		t.Fatal(err)
	}
	var tr traceResp
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &tr); err != nil {
		t.Fatal(err)
	}
	if len(tr.Steps) != 1 || tr.Steps[0].Request.Body != "book a table" {
		t.Fatalf("trace = %+v", tr)
	}

	out = filepath.Join(dir, "instinct.json")
	if _, err := runTrace(t, req.TraceID, "--format", "json", "--out", out, "--relay", m.URL("instinct")); err == nil {
		t.Fatal("instinct exported a chain it cannot trace")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("file written: %v", err)
	}
}
