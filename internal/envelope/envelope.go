// Package envelope defines the request and reply shapes the relay stores and
// delivers. The relay owns identity and chain fields: From comes from WhoIs,
// and ID, TraceID, Hop, Chain, and CreatedAt are assigned when a request is
// queued. A client only supplies To, Body, Kind, and an optional ParentID.
package envelope

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultMaxBody caps a request or reply body.
const DefaultMaxBody = 256 << 10

// ErrBodyTooLarge is returned when a body exceeds the configured cap.
var ErrBodyTooLarge = errors.New("body too large")

// Kind says what the sender expects back.
type Kind string

const (
	// KindAsk expects a reply.
	KindAsk Kind = "ask"
	// KindNotify is fire-and-forget; the target may still reply.
	KindNotify Kind = "notify"
)

// Status is a request's lifecycle state.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusDelivered Status = "delivered"
	StatusClaimed   Status = "claimed"
	StatusAnswered  Status = "answered"
	StatusFailed    Status = "failed"
	StatusDeclined  Status = "declined"
	StatusCancelled Status = "cancelled"
	StatusExpired   Status = "expired"
)

// Terminal reports whether s is a final reply status an agent may set.
func (s Status) Terminal() bool {
	return s == StatusAnswered || s == StatusFailed || s == StatusDeclined
}

// Request is one agent asking another to do something.
type Request struct {
	ID        string    `json:"id,omitempty"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to"`
	ParentID  string    `json:"parent_id,omitempty"`
	TraceID   string    `json:"trace_id,omitempty"`
	Hop       int       `json:"hop,omitempty"`
	Chain     []string  `json:"chain,omitempty"`
	Kind      Kind      `json:"kind,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// Pending names a queued request without its body, as a peek reports it.
type Pending struct {
	ID   string `json:"id"`
	From string `json:"from"`
}

// Reply is the target's answer to a request.
type Reply struct {
	RequestID string    `json:"request_id,omitempty"`
	From      string    `json:"from,omitempty"`
	Status    Status    `json:"status,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// Result is a request with its current status and reply, if any. The relay
// returns it for get-reply and for each step of a trace.
type Result struct {
	Request Request `json:"request"`
	Status  Status  `json:"status"`
	Reply   *Reply  `json:"reply,omitempty"`
	// Parent is set on an unseen reply whose request was asked while the
	// asker was handling another request addressed to it, so a fresh session
	// woken by the reply knows which request to finish.
	Parent *Parent `json:"parent,omitempty"`
}

// Parent summarizes the request an ask was made while handling: who sent
// it, a preview of its body, and its current status.
type Parent struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	Body   string `json:"body"`
	Status Status `json:"status"`
}

// Done reports whether the request has reached a final state.
func (r Result) Done() bool {
	return r.Status.Terminal() || r.Status == StatusCancelled || r.Status == StatusExpired
}

// sendInput is the only part of a send the relay accepts from a client.
type sendInput struct {
	To       string `json:"to"`
	Body     string `json:"body"`
	Kind     Kind   `json:"kind"`
	ParentID string `json:"parent_id"`
}

// ParseSend decodes a client's send and attributes it to sender, the agent
// name the relay resolved from WhoIs. Client-supplied identity and chain
// fields are ignored.
func ParseSend(raw []byte, sender string, maxBody int) (Request, error) {
	var in sendInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Request{}, fmt.Errorf("decode send: %w", err)
	}
	in.To = strings.TrimSpace(in.To)
	switch {
	case in.To == "":
		return Request{}, errors.New("send: to is required")
	case in.To == sender:
		return Request{}, errors.New("send: an agent cannot send to itself")
	case in.Body == "":
		return Request{}, errors.New("send: body is required")
	case len(in.Body) > maxBody:
		return Request{}, fmt.Errorf("send: %w (%d > %d bytes)", ErrBodyTooLarge, len(in.Body), maxBody)
	}
	switch in.Kind {
	case "":
		in.Kind = KindAsk
	case KindAsk, KindNotify:
	default:
		return Request{}, fmt.Errorf("send: unknown kind %q", in.Kind)
	}
	return Request{From: sender, To: in.To, ParentID: in.ParentID, Kind: in.Kind, Body: in.Body}, nil
}

type replyInput struct {
	Body   string `json:"body"`
	Status Status `json:"status"`
}

// ParseReply decodes a client's reply. A missing status means answered.
func ParseReply(raw []byte, maxBody int) (Reply, error) {
	var in replyInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Reply{}, fmt.Errorf("decode reply: %w", err)
	}
	if len(in.Body) > maxBody {
		return Reply{}, fmt.Errorf("reply: %w (%d > %d bytes)", ErrBodyTooLarge, len(in.Body), maxBody)
	}
	if in.Status == "" {
		in.Status = StatusAnswered
	}
	if !in.Status.Terminal() {
		return Reply{}, fmt.Errorf("reply: status %q is not answered, failed, or declined", in.Status)
	}
	return Reply{Status: in.Status, Body: in.Body}, nil
}
