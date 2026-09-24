package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// session connects a real MCP client to a tincan MCP server acting as agent.
func session(t *testing.T, m *testrelay.Mesh, agent string) *mcp.ClientSession {
	t.Helper()
	srvT, cliT := mcp.NewInMemoryTransports()
	srv := mcpserver.New(m.Client(t, agent), "test")
	ss, err := srv.Connect(t.Context(), srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(t.Context(), cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return "ERROR: " + b.String()
	}
	return b.String()
}

func TestToolListIsExactlyTheAgentTools(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	res, err := session(t, m, "grokbot").ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	want := slices.Clone(mcpserver.ToolNames)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	if !slices.Contains(names, "get_attachment") {
		t.Fatalf("get_attachment tool missing: %v", names)
	}
	if !slices.Contains(names, "onboard") {
		t.Fatalf("onboard tool missing: %v", names)
	}
	if !strings.Contains(mcpserver.Instructions, "onboard") {
		t.Fatal("Instructions should mention the onboard tool")
	}
	if !strings.Contains(mcpserver.Instructions, "When check_inbox shows a reply tied to one of your open requests, finish that request and reply to it.") {
		t.Fatal("Instructions should tell the agent to finish and reply to the open request a reply is tied to")
	}
	for _, n := range names {
		if strings.Contains(n, "invite") || strings.Contains(n, "remove") {
			t.Fatalf("admin action exposed as tool: %s", n)
		}
	}
}

// AE3 over MCP: Instinct asks Muse, Muse checks its inbox, replies, and the
// chain is visible.
func TestAskInboxReplyOverMCP(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: 3 * time.Second})
	inst, muse := session(t, m, "instinct"), session(t, m, "muse")

	out := call(t, inst, "ask", map[string]any{"to": "muse", "message": "call the dentist and reschedule", "wait_seconds": 1})
	if !strings.Contains(out, "No reply yet") {
		t.Fatalf("ask = %q", out)
	}
	inbox := call(t, muse, "check_inbox", nil)
	if !strings.Contains(inbox, "from instinct (your teammate)") || !strings.Contains(inbox, "call the dentist") {
		t.Fatalf("inbox = %q", inbox)
	}
	id := between(inbox, "Request ", " from")
	if got := call(t, muse, "reply", map[string]any{"request_id": id, "message": "done, Tue 3pm"}); !strings.Contains(got, "Replied") {
		t.Fatalf("reply = %q", got)
	}
	if got := call(t, inst, "get_reply", map[string]any{"request_id": id}); !strings.Contains(got, "done, Tue 3pm") {
		t.Fatalf("get_reply = %q", got)
	}
	got := call(t, muse, "trace", map[string]any{"trace_id": id})
	if !strings.Contains(got, "hop 1: instinct -> muse [answered]") {
		t.Fatalf("trace = %q", got)
	}
	// The audit trail the CLI shows is there too.
	if !strings.Contains(got, "Events:\n") || !regexp.MustCompile(`#\d+ claimed\s+muse\s+`+id).MatchString(got) {
		t.Fatalf("trace missing audit events: %q", got)
	}
	// A second check finds nothing: the request was claimed.
	if got := call(t, muse, "check_inbox", nil); !strings.Contains(got, "No requests waiting") {
		t.Fatalf("second inbox = %q", got)
	}
}

// An agent handling two requests names the one its ask continues; the relay
// cannot guess with two open claims.
func TestAskWithParentIDContinuesChain(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: 3 * time.Second})
	inst, grok, muse := session(t, m, "instinct"), session(t, m, "grokbot"), session(t, m, "muse")
	call(t, inst, "ask", map[string]any{"to": "muse", "message": "call the dentist", "wait_seconds": 1})
	call(t, grok, "ask", map[string]any{"to": "muse", "message": "something else", "wait_seconds": 1})
	inbox := call(t, muse, "check_inbox", nil)
	var id string
	for _, block := range strings.Split(inbox, "Request ")[1:] {
		if strings.Contains(block, "call the dentist") {
			id, _, _ = strings.Cut(block, " from")
		}
	}
	if id == "" {
		t.Fatalf("inbox = %q", inbox)
	}
	out := call(t, muse, "ask", map[string]any{"to": "grokbot", "message": "confirm Tue 3pm", "wait_seconds": 1, "parent_id": id})
	if strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("ask with parent_id = %q", out)
	}
	if got := call(t, muse, "trace", map[string]any{"trace_id": id}); !strings.Contains(got, "hop 2: muse -> grokbot") {
		t.Fatalf("trace = %q", got)
	}
}

func TestListAgentsShowsWake(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	out := call(t, session(t, m, "grokbot"), "list_agents", nil)
	if !strings.Contains(out, "muse: offline, wake=none, never seen") {
		t.Fatalf("list_agents = %q", out)
	}
}

func TestErrorsComeBackAsToolErrors(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	out := call(t, session(t, m, "grokbot"), "ask", map[string]any{"to": "nobody", "message": "x", "wait_seconds": 1})
	if !strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "no such agent") {
		t.Fatalf("ask to unknown agent = %q", out)
	}
}

// Parity: the MCP ask and the client ask render the same result.
func TestMCPAndClientAgree(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: time.Second})
	viaMCP := call(t, session(t, m, "grokbot"), "ask", map[string]any{"to": "muse", "message": "x", "wait_seconds": 1})
	res, err := m.Client(t, "grokbot").Ask(context.Background(), "muse", "x", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	viaCLI := client.FormatResult(res)
	norm := func(s string) string { return strings.Split(strings.TrimSpace(s), " Request id")[0] }
	if norm(viaMCP) != norm(viaCLI) {
		t.Fatalf("mcp %q vs client %q", viaMCP, viaCLI)
	}
}

func TestNotifySendsWithoutWaiting(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	start := time.Now()
	out := call(t, session(t, m, "muse"), "ask", map[string]any{"to": "grokbot", "message": "new time Tue 3pm", "notify": true})
	if !strings.Contains(out, "Sent to grokbot") || time.Since(start) > 2*time.Second {
		t.Fatalf("notify = %q after %v", out, time.Since(start))
	}
	in, _ := m.Client(t, "grokbot").Poll(context.Background(), 0)
	if len(in.Requests) != 1 || in.Requests[0].Kind != envelope.KindNotify {
		t.Fatalf("grokbot got %+v", in)
	}
}

func between(s, a, b string) string {
	_, rest, _ := strings.Cut(s, a)
	out, _, _ := strings.Cut(rest, b)
	return out
}

func kitFrom(t *testing.T, out string) onboard.Kit {
	t.Helper()
	var k onboard.Kit
	if err := json.Unmarshal([]byte(out), &k); err != nil {
		t.Fatalf("onboard output is not a Kit: %v: %q", err, out)
	}
	return k
}

func blockNames(k onboard.Kit) []string {
	var names []string
	for _, a := range k.Agents {
		names = append(names, a.Name)
	}
	return names
}

// The onboard tool returns the kit as JSON, with one agent block per
// list_agents entry and the relay URL the agent is connected to.
func TestOnboardAgentsMatchListAgents(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	cs := session(t, m, "grokbot")
	var listed []string
	for line := range strings.SplitSeq(strings.TrimSpace(call(t, cs, "list_agents", nil)), "\n") {
		name, _, _ := strings.Cut(line, ":")
		listed = append(listed, name)
	}
	k := kitFrom(t, call(t, cs, "onboard", nil))
	if got := blockNames(k); !slices.Equal(got, listed) || len(got) != 3 {
		t.Fatalf("onboard agents %v, list_agents %v", got, listed)
	}
	if k.RelayURL != m.URL("grokbot") || k.Operator == "" || len(k.Recipes) == 0 {
		t.Fatalf("kit = relay %q, operator %d bytes, %d recipes", k.RelayURL, len(k.Operator), len(k.Recipes))
	}
}

func TestOnboardArgs(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	cs := session(t, m, "muse")
	k := kitFrom(t, call(t, cs, "onboard", map[string]any{
		"section": "agents", "operator": "grokbot", "owner": "Matt", "kinds": map[string]any{"muse": "proxy-sandbox"},
	}))
	if k.Host != "grokbot" || k.Owner != "Matt" || k.Operator != "" || len(k.Recipes) != 0 {
		t.Fatalf("section agents kit: host %q owner %q operator %d bytes, %d recipes", k.Host, k.Owner, len(k.Operator), len(k.Recipes))
	}
	for _, a := range k.Agents {
		if a.Name == "muse" && a.Kind != "proxy-sandbox" {
			t.Fatalf("kinds override ignored: %+v", a)
		}
	}
	op := kitFrom(t, call(t, cs, "onboard", map[string]any{"section": "operator"}))
	if op.Operator == "" || len(op.Agents) != 0 || len(op.Recipes) != 0 {
		t.Fatalf("section operator kit: %+v", op)
	}
	if out := call(t, cs, "onboard", map[string]any{"section": "everything"}); !strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("bad section = %q", out)
	}
	if out := call(t, cs, "onboard", map[string]any{"kinds": map[string]any{"muse": "nope"}}); !strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("bad kind = %q", out)
	}
}

// Each call reads the live roster: a new agent shows up on the next call.
func TestOnboardReadsLiveRoster(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	cs := session(t, m, "grokbot")
	before := blockNames(kitFrom(t, call(t, cs, "onboard", nil)))
	m.JoinOnMachineOf(t, "muse", "codex")
	after := kitFrom(t, call(t, cs, "onboard", nil))
	if slices.Contains(before, "codex") || !slices.Contains(blockNames(after), "codex") {
		t.Fatalf("before %v, after %v", before, blockNames(after))
	}
	for _, a := range after.Agents {
		if a.Name == "codex" && a.Kind != onboard.KindCodex {
			t.Fatalf("codex kind = %q", a.Kind)
		}
	}
}

// recorder is a Backend that records every call and fails the mutating ones.
type recorder struct {
	calls []string
}

func (r *recorder) Ask(context.Context, string, string, string, time.Duration) (client.Result, error) {
	r.calls = append(r.calls, "Ask")
	return client.Result{}, nil
}

func (r *recorder) Send(context.Context, string, string, envelope.Kind, string) (envelope.Request, error) {
	r.calls = append(r.calls, "Send")
	return envelope.Request{}, nil
}

func (r *recorder) Get(context.Context, string, time.Duration) (client.Result, error) {
	r.calls = append(r.calls, "Get")
	return client.Result{}, nil
}

func (r *recorder) Poll(context.Context, time.Duration) (client.Inbox, error) {
	r.calls = append(r.calls, "Poll")
	return client.Inbox{}, nil
}

func (r *recorder) AckReplies(context.Context, []string) error {
	r.calls = append(r.calls, "AckReplies")
	return nil
}

func (r *recorder) Claim(context.Context, string) (envelope.Request, error) {
	r.calls = append(r.calls, "Claim")
	return envelope.Request{}, nil
}

func (r *recorder) Reply(context.Context, string, string, envelope.Status) (envelope.Reply, error) {
	r.calls = append(r.calls, "Reply")
	return envelope.Reply{}, nil
}

func (r *recorder) Cancel(context.Context, string) error {
	r.calls = append(r.calls, "Cancel")
	return nil
}

func (r *recorder) Agents(context.Context) ([]client.AgentInfo, error) {
	r.calls = append(r.calls, "Agents")
	return []client.AgentInfo{{Name: "hermes", Wake: "webhook", Kind: "hermes"}, {Name: "muse", Wake: "wait"}}, nil
}

func (r *recorder) Raw(_ context.Context, method, path string, _, _ any) error {
	r.calls = append(r.calls, "Raw "+method+" "+path)
	return nil
}

func (r *recorder) Base() string { return "http://tincan-relay" }

// Onboarding only reads the roster: one Agents call, nothing else.
func TestOnboardOnlyReadsRoster(t *testing.T) {
	rec := &recorder{}
	srvT, cliT := mcp.NewInMemoryTransports()
	ss, err := mcpserver.New(rec, "test").Connect(t.Context(), srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(t.Context(), cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	k := kitFrom(t, call(t, cs, "onboard", nil))
	if !slices.Equal(rec.calls, []string{"Agents"}) {
		t.Fatalf("onboard made calls %v, want only Agents", rec.calls)
	}
	if k.RelayURL != "http://tincan-relay" || !slices.Equal(blockNames(k), []string{"hermes", "muse"}) || k.Agents[0].Kind != "hermes" {
		t.Fatalf("kit = %+v", k)
	}
}

// check_inbox shows replies to the agent's own asks first, under their own
// heading, then new requests, and marks the replies seen.
func TestCheckInboxShowsRepliesFirst(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	grok, muse := m.Client(t, "grokbot"), m.Client(t, "muse")
	req, _ := grok.Send(ctx, "muse", "call the garage", envelope.KindAsk, "")
	muse.Claim(ctx, req.ID)
	muse.Reply(ctx, req.ID, "no slots this week", envelope.StatusDeclined)
	m.Client(t, "instinct").Send(ctx, "grokbot", "summarize the report", envelope.KindAsk, "")

	cs := session(t, m, "grokbot")
	got := call(t, cs, "check_inbox", nil)
	heading, reply, request := strings.Index(got, "Replies to your requests:"), strings.Index(got, "no slots this week"), strings.Index(got, "summarize the report")
	if heading < 0 || reply < heading || request < reply || !strings.Contains(got, req.ID) || !strings.Contains(got, "declined") {
		t.Fatalf("check_inbox = %q", got)
	}
	if again := call(t, cs, "check_inbox", nil); strings.Contains(again, "no slots") || !strings.Contains(again, "No requests waiting") {
		t.Fatalf("second check_inbox = %q", again)
	}
}

var testPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 64)...)

// attachMesh is a mesh whose relay stores attachments.
func attachMesh(t *testing.T, cfg relay.Config) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, cfg)
	m.Server.SetAttachmentDir(t.TempDir())
	return m
}

// fileSession is session with local files on: attach paths are read and
// non-image attachments are saved under dir.
func fileSession(t *testing.T, m *testrelay.Mesh, agent, dir string) *mcp.ClientSession {
	t.Helper()
	srvT, cliT := mcp.NewInMemoryTransports()
	srv := mcpserver.New(m.Client(t, agent), "test", mcpserver.LocalFiles(dir))
	ss, err := srv.Connect(t.Context(), srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(t.Context(), cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// callFull returns a tool's text and its image contents.
func callFull(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, []*mcp.ImageContent) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	var imgs []*mcp.ImageContent
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			b.WriteString(c.Text)
		case *mcp.ImageContent:
			imgs = append(imgs, c)
		}
	}
	if res.IsError {
		return "ERROR: " + b.String(), imgs
	}
	return b.String(), imgs
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A reply with an attached PNG reaches the asker as image content, in
// check_inbox and in get_reply.
func TestReplyImageShowsAsImageContent(t *testing.T) {
	m := attachMesh(t, relay.Config{MaxWait: 3 * time.Second})
	grok := fileSession(t, m, "grokbot", filepath.Join(t.TempDir(), "grok"))
	muse := fileSession(t, m, "muse", filepath.Join(t.TempDir(), "muse"))

	call(t, grok, "ask", map[string]any{"to": "muse", "message": "send the chart", "wait_seconds": 1})
	id := between(call(t, muse, "check_inbox", nil), "Request ", " from")
	img := writeFile(t, "chart.png", testPNG)
	if got := call(t, muse, "reply", map[string]any{"request_id": id, "message": "here it is", "attach": []string{img}}); !strings.Contains(got, "Replied") {
		t.Fatalf("reply = %q", got)
	}

	for _, tool := range []string{"check_inbox", "get_reply"} {
		var args map[string]any
		if tool == "get_reply" {
			args = map[string]any{"request_id": id}
		}
		out, imgs := callFull(t, grok, tool, args)
		if !strings.Contains(out, "here it is") || !strings.Contains(out, "chart.png") {
			t.Fatalf("%s text = %q", tool, out)
		}
		if len(imgs) != 1 || imgs[0].MIMEType != "image/png" || !bytes.Equal(imgs[0].Data, testPNG) {
			t.Fatalf("%s images = %+v", tool, imgs)
		}
	}
}

// An image the asker attaches shows to the target in check_inbox.
func TestAskImageShowsInTargetInbox(t *testing.T) {
	m := attachMesh(t, relay.Config{})
	grok := fileSession(t, m, "grokbot", t.TempDir())
	muse := fileSession(t, m, "muse", t.TempDir())
	out := call(t, grok, "ask", map[string]any{"to": "muse", "message": "what is this", "notify": true, "attach": []string{writeFile(t, "x.png", testPNG)}})
	if !strings.Contains(out, "Sent to muse") {
		t.Fatalf("ask = %q", out)
	}
	text, imgs := callFull(t, muse, "check_inbox", nil)
	if !strings.Contains(text, "what is this") || len(imgs) != 1 || !bytes.Equal(imgs[0].Data, testPNG) {
		t.Fatalf("inbox = %q, %d images", text, len(imgs))
	}
}

// Non-image attachments are saved as <id>.<ext> in the agent's directory,
// 0600 in a 0700 dir, whatever name the uploader gave them.
func TestNonImageSavedByIDInsideDir(t *testing.T) {
	m := attachMesh(t, relay.Config{})
	ctx := t.Context()
	root := t.TempDir()
	dir := filepath.Join(root, "attachments", "grokbot")
	grok := fileSession(t, m, "grokbot", dir)
	museC := m.Client(t, "muse")

	req, err := m.Client(t, "grokbot").Send(ctx, "muse", "send the notes", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := museC.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, name := range []string{"../../escape.txt", "/etc/evil.txt", "..", "notes.pdf"} {
		body, mime := "hello notes", "text/plain"
		if name == "notes.pdf" {
			body, mime = "%PDF-1.4 fake", "application/pdf"
		}
		up, err := museC.UploadAttachment(ctx, name, mime, strings.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, up.ID)
	}
	if _, err := museC.ReplyAttached(ctx, req.ID, "notes attached", envelope.StatusAnswered, ids); err != nil {
		t.Fatal(err)
	}

	out, imgs := callFull(t, grok, "check_inbox", nil)
	if len(imgs) != 0 {
		t.Fatalf("non-images came back as images: %d", len(imgs))
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir = %v, %v", st, err)
	}
	want := map[string]bool{ids[0] + ".txt": true, ids[1] + ".txt": true, ids[2] + ".txt": true, ids[3] + ".pdf": true}
	entries, _ := os.ReadDir(dir)
	if len(entries) != len(want) {
		t.Fatalf("dir holds %v, want %v", entries, want)
	}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Fatalf("unexpected file %q", e.Name())
		}
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", e.Name(), info.Mode().Perm())
		}
		if !strings.Contains(out, filepath.Join(dir, e.Name())) {
			t.Fatalf("inbox does not report %s:\n%s", e.Name(), out)
		}
	}
	// Nothing escaped: the root holds only the attachments tree.
	if es, _ := os.ReadDir(root); len(es) != 1 || es[0].Name() != "attachments" {
		t.Fatalf("root holds %v", es)
	}
	if es, _ := os.ReadDir(filepath.Join(root, "attachments")); len(es) != 1 {
		t.Fatalf("attachments holds %v", es)
	}
	if !strings.Contains(out, "application/pdf") || !strings.Contains(out, "escape.txt") {
		t.Fatalf("inbox should show mime and display name:\n%s", out)
	}
}

// Against a relay that stores no attachments, ask and reply with attach
// fail before anything is sent.
func TestAttachAgainstRelayWithoutSupport(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := t.Context()
	grok := fileSession(t, m, "grokbot", t.TempDir())
	img := writeFile(t, "a.png", testPNG)
	out := call(t, grok, "ask", map[string]any{"to": "muse", "message": "look", "attach": []string{img}})
	if !strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "does not support attachments") {
		t.Fatalf("ask = %q", out)
	}
	if n, _ := m.Store.CountQueued(ctx, "muse"); n != 0 {
		t.Fatalf("refused ask queued %d requests", n)
	}
	req, _ := m.Client(t, "muse").Send(ctx, "grokbot", "send it", envelope.KindAsk, "")
	m.Client(t, "grokbot").Claim(ctx, req.ID)
	out = call(t, grok, "reply", map[string]any{"request_id": req.ID, "message": "here", "attach": []string{img}})
	if !strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "does not support attachments") {
		t.Fatalf("reply = %q", out)
	}
	if res, _ := m.Client(t, "muse").Get(ctx, req.ID, 0); res.Reply != nil {
		t.Fatalf("refused reply was sent: %+v", res.Reply)
	}
}

// Bad attach lists fail before sending: a missing file, a file over the
// relay's cap, and more files than a message carries.
func TestAttachEdgeCases(t *testing.T) {
	m := attachMesh(t, relay.Config{Attachments: relay.AttachmentConfig{MaxFileBytes: 1024}})
	ctx := t.Context()
	grok := fileSession(t, m, "grokbot", t.TempDir())
	small := writeFile(t, "s.txt", []byte("hi"))
	many := make([]string, envelope.MaxAttachments+1)
	for i := range many {
		many[i] = small
	}
	for name, tc := range map[string]struct {
		paths []string
		want  string
	}{
		"missing":  {[]string{filepath.Join(t.TempDir(), "nope.png")}, "nope.png"},
		"oversize": {[]string{writeFile(t, "big.bin", bytes.Repeat([]byte("x"), 2048))}, "larger than 1024 bytes"},
		"too many": {many, "too many attachments"},
		"dir":      {[]string{t.TempDir()}, "not a regular file"},
	} {
		out := call(t, grok, "ask", map[string]any{"to": "muse", "message": "x", "attach": tc.paths})
		if !strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, tc.want) {
			t.Fatalf("%s: ask = %q, want error containing %q", name, out, tc.want)
		}
	}
	if n, _ := m.Store.CountQueued(ctx, "muse"); n != 0 {
		t.Fatalf("refused asks queued %d requests", n)
	}
}

// A server without local files (the gateway) never reads local paths.
func TestAttachNeedsLocalFiles(t *testing.T) {
	m := attachMesh(t, relay.Config{})
	out := call(t, session(t, m, "grokbot"), "ask", map[string]any{"to": "muse", "message": "x", "attach": []string{writeFile(t, "a.png", testPNG)}})
	if !strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "local files") {
		t.Fatalf("ask = %q", out)
	}
	if n, _ := m.Store.CountQueued(t.Context(), "muse"); n != 0 {
		t.Fatalf("queued %d", n)
	}
}

// uploadTo sends a request from sender to target carrying a PNG and a text
// file, and returns their attachment ids.
func uploadTo(t *testing.T, m *testrelay.Mesh, sender, target string) (png, txt string) {
	t.Helper()
	ctx := t.Context()
	c := m.Client(t, sender)
	ip, err := c.UploadAttachment(ctx, "chart.png", "image/png", bytes.NewReader(testPNG), int64(len(testPNG)))
	if err != nil {
		t.Fatal(err)
	}
	body := "hello notes"
	it, err := c.UploadAttachment(ctx, "notes.txt", "text/plain", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendAttached(ctx, target, "see attached", envelope.KindAsk, "", []string{ip.ID, it.ID}); err != nil {
		t.Fatal(err)
	}
	return ip.ID, it.ID
}

// emptyDirs points HOME, the config dir and the working directory at fresh
// directories and returns a check that nothing was written to any of them.
func emptyDirs(t *testing.T) func() {
	t.Helper()
	home, cfg, wd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("TINCAN_CONFIG", filepath.Join(cfg, "tincan", "config.json"))
	t.Chdir(wd)
	return func() {
		t.Helper()
		for _, d := range []string{home, cfg, wd} {
			if es, _ := os.ReadDir(d); len(es) != 0 {
				t.Fatalf("%s holds %v, want nothing written", d, es)
			}
		}
	}
}

// A server without local files (the gateway's shape) shows images it
// receives, says other files are not saved here, writes nothing to disk,
// and its instructions do not promise local attach or saving.
func TestGatewayShapeReceivesAttachments(t *testing.T) {
	m := attachMesh(t, relay.Config{MaxWait: 3 * time.Second})
	checkEmpty := emptyDirs(t)
	grok := session(t, m, "grokbot")

	instr := grok.InitializeResult().Instructions
	if strings.Contains(instr, "ask and reply take attach") || strings.Contains(instr, "saved on this machine") {
		t.Fatalf("gateway instructions claim local attach or saving:\n%s", instr)
	}
	for _, want := range []string{"get_attachment", "not saved on this server", "Attaching local files is not available"} {
		if !strings.Contains(instr, want) {
			t.Fatalf("gateway instructions missing %q:\n%s", want, instr)
		}
	}

	// A request carrying both kinds, through check_inbox.
	png, txt := uploadTo(t, m, "muse", "grokbot")
	out, imgs := callFull(t, grok, "check_inbox", nil)
	if len(imgs) != 1 || !bytes.Equal(imgs[0].Data, testPNG) || imgs[0].MIMEType != "image/png" {
		t.Fatalf("check_inbox images = %+v", imgs)
	}
	if !strings.Contains(out, png) || !strings.Contains(out, txt+` "notes.txt" (text/plain, 11 bytes): not saved here`) {
		t.Fatalf("check_inbox = %q", out)
	}

	// A reply carrying both kinds, through get_reply.
	ctx := t.Context()
	req, err := m.Client(t, "grokbot").Send(ctx, "instinct", "send both", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	instC := m.Client(t, "instinct")
	if _, err := instC.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	ups, err := instC.UploadFiles(ctx, []string{writeFile(t, "r.png", testPNG), writeFile(t, "r.txt", []byte("reply notes"))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instC.ReplyAttached(ctx, req.ID, "both attached", envelope.StatusAnswered, client.AttachmentIDs(ups)); err != nil {
		t.Fatal(err)
	}
	out, imgs = callFull(t, grok, "get_reply", map[string]any{"request_id": req.ID})
	if len(imgs) != 1 || !bytes.Equal(imgs[0].Data, testPNG) {
		t.Fatalf("get_reply images = %d", len(imgs))
	}
	if !strings.Contains(out, "both attached") || !strings.Contains(out, ups[1].ID+` "r.txt" (text/plain, 11 bytes): not saved here`) {
		t.Fatalf("get_reply = %q", out)
	}
	checkEmpty()
}

// A server with local files keeps the attach and save wording and names
// get_attachment.
func TestLocalFilesInstructions(t *testing.T) {
	m := attachMesh(t, relay.Config{})
	instr := fileSession(t, m, "grokbot", t.TempDir()).InitializeResult().Instructions
	for _, want := range []string{"ask and reply take attach", "saved on this machine", "get_attachment"} {
		if !strings.Contains(instr, want) {
			t.Fatalf("local instructions missing %q:\n%s", want, instr)
		}
	}
	if strings.Contains(instr, "not saved on this server") {
		t.Fatalf("local instructions say files are not saved:\n%s", instr)
	}
	if !strings.Contains(mcpserver.Instructions, "get_attachment") {
		t.Fatal("Instructions should name get_attachment")
	}
}

// get_attachment fetches one attachment again: an image as image content,
// another file saved with local files on or "not saved here" without.
func TestGetAttachment(t *testing.T) {
	m := attachMesh(t, relay.Config{})
	png, txt := uploadTo(t, m, "muse", "grokbot")
	gw := session(t, m, "grokbot")

	out, imgs := callFull(t, gw, "get_attachment", map[string]any{"id": png})
	if strings.HasPrefix(out, "ERROR:") || len(imgs) != 1 || !bytes.Equal(imgs[0].Data, testPNG) || !strings.Contains(out, png) {
		t.Fatalf("image: %q, %d images", out, len(imgs))
	}
	out, imgs = callFull(t, gw, "get_attachment", map[string]any{"id": txt})
	if len(imgs) != 0 || !strings.Contains(out, txt+" (text/plain, 11 bytes): not saved here") {
		t.Fatalf("gateway text file: %q, %d images", out, len(imgs))
	}

	dir := filepath.Join(t.TempDir(), "files")
	local := fileSession(t, m, "grokbot", dir)
	out = call(t, local, "get_attachment", map[string]any{"id": txt})
	p := filepath.Join(dir, txt+".txt")
	if !strings.Contains(out, "saved to "+p) {
		t.Fatalf("local text file: %q", out)
	}
	if data, err := os.ReadFile(p); err != nil || string(data) != "hello notes" {
		t.Fatalf("saved file = %q, %v", data, err)
	}

	// Someone else's attachment is refused by the relay.
	other, _ := uploadTo(t, m, "muse", "instinct")
	if out := call(t, gw, "get_attachment", map[string]any{"id": other}); !strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "get_attachment") {
		t.Fatalf("foreign attachment = %q", out)
	}
}

// fetcher is a recorder that also moves attachments, serving fetches from
// a map and failing ids it does not hold.
type fetcher struct {
	recorder
	blobs   map[string][]byte
	fetched []string
	inbox   client.Inbox
}

func (f *fetcher) Poll(context.Context, time.Duration) (client.Inbox, error) { return f.inbox, nil }

func (f *fetcher) UploadFiles(context.Context, []string) ([]client.UploadedAttachment, error) {
	return nil, nil
}

func (f *fetcher) SendAttached(context.Context, string, string, envelope.Kind, string, []string) (envelope.Request, error) {
	return envelope.Request{}, nil
}

func (f *fetcher) AskAttached(context.Context, string, string, string, []string, time.Duration) (client.Result, error) {
	return client.Result{}, nil
}

func (f *fetcher) ReplyAttached(context.Context, string, string, envelope.Status, []string) (envelope.Reply, error) {
	return envelope.Reply{}, nil
}

func (f *fetcher) FetchAttachment(_ context.Context, id string) ([]byte, client.DownloadedAttachment, error) {
	f.fetched = append(f.fetched, id)
	data, ok := f.blobs[id]
	if !ok {
		return nil, client.DownloadedAttachment{}, errors.New("relay unreachable")
	}
	return data, client.DownloadedAttachment{ID: id, MIME: "text/plain", Size: int64(len(data))}, nil
}

func connect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	srvT, cliT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(t.Context(), srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(t.Context(), cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// get_attachment checks the id with the rule the client uses to name saved
// files, before any fetch, and reports a failed fetch as a tool error.
func TestGetAttachmentInvalidIDAndFetchFailure(t *testing.T) {
	f := &fetcher{blobs: map[string][]byte{}}
	cs := connect(t, mcpserver.New(f, "test"))
	dir := t.TempDir()
	for _, id := range []string{"", "../etc/passwd", "a/b", "a.b", "with space", strings.Repeat("a", 65), "ok_ID-1", strings.Repeat("b", 64)} {
		_, saveErr := client.SaveAttachmentFile(dir, id, "text/plain", nil)
		clientOK := saveErr == nil
		f.fetched = nil
		out := call(t, cs, "get_attachment", map[string]any{"id": id})
		fetched := len(f.fetched) > 0
		if clientOK != fetched {
			t.Fatalf("id %q: client accepts=%v, get_attachment fetched=%v (%q)", id, clientOK, fetched, out)
		}
		if !clientOK && (!strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "attachment id")) {
			t.Fatalf("invalid id %q = %q", id, out)
		}
		if clientOK && (!strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "relay unreachable") || !strings.Contains(out, "get_attachment")) {
			t.Fatalf("fetch failure for %q = %q", id, out)
		}
	}
}

// When check_inbox cannot fetch an attachment of a request it just claimed,
// the notice names get_attachment and the id so the agent can retry.
func TestFetchFailureNoticeNamesGetAttachment(t *testing.T) {
	f := &fetcher{inbox: client.Inbox{Requests: []envelope.Request{{
		ID: "req1", From: "muse", To: "grokbot", Kind: envelope.KindAsk, Body: "see file",
		Attachments: []envelope.Attachment{{ID: "att_missing", Name: "notes.txt", MIME: "text/plain"}},
	}}}}
	out := call(t, connect(t, mcpserver.New(f, "test")), "check_inbox", nil)
	if !strings.Contains(out, "could not fetch it") || !strings.Contains(out, `get_attachment with id "att_missing"`) {
		t.Fatalf("check_inbox = %q", out)
	}
}
