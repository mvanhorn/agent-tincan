package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// AE1 in tincan agents: a schedule agent shows its interval, and an overdue
// one also shows how long ago it last checked its inbox.
func TestFormatAgentsSchedule(t *testing.T) {
	now := time.Now()
	got := formatAgents([]client.AgentInfo{
		{Name: "fo", Wake: "schedule", Target: envelope.Target{CheckEverySeconds: 300, ExpectReplySeconds: 600}, LastPoll: now.Add(-3 * time.Minute)},
		{Name: "late", Wake: "schedule", Target: envelope.Target{CheckEverySeconds: 300, ExpectReplySeconds: 600, Overdue: true}, LastPoll: now.Add(-25 * time.Minute)},
		{Name: "grokbot", Wake: "webhook", LastPoll: now},
	}, now)
	want := "fo             offline  wake=schedule (every 5m) last seen 3m ago\n" +
		"late           offline  wake=schedule (every 5m) last seen 25m ago overdue: last check 25m ago\n" +
		"grokbot        offline  wake=webhook last seen just now\n"
	if got != want {
		t.Fatalf("agents =\n%s\nwant\n%s", got, want)
	}
}

// AE2 through the CLI: ask with its default wait against a schedule target
// says how often it checks and when to expect a reply; against a webhook
// target the text is unchanged.
func TestAskScheduleTargetDefaultWait(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: 200 * time.Millisecond})
	m.Server.SetWakeNamer(wake.New(wake.Config{
		"muse":    {Method: wake.Schedule, Every: "5m"},
		"grokbot": {Method: wake.Webhook, URL: "http://127.0.0.1:1/hook"},
	}, nil, wake.Options{}))
	useConfig(t, client.Config{Relay: m.URL("instinct"), Agent: "instinct"})

	out, _, err := runSplit(t, askCmd(), "muse", "reschedule the dentist")
	if err != nil || !strings.HasPrefix(out, "muse checks its inbox every 5m; expect a reply within about 10m.\nNo reply yet from muse.") {
		t.Fatalf("ask muse = %q, %v", out, err)
	}
	out, _, err = runSplit(t, askCmd(), "grokbot", "status")
	if err != nil || !strings.HasPrefix(out, "No reply yet from grokbot.") {
		t.Fatalf("ask grokbot = %q, %v", out, err)
	}
}

// An unanswered webhook agent shows when it was last woken and the result on
// its own line, just before its good-at line, which stays last.
func TestFormatAgentsUnanswered(t *testing.T) {
	now := time.Now()
	got := formatAgents([]client.AgentInfo{
		{Name: "grokbot", Wake: "webhook", LastPoll: now.Add(-3 * time.Hour), Kind: "openclaw", GoodAt: "phone calls",
			Target: envelope.Target{WokenAt: now.Add(-12 * time.Minute), WakeResult: "ok", Unanswered: true}},
		{Name: "hermes", Wake: "webhook", LastPoll: now, Target: envelope.Target{WokenAt: now.Add(-time.Minute), WakeResult: "ok"}},
	}, now)
	want := `grokbot        offline  wake=webhook last seen 3h ago kind=openclaw unanswered="woken 12m ago, no check-in (webhook ok)" good_at="phone calls"` + "\n" +
		"hermes         offline  wake=webhook last seen just now\n"
	if got != want {
		t.Fatalf("agents =\n%s\nwant\n%s", got, want)
	}
}

// AE4 through the CLI: an ask to an agent whose last wake failed says so and
// that the request is queued.
func TestAskUnansweredTarget(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: 200 * time.Millisecond})
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	woke := time.Now().Add(-12 * time.Minute)
	m.Backdate(t, "grokbot", time.Hour)
	if err := st.SetLastWake(t.Context(), "grokbot", store.Wake{At: woke, Result: "hooks.example returned 502 Bad Gateway"}); err != nil {
		t.Fatal(err)
	}
	w := wake.New(wake.Config{"grokbot": {Method: wake.Webhook, URL: "http://127.0.0.1:1/hook"}}, st, wake.Options{Debounce: time.Hour})
	t.Cleanup(w.Stop)
	m.Server.SetWakeNamer(w)
	useConfig(t, client.Config{Relay: m.URL("instinct"), Agent: "instinct"})
	out, _, err := runSplit(t, askCmd(), "grokbot", "status")
	// The wake shows as a clock time today, with its date once midnight has
	// passed since (local clock), as the ask output prints it.
	at := woke.Local().Format("15:04")
	if now := time.Now().Local(); woke.Local().YearDay() != now.YearDay() || woke.Local().Year() != now.Year() {
		at = woke.Local().Format("Jan 2 15:04")
	}
	want := "grokbot's wake at " + at + " failed (hooks.example returned 502 Bad Gateway) and it has not checked in yet; the request is queued.\nNo reply yet from grokbot."
	if err != nil || !strings.HasPrefix(out, want) {
		t.Fatalf("ask grokbot = %q, %v", out, err)
	}
}
