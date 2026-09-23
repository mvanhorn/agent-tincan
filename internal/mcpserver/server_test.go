package mcpserver_test

import (
	"context"
	"encoding/json"
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
