package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

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
	// A database from before rooms: drop the tables this version added.
	if _, err := s.DB().Exec(`DROP TABLE room_posts; DROP TABLE room_members; DROP TABLE rooms;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for range 2 {
		s, _ = open(t, path)
		if a, ok, err := s.AgentByName(ctx, "codex"); err != nil || !ok || a.NodeID != "n1" {
			t.Fatalf("agent = %+v %v %v", a, ok, err)
		}
		if got, _, err := s.Request(ctx, req.ID); err != nil || got.Body != "hi" {
			t.Fatalf("request = %+v %v", got, err)
		}
		if _, err := s.CreateRoom(ctx, "launch"); err != nil && !errors.Is(err, ErrRoomExists) {
			t.Fatal(err)
		}
		s.Close()
	}
}
