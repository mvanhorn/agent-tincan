package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestAttention(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		agent client.AgentInfo
		score int
		flags string
	}{
		{"healthy", client.AgentInfo{Online: true, Version: "1.2.0"}, 0, ""},
		{"offline", client.AgentInfo{Queued: 1, Wake: "none"}, 8, "QUEUED-OFFLINE"},
		{"wakeable", client.AgentInfo{Queued: 1, Wake: "webhook"}, 0, ""},
		{"email", client.AgentInfo{Queued: 1, Wake: "email"}, 0, ""},
		{"wait loop down", client.AgentInfo{Queued: 1, Wake: "wait"}, 8, "QUEUED-OFFLINE"},
		{"listener down", client.AgentInfo{Queued: 1, Wake: "command"}, 8, "QUEUED-OFFLINE"},
		{"channel down", client.AgentInfo{Queued: 1, Wake: "channel"}, 8, "QUEUED-OFFLINE"},
		{"wait loop up", client.AgentInfo{Online: true, Queued: 1, Wake: "wait"}, 0, ""},
		{"scheduled on time", client.AgentInfo{Queued: 1, Wake: "schedule"}, 0, ""},
		{"scheduled overdue", client.AgentInfo{Queued: 1, Wake: "schedule", Target: envelope.Target{Overdue: true}}, 10, "QUEUED-OFFLINE|OVERDUE"},
		{"stale", client.AgentInfo{Online: true, Queued: 1, OldestQueued: now.Add(-time.Hour - time.Second)}, 4, "STALE"},
		{"boundary", client.AgentInfo{Online: true, Queued: 1, OldestQueued: now.Add(-time.Hour)}, 0, ""},
		{"overdue", client.AgentInfo{Wake: "schedule", Target: envelope.Target{Overdue: true}}, 2, "OVERDUE"},
		{"old", client.AgentInfo{Version: "1.1.0"}, 1, "OLD BUILD"},
		{"new", client.AgentInfo{Version: "1.3.0"}, 0, ""},
		{"claims", client.AgentInfo{Claimed: 2}, 0, "CLAIMED"},
		{"unanswered webhook", client.AgentInfo{Queued: 1, Wake: "webhook", Target: envelope.Target{Unanswered: true}}, 6, "UNANSWERED"},
		{"unanswered email, stale", client.AgentInfo{Queued: 1, Wake: "email", OldestQueued: now.Add(-2 * time.Hour), Target: envelope.Target{Unanswered: true}}, 10, "UNANSWERED|STALE"},
		{"unanswered, nothing queued", client.AgentInfo{Wake: "webhook", Target: envelope.Target{Unanswered: true}}, 6, "UNANSWERED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			score, flags := attention(tc.agent, "1.2.0", now)
			if score != tc.score || strings.Join(flags, "|") != tc.flags {
				t.Fatalf("got %d %v", score, flags)
			}
		})
	}
}

func TestRenderFrame(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := topFrame{at: at, roster: client.Roster{RelayVersion: "1.2.0", Agents: []client.AgentInfo{{Name: "alpha", Online: true, Wake: "none", Version: "1.2.0"}, {Name: "zeta", Wake: "none", Queued: 2}}}, note: "held and recent chains need an admin device"}
	want := "tincan top | relay 1.2.0 | online 1/2 | queued 2 | held ? | 12:00:00\n" +
		"AGENT            STATE   WAKE         QUEUED OLDEST CLAIMS VERSION\n" +
		"zeta             offline none              2      -      0 \n" +
		"  QUEUED-OFFLINE\n" +
		"alpha            online  none              0      -      0 1.2.0\n" +
		"held and recent chains need an admin device\n"
	if got := renderFrame(f, 120); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	if f.roster.Agents[0].Name != "alpha" {
		t.Fatal("render mutated roster")
	}
	f.admin = true
	f.note = ""
	f.held = []envelope.Request{{ID: "req1", From: "alpha", To: "zeta", Body: "approve me"}}
	f.chains = []envelope.Result{{Request: envelope.Request{CreatedAt: at, From: "alpha", To: "zeta", Body: "hello"}, Status: envelope.StatusQueued}}
	got := renderFrame(f, 120)
	suffix := "Held for approval (1):\n  req1 alpha -> zeta approve me\nRecent chains:\n  " + at.Local().Format("15:04:05") + " alpha -> zeta [queued] hello\n"
	if !strings.HasSuffix(got, suffix) || !strings.Contains(got, "held 1") {
		t.Fatal(got)
	}
	f.held[0].Body = "\x1b[2J\r\n" + strings.Repeat("界", 80)
	for _, width := range []int{1, 40, 80} {
		for line := range strings.SplitSeq(renderFrame(f, width), "\n") {
			if topTestDisplayWidth(line) > width || strings.ContainsAny(line, "\x1b\r") {
				t.Fatalf("unsafe line %q", line)
			}
		}
	}
}

func TestTopOnceMesh(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	// dot-web requests are held by default.
	if err := m.Dir.SetKind(t.Context(), "100.0.0.1:1", "muse", "dot-web"); err != nil {
		t.Fatal(err)
	}
	held, err := m.Client(t, "grokbot").Ask(t.Context(), "muse", "owner review", "", 0, false)
	if err != nil || held.Status != envelope.StatusHeld {
		t.Fatalf("seed held: %+v %v", held, err)
	}
	queued, err := m.Client(t, "grokbot").Ask(t.Context(), "instinct", "queued work", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	for _, args := range [][]string{{"--once", "--relay", m.URL("admin")}, {"--relay", m.URL("admin")}, {"--once", "--socket", adminSocket(t, m)}} {
		out, err := run(t, topCmd(), args...)
		if err != nil || !strings.Contains(out, "owner review") || !strings.Contains(out, "Recent chains:") || strings.Contains(out, "\x1b") {
			t.Fatalf("%q %v", out, err)
		}
	}
	out, err := run(t, topCmd(), "--once")
	if err != nil || !strings.Contains(out, "held and recent chains need an admin device") || strings.Contains(out, "owner review") {
		t.Fatalf("nonadmin %q %v", out, err)
	}
	for id, status := range map[string]envelope.Status{held.Request.ID: envelope.StatusHeld, queued.Request.ID: envelope.StatusQueued} {
		res, err := m.Client(t, "grokbot").Get(t.Context(), id, 0)
		if err != nil || res.Status != status {
			t.Fatalf("changed request: %+v %v", res, err)
		}
	}
	if _, err := run(t, topCmd(), "--interval", "999ms"); err == nil {
		t.Fatal("accepted short interval")
	}
}

func TestTopFetchRoutes(t *testing.T) {
	for _, code := range []int{200, 403, 404, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.RequestURI())
				if r.Method != "GET" {
					t.Errorf("mutation: %s", r.Method)
				}
				switch r.URL.Path {
				case "/v1/agents":
					fmt.Fprint(w, `{"agents":[]}`)
				case "/v1/admin/held":
					w.WriteHeader(code)
					if code == 200 {
						fmt.Fprint(w, `[]`)
					} else {
						fmt.Fprint(w, `{"error":"unavailable"}`)
					}
				case "/v1/trace":
					fmt.Fprint(w, `{"traces":[]}`)
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			r, err := client.NewRelayFor(client.Config{Relay: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			f, err := fetchFrame(t.Context(), r)
			if (err != nil) != (code == 500) {
				t.Fatalf("error %v", err)
			}
			want := "/v1/agents,/v1/admin/held"
			if code == 200 {
				want += ",/v1/trace?limit=8&exclude_pings=true"
			}
			if strings.Join(paths, ",") != want || f.admin != (code == 200) {
				t.Fatalf("paths=%v admin=%v", paths, f.admin)
			}
		})
	}
}

func topTestDisplayWidth(s string) int {
	cells := 0
	for _, r := range s {
		cells += topRuneWidth(r)
	}
	return cells
}

func TestTopTextDisplayWidth(t *testing.T) {
	for _, tc := range []struct {
		text  string
		cells int
	}{
		{"界", 2}, {"Ａ", 2}, {"😀", 2}, {"e\u0301", 1}, {"a", 1}, {"\u200d", 0},
	} {
		if got := topTestDisplayWidth(tc.text); got != tc.cells {
			t.Fatalf("width of %q = %d, want %d", tc.text, got, tc.cells)
		}
	}
	for _, input := range []string{strings.Repeat("界", 60), strings.Repeat("😀", 60), strings.Repeat("e\u0301", 60)} {
		for _, width := range []int{1, 2, 3, 4, 20} {
			got := topText(input, width)
			if topTestDisplayWidth(got) > width {
				t.Fatalf("width %d: overflowing text %q", width, got)
			}
		}
	}
	if got := topText("e\u0301界😀", 5); got != "e\u0301界😀" {
		t.Fatalf("truncated fitting text: %q", got)
	}
}

func TestTopLiveFrameHeight(t *testing.T) {
	f := topFrame{admin: true}
	for i := range 30 {
		f.roster.Agents = append(f.roster.Agents, client.AgentInfo{Name: fmt.Sprintf("agent-%02d", i), Online: true})
		f.held = append(f.held, envelope.Request{ID: fmt.Sprintf("held-%02d", i)})
		f.chains = append(f.chains, envelope.Result{Request: envelope.Request{Body: fmt.Sprintf("chain-%02d", i)}})
	}
	f.roster.Agents = append(f.roster.Agents, client.AgentInfo{Name: "urgent", Queued: 1})
	got := topLiveFrame(f, 120, 24)
	if len(strings.Split(got, "\n")) > 24 || strings.HasSuffix(got, "\n") {
		t.Fatalf("unbounded frame: %q", got)
	}
	for _, want := range []string{"tincan top", "AGENT", "urgent", "Held for approval (30):", "held-02", "Recent chains:", "chain-02", "... 27 more", "more agents (tincan top --once for all)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "urgent") > strings.Index(got, "agent-00") {
		t.Fatal("attention order lost")
	}
	plain := renderFrame(f, 120)
	for _, want := range []string{"agent-29", "held-29", "chain-29"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("snapshot missing %s", want)
		}
	}
	for _, height := range []int{1, 4, 8, 24} {
		got := topLiveFrame(f, 20, height)
		if height >= 8 && (!strings.Contains(got, "Held for approval") || !strings.Contains(got, "Recent chains:")) {
			t.Fatalf("lost admin sections: %q", got)
		}
		if len(strings.Split(got, "\n")) > height {
			t.Fatal("height overflow")
		}
		for line := range strings.SplitSeq(got, "\n") {
			if topTestDisplayWidth(line) > 20 {
				t.Fatalf("width overflow: %q", line)
			}
		}
	}
}

func TestTopLoopRefresh(t *testing.T) {
	failure := errors.New("relay unavailable")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	var frames []string
	err := topLoop(ctx, true, time.Millisecond, func(context.Context) (topFrame, error) {
		calls++
		if calls == 1 || calls == 3 {
			return topFrame{}, failure
		}
		return topFrame{roster: client.Roster{Agents: []client.AgentInfo{{Name: "recovered"}}}}, nil
	}, func(f topFrame) error {
		frames = append(frames, topLiveFrame(f, 120, 24))
		if len(frames) == 4 {
			cancel()
		}
		return nil
	})
	if err != nil || len(frames) != 4 {
		t.Fatalf("frames=%v err=%v", frames, err)
	}
	for i, frame := range frames {
		if strings.Contains(frame, "refresh failed at") != (i == 0 || i == 2) {
			t.Fatalf("wrong error state: %q", frame)
		}
		if i > 0 && !strings.Contains(frame, "recovered") {
			t.Fatalf("lost roster: %q", frame)
		}
	}
	if !strings.Contains(frames[0], "relay unavailable (retrying)") {
		t.Fatal(frames[0])
	}
	err = topLoop(t.Context(), false, time.Millisecond, func(context.Context) (topFrame, error) {
		return topFrame{}, failure
	}, func(topFrame) error { t.Fatal("rendered failed snapshot"); return nil })
	if !errors.Is(err, failure) {
		t.Fatalf("once error: %v", err)
	}
}

func TestTopTextJoinedEmoji(t *testing.T) {
	family := "👨‍👩‍👧‍👦"
	if got := topText(family, 20); got != family {
		t.Fatalf("broken family: %q", got)
	}
	for width := 1; width < 8; width++ {
		got := topText(family+"abcdef", width)
		if strings.ContainsAny(got, "👨👩👧👦\u200d") {
			t.Fatalf("partial family at width %d: %q", width, got)
		}
	}
	if got := topText("\x1b[2J\u202e", 20); got != " [2J " {
		t.Fatalf("unsafe text: %q", got)
	}
	if got := topText("a\u200c\ufe0e\ufe0f", 1); got != "a\u200c\ufe0e\ufe0f" {
		t.Fatalf("stripped harmless runes: %q", got)
	}
}

func TestTopWideColumns(t *testing.T) {
	f := topFrame{roster: client.Roster{Agents: []client.AgentInfo{
		{Name: "ascii", Online: true, Wake: "none"},
		{Name: "界界界界界界界界界", Online: true, Wake: "界界"},
	}}}
	lines := strings.Split(renderFrame(f, 120), "\n")
	stateColumn := strings.Index(lines[1], "STATE")
	for _, row := range lines[2:4] {
		prefix, _, found := strings.Cut(row, "online")
		if !found || topTestDisplayWidth(prefix) != stateColumn {
			t.Fatalf("misaligned state: %q", row)
		}
		if topTestDisplayWidth(row[:strings.LastIndex(row, "0")]) != topTestDisplayWidth(lines[2][:strings.LastIndex(lines[2], "0")]) {
			t.Fatalf("misaligned wake: %q", row)
		}
	}
}

// A day-old queue keeps its row in the column layout, and chain times are
// shown in local time like the header.
func TestRenderFrameAgeAndLocalTime(t *testing.T) {
	at := time.Date(2026, 10, 2, 20, 26, 0, 0, time.Local)
	sent := at.Add(-time.Hour).UTC()
	f := topFrame{at: at, admin: true, roster: client.Roster{RelayVersion: "1.0.0", Agents: []client.AgentInfo{
		{Name: "muse", Wake: "wait", Version: "1.0.0", Queued: 25, OldestQueued: at.Add(-(23*time.Hour + 31*time.Minute + 12*time.Second))},
	}}, chains: []envelope.Result{{Request: envelope.Request{From: "a", To: "b", Body: "hi", CreatedAt: sent}, Status: envelope.StatusAnswered}}}
	out := renderFrame(f, 120)
	if !strings.Contains(out, "     25    23h      0 1.0.0") {
		t.Fatalf("queue row not aligned:\n%s", out)
	}
	if want := "  " + sent.Local().Format("15:04:05") + " a -> b"; !strings.Contains(out, want) {
		t.Fatalf("chain time not local, want %q in:\n%s", want, out)
	}
}

func TestQueueAge(t *testing.T) {
	for d, want := range map[time.Duration]string{-time.Minute: "0m", 14 * time.Minute: "14m", 23*time.Hour + 31*time.Minute: "23h", 50 * time.Hour: "2d"} {
		if got := client.QueueAge(d); got != want {
			t.Errorf("QueueAge(%v) = %q, want %q", d, got, want)
		}
	}
}

// AE1 in top: an unanswered webhook agent sorts above an ordinary offline
// webhook agent and below a queued agent that nothing will wake.
func TestTopUnansweredOrder(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := topFrame{at: at, roster: client.Roster{Agents: []client.AgentInfo{
		{Name: "alpha", Wake: "webhook", Queued: 1},
		{Name: "grokbot", Wake: "webhook", Queued: 1, Target: envelope.Target{WokenAt: at.Add(-12 * time.Minute), WakeResult: "ok", Unanswered: true}},
		{Name: "muse", Wake: "wait", Queued: 1},
	}}}
	var order []string
	for line := range strings.SplitSeq(renderFrame(f, 120), "\n") {
		if name, _, ok := strings.Cut(line, " "); ok && (name == "alpha" || name == "grokbot" || name == "muse") {
			order = append(order, name)
		}
	}
	if got := strings.Join(order, ","); got != "muse,grokbot,alpha" {
		t.Fatalf("order = %s\n%s", got, renderFrame(f, 120))
	}
	if !strings.Contains(renderFrame(f, 120), "  UNANSWERED\n") {
		t.Fatal(renderFrame(f, 120))
	}
}

func TestSignedOutOnlineAttention(t *testing.T) {
	score, flags := attention(client.AgentInfo{Online: true, Target: envelope.Target{SignedOutSite: "chatgpt.com"}}, "", time.Now())
	if score != 6 || len(flags) != 1 || flags[0] != "SIGNED-OUT" {
		t.Fatalf("attention = %d %v", score, flags)
	}
}
