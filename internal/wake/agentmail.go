package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// AgentMail inbox reads, for email replies.
//
// The relay polls the sending inbox of an agent with include_requests on
// for the agent's replies to its request emails (relay/emailreplies.go).
// These are the three calls that needs, next to the send in sendEmail: list
// received mail, get one message for its reply-stripped text, and reply in
// the thread. The relay never deletes, marks read or relabels mail: other
// readers of a shared inbox see their mail unchanged.
//
// The key goes only in the Authorization header. Errors name the AgentMail
// host and the HTTP status, never the inbox, a message id, the key or any
// mail text, so they are safe for the relay log.

// MailMessage is one AgentMail message, with the fields the relay reads.
// The JSON names are AgentMail's (docs.agentmail.to): a list item carries
// everything but the text fields, which only a get returns.
type MailMessage struct {
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id"`
	// From is the sender as AgentMail reports it, which may be a bare
	// address or "Name <address>".
	From    string   `json:"from"`
	Subject string   `json:"subject"`
	Labels  []string `json:"labels"`
	// Timestamp is when AgentMail received the message.
	Timestamp time.Time `json:"timestamp"`
	// ExtractedText is the new text of a reply, with quoted history
	// stripped by AgentMail. Nil when AgentMail returned none.
	ExtractedText *string          `json:"extracted_text"`
	Attachments   []MailAttachment `json:"attachments"`
}

// MailAttachment is an attachment on a MailMessage. The relay only counts
// them: no attachment is read or kept from email.
type MailAttachment struct {
	AttachmentID string `json:"attachment_id"`
}

// HasLabel reports whether m carries label.
func (m MailMessage) HasLabel(label string) bool { return slices.Contains(m.Labels, label) }

// AgentMail reads and replies in one AgentMail inbox.
type AgentMail struct {
	api   string
	inbox string
	key   string
	http  *http.Client
	// Sleep waits out a 429's Retry-After; tests replace it. It returns
	// ctx's error when ctx ends first.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultAgentMailAPI is the AgentMail API base URL, used for wake emails
// and for reading email replies unless a test points elsewhere.
const DefaultAgentMailAPI = "https://api.agentmail.to/v0"

// NewAgentMail returns a client for inbox at api (default
// DefaultAgentMailAPI) with key. A nil hc uses the relay's usual
// outbound client.
func NewAgentMail(api, inbox, key string, hc *http.Client) *AgentMail {
	if api == "" {
		api = DefaultAgentMailAPI
	}
	if hc == nil {
		hc = client.New(client.APIClient)
	}
	return &AgentMail{api: api, inbox: inbox, key: key, http: hc, Sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maxListPages bounds how many pages one Received call reads, so a busy
// shared inbox cannot keep one poll going forever. What is left is read by
// the next poll, since the caller only moves its cursor past a full read.
const maxListPages = 20

// ErrListTruncated means Received stopped at maxListPages with more pages
// left; the messages it returns are still good.
var ErrListTruncated = errors.New("AgentMail list has more pages than one poll reads")

// Received lists the inbox's received mail with a timestamp after after,
// oldest first, following page tokens. It asks only for the received label and never
// for spam or unauthenticated mail, which AgentMail leaves out by default.
func (a *AgentMail) Received(ctx context.Context, after time.Time) ([]MailMessage, error) {
	var out []MailMessage
	token := ""
	for range maxListPages {
		q := url.Values{}
		q.Set("labels", "received")
		q.Set("after", after.UTC().Format(time.RFC3339))
		q.Set("ascending", "true")
		if token != "" {
			q.Set("page_token", token)
		}
		var page struct {
			Messages      []MailMessage `json:"messages"`
			NextPageToken string        `json:"next_page_token"`
		}
		if err := a.call(ctx, http.MethodGet, a.inboxURL("messages")+"?"+q.Encode(), nil, "", &page); err != nil {
			return out, err
		}
		out = append(out, page.Messages...)
		if page.NextPageToken == "" {
			return out, nil
		}
		token = page.NextPageToken
	}
	return out, ErrListTruncated
}

// Message gets one message in full, for its reply-stripped text.
func (a *AgentMail) Message(ctx context.Context, messageID string) (MailMessage, error) {
	var m MailMessage
	err := a.call(ctx, http.MethodGet, a.inboxURL("messages", messageID), nil, "", &m)
	return m, err
}

// Reply sends text in messageID's thread, back to its sender. The
// idempotency key makes a retried reply land once: AgentMail returns the
// first reply's result for a repeated key.
func (a *AgentMail) Reply(ctx context.Context, messageID, idempotencyKey, text string) error {
	body, _ := json.Marshal(map[string]string{"text": text})
	return a.call(ctx, http.MethodPost, a.inboxURL("messages", messageID, "reply"), body, idempotencyKey, nil)
}

func (a *AgentMail) inboxURL(parts ...string) string {
	segs := []string{a.api, "inboxes", url.PathEscape(a.inbox)}
	for _, p := range parts {
		segs = append(segs, url.PathEscape(p))
	}
	return strings.Join(segs, "/")
}

// maxRateWaits is how many 429s in a row one call waits out before it gives
// up until the next poll.
const maxRateWaits = 3

// maxRetryAfter caps one Retry-After wait.
const maxRetryAfter = 2 * time.Minute

// call sends one request and decodes a 2xx JSON answer into out (when not
// nil). A 429 is waited out per its Retry-After, up to maxRateWaits times.
func (a *AgentMail) call(ctx context.Context, method, u string, body []byte, idempotencyKey string, out any) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if err != nil {
			return errors.New("invalid AgentMail URL")
		}
		req.Header.Set("Authorization", "Bearer "+a.key)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		resp, err := a.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New(transportReason(req, err))
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateWaits {
			if err := a.Sleep(ctx, retryAfter(resp.Header.Get("Retry-After"), time.Now())); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("AgentMail %s %s returned %s", method, req.URL.Host, resp.Status)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("AgentMail %s %s: unreadable answer", method, req.URL.Host)
		}
		return nil
	}
}

// retryAfter is how long a 429 asks to wait: Retry-After in seconds or as
// an HTTP date, between one second and maxRetryAfter; one second when the
// header is missing or unreadable.
func retryAfter(h string, now time.Time) time.Duration {
	d := time.Second
	if n, err := strconv.Atoi(h); err == nil {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, time.Second), maxRetryAfter)
}
