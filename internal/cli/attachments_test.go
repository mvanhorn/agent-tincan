package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

var cliPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{9}, 32)...)

func tempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ask --attach, reply --attach, and attachment get, end to end through the
// relay.
func TestAttachCLIRoundTrip(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	ctx := context.Background()

	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	if out, err := run(t, askCmd(), "muse", "what is in this picture", "--attach", tempFile(t, "pic.png", cliPNG), "--wait", "0s"); err != nil || !strings.Contains(out, "No reply yet") {
		t.Fatalf("ask = %q, %v", out, err)
	}

	muse := m.Client(t, "muse")
	in, err := muse.Poll(ctx, 0)
	if err != nil || len(in.Requests) != 1 || len(in.Requests[0].Attachments) != 1 {
		t.Fatalf("muse poll = %+v, %v", in, err)
	}
	req := in.Requests[0]
	if a := req.Attachments[0]; a.Name != "pic.png" || a.MIME != "image/png" {
		t.Fatalf("request attachment = %+v", a)
	}
	if _, err := muse.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}

	// muse fetches the picture to a path of its choosing.
	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	dst := filepath.Join(t.TempDir(), "got.png")
	if out, err := run(t, attachmentCmd(), "get", req.Attachments[0].ID, "-o", dst); err != nil || !strings.Contains(out, dst) {
		t.Fatalf("attachment get -o = %q, %v", out, err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, cliPNG) {
		t.Fatalf("fetched %d bytes, want the png", len(got))
	}
	if out, err := run(t, replyCmd(), req.ID, "a cat", "--attach", tempFile(t, "notes.txt", []byte("tabby, 4kg"))); err != nil || !strings.Contains(out, "Replied") {
		t.Fatalf("reply = %q, %v", out, err)
	}

	// grokbot sees the attachment listed and saves it to its own directory.
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	out, err := run(t, getCmd(), req.ID)
	if err != nil || !strings.Contains(out, "a cat") || !strings.Contains(out, "notes.txt") || !strings.Contains(out, "tincan attachment get") {
		t.Fatalf("get = %q, %v", out, err)
	}
	res, err := m.Client(t, "grokbot").Get(ctx, req.ID, 0)
	if err != nil || res.Reply == nil || len(res.Reply.Attachments) != 1 {
		t.Fatalf("reply = %+v, %v", res, err)
	}
	id := res.Reply.Attachments[0].ID
	out, err = run(t, attachmentCmd(), "get", id)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := client.LoadConfig()
	want := filepath.Join(client.AttachmentDir(cfg), id+".txt")
	if !strings.Contains(out, want) {
		t.Fatalf("attachment get = %q, want path %s", out, want)
	}
	st, err := os.Stat(want)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("saved file = %v, %v", st, err)
	}
	if got, _ := os.ReadFile(want); string(got) != "tabby, 4kg" {
		t.Fatalf("saved %q", got)
	}
}

// --attach against a relay without attachments fails and sends nothing.
func TestAttachCLIRelayWithoutSupport(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	_, err := run(t, askCmd(), "muse", "look", "--attach", tempFile(t, "a.png", cliPNG), "--notify")
	if err == nil || !strings.Contains(err.Error(), "does not support attachments") {
		t.Fatalf("err = %v", err)
	}
	if n, _ := m.Store.CountQueued(context.Background(), "muse"); n != 0 {
		t.Fatalf("queued %d", n)
	}
}

// A missing --attach file fails before sending.
func TestAttachCLIMissingFile(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	req, _ := m.Client(t, "muse").Send(context.Background(), "grokbot", "send it", envelope.KindAsk, "")
	_, err := run(t, replyCmd(), req.ID, "here", "--attach", filepath.Join(t.TempDir(), "gone.txt"))
	if err == nil || !strings.Contains(err.Error(), "gone.txt") {
		t.Fatalf("err = %v", err)
	}
	if res, _ := m.Client(t, "muse").Get(context.Background(), req.ID, 0); res.Reply != nil {
		t.Fatalf("reply sent: %+v", res.Reply)
	}
}

// The agent's attachment directory sits beside its config and is named for
// the agent; a name that is not a plain agent name never shapes the path.
func TestAttachmentDirIsPerAgent(t *testing.T) {
	useConfig(t, client.Config{})
	base := filepath.Dir(client.ConfigPath())
	if got := client.AttachmentDir(client.Config{Agent: "codex"}); got != filepath.Join(base, "attachments", "codex") {
		t.Fatalf("dir = %s", got)
	}
	for _, bad := range []string{"../x", "/etc", "", "a/b"} {
		got := client.AttachmentDir(client.Config{Agent: bad})
		if filepath.Dir(got) != filepath.Join(base, "attachments") {
			t.Fatalf("agent %q gave dir %s", bad, got)
		}
	}
}
