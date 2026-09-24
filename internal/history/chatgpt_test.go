package history

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePNG returns n bytes that sniff as image/png.
func fakePNG(n int) []byte {
	b := make([]byte, n)
	copy(b, "\x89PNG\r\n\x1a\n")
	for i := 8; i < n; i++ {
		b[i] = byte(i % 251)
	}
	return b
}

// chunkFrames splits data into base64 chunk frames the way the extension
// sends file bytes.
func chunkFrames(data []byte, mime string, size int) []NativeResponse {
	var out []NativeResponse
	for seq, off := 0, 0; ; seq++ {
		end := min(off+size, len(data))
		out = append(out, NativeResponse{
			OK:    true,
			MIME:  mime,
			Size:  int64(len(data)),
			Chunk: &NativeChunk{Seq: seq, Last: end == len(data), Data: base64.StdEncoding.EncodeToString(data[off:end])},
		})
		off = end
		if off == len(data) {
			return out
		}
	}
}

// fakeChannel stands in for the native host and extension.
type fakeChannel struct {
	mu     sync.Mutex
	calls  []NativeRequest
	handle func(NativeRequest) ([]NativeResponse, error)
}

func (f *fakeChannel) Exchange(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	frames, err := f.handle(req)
	if err != nil {
		return err
	}
	for _, fr := range frames {
		fr.ID = req.ID
		done, err := recv(fr)
		if err != nil || done {
			return err
		}
	}
	return errHostClosed
}

func (f *fakeChannel) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s []string
	for _, c := range f.calls {
		s = append(s, string(c.Op)+":"+c.Args.ID+c.Args.FileID)
	}
	return s
}

func fixture(t *testing.T, path string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/" + path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var liveNow = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// chatgptFake answers from the chatgpt fixtures; files are PNGs whose
// content encodes the file id.
func chatgptFake(t *testing.T) *fakeChannel {
	return &fakeChannel{handle: func(req NativeRequest) ([]NativeResponse, error) {
		if err := ValidateOp(req.Op, req.Args); err != nil {
			return []NativeResponse{{Error: &NativeError{Code: "bad_request", Message: err.Error()}}}, nil
		}
		switch req.Op {
		case OpChatGPTList:
			return []NativeResponse{{OK: true, Result: fixture(t, "chatgpt/conversations.json")}}, nil
		case OpChatGPTDetail:
			b, err := os.ReadFile("testdata/chatgpt/conversation-" + req.Args.ID + ".json")
			if err != nil {
				return []NativeResponse{{Error: &NativeError{Code: "not_found", Message: "404"}}}, nil
			}
			return []NativeResponse{{OK: true, Result: b}}, nil
		case OpChatGPTFile:
			return chunkFrames(append(fakePNG(64), req.Args.FileID...), "image/png", 40), nil
		}
		return nil, errors.New("unexpected op")
	}}
}

func newTestChatGPT(ch Channel) *ChatGPT {
	r := NewChatGPT(&Client{Channel: ch, Timeout: 2 * time.Second})
	r.Now = func() time.Time { return liveNow }
	return r
}

func TestChatGPTList(t *testing.T) {
	r := newTestChatGPT(chatgptFake(t))
	convs, err := r.List(context.Background(), 10, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 2 {
		t.Fatalf("want 2 inside the 30 day cap, got %+v", convs)
	}
	if convs[0].ID != "6a1f0c2e-1111-4a2b-9c3d-000000000001" || convs[0].Title != "Fox logo sketch" || convs[0].Source != SourceChatGPT {
		t.Fatalf("first %+v", convs[0])
	}
	if !convs[0].UpdatedAt.Equal(time.Date(2026, 9, 22, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("ISO update_time parsed as %v", convs[0].UpdatedAt)
	}
	if !convs[1].UpdatedAt.Equal(time.Date(2026, 9, 21, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("epoch update_time parsed as %v", convs[1].UpdatedAt)
	}
	if len(convs[0].Messages) != 0 {
		t.Fatal("list carries messages")
	}
}

func imageSHAs(c Conversation, role Role) []string {
	var s []string
	for _, m := range c.Messages {
		if m.Role == role {
			for _, im := range m.Images {
				s = append(s, string(im.Data[64:]))
			}
		}
	}
	return s
}

func TestChatGPTLatestWithImages(t *testing.T) {
	fake := chatgptFake(t)
	r := newTestChatGPT(fake)
	convs, err := r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeLatest, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("got %d conversations", len(convs))
	}
	c := convs[0]
	if c.Title != "Fox logo sketch" || len(c.Messages) != 2 {
		t.Fatalf("conv %+v", c)
	}
	u, a := c.Messages[0], c.Messages[1]
	if u.Role != RoleUser || u.Text != "make the fox ears bigger like this sketch" || !u.Time.Equal(time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("prompt %+v", u)
	}
	if a.Role != RoleAssistant || a.Text != "Here is the fox with bigger ears." {
		t.Fatalf("reply %+v", a)
	}
	if got := imageSHAs(c, RoleUser); len(got) != 1 || got[0] != "file-Sk3tchAbc123" {
		t.Fatalf("attached images %v", got)
	}
	if got := imageSHAs(c, RoleAssistant); len(got) != 1 || got[0] != "file-Gen3rated456" {
		t.Fatalf("generated images %v", got)
	}
	for _, m := range c.Messages {
		for _, im := range m.Images {
			if im.MIME != "image/png" || im.SHA256 == "" || im.Size == 0 || strings.Contains(im.Name, "://") {
				t.Fatalf("image metadata %+v", im)
			}
		}
	}
	if u.Images[0].Name != "sketch.png" {
		t.Fatalf("attachment name %q", u.Images[0].Name)
	}
	// The PDF attachment is not an image and is never fetched; the older
	// conversation is never opened once the newest prompt is known.
	for _, op := range fake.ops() {
		if strings.Contains(op, "Br1efDoc789") || strings.Contains(op, "000000000002") {
			t.Fatalf("unneeded fetch %s in %v", op, fake.ops())
		}
	}
}

func TestChatGPTLatestWithoutImagesFetchesNoFiles(t *testing.T) {
	fake := chatgptFake(t)
	r := newTestChatGPT(fake)
	convs, err := r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeLatest, Count: 2}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 2 || convs[1].Messages[0].Text != "draw a fox logo for my landing page" {
		t.Fatalf("latest 2: %+v", convs)
	}
	for _, c := range convs {
		for _, m := range c.Messages {
			if len(m.Images) > 0 {
				t.Fatal("images without want_images")
			}
		}
	}
	for _, op := range fake.ops() {
		if strings.HasPrefix(op, string(OpChatGPTFile)) {
			t.Fatalf("file fetched without want_images: %v", fake.ops())
		}
	}
}

func TestChatGPTSearchAndConversation(t *testing.T) {
	r := newTestChatGPT(chatgptFake(t))
	convs, err := r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeSearch, Terms: []string{"sourdough"}, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || !strings.Contains(convs[0].Messages[0].Text, "nail polish") {
		t.Fatalf("search %+v", convs)
	}
	if got := imageSHAs(convs[0], RoleUser); len(got) != 1 || got[0] != "file_00000000abcd1234" {
		t.Fatalf("sediment pointer image %v", got)
	}
	convs, err = r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeConversation, ConversationID: "6a1f0c2e-1111-4a2b-9c3d-000000000001"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range convs[0].Messages {
		texts = append(texts, m.Text)
	}
	all := strings.Join(texts, "|")
	if all != "draw a fox logo for my landing page|Here is a first fox logo concept.|make the fox ears bigger like this sketch|Here is the fox with bigger ears." {
		t.Fatalf("conversation messages %q", all)
	}
	if strings.Contains(all, "ABANDONED") || strings.Contains(all, "prompt\"") {
		t.Fatal("abandoned branch or tool call leaked")
	}
	_, err = r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeConversation, ConversationID: "6a1f0c2e-1111-4a2b-9c3d-00000000dead"}, Options{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing conversation: %v", err)
	}
}

func TestClientChunkReassemblyAndCaps(t *testing.T) {
	data := fakePNG(1000)
	ok := &fakeChannel{handle: func(NativeRequest) ([]NativeResponse, error) { return chunkFrames(data, "image/png", 300), nil }}
	c := &Client{Channel: ok, Timeout: time.Second}
	got, mime, err := c.File(context.Background(), OpClaudeAIFile, OpArgs{FileID: "f1"})
	if err != nil || mime != "image/png" || !bytes.Equal(got, data) {
		t.Fatalf("reassembly: %d %s %v", len(got), mime, err)
	}
	cases := map[string]func() []NativeResponse{
		"over cap": func() []NativeResponse { return chunkFrames(fakePNG(2000), "image/png", 300) },
		"size lies": func() []NativeResponse {
			f := chunkFrames(data, "image/png", 300)
			for i := range f {
				f[i].Size = 999999
			}
			return f
		},
		"out of order": func() []NativeResponse {
			f := chunkFrames(data, "image/png", 300)
			f[1], f[2] = f[2], f[1]
			return f
		},
		"bad base64": func() []NativeResponse {
			f := chunkFrames(data, "image/png", 300)
			f[0].Chunk.Data = "!!!"
			return f
		},
		"truncated": func() []NativeResponse { return chunkFrames(data, "image/png", 300)[:2] },
	}
	for name, frames := range cases {
		ch := &fakeChannel{handle: func(NativeRequest) ([]NativeResponse, error) { return frames(), nil }}
		c := &Client{Channel: ch, Timeout: time.Second, MaxFile: 1500}
		if _, _, err := c.File(context.Background(), OpClaudeAIFile, OpArgs{FileID: "f1"}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	big := &fakeChannel{handle: func(NativeRequest) ([]NativeResponse, error) {
		return []NativeResponse{{OK: true, Result: json.RawMessage(`"` + strings.Repeat("a", 100) + `"`)}}, nil
	}}
	if _, err := (&Client{Channel: big, MaxJSON: 50}).Request(context.Background(), OpChatGPTList, OpArgs{Count: 1}); err == nil {
		t.Error("oversize JSON result accepted")
	}
}

func TestClientRejectsBadOpsBeforeSending(t *testing.T) {
	fake := chatgptFake(t)
	c := &Client{Channel: fake}
	if _, err := c.Request(context.Background(), "chatgpt.eval", OpArgs{Count: 1}); !errors.Is(err, ErrRejected) {
		t.Fatalf("unknown op: %v", err)
	}
	if _, err := c.Request(context.Background(), OpChatGPTDetail, OpArgs{ID: "../x"}); !errors.Is(err, ErrRejected) {
		t.Fatalf("malformed id: %v", err)
	}
	if len(fake.ops()) != 0 {
		t.Fatalf("rejected ops were sent: %v", fake.ops())
	}
	r := newTestChatGPT(fake)
	if _, err := r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeConversation, ConversationID: "a.b"}, Options{}); err == nil {
		t.Fatal("dotted id accepted by the chatgpt reader")
	}
}

func TestSourceUnavailableMessages(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		kind error
		want string
	}{
		{"chrome not running", errChannel(ErrChromeNotRunning), ErrChromeNotRunning, "source unavailable: chatgpt: Chrome is not running"},
		{"extension not connected", errChannel(ErrExtensionNotConnected), ErrExtensionNotConnected, "source unavailable: chatgpt: the Tincan Chrome extension is not connected (install it, then run tincan history install)"},
		{"host closed", frames(), ErrExtensionNotConnected, "source unavailable: chatgpt: the Tincan Chrome extension is not connected (install it, then run tincan history install)"},
		{"not logged in", frames(NativeResponse{Error: &NativeError{Code: "not_logged_in", Message: "401"}}), ErrNotLoggedIn, "source unavailable: chatgpt: not logged in to chatgpt.com in Chrome"},
		{"endpoint changed", frames(NativeResponse{Error: &NativeError{Code: "endpoint_changed", Message: "404"}}), ErrEndpointChanged, "source unavailable: chatgpt: chatgpt.com changed its API (404)"},
		{"blocked", frames(NativeResponse{Error: &NativeError{Code: "blocked", Message: "403 challenge"}}), ErrEndpointChanged, "source unavailable: chatgpt: chatgpt.com changed its API (blocked: 403 challenge)"},
		{"unparseable result", frames(NativeResponse{OK: true, Result: json.RawMessage(`{"items": "nope"}`)}), ErrEndpointChanged, "source unavailable: chatgpt: chatgpt.com changed its API (unexpected list shape)"},
		{"timeout", blockChannel{}, ErrTimeout, "source unavailable: chatgpt: Chrome did not answer in time"},
		{"rejected", frames(NativeResponse{Error: &NativeError{Code: "bad_request", Message: "unknown op"}}), ErrRejected, "source unavailable: chatgpt: the extension rejected the request (unknown op)"},
		{"failed", frames(NativeResponse{Error: &NativeError{Code: "http_error", Message: "HTTP 500"}}), ErrSourceFailed, "source unavailable: chatgpt: chatgpt.com request failed (HTTP 500)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewChatGPT(&Client{Channel: c.ch, Timeout: 50 * time.Millisecond})
			r.Now = func() time.Time { return liveNow }
			_, err := r.List(context.Background(), 5, Options{})
			if !errors.Is(err, c.kind) {
				t.Fatalf("kind: got %v, want %v", err, c.kind)
			}
			var ue *UnavailableError
			if !errors.As(err, &ue) || ue.Source != SourceChatGPT {
				t.Fatalf("not an UnavailableError: %T %v", err, err)
			}
			if err.Error() != c.want {
				t.Fatalf("message:\n got %q\nwant %q", err.Error(), c.want)
			}
		})
	}
}

func errChannel(err error) Channel { return errCh{err} }

type errCh struct{ err error }

func (e errCh) Exchange(context.Context, NativeRequest, func(NativeResponse) (bool, error)) error {
	return e.err
}

type blockChannel struct{}

func (blockChannel) Exchange(ctx context.Context, _ NativeRequest, _ func(NativeResponse) (bool, error)) error {
	<-ctx.Done()
	return ctx.Err()
}

func frames(f ...NativeResponse) Channel {
	return &fakeChannel{handle: func(NativeRequest) ([]NativeResponse, error) { return f, nil }}
}

// withNewerTextConversation makes the fake list a text-only conversation
// newer than every fixture conversation.
func withNewerTextConversation(t *testing.T, fake *fakeChannel, listOp, detailOp Op, list func(json.RawMessage) json.RawMessage, id string, detail string) {
	t.Helper()
	inner := fake.handle
	fake.handle = func(req NativeRequest) ([]NativeResponse, error) {
		switch {
		case req.Op == listOp:
			frames, err := inner(req)
			if err != nil || len(frames) != 1 {
				return frames, err
			}
			frames[0].Result = list(frames[0].Result)
			return frames, nil
		case req.Op == detailOp && req.Args.ID == id:
			return []NativeResponse{{OK: true, Result: json.RawMessage(detail)}}, nil
		}
		return inner(req)
	}
}

func TestChatGPTWithImagesPicksOlderTurnThatHasImages(t *testing.T) {
	const newer = "6a1f0c2e-1111-4a2b-9c3d-000000000004"
	fake := chatgptFake(t)
	withNewerTextConversation(t, fake, OpChatGPTList, OpChatGPTDetail, func(raw json.RawMessage) json.RawMessage {
		var l map[string]any
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		items := l["items"].([]any)
		l["items"] = append([]any{map[string]any{"id": newer, "title": "Weather", "update_time": "2026-09-22T11:00:00Z"}}, items...)
		b, _ := json.Marshal(l)
		return b
	}, newer, `{"title":"Weather","update_time":"2026-09-22T11:00:00Z","conversation_id":"`+newer+`","current_node":"w-a1","mapping":{
		"w-root":{"id":"w-root","message":null,"parent":null,"children":["w-u1"]},
		"w-u1":{"id":"w-u1","message":{"id":"w-u1","author":{"role":"user"},"create_time":"2026-09-22T10:59:00Z","content":{"content_type":"text","parts":["will it rain tomorrow"]},"metadata":{},"recipient":"all"},"parent":"w-root","children":["w-a1"]},
		"w-a1":{"id":"w-a1","message":{"id":"w-a1","author":{"role":"assistant"},"create_time":"2026-09-22T11:00:00Z","content":{"content_type":"text","parts":["Probably not."]},"metadata":{},"recipient":"all"},"parent":"w-u1","children":[]}}}`)
	r := newTestChatGPT(fake)
	convs, err := r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeLatest}, Options{})
	if err != nil || ids(convs) != newer {
		t.Fatalf("plain latest = %s, %v; want the newer text-only conversation", ids(convs), err)
	}
	convs, err = r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeLatest, WithImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ids(convs) != "6a1f0c2e-1111-4a2b-9c3d-000000000001" || convs[0].Messages[0].Text != "make the fox ears bigger like this sketch" {
		t.Fatalf("with_images latest = %+v", convs)
	}
	if got := imageSHAs(convs[0], RoleUser); len(got) != 1 || got[0] != "file-Sk3tchAbc123" {
		t.Fatalf("attached images %v", got)
	}
	if got := imageSHAs(convs[0], RoleAssistant); len(got) != 1 || got[0] != "file-Gen3rated456" {
		t.Fatalf("generated images %v", got)
	}
}
