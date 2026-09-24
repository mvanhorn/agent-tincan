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

// MaxAttachments caps how many attachments one request or reply carries.
const MaxAttachments = 8

var (
	// ErrBodyTooLarge is returned when a body exceeds the configured cap.
	ErrBodyTooLarge = errors.New("body too large")
	// ErrTooManyAttachments is returned when a message names more than
	// MaxAttachments attachments.
	ErrTooManyAttachments = errors.New("too many attachments")
)

// Attachment is a file stored on the relay and carried by a request or
// reply. A client names it by ID, the id the upload returned; the relay
// fills Name, MIME, and Size from the upload. Name is display metadata only
// and never builds a path.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	MIME string `json:"mime,omitempty"`
	Size int64  `json:"size,omitempty"`
}

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
	ID       string   `json:"id,omitempty"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to"`
	ParentID string   `json:"parent_id,omitempty"`
	TraceID  string   `json:"trace_id,omitempty"`
	Hop      int      `json:"hop,omitempty"`
	Chain    []string `json:"chain,omitempty"`
	Kind     Kind     `json:"kind,omitempty"`
	Body     string   `json:"body"`
	// Attachments are files stored on the relay. A relay that predates
	// attachments ignores the field, so clients send it only when the relay
	// advertises support.
	Attachments []Attachment `json:"attachments,omitempty"`
	CreatedAt   time.Time    `json:"created_at,omitzero"`
}

// Pending names a queued request without its body, as a peek reports it.
type Pending struct {
	ID   string `json:"id"`
	From string `json:"from"`
}

// Reply is the target's answer to a request.
type Reply struct {
	RequestID   string       `json:"request_id,omitempty"`
	From        string       `json:"from,omitempty"`
	Status      Status       `json:"status,omitempty"`
	Body        string       `json:"body"`
	Attachments []Attachment `json:"attachments,omitempty"`
	CreatedAt   time.Time    `json:"created_at,omitzero"`
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
	To          string       `json:"to"`
	Body        string       `json:"body"`
	Kind        Kind         `json:"kind"`
	ParentID    string       `json:"parent_id"`
	Attachments []Attachment `json:"attachments"`
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
	case in.Body == "" && len(in.Attachments) == 0:
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
	atts, err := attachmentIDs(in.Attachments)
	if err != nil {
		return Request{}, fmt.Errorf("send: %w", err)
	}
	return Request{From: sender, To: in.To, ParentID: in.ParentID, Kind: in.Kind, Body: in.Body, Attachments: atts}, nil
}

// attachmentIDs keeps only the ids a client names, checking the count and
// that each is present and named once. Name, MIME, and Size are dropped:
// the relay fills them from the upload.
func attachmentIDs(in []Attachment) ([]Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxAttachments {
		return nil, fmt.Errorf("%w (%d > %d)", ErrTooManyAttachments, len(in), MaxAttachments)
	}
	out := make([]Attachment, 0, len(in))
	seen := map[string]bool{}
	for _, a := range in {
		id := strings.TrimSpace(a.ID)
		switch {
		case id == "":
			return nil, errors.New("attachment id is required")
		case seen[id]:
			return nil, fmt.Errorf("attachment %s is named twice", id)
		}
		seen[id] = true
		out = append(out, Attachment{ID: id})
	}
	return out, nil
}

type replyInput struct {
	Body        string       `json:"body"`
	Status      Status       `json:"status"`
	Attachments []Attachment `json:"attachments"`
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
	atts, err := attachmentIDs(in.Attachments)
	if err != nil {
		return Reply{}, fmt.Errorf("reply: %w", err)
	}
	return Reply{Status: in.Status, Body: in.Body, Attachments: atts}, nil
}
