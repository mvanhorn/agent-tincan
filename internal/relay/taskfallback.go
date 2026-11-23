package relay

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

func (s *Server) handleSetNotifyDestination(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	var in struct {
		Notify *string `json:"notify"`
	}
	if err := decodeOne(http.MaxBytesReader(w, r.Body, 1024), &in, "notify setting"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Notify == nil {
		writeErr(w, http.StatusBadRequest, errors.New("notify is required; send an empty string to clear it"))
		return
	}
	name := *in.Notify
	if name != "" {
		joined, err := s.dir.Has(r.Context(), name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !joined {
			writeErr(w, http.StatusNotFound, identity.ErrUnknownAgent)
			return
		}
	}
	p, ok := s.preparer().(interface{ SetNotifyDestination(string) error })
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, errors.New("approval policy updates are unavailable on this relay"))
		return
	}
	if err := p.SetNotifyDestination(name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.record(context.WithoutCancel(r.Context()), "relay_notify_updated", "", "", s.remote(r), store.DetailJSON(map[string]any{"notify": name}))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) warnTaskFallback(ctx context.Context) {
	agents, err := s.dir.Agents(ctx)
	if err != nil {
		log.Printf("task fallback: read agents: %v", err)
		return
	}
	status := client.TaskFallbackStatus{NotifySet: s.notifyDestination() != ""}
	for _, agent := range agents {
		if agent.TaskHost {
			status.Hosts = append(status.Hosts, agent.Name)
		}
	}
	if warning := status.Warning(); warning != "" {
		log.Printf("warning: %s %s", warning, client.TaskFallbackFix)
	}
}
