package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
