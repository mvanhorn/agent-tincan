package relay

import (
	"context"
	"log"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

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
