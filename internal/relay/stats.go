package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

const (
	// statsDefaultWindow is the window the roster shows and /v1/stats uses
	// when a caller names none.
	statsDefaultWindow = 7 * 24 * time.Hour
	statsMaxDays       = 365
	// statsCacheTTL keeps roster reads off the store: tincan top reads the
	// roster every 2 seconds.
	statsCacheTTL = 60 * time.Second
	// statsCacheSize is how many caller-chosen windows the relay reads per
	// statsCacheTTL. Each read walks the window's finished asks on the store's
	// one connection, so the cache is also the budget; the default window sits
	// outside it, and the roster always has its read.
	statsCacheSize = 8
)

// errStatsBusy refuses a window past the budget.
var errStatsBusy = errors.New("too many stats windows read this minute: try again in a minute")

// statsEntry is one window's record, read at at.
type statsEntry struct {
	at      time.Time
	since   time.Time
	byAgent map[string]client.AgentStats
}

// parseStatsWindow reads a window such as 7d or 12h; "" is the default.
func parseStatsWindow(s string) (time.Duration, error) {
	if s == "" {
		return statsDefaultWindow, nil
	}
	bad := fmt.Errorf("cannot read since %q: use a duration such as 7d or 12h, at most %dd", s, statsMaxDays)
	d, err := time.ParseDuration(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, convErr := strconv.Atoi(days)
		if convErr != nil || n > statsMaxDays {
			return 0, bad
		}
		d, err = time.Duration(n)*24*time.Hour, nil
	}
	if err != nil || d <= 0 || d > statsMaxDays*24*time.Hour {
		return 0, bad
	}
	return d, nil
}

// agentStats returns window's record, reading the store at most once per
// statsCacheTTL. Holding statsMu across the read lets concurrent callers
// share it. A failed read is not kept. A window other than the default past
// statsCacheSize live windows answers errStatsBusy.
func (s *Server) agentStats(ctx context.Context, window time.Duration) (statsEntry, error) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	now := s.cfg.Now()
	if e, ok := s.statsCache[window]; ok && now.Sub(e.at) < statsCacheTTL {
		return e, nil
	}
	live := 0
	for w, e := range s.statsCache {
		if now.Sub(e.at) >= statsCacheTTL {
			delete(s.statsCache, w)
		} else if w != statsDefaultWindow {
			live++
		}
	}
	if window != statsDefaultWindow && live >= statsCacheSize {
		return statsEntry{}, errStatsBusy
	}
	since := now.Add(-window)
	stats, err := s.store.AgentStats(ctx, since)
	if err != nil {
		return statsEntry{}, err
	}
	e := statsEntry{at: now, since: since, byAgent: make(map[string]client.AgentStats, len(stats))}
	for agent, st := range stats {
		rec := client.AgentStats{Answered: st.Answered, Failed: st.Failed, Expired: st.Expired}
		if st.HasMedian {
			rec.MedianReplyMS = new(st.MedianReply.Milliseconds())
		}
		e.byAgent[agent] = rec
	}
	s.statsCache[window] = e
	return e, nil
}

// addStats fills each roster entry's seven-day record. The roster stands
// without it, so a failed read is only logged.
func (s *Server) addStats(ctx context.Context, roster []client.AgentInfo) {
	e, err := s.agentStats(ctx, statsDefaultWindow)
	if err != nil {
		log.Printf("agents stats: %v", err)
		return
	}
	for i := range roster {
		roster[i].Stats = e.byAgent[roster[i].Name]
	}
}

// handleStats lists every joined agent's record for joined agents and admin
// devices, as handleAgents does the roster: a guest is listed for an admin
// only.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	admin := s.isAdmin(r)
	if !admin && s.agent(w, r) == "" {
		return
	}
	window, err := parseStatsWindow(r.URL.Query().Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	agents, err := s.dir.Agents(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	e, err := s.agentStats(r.Context(), window)
	if errors.Is(err, errStatsBusy) {
		w.Header().Set("Retry-After", strconv.Itoa(int(statsCacheTTL/time.Second)))
		writeErr(w, http.StatusTooManyRequests, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := client.StatsReport{Since: e.since, Agents: make([]client.AgentStats, 0, len(agents))}
	for _, a := range agents {
		if a.Guest.Set() && !admin {
			continue
		}
		st := e.byAgent[a.Name]
		st.Agent = a.Name
		out.Agents = append(out.Agents, st)
	}
	writeJSON(w, http.StatusOK, out)
}
