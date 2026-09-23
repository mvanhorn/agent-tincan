package relay

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// fakeClock is a settable clock safe to read from handler goroutines.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func agentInfo(t *testing.T, h *harness, addr, name string) client.AgentInfo {
	t.Helper()
	var out struct{ Agents []client.AgentInfo }
	h.do(addr, "GET", "/v1/agents", "", http.StatusOK, &out)
	for _, a := range out.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("%s not in %+v", name, out.Agents)
	return client.AgentInfo{}
}

// A webhook agent never long-polls, but its sends still count as activity:
// the roster shows it as recently seen, while Online and LastPoll stay about
// a live poller.
func TestSendFromNonPollerSetsLastActive(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_790_000_000, 0)}
	h := newHarness(t, Config{Now: clk.Now})
	h.send(grokAddr, "muse", "hi")
	a := agentInfo(t, h, macAddr, "grokbot")
	if !a.LastActive.Equal(clk.Now()) {
		t.Fatalf("LastActive = %v, want %v", a.LastActive, clk.Now())
	}
	if a.Online || !a.LastPoll.IsZero() {
		t.Fatalf("a sender that never polled should stay offline with no LastPoll: %+v", a)
	}
	clk.advance(3 * time.Minute)
	if got := a.LastSeen(clk.Now()); got != "last seen 3m ago" {
		t.Fatalf("LastSeen = %q", got)
	}
	if never := agentInfo(t, h, macAddr, "instinct"); !never.LastActive.IsZero() {
		t.Fatalf("instinct never called but LastActive = %v", never.LastActive)
	}
}

// A long-poll counts as activity too.
func TestPollSetsLastActive(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_790_000_000, 0)}
	h := newHarness(t, Config{Now: clk.Now})
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	if a := agentInfo(t, h, macAddr, "muse"); !a.LastActive.Equal(clk.Now()) || !a.Online {
		t.Fatalf("muse after poll = %+v", a)
	}
}

// Activity is persisted, so a relay restarted over the same database still
// knows when each agent was last seen.
func TestLastActiveSurvivesRestart(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_790_000_000, 0)}
	h := newHarness(t, Config{Now: clk.Now})
	sentAt := clk.Now()
	h.send(grokAddr, "muse", "hi")

	clk.advance(time.Hour)
	srv := New(identity.NewDirectory(h.st, h.who, identity.Config{Admins: []string{"macbook-pro-44"}}), h.st, Config{Now: clk.Now})
	h2 := &harness{t: t, srv: srv, h: srv.Handler(), st: h.st, who: h.who}
	a := agentInfo(t, h2, macAddr, "grokbot")
	if !a.LastActive.Equal(sentAt) {
		t.Fatalf("LastActive after restart = %v, want %v", a.LastActive, sentAt)
	}
	if got := a.LastSeen(clk.Now()); got != "last seen 1h ago" {
		t.Fatalf("LastSeen after restart = %q", got)
	}
}

// Activity is written to the store at most once a minute per agent; the
// in-memory time still moves on every call.
func TestLastActivePersistIsThrottled(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_790_000_000, 0)}
	h := newHarness(t, Config{Now: clk.Now})
	ctx := context.Background()
	first := clk.Now()
	h.send(grokAddr, "muse", "one")
	clk.advance(30 * time.Second)
	h.send(grokAddr, "muse", "two")

	seen, err := h.st.AgentsLastSeen(ctx)
	if err != nil || !seen["grokbot"].Equal(first) {
		t.Fatalf("persisted after two calls in a minute = %v, %v; want only the first write (%v)", seen["grokbot"], err, first)
	}
	if a := agentInfo(t, h, macAddr, "grokbot"); !a.LastActive.Equal(clk.Now()) {
		t.Fatalf("in-memory LastActive = %v, want %v", a.LastActive, clk.Now())
	}

	clk.advance(31 * time.Second)
	h.send(grokAddr, "muse", "three")
	if seen, _ := h.st.AgentsLastSeen(ctx); !seen["grokbot"].Equal(clk.Now()) {
		t.Fatalf("persisted after a minute = %v, want %v", seen["grokbot"], clk.Now())
	}
}
