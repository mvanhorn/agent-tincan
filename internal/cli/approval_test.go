package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestApprovalCommands(t *testing.T) {
	skipOnWindows(t, skipRelayHost)
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	out, err := run(t, askCmd(), "muse", "please call", "--wait", "0s")
	if err != nil || !strings.Contains(out, "waiting for the owner's approval") {
		t.Fatalf("ask: %q %v", out, err)
	}
	held, err := m.Store.Held(t.Context())
	if err != nil || len(held) != 1 {
		t.Fatalf("held: %+v %v", held, err)
	}
	id := held[0].ID
	out, err = run(t, heldCmd(), "--relay", m.URL("admin"))
	if err != nil || !strings.Contains(out, id) || !strings.Contains(out, "grokbot -> muse") || !strings.Contains(out, "please call") {
		t.Fatalf("list: %q %v", out, err)
	}
	socket := adminSocket(t, m)
	if _, err := run(t, approvalCmd("approve"), id, "--socket", socket); err != nil {
		t.Fatal(err)
	}
	res, err := m.Client(t, "grokbot").Get(t.Context(), id, 0)
	if err != nil || res.Status != envelope.StatusQueued {
		t.Fatalf("approved: %+v %v", res, err)
	}
	req, err := m.Client(t, "grokbot").Send(t.Context(), "muse", "another", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, approvalCmd("deny"), req.ID, "not", "today", "--relay", m.URL("admin")); err != nil {
		t.Fatal(err)
	}
	res, err = m.Client(t, "grokbot").Get(t.Context(), req.ID, 0)
	if err != nil || res.Reply == nil || res.Reply.Body != "not today" {
		t.Fatalf("denied: %+v %v", res, err)
	}
	if _, err := run(t, heldCmd(), "--relay", m.URL("grokbot")); !client.IsStatus(err, 403) {
		t.Fatalf("agent list: %v", err)
	}
}

// tincan held shows a held pass as the chat that would move and where, or
// says the pass will fail when its request does not parse.
func TestHeldShowsPass(t *testing.T) {
	m := testrelay.New(t, relay.Config{ParsePass: parsePassRef})
	if ok, err := m.Store.SetAgentKind(t.Context(), "muse", "pass"); !ok || err != nil {
		t.Fatalf("set kind: %v %v", ok, err)
	}
	sender := m.Client(t, "grokbot")
	good, err := sender.Send(t.Context(), "muse", "from: https://chatgpt.com/c/68f0c1d2-aaaa\nto: claude-web", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(t.Context(), "muse", "from: chatgpt latest\nto: claude-web", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, heldCmd(), "--relay", m.URL("admin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{good.ID, "grokbot -> muse (pass)", "pass: ChatGPT conversation 68f0c1d2-aaaa -> claude-web", `pass: will fail: a pass names one conversation, never "latest"`} {
		if !strings.Contains(out, want) {
			t.Errorf("held output missing %q:\n%s", want, out)
		}
	}
}

// tincan held shows the target's kind and each attachment's name and size,
// so the owner sees what a council ask would send on to vendors.
func TestHeldListsKindAndAttachments(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	if ok, err := m.Store.SetAgentKind(t.Context(), "muse", "council"); !ok || err != nil {
		t.Fatalf("set kind: %v %v", ok, err)
	}
	sender := m.Client(t, "grokbot")
	up, err := sender.UploadAttachment(t.Context(), "design.md", "text/markdown", strings.NewReader("twelve bytes"), 12)
	if err != nil {
		t.Fatal(err)
	}
	req, err := sender.SendAttached(t.Context(), "muse", "which design?", envelope.KindAsk, "", []string{up.ID}, false)
	if err != nil || req.Status != envelope.StatusHeld {
		t.Fatalf("send: %+v %v", req, err)
	}
	out, err := run(t, heldCmd(), "--relay", m.URL("admin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{req.ID, "grokbot -> muse (council)", "which design?", "design.md (12 bytes)"} {
		if !strings.Contains(out, want) {
			t.Errorf("held output missing %q:\n%s", want, out)
		}
	}
}

func TestHeldShowsThen(t *testing.T) {
	skipOnWindows(t, skipRelayHost)
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	if _, err := run(t, askCmd(), "muse", "find it", "--then", "instinct", "file the receipt", "--wait", "0s"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, heldCmd(), "--relay", m.URL("admin"))
	if err != nil || !strings.Contains(out, "then, once answered: asks instinct: file the receipt") {
		t.Fatalf("held: %q %v", out, err)
	}
}
