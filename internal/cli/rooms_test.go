package cli

import (
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// The owner creates a room and adds members from an admin device; members
// post, read after a cursor and list their rooms; an agent cannot change
// membership.
func TestRoomCommands(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	admin := m.URL("admin")
	if out, err := run(t, roomCmd(), "create", "launch", "--relay", admin); err != nil || !strings.Contains(out, `Created room "launch"`) {
		t.Fatalf("create = %q, %v", out, err)
	}
	for _, a := range []string{"grokbot", "muse"} {
		if out, err := run(t, roomCmd(), "add", "launch", a, "--relay", admin); err != nil || !strings.Contains(out, "Added "+a) {
			t.Fatalf("add %s = %q, %v", a, out, err)
		}
	}
	if _, err := run(t, roomCmd(), "add", "launch", "instinct"); err == nil {
		t.Fatal("an agent added a member")
	}
	if out, err := run(t, roomCmd(), "post", "launch", "plan", "is", "up"); err != nil || !strings.Contains(out, "Posted #1 to launch.") {
		t.Fatalf("post = %q, %v", out, err)
	}
	if out, err := run(t, roomCmd(), "post", "launch", "@muse take a look"); err != nil || !strings.Contains(out, "Asked muse") {
		t.Fatalf("mention = %q, %v", out, err)
	}
	out, err := run(t, roomCmd(), "read", "launch", "--after", "1")
	if err != nil || strings.Contains(out, "plan is up") || !strings.Contains(out, "#2 grokbot") || !strings.Contains(out, "waiting on muse") {
		t.Fatalf("read = %q, %v", out, err)
	}
	if out, err := run(t, roomCmd(), "list"); err != nil || !strings.Contains(out, "launch (members: grokbot, muse)") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if out, err := run(t, roomCmd(), "remove", "launch", "muse", "--relay", admin); err != nil || !strings.Contains(out, "Removed muse") {
		t.Fatalf("remove = %q, %v", out, err)
	}
	if out, err := run(t, roomCmd(), "list", "--all", "--relay", admin); err != nil || !strings.Contains(out, "launch (members: grokbot)") {
		t.Fatalf("list --all = %q, %v", out, err)
	}
}

// Adding a web chat agent prints the vendor warning; adding an agent rooms
// exclude fails with the relay's reason.
func TestRoomAddWarnsAndRefuses(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	admin := m.URL("admin")
	m.JoinOnMachineOf(t, "muse", "chatgpt-web")
	m.JoinOnMachineOf(t, "muse", "council")
	for name, kind := range map[string]string{"chatgpt-web": "chatgpt-web", "council": "council"} {
		if _, err := m.Store.SetAgentKind(t.Context(), name, kind); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(t, roomCmd(), "create", "launch", "--relay", admin); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, roomCmd(), "add", "launch", "chatgpt-web", "--relay", admin); err != nil || !strings.Contains(out, "Warning:") || !strings.Contains(out, "vendor") {
		t.Fatalf("add chatgpt-web = %q, %v", out, err)
	}
	if _, err := run(t, roomCmd(), "add", "launch", "council", "--relay", admin); err == nil || !strings.Contains(err.Error(), "held for the owner's approval") {
		t.Fatalf("add council: %v", err)
	}
}
