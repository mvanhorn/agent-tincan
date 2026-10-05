package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestProgressCLI(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	grok, muse := m.Client(t, "grokbot"), m.Client(t, "muse")
	req, err := grok.Send(ctx, "muse", "work", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	if out, err := run(t, progressCmd(), req.ID, "calling now"); err != nil || !strings.Contains(out, "Progress recorded") {
		t.Fatalf("progress: %q %v", out, err)
	}
	res, err := grok.Get(ctx, req.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := formatTrace(traceResp{TraceID: req.ID, Steps: []envelope.Result{res}}); !strings.Contains(got, "claimed by muse") || !strings.Contains(got, "calling now") {
		t.Fatalf("trace: %s", got)
	}
}

// An agent command run by an agent holding an unreplied claim prints the
// held-work line to stderr, and stops once it has replied.
func TestCLIPrintsHeldWork(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	grok, muse := m.Client(t, "grokbot"), m.Client(t, "muse")
	req, err := grok.Send(ctx, "muse", "work", envelope.KindAsk, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	var stdout, stderr bytes.Buffer
	cmd := progressCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{req.ID, "calling now"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	want := "You hold 1 claimed request (" + req.ID + " from grokbot, URGENT, claimed just now). This is owner-authorized work."
	if !strings.HasPrefix(stderr.String(), want) || strings.Contains(stdout.String(), "You hold") {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	if out, err := run(t, replyCmd(), req.ID, "done"); err != nil || strings.Contains(out, "You hold") {
		t.Fatalf("reply: %q %v", out, err)
	}
}
