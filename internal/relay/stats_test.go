package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

type statsClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *statsClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *statsClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newStatsHarness(t *testing.T) (*harness, *statsClock) {
	t.Helper()
	c := &statsClock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	h := newHarness(t, Config{Now: c.now})
	h.st.SetClock(c.now)
	return h, c
}

// finishAsk sends muse an ask and closes it with status after d.
func (h *harness) finishAsk(c *statsClock, status envelope.Status, d time.Duration) {
	h.t.Helper()
	req := h.send(grokAddr, "muse", "dummy secret question")
	c.advance(d)
	if _, err := h.st.Reply(h.t.Context(), req.ID, "muse", envelope.Reply{Status: status, Body: "dummy secret reply"}); err != nil {
		h.t.Fatal(err)
	}
}

// checkMedian fails unless st's median is wantMS, and returns st without it so
// the rest compares with !=.
func checkMedian(t *testing.T, st client.AgentStats, wantMS int64) client.AgentStats {
	t.Helper()
	if st.MedianReplyMS == nil || *st.MedianReplyMS != wantMS {
		t.Fatalf("median of %+v is not %d", st, wantMS)
	}
	st.MedianReplyMS = nil
	return st
}

func museStats(t *testing.T, agents []client.AgentInfo) client.AgentStats {
	t.Helper()
	for _, a := range agents {
		if a.Name == "muse" {
			return a.Stats
		}
	}
	t.Fatalf("no muse in %+v", agents)
	return client.AgentStats{}
}

// /v1/stats lists every joined agent for any joined caller, counts and
// durations only, and refuses a caller that is neither joined nor admin.
func TestStatsRoute(t *testing.T) {
	h, c := newStatsHarness(t)
	h.finishAsk(c, envelope.StatusAnswered, time.Minute)
	h.finishAsk(c, envelope.StatusAnswered, 3*time.Minute)
	h.finishAsk(c, envelope.StatusFailed, time.Minute)

	var got client.StatsReport
	rec := h.do(instinctAddr, "GET", "/v1/stats", "", http.StatusOK, &got)
	if len(got.Agents) != 3 {
		t.Fatalf("agents = %+v, want every joined agent", got.Agents)
	}
	if !got.Since.Equal(c.now().Add(-7 * 24 * time.Hour)) {
		t.Fatalf("since = %v, want seven days back", got.Since)
	}
	for _, a := range got.Agents {
		switch a.Agent {
		case "muse":
			if want := (client.AgentStats{Agent: "muse", Answered: 2, Failed: 1}); checkMedian(t, a, 120_000) != want {
				t.Fatalf("muse = %+v, want %+v", a, want)
			}
		default:
			if want := (client.AgentStats{Agent: a.Agent}); a != want {
				t.Fatalf("%s = %+v, want zeros", a.Agent, a)
			}
		}
	}
	if body := rec.Body.String(); strings.Contains(body, "secret") {
		t.Fatalf("stats carry text: %s", body)
	}
	h.do(macAddr, "GET", "/v1/stats", "", http.StatusOK, nil)
	h.do(strangerAddr, "GET", "/v1/stats", "", http.StatusForbidden, nil)

	// The relay's local admin socket serves it too.
	req := httptest.NewRequest("GET", "/v1/stats", nil)
	sock := httptest.NewRecorder()
	h.srv.AdminHandler().ServeHTTP(sock, req)
	if sock.Code != http.StatusOK {
		t.Fatalf("admin socket: %d %s", sock.Code, sock.Body.String())
	}
}

func TestStatsRouteWindow(t *testing.T) {
	h, c := newStatsHarness(t)
	h.finishAsk(c, envelope.StatusAnswered, time.Minute)
	c.advance(3 * 24 * time.Hour)

	for since, want := range map[string]int{"7d": 1, "2d": 0, "12h": 0, "96h": 1} {
		var got client.StatsReport
		h.do(grokAddr, "GET", "/v1/stats?since="+since, "", http.StatusOK, &got)
		var answered int
		for _, a := range got.Agents {
			answered += a.Answered
		}
		if answered != want {
			t.Errorf("since=%s: answered = %d, want %d", since, answered, want)
		}
	}
	for _, bad := range []string{"soon", "-1h", "0", "0d", "1.5d", "99999999d", "400d"} {
		h.do(grokAddr, "GET", "/v1/stats?since="+bad, "", http.StatusBadRequest, nil)
	}
}

// Stats for a removed agent's name are not shown.
func TestStatsLeavesOutRemovedAgents(t *testing.T) {
	h, c := newStatsHarness(t)
	h.finishAsk(c, envelope.StatusAnswered, time.Minute)
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"muse"}`, http.StatusOK, nil)
	var got client.StatsReport
	h.do(grokAddr, "GET", "/v1/stats", "", http.StatusOK, &got)
	for _, a := range got.Agents {
		if a.Agent == "muse" {
			t.Fatalf("removed agent listed: %+v", got.Agents)
		}
	}
}

// The roster carries the seven-day record, and an agent with none has no
// field on the wire, so older clients and older relays look the same.
func TestRosterCarriesStats(t *testing.T) {
	h, c := newStatsHarness(t)
	h.finishAsk(c, envelope.StatusAnswered, 2*time.Minute)
	var ro client.Roster
	rec := h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, &ro)
	if want := (client.AgentStats{Answered: 1}); checkMedian(t, museStats(t, ro.Agents), 120_000) != want {
		t.Fatalf("muse = %+v, want %+v", museStats(t, ro.Agents), want)
	}
	if got := strings.Count(rec.Body.String(), `"stats"`); got != 1 {
		t.Fatalf("%d stats fields, want one: %s", got, rec.Body.String())
	}
}

// A reply inside the millisecond is a known median of 0: it stays on the wire
// and reads as <1s. A record with no known time leaves the field out.
func TestRosterKeepsKnownZeroMedian(t *testing.T) {
	h, c := newStatsHarness(t)
	h.finishAsk(c, envelope.StatusFailed, time.Minute)
	if rec := h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, nil); strings.Contains(rec.Body.String(), "median_reply_ms") {
		t.Fatalf("unknown median on the wire: %s", rec.Body.String())
	}

	h, c = newStatsHarness(t)
	h.finishAsk(c, envelope.StatusAnswered, 0)
	var ro client.Roster
	rec := h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, &ro)
	if !strings.Contains(rec.Body.String(), `"median_reply_ms":0`) {
		t.Fatalf("known zero median left out: %s", rec.Body.String())
	}
	if got := museStats(t, ro.Agents).MedianReply(); got != "<1s" {
		t.Fatalf("median = %q, want <1s", got)
	}
}

// tincan top reads the roster every 2 seconds, so a second roster inside 60
// seconds runs no query: it shows what the first saw, and the next one after
// that sees the new row.
func TestRosterStatsCachedSixtySeconds(t *testing.T) {
	h, c := newStatsHarness(t)
	h.finishAsk(c, envelope.StatusAnswered, time.Second)
	roster := func() client.AgentStats {
		t.Helper()
		var ro client.Roster
		h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, &ro)
		return museStats(t, ro.Agents)
	}
	if got := roster(); got.Answered != 1 {
		t.Fatalf("first roster = %+v", got)
	}
	h.finishAsk(c, envelope.StatusAnswered, time.Second)
	c.advance(58 * time.Second)
	if got := roster(); got.Answered != 1 {
		t.Fatalf("roster inside 60s = %+v, want the cached record", got)
	}
	c.advance(2 * time.Second)
	if got := roster(); got.Answered != 2 {
		t.Fatalf("roster after 60s = %+v, want a fresh record", got)
	}
}

// A caller cycling through windows gets statsCacheSize store reads a minute
// and then a 429, while windows it already read, the roster's record and its
// cached entry all keep working. The budget comes back with the TTL.
func TestStatsWindowsAreBudgeted(t *testing.T) {
	h, c := newStatsHarness(t)
	h.finishAsk(c, envelope.StatusAnswered, time.Minute)
	roster := func() client.AgentStats {
		t.Helper()
		var ro client.Roster
		h.do(grokAddr, "GET", "/v1/agents", "", http.StatusOK, &ro)
		return museStats(t, ro.Agents)
	}
	if got := roster(); got.Answered != 1 {
		t.Fatalf("first roster = %+v", got)
	}
	h.finishAsk(c, envelope.StatusAnswered, time.Second)

	window := func(i int) string { return "/v1/stats?since=" + time.Duration(i*int(time.Hour)).String() }
	for i := 1; i <= statsCacheSize; i++ {
		h.do(grokAddr, "GET", window(i), "", http.StatusOK, nil)
	}
	for i := statsCacheSize + 1; i <= 3*statsCacheSize; i++ {
		rec := h.do(grokAddr, "GET", window(i), "", http.StatusTooManyRequests, nil)
		if got := rec.Header().Get("Retry-After"); got != "60" {
			t.Fatalf("Retry-After = %q", got)
		}
	}
	h.do(grokAddr, "GET", window(1), "", http.StatusOK, nil)
	h.do(grokAddr, "GET", "/v1/stats", "", http.StatusOK, nil)
	if got := roster(); got.Answered != 1 {
		t.Fatalf("roster = %+v, want the cached record to survive the windows", got)
	}
	h.srv.statsMu.Lock()
	n := len(h.srv.statsCache)
	h.srv.statsMu.Unlock()
	if n != statsCacheSize+1 {
		t.Fatalf("cache holds %d windows, want %d", n, statsCacheSize+1)
	}

	c.advance(statsCacheTTL)
	h.do(grokAddr, "GET", window(3*statsCacheSize), "", http.StatusOK, nil)
}
