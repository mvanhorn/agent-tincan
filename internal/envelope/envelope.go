// Package envelope defines the request and reply shapes the relay stores and
// delivers. The relay owns identity and chain fields: From comes from WhoIs,
// and ID, TraceID, Hop, Chain, and CreatedAt are assigned when a request is
// queued. A client only supplies To, Body, Kind, and an optional ParentID.
package envelope

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// DefaultMaxBody caps a request or reply body.
const DefaultMaxBody = 256 << 10

// MaxInputBody caps each clarification question and answer in bytes.
const MaxInputBody = 16 << 10

// MaxExchanges caps clarification rounds per request.
const MaxExchanges = 3

// MaxProgressNote caps a progress note in bytes.
const MaxProgressNote = 1024

// MaxAttachments caps how many attachments one request or reply carries.
const MaxAttachments = 8

var (
	groupID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
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
	// KindPing is answered automatically by the receiving client.
	KindPing Kind = "ping"
	// KindNotify is fire-and-forget; the target may still reply.
	KindNotify Kind = "notify"
)

// Status is a request's lifecycle state.
type Status string

const (
	StatusHeld       Status = "held"
	StatusQueued     Status = "queued"
	StatusDelivered  Status = "delivered"
	StatusClaimed    Status = "claimed"
	StatusNeedsInput Status = "needs_input"
	StatusAnswered   Status = "answered"
	StatusFailed     Status = "failed"
	StatusDeclined   Status = "declined"
	StatusCancelled  Status = "cancelled"
	StatusExpired    Status = "expired"
)

// Terminal reports whether s is a final reply status an agent may set.
func (s Status) Terminal() bool {
	return s == StatusAnswered || s == StatusFailed || s == StatusDeclined
}

// Request is one agent asking another to do something.
type Request struct {
	Exchanges []Exchange `json:"exchanges,omitempty"`
	Resumed   bool       `json:"resumed,omitempty"`
	Group     string     `json:"group,omitempty"`
	Urgent    bool       `json:"urgent,omitempty"`
	ID        string     `json:"id,omitempty"`
	From      string     `json:"from,omitempty"`
	To        string     `json:"to"`
	ParentID  string     `json:"parent_id,omitempty"`
	TraceID   string     `json:"trace_id,omitempty"`
	Hop       int        `json:"hop,omitempty"`
	Chain     []string   `json:"chain,omitempty"`
	Kind      Kind       `json:"kind,omitempty"`
	Body      string     `json:"body"`
	// Attachments are files stored on the relay. A relay that predates
	// attachments ignores the field, so clients send it only when the relay
	// advertises support.
	Attachments []Attachment `json:"attachments,omitempty"`
	CreatedAt   time.Time    `json:"created_at,omitzero"`
	// Progress is store metadata; the wire exposes it on Result.
	Progress *Progress `json:"-"`

	// Status is set on held sends; omitted for ordinary requests.
	Status Status `json:"status,omitempty"`
	// HoldTTL and ApprovalNotify are internal policy metadata, never wire input.
	HoldTTL        time.Duration `json:"-"`
	ApprovalNotify string        `json:"-"`
	// WasHeld and Approved are persisted relay-only approval history.
	WasHeld  bool `json:"-"`
	Approved bool `json:"-"`
}

// RedactFor hides content that the owner has never released to other agents.
func (r *Request) RedactFor(agent string) {
	if r.WasHeld && !r.Approved && r.From != agent {
		r.Body = "waiting for the owner's approval"
		r.Attachments = nil
	}
}

// GroupMember identifies a request without fetching its result or marking replies seen.
type GroupMember struct {
	ID string `json:"id"`
	To string `json:"to"`
}

// Pending names a queued request without its body, as a peek reports it.
type Pending struct {
	Kind   Kind   `json:"kind,omitempty"`
	Urgent bool   `json:"urgent,omitempty"`
	ID     string `json:"id"`
	From   string `json:"from"`
}

// Held is a claimed ask the agent has not replied to, as the relay reports
// it on the agent's own calls so the agent handles it before other work.
type Held struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	Urgent    bool      `json:"urgent,omitempty"`
	ClaimedAt time.Time `json:"claimed_at,omitzero"`
}

// ReplyAck identifies a specific reply generation for acknowledgement.
type ReplyAck struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
}

// Reply is the target's answer to a request.
type Reply struct {
	Generation  int64        `json:"generation"`
	RequestID   string       `json:"request_id,omitempty"`
	From        string       `json:"from,omitempty"`
	Status      Status       `json:"status,omitempty"`
	Body        string       `json:"body"`
	Attachments []Attachment `json:"attachments,omitempty"`
	CreatedAt   time.Time    `json:"created_at,omitzero"`
}

// Progress is the latest note from the agent handling a request.
type Progress struct {
	Note string    `json:"note"`
	At   time.Time `json:"at"`
	By   string    `json:"by"`
}

// Result is a request with its current status and reply, if any. The relay
// returns it for get-reply and for each step of a trace.
type Result struct {
	Progress  *Progress  `json:"progress,omitempty"`
	Exchanges []Exchange `json:"exchanges,omitempty"`
	Request   Request    `json:"request"`
	Status    Status     `json:"status"`
	Reply     *Reply     `json:"reply,omitempty"`
	// Parent is set on an unseen reply whose request was asked while the
	// asker was handling another request addressed to it, so a fresh session
	// woken by the reply knows which request to finish.
	Parent *Parent `json:"parent,omitempty"`
	// Target is what the relay said about the recipient when the request
	// was sent. It is wire-only: the relay never stores it or returns it
	// from a get; the client copies it from the send response onto the
	// Result that an ask returns. Nil unless the recipient checks its inbox
	// on a schedule, and always nil from a relay that predates schedules.
	Target *Target `json:"target,omitempty"`
	// RelayNote is the latest note the relay itself added to the request,
	// By "relay": that the target was woken and never checked in, that its
	// claim went stale, or that the request expired with no reply. Only the
	// asker sees it. Older relays never send it and older clients ignore
	// it.
	RelayNote *Progress `json:"relay_note,omitempty"`
}

// SendResponse is the relay's reply to a send: the queued request and, for
// a recipient on a schedule, its schedule facts, or for a recipient the relay
// wakes, its last wake. Older relays send only the request and older clients
// ignore Target.
type SendResponse struct {
	Request
	Target *Target `json:"target,omitempty"`
}

// Target is the relay's facts about a recipient: its schedule when it
// checks its inbox on its own interval, or its last wake when the relay
// wakes it (webhook or email). Every field is optional.
type Target struct {
	SignedOutSite       string    `json:"signed_out_site,omitempty"`
	SignedOutSince      time.Time `json:"signed_out_since,omitzero"`
	WebStatusObservedAt time.Time `json:"web_status_observed_at,omitzero"`
	WebHost             string    `json:"web_host,omitempty"`
	// CheckEverySeconds is how often the recipient checks its inbox.
	CheckEverySeconds int `json:"check_every_seconds,omitempty"`
	// ExpectReplySeconds is how long a sender should expect to wait for a
	// reply: one interval plus a grace for a late check.
	ExpectReplySeconds int `json:"expect_reply_seconds,omitempty"`
	// Overdue is set when the recipient has missed its checks: its last
	// inbox poll (its join, if it never polled) is more than two intervals
	// plus the grace ago.
	Overdue bool `json:"overdue,omitempty"`

	// WokenAt is when the relay last sent the recipient a wake, set only
	// for a relay-woken recipient it has woken.
	WokenAt time.Time `json:"woken_at,omitzero"`
	// WakeResult is that wake's result: "ok", or the error that made the
	// send and its retry fail.
	WakeResult string `json:"wake_result,omitempty"`
	// Unanswered is set when the recipient has not checked in since that
	// wake: the send failed, or the wake grace has passed without a poll.
	Unanswered bool `json:"unanswered,omitempty"`
}

// WakeOK is Target.WakeResult for a wake the agent's platform accepted.
const WakeOK = "ok"

// Exchange is one clarification round; At is when the question was asked.
type Exchange struct {
	Question string    `json:"question"`
	Answer   string    `json:"answer,omitempty"`
	At       time.Time `json:"at"`
}

// ValidateInput checks a clarification body before it is stored.
func ValidateInput(body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("clarification body is required")
	}
	if len(body) > MaxInputBody {
		return ErrBodyTooLarge
	}
	return nil
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
	Group       string       `json:"group"`
	Urgent      bool         `json:"urgent"`
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
	if in.Group != "" && !groupID.MatchString(in.Group) {
		return Request{}, errors.New("send: invalid group id")
	}
	in.To = strings.TrimSpace(in.To)
	switch {
	case in.To == "":
		return Request{}, errors.New("send: to is required")
	case in.To == sender:
		return Request{}, errors.New("send: an agent cannot send to itself")
	case in.Kind != KindPing && in.Body == "" && len(in.Attachments) == 0:
		return Request{}, errors.New("send: body is required")
	case len(in.Body) > maxBody:
		return Request{}, fmt.Errorf("send: %w (%d > %d bytes)", ErrBodyTooLarge, len(in.Body), maxBody)
	}
	switch in.Kind {
	case "":
		in.Kind = KindAsk
	case KindPing:
		if len(in.Body) > 4 || in.ParentID != "" || len(in.Attachments) != 0 {
			return Request{}, errors.New("send: ping requires no parent or attachments and at most 4 body bytes")
		}
		in.Body = ""
	case KindAsk, KindNotify:
	default:
		return Request{}, fmt.Errorf("send: unknown kind %q", in.Kind)
	}
	atts, err := attachmentIDs(in.Attachments)
	if err != nil {
		return Request{}, fmt.Errorf("send: %w", err)
	}
	return Request{Group: in.Group, Urgent: in.Urgent, From: sender, To: in.To, ParentID: in.ParentID, Kind: in.Kind, Body: in.Body, Attachments: atts}, nil
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
	if in.Status == StatusNeedsInput {
		if err := ValidateInput(in.Body); err != nil {
			return Reply{}, err
		}
		if len(in.Attachments) != 0 {
			return Reply{}, errors.New("clarifications do not accept attachments")
		}
	} else if !in.Status.Terminal() {
		return Reply{}, fmt.Errorf("reply: status %q is not answered, failed, declined, or needs_input", in.Status)
	}
	atts, err := attachmentIDs(in.Attachments)
	if err != nil {
		return Reply{}, fmt.Errorf("reply: %w", err)
	}
	return Reply{Status: in.Status, Body: in.Body, Attachments: atts}, nil
}

// SearchResult is a matching request or reply excerpt, without full bodies or files.
type SearchResult struct {
	RequestID    string    `json:"request_id"`
	TraceID      string    `json:"trace_id"`
	From         string    `json:"from"`
	To           string    `json:"to"`
	Status       Status    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	Snippet      string    `json:"snippet,omitempty"`
	ReplySnippet string    `json:"reply_snippet,omitempty"`
	// QuestionSnippet excerpts a clarification question (a needs_input
	// reply) that matched. It is an exchange on the request, never its reply.
	QuestionSnippet string   `json:"question_snippet,omitempty"`
	AttachmentNames []string `json:"attachment_names,omitempty"`
}

// WebStatus is a fresh, credential-free authentication observation.
type WebStatus struct {
	Site       string    `json:"site"`
	State      string    `json:"state"`
	ObservedAt time.Time `json:"observed_at"`
}
