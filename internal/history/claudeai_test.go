package history

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func claudeFake(t *testing.T) *fakeChannel {
	return &fakeChannel{handle: func(req NativeRequest) ([]NativeResponse, error) {
		if err := ValidateOp(req.Op, req.Args); err != nil {
			return []NativeResponse{{Error: &NativeError{Code: "bad_request", Message: err.Error()}}}, nil
		}
		switch req.Op {
		case OpClaudeAIList:
			return []NativeResponse{{OK: true, Result: fixture(t, "claudeai/chat_conversations.json")}}, nil
		case OpClaudeAIDetail:
			b, err := os.ReadFile("testdata/claudeai/conversation-" + req.Args.ID + ".json")
			if err != nil {
				return []NativeResponse{{Error: &NativeError{Code: "not_found", Message: "404"}}}, nil
			}
			return []NativeResponse{{OK: true, Result: b}}, nil
		case OpClaudeAIFile:
			return chunkFrames(append(fakePNG(64), req.Args.FileID...), "image/png", 50), nil
		}
		return nil, errors.New("unexpected op")
	}}
}

func newTestClaudeAI(ch Channel) *ClaudeAI {
	r := NewClaudeAI(&Client{Channel: ch, Timeout: 2 * time.Second})
	r.Now = func() time.Time { return liveNow }
	return r
}

func TestClaudeAIList(t *testing.T) {
	r := newTestClaudeAI(claudeFake(t))
	convs, err := r.List(context.Background(), 10, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 2 || convs[0].Title != "Tide pool photo" || convs[1].Title != "Trip packing list" || convs[0].Source != SourceClaudeAI {
		t.Fatalf("list %+v", convs)
	}
	if !convs[0].UpdatedAt.Equal(time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("updated %v", convs[0].UpdatedAt)
	}
	convs, err = r.List(context.Background(), 1, Options{})
	if err != nil || len(convs) != 1 {
		t.Fatalf("count 1: %v %v", convs, err)
	}
}

func TestClaudeAILatestWithImages(t *testing.T) {
	fake := claudeFake(t)
	r := newTestClaudeAI(fake)
	convs, err := r.Read(context.Background(), Query{Source: SourceClaudeAI, Mode: ModeLatest, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || len(convs[0].Messages) != 2 {
		t.Fatalf("latest %+v", convs)
	}
	u, a := convs[0].Messages[0], convs[0].Messages[1]
	if u.Text != "what is this creature in my photo?" || !u.Time.Equal(time.Date(2026, 9, 22, 10, 59, 0, 0, time.UTC)) {
		t.Fatalf("prompt %+v (the abandoned branch must not win)", u)
	}
	if a.Text != "That looks like an ochre sea star." {
		t.Fatalf("reply %+v", a)
	}
	if got := imageSHAs(convs[0], RoleUser); len(got) != 1 || got[0] != "f11e0000-0000-4000-8000-0000000000aa" {
		t.Fatalf("attached image (files and files_v2 deduplicated) %v", got)
	}
	if u.Images[0].Name != "tidepool.jpg" {
		t.Fatalf("image name %q", u.Images[0].Name)
	}
	if got := imageSHAs(convs[0], RoleAssistant); len(got) != 1 || got[0] != "f11e0000-0000-4000-8000-0000000000bb" {
		t.Fatalf("reply image %v", got)
	}
	for _, op := range fake.ops() {
		if strings.Contains(op, "000000000002") {
			t.Fatalf("older conversation opened: %v", fake.ops())
		}
	}
}

func TestClaudeAISearchConversationAndNotFound(t *testing.T) {
	r := newTestClaudeAI(claudeFake(t))
	convs, err := r.Read(context.Background(), Query{Source: SourceClaudeAI, Mode: ModeSearch, Terms: []string{"Portland"}}, Options{})
	if err != nil || len(convs) != 1 || convs[0].ID != "c1a0d000-0000-4000-8000-000000000002" {
		t.Fatalf("search %+v %v", convs, err)
	}
	if convs[0].Messages[0].Text != "make me a packing list for a rainy weekend in Portland" || convs[0].Messages[1].Text != "Rain jacket, boots, umbrella." {
		t.Fatalf("index-ordered messages %+v", convs[0].Messages)
	}
	convs, err = r.Read(context.Background(), Query{Source: SourceClaudeAI, Mode: ModeConversation, ConversationID: "c1a0d000-0000-4000-8000-000000000001"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range convs[0].Messages {
		texts = append(texts, m.Text)
	}
	if got := strings.Join(texts, "|"); got != "what lives in tide pools|Anemones, hermit crabs and sea stars.|what is this creature in my photo?|That looks like an ochre sea star." {
		t.Fatalf("conversation %q", got)
	}
	_, err = r.Read(context.Background(), Query{Source: SourceClaudeAI, Mode: ModeConversation, ConversationID: "c1a0d000-0000-4000-8000-00000000dead"}, Options{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeLatest}, Options{}); err == nil {
		t.Fatal("claude-ai reader answered a chatgpt query")
	}
}

func TestClaudeAIUnavailableMessages(t *testing.T) {
	cases := []struct {
		ch   Channel
		kind error
		want string
	}{
		{frames(NativeResponse{Error: &NativeError{Code: "not_logged_in", Message: "no organization"}}), ErrNotLoggedIn, "source unavailable: claude-ai: not logged in to claude.ai in Chrome"},
		{frames(NativeResponse{OK: true, Result: json.RawMessage(`{"not":"an array"}`)}), ErrEndpointChanged, "source unavailable: claude-ai: claude.ai changed its API (unexpected list shape)"},
		{errChannel(ErrChromeNotRunning), ErrChromeNotRunning, "source unavailable: claude-ai: Chrome is not running"},
	}
	for _, c := range cases {
		r := NewClaudeAI(&Client{Channel: c.ch, Timeout: time.Second})
		r.Now = func() time.Time { return liveNow }
		_, err := r.List(context.Background(), 5, Options{})
		if !errors.Is(err, c.kind) || err.Error() != c.want {
			t.Errorf("got %v, want %q", err, c.want)
		}
	}
}

func TestLiveImageFetchFailureDropsImageOnly(t *testing.T) {
	fake := claudeFake(t)
	inner := fake.handle
	fake.handle = func(req NativeRequest) ([]NativeResponse, error) {
		if req.Op == OpClaudeAIFile && strings.HasSuffix(req.Args.FileID, "bb") {
			return []NativeResponse{{Error: &NativeError{Code: "not_found", Message: "404"}}}, nil
		}
		return inner(req)
	}
	r := newTestClaudeAI(fake)
	convs, err := r.Read(context.Background(), Query{Source: SourceClaudeAI, Mode: ModeLatest, WantImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(imageSHAs(convs[0], RoleUser)) != 1 || len(imageSHAs(convs[0], RoleAssistant)) != 0 {
		t.Fatalf("images %+v", convs[0].Messages)
	}
}

func TestClaudeAIWithImagesPicksOlderTurnThatHasImages(t *testing.T) {
	const newer = "c1a0d000-0000-4000-8000-000000000004"
	fake := claudeFake(t)
	withNewerTextConversation(t, fake, OpClaudeAIList, OpClaudeAIDetail, func(raw json.RawMessage) json.RawMessage {
		var l []any
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(append([]any{map[string]any{"uuid": newer, "name": "Weather", "created_at": "2026-09-22T11:30:00Z", "updated_at": "2026-09-22T11:31:00Z"}}, l...))
		return b
	}, newer, `{"uuid":"`+newer+`","name":"Weather","created_at":"2026-09-22T11:30:00Z","updated_at":"2026-09-22T11:31:00Z","chat_messages":[
		{"uuid":"w0000000-0000-4000-8000-000000000001","text":"will it rain tomorrow","sender":"human","index":0,"created_at":"2026-09-22T11:30:00Z","attachments":[],"files":[]},
		{"uuid":"w0000000-0000-4000-8000-000000000002","text":"Probably not.","sender":"assistant","index":1,"created_at":"2026-09-22T11:30:05Z","attachments":[],"files":[]}]}`)
	r := newTestClaudeAI(fake)
	convs, err := r.Read(context.Background(), Query{Source: SourceClaudeAI, Mode: ModeLatest}, Options{})
	if err != nil || ids(convs) != newer {
		t.Fatalf("plain latest = %s, %v; want the newer text-only conversation", ids(convs), err)
	}
	convs, err = r.Read(context.Background(), Query{Source: SourceClaudeAI, Mode: ModeLatest, WithImages: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ids(convs) != "c1a0d000-0000-4000-8000-000000000001" || convs[0].Messages[0].Text != "what is this creature in my photo?" {
		t.Fatalf("with_images latest = %+v", convs)
	}
	if got := imageSHAs(convs[0], RoleUser); len(got) != 1 || got[0] != "f11e0000-0000-4000-8000-0000000000aa" {
		t.Fatalf("attached image %v", got)
	}
}
