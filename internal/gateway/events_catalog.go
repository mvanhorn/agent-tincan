package gateway

import (
	"errors"
	"fmt"
	"strings"
)

// MCP Events protocol and catalog identifiers.
const (
	EventsProtocolVersion = "2026-07-28"
	EventsExtensionID     = "io.modelcontextprotocol/events"
	WakeMethodMCPEvents   = "mcp-events"

	EventInboxRequest      = "inbox.request"
	EventRequestReplied    = "request.replied"
	EventRequestNeedsInput = "request.needs_input"
)

// ErrUnknownEvent means a subscription named an event not in Agent Tincan's catalog.
var ErrUnknownEvent = errors.New("unknown event name")

// EventDefinition describes one event in Agent Tincan's MCP Events catalog.
type EventDefinition struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	DeliveryModes []string       `json:"deliveryModes"`
	InputSchema   map[string]any `json:"inputSchema"`
	PayloadSchema map[string]any `json:"payloadSchema"`
}

// EventCatalog returns the agent-scoped webhook events served on /mcp.
func EventCatalog() []EventDefinition {
	return []EventDefinition{
		{
			Name:          EventInboxRequest,
			Description:   "A request was queued or requeued for this agent. Payload is metadata-only; call check_inbox to read pending requests.",
			DeliveryModes: []string{"webhook"},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"urgent_only": map[string]any{
						"type":        "boolean",
						"description": "When true, deliver events only for urgent requests.",
					},
				},
				"additionalProperties": false,
			},
			PayloadSchema: map[string]any{
				"type":     "object",
				"required": []string{"request_id", "from", "kind", "urgent", "queued_count", "queued_at", "hint"},
				"properties": map[string]any{
					"request_id":   map[string]any{"type": "string"},
					"from":         map[string]any{"type": "string"},
					"kind":         map[string]any{"type": "string", "enum": []string{"ask", "notify", "ping"}},
					"urgent":       map[string]any{"type": "boolean"},
					"queued_count": map[string]any{"type": "integer", "minimum": 0},
					"queued_at":    map[string]any{"type": "string", "format": "date-time"},
					"hint":         map[string]any{"type": "string"},
				},
				"additionalProperties": false,
			},
		},
		{
			Name:          EventRequestReplied,
			Description:   "A reply arrived for an ask this agent sent. Payload is metadata-only; call get_reply or check_inbox to read the response.",
			DeliveryModes: []string{"webhook"},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{
						"type":        "string",
						"description": "Optional request ID to match replies for a single ask.",
					},
				},
				"additionalProperties": false,
			},
			PayloadSchema: map[string]any{
				"type":     "object",
				"required": []string{"request_id", "from", "status", "replied_at", "hint"},
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
					"from":       map[string]any{"type": "string"},
					"status":     map[string]any{"type": "string"},
					"replied_at": map[string]any{"type": "string", "format": "date-time"},
					"hint":       map[string]any{"type": "string"},
				},
				"additionalProperties": false,
			},
		},
		{
			Name:          EventRequestNeedsInput,
			Description:   "A teammate asked for clarification on a request this agent sent. Payload is metadata-only; call check_inbox or get_reply to answer.",
			DeliveryModes: []string{"webhook"},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{
						"type":        "string",
						"description": "Optional request ID to match clarifications for a single ask.",
					},
				},
				"additionalProperties": false,
			},
			PayloadSchema: map[string]any{
				"type":     "object",
				"required": []string{"request_id", "from", "updated_at", "hint"},
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
					"from":       map[string]any{"type": "string"},
					"updated_at": map[string]any{"type": "string", "format": "date-time"},
					"hint":       map[string]any{"type": "string"},
				},
				"additionalProperties": false,
			},
		},
	}
}

// KnownEvent reports whether name is in Agent Tincan's MCP Events catalog.
func KnownEvent(name string) bool {
	switch name {
	case EventInboxRequest, EventRequestReplied, EventRequestNeedsInput:
		return true
	default:
		return false
	}
}

// ValidateEventFilters validates subscription filters against the event's inputSchema
// and returns a normalized copy.
func ValidateEventFilters(name string, filters map[string]any) (map[string]any, error) {
	if !KnownEvent(name) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownEvent, name)
	}
	out := map[string]any{}
	if len(filters) == 0 {
		return out, nil
	}
	switch name {
	case EventInboxRequest:
		for k, v := range filters {
			if k != "urgent_only" {
				return nil, fmt.Errorf("unknown filter %q for %s", k, name)
			}
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("filter urgent_only must be a boolean")
			}
			out["urgent_only"] = b
		}
	case EventRequestReplied, EventRequestNeedsInput:
		for k, v := range filters {
			if k != "request_id" {
				return nil, fmt.Errorf("unknown filter %q for %s", k, name)
			}
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("filter request_id must be a string")
			}
			s = strings.TrimSpace(s)
			if len(s) > 128 {
				return nil, fmt.Errorf("filter request_id is too long")
			}
			if s != "" {
				out["request_id"] = s
			}
		}
	}
	return out, nil
}
