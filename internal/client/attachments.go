package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Attachment is a file stored on the relay and carried by a request or
// reply.
type Attachment = envelope.Attachment

// AttachmentSHA256Header carries a fetched attachment's hex sha256, so the
// client can check what it received.
const AttachmentSHA256Header = "X-Tincan-SHA256"

// ErrAttachmentsUnsupported means the relay does not advertise attachments,
// either because it predates them or because it stores none. Such a relay
// would silently drop the attachments field, so the client refuses to send.
var ErrAttachmentsUnsupported = errors.New("this relay does not support attachments (upgrade the relay)")

// Capabilities is what a relay says it supports, from GET /v1/capabilities.
// A relay that predates that endpoint supports none of it.
type Capabilities struct {
	WebStatus          bool  `json:"web_status,omitempty"`
	Groups             bool  `json:"groups,omitempty"`
	Progress           bool  `json:"progress,omitempty"`
	Search             bool  `json:"search,omitempty"`
	NeedsInput         bool  `json:"needs_input,omitempty"`
	Attachments        bool  `json:"attachments"`
	MaxAttachmentBytes int64 `json:"max_attachment_bytes,omitempty"`
	MaxAttachments     int   `json:"max_attachments,omitempty"`
}

// UploadedAttachment is what the relay reports for a stored upload.
type UploadedAttachment struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	MIME   string `json:"mime"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// DownloadedAttachment describes a fetched attachment.
type DownloadedAttachment struct {
	// Verified reports whether the relay supplied a matching checksum.
	Verified bool
	ID       string
	MIME     string
	Size     int64
	SHA256   string
}

// AttachmentTimeout bounds one upload or download, well past the short API
// timeout, so a slow proxy can still move ten megabytes.
const AttachmentTimeout = 5 * time.Minute

// MaxAttachmentBytes caps a download. A var so tests can lower it.
var MaxAttachmentBytes int64 = 10 << 20

// Capabilities asks the relay what it supports. A relay that predates the
// endpoint answers 404, which reports no capabilities and no error.
func (r *Relay) Capabilities(ctx context.Context) (Capabilities, error) {
	var out Capabilities
	err := r.call(ctx, r.api, "GET", "/v1/capabilities", nil, &out)
	if IsStatus(err, http.StatusNotFound) {
		return Capabilities{}, nil
	}
	return out, err
}

// requireAttachments returns the relay's capabilities, or
// ErrAttachmentsUnsupported unless the relay advertises attachments.
func (r *Relay) requireAttachments(ctx context.Context) (Capabilities, error) {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return Capabilities{}, fmt.Errorf("check relay capabilities: %w", err)
	}
	if !caps.Attachments {
		return Capabilities{}, ErrAttachmentsUnsupported
	}
	return caps, nil
}

// UploadAttachment stores body on the relay as this agent and returns its
// id, to name in SendAttached or ReplyAttached. name is display metadata
// only. mime may be "" to let the relay detect it. size is body's length,
// or negative when unknown (the relay then reserves its full per-file cap
// against the quota while the upload runs).
func (r *Relay) UploadAttachment(ctx context.Context, name, mime string, body io.Reader, size int64) (UploadedAttachment, error) {
	if _, err := r.requireAttachments(ctx); err != nil {
		return UploadedAttachment{}, err
	}
	return r.upload(ctx, name, mime, body, size)
}

// upload is UploadAttachment without the capability check, for callers
// that already made it.
func (r *Relay) upload(ctx context.Context, name, mime string, body io.Reader, size int64) (UploadedAttachment, error) {
	resp, err := r.do(r.withTimeout(AttachmentTimeout), func(attempt int) (*http.Request, error) {
		b, err := uploadBody(body, size, attempt)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", r.Base()+"/v1/attachments?name="+url.QueryEscape(name), b)
		if err != nil {
			return nil, err
		}
		req.ContentLength = size
		if size < 0 {
			req.ContentLength = -1
		}
		if size == 0 {
			req.Body = http.NoBody
		}
		if mime != "" {
			req.Header.Set("Content-Type", mime)
		}
		return req, nil
	})
	if err != nil {
		return UploadedAttachment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return UploadedAttachment{}, apiError(resp)
	}
	var out UploadedAttachment
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return UploadedAttachment{}, fmt.Errorf("decode upload: %w", err)
	}
	return out, nil
}

// DownloadAttachment streams attachment id from the relay into w, checking
// its size against MaxAttachmentBytes and its content against the sha256
// the relay reports.
func (r *Relay) DownloadAttachment(ctx context.Context, id string, w io.Writer) (DownloadedAttachment, error) {
	resp, err := r.do(r.withTimeout(AttachmentTimeout), func(int) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, "GET", r.Base()+"/v1/attachments/"+url.PathEscape(id), nil)
	})
	if err != nil {
		return DownloadedAttachment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return DownloadedAttachment{}, apiError(resp)
	}
	if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && n > MaxAttachmentBytes {
		return DownloadedAttachment{}, fmt.Errorf("attachment %s is larger than %d bytes", id, MaxAttachmentBytes)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, MaxAttachmentBytes+1))
	if err != nil {
		return DownloadedAttachment{}, err
	}
	if n > MaxAttachmentBytes {
		return DownloadedAttachment{}, fmt.Errorf("attachment %s is larger than %d bytes", id, MaxAttachmentBytes)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if want := resp.Header.Get(AttachmentSHA256Header); want != "" && !strings.EqualFold(want, got) {
		return DownloadedAttachment{}, fmt.Errorf("attachment %s: sha256 %s does not match the relay's %s", id, got, want)
	}
	return DownloadedAttachment{ID: id, MIME: resp.Header.Get("Content-Type"), Size: n, SHA256: got, Verified: resp.Header.Get(AttachmentSHA256Header) != ""}, nil
}

// SendAttached is Send with attachments, named by the ids UploadAttachment
// returned. With none it is exactly Send. With some it first checks that the
// relay supports attachments and returns ErrAttachmentsUnsupported without
// sending if not, since an older relay would deliver the request without
// them.
func (r *Relay) SendAttached(ctx context.Context, to, body string, kind envelope.Kind, parent string, attachments []string, urgent bool) (envelope.Request, error) {
	out, err := r.sendAttached(ctx, to, body, kind, parent, attachments, urgent)
	return out.Request, err
}

func (r *Relay) sendAttached(ctx context.Context, to, body string, kind envelope.Kind, parent string, attachments []string, urgent bool) (sent, error) {
	in := map[string]any{"to": to, "body": body, "kind": kind, "parent_id": parent, "urgent": urgent}
	if err := r.attachIfAny(ctx, in, attachments); err != nil {
		return sent{}, err
	}
	return r.send(ctx, in)
}

// AskAttached is Ask with attachments; see SendAttached.
func (r *Relay) AskAttached(ctx context.Context, to, body, parent string, attachments []string, wait time.Duration, urgent bool) (Result, error) {
	s, err := r.sendAttached(ctx, to, body, envelope.KindAsk, parent, attachments, urgent)
	if err != nil {
		return Result{}, err
	}
	return r.asked(ctx, s, wait)
}

// ReplyAttached is Reply with attachments; see SendAttached.
func (r *Relay) ReplyAttached(ctx context.Context, id, body string, status envelope.Status, attachments []string) (envelope.Reply, error) {
	if status == envelope.StatusNeedsInput {
		if err := r.requireNeedsInput(ctx); err != nil {
			return envelope.Reply{}, err
		}
		if err := envelope.ValidateInput(body); err != nil {
			return envelope.Reply{}, err
		}
		if len(attachments) != 0 {
			return envelope.Reply{}, errors.New("clarifications do not accept attachments")
		}
	}
	in := map[string]any{"body": body, "status": status}
	if err := r.attachIfAny(ctx, in, attachments); err != nil {
		return envelope.Reply{}, err
	}
	var out envelope.Reply
	err := r.call(ctx, r.api, "POST", "/v1/requests/"+url.PathEscape(id)+"/reply", in, &out)
	return out, err
}

// attachIfAny adds attachments to the call body in, first checking that the
// relay supports them. With none it leaves in alone and makes no call.
func (r *Relay) attachIfAny(ctx context.Context, in map[string]any, attachments []string) error {
	if len(attachments) == 0 {
		return nil
	}
	if _, err := r.requireAttachments(ctx); err != nil {
		return err
	}
	in["attachments"] = attachmentRefs(attachments)
	return nil
}

func attachmentRefs(ids []string) []Attachment {
	out := make([]Attachment, len(ids))
	for i, id := range ids {
		out[i] = Attachment{ID: id}
	}
	return out
}

// errNoRewind is why an upload whose body cannot be read again is not
// retried after a 407.
var errNoRewind = errors.New("the upload body cannot be read again")

// uploadBody is the body for an upload's attempt (0 first). A body that
// can be read from any offset (a file, or bytes or a string in memory) is
// given to each attempt as its own reader from where it stood, so a retry
// sends it whole and never shares a reader with an attempt the transport
// may still be writing. Any other body is sent once.
func uploadBody(body io.Reader, size int64, attempt int) (io.Reader, error) {
	ra, okAt := body.(io.ReaderAt)
	sk, okSeek := body.(io.Seeker)
	if okAt && okSeek {
		start, err := sk.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		n := size
		if n < 0 {
			n = math.MaxInt64 - start
		}
		return io.NewSectionReader(ra, start, n), nil
	}
	if attempt > 0 {
		return nil, errNoRewind
	}
	return body, nil
}

// do makes a transfer (an attachment or release download, an upload) with
// client c, the request newReq builds for each attempt (0 first) naming the
// agent. A transfer the proxy refused with 407 is made once more if the
// client now uses a different proxy (see proxyAuthRetry) and otherwise ends
// with a ProxyAuthError, as a relay call does. The caller closes the
// response body.
func (r *Relay) do(c *http.Client, newReq func(attempt int) (*http.Request, error)) (*http.Response, error) {
	var resp *http.Response
	var refused error
	attempt := 0
	err := r.proxyAuthRetry(func() error {
		n := attempt
		attempt++
		req, err := newReq(n)
		if errors.Is(err, errNoRewind) && refused != nil {
			return refused // nothing sent; the first 407 stands
		}
		if err != nil {
			return err
		}
		r.headers(req)
		resp, err = c.Do(req)
		if err != nil {
			refused = err
			return err
		}
		if resp.StatusCode == http.StatusProxyAuthRequired {
			refused = apiError(resp)
			resp.Body.Close()
			resp = nil
			return refused
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// withTimeout is this client's API client (same transport and proxy) with
// timeout d.
func (r *Relay) withTimeout(d time.Duration) *http.Client {
	c := *r.api
	c.Timeout = d
	return &c
}

// apiError reads a non-2xx response into an APIError.
func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Error == "" {
		e.Error = strings.TrimSpace(string(raw))
	}
	return &APIError{Code: resp.StatusCode, Message: e.Error}
}
