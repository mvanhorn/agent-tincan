package relay_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
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

// mentionMesh is a mesh with room "launch" holding codex, claude-code,
// claude-web (a web chat agent), a phone, and muse; grokbot and instinct
// are not members.
func mentionMesh(t *testing.T) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	admin := m.Client(t, "admin")
	if _, err := client.CreateRoom(t.Context(), admin, "launch"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "claude-code", "claude-web", "phone"} {
		m.JoinOnMachineOf(t, "grokbot", name)
	}
	for name, kind := range map[string]string{"codex": "codex", "claude-code": "claude-code", "claude-web": "claude-web", "phone": "phone"} {
		if err := admin.SetKind(t.Context(), name, kind); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []string{"codex", "claude-code", "claude-web", "phone", "muse"} {
		if _, err := client.AddRoomMember(t.Context(), admin, "launch", a); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// take polls agent's inbox for exactly one request and claims it.
func take(t *testing.T, m *testrelay.Mesh, agent string) envelope.Request {
	t.Helper()
	in, err := m.Client(t, agent).Poll(t.Context(), time.Millisecond)
	if err != nil || len(in.Requests) != 1 {
		t.Fatalf("%s inbox = %+v %v", agent, in, err)
	}
	req := in.Requests[0]
	if _, err := m.Client(t, agent).Claim(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	return req
}

func post(t *testing.T, m *testrelay.Mesh, agent, body string) envelope.RoomPostResponse {
	t.Helper()
	res, err := client.PostToRoom(t.Context(), m.Client(t, agent), "launch", body)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func thread(t *testing.T, m *testrelay.Mesh, agent string) []envelope.RoomPost {
	t.Helper()
	th, err := client.ReadRoom(t.Context(), m.Client(t, agent), "launch", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return th.Posts
}

// Covers AE7. A mention of claude-code becomes an ask from codex naming the
// room, the poster and the post; claude-code's reply appears in the room as
// its post, and the mention shows answered.
func TestRoomMentionBecomesAsk(t *testing.T) {
	m := mentionMesh(t)
	res := post(t, m, "codex", "@claude-code review this")
	if len(res.Post.Mentions) != 1 || res.Post.Mentions[0].Agent != "claude-code" || len(res.Notices) != 0 {
		t.Fatalf("post = %+v", res)
	}
	req := take(t, m, "claude-code")
	if req.From != "codex" || req.Kind != envelope.KindAsk || req.Group != fmt.Sprintf("room-%d", res.Post.ID) || req.RoomPostID != res.Post.ID ||
		!strings.Contains(req.Body, `room "launch"`) || !strings.Contains(req.Body, "review this") || !strings.Contains(req.Body, "tincan room read launch") {
		t.Fatalf("mention ask = %+v", req)
	}
	if _, err := m.Client(t, "claude-code").Reply(t.Context(), req.ID, "lgtm", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	posts := thread(t, m, "muse")
	if len(posts) != 2 || posts[1].Author != "claude-code" || posts[1].Body != "lgtm" || posts[1].Status != envelope.StatusAnswered || posts[1].ReplyTo != req.ID {
		t.Fatalf("thread = %+v", posts)
	}
	if mm := posts[0].Mentions; len(mm) != 1 || mm[0].Status != envelope.StatusAnswered {
		t.Fatalf("mention status = %+v", mm)
	}
	// The asker still gets the reply as for any ask.
	if got, err := m.Client(t, "codex").Get(t.Context(), req.ID, 0); err != nil || got.Reply == nil || got.Reply.Body != "lgtm" {
		t.Fatalf("codex get = %+v %v", got, err)
	}
	events, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	mirrored := false
	for _, e := range events {
		if strings.Contains(e.Detail, "lgtm") || strings.Contains(e.Detail, "review this") {
			t.Fatalf("room text in audit: %+v", e)
		}
		mirrored = mirrored || (e.Event == "room_posted" && e.RequestID == req.ID && e.Actor == "claude-code")
	}
	if !mirrored {
		t.Fatal("no room_posted event for the mirrored reply")
	}
}

// Covers AE7. While claude-code holds codex's mention, its own mention of
// codex would loop and is refused with a notice; its reply is still
// mirrored, and once it has replied a new mention of codex is a fresh ask.
func TestRoomMentionLoopRejected(t *testing.T) {
	m := mentionMesh(t)
	post(t, m, "codex", "@claude-code review this")
	req := take(t, m, "claude-code")
	res := post(t, m, "claude-code", "@codex which file?")
	if len(res.Post.Mentions) != 0 || len(res.Notices) != 1 || !strings.Contains(res.Notices[0], "loop") {
		t.Fatalf("looping post = %+v", res)
	}
	if n := m.Server.QueuedCount("codex"); n != 0 {
		t.Fatalf("codex queued %d", n)
	}
	if _, err := m.Client(t, "claude-code").Reply(t.Context(), req.ID, "done", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	res = post(t, m, "claude-code", "@codex next step?")
	if len(res.Post.Mentions) != 1 || len(res.Notices) != 0 {
		t.Fatalf("fresh post = %+v", res)
	}
	if got := take(t, m, "codex"); len(got.Chain) != 1 || got.Chain[0] != "claude-code" {
		t.Fatalf("fresh chain = %v", got.Chain)
	}
}

// A mention made while the poster handles another agent's request carries
// that request's chain, so the target's allowlist sees the whole chain.
func TestRoomMentionCarriesPosterChain(t *testing.T) {
	m := mentionMesh(t)
	held, err := m.Client(t, "grokbot").Send(t.Context(), "codex", "plan it", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	take(t, m, "codex")
	post(t, m, "codex", "@claude-code review the plan")
	req := take(t, m, "claude-code")
	if req.ParentID != held.ID || !slices.Equal(req.Chain, []string{"grokbot", "codex"}) || req.TraceID != held.TraceID {
		t.Fatalf("mention chain = parent %s chain %v", req.ParentID, req.Chain)
	}
}

// Mentions of a non-member, a phone member or a non-agent ask no one;
// the poster is told about the agents.
func TestRoomMentionNonTargets(t *testing.T) {
	m := mentionMesh(t)
	res := post(t, m, "codex", "@instinct @phone @nobody mail me at codex@example.com")
	if len(res.Post.Mentions) != 0 || len(res.Notices) != 2 || !strings.Contains(res.Notices[0], "not a member") || !strings.Contains(res.Notices[1], "phone") {
		t.Fatalf("post = %+v", res)
	}
	for _, a := range []string{"instinct", "phone", "codex"} {
		if n := m.Server.QueuedCount(a); n != 0 {
			t.Fatalf("%s queued %d", a, n)
		}
	}
}

// A post's mentions become at most 8 asks, one group; the rest are named
// in a notice.
func TestRoomMentionCap(t *testing.T) {
	m := mentionMesh(t)
	admin := m.Client(t, "admin")
	var names []string
	for i := range 9 {
		name := fmt.Sprintf("dev%d", i)
		m.JoinOnMachineOf(t, "instinct", name)
		if _, err := client.AddRoomMember(t.Context(), admin, "launch", name); err != nil {
			t.Fatal(err)
		}
		names = append(names, "@"+name)
	}
	res := post(t, m, "codex", strings.Join(names, " ")+" please look")
	if len(res.Post.Mentions) != 8 || len(res.Notices) != 1 || !strings.Contains(res.Notices[0], "@dev8") {
		t.Fatalf("post = %d mentions, notices %v", len(res.Post.Mentions), res.Notices)
	}
	if n := m.Server.QueuedCount("dev8"); n != 0 {
		t.Fatalf("ninth mention queued %d", n)
	}
}

// A web chat member cannot read the thread with tools: its ask opens a new
// chat and carries the earlier posts, newest first, within the site's
// message limit. Its reply, though it mentions codex, is posted as text
// and asks no one.
func TestRoomMentionToWebMember(t *testing.T) {
	m := mentionMesh(t)
	for i := range 4 {
		post(t, m, "muse", fmt.Sprintf("note %d %s", i, strings.Repeat("x", 10<<10)))
	}
	post(t, m, "codex", "@claude-web what do you think?")
	req := take(t, m, "claude-web")
	if relay.WebSendBudget != history.MaxSendMessage {
		t.Fatalf("web send budget %d, history sends %d", relay.WebSendBudget, history.MaxSendMessage)
	}
	if !strings.HasPrefix(req.Body, "new chat\n") || len(req.Body) > history.MaxSendMessage || !strings.Contains(req.Body, "context from several teammates") {
		t.Fatalf("web ask: %d bytes, starts %q", len(req.Body), req.Body[:min(40, len(req.Body))])
	}
	n3, n2, n0 := strings.Index(req.Body, "note 3"), strings.Index(req.Body, "note 2"), strings.Index(req.Body, "note 0")
	if n3 < 0 || n2 < n3 || n0 >= 0 {
		t.Fatalf("packed posts out of order or over budget: %d %d %d", n3, n2, n0)
	}
	if _, err := m.Client(t, "claude-web").Reply(t.Context(), req.ID, "ask @codex about it", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if n := m.Server.QueuedCount("codex"); n != 0 {
		t.Fatalf("mirrored reply asked codex: %d", n)
	}
	posts := thread(t, m, "codex")
	if last := posts[len(posts)-1]; last.Author != "claude-web" || last.Body != "ask @codex about it" || len(last.Mentions) != 0 {
		t.Fatalf("mirrored = %+v", last)
	}
}

// A question posts as a question; a replier removed from the room meanwhile
// has its answer recorded but not posted.
func TestRoomMirrorQuestionAndFormerMember(t *testing.T) {
	m := mentionMesh(t)
	post(t, m, "codex", "@claude-code review this")
	req := take(t, m, "claude-code")
	cc := m.Client(t, "claude-code")
	if _, err := cc.Reply(t.Context(), req.ID, "which branch?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	posts := thread(t, m, "muse")
	if q := posts[len(posts)-1]; q.Status != envelope.StatusNeedsInput || q.Body != "which branch?" {
		t.Fatalf("question = %+v", q)
	}
	if err := m.Client(t, "codex").Raw(t.Context(), "POST", "/v1/requests/"+req.ID+"/answer", map[string]string{"body": "main"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RemoveRoomMember(t.Context(), m.Client(t, "admin"), "launch", "claude-code"); err != nil {
		t.Fatal(err)
	}
	take(t, m, "claude-code")
	if _, err := cc.Reply(t.Context(), req.ID, "merged", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if got := thread(t, m, "muse"); len(got) != len(posts) {
		t.Fatalf("former member's reply posted: %+v", got[len(got)-1])
	}
	if res, err := m.Client(t, "codex").Get(t.Context(), req.ID, 0); err != nil || res.Reply == nil || res.Reply.Body != "merged" {
		t.Fatalf("reply not recorded: %+v %v", res, err)
	}
}

// A mention wakes its target like any ask, and the wake carries counts,
// never the post.
func TestRoomMentionWakeCarriesCounts(t *testing.T) {
	m := mentionMesh(t)
	var mu sync.Mutex
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer ts.Close()
	waker := wake.New(wake.Config{"claude-code": {Method: wake.Webhook, URL: ts.URL}}, m.Store, wake.Options{Debounce: time.Millisecond})
	m.Server.SetEvents(waker)
	post(t, m, "codex", "@claude-code secret launch plan")
	waker.Flush()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("mention did not wake claude-code")
	}
	for _, b := range bodies {
		if strings.Contains(b, "secret") || strings.Contains(b, "launch") {
			t.Fatalf("wake carries room text: %s", b)
		}
	}
}

// An older client sees a mention as an ordinary ask and answers it with
// the routes it already has.
func TestRoomMentionToOlderClient(t *testing.T) {
	m := mentionMesh(t)
	post(t, m, "codex", "@claude-code review this")
	var raw json.RawMessage
	if err := m.Client(t, "claude-code").Raw(t.Context(), "GET", "/v1/poll?hold=0", nil, &raw); err != nil {
		t.Fatal(err)
	}
	// The fields an older client reads.
	var old struct {
		Requests []struct {
			ID, From, To, Body string
			Kind               string `json:"kind"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(raw, &old); err != nil || len(old.Requests) != 1 || old.Requests[0].Kind != "ask" || old.Requests[0].From != "codex" {
		t.Fatalf("old poll = %s %v", raw, err)
	}
	id := old.Requests[0].ID
	cc := m.Client(t, "claude-code")
	if err := cc.Raw(t.Context(), "POST", "/v1/requests/"+id+"/claim", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := cc.Raw(t.Context(), "POST", "/v1/requests/"+id+"/reply", map[string]string{"body": "ok", "status": "answered"}, nil); err != nil {
		t.Fatal(err)
	}
	if posts := thread(t, m, "codex"); posts[len(posts)-1].Body != "ok" {
		t.Fatalf("reply not mirrored: %+v", posts)
	}
}

// Three agents in one exchange produce the thread in the order things
// happened: the post, then each reply as it came.
func TestRoomThreeAgentExchange(t *testing.T) {
	m := mentionMesh(t)
	post(t, m, "codex", "@claude-code @muse plan, then review")
	cc := take(t, m, "claude-code")
	mu := take(t, m, "muse")
	if _, err := m.Client(t, "muse").Reply(t.Context(), mu.ID, "reviewed", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "claude-code").Reply(t.Context(), cc.ID, "can't", envelope.StatusDeclined); err != nil {
		t.Fatal(err)
	}
	posts := thread(t, m, "codex")
	var got []string
	for _, p := range posts {
		got = append(got, p.Author+"/"+string(p.Status))
	}
	if want := []string{"codex/", "muse/answered", "claude-code/declined"}; !slices.Equal(got, want) {
		t.Fatalf("thread = %v, want %v", got, want)
	}
	if mm := posts[0].Mentions; len(mm) != 2 || mm[0].Agent != "claude-code" || mm[1].Agent != "muse" {
		t.Fatalf("mentions = %+v", mm)
	}
}
