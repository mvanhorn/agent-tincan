package client

import (
	"context"
	"errors"
	"net/http"
)

// ErrHostStatusUnsupported identifies a relay that predates host-reported
// status.
var ErrHostStatusUnsupported = errors.New("relay does not support host status; upgrade the relay")

// ReportHostStatus tells the relay whether this session's host is working
// right now (a Claude Code session in the middle of a turn), so the roster
// can show the agent busy without any claimed work. The relay files it
// under the authenticated agent and this client's session and drops it if
// no report refreshes it within the relay's session grace.
func (r *Relay) ReportHostStatus(ctx context.Context, busy bool) error {
	err := r.call(ctx, r.api, "PUT", "/v1/agents/self/status", map[string]bool{"busy": busy}, nil)
	if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusMethodNotAllowed) {
		return ErrHostStatusUnsupported
	}
	return err
}
