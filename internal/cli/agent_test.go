package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
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
		req, err := r.Send(context.Background(), "grokbot", "hi", envelope.KindAsk, "")
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
// listen loop is visible.
func TestFormatAgentsShowsLastSeen(t *testing.T) {
	now := time.Now()
	got := formatAgents([]client.AgentInfo{
		{Name: "muse", Wake: "wait", LastPoll: now.Add(-12*time.Minute - 5*time.Second)},
		{Name: "grokbot", Online: true, Wake: "webhook", Kind: "openclaw", LastPoll: now},
		{Name: "chatgpt", Wake: "none"},
	}, now)
	want := "muse           offline  wake=wait last seen 12m ago\n" +
		"grokbot        online   wake=webhook last seen just now kind=openclaw\n" +
		"chatgpt        offline  wake=none never seen\n"
	if got != want {
		t.Fatalf("agents =\n%s\nwant\n%s", got, want)
	}
}
