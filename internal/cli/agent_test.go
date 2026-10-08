package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// A second agent on a machine joins with its own TINCAN_CONFIG. The first
// agent's config is untouched and each config sends as its own agent.
func TestSecondAgentJoinKeepsFirstConfig(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	dir := t.TempDir()
	first, second := filepath.Join(dir, "claude-code.json"), filepath.Join(dir, "codex.json")
	url := m.URL("muse") // the machine muse runs on
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")

	t.Setenv("TINCAN_CONFIG", first)
	if err := client.SaveConfig(client.Config{Relay: url, Agent: "muse"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(first)

	t.Setenv("TINCAN_CONFIG", second)
	cmd := joinCmd()
	cmd.SetArgs([]string{m.Invite(t, "codex"), "--relay", url})
	cmd.SetOut(new(nopWriter))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("join codex: %v", err)
	}
	cfg, err := client.LoadConfig()
	if err != nil || cfg.Agent != "codex" || cfg.Relay != url {
		t.Fatalf("codex config = %+v, %v", cfg, err)
	}
	if after, _ := os.ReadFile(first); string(after) != string(before) {
		t.Fatalf("first agent's config changed:\n%s\n->\n%s", before, after)
	}

	for path, want := range map[string]string{first: "muse", second: "codex"} {
		t.Setenv("TINCAN_CONFIG", path)
		r, _, err := connect()
		if err != nil {
			t.Fatal(err)
		}
		req, err := r.Send(context.Background(), "grokbot", "hi", envelope.KindAsk, "", false)
		if err != nil || req.From != want {
			t.Fatalf("config %s sent as %q, %v; want %s", filepath.Base(path), req.From, err, want)
		}
	}
}

type nopWriter struct{}

func (*nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// A default config that already names an agent is not silently repointed by
// a join for a different agent: the first agent would start acting as the
// new one. The join is refused, the config is untouched, and the error says
// how to give the second agent its own config.
func TestJoinDifferentAgentOnSameConfigIsRefused(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	url := m.URL("muse")
	useConfig(t, client.Config{Relay: url, Agent: "muse"})
	path := client.ConfigPath()
	before, _ := os.ReadFile(path)

	_, err := run(t, joinCmd(), m.Invite(t, "codex"), "--relay", url)
	if err == nil {
		t.Fatal("join as codex over muse's config should be refused")
	}
	for _, want := range []string{`"muse"`, path, "TINCAN_CONFIG", "--replace"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("config changed on refusal:\n%s\n->\n%s", before, after)
	}

	// The refused agent was admitted by the relay, so it can still get its
	// own config without a new invite, as the error says.
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "codex.json"))
	if _, err := run(t, Root(), "rejoin", "--relay", url, "--name", "codex"); err != nil {
		t.Fatalf("rejoin codex into its own config: %v", err)
	}
	if cfg, _ := client.LoadConfig(); cfg.Agent != "codex" {
		t.Fatalf("codex config = %+v", cfg)
	}
}

func TestJoinReplaceRepointsConfig(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	url := m.URL("muse")
	useConfig(t, client.Config{Relay: url, Agent: "muse"})
	if _, err := run(t, joinCmd(), m.Invite(t, "codex"), "--relay", url, "--replace"); err != nil {
		t.Fatalf("join --replace: %v", err)
	}
	if cfg, _ := client.LoadConfig(); cfg.Agent != "codex" {
		t.Fatalf("config after --replace = %+v", cfg)
	}
}

func TestJoinSameAgentAgainNeedsNoReplace(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	url := m.URL("muse")
	useConfig(t, client.Config{Relay: url, Agent: "muse"})
	if _, err := run(t, joinCmd(), m.Invite(t, "muse"), "--relay", url); err != nil {
		t.Fatalf("rejoin same name: %v", err)
	}
	if cfg, _ := client.LoadConfig(); cfg.Agent != "muse" || cfg.Relay != url {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestInviteNextStepMentionsSecondAgentConfig(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	out, err := run(t, inviteCmd(), "codex", "--relay", m.URL("admin"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "for a second agent on a machine that already runs one, prefix with TINCAN_CONFIG=<new file>") {
		t.Fatalf("invite output = %q", out)
	}
}

// tincan agents shows how long ago each agent last polled, so a dead wait or
// listen loop is visible, and the build each agent last called with, so one
// that has not upgraded is visible too.
func TestFormatAgentsShowsLastSeen(t *testing.T) {
	now := time.Now()
	got := formatAgents([]client.AgentInfo{
		{Name: "muse", Wake: "wait", LastPoll: now.Add(-12*time.Minute - 5*time.Second)},
		{Name: "grokbot", Online: true, Wake: "webhook", Kind: "openclaw", LastPoll: now, Version: "0.5.2"},
		{Name: "chatgpt", Wake: "none"},
	}, now)
	want := "muse           offline  wake=wait last seen 12m ago\n" +
		"grokbot        online   wake=webhook last seen just now kind=openclaw version=0.5.2\n" +
		"chatgpt        offline  wake=none never seen\n"
	if got != want {
		t.Fatalf("agents =\n%s\nwant\n%s", got, want)
	}
}

// The relay's own build heads the list when the relay reports one, so an
// agent behind it stands out; a relay that predates it adds no line.
func TestFormatRosterNamesRelayVersion(t *testing.T) {
	now := time.Now()
	agents := []client.AgentInfo{{Name: "muse", Wake: "wait", Version: "0.5.1"}}
	got := formatRoster(client.Roster{Agents: agents, RelayVersion: "0.5.2"}, now)
	want := "# relay version 0.5.2\nmuse           offline  wake=wait never seen version=0.5.1\n"
	if got != want {
		t.Fatalf("roster =\n%s\nwant\n%s", got, want)
	}
	if got := formatRoster(client.Roster{Agents: agents}, now); got != formatAgents(agents, now) {
		t.Fatalf("roster without a relay version =\n%s", got)
	}
}

// tincan join saves the relay key and addresses at once, not on the next
// command: a service agent (history serve, web serve) never runs one.
func TestJoinSavesRelayKey(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetURLs([]string{"http://tincan-relay"})
	useConfig(t, client.Config{})
	if _, err := run(t, joinCmd(), m.Invite(t, "hermes"), "--relay", m.URL("stranger")); err != nil {
		t.Fatalf("join: %v", err)
	}
	cfg, err := client.LoadConfig()
	if err != nil || cfg.Agent != "hermes" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if cfg.RelayKey == "" || cfg.RelayInfoAt.IsZero() || !slices.Equal(cfg.RelayURLs, []string{"http://tincan-relay"}) {
		t.Fatalf("join left the relay info out: %+v", cfg)
	}
	r, err := client.NewRelayFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		RelayKey string `json:"relay_key"`
	}
	if err := r.Raw(context.Background(), "GET", "/v1/whoami", nil, &raw); err != nil || raw.RelayKey != cfg.RelayKey {
		t.Fatalf("saved key %q, relay's %q, %v", cfg.RelayKey, raw.RelayKey, err)
	}
}

func TestAskGroupCLI(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	if err := client.SaveConfig(client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd := askCmd()
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"instinct,muse", "hello", "--wait=0", "--json"})
	err := cmd.ExecuteContext(t.Context())
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("%v: %s", err, out.String())
	}
	var g client.GroupResult
	if err := json.Unmarshal([]byte(out.String()), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Results) != 2 || g.Outcome != "pending" {
		t.Fatalf("%+v", g)
	}
	out.Reset()
	cmd = getCmd()
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{g.Group, "--json"})
	err = cmd.ExecuteContext(t.Context())
	if !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("%v: %s", err, out.String())
	}
}

func TestFormatAgentsBacklog(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		info client.AgentInfo
		want string
	}{
		{"idle", client.AgentInfo{}, ""},
		{"queued", client.AgentInfo{Queued: 2, OldestQueued: now.Add(-14 * time.Minute)}, "2 queued (oldest 14m)"},
		{"claimed", client.AgentInfo{Claimed: 1}, "1 claimed"},
		{"both", client.AgentInfo{Queued: 2, OldestQueued: now.Add(-time.Hour), Claimed: 1}, "2 queued (oldest 1h), 1 claimed"},
		{"days", client.AgentInfo{Queued: 1, OldestQueued: now.Add(-72 * time.Hour)}, "1 queued (oldest 3d)"},
		{"future", client.AgentInfo{Queued: 1, OldestQueued: now.Add(time.Minute)}, "1 queued (oldest 0m)"},
		{"missing time", client.AgentInfo{Queued: 1}, "1 queued"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.Backlog(now); got != tc.want {
				t.Fatalf("Backlog = %q, want %q", got, tc.want)
			}
			got := formatAgents([]client.AgentInfo{tc.info}, now)
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("roster = %q", got)
			}
			if tc.want == "" && got != "               offline  wake= never seen\n" {
				t.Fatalf("idle roster = %q", got)
			}
		})
	}
}

// An agent's good-at line is the last field on its own line, labelled and
// quoted; an agent without one renders exactly as before.
func TestFormatAgentsShowsGoodAtLast(t *testing.T) {
	now := time.Now()
	without := client.AgentInfo{Name: "grokbot", Online: true, Wake: "webhook", Kind: "openclaw", LastPoll: now, Version: "0.5.2"}
	with := without
	with.GoodAt = "phone calls, texts; fast pickup"
	got := formatAgents([]client.AgentInfo{with, {Name: "chatgpt", Wake: "none"}}, now)
	want := `grokbot        online   wake=webhook last seen just now kind=openclaw version=0.5.2 good_at="phone calls, texts; fast pickup"` + "\n" +
		"chatgpt        offline  wake=none never seen\n"
	if got != want {
		t.Fatalf("agents =\n%s\nwant\n%s", got, want)
	}
	// The commas and semicolon stay inside the quotes: the rest of the line
	// is what the agent renders without a line, and the quoted text unquotes
	// back to the owner's line.
	first, _, _ := strings.Cut(got, "\n")
	rest, quoted, ok := strings.Cut(first, " good_at=")
	if !ok || rest+"\n" != formatAgents([]client.AgentInfo{without}, now) {
		t.Fatalf("line without the good-at field = %q", rest)
	}
	if line, err := strconv.Unquote(quoted); err != nil || line != with.GoodAt {
		t.Fatalf("good_at value %s unquotes to %q, %v", quoted, line, err)
	}
}

// For the same roster, tincan agents and list_agents show the same good-at
// text for every agent, owner-set and stock lines alike, and list_agents
// keeps one line per agent that splits on the first ":".
func TestGoodAtParityAcrossCLIAndMCP(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	admin := m.Client(t, "admin")
	code, err := admin.InviteKind(ctx, "history", "history")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "stranger").Join(ctx, code); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.SetGoodAt(ctx, "muse", `phone calls, texts; says "hi"`); err != nil {
		t.Fatal(err)
	}
	agents, err := admin.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}

	useConfig(t, client.Config{Relay: m.URL("admin")})
	cliOut, err := run(t, agentsCmd())
	if err != nil {
		t.Fatal(err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ss, err := mcpserver.New(m.Client(t, "grokbot"), "test").Connect(ctx, srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents"})
	if err != nil {
		t.Fatal(err)
	}
	mcpOut := res.Content[0].(*mcp.TextContent).Text

	cliLines := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(cliOut), "\n") {
		if name, _, ok := strings.Cut(line, " "); ok && !strings.HasPrefix(line, "#") {
			cliLines[name] = line
		}
	}
	mcpLines := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(mcpOut), "\n") {
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("list_agents line without a name: %q", line)
		}
		mcpLines[name] = line
	}
	if len(mcpLines) != len(agents) {
		t.Fatalf("list_agents has %d lines for %d agents:\n%s", len(mcpLines), len(agents), mcpOut)
	}
	lined := 0
	for _, a := range agents {
		field := a.GoodAtField()
		if field == "" {
			if strings.Contains(cliLines[a.Name], "good_at=") || strings.Contains(mcpLines[a.Name], "good_at=") {
				t.Errorf("%s has no line but shows one:\n%s\n%s", a.Name, cliLines[a.Name], mcpLines[a.Name])
			}
			continue
		}
		lined++
		if !strings.HasSuffix(cliLines[a.Name], " "+field) || !strings.HasSuffix(mcpLines[a.Name], ", "+field) {
			t.Errorf("%s good-at differs:\ncli %q\nmcp %q\nwant suffix %s", a.Name, cliLines[a.Name], mcpLines[a.Name], field)
		}
	}
	if lined != 2 {
		t.Fatalf("want the owner line on muse and the stock line on history, got %d lines in %+v", lined, agents)
	}
}

// tincan good-at sets, replaces and clears a line; a line the relay rejects
// reaches the owner as the relay's own message; an unknown agent's 404 is
// not mistaken for an older relay.
func TestGoodAtCommand(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("admin")})
	out, err := run(t, goodAtCmd(), "muse", "phone calls")
	if err != nil || out != "\"muse\" is now good at: phone calls\n" {
		t.Fatalf("set: %q, %v", out, err)
	}
	if out, _ := run(t, agentsCmd()); !strings.Contains(out, `good_at="phone calls"`) {
		t.Fatalf("roster after set:\n%s", out)
	}
	if _, err := run(t, goodAtCmd(), "muse", "phone calls and texts"); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(t, agentsCmd()); !strings.Contains(out, `good_at="phone calls and texts"`) {
		t.Fatalf("roster after replace:\n%s", out)
	}
	out, err = run(t, goodAtCmd(), "muse", "")
	if err != nil || out != "Cleared the good-at line of \"muse\".\n" {
		t.Fatalf("clear: %q, %v", out, err)
	}
	if out, _ := run(t, agentsCmd()); strings.Contains(out, "good_at=") {
		t.Fatalf("roster after clear:\n%s", out)
	}
	// The confirmation reports what the relay stored: surrounding space is
	// trimmed, and a whitespace-only line is a clear.
	out, err = run(t, goodAtCmd(), "muse", "  texts  ")
	if err != nil || out != "\"muse\" is now good at: texts\n" {
		t.Fatalf("padded set: %q, %v", out, err)
	}
	out, err = run(t, goodAtCmd(), "muse", "   ")
	if err != nil || out != "Cleared the good-at line of \"muse\".\n" {
		t.Fatalf("whitespace clear: %q, %v", out, err)
	}

	_, err = run(t, goodAtCmd(), "muse", strings.Repeat("x", 121))
	if err == nil || !strings.Contains(err.Error(), "the good-at line is over 120 characters") || strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("too long: %v", err)
	}
	_, err = run(t, goodAtCmd(), "nobody", "anything")
	if !client.IsStatus(err, http.StatusNotFound) || strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("unknown agent: %v", err)
	}
}

// A relay older than the good-at route answers Go's plain "404 page not
// found", which becomes an upgrade hint; a JSON 404 for an unknown agent
// and other errors pass through unchanged.
func TestOlderRelayGoodAt(t *testing.T) {
	old := &client.APIError{Code: http.StatusNotFound, Message: "404 page not found"}
	err := olderRelayGoodAt(old)
	if !errors.Is(err, old) || !strings.Contains(err.Error(), "older than this tincan") || !strings.Contains(err.Error(), "upgrade the relay") {
		t.Fatalf("older relay: %v", err)
	}
	for _, e := range []error{
		&client.APIError{Code: http.StatusNotFound, Message: `no such agent "nobody"`},
		&client.APIError{Code: http.StatusBadRequest, Message: "the good-at line is over 120 characters"},
		errors.New("dial tcp: connection refused"),
	} {
		if got := olderRelayGoodAt(e); got != e {
			t.Fatalf("%v became %v", e, got)
		}
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	useConfig(t, client.Config{})
	if _, err := run(t, goodAtCmd(), "muse", "phone calls", "--relay", srv.URL); err == nil || !strings.Contains(err.Error(), "upgrade the relay") {
		t.Fatalf("good-at against an older relay: %v", err)
	}
}

// forwardProxy is a forward proxy in front of the test relay. It forwards a
// call whose Proxy-Authorization carries pass and answers 407 otherwise.
func forwardProxy(t *testing.T, pass string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := ""
		if raw, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Basic "); ok {
			if dec, err := base64.StdEncoding.DecodeString(raw); err == nil {
				_, got, _ = strings.Cut(string(dec), ":")
			}
		}
		if got != pass {
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.Header.Del("Proxy-Authorization")
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// With --proxy-credentials-from-env, join dials with the password it was
// given but saves the proxy without it, along with the flag. Without the
// flag the full URL is saved as before.
func TestJoinProxyCredentialsFromEnvSavesNoPassword(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flag      bool
		wantProxy func(host string) string
	}{
		{"flag on", true, func(host string) string { return "http://" + host }},
		{"flag off", false, func(host string) string { return "http://u:join-secret@" + host }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testrelay.New(t, relay.Config{})
			useConfig(t, client.Config{})
			proxy := forwardProxy(t, "join-secret")
			host := strings.TrimPrefix(proxy.URL, "http://")
			args := []string{m.Invite(t, "hermes"), "--relay", m.URL("stranger"), "--proxy", "http://u:join-secret@" + host}
			if tc.flag {
				args = append(args, "--proxy-credentials-from-env")
			}
			out, err := run(t, joinCmd(), args...)
			if err != nil {
				t.Fatalf("join: %v", err)
			}
			cfg, err := client.LoadConfig()
			if err != nil || cfg.Agent != "hermes" {
				t.Fatalf("config = %+v, %v", cfg, err)
			}
			if cfg.Proxy != tc.wantProxy(host) || cfg.ProxyCredentialsFromEnv != tc.flag {
				t.Fatalf("saved proxy %q, credentials from env %v; want %q, %v", cfg.Proxy, cfg.ProxyCredentialsFromEnv, tc.wantProxy(host), tc.flag)
			}
			raw, err := os.ReadFile(client.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if tc.flag && strings.Contains(string(raw)+out, "join-secret") {
				t.Fatalf("the proxy password was saved or printed: %s\n%s", raw, out)
			}
			if tc.flag != strings.Contains(out, "not saved") {
				t.Fatalf("join output = %q", out)
			}
		})
	}
}
