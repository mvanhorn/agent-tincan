package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// A member posts and another member reads the post with its author and
// time, after a cursor.
func TestRoomPostAndRead(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	if _, err := s.CreateRoom(ctx, "launch"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRoom(ctx, "launch"); !errors.Is(err, ErrRoomExists) {
		t.Fatalf("second create: %v", err)
	}
	for _, a := range []string{"codex", "claude-code"} {
		if added, err := s.AddRoomMember(ctx, "launch", a); err != nil || !added {
			t.Fatalf("add %s: %v %v", a, added, err)
		}
	}
	if added, err := s.AddRoomMember(ctx, "launch", "codex"); err != nil || added {
		t.Fatalf("re-add: %v %v", added, err)
	}
	first, err := s.AddRoomPost(ctx, "launch", "codex", "plan is up")
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second)
	second, err := s.AddRoomPost(ctx, "launch", "claude-code", "looks good")
	if err != nil {
		t.Fatal(err)
	}
	posts, err := s.RoomPosts(ctx, "launch", 0, 0)
	if err != nil || len(posts) != 2 || posts[0].Author != "codex" || posts[0].Body != "plan is up" || !posts[0].CreatedAt.Equal(first.CreatedAt) || posts[1].ID != second.ID {
		t.Fatalf("posts = %+v %v", posts, err)
	}
	after, err := s.RoomPosts(ctx, "launch", first.ID, 0)
	if err != nil || len(after) != 1 || after[0].Author != "claude-code" {
		t.Fatalf("after cursor = %+v %v", after, err)
	}
	if _, err := s.AddRoomPost(ctx, "launch", "grokbot", "let me in"); !errors.Is(err, ErrNotRoomMember) {
		t.Fatalf("non-member post: %v", err)
	}
	if _, err := s.RoomPosts(ctx, "nope", 0, 0); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("missing room: %v", err)
	}
	rooms, err := s.Rooms(ctx, "codex")
	if err != nil || len(rooms) != 1 || !slices.Equal(rooms[0].Members, []string{"claude-code", "codex"}) || rooms[0].LastPost != second.ID {
		t.Fatalf("rooms = %+v %v", rooms, err)
	}
	if rooms, err := s.Rooms(ctx, "grokbot"); err != nil || len(rooms) != 0 {
		t.Fatalf("grokbot rooms = %+v %v", rooms, err)
	}
	if removed, err := s.RemoveRoomMember(ctx, "launch", "codex"); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if ok, err := s.IsRoomMember(ctx, "launch", "codex"); err != nil || ok {
		t.Fatalf("still member: %v %v", ok, err)
	}
}

// Removing an agent removes its memberships in the same transaction, so a
// new agent joined under the name is in no rooms; its posts stay.
func TestDeleteAgentLeavesRooms(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	if err := s.PutAgent(ctx, identity.Agent{Name: "codex", NodeID: "n1", NodeName: "mac", JoinedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRoom(ctx, "launch"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRoomMember(ctx, "launch", "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRoomPost(ctx, "launch", "codex", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAgent(ctx, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgent(ctx, identity.Agent{Name: "codex", NodeID: "n2", NodeName: "other", JoinedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	if rooms, err := s.Rooms(ctx, "codex"); err != nil || len(rooms) != 0 {
		t.Fatalf("new codex rooms = %+v %v", rooms, err)
	}
	if posts, err := s.RoomPosts(ctx, "launch", 0, 0); err != nil || len(posts) != 1 || posts[0].Author != "codex" {
		t.Fatalf("posts = %+v %v", posts, err)
	}
}

// The room tables migrate onto an existing relay database, keep its agents
// and requests, and opening again changes nothing.
func TestRoomsMigrateOnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, c := open(t, path)
	ctx := context.Background()
	if err := s.PutAgent(ctx, identity.Agent{Name: "codex", NodeID: "n1", NodeName: "mac", JoinedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	req := ask(t, s, "codex", "muse", "hi")
	// A database from before rooms: drop what this version added.
	if _, err := s.DB().Exec(`DROP TABLE room_posts; DROP TABLE room_members; DROP TABLE rooms; DROP INDEX requests_room_post; ALTER TABLE requests DROP COLUMN room_post_id;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for range 2 {
		s, _ = open(t, path)
		if a, ok, err := s.AgentByName(ctx, "codex"); err != nil || !ok || a.NodeID != "n1" {
			t.Fatalf("agent = %+v %v %v", a, ok, err)
		}
		if got, _, err := s.Request(ctx, req.ID); err != nil || got.Body != "hi" || got.RoomPostID != 0 {
			t.Fatalf("request = %+v %v", got, err)
		}
		if _, err := s.CreateRoom(ctx, "launch"); err != nil && !errors.Is(err, ErrRoomExists) {
			t.Fatal(err)
		}
		s.Close()
	}
}

// mention queues codex's post in launch as an ask to claude-code, as the
// relay does for an @mention, and claims it.
func mention(t *testing.T, s *Store) (envelope.RoomPost, envelope.Request) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRoom(ctx, "launch"); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"codex", "claude-code"} {
		if _, err := s.AddRoomMember(ctx, "launch", a); err != nil {
			t.Fatal(err)
		}
	}
	post, err := s.AddRoomPost(ctx, "launch", "codex", "@claude-code review this")
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.Enqueue(ctx, envelope.Request{From: "codex", To: "claude-code", Kind: envelope.KindAsk, Body: "review this", Hop: 1, Chain: []string{"codex"}, Group: "room-1", RoomPostID: post.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, req.ID, "claude-code", time.Hour); err != nil {
		t.Fatal(err)
	}
	return post, req
}

// A reply to a mention is posted to the room as the replier's post in the
// reply's own transaction: a question first, then the answer, each once.
func TestReplyMirroredIntoRoom(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	post, req := mention(t, s)
	if got, _, err := s.Request(ctx, req.ID); err != nil || got.RoomPostID != post.ID {
		t.Fatalf("room_post_id = %d %v", got.RoomPostID, err)
	}
	q, err := s.Reply(ctx, req.ID, "claude-code", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(ctx, req.ID, "codex", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, req.ID, "claude-code", time.Hour); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Reply(ctx, req.ID, "claude-code", envelope.Reply{Status: envelope.StatusAnswered, Body: "lgtm"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "claude-code", envelope.Reply{Status: envelope.StatusAnswered, Body: "lgtm"}); !errors.Is(err, ErrWrongState) {
		t.Fatalf("retried reply: %v", err)
	}
	posts, err := s.RoomPosts(ctx, "launch", 0, 0)
	if err != nil || len(posts) != 3 {
		t.Fatalf("posts = %+v %v", posts, err)
	}
	if p := posts[1]; p.Author != "claude-code" || p.Status != envelope.StatusNeedsInput || p.Body != "which branch?" || p.ReplyTo != req.ID {
		t.Fatalf("question post = %+v", p)
	}
	if p := posts[2]; p.Author != "claude-code" || p.Status != envelope.StatusAnswered || p.Body != "lgtm" {
		t.Fatalf("answer post = %+v", p)
	}
	if m := posts[0].Mentions; len(m) != 1 || m[0].Agent != "claude-code" || m[0].RequestID != req.ID || m[0].Status != envelope.StatusAnswered {
		t.Fatalf("mentions = %+v", m)
	}
	for _, g := range []int64{q.Generation, rep.Generation} {
		if _, ok, err := s.MirroredPost(ctx, req.ID, g); err != nil || !ok {
			t.Fatalf("mirrored gen %d: %v %v", g, ok, err)
		}
	}
}

// When the room post cannot be written, the reply is not recorded either:
// the two commit together, so no crash can leave a reply that never
// reaches the room.
func TestReplyAndMirrorCommitTogether(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	_, req := mention(t, s)
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_mirror BEFORE INSERT ON room_posts WHEN NEW.reply_to != '' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "claude-code", envelope.Reply{Status: envelope.StatusAnswered, Body: "lgtm"}); err == nil {
		t.Fatal("reply recorded without its room post")
	}
	if _, status, err := s.Request(ctx, req.ID); err != nil || status != envelope.StatusClaimed {
		t.Fatalf("status = %s %v", status, err)
	}
}

// A replier removed from the room after the mention has its reply
// recorded but not posted; a long reply is cut with a pointer.
func TestMirrorSkipsFormerMember(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	_, req := mention(t, s)
	if _, err := s.RemoveRoomMember(ctx, "launch", "claude-code"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "claude-code", envelope.Reply{Status: envelope.StatusAnswered, Body: "lgtm"}); err != nil {
		t.Fatal(err)
	}
	if posts, err := s.RoomPosts(ctx, "launch", 0, 0); err != nil || len(posts) != 1 {
		t.Fatalf("posts = %+v %v", posts, err)
	}
	long := mirroredBody(envelope.Reply{RequestID: "r1", Body: strings.Repeat("界", envelope.MaxRoomPost)})
	if len(long) > envelope.MaxRoomPost || !utf8.ValidString(long) || !strings.HasSuffix(long, "[cut; the whole reply is on request r1]") {
		t.Fatalf("long mirror: %d bytes, valid %v", len(long), utf8.ValidString(long))
	}
}
