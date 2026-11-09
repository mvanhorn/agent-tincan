package relay_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// A sender's rate limit does not split by project label.
func TestProjectLabelSharesRateLimit(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{PerMinute: 1}))
	sender := m.Client(t, "grokbot")
	sender.SetProject("one")
	if _, err := sender.Send(t.Context(), "muse", "first", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	sender.SetProject("two")
	if _, err := sender.Send(t.Context(), "muse", "second", envelope.KindAsk, "", false); !client.IsStatus(err, http.StatusTooManyRequests) {
		t.Fatalf("second send under another label: %v, want 429", err)
	}
}

// A room mention names no project, whatever the poster's window works in:
// it reaches any session of the member at once.
func TestRoomMentionIsNotScoped(t *testing.T) {
	m := mentionMesh(t)
	poster := m.Client(t, "codex")
	poster.SetProject("alpha")
	if _, err := client.PostToRoom(t.Context(), poster, "launch", "@claude-code review this"); err != nil {
		t.Fatal(err)
	}
	member := m.Client(t, "claude-code")
	member.SetProject("beta")
	in, err := member.Poll(t.Context(), time.Millisecond)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].Project != "" {
		t.Fatalf("member's poll = %+v %v", in, err)
	}
}

// A grouped ask names the project for every target, each of which routes it
// among its own sessions.
func TestGroupedAskScopesEveryTarget(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := client.WithProject(t.Context(), "alpha")
	g, err := m.Client(t, "grokbot").SendGroup(ctx, []string{"muse", "instinct"}, "review", envelope.KindAsk, "", nil, false)
	if err != nil || len(g.Results) != 2 {
		t.Fatalf("group = %+v %v", g, err)
	}
	for _, name := range []string{"muse", "instinct"} {
		wrong, right := m.Client(t, name), m.Client(t, name)
		wrong.SetProject("beta")
		right.SetProject("alpha")
		if in, err := wrong.Poll(t.Context(), time.Millisecond); err != nil || len(in.Requests) != 0 {
			t.Fatalf("%s wrong window = %+v %v", name, in, err)
		}
		if in, err := right.Poll(t.Context(), time.Millisecond); err != nil || len(in.Requests) != 1 {
			t.Fatalf("%s matching window = %+v %v", name, in, err)
		}
	}
}
