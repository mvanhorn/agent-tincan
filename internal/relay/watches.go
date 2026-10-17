package relay

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// WatchTTL is how long a watch waits for its target to become idle before
// it lapses unsent.
const WatchTTL = 24 * time.Hour

// handleWatch records that the calling agent wants one relay notice when
// the named agent is next idle. The watcher is the authenticated caller.
// Asking again for the same agent while a watch is pending answers 200
// with that watch; a new one answers 201.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	var in struct {
		Agent string `json:"agent"`
	}
	if err := decodeOne(r.Body, &in, "watch object"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.isAgent(r.Context(), in.Agent) {
		writeErr(w, http.StatusNotFound, errors.New("unknown agent"))
		return
	}
	wt, created, err := s.store.AddWatch(r.Context(), name, in.Agent, WatchTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, client.WatchResult{Agent: wt.Target, ExpiresAt: wt.Expires.UTC()})
}

// checkWatches fires every pending watch whose target is free now: idle,
// or offline with nothing in progress or waiting, which is as free as an
// agent the relay wakes on demand gets. Busy, stuck and any later status
// (a limited web agent) are not free. The sweep calls it with target ""
// to check every watch; a reply calls it with the replying agent, since
// only that agent's status can have changed, and skips the roster when no
// watch is on it. The store's fire is conditional, so the two racing send
// one notice.
func (s *Server) checkWatches(ctx context.Context, target string) {
	pending, err := s.store.PendingWatches(ctx)
	if err != nil {
		log.Printf("watches: %v", err)
		return
	}
	if target != "" {
		pending = slices.DeleteFunc(pending, func(wt store.Watch) bool { return wt.Target != target })
	}
	if len(pending) == 0 {
		return
	}
	roster, err := s.roster(ctx, false)
	if err != nil {
		log.Printf("watches: roster: %v", err)
		return
	}
	status := make(map[string]string, len(roster))
	for _, a := range roster {
		status[a.Name] = a.Status
	}
	for _, wt := range pending {
		body := ""
		switch status[wt.Target] {
		case client.StatusIdle:
			body = wt.Target + " is idle now. No reply needed."
		case client.StatusOffline:
			body = wt.Target + " is not busy now (offline). No reply needed."
		default:
			continue
		}
		notice := envelope.Request{From: client.RelaySender, To: wt.Watcher, Kind: envelope.KindNotify, Hop: 1, Chain: []string{client.RelaySender}, Body: body}
		n, fired, err := s.store.FireWatch(ctx, wt, notice, s.requestTTL(ctx, wt.Watcher))
		if err != nil {
			log.Printf("watch %d: %v", wt.ID, err)
			continue
		}
		if !fired {
			continue
		}
		s.record(ctx, "watch_fired", n.ID, n.TraceID, "relay", store.DetailJSON(map[string]any{"watcher": wt.Watcher, "target": wt.Target}))
		s.noticeQueued(ctx, n)
	}
}
