package notes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// fakeExtractor returns a fixed request (or error) and records every text
// it was shown.
type fakeExtractor struct {
	mu   sync.Mutex
	r    Request
	err  error
	seen []string
}

func (f *fakeExtractor) Extract(_ context.Context, text string) (Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, text)
	return f.r, f.err
}

func (f *fakeExtractor) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

type testLog struct {
	t   *testing.T
	mu  sync.Mutex
	buf strings.Builder
	// panicOn, when set, makes the first log line containing it panic, to
	// stand in for a bug at that point of the service.
	panicOn string
}

func (l *testLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.buf.Write(p)
	trip := l.panicOn != "" && strings.Contains(string(p), l.panicOn)
	if trip {
		l.panicOn = ""
	}
	l.mu.Unlock()
	l.t.Log(strings.TrimRight(string(p), "\n"))
	if trip {
		panic("injected panic at: " + strings.TrimSpace(string(p)))
	}
	return len(p), nil
}

func (l *testLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type rig struct {
	mesh      *testrelay.Mesh
	helper    *fakeHelper
	ext       *fakeExtractor
	cfg       Config
	svc       *Service
	log       *testLog
	replyFail atomic.Bool
	replyLose atomic.Bool
	getFail   atomic.Bool
}

// newRig joins notes and codex to a test mesh and builds a notes service
// over a fake helper. The service reaches the relay through a proxy that
// can fail replies, as a network drop right after the helper call would.
func newRig(t *testing.T, rc relay.Config) *rig {
	t.Helper()
	m := testrelay.New(t, rc)
	m.JoinOnMachineOf(t, "instinct", "notes")
	m.JoinOnMachineOf(t, "instinct", "codex")
	// The relay requeues a service's lapsed claim every time; it retries by redelivery.
	if ok, err := m.Store.SetAgentKind(t.Context(), "notes", "notes"); !ok || err != nil {
		t.Fatalf("set kind: %v %v", ok, err)
	}
	r := &rig{mesh: m, ext: &fakeExtractor{}, log: &testLog{t: t}}
	target, err := url.Parse(m.URL("notes"))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.replyFail.Load() && strings.HasSuffix(req.URL.Path, "/reply") {
			http.Error(w, `{"error":"injected reply failure"}`, http.StatusServiceUnavailable)
			return
		}
		if r.replyLose.Load() && strings.HasSuffix(req.URL.Path, "/reply") {
			// The relay stores the reply, but its response never arrives.
			proxy.ServeHTTP(httptest.NewRecorder(), req)
			http.Error(w, `{"error":"injected lost response"}`, http.StatusServiceUnavailable)
			return
		}
		if r.getFail.Load() && req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/v1/requests/") {
			// A dropped connection, not an HTTP answer.
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
					return
				}
			}
			http.Error(w, "unreachable", http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, req)
	}))
	t.Cleanup(ps.Close)
	rel, err := client.NewRelayForFile(client.Config{Relay: ps.URL, Agent: "notes"}, "")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	lib := filepath.Join(base, "Library")
	if err := os.Mkdir(lib, 0o700); err != nil {
		t.Fatal(err)
	}
	r.helper = newFakeHelper(t, lib)
	r.cfg = Config{
		Relay:             rel,
		Agent:             "notes",
		HelperPath:        r.helper.path,
		LibraryRoot:       lib,
		AppSupportRoot:    filepath.Join(base, "support"),
		SpoolDir:          filepath.Join(base, "spool"),
		ReadAllowlistPath: filepath.Join(base, "notes-allow.txt"),
		AddAllowlistPath:  filepath.Join(base, "notes-add-allow.txt"),
		HealthPath:        filepath.Join(base, "health.json"),
		Extractor:         r.ext,
		Hold:              time.Second,
		Log:               r.log,
	}
	r.restart(t)
	return r
}

// restart builds a fresh service from the same config, as a service
// restart does.
func (r *rig) restart(t *testing.T) {
	t.Helper()
	svc, err := New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// send sends body from agent to notes without serving it.
func (r *rig) send(t *testing.T, from, body string) envelope.Request {
	t.Helper()
	req, err := r.mesh.Client(t, from).Send(t.Context(), "notes", body, envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// poll runs one service poll and checks how many requests it handled.
func (r *rig) poll(t *testing.T, want int) {
	t.Helper()
	n, err := r.svc.PollOnce(t.Context())
	if err != nil || n != want {
		t.Fatalf("PollOnce = %d, %v; want %d", n, err, want)
	}
}

func (r *rig) get(t *testing.T, from, id string) client.Result {
	t.Helper()
	res, err := r.mesh.Client(t, from).Get(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ask sends body from agent, serves it, and returns the reply.
func (r *rig) ask(t *testing.T, from, body string) client.Result {
	t.Helper()
	req := r.send(t, from, body)
	r.poll(t, 1)
	res := r.get(t, from, req.ID)
	if res.Reply == nil {
		t.Fatalf("no reply: status %s", res.Status)
	}
	return res
}

func (r *rig) spoolFiles(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(r.cfg.SpoolDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func (r *rig) assertSpoolEmpty(t *testing.T) {
	t.Helper()
	if left := r.spoolFiles(t); len(left) != 0 {
		t.Fatalf("spool not empty: %v", left)
	}
}

func addBody(title, body string, tags ...string) string {
	m := map[string]any{"op": "add", "title": title, "body": body}
	if tags != nil {
		m["tags"] = tags
	}
	b, _ := json.Marshal(m)
	return "note: " + string(b)
}

func TestAddAnswersWithNoteIDAndProvenance(t *testing.T) {
	r := newRig(t, relay.Config{})
	req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent", "camping"))
	r.poll(t, 1)
	res := r.get(t, "grokbot", req.ID)
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s, reply %+v", res.Status, res.Reply)
	}
	notes := r.helper.notes()
	if len(notes) != 1 {
		t.Fatalf("notes = %+v, want one", notes)
	}
	n := notes[0]
	if !strings.Contains(res.Reply.Body, n.ID) || !strings.Contains(res.Reply.Body, "Tent") {
		t.Fatalf("reply %q does not name note %s", res.Reply.Body, n.ID)
	}
	if n.Body != "buy the blue tent" || !slices.Contains(n.Tags, "camping") || !slices.Contains(n.Tags, "from-agent") {
		t.Fatalf("note = %+v", n)
	}
	creates := r.helper.callsOf(t, "create")
	if len(creates) != 1 {
		t.Fatalf("creates = %d", len(creates))
	}
	args := creates[0].Args
	if key, _ := argValue(args, "idempotency-key"); key != "tincan:"+req.ID {
		t.Fatalf("idempotency key = %q, want tincan:%s", key, req.ID)
	}
	if !slices.Contains(args, "--background") {
		t.Fatalf("create not in the background: %q", args)
	}
	raw, _ := argValue(args, "properties")
	var props map[string]string
	if err := json.Unmarshal([]byte(raw), &props); err != nil {
		t.Fatalf("properties %q: %v", raw, err)
	}
	if props["remote.agent"] != "grokbot" || props["remote.trace-id"] != req.TraceID || props["remote.via"] != "tincan" {
		t.Fatalf("properties = %v (trace %s)", props, req.TraceID)
	}
	r.assertSpoolEmpty(t)
}

// Covers R4: a free-text add stores the sender's text verbatim, whatever
// body the extractor produced.
func TestFreeTextAddKeepsSenderTextVerbatim(t *testing.T) {
	r := newRig(t, relay.Config{})
	text := "save this: buy the blue tent, not the green one"
	r.ext.r = Request{Op: OpAdd, Title: "Tent choice", Body: "the model wrote this"}
	res := r.ask(t, "grokbot", text)
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	if notes := r.helper.notes(); len(notes) != 1 || notes[0].Body != text {
		t.Fatalf("notes = %+v, want body exactly %q", notes, text)
	}
}

// With no extractor configured, free text fails with the structured form.
func TestFreeTextWithoutExtractorFailsWithStructuredForm(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.cfg.Extractor = nil
	r.restart(t)
	res := r.ask(t, "grokbot", "save this: buy the blue tent")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, `note: {"op":"add"`) {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	if len(r.helper.calls(t)) != 0 {
		t.Fatal("helper ran for an unparsed request")
	}
	r.assertSpoolEmpty(t)
}

func TestSearchReturnsActiveMatchesUpToCount(t *testing.T) {
	r := newRig(t, relay.Config{})
	var active []string
	for i := range 5 {
		n := r.helper.put(t, fakeNote{Title: "Tent " + string(rune('A'+i)), Body: "tent notes number " + string(rune('A'+i)), Tags: []string{"camping"}})
		active = append(active, n.ID)
	}
	trashed := r.helper.put(t, fakeNote{Title: "Tent trashed", Body: "tent", State: "trashed"})
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"tent","count":3}`)
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	found := 0
	for _, id := range active {
		if strings.Contains(res.Reply.Body, id) {
			found++
		}
	}
	if found != 3 {
		t.Fatalf("reply names %d notes, want 3:\n%s", found, res.Reply.Body)
	}
	if strings.Contains(res.Reply.Body, trashed.ID) {
		t.Fatalf("reply names the trashed note:\n%s", res.Reply.Body)
	}
	if !strings.Contains(res.Reply.Body, "tent notes number") {
		t.Fatalf("reply has no snippet:\n%s", res.Reply.Body)
	}
	r.assertSpoolEmpty(t)
}

func TestSearchMatchingOnlyTrashedOrArchivedReturnsNothing(t *testing.T) {
	r := newRig(t, relay.Config{})
	a := r.helper.put(t, fakeNote{Title: "Old tent", Body: "x", State: "archived"})
	b := r.helper.put(t, fakeNote{Title: "Bad tent", Body: "y", State: "trashed"})
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	if strings.Contains(res.Reply.Body, a.ID) || strings.Contains(res.Reply.Body, b.ID) || !strings.Contains(res.Reply.Body, "No notes") {
		t.Fatalf("reply = %q, want no results", res.Reply.Body)
	}
}

func TestReadReturnsNote(t *testing.T) {
	r := newRig(t, relay.Config{})
	n := r.helper.put(t, fakeNote{Title: "Tent", Body: "# Tent\nbuy the blue one", Tags: []string{"camping"}})
	res := r.ask(t, "grokbot", `note: {"op":"read","id":"`+n.ID+`"}`)
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	for _, want := range []string{"Tent", n.ID, "camping", "# Tent\nbuy the blue one"} {
		if !strings.Contains(res.Reply.Body, want) {
			t.Fatalf("reply missing %q:\n%s", want, res.Reply.Body)
		}
	}
	r.assertSpoolEmpty(t)
}

func TestReadUnknownIDFails(t *testing.T) {
	r := newRig(t, relay.Config{})
	id := fakeUUID()
	res := r.ask(t, "grokbot", `note: {"op":"read","id":"`+id+`"}`)
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "No note with id "+id) {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
}

func TestReadOfArchivedOrTrashedIsNotFound(t *testing.T) {
	r := newRig(t, relay.Config{})
	for _, state := range []string{"archived", "trashed"} {
		n := r.helper.put(t, fakeNote{Title: "Secret " + state, Body: "hidden body", State: state})
		res := r.ask(t, "grokbot", `note: {"op":"read","id":"`+n.ID+`"}`)
		if res.Status != envelope.StatusFailed || res.Reply.Body != notFoundReply(n.ID) || strings.Contains(res.Reply.Body, "hidden body") {
			t.Fatalf("%s: status %s body %q", state, res.Status, res.Reply.Body)
		}
	}
	unknown := fakeUUID()
	if res := r.ask(t, "grokbot", `note: {"op":"read","id":"`+unknown+`"}`); res.Reply.Body != notFoundReply(unknown) {
		t.Fatalf("unknown id reply %q differs from the non-active one", res.Reply.Body)
	}
}

func TestReadBodyOverCapIsTruncated(t *testing.T) {
	r := newRig(t, relay.Config{})
	big := strings.Repeat("0123456789", 10_000) + "TAIL-OF-BODY"
	n := r.helper.put(t, fakeNote{Title: "Big", Body: big})
	res := r.ask(t, "grokbot", `note: {"op":"read","id":"`+n.ID+`"}`)
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s", res.Status)
	}
	if len(res.Reply.Body) > maxReplyBytes {
		t.Fatalf("reply is %d bytes, over the %d cap", len(res.Reply.Body), maxReplyBytes)
	}
	if strings.Contains(res.Reply.Body, "TAIL-OF-BODY") || !strings.Contains(res.Reply.Body, "truncated") {
		t.Fatalf("reply not truncated with a notice (tail: %q)", res.Reply.Body[len(res.Reply.Body)-200:])
	}
}

// Covers AE2: the helper succeeds but the reply fails, the service
// restarts, and the relay redelivers: the recorded reply is resent with
// the same note id, without calling the helper again.
func TestReplyFailureThenRedeliveryRepliesSameNoteID(t *testing.T) {
	r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
	r.replyFail.Store(true)
	req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent"))
	r.poll(t, 1)
	if res := r.get(t, "grokbot", req.ID); res.Reply != nil {
		t.Fatalf("got a reply through a failing relay: %+v", res.Reply)
	}
	notes := r.helper.notes()
	if len(notes) != 1 {
		t.Fatalf("notes = %d, want 1", len(notes))
	}
	if len(r.spoolFiles(t)) != 1 {
		t.Fatalf("spool = %v, want the unanswered add kept", r.spoolFiles(t))
	}

	r.replyFail.Store(false)
	r.restart(t)
	time.Sleep(200 * time.Millisecond)
	r.mesh.Server.Sweep(t.Context())
	r.poll(t, 1)
	res := r.get(t, "grokbot", req.ID)
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, notes[0].ID) {
		t.Fatalf("status %s reply %+v, want answered with %s", res.Status, res.Reply, notes[0].ID)
	}
	creates := r.helper.callsOf(t, "create")
	if len(creates) != 1 {
		t.Fatalf("creates = %d, want the redelivery to resend the recorded reply", len(creates))
	}
	if key, _ := argValue(creates[0].Args, "idempotency-key"); key != "tincan:"+req.ID {
		t.Fatalf("key = %q", key)
	}
	if len(r.helper.notes()) != 1 {
		t.Fatalf("a second note was created: %+v", r.helper.notes())
	}
	r.assertSpoolEmpty(t)
}

// Covers AE1: with the library missing, an add stays spooled and
// unanswered, the asker gets a progress note, and once the library is back
// the add is applied and answered.
func TestAddStaysSpooledWhileLibraryMissingThenApplied(t *testing.T) {
	r := newRig(t, relay.Config{})
	if err := os.RemoveAll(r.cfg.LibraryRoot); err != nil {
		t.Fatal(err)
	}
	req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent"))
	r.poll(t, 1)
	res := r.get(t, "grokbot", req.ID)
	if res.Reply != nil {
		t.Fatalf("an add that could not be applied was answered: %+v", res.Reply)
	}
	if res.Progress == nil || !strings.Contains(res.Progress.Note, "unauthorized_library") {
		t.Fatalf("progress = %+v, want a note naming the helper error", res.Progress)
	}
	if len(r.spoolFiles(t)) != 1 {
		t.Fatalf("spool = %v", r.spoolFiles(t))
	}
	var h Health
	b, err := os.ReadFile(r.cfg.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &h); err != nil || h.OK || h.Code != "unauthorized_library" {
		t.Fatalf("health = %s (%v)", b, err)
	}

	if err := os.Mkdir(r.cfg.LibraryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if n := r.svc.RetrySpooled(t.Context()); n != 1 {
		t.Fatalf("RetrySpooled tried %d entries", n)
	}
	res = r.get(t, "grokbot", req.ID)
	notes := r.helper.notes()
	if res.Status != envelope.StatusAnswered || len(notes) != 1 || !strings.Contains(res.Reply.Body, notes[0].ID) {
		t.Fatalf("status %s reply %+v notes %+v", res.Status, res.Reply, notes)
	}
	r.assertSpoolEmpty(t)
	b, _ = os.ReadFile(r.cfg.HealthPath)
	if err := json.Unmarshal(b, &h); err != nil || !h.OK {
		t.Fatalf("health after recovery = %s", b)
	}
}

// Covers AE3: an add applied and then trashed by the owner is redelivered:
// no new note, and the reply names the trashed note.
func TestRedeliveredAddOfTrashedNoteRepliesWithIt(t *testing.T) {
	r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
	r.replyFail.Store(true)
	req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent"))
	r.poll(t, 1)
	notes := r.helper.notes()
	if len(notes) != 1 {
		t.Fatalf("notes = %d", len(notes))
	}
	r.helper.setState(t, notes[0].ID, "trashed")
	r.helper.setTitle(t, notes[0].ID, "Renamed by the owner")
	r.replyFail.Store(false)
	time.Sleep(200 * time.Millisecond)
	r.mesh.Server.Sweep(t.Context())
	r.poll(t, 1)
	res := r.get(t, "grokbot", req.ID)
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, notes[0].ID) || !strings.Contains(res.Reply.Body, "trash") {
		t.Fatalf("status %s reply %+v", res.Status, res.Reply)
	}
	if strings.Contains(res.Reply.Body, "Renamed by the owner") {
		t.Fatalf("resent reply exposed the note's current title: %q", res.Reply.Body)
	}
	if len(r.helper.notes()) != 1 {
		t.Fatalf("a new note was created: %+v", r.helper.notes())
	}
	r.assertSpoolEmpty(t)
}

// A stuck add never holds up a search queued behind it.
func TestSearchBehindStuckAddIsAnswered(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.helper.put(t, fakeNote{Title: "Tent", Body: "blue"})
	r.helper.failCreates(t, "operation_failed")
	add := r.send(t, "grokbot", addBody("Stove", "buy fuel"))
	search := r.send(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	r.poll(t, 2)
	if res := r.get(t, "grokbot", add.ID); res.Reply != nil || res.Progress == nil || !strings.Contains(res.Progress.Note, "operation_failed") {
		t.Fatalf("stuck add: reply %+v progress %+v", res.Reply, res.Progress)
	}
	if res := r.get(t, "grokbot", search.ID); res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "Tent") {
		t.Fatalf("search: status %s reply %+v", res.Status, res.Reply)
	}
	if len(r.spoolFiles(t)) != 1 {
		t.Fatalf("spool = %v, want only the stuck add", r.spoolFiles(t))
	}
}

// A retry that is still failing keeps the entry and the request unanswered.
func TestRetryWhileStillFailingKeepsEntry(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.helper.failCreates(t, "operation_failed")
	req := r.send(t, "grokbot", addBody("Stove", "buy fuel"))
	r.poll(t, 1)
	r.svc.RetrySpooled(t.Context())
	if res := r.get(t, "grokbot", req.ID); res.Reply != nil {
		t.Fatalf("answered while failing: %+v", res.Reply)
	}
	if len(r.spoolFiles(t)) != 1 {
		t.Fatalf("spool = %v", r.spoolFiles(t))
	}
}

// Permanent failures reply failed, clear the spool entry, and let the next
// request through.
func TestPermanentAddFailuresReplyFailedAndClearSpool(t *testing.T) {
	t.Run("newline in title", func(t *testing.T) {
		r := newRig(t, relay.Config{})
		res := r.ask(t, "grokbot", addBody("Tent\nsecond line", "body"))
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "title") {
			t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
		}
		r.assertSpoolEmpty(t)
		if len(r.helper.notes()) != 0 {
			t.Fatal("a note was written")
		}
		if res := r.ask(t, "grokbot", `note: {"op":"search","query":"x"}`); res.Status != envelope.StatusAnswered {
			t.Fatalf("next request: status %s", res.Status)
		}
	})
	t.Run("helper validation error", func(t *testing.T) {
		r := newRig(t, relay.Config{})
		r.helper.failCreates(t, "invalid_metadata")
		res := r.ask(t, "grokbot", addBody("Tent", "body"))
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "invalid_metadata") {
			t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
		}
		r.assertSpoolEmpty(t)
	})
}

// With a spool entry present at startup whose relay request already
// expired, the note is still created and the failed reply is logged, not
// retried forever.
func TestStartupDrainOfExpiredRequestCreatesNoteOnce(t *testing.T) {
	r := newRig(t, relay.Config{RequestTTL: 400 * time.Millisecond, NotesRequestTTL: 400 * time.Millisecond, ClaimLease: 100 * time.Millisecond})
	r.helper.failCreates(t, "operation_failed")
	req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent"))
	r.poll(t, 1)
	time.Sleep(500 * time.Millisecond)
	r.mesh.Server.Sweep(t.Context())
	if res := r.get(t, "grokbot", req.ID); res.Status != envelope.StatusExpired {
		t.Fatalf("status %s, want expired", res.Status)
	}
	r.helper.failCreates(t, "")
	r.restart(t)
	if n := r.svc.RetrySpooled(t.Context()); n != 1 {
		t.Fatalf("drain tried %d", n)
	}
	if len(r.helper.notes()) != 1 {
		t.Fatalf("notes = %+v", r.helper.notes())
	}
	r.assertSpoolEmpty(t)
	if !strings.Contains(r.log.String(), "reply failed") {
		t.Fatalf("the failed reply was not logged:\n%s", r.log.String())
	}
	before := len(r.helper.callsOf(t, "create"))
	r.svc.RetrySpooled(t.Context())
	if after := len(r.helper.callsOf(t, "create")); after != before {
		t.Fatalf("drained again: creates %d -> %d", before, after)
	}
}

// Run drains the spool at startup without holding up polling.
func TestRunDrainsSpoolAndKeepsPolling(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.helper.failCreates(t, "operation_failed")
	add := r.send(t, "grokbot", addBody("Tent", "blue"))
	r.poll(t, 1)
	r.helper.failCreates(t, "")
	r.cfg.RetryEvery = 50 * time.Millisecond
	r.restart(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.svc.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	search := r.send(t, "muse", `note: {"op":"search","query":"tent"}`)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		a, s := r.get(t, "grokbot", add.ID), r.get(t, "muse", search.ID)
		if a.Status == envelope.StatusAnswered && s.Status == envelope.StatusAnswered {
			r.assertSpoolEmpty(t)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("add or search not answered while running")
}

// An agent on neither allowlist is declined before the extractor runs.
func TestNeitherAllowlistDeclinedBeforeExtractor(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, r.cfg.ReadAllowlistPath, "grokbot\n")
	writeFile(t, r.cfg.AddAllowlistPath, "grokbot\n")
	r.ext.r = Request{Op: OpSearch, Query: "tent"}
	res := r.ask(t, "muse", "find my tent notes")
	if res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "muse") {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	if r.ext.calls() != 0 || len(r.helper.calls(t)) != 0 {
		t.Fatal("a declined request reached the extractor or the helper")
	}
}

// The add allowlist denies an agent: declined, nothing spooled or written.
func TestAddAllowlistDeniesAdd(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, r.cfg.AddAllowlistPath, "grokbot\n")
	res := r.ask(t, "muse", addBody("Tent", "blue"))
	if res.Status != envelope.StatusDeclined {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	r.assertSpoolEmpty(t)
	if len(r.helper.calls(t)) != 0 || len(r.helper.notes()) != 0 {
		t.Fatal("a declined add reached the helper")
	}
}

// On the read allowlist but not the add one: search works, add is declined.
func TestReadAllowedAddDenied(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, r.cfg.ReadAllowlistPath, "muse grokbot\n")
	writeFile(t, r.cfg.AddAllowlistPath, "grokbot\n")
	r.helper.put(t, fakeNote{Title: "Tent", Body: "blue"})
	if res := r.ask(t, "muse", `note: {"op":"search","query":"tent"}`); res.Status != envelope.StatusAnswered {
		t.Fatalf("search: status %s body %q", res.Status, res.Reply.Body)
	}
	if res := r.ask(t, "muse", addBody("Stove", "fuel")); res.Status != envelope.StatusDeclined {
		t.Fatalf("add: status %s body %q", res.Status, res.Reply.Body)
	}
	if res := r.ask(t, "grokbot", addBody("Stove", "fuel")); res.Status != envelope.StatusAnswered {
		t.Fatalf("grokbot add: status %s body %q", res.Status, res.Reply.Body)
	}
}

// On the add allowlist but not the read one: add works, search is declined.
func TestAddAllowedReadDenied(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, r.cfg.ReadAllowlistPath, "grokbot\n")
	writeFile(t, r.cfg.AddAllowlistPath, "muse\n")
	if res := r.ask(t, "muse", `note: {"op":"read","id":"`+fakeUUID()+`"}`); res.Status != envelope.StatusDeclined {
		t.Fatalf("read: status %s body %q", res.Status, res.Reply.Body)
	}
	if res := r.ask(t, "muse", addBody("Stove", "fuel")); res.Status != envelope.StatusAnswered {
		t.Fatalf("add: status %s body %q", res.Status, res.Reply.Body)
	}
}

// A read allowlist that cannot be loaded does not decline an agent the
// add allowlist allows: its add goes through, while its search is still
// declined on the broken read list.
func TestBrokenReadAllowlistStillAllowsAdd(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, r.cfg.ReadAllowlistPath, "bad!name\n")
	writeFile(t, r.cfg.AddAllowlistPath, "muse\n")
	if res := r.ask(t, "muse", addBody("Stove", "fuel")); res.Status != envelope.StatusAnswered {
		t.Fatalf("add: status %s body %q", res.Status, res.Reply.Body)
	}
	if len(r.helper.notes()) != 1 {
		t.Fatalf("notes = %+v, want the add written", r.helper.notes())
	}
	res := r.ask(t, "muse", `note: {"op":"search","query":"stove"}`)
	if res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "could not read its allowlist") {
		t.Fatalf("search: status %s body %q", res.Status, res.Reply.Body)
	}
	if !strings.Contains(r.log.String(), "read allowlist") {
		t.Fatalf("the read list error was not logged:\n%s", r.log.String())
	}
}

// When neither allowlist loads, the request is declined before the
// extractor runs.
func TestBothAllowlistsBrokenDeclinedBeforeExtractor(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, r.cfg.ReadAllowlistPath, "bad!name\n")
	writeFile(t, r.cfg.AddAllowlistPath, "bad!name\n")
	r.ext.r = Request{Op: OpSearch, Query: "tent"}
	res := r.ask(t, "muse", "find my tent notes")
	if res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "could not read its allowlist") {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	if r.ext.calls() != 0 || len(r.helper.calls(t)) != 0 {
		t.Fatal("a declined request reached the extractor or the helper")
	}
	for _, list := range []string{"read allowlist", "add allowlist"} {
		if !strings.Contains(r.log.String(), list) {
			t.Fatalf("the %s error was not logged:\n%s", list, r.log.String())
		}
	}
}

func TestMalformedNoteBodyFails(t *testing.T) {
	r := newRig(t, relay.Config{})
	for _, body := range []string{
		`note: {"op":"add","title":"x","body":"y","bogus":1}`,
		`note: {"op":"add","title":"x","title":"y","body":"z"}`,
		`note: {"op":"add","title":null,"body":"z"}`,
		`note: {"op":"search","query":"x"} trailing`,
		`note: {"op":"delete","id":"x"}`,
		"note:\n{\"op\":",
	} {
		res := r.ask(t, "grokbot", body)
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "note:") {
			t.Fatalf("%s: status %s body %q", body, res.Status, res.Reply.Body)
		}
	}
	if r.ext.calls() != 0 || len(r.helper.calls(t)) != 0 {
		t.Fatal("a malformed structured request reached the extractor or helper")
	}
	r.assertSpoolEmpty(t)
}

// Every helper call carries the service's own Application Support root, so
// a stale in-app session context never makes it fail turn_context_invalid.
func TestHelperRunsWithServiceOwnRoots(t *testing.T) {
	stale := t.TempDir()
	writeFile(t, filepath.Join(stale, "session-context.json"), `{"stale":true}`)
	t.Setenv("AGENT_NOTES_APPLICATION_SUPPORT_ROOT", stale)
	t.Setenv("AGENT_NOTES_LIBRARY_ROOT", t.TempDir())
	t.Setenv("AGENT_NOTES_PROVIDER", "claude")
	r := newRig(t, relay.Config{})
	if res := r.ask(t, "grokbot", addBody("Tent", "blue")); res.Status != envelope.StatusAnswered {
		t.Fatalf("add: status %s body %q", res.Status, res.Reply.Body)
	}
	if res := r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`); res.Status != envelope.StatusAnswered {
		t.Fatalf("search: status %s body %q", res.Status, res.Reply.Body)
	}
	calls := r.helper.calls(t)
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	for _, c := range calls {
		want := map[string]string{
			"AGENT_NOTES_APPLICATION_SUPPORT_ROOT": r.cfg.AppSupportRoot,
			"AGENT_NOTES_LIBRARY_ROOT":             r.cfg.LibraryRoot,
		}
		if len(c.Env) != len(want) || c.Env["AGENT_NOTES_APPLICATION_SUPPORT_ROOT"] != want["AGENT_NOTES_APPLICATION_SUPPORT_ROOT"] || c.Env["AGENT_NOTES_LIBRARY_ROOT"] != want["AGENT_NOTES_LIBRARY_ROOT"] {
			t.Fatalf("%s env = %v, want %v", c.Args[0], c.Env, want)
		}
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	r := newRig(t, relay.Config{})
	for name, mut := range map[string]func(*Config){
		"no relay":       func(c *Config) { c.Relay = nil },
		"no library":     func(c *Config) { c.LibraryRoot = "" },
		"no app support": func(c *Config) { c.AppSupportRoot = "" },
		"no spool":       func(c *Config) { c.SpoolDir = "" },
	} {
		c := r.cfg
		mut(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	c := r.cfg
	c.HelperPath = ""
	svc, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if svc.helper.Path != DefaultHelperPath {
		t.Fatalf("helper path = %q, want the default", svc.helper.Path)
	}
}

func TestHelperErrorClassification(t *testing.T) {
	for code, permanent := range map[string]bool{
		"invalid_metadata":      true,
		"missing_option":        true,
		"unsupported_option":    true,
		"invalid_identifier":    true,
		"input_too_large":       true,
		"invalid_utf8":          true,
		"unknown_command":       true,
		"invalid_request":       true,
		"unauthorized_library":  false,
		"operation_failed":      false,
		"turn_context_invalid":  false,
		"missing_authorization": false,
		"something_new":         false,
	} {
		err := error(&HelperError{Code: code})
		if got := IsPermanent(err); got != permanent {
			t.Errorf("%s: permanent = %v, want %v", code, got, permanent)
		}
	}
	if IsPermanent(errors.New("exec failed")) {
		t.Error("a non-helper error is permanent")
	}
}

// The helper looks keys up in the library as loaded at its start, so the
// service never runs two creates at once, even across the poll loop and
// concurrent retries.
func TestCreatesNeverOverlap(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, filepath.Join(r.helper.state, "slow-create"), "")
	for i, id := range []string{"aaaa01", "aaaa02", "aaaa03"} {
		e := SpoolEntry{Request: envelope.Request{ID: id, From: "grokbot", TraceID: id}, Note: Request{Op: OpAdd, Title: "T" + id}, SpooledAt: time.Now().Add(time.Duration(i) * time.Second)}
		if err := r.svc.spool.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			r.svc.RetrySpooled(t.Context())
		})
	}
	wg.Wait()
	if _, err := os.Stat(filepath.Join(r.helper.state, "overlap")); err == nil {
		t.Fatal("two helper creates ran at once")
	}
	if n := len(r.helper.notes()); n != 3 {
		t.Fatalf("notes = %d, want 3", n)
	}
}

// redeliver lets the claim lease run out and requeues the request, as the
// relay does for a claimed request left unanswered.
func (r *rig) redeliver(t *testing.T) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	r.mesh.Server.Sweep(t.Context())
}

// KTD6: an extraction that fails (not unclear) leaves the request
// unanswered for redelivery; the third failure replies failed with the
// structured form. The helper is never called and nothing is spooled.
func TestExtractorFailureRedeliveredThenFailsOnThird(t *testing.T) {
	r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
	r.ext.err = errors.New("codex timed out")
	req := r.send(t, "grokbot", "save this: buy the blue tent")
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			r.redeliver(t)
		}
		r.poll(t, 1)
		if res := r.get(t, "grokbot", req.ID); res.Reply != nil {
			t.Fatalf("attempt %d: answered after an extractor failure: %+v", attempt, res.Reply)
		}
		if len(r.helper.calls(t)) != 0 {
			t.Fatalf("attempt %d: helper ran after an extractor failure", attempt)
		}
		r.assertSpoolEmpty(t)
	}
	r.redeliver(t)
	r.poll(t, 1)
	res := r.get(t, "grokbot", req.ID)
	if res.Reply == nil || res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, `note: {"op":"add"`) {
		t.Fatalf("third failure: status %s reply %+v", res.Status, res.Reply)
	}
	if r.ext.calls() != 3 {
		t.Fatalf("extractor ran %d times, want 3", r.ext.calls())
	}
	if len(r.helper.calls(t)) != 0 || len(r.helper.notes()) != 0 {
		t.Fatal("helper ran or a note was saved after extraction failed")
	}
	r.assertSpoolEmpty(t)
}

// Unclear is the model's answer, not a failure: replied failed at once.
func TestUnclearFreeTextFailsImmediately(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.ext.err = fmt.Errorf("%w: op unclear", ErrUnclearRequest)
	res := r.ask(t, "grokbot", "how is the weather")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, `note: {"op":"add"`) {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	if r.ext.calls() != 1 || len(r.helper.calls(t)) != 0 {
		t.Fatal("unclear request retried or reached the helper")
	}
	r.assertSpoolEmpty(t)
}

func TestStructuredRequestNeverInvokesExtractor(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.ext.err = errors.New("must not run")
	if res := r.ask(t, "grokbot", addBody("Tent", "blue")); res.Status != envelope.StatusAnswered {
		t.Fatalf("add: status %s body %q", res.Status, res.Reply.Body)
	}
	if res := r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`); res.Status != envelope.StatusAnswered {
		t.Fatalf("search: status %s body %q", res.Status, res.Reply.Body)
	}
	if r.ext.calls() != 0 {
		t.Fatalf("extractor ran %d times for structured requests", r.ext.calls())
	}
}

// R11: an agent on the read allowlist only whose free text extracts as an
// add is declined, and nothing is spooled or written.
func TestReadOnlyAgentFreeTextAddDeclined(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, r.cfg.ReadAllowlistPath, "muse\n")
	writeFile(t, r.cfg.AddAllowlistPath, "grokbot\n")
	r.ext.r = Request{Op: OpAdd, Title: "Tent"}
	res := r.ask(t, "muse", "save this: buy the blue tent")
	if res.Status != envelope.StatusDeclined {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	r.assertSpoolEmpty(t)
	if len(r.helper.calls(t)) != 0 || len(r.helper.notes()) != 0 {
		t.Fatal("a declined free-text add reached the helper")
	}
}

// Covers AE4 end to end through the Codex extractor: the saved body is
// the sender's text byte for byte, whatever the model chose.
func TestCodexFreeTextAddBodyIsVerbatim(t *testing.T) {
	r := newRig(t, relay.Config{})
	_, x := fakeCodex(t, `{"op":"add","title":"Tent choice","tags":["camping"],"query":"","count":0,"id":""}`)
	r.cfg.Extractor = x
	r.restart(t)
	text := "save this: buy the blue tent, not the green one  \n\tcafé ✓\n"
	res := r.ask(t, "grokbot", text)
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	notes := r.helper.notes()
	if len(notes) != 1 || notes[0].Body != text || notes[0].Title != "Tent choice" {
		t.Fatalf("notes = %+v, want body exactly %q", notes, text)
	}
}

// An Agent Notes build older than the idempotency feature ignores the key
// and sends no "existed" field: the note was written, so the add is
// answered, but health says the helper is too old.
func TestOldHelperWithoutExistedAnsweredAndFlagged(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, filepath.Join(r.helper.state, "old-helper"), "")
	res := r.ask(t, "grokbot", addBody("Tent", "blue"))
	notes := r.helper.notes()
	if res.Status != envelope.StatusAnswered || len(notes) != 1 || !strings.Contains(res.Reply.Body, notes[0].ID) {
		t.Fatalf("status %s reply %+v notes %+v", res.Status, res.Reply, notes)
	}
	r.assertSpoolEmpty(t)
	var h Health
	b, err := os.ReadFile(r.cfg.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &h); err != nil || h.OK || h.Code != "helper_too_old" || !strings.Contains(h.Message, "update Agent Notes") {
		t.Fatalf("health = %s (%v)", b, err)
	}
	if !strings.Contains(r.log.String(), "helper_too_old") {
		t.Fatalf("log does not flag the old helper:\n%s", r.log.String())
	}
}

// A current helper reports "existed": false on a fresh create: healthy.
func TestCurrentHelperFreshCreateIsHealthy(t *testing.T) {
	r := newRig(t, relay.Config{})
	if res := r.ask(t, "grokbot", addBody("Tent", "blue")); res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s body %q", res.Status, res.Reply.Body)
	}
	var h Health
	b, _ := os.ReadFile(r.cfg.HealthPath)
	if err := json.Unmarshal(b, &h); err != nil || !h.OK || h.Code != "" {
		t.Fatalf("health = %s (%v)", b, err)
	}
}

// Finding #1: exec rejects an argv string holding NUL before the helper
// starts, so an add body with NUL is refused at validation: answered
// failed, nothing spooled or written.
func TestAddWithNULBodyAnsweredFailedNothingSpooled(t *testing.T) {
	r := newRig(t, relay.Config{})
	res := r.ask(t, "grokbot", addBody("Tent", "a\x00b"))
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "NUL") {
		t.Fatalf("status %s reply %+v", res.Status, res.Reply)
	}
	r.assertSpoolEmpty(t)
	if len(r.helper.calls(t)) != 0 || len(r.helper.notes()) != 0 {
		t.Fatal("the NUL add reached the helper")
	}
}

// A helper that cannot even be started with these arguments (EINVAL from
// a NUL in argv) is a permanent failure, not a missing helper.
func TestHelperStartFailureFromArgsIsPermanent(t *testing.T) {
	r := newRig(t, relay.Config{})
	_, err := r.svc.helper.Create(t.Context(), "Tent", "a\x00b", nil, map[string]string{}, "tincan:x")
	if !IsPermanent(err) || errorCode(err) != "invalid_request" {
		t.Fatalf("err = %v (code %q), want permanent invalid_request", err, errorCode(err))
	}
	missing := &Helper{Path: filepath.Join(t.TempDir(), "no-helper"), LibraryRoot: r.cfg.LibraryRoot, AppSupportRoot: r.cfg.AppSupportRoot}
	if _, err := missing.Search(t.Context(), "x"); IsPermanent(err) || errorCode(err) != "helper_unavailable" {
		t.Fatalf("missing helper: err = %v, want transient helper_unavailable", err)
	}
}

// An add already spooled with a NUL body (from a build before validation
// refused it) gets a final failed reply on its retry instead of being
// retried forever.
func TestSpooledPoisonAddGetsFinalFailedReply(t *testing.T) {
	r := newRig(t, relay.Config{})
	req := r.send(t, "grokbot", addBody("Tent", "placeholder"))
	claimed, err := r.cfg.Relay.Claim(t.Context(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := SpoolEntry{Request: claimed, Note: Request{Op: OpAdd, Title: "Tent", Body: "a\x00b"}, SpooledAt: time.Now().UTC()}
	if err := r.svc.spool.Put(e); err != nil {
		t.Fatal(err)
	}
	if n := r.svc.RetrySpooled(t.Context()); n != 1 {
		t.Fatalf("RetrySpooled tried %d", n)
	}
	res := r.get(t, "grokbot", req.ID)
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "invalid_request") {
		t.Fatalf("status %s reply %+v", res.Status, res.Reply)
	}
	r.assertSpoolEmpty(t)
}

// stuckThenRequeued spools an add the helper cannot apply, then lets the
// claim lease lapse so the relay puts the request back to queued.
func (r *rig) stuckThenRequeued(t *testing.T) envelope.Request {
	t.Helper()
	r.helper.failCreates(t, "operation_failed")
	req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent"))
	r.poll(t, 1)
	if len(r.spoolFiles(t)) != 1 {
		t.Fatalf("spool = %v", r.spoolFiles(t))
	}
	r.redeliver(t)
	r.helper.failCreates(t, "")
	return req
}

// Finding #2: a spooled add whose request the sender cancelled after the
// lease lapsed is dropped on retry without calling the helper.
func TestSpooledAddCancelledBySenderIsDropped(t *testing.T) {
	r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
	req := r.stuckThenRequeued(t)
	if err := r.mesh.Client(t, "grokbot").Cancel(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	before := len(r.helper.callsOf(t, "create"))
	if n := r.svc.RetrySpooled(t.Context()); n != 1 {
		t.Fatalf("RetrySpooled tried %d", n)
	}
	if after := len(r.helper.callsOf(t, "create")); after != before {
		t.Fatalf("helper create ran for a cancelled add: %d -> %d", before, after)
	}
	if len(r.helper.notes()) != 0 {
		t.Fatalf("a cancelled add was written: %+v", r.helper.notes())
	}
	r.assertSpoolEmpty(t)
	if !strings.Contains(r.log.String(), "cancelled") {
		t.Fatalf("the drop was not logged:\n%s", r.log.String())
	}
}

// A requeued (not cancelled) spooled add is still written on retry.
func TestSpooledAddRequeuedIsStillWritten(t *testing.T) {
	r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
	r.stuckThenRequeued(t)
	r.svc.RetrySpooled(t.Context())
	if len(r.helper.notes()) != 1 {
		t.Fatalf("notes = %+v, want the add written", r.helper.notes())
	}
}

// When the relay cannot be reached to check the request, the add is kept
// and not written this round.
func TestSpooledAddKeptWhenStatusUnreadable(t *testing.T) {
	r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
	r.stuckThenRequeued(t)
	r.getFail.Store(true)
	before := len(r.helper.callsOf(t, "create"))
	r.svc.RetrySpooled(t.Context())
	if after := len(r.helper.callsOf(t, "create")); after != before {
		t.Fatalf("helper create ran without a status check: %d -> %d", before, after)
	}
	if len(r.spoolFiles(t)) != 1 {
		t.Fatalf("spool = %v, want the entry kept", r.spoolFiles(t))
	}
	r.getFail.Store(false)
	r.svc.RetrySpooled(t.Context())
	if len(r.helper.notes()) != 1 {
		t.Fatalf("notes = %+v after the relay came back", r.helper.notes())
	}
}

// A panic after the add is spooled does not answer it failed: the entry
// stays, and the retry saves it and answers with the note id.
func TestPanicAfterSpoolLeavesAddForRetry(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.log.panicOn = "add applied as note"
	req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent"))
	r.poll(t, 1)
	if res := r.get(t, "grokbot", req.ID); res.Reply != nil {
		t.Fatalf("answered after a panic with the add spooled: %+v", res.Reply)
	}
	if len(r.spoolFiles(t)) != 1 {
		t.Fatalf("spool = %v, want the entry kept", r.spoolFiles(t))
	}
	r.svc.RetrySpooled(t.Context())
	res := r.get(t, "grokbot", req.ID)
	notes := r.helper.notes()
	if res.Status != envelope.StatusAnswered || len(notes) != 1 || !strings.Contains(res.Reply.Body, notes[0].ID) {
		t.Fatalf("status %s reply %+v notes %+v", res.Status, res.Reply, notes)
	}
	r.assertSpoolEmpty(t)
}

// A panic before anything is spooled is still answered failed.
func TestPanicBeforeSpoolAnswersFailed(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.log.panicOn = "structured search"
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "internal error") {
		t.Fatalf("status %s reply %+v", res.Status, res.Reply)
	}
}

func readHealth(t *testing.T, path string) Health {
	t.Helper()
	var h Health
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatalf("health %s: %v", b, err)
	}
	return h
}

// helper_too_old sticks: a later search or read does not clear it; only
// a create answered with "existed" does, and it survives a restart.
func TestHelperTooOldIsStickyUntilIdempotentCreate(t *testing.T) {
	r := newRig(t, relay.Config{})
	writeFile(t, filepath.Join(r.helper.state, "old-helper"), "")
	r.ask(t, "grokbot", addBody("Tent", "blue"))
	if h := readHealth(t, r.cfg.HealthPath); !h.IdempotencyUnsupported {
		t.Fatalf("health = %+v, want idempotency unsupported", h)
	}
	r.restart(t)
	r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	if h := readHealth(t, r.cfg.HealthPath); !h.OK || h.Command != "search" || !h.IdempotencyUnsupported {
		t.Fatalf("health after a search = %+v, want the flag kept", h)
	}
	if err := os.Remove(filepath.Join(r.helper.state, "old-helper")); err != nil {
		t.Fatal(err)
	}
	r.ask(t, "grokbot", addBody("Stove", "fuel"))
	if h := readHealth(t, r.cfg.HealthPath); !h.OK || h.IdempotencyUnsupported {
		t.Fatalf("health after an idempotent create = %+v, want the flag cleared", h)
	}
}

// A sender removed from the add allowlist while its add sits in the spool
// gets the declined reply a new request would, and nothing is written.
func TestSpooledAddOfRevokedSenderIsDeclined(t *testing.T) {
	for _, via := range []string{"retry", "redelivery"} {
		t.Run(via, func(t *testing.T) {
			r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
			req := r.stuckThenRequeued(t)
			writeFile(t, r.cfg.AddAllowlistPath, "muse\n")
			before := len(r.helper.callsOf(t, "create"))
			if via == "retry" {
				r.svc.RetrySpooled(t.Context())
			} else {
				r.poll(t, 1)
			}
			if after := len(r.helper.callsOf(t, "create")); after != before {
				t.Fatalf("helper create ran for a revoked sender: %d -> %d", before, after)
			}
			if len(r.helper.notes()) != 0 {
				t.Fatalf("a revoked sender's add was written: %+v", r.helper.notes())
			}
			res := r.get(t, "grokbot", req.ID)
			if res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "grokbot is not on the notes add allowlist") {
				t.Fatalf("status %s reply %+v", res.Status, res.Reply)
			}
			r.assertSpoolEmpty(t)
			if !strings.Contains(r.log.String(), "no longer allowed to add") {
				t.Fatalf("the decline was not logged:\n%s", r.log.String())
			}
		})
	}
}

// An add allowlist that cannot be read keeps a spooled add and writes
// nothing this round; once the list is fixed, the retry writes it.
func TestSpooledAddKeptWhenAddAllowlistUnreadable(t *testing.T) {
	r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
	r.stuckThenRequeued(t)
	writeFile(t, r.cfg.AddAllowlistPath, "bad!name\n")
	before := len(r.helper.callsOf(t, "create"))
	r.svc.RetrySpooled(t.Context())
	if after := len(r.helper.callsOf(t, "create")); after != before {
		t.Fatalf("helper create ran with an unreadable add allowlist: %d -> %d", before, after)
	}
	if len(r.spoolFiles(t)) != 1 {
		t.Fatalf("spool = %v, want the entry kept", r.spoolFiles(t))
	}
	writeFile(t, r.cfg.AddAllowlistPath, "grokbot\n")
	r.svc.RetrySpooled(t.Context())
	if len(r.helper.notes()) != 1 {
		t.Fatalf("notes = %+v after the list was fixed", r.helper.notes())
	}
	r.assertSpoolEmpty(t)
}

// Duplicate protection is verified only by a create answered with
// "existed": a search alone leaves it unverified, helper_too_old clears
// it, and it carries across later searches.
func TestIdempotencyVerifiedOnlyByCreate(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	if h := readHealth(t, r.cfg.HealthPath); !h.OK || h.IdempotencyVerified {
		t.Fatalf("health after a search = %+v, want unverified", h)
	}
	r.ask(t, "grokbot", addBody("Tent", "blue"))
	if h := readHealth(t, r.cfg.HealthPath); !h.IdempotencyVerified || h.IdempotencyUnsupported {
		t.Fatalf("health after a create = %+v, want verified", h)
	}
	r.restart(t)
	r.ask(t, "grokbot", `note: {"op":"search","query":"tent"}`)
	if h := readHealth(t, r.cfg.HealthPath); h.Command != "search" || !h.IdempotencyVerified {
		t.Fatalf("health after a later search = %+v, want verified kept", h)
	}
	writeFile(t, filepath.Join(r.helper.state, "old-helper"), "")
	r.ask(t, "grokbot", addBody("Stove", "fuel"))
	if h := readHealth(t, r.cfg.HealthPath); h.IdempotencyVerified || !h.IdempotencyUnsupported {
		t.Fatalf("health after helper_too_old = %+v, want verified cleared", h)
	}
}

// Once a spooled add is saved, its answered reply is the final decision:
// removing the sender from the add allowlist before the reply lands does
// not turn it into a decline, and the helper is not called again.
func TestSavedAddReplyFailsThenSenderRemovedStillAnswered(t *testing.T) {
	for _, via := range []string{"retry", "redelivery"} {
		t.Run(via, func(t *testing.T) {
			r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
			r.replyFail.Store(true)
			req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent"))
			r.poll(t, 1)
			notes := r.helper.notes()
			if len(notes) != 1 || len(r.spoolFiles(t)) != 1 {
				t.Fatalf("notes %+v spool %v, want one saved note kept spooled", notes, r.spoolFiles(t))
			}
			writeFile(t, r.cfg.AddAllowlistPath, "muse\n")
			r.replyFail.Store(false)
			if via == "retry" {
				r.svc.RetrySpooled(t.Context())
			} else {
				r.redeliver(t)
				r.poll(t, 1)
			}
			res := r.get(t, "grokbot", req.ID)
			if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, notes[0].ID) {
				t.Fatalf("status %s reply %+v, want answered with %s", res.Status, res.Reply, notes[0].ID)
			}
			if n := len(r.helper.callsOf(t, "create")); n != 1 {
				t.Fatalf("creates = %d, want the recorded reply resent without the helper", n)
			}
			r.assertSpoolEmpty(t)
		})
	}
}

// A declined spooled add stays declined: if its reply does not reach the
// relay, or reaches it but the response is lost, restoring the sender's
// add access later never saves the note.
func TestDeclinedAddNeverSavedAfterAccessRestored(t *testing.T) {
	for _, mode := range []string{"reply failed", "response lost"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
			req := r.stuckThenRequeued(t)
			writeFile(t, r.cfg.AddAllowlistPath, "muse\n")
			lose := &r.replyFail
			if mode == "response lost" {
				lose = &r.replyLose
			}
			lose.Store(true)
			r.svc.RetrySpooled(t.Context())
			if len(r.spoolFiles(t)) != 1 {
				t.Fatalf("spool = %v, want the declined add kept until its reply lands", r.spoolFiles(t))
			}
			lose.Store(false)
			writeFile(t, r.cfg.AddAllowlistPath, "grokbot\n")
			before := len(r.helper.callsOf(t, "create"))
			r.svc.RetrySpooled(t.Context())
			if after := len(r.helper.callsOf(t, "create")); after != before {
				t.Fatalf("helper create ran for a declined add: %d -> %d", before, after)
			}
			if len(r.helper.notes()) != 0 {
				t.Fatalf("a declined add was saved: %+v", r.helper.notes())
			}
			res := r.get(t, "grokbot", req.ID)
			if res.Status != envelope.StatusDeclined || !strings.Contains(res.Reply.Body, "grokbot is not on the notes add allowlist") {
				t.Fatalf("status %s reply %+v", res.Status, res.Reply)
			}
			r.assertSpoolEmpty(t)
		})
	}
}

// putOldEntry writes a spool file as a build before recorded outcomes
// did, for a request notes has claimed.
func (r *rig) putOldEntry(t *testing.T, claimed envelope.Request) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"request":    claimed,
		"note":       Request{Op: OpAdd, Title: "Tent", Body: "buy the blue tent"},
		"spooled_at": time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.cfg.SpoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.cfg.SpoolDir, claimed.ID+".json"), string(b))
}

// An undecided spooled add whose request the relay already shows closed
// by a reply is dropped on retry: the helper is not called and nothing is
// resent.
func TestSpooledAddAlreadyRepliedIsDropped(t *testing.T) {
	for _, status := range []envelope.Status{envelope.StatusAnswered, envelope.StatusDeclined, envelope.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			r := newRig(t, relay.Config{})
			req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent"))
			claimed, err := r.cfg.Relay.Claim(t.Context(), req.ID)
			if err != nil {
				t.Fatal(err)
			}
			r.putOldEntry(t, claimed)
			if _, err := r.cfg.Relay.Reply(t.Context(), req.ID, "earlier reply", status); err != nil {
				t.Fatal(err)
			}
			r.svc.RetrySpooled(t.Context())
			if n := len(r.helper.callsOf(t, "create")); n != 0 {
				t.Fatalf("creates = %d, want none for a request already replied to", n)
			}
			r.assertSpoolEmpty(t)
			if res := r.get(t, "grokbot", req.ID); res.Status != status || res.Reply.Body != "earlier reply" {
				t.Fatalf("status %s reply %+v", res.Status, res.Reply)
			}
			if !strings.Contains(r.log.String(), "already has a reply") {
				t.Fatalf("the drop was not logged:\n%s", r.log.String())
			}
		})
	}
}

// A spool file written before outcomes were recorded is still applied.
func TestOldFormatSpoolEntryIsApplied(t *testing.T) {
	r := newRig(t, relay.Config{})
	req := r.send(t, "grokbot", addBody("Tent", "buy the blue tent"))
	claimed, err := r.cfg.Relay.Claim(t.Context(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.putOldEntry(t, claimed)
	r.svc.RetrySpooled(t.Context())
	res := r.get(t, "grokbot", req.ID)
	notes := r.helper.notes()
	if res.Status != envelope.StatusAnswered || len(notes) != 1 || !strings.Contains(res.Reply.Body, notes[0].ID) {
		t.Fatalf("status %s reply %+v notes %+v", res.Status, res.Reply, notes)
	}
	r.assertSpoolEmpty(t)
}
