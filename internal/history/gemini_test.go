package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// geminiFake answers from the gemini fixtures; files are PNGs whose
// content encodes the file id and the conversation it was read from.
func geminiFake(t *testing.T) *fakeChannel {
	return &fakeChannel{handle: func(req NativeRequest) ([]NativeResponse, error) {
		if err := ValidateOp(req.Op, req.Args); err != nil {
			return []NativeResponse{{Error: &NativeError{Code: "bad_request", Message: err.Error()}}}, nil
		}
		switch req.Op {
		case OpGeminiList:
			return []NativeResponse{{OK: true, Result: fixture(t, "gemini/list.json")}}, nil
		case OpGeminiDetail:
			b, err := os.ReadFile("testdata/gemini/conversation-" + req.Args.ID + ".json")
			if err != nil {
				return []NativeResponse{{Error: &NativeError{Code: "not_found", Message: "no such conversation"}}}, nil
			}
			return []NativeResponse{{OK: true, Result: b}}, nil
		case OpGeminiFile:
			return chunkFrames(append(fakePNG(64), req.Args.ConversationID+"/"+req.Args.FileID...), "image/png", 40), nil
		}
		return nil, errors.New("unexpected op")
	}}
}

func newTestGemini(ch Channel) *Gemini {
	r := NewGemini(&Client{Channel: ch, Timeout: 2 * time.Second, Cooldown: &SiteCooldown{}})
	r.Now = func() time.Time { return liveNow }
	r.AgentChats = ""
	return r
}

// The list fixture is two MaZiqc pages: every conversation on both is
// read, newest first, with canonical ids; a malformed id and a repeat
// across pages are skipped.
func TestGeminiListReadsEveryPage(t *testing.T) {
	r := newTestGemini(geminiFake(t))
	convs, err := convsOf(r.List(context.Background(), 10, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(convs); got != "00000000000000a1,00000000000000a2,00000000000000a3" {
		t.Fatalf("ids %s", got)
	}
	if convs[0].Title != "Tide pool photo" || convs[0].Source != SourceGemini {
		t.Fatalf("first %+v", convs[0])
	}
	if !convs[1].UpdatedAt.Equal(time.Date(2026, 9, 22, 10, 0, 0, 5e8, time.UTC)) {
		t.Fatalf("updated %v", convs[1].UpdatedAt)
	}
	if convs[2].Title != "Sourdough starter" {
		t.Fatalf("second page %+v", convs[2])
	}
}

func TestGeminiLatestWithImages(t *testing.T) {
	fake := geminiFake(t)
	r := newTestGemini(fake)
	convs, err := convsOf(r.Read(context.Background(), Query{Source: SourceGemini, Mode: ModeLatest, WantImages: true}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || len(convs[0].Messages) != 2 || convs[0].ID != "00000000000000a1" {
		t.Fatalf("latest %+v", convs)
	}
	u, a := convs[0].Messages[0], convs[0].Messages[1]
	if u.Text != "draw an ochre sea star" || !u.Time.Equal(time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("prompt %+v", u)
	}
	if a.Text != "Here is your sea star." {
		t.Fatalf("reply %q (the image placeholder must be dropped)", a.Text)
	}
	if got := imageSHAs(convs[0], RoleAssistant); len(got) != 1 || got[0] != "00000000000000a1/rc_00000000000000b2-0" {
		t.Fatalf("reply image %v", got)
	}
}

// Conversation mode accepts the id as the URL shows it and as
// batchexecute carries it; the turns come back oldest first with the
// chosen answer of each.
func TestGeminiSearchAndConversation(t *testing.T) {
	r := newTestGemini(geminiFake(t))
	convs, err := convsOf(r.Read(context.Background(), Query{Source: SourceGemini, Mode: ModeSearch, Terms: []string{"Portland"}}, Options{}))
	if err != nil || ids(convs) != "00000000000000a2" || convs[0].Messages[1].Text != "Rain jacket, boots, umbrella." {
		t.Fatalf("search %+v %v", convs, err)
	}
	for _, id := range []string{"00000000000000a1", "c_00000000000000a1", "00000000000000A1"} {
		convs, err := convsOf(r.Read(context.Background(), Query{Source: SourceGemini, Mode: ModeConversation, ConversationID: id}, Options{}))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		var texts []string
		for _, m := range convs[0].Messages {
			texts = append(texts, m.Text)
		}
		if got := strings.Join(texts, "|"); got != "what lives in tide pools|Anemones, hermit crabs and sea stars.|draw an ochre sea star|Here is your sea star." {
			t.Fatalf("%s: conversation %q", id, got)
		}
		if convs[0].ID != "00000000000000a1" {
			t.Fatalf("%s: id %q", id, convs[0].ID)
		}
	}
	_, err = convsOf(r.Read(context.Background(), Query{Source: SourceGemini, Mode: ModeConversation, ConversationID: "00000000000000ff"}, Options{}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	_, err = convsOf(r.Read(context.Background(), Query{Source: SourceGemini, Mode: ModeConversation, ConversationID: "not-hex"}, Options{}))
	if err == nil {
		t.Fatal("a non-Gemini id was read")
	}
}

// History leaves out the conversations gemini-web sent into unless all is
// asked for.
func TestGeminiSkipsWebAgentConversations(t *testing.T) {
	used := filepath.Join(t.TempDir(), "web-agent-gemini-conversations.json")
	if err := recordWebUsed(used, "00000000000000a1", liveNow); err != nil {
		t.Fatal(err)
	}
	r := newTestGemini(geminiFake(t))
	r.AgentChats = used
	ctx := context.Background()
	convs, err := convsOf(r.Read(ctx, Query{Source: SourceGemini, Mode: ModeLatest}, Options{}))
	if err != nil || ids(convs) != "00000000000000a2" || convs[0].Automated {
		t.Fatalf("latest skips the web agent's chat: %+v %v", convs, err)
	}
	convs, err = convsOf(r.Read(ctx, Query{Source: SourceGemini, Mode: ModeLatest}, Options{All: true}))
	if err != nil || ids(convs) != "00000000000000a1" || !convs[0].Automated {
		t.Fatalf("latest with all: %+v %v", convs, err)
	}
	list, err := convsOf(r.List(ctx, 10, Options{}))
	if err != nil || ids(list) != "00000000000000a2,00000000000000a3" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if DefaultWebUsedPath(SourceGemini) != configPath("", "web-agent-gemini-conversations.json") {
		t.Fatal("gemini used list path")
	}
}

// An answer the reader does not recognize is endpoint_changed, never a
// crash: the named shapes, and every fixture position replaced by
// something else.
func TestGeminiUnexpectedShapes(t *testing.T) {
	for _, raw := range []string{`{}`, `{"pages":"x"}`, `{"pages":[{"a":1}]}`, `{"pages":[[null,null,"x"]]}`, `{"pages":[[null,null,[5]]]}`, `{"pages":[[null,null,[[7]]]]}`, `[]`,
		// Entries, but no "c_" + hex id in any: the id format changed.
		`{"pages":[[null,null,[["x_00000000000000a1","t"]]]]}`, `{"pages":[[null,null,[["c_not-hex","t"],["r_00000000000000a1","t"]]]]}`} {
		if _, err := parseGeminiList(json.RawMessage(raw)); err == nil {
			t.Errorf("list %s parsed", raw)
		}
	}
	// No entries at all is an empty history, not a changed endpoint.
	if convs, err := parseGeminiList(json.RawMessage(`{"pages":[[null,null,[]]]}`)); err != nil || len(convs) != 0 {
		t.Errorf("empty list: %+v %v", convs, err)
	}
	for _, raw := range []string{`{}`, `"x"`, `[5]`, `[["turn"]]`, `[[[["c_x"]]]]`, `[[[["c_x","r_1"],null,null]]]`, `[[[["c_x","r_1"],null,[["hi"]],[5]]]]`, `[[[["c_x","r_1"],null,[["hi"]],[[[5]]]]]]`,
		// The chosen candidate is named but not among the candidates: the
		// first one may be a draft, so none is taken for the answer.
		`[[[["c_x","r_1"],null,[["hi"]],[[["rc_1",["a draft"]],["rc_2",["another draft"]]],null,null,"rc_9"]]]]`} {
		if _, err := parseGeminiDetail("00000000000000a1", json.RawMessage(raw)); err == nil {
			t.Errorf("detail %s parsed", raw)
		}
		if _, err := geminiNodes(json.RawMessage(raw)); err == nil {
			t.Errorf("nodes %s parsed", raw)
		}
	}
	for _, name := range []string{"list.json", "conversation-00000000000000a1.json"} {
		var v any
		if err := json.Unmarshal(fixture(t, "gemini/"+name), &v); err != nil {
			t.Fatal(err)
		}
		n := 0
		mutate(v, func(root any) {
			n++
			b, _ := json.Marshal(root)
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("%s variant %d panicked: %v\n%s", name, n, p, b)
					}
				}()
				_, _ = parseGeminiList(b)
				_, _ = parseGeminiDetail("00000000000000a1", b)
				_, _ = geminiNodes(b)
			}()
		}, v)
		if n < 50 {
			t.Fatalf("%s: only %d variants", name, n)
		}
	}

	ch := frames(NativeResponse{OK: true, Result: json.RawMessage(`{"pages":[[null,null,"x"]]}`)})
	_, err := convsOf(newTestGemini(ch).List(context.Background(), 5, Options{}))
	if !errors.Is(err, ErrEndpointChanged) || err.Error() != "source unavailable: gemini: gemini.google.com changed its API (unexpected list shape)" {
		t.Fatalf("list err %v", err)
	}
}

// mutate calls visit with root after replacing each array element of v, in
// turn, with null, a string, a number and an object, restoring it after.
func mutate(v any, visit func(root any), root any) {
	a, ok := v.([]any)
	if !ok {
		if m, ok := v.(map[string]any); ok {
			for _, e := range m {
				mutate(e, visit, root)
			}
		}
		return
	}
	for i := range a {
		orig := a[i]
		for _, repl := range []any{nil, "x", 5.0, map[string]any{"k": 1.0}, []any{}} {
			a[i] = repl
			visit(root)
		}
		a[i] = orig
		mutate(orig, visit, root)
	}
}

// Conversation ids and URLs in every Gemini form become the canonical hex.
func TestGeminiIDForms(t *testing.T) {
	for ref, want := range map[string]string{
		"https://gemini.google.com/app/00000000000000d2":                     "00000000000000d2",
		"https://gemini.google.com/u/1/app/00000000000000d2":                 "00000000000000d2",
		"https://gemini.google.com/gem/coding-partner/00000000000000d2":      "00000000000000d2",
		"https://gemini.google.com/u/2/gem/coding-partner/00000000000000d2/": "00000000000000d2",
		"https://gemini.google.com/app":                                      "",
		"https://gemini.google.com/app/not-hex":                              "",
		"http://gemini.google.com/app/00000000000000d2":                      "",
		"https://evil.example.com/app/00000000000000d2":                      "",
	} {
		id, ok := conversationRef(ref)
		if ok {
			id, ok = siteFor(SourceGemini).canonical(id)
		}
		if ok != (want != "") || id != want {
			t.Errorf("%s: %q %v, want %q", ref, id, ok, want)
		}
	}
	for in, want := range map[string]string{"c_00000000000000D2": "00000000000000d2", "00000000000000d2": "00000000000000d2", "abc-1": "", "c_": "", "": ""} {
		got, ok := geminiCanonicalID(in)
		if ok != (want != "") || got != want {
			t.Errorf("canonical %q = %q %v, want %q", in, got, ok, want)
		}
	}
	op, args, ok := geminiFileArgs("00000000000000d2", geminiImagePointer("rc_00000000000000b9", 1))
	if !ok || op != OpGeminiFile || args.FileID != "rc_00000000000000b9-1" || args.ConversationID != "00000000000000d2" || ValidateOp(op, args) != nil {
		t.Fatalf("file args %s %+v %v", op, args, ok)
	}
	for _, p := range []string{"gemini-image://rc_1/x", "gemini-image://../1", "claudeai-file://x", "gemini-image://rc_1"} {
		if _, _, ok := geminiFileArgs("00000000000000d2", p); ok {
			t.Errorf("pointer %q accepted", p)
		}
	}
}

// ---- The gemini-web agent.

const gemConv = "00000000000000d1"

// gemT is one turn of a hand-built hNvQHb answer. rc "" means Gemini has
// not started an answer.
type gemT struct {
	rid, prompt, rc, reply string
	images                 []string
	at                     time.Time
}

// gemDetail renders turns as hNvQHb's inner payload, newest first as the
// site lists them.
func gemDetail(conv string, turns ...gemT) json.RawMessage {
	var list []any
	for _, t := range slices.Backward(turns) {
		var answer any
		if t.rc != "" {
			var imgs []any
			for _, u := range t.images {
				imgs = append(imgs, []any{nil, nil, nil, []any{nil, 1, "image.png", u}})
			}
			cand := []any{t.rc, []any{t.reply}, []any{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, []any{nil, nil, nil, nil, nil, nil, nil, []any{imgs}}}
			answer = []any{[]any{cand}, nil, nil, t.rc}
		}
		list = append(list, []any{[]any{"c_" + conv, t.rid}, nil, []any{[]any{t.prompt}, 1, nil, 0, "dummy", 0}, answer, []any{t.at.Unix(), t.at.Nanosecond()}})
	}
	b, _ := json.Marshal([]any{list, nil, nil})
	return b
}

// gemRig is a web agent for Gemini whose extension answers from a
// script: detail(n, sentAt) is the n-th detail read, file answers
// gemini.file. It records ops, send arguments and closed conversations.
type gemRig struct {
	*webRig
	clock  *fakeClock
	mu     sync.Mutex
	ops    []Op
	sends  []OpArgs
	files  []OpArgs
	closes []string
	// sendID is what the send answers as the conversation id (the id
	// asked for, or gemConv for a new chat, when empty).
	sendID  string
	sendErr *NativeError
}

func newGemRig(t *testing.T, detail func(n int, sentAt time.Time) (json.RawMessage, *NativeError), file func(OpArgs) []NativeResponse) *gemRig {
	t.Helper()
	g := &gemRig{webRig: newWebRig(t)}
	g.agent.Site = SourceGemini
	g.agent.Name = "gemini-web"
	g.agent.ClaudeStableFor = time.Nanosecond
	var sentAt time.Time
	polls := 0
	g.agent.Native = &Client{Cooldown: &SiteCooldown{}, Channel: channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		g.mu.Lock()
		g.ops = append(g.ops, req.Op)
		g.mu.Unlock()
		if err := ValidateOp(req.Op, req.Args); err != nil {
			_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request", Message: err.Error()}})
			return err
		}
		now := time.Now
		if g.clock != nil {
			now = g.clock.Now
		}
		switch req.Op {
		case OpGeminiSend:
			g.mu.Lock()
			g.sends = append(g.sends, req.Args)
			serr := g.sendErr
			sentAt = now()
			id := g.sendID
			g.mu.Unlock()
			if serr != nil {
				_, err := recv(NativeResponse{ID: req.ID, Error: serr})
				return err
			}
			if id == "" {
				id = req.Args.ConversationID
			}
			if id == "" {
				id = gemConv
			}
			res, _ := json.Marshal(SendResult{ConversationID: id, URL: "https://gemini.google.com/app/" + strings.TrimPrefix(id, "c_"), SubmittedAt: sentAt.UnixMilli()})
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: res})
			return err
		case OpGeminiDetail:
			g.mu.Lock()
			n, at := polls, sentAt
			polls++
			g.mu.Unlock()
			raw, nerr := detail(n, at)
			if nerr != nil {
				_, err := recv(NativeResponse{ID: req.ID, Error: nerr})
				return err
			}
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: raw})
			return err
		case OpGeminiFile:
			g.mu.Lock()
			g.files = append(g.files, req.Args)
			g.mu.Unlock()
			for _, fr := range file(req.Args) {
				fr.ID = req.ID
				if done, err := recv(fr); done || err != nil {
					return err
				}
			}
			return nil
		case OpGeminiClose:
			g.mu.Lock()
			g.closes = append(g.closes, req.Args.ConversationID)
			g.mu.Unlock()
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(`{"closed":1}`)})
			return err
		}
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request", Message: "unexpected op"}})
		return err
	})}
	return g
}

// onFakeClock runs the rig's reply wait on a fake clock at the real poll
// cadence and the site's own stability windows.
func (g *gemRig) onFakeClock() {
	g.clock = newFakeClock()
	g.agent.clock = g.clock
	g.agent.PollInterval = 0
	g.agent.ClaudeStableFor = 0
	g.agent.Native.Cooldown.Now = g.clock.Now
}

func (g *gemRig) count(op Op) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, o := range g.ops {
		if o == op {
			n++
		}
	}
	return n
}

func (g *gemRig) closed() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.closes...)
}

func noFile(OpArgs) []NativeResponse {
	return []NativeResponse{{Error: &NativeError{Code: "bad_request", Message: "no file expected"}}}
}

// A new chat: the answer appears, grows, then holds still; it comes back
// with the image Gemini drew attached and the Gemini conversation footer,
// and every store holds the canonical id.
// Gemini's placeholder can carry a suffix after the image index
// ("image_generation_content/1_733"); none of it may leak into the reply.
func TestCleanGeminiTextPlaceholders(t *testing.T) {
	for in, want := range map[string]string{
		"Here is your fox.\nhttp://googleusercontent.com/image_generation_content/0": "Here is your fox.",
		"\n\nhttp://googleusercontent.com/image_generation_content/1_733\n\n":        "",
		"A dog: http://googleusercontent.com/image_generation_content/2_15 done":     "A dog: done",
	} {
		if got := cleanGeminiText(in); got != want {
			t.Errorf("cleanGeminiText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebGeminiAnswerWithImage(t *testing.T) {
	img := fakePNG(3000)
	const url = "https://lh3.googleusercontent.com/gg/dummy-fox-1"
	rig := newGemRig(t, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		turn := gemT{rid: "r_00000000000000e1", prompt: "Draw a fox logo", at: at}
		switch n {
		case 0:
			return gemDetail(gemConv), nil
		case 1:
		case 2:
			turn.rc, turn.reply = "rc_00000000000000f1", "Here is"
		default:
			turn.rc, turn.reply, turn.images = "rc_00000000000000f1", "Here is your fox.\nhttp://googleusercontent.com/image_generation_content/0", []string{url}
		}
		return gemDetail(gemConv, turn), nil
	}, func(a OpArgs) []NativeResponse { return chunkFrames(img, "image/png", 1<<10) })
	res := rig.ask(t, "grokbot", "Draw a fox logo")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	body := res.Reply.Body
	for _, want := range []string{"Here is your fox.", "Gemini conversation: " + gemConv, "1 image attached."} {
		if !strings.Contains(body, want) {
			t.Errorf("reply missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "googleusercontent.com") || strings.Contains(body, "could not be attached") {
		t.Errorf("reply carries the placeholder or a failure note:\n%s", body)
	}
	if len(rig.files) != 1 || rig.files[0].FileID != "rc_00000000000000f1-0" || rig.files[0].ConversationID != gemConv {
		t.Fatalf("file ops %+v", rig.files)
	}
	if got := rig.closed(); len(got) != 1 || got[0] != gemConv {
		t.Fatalf("closes %v", got)
	}
	if len(res.Reply.Attachments) != 1 {
		t.Fatalf("attachments %+v", res.Reply.Attachments)
	}
	data, _, err := rig.mesh.Client(t, "grokbot").FetchAttachment(t.Context(), res.Reply.Attachments[0].ID)
	if err != nil || !bytes.Equal(data, img) {
		t.Fatalf("attachment %d bytes %v", len(data), err)
	}
	if used := loadWebUsed(rig.agent.UsedPath); !used[gemConv] {
		t.Fatalf("used list %v", used)
	}
	if st := rig.agent.loadState(); st.Conversations["grokbot"].ID != gemConv {
		t.Fatalf("state %+v", st)
	}
}

// The image could not be captured from the page or fetched by the
// worker: the reply is the text with a note saying so.
func TestWebGeminiImageCaptureFailureNotes(t *testing.T) {
	rig := newGemRig(t, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		return gemDetail(gemConv, gemT{rid: "r_1", prompt: "Draw a fox logo", rc: "rc_1", reply: "Here is your fox.", images: []string{"https://lh3.googleusercontent.com/gg/dummy-fox-2"}, at: at}), nil
	}, func(OpArgs) []NativeResponse {
		return []NativeResponse{{Error: &NativeError{Code: "http_error", Message: "HTTP 403 from /gg/dummy-fox-2"}}}
	})
	res := rig.ask(t, "grokbot", "Draw a fox logo")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "Here is your fox.") || !strings.Contains(body, "Gemini's reply had 1 image that could not be attached") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if len(res.Reply.Attachments) != 0 || strings.Contains(body, "attached.") && !strings.Contains(body, "could not be attached") {
		t.Fatalf("attachments %+v body %q", res.Reply.Attachments, body)
	}
	if !strings.HasSuffix(body, "Gemini conversation: "+gemConv) {
		t.Fatalf("footer: %q", body)
	}
}

// "conversation:" takes a Gemini conversation in any of its forms; the
// send, the footer and the asker's state all get the canonical id. An id
// from another site is refused without a send.
func TestWebGeminiConversationForms(t *testing.T) {
	const conv = "00000000000000d2"
	for _, ref := range []string{
		"https://gemini.google.com/app/" + conv,
		"https://gemini.google.com/u/1/app/" + conv,
		"https://gemini.google.com/gem/coding-partner/" + conv,
		"c_00000000000000D2",
		conv,
	} {
		rig := newGemRig(t, func(n int, at time.Time) (json.RawMessage, *NativeError) {
			earlier := gemT{rid: "r_0", prompt: "say hello", rc: "rc_0", reply: "Hello.", at: time.Now().Add(-time.Hour)}
			if at.IsZero() {
				// The read before the send: only the earlier turn.
				return gemDetail(conv, earlier), nil
			}
			return gemDetail(conv, earlier, gemT{rid: "r_1", prompt: "and now in French", rc: "rc_1", reply: "Et maintenant.", at: at}), nil
		}, noFile)
		// The extension may answer in the batchexecute form; it is
		// canonicalized too.
		rig.sendID = "c_" + conv
		res := rig.ask(t, "grokbot", "conversation: "+ref+"\nand now in French")
		if res.Status != envelope.StatusAnswered || !strings.HasSuffix(res.Reply.Body, "Gemini conversation: "+conv) {
			t.Fatalf("%s: %s %q", ref, res.Status, res.Reply.Body)
		}
		if len(rig.sends) != 1 || rig.sends[0].ConversationID != conv || rig.sends[0].Message != "and now in French" {
			t.Fatalf("%s: sends %+v", ref, rig.sends)
		}
		if st := rig.agent.loadState(); st.Conversations["grokbot"].ID != conv {
			t.Fatalf("%s: state %+v", ref, st)
		}
		if used := loadWebUsed(rig.agent.UsedPath); !used[conv] || len(used) != 1 {
			t.Fatalf("%s: used %v", ref, used)
		}
		if c := rig.closed(); len(c) != 1 || c[0] != conv {
			t.Fatalf("%s: closes %v", ref, c)
		}
	}
	rig := newGemRig(t, func(int, time.Time) (json.RawMessage, *NativeError) { return nil, nil }, noFile)
	res := rig.ask(t, "grokbot", "conversation: https://chatgpt.com/c/abc-1\nhi")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, `"abc-1" is not a Gemini conversation id`) || rig.count(OpGeminiSend) != 0 {
		t.Fatalf("%s %q sends %d", res.Status, res.Reply.Body, rig.count(OpGeminiSend))
	}
}

// Gemini holds its text still for longer than claude.ai's window while it
// thinks: a pause of 25 seconds (reads 5s, 10s, 18s and 30s after the
// send see the same text) is not taken for the end, and the finished
// answer then has to hold for Gemini's own window.
func TestWebGeminiLongPauseIsNotCutEarly(t *testing.T) {
	rig := newGemRig(t, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		turn := gemT{rid: "r_1", prompt: "plan my week", rc: "rc_1", reply: "Let me think about your week.", at: at}
		if n >= 4 {
			turn.reply = "Monday: rest. Tuesday: run."
		}
		return gemDetail(gemConv, turn), nil
	}, noFile)
	rig.onFakeClock()
	res := rig.ask(t, "grokbot", "plan my week")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "Monday: rest. Tuesday: run.") {
		t.Fatalf("a paused answer was cut early: %s %q", res.Status, res.Reply.Body)
	}
	if DefaultGeminiStableFor <= DefaultClaudeStableFor || 25*time.Second < DefaultClaudeStableFor {
		t.Fatal("the pause in this test must outlast claude.ai's window and not Gemini's")
	}
	if r := rig.agent.stableRule(); r == nil || r.polls != DefaultGeminiStablePolls || r.span != DefaultGeminiStableFor {
		t.Fatalf("gemini rule %+v", r)
	}
}

// An answer still changing when the request's budget runs out ends with
// the timeout reply, and the tab is closed.
func TestWebGeminiStillChangingTimesOut(t *testing.T) {
	rig := newGemRig(t, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		return gemDetail(gemConv, gemT{rid: "r_1", prompt: "write forever", rc: "rc_1", reply: strings.Repeat("x", n+1), at: at}), nil
	}, noFile)
	rig.onFakeClock()
	res := rig.ask(t, "grokbot", "write forever")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "Gemini did not finish answering in time") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if c := rig.closed(); len(c) != 1 || c[0] != gemConv {
		t.Fatalf("closes %v", c)
	}
}

// Google's /sorry/ page: the reply names the anti-bot check, and Gemini
// alone is held back for a while; ChatGPT and claude.ai are not.
func TestWebGeminiBlockedCoolsDownGeminiOnly(t *testing.T) {
	rig := newGemRig(t, func(int, time.Time) (json.RawMessage, *NativeError) { return nil, nil }, noFile)
	rig.sendErr = &NativeError{Code: "blocked", Message: "anti-bot check (HTTP 429 from /sorry/index)"}
	res := rig.ask(t, "grokbot", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "gemini.google.com showed an anti-bot check") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	cd := rig.agent.Native
	if cd.CooldownRemaining(SourceGemini) <= 0 || cd.CooldownRemaining(SourceChatGPT) != 0 || cd.CooldownRemaining(SourceClaudeAI) != 0 {
		t.Fatalf("cooldowns gemini %s chatgpt %s claude %s", cd.CooldownRemaining(SourceGemini), cd.CooldownRemaining(SourceChatGPT), cd.CooldownRemaining(SourceClaudeAI))
	}
	res = rig.ask(t, "grokbot", "hello again")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "anti-bot check") || strings.Contains(res.Reply.Body, "rate-limiting") {
		t.Fatalf("during the cooldown: %s %q", res.Status, res.Reply.Body)
	}
	if n := rig.count(OpGeminiSend); n != 1 {
		t.Fatalf("%d sends; the cooldown must hold the second one back", n)
	}
	// Another site's blocked answer starts no cooldown, as before.
	other := &Client{Cooldown: &SiteCooldown{}, Channel: frames(NativeResponse{Error: &NativeError{Code: "blocked", Message: "cf"}})}
	if _, err := other.Request(context.Background(), OpChatGPTList, OpArgs{Count: 1}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("chatgpt blocked: %v", err)
	}
	if other.CooldownRemaining(SourceChatGPT) != 0 {
		t.Fatal("a ChatGPT anti-bot answer started a cooldown")
	}
}

// A send that failed after its click (the tab went to /sorry/ or a
// sign-in page once the button was clicked) may have posted the message:
// the reply keeps the cause and says so, so the asker does not simply
// send it again. A failure before the click says nothing of the kind.
func TestWebGeminiFailureAfterClickMayHaveSent(t *testing.T) {
	for _, tc := range []struct {
		code, cause string
	}{
		{"blocked", "gemini.google.com showed an anti-bot check"},
		{"not_logged_in", "not logged in to gemini.google.com"},
		{"send_failed", "the message could not be sent on gemini.google.com"},
	} {
		rig := newGemRig(t, func(int, time.Time) (json.RawMessage, *NativeError) { return nil, nil }, noFile)
		rig.sendErr = &NativeError{Code: tc.code, Message: "the page went to www.google.com", Clicked: true}
		res := rig.ask(t, "grokbot", "hello")
		body := res.Reply.Body
		if res.Status != envelope.StatusFailed || !strings.Contains(body, tc.cause) || !strings.Contains(body, "may have been sent") {
			t.Fatalf("%s after the click: %s %q", tc.code, res.Status, body)
		}
		rig = newGemRig(t, func(int, time.Time) (json.RawMessage, *NativeError) { return nil, nil }, noFile)
		rig.sendErr = &NativeError{Code: tc.code, Message: "the page went to www.google.com"}
		res = rig.ask(t, "grokbot", "hello")
		if body := res.Reply.Body; res.Status != envelope.StatusFailed || !strings.Contains(body, tc.cause) || strings.Contains(body, "may have been sent") {
			t.Fatalf("%s before the click: %s %q", tc.code, res.Status, body)
		}
	}
}

// A rate limit still wins the cooldown's reason when it runs longer, and
// the cooldown's kind follows the latest end.
func TestSiteCooldownKinds(t *testing.T) {
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	cd := &SiteCooldown{Now: func() time.Time { return now }}
	c := &Client{Cooldown: cd}
	if c.cooldownError(SourceGemini) != nil {
		t.Fatal("cooldown before any")
	}
	cd.NoteBlocked(SourceGemini, time.Minute)
	if err := c.cooldownError(SourceGemini); !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocked: %v", err)
	}
	cd.Note(SourceGemini, 30*time.Second)
	if err := c.cooldownError(SourceGemini); !errors.Is(err, ErrBlocked) {
		t.Fatalf("a shorter rate limit changed the kind: %v", err)
	}
	cd.Note(SourceGemini, 2*time.Minute)
	if _, ok := rateLimited(c.cooldownError(SourceGemini)); !ok {
		t.Fatalf("a longer rate limit: %v", c.cooldownError(SourceGemini))
	}
	now = now.Add(3 * time.Minute)
	if c.cooldownError(SourceGemini) != nil || cd.Blocked(SourceGemini) {
		t.Fatal("cooldown outlived its end")
	}
}

// A detail answer in a shape the reader does not know is endpoint_changed:
// the asker hears that Gemini changed its API, and the tab is closed.
func TestWebGeminiUnexpectedShapeIsEndpointChanged(t *testing.T) {
	rig := newGemRig(t, func(n int, at time.Time) (json.RawMessage, *NativeError) {
		return json.RawMessage(`{"surprise":true}`), nil
	}, noFile)
	res := rig.ask(t, "grokbot", "new chat\nhello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "gemini.google.com changed its API") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if c := rig.closed(); len(c) != 1 {
		t.Fatalf("closes %v", c)
	}
}

// The ops the Go side can send for Gemini are exactly the ones the
// extension lists (gemini.list, .detail, .file, .send, .close), with
// gemini.file carrying the conversation id.
func TestValidateGeminiOps(t *testing.T) {
	good := map[Op]OpArgs{
		OpGeminiList:   {Count: 5},
		OpGeminiDetail: {ID: gemConv},
		OpGeminiFile:   {FileID: "rc_1-0", ConversationID: gemConv},
		OpGeminiSend:   {Message: "hi", ConversationID: gemConv},
		OpGeminiClose:  {ConversationID: gemConv},
	}
	for op, a := range good {
		if err := ValidateOp(op, a); err != nil {
			t.Errorf("%s %+v: %v", op, a, err)
		}
	}
	if ValidateOp(OpGeminiFile, OpArgs{FileID: "https://lh3.googleusercontent.com/x"}) == nil {
		t.Fatal("a URL passed as a file id")
	}
}
