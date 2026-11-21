package client

import (
	"context"
	"fmt"
	"strings"
)

const TaskFallbackFix = "Set notify in the relay's approval.json to a joined agent that can alert you."

// TaskFallbackStatus is visible to admin callers of the relay roster.
type TaskFallbackStatus struct {
	Hosts     []string `json:"hosts"`
	NotifySet bool     `json:"notify_set"`
}

func (f TaskFallbackStatus) Warning() string {
	if len(f.Hosts) == 0 || f.NotifySet {
		return ""
	}
	return fmt.Sprintf("Task hosts %s have no fallback notification destination.", strings.Join(f.Hosts, ", "))
}

// TaskFallback reads the admin-only task delivery status. A nil result
// means the caller is not an admin or the relay predates this field.
func (r *Relay) TaskFallback(ctx context.Context) (*TaskFallbackStatus, error) {
	roster, err := r.Roster(ctx)
	return roster.TaskFallback, err
}
