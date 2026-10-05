package client

import (
	"context"
	"errors"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"net/http"
)

// ErrWebStatusUnsupported identifies a relay that predates self reports.
var ErrWebStatusUnsupported = errors.New("relay does not support web status; upgrade the relay")

// ReportWebStatus reports only this client's authenticated identity.
func (r *Relay) ReportWebStatus(ctx context.Context, observation envelope.WebStatus) error {
	err := r.call(ctx, r.api, "PUT", "/v1/agents/self/web-status", observation, nil)
	if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusMethodNotAllowed) {
		return ErrWebStatusUnsupported
	}
	return err
}

// SendResult preserves recipient facts for callers sending notifications.
func (r *Relay) SendResult(ctx context.Context, to, body string, kind envelope.Kind, parent string, urgent bool) (envelope.SendResponse, error) {
	return r.send(ctx, map[string]any{"to": to, "body": body, "kind": kind, "parent_id": parent, "urgent": urgent})
}

// SendAttachedResult is the additive counterpart to SendAttached.
func (r *Relay) SendAttachedResult(ctx context.Context, to, body string, kind envelope.Kind, parent string, attachments []string, urgent bool) (envelope.SendResponse, error) {
	return r.sendAttached(ctx, to, body, kind, parent, attachments, urgent)
}
