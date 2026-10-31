package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	if err != nil || !hasMode(st, 0o600) {
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
	req, _ := m.Client(t, "muse").Send(context.Background(), "grokbot", "send it", envelope.KindAsk, "", false)
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

func TestUrgentCLIAndChannel(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	if _, err := run(t, askCmd(), "muse", "routine", "--wait", "0s"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, askCmd(), "muse", "urgent", "--urgent", "--attach", tempFile(t, "pic.png", cliPNG), "--wait", "0s"); err != nil {
		t.Fatal(err)
	}
	pending, err := m.Store.PendingRequests(t.Context(), "muse", 10)
	if err != nil {
		t.Fatal(err)
	}
	a := announcer{}
	n, ok := a.next(client.Waiting{Total: 2, Queued: 2, Pending: pending}, time.Now())
	if !ok || !strings.Contains(n.content, "1 urgent request waiting") {
		t.Fatalf("notice = %+v", n)
	}
	in, err := m.Client(t, "muse").Poll(t.Context(), 0)
	if err != nil || len(in.Requests) != 2 || !in.Requests[0].Urgent || in.Requests[1].Urgent || len(in.Requests[0].Attachments) != 1 {
		t.Fatalf("inbox = %+v, %v", in, err)
	}
}

func TestUrgentNoticeCountsPastThePendingList(t *testing.T) {
	a := announcer{}
	pending := make([]envelope.Pending, 50)
	for i := range pending {
		pending[i] = envelope.Pending{ID: fmt.Sprintf("r%d", i), From: "grokbot", Urgent: true}
	}
	n, ok := a.next(client.Waiting{Total: 60, Queued: 60, Urgent: 60, Pending: pending}, time.Now())
	if !ok || !strings.Contains(n.content, "60 urgent requests waiting") {
		t.Fatalf("notice = %+v", n)
	}
}

func TestNewUrgentPastThePendingListIsAnnouncedAtOnce(t *testing.T) {
	a := announcer{}
	pending := make([]envelope.Pending, 50)
	for i := range pending {
		pending[i] = envelope.Pending{ID: fmt.Sprintf("r%d", i), From: "grokbot", Urgent: true}
	}
	now := time.Now()
	n, ok := a.next(client.Waiting{Total: 50, Queued: 50, Urgent: 50, Pending: pending}, now)
	if !ok {
		t.Fatal("first peek not announced")
	}
	a.pushed(n, now)
	// A 51st urgent request lands past the capped pending list: same ids, one
	// more urgent. It must be announced at once, not after reannounceAfter.
	w := client.Waiting{Total: 51, Queued: 51, Urgent: 51, Pending: pending}
	n, ok = a.next(w, now.Add(time.Second))
	if !ok || !strings.Contains(n.content, "51 urgent requests waiting") {
		t.Fatalf("new urgent request not announced: %v %q", ok, n.content)
	}
	a.pushed(n, now.Add(time.Second))
	if _, ok := a.next(w, now.Add(2*time.Second)); ok {
		t.Fatal("same urgent count announced twice")
	}
	// Some taken, then one more: a rise from the lower count is new again.
	w = client.Waiting{Total: 50, Queued: 50, Urgent: 50, Pending: pending}
	if _, ok := a.next(w, now.Add(3*time.Second)); ok {
		t.Fatal("a drop in urgent announced")
	}
	w = client.Waiting{Total: 51, Queued: 51, Urgent: 51, Pending: pending}
	if _, ok := a.next(w, now.Add(4*time.Second)); !ok {
		t.Fatal("urgent rise after a drop not announced")
	}
}

func TestRelayRejectsNonPositiveUrgentLimit(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		cmd := relayCmd()
		cmd.SetArgs([]string{"--urgent-per-hour", v, "--state-dir", t.TempDir()})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "--urgent-per-hour") {
			t.Fatalf("urgent-per-hour %s: %v", v, err)
		}
	}
}
