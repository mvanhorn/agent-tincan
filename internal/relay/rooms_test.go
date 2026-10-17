package relay_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// roomMesh is a mesh with room "launch" holding muse and grokbot.
func roomMesh(t *testing.T) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	admin := m.Client(t, "admin")
	if _, err := client.CreateRoom(t.Context(), admin, "launch"); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"muse", "grokbot"} {
		if _, err := client.AddRoomMember(t.Context(), admin, "launch", a); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// A member posts and another member reads the post with its author and
// time; a non-member can do neither.
func TestRoomPostAndRead(t *testing.T) {
	m := roomMesh(t)
	res, err := client.PostToRoom(t.Context(), m.Client(t, "muse"), "launch", "plan is up")
	if err != nil {
		t.Fatal(err)
	}
	if res.Post.Author != "muse" || res.Post.ID == 0 || res.Post.CreatedAt.IsZero() {
		t.Fatalf("post = %+v", res.Post)
	}
	th, err := client.ReadRoom(t.Context(), m.Client(t, "grokbot"), "launch", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Posts) != 1 || th.Posts[0].Author != "muse" || th.Posts[0].Body != "plan is up" || !th.Posts[0].CreatedAt.Equal(res.Post.CreatedAt) || th.Next != res.Post.ID || !slices.Equal(th.Members, []string{"grokbot", "muse"}) {
		t.Fatalf("thread = %+v", th)
	}
	if th, err := client.ReadRoom(t.Context(), m.Client(t, "grokbot"), "launch", th.Next, 0); err != nil || len(th.Posts) != 0 || th.Next != res.Post.ID {
		t.Fatalf("after cursor = %+v %v", th, err)
	}
	stranger := m.Client(t, "instinct")
	if _, err := client.ReadRoom(t.Context(), stranger, "launch", 0, 0); !client.IsStatus(err, 403) {
		t.Fatalf("non-member read: %v", err)
	}
	if _, err := client.PostToRoom(t.Context(), stranger, "launch", "hi"); !client.IsStatus(err, 403) {
		t.Fatalf("non-member post: %v", err)
	}
	if rooms, err := client.Rooms(t.Context(), stranger, false); err != nil || len(rooms) != 0 {
		t.Fatalf("non-member rooms = %+v %v", rooms, err)
	}
	if rooms, err := client.Rooms(t.Context(), m.Client(t, "muse"), false); err != nil || len(rooms) != 1 || rooms[0].Name != "launch" {
		t.Fatalf("member rooms = %+v %v", rooms, err)
	}
	if _, err := client.ReadRoom(t.Context(), m.Client(t, "muse"), "nope", 0, 0); !client.IsStatus(err, 404) {
		t.Fatalf("missing room: %v", err)
	}
	if _, err := client.PostToRoom(t.Context(), m.Client(t, "muse"), "launch", strings.Repeat("x", 16<<10+1)); !client.IsStatus(err, 413) {
		t.Fatalf("oversized post: %v", err)
	}
}

// Only admins create rooms and change members.
func TestRoomAdminOnly(t *testing.T) {
	m := roomMesh(t)
	muse := m.Client(t, "muse")
	if _, err := client.CreateRoom(t.Context(), muse, "mine"); !client.IsStatus(err, 403) {
		t.Fatalf("agent create: %v", err)
	}
	if _, err := client.AddRoomMember(t.Context(), muse, "launch", "instinct"); !client.IsStatus(err, 403) {
		t.Fatalf("agent add: %v", err)
	}
	if _, err := client.RemoveRoomMember(t.Context(), muse, "launch", "grokbot"); !client.IsStatus(err, 403) {
		t.Fatalf("agent remove: %v", err)
	}
	if _, err := client.Rooms(t.Context(), muse, true); !client.IsStatus(err, 403) {
		t.Fatalf("agent list all: %v", err)
	}
	if _, err := client.CreateRoom(t.Context(), m.Client(t, "admin"), "launch"); !client.IsStatus(err, 409) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := client.CreateRoom(t.Context(), m.Client(t, "admin"), "Bad Name"); !client.IsStatus(err, 400) {
		t.Fatalf("bad name: %v", err)
	}
	res, err := client.RemoveRoomMember(t.Context(), m.Client(t, "admin"), "launch", "grokbot")
	if err != nil || !res.Changed {
		t.Fatalf("remove = %+v %v", res, err)
	}
	if _, err := client.ReadRoom(t.Context(), m.Client(t, "grokbot"), "launch", 0, 0); !client.IsStatus(err, 403) {
		t.Fatalf("removed member read: %v", err)
	}
}

// Agents whose asks are held, and service agents, are refused with the
// reason; a phone is a member; a web chat member is added with a warning
// that the thread goes to its vendor.
func TestRoomMemberEligibility(t *testing.T) {
	m := roomMesh(t)
	admin := m.Client(t, "admin")
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"instinct":{"from":["grokbot"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	for name, kind := range map[string]string{"council": "council", "dot-web": "dot-web", "history": "history", "notes": "notes", "pass": "pass", "phone": "phone", "chatgpt-web": "chatgpt-web"} {
		m.JoinOnMachineOf(t, "muse", name)
		if _, err := m.Store.SetAgentKind(t.Context(), name, kind); err != nil {
			t.Fatal(err)
		}
	}
	for name, why := range map[string]string{"council": "held for the owner's approval", "dot-web": "held for the owner's approval", "history": "service agent", "notes": "service agent", "pass": "service agent", "instinct": "approval.json holds asks"} {
		if _, err := client.AddRoomMember(t.Context(), admin, "launch", name); !client.IsStatus(err, 400) || !strings.Contains(err.Error(), why) {
			t.Errorf("add %s: %v", name, err)
		}
	}
	if res, err := client.AddRoomMember(t.Context(), admin, "launch", "phone"); err != nil || !res.Changed || res.Warning != "" {
		t.Fatalf("phone = %+v %v", res, err)
	}
	res, err := client.AddRoomMember(t.Context(), admin, "launch", "chatgpt-web")
	if err != nil || !res.Changed || !strings.Contains(res.Warning, "vendor") {
		t.Fatalf("chatgpt-web = %+v %v", res, err)
	}
	if _, err := client.AddRoomMember(t.Context(), admin, "launch", "nobody"); !client.IsStatus(err, 404) {
		t.Fatalf("unknown agent: %v", err)
	}
	if _, err := client.PostToRoom(t.Context(), m.Client(t, "phone"), "launch", "from the phone"); err != nil {
		t.Fatalf("phone post: %v", err)
	}
}

// A member that approval.json starts holding is shut out of read and post
// until the owner removes it.
func TestRoomMemberBecomesGated(t *testing.T) {
	m := roomMesh(t)
	path := filepath.Join(t.TempDir(), "approval.json")
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	grokbot := m.Client(t, "grokbot")
	if _, err := client.ReadRoom(t.Context(), grokbot, "launch", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"gate":{"grokbot":{"from":"*"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadRoom(t.Context(), grokbot, "launch", 0, 0); !client.IsStatus(err, 403) {
		t.Fatalf("gated read: %v", err)
	}
	if _, err := client.PostToRoom(t.Context(), grokbot, "launch", "hi"); !client.IsStatus(err, 403) {
		t.Fatalf("gated post: %v", err)
	}
}

// Removing an agent drops its memberships: a new agent joined under the
// same name sees no rooms.
func TestRoomMembershipGoesWithRemovedAgent(t *testing.T) {
	m := roomMesh(t)
	if err := m.Client(t, "admin").Remove(t.Context(), "grokbot"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "grokbot").Join(t.Context(), m.Invite(t, "grokbot")); err != nil {
		t.Fatal(err)
	}
	if rooms, err := client.Rooms(t.Context(), m.Client(t, "grokbot"), false); err != nil || len(rooms) != 0 {
		t.Fatalf("rooms = %+v %v", rooms, err)
	}
	if _, err := client.ReadRoom(t.Context(), m.Client(t, "grokbot"), "launch", 0, 0); !client.IsStatus(err, 403) {
		t.Fatalf("read: %v", err)
	}
}

// Posts take slots from the poster's send rate: the 31st in a minute is
// refused.
func TestRoomPostRateLimited(t *testing.T) {
	m := roomMesh(t)
	muse := m.Client(t, "muse")
	for i := range 30 {
		if _, err := client.PostToRoom(t.Context(), muse, "launch", "post"); err != nil {
			t.Fatalf("post %d: %v", i+1, err)
		}
	}
	if _, err := client.PostToRoom(t.Context(), muse, "launch", "post"); !client.IsStatus(err, 429) {
		t.Fatalf("31st post: %v", err)
	}
	if _, err := muse.Send(t.Context(), "grokbot", "hi", "ask", "", false); !client.IsStatus(err, 429) {
		t.Fatalf("send after 30 posts: %v", err)
	}
}

// Room audit rows carry ids and byte counts, never post text, and the
// relay advertises rooms.
func TestRoomAuditAndCapability(t *testing.T) {
	m := roomMesh(t)
	if _, err := client.PostToRoom(t.Context(), m.Client(t, "muse"), "launch", "secret plan 界"); err != nil {
		t.Fatal(err)
	}
	events, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Event] = true
		if strings.Contains(e.Detail, "secret") {
			t.Fatalf("post text in audit: %+v", e)
		}
		if e.Event == "room_posted" && !strings.Contains(e.Detail, `"bytes":15`) {
			t.Fatalf("room_posted = %s", e.Detail)
		}
	}
	for _, ev := range []string{"room_created", "room_member_added", "room_posted"} {
		if !seen[ev] {
			t.Errorf("no %s event", ev)
		}
	}
	caps, err := m.Client(t, "muse").Capabilities(t.Context())
	if err != nil || !caps.Rooms {
		t.Fatalf("caps = %+v %v", caps, err)
	}
}
