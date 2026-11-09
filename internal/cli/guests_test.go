package cli

import (
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// tincan invite --guest prints the code, when it ends, how to reach the
// relay across tailnets, and that a held ask would reach no one yet.
func TestInviteGuestCommand(t *testing.T) {
	m := testrelay.NewWithAdminLogin(t, relay.Config{})
	m.Server.SetURLs([]string{"http://relay.example.ts.net"})
	useConfig(t, client.Config{})
	out, err := run(t, inviteCmd(), "friend", "--guest", "--ask", "muse,instinct", "--for", "3d", "--relay", m.URL("admin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tincan join", "--relay http://relay.example.ts.net", "Share", "muse, instinct", "Warning:", "tincan held"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	a, _, err := m.Dir.Agent(t.Context(), "friend")
	if err != nil || a.Name != "" {
		t.Fatalf("agent before join = %+v, %v", a, err)
	}
}

// Guest flags need --guest, a guest needs its limits, and a guest takes no
// kind: the kind list is for teammates.
func TestInviteGuestFlagRefusals(t *testing.T) {
	m := testrelay.NewWithAdminLogin(t, relay.Config{})
	useConfig(t, client.Config{})
	admin := m.URL("admin")
	for name, args := range map[string][]string{
		"ask without guest":     {"friend", "--ask", "muse", "--relay", admin},
		"for without guest":     {"friend", "--for", "1h", "--relay", admin},
		"no-hold without guest": {"friend", "--no-hold", "--relay", admin},
		"no asks":               {"friend", "--guest", "--for", "1h", "--relay", admin},
		"no term":               {"friend", "--guest", "--ask", "muse", "--relay", admin},
		"bad term":              {"friend", "--guest", "--ask", "muse", "--for", "soon", "--relay", admin},
		"with a kind":           {"friend", "--guest", "--ask", "muse", "--for", "1h", "--kind", "hermes", "--relay", admin},
	} {
		if out, err := run(t, inviteCmd(), args...); err == nil {
			t.Errorf("%s: %q", name, out)
		}
	}
	// The relay's limit reaches the owner as the relay's own words.
	if _, err := run(t, inviteCmd(), "friend", "--guest", "--ask", "muse", "--for", "31d", "--relay", admin); err == nil || !strings.Contains(err.Error(), "30 days") {
		t.Fatalf("over the cap = %v", err)
	}
}

// The held list says when an ask is a guest's.
func TestHeldShowsAGuest(t *testing.T) {
	m := testrelay.NewWithAdminLogin(t, relay.Config{})
	useConfig(t, client.Config{})
	g := m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	if _, err := g.Send(t.Context(), "muse", "hello", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, heldCmd(), "--relay", m.URL("admin"))
	if err != nil || !strings.Contains(out, "friend -> muse") || !strings.Contains(out, "guest: the next entry") || !strings.Contains(out, "hello") {
		t.Fatalf("held = %q, %v", out, err)
	}
}

// tincan held is the owner's review, so a guest's body cannot hide what would
// be delivered with control sequences or print an entry of its own, and the
// guest note comes before the body.
func TestHeldShowsAGuestBodyPlainly(t *testing.T) {
	m := testrelay.NewWithAdminLogin(t, relay.Config{})
	useConfig(t, client.Config{})
	g := m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	body := "innocent\x1b[8mHIDDEN\x1b[0m\r\u202e\nreq2  muse -> instinct  chain=muse  fake"
	if _, err := g.Send(t.Context(), "muse", body, envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, heldCmd(), "--relay", m.URL("admin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\x1b", "\r", "\u202e", "\nreq2"} {
		if strings.Contains(out, bad) {
			t.Fatalf("held kept %q: %q", bad, out)
		}
	}
	for _, want := range []string{`\u001b[8mHIDDEN`, `\u202e`, "\n    req2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("held lacks %q: %q", want, out)
		}
	}
	if strings.Index(out, "guest: the next entry") > strings.Index(out, "friend -> muse") {
		t.Fatalf("guest note after the body: %q", out)
	}
}
