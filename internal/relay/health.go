package relay

// Relay health. A relay whose store has locked up, or whose sweep loop has
// stopped, keeps its process alive and its listener open, so from outside it
// looks exactly like an idle relay. Two routes tell them apart:
//
//   - GET /healthz says only that this process serves HTTP. It needs no join,
//     so it carries nothing else: no names, no counts, no version.
//   - GET /metrics is for admin devices and the local admin socket. It says
//     whether the relay is still doing its work: when its sweep loop last
//     completed a pass, whether the store answers a real query within a
//     bound, and the open backlog. It answers 503 when the relay is stalled,
//     so `curl -f` fails on a wedged relay and passes on an idle one.
//
// Both must answer while the store does not, which is the case they exist
// for, so neither waits on the store for longer than a fixed bound (see
// healthRoutes in guests.go). Over the tailnet, /metrics needs the store to
// check that an admin device carries no guest, so a failed read refuses it
// there; the local admin socket needs no such read and still serves it.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// storeProbeTimeout bounds how long /metrics waits on the store for its
// probe. The store has one connection, so a wedged store makes every query
// wait for it; /metrics must answer anyway.
const storeProbeTimeout = 2 * time.Second

// health is what the relay records for /metrics as it runs. It is in memory,
// so reading it never touches the store.
type health struct {
	lastSweep   atomic.Int64 // unix nanos of the last completed sweep pass; 0 before the first
	passes      atomic.Uint64
	sweepErrors atomic.Uint64
}

// swept records a completed sweep pass.
func (s *Server) swept() {
	s.health.lastSweep.Store(s.cfg.Now().UnixNano())
	s.health.passes.Add(1)
}

// Metrics is the /metrics answer. Ages and the uptime are in seconds. It
// holds counts and ages only: no agent names, request ids or text.
type Metrics struct {
	// Healthy is false when the sweep loop has not completed a pass within
	// StallAfter or the store did not answer within its bound; Problems
	// says which, in words.
	Healthy       bool         `json:"healthy"`
	Problems      []string     `json:"problems"`
	UptimeSeconds float64      `json:"uptime_seconds"`
	Sweep         SweepMetrics `json:"sweep"`
	Store         StoreMetrics `json:"store"`
	Queue         QueueMetrics `json:"queue"`
	// WakeFailedAgents counts relay-woken agents whose last wake failed.
	// It is 0 and unknown when the agents read failed; Store.OK is then
	// false and Problems says so.
	WakeFailedAgents int `json:"wake_failed_agents"`
}

// SweepMetrics describes the relay's sweep loop, which expires requests,
// returns lapsed claims to the queue and re-wakes their agents.
type SweepMetrics struct {
	LastOKAgeSeconds  float64 `json:"last_ok_age_seconds"`
	StallAfterSeconds float64 `json:"stall_after_seconds"`
	Passes            uint64  `json:"passes"`
	Errors            uint64  `json:"errors"`
}

// StoreMetrics says whether the store answered real queries within the
// bound, how long the probe took, and the size of the database on disk.
type StoreMetrics struct {
	OK        bool  `json:"ok"`
	ProbeMS   int64 `json:"probe_ms"`
	SizeBytes int64 `json:"size_bytes"`
}

// QueueMetrics is the open backlog across every agent, from the roster's own
// backlog query. It reads zero when the store is not ok.
type QueueMetrics struct {
	Queued                 int     `json:"queued"`
	Claimed                int     `json:"claimed"`
	OldestQueuedAgeSeconds float64 `json:"oldest_queued_age_seconds"`
}

// handleHealthz is liveness only. It deliberately cannot tell a wedged relay
// from an idle one: anything that could would also tell a caller that never
// joined something about this relay. /metrics does that, for admins.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// The admin check reads the store to rule out a node carrying a guest,
	// and a failed read refuses, as on every admin route. The read is
	// bounded so a wedged store refuses promptly instead of hanging; the
	// local admin socket skips it.
	ctx, cancel := context.WithTimeout(r.Context(), healthLookup)
	admin := s.dir.IsAdmin(ctx, s.remote(r))
	cancel()
	if !admin {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	m := s.metrics(r.Context())
	code := http.StatusOK
	if !m.Healthy {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, m)
}

func (s *Server) metrics(ctx context.Context) Metrics {
	now := s.cfg.Now()
	m := Metrics{Problems: []string{}, UptimeSeconds: now.Sub(s.started).Seconds()}

	// The sweep loop. Before its first pass, measure from the start, so a
	// relay that just started is not called stalled.
	last := s.started
	if ns := s.health.lastSweep.Load(); ns != 0 {
		last = time.Unix(0, ns)
	}
	age := now.Sub(last)
	m.Sweep = SweepMetrics{LastOKAgeSeconds: age.Seconds(), StallAfterSeconds: s.cfg.StallAfter.Seconds(),
		Passes: s.health.passes.Load(), Errors: s.health.sweepErrors.Load()}
	if age > s.cfg.StallAfter {
		m.Problems = append(m.Problems, fmt.Sprintf("sweep loop has not completed a pass in %s (stall after %s)", age.Round(time.Millisecond), s.cfg.StallAfter))
	}

	// The store: the roster's backlog query, then the agents read, both
	// under one bound.
	pctx, cancel := context.WithTimeout(ctx, storeProbeTimeout)
	defer cancel()
	start := time.Now()
	stats, err := s.store.QueueStats(pctx)
	if err != nil {
		log.Printf("metrics: store probe: %v", err)
		m.Problems = append(m.Problems, fmt.Sprintf("store did not answer within %s", storeProbeTimeout))
	} else {
		m.Store.OK = true
		var oldest time.Time
		for _, st := range stats {
			m.Queue.Queued += st.Queued
			m.Queue.Claimed += st.Claimed
			if !st.OldestQueued.IsZero() && (oldest.IsZero() || st.OldestQueued.Before(oldest)) {
				oldest = st.OldestQueued
			}
		}
		if !oldest.IsZero() {
			m.Queue.OldestQueuedAgeSeconds = now.Sub(oldest).Seconds()
		}
		// A failed agents read is the store's failure too: reporting
		// healthy with a wake_failed_agents of 0 that was never measured
		// would hide it.
		if n, err := s.wakeFailedAgents(pctx); err != nil {
			log.Printf("metrics: agents: %v", err)
			m.Store.OK = false
			m.Problems = append(m.Problems, "store did not answer the agents read; wake_failed_agents is unknown")
		} else {
			m.WakeFailedAgents = n
		}
	}
	m.Store.ProbeMS = time.Since(start).Milliseconds()
	m.Store.SizeBytes = storeSize(s.store.Path())
	m.Healthy = len(m.Problems) == 0
	return m
}

// wakeFailedAgents counts relay-woken agents whose last wake since they
// joined was not accepted, from the waker's own record, as the roster reads
// it.
func (s *Server) wakeFailedAgents(ctx context.Context) (int, error) {
	wr, ok := s.wake.(WakeReporter)
	if !ok {
		return 0, nil
	}
	agents, err := s.dir.Agents(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range agents {
		if wk, ok := wr.LastWake(a.Name); ok && relayWoken(s.wake.WakeMethod(a.Name)) && !wk.At.Before(a.JoinedAt) && !envelope.WakeAccepted(wk.Result) {
			n++
		}
	}
	return n, nil
}

// storeSize is the database file plus its write-ahead log, 0 for an
// in-memory store. It reads the file system, never the store.
func storeSize(path string) int64 {
	if path == "" {
		return 0
	}
	var n int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}
