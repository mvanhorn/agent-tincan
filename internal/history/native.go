package history

// Live ChatGPT and claude.ai reads go through the Tincan Chrome extension
// (extension/ in this repo) so the user's own logged-in Chrome session makes
// the requests and no cookie or token ever leaves the browser.
//
//	history service --unix socket--> native host --stdio--> extension --fetch--> site
//
// Chrome starts the native host (`tincan history native-host`) when the
// extension calls chrome.runtime.connectNative. Chrome finds the host
// through a manifest in the per-user NativeMessagingHosts directory, written
// by `tincan history install`, whose allowed_origins names only the Tincan
// extension. The host listens on a unix socket (0600, in a 0700 directory)
// for the history service and relays each validated request to the
// extension. The extension accepts only the fixed operation set below and
// runs its own fixed fetch code for each (the send operations type the
// message into a background tab the extension opens; see extension/send.js).
//
// When the extension connects it says hello with its version and the
// sha256 of its files. If the host knows the unpacked extension directory
// (TINCAN_EXTENSION_DIR, set by the wrapper `tincan history install
// --extension-dir` writes) and the files there differ, it sends the fixed
// extension.reload operation so Chrome re-reads them: an update never needs
// a manual Reload click.
//
// Extension id: extension/manifest.json carries a "key" (an RSA public key,
// base64 DER SubjectPublicKeyInfo). Chrome derives the id from it (sha256 of
// the DER bytes, first 16 bytes, hex digits 0-f mapped to a-p), so an
// unpacked load always gets DefaultExtensionID. Only the public key is in
// the repo; the private key is not needed to load unpacked. A Chrome Web
// Store listing may assign a different id; pass it with
// `tincan history install --extension-id`.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// NativeHostName is the native messaging host name the extension connects
// to.
const NativeHostName = "com.agenttincan.history"

// DefaultExtensionID is the id Chrome gives extension/ when loaded unpacked,
// derived from the "key" in extension/manifest.json.
const DefaultExtensionID = "ciejooalclcpgpapboofdbbddphldhnh"

// Message size limits. Chrome refuses messages over 1 MB from a native host;
// messages from the extension to the host may be up to 64 MiB.
const (
	MaxHostMessage   = 1 << 20
	MaxChromeMessage = 64 << 20
	// maxServiceRequest caps a request from the history service to the
	// host: room for a MaxSendMessage message even when JSON escaping
	// grows every byte sixfold.
	maxServiceRequest = 256 << 10
	// MaxListCount caps a list operation, matching the extension.
	MaxListCount = 100
	// MaxSendMessage caps a send operation's message in UTF-8 bytes,
	// matching the extension.
	MaxSendMessage = 32 << 10
)

// Send timeouts, nested so each layer hears the inner one's answer: the
// extension gives up on a send after 5 minutes, the host after
// SendHostTimeout, and the client after SendClientTimeout.
const (
	SendHostTimeout   = 5*time.Minute + 30*time.Second
	SendClientTimeout = 6 * time.Minute
)

// ErrMessageTooLarge is returned for a native message over its limit.
var ErrMessageTooLarge = errors.New("native message too large")

// WriteMessage writes v as one native message: a 4-byte little-endian
// length, then the JSON body. Nothing is written if the body exceeds limit.
func WriteMessage(w io.Writer, v any, limit int) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeRaw(w, body, limit)
}

func writeRaw(w io.Writer, body []byte, limit int) error {
	if len(body) > limit {
		return fmt.Errorf("%w: %d bytes over the %d byte limit", ErrMessageTooLarge, len(body), limit)
	}
	buf := make([]byte, 4+len(body))
	binary.LittleEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	_, err := w.Write(buf)
	return err
}

// ReadMessage reads one native message body. A length over limit is
// refused without reading the body; the stream is then unusable.
func ReadMessage(r io.Reader, limit int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if uint64(n) > uint64(limit) {
		return nil, fmt.Errorf("%w: %d bytes over the %d byte limit", ErrMessageTooLarge, n, limit)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

// Op is one fixed extension operation.
type Op string

// The complete operation set. The extension refuses anything else.
const (
	OpChatGPTList    Op = "chatgpt.list"
	OpChatGPTDetail  Op = "chatgpt.detail"
	OpChatGPTFile    Op = "chatgpt.file"
	OpClaudeAIList   Op = "claudeai.list"
	OpClaudeAIDetail Op = "claudeai.detail"
	OpClaudeAIFile   Op = "claudeai.file"
	OpChatGPTSend    Op = "chatgpt.send"
	OpClaudeAISend   Op = "claudeai.send"
	// The close operations close the tab a send left open for a
	// conversation, once its reply is finished.
	OpChatGPTClose  Op = "chatgpt.close"
	OpClaudeAIClose Op = "claudeai.close"
	// OpExtensionReload is sent only by the native host itself, never
	// relayed from the socket.
	OpExtensionReload Op = "extension.reload"
)

// OpArgs are an operation's arguments: validated ids and integers, a
// boolean, and for the send operations one capped message that the
// extension treats as text only.
type OpArgs struct {
	Count          int    `json:"count,omitempty"`
	ID             string `json:"id,omitempty"`
	FileID         string `json:"file_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Message        string `json:"message,omitempty"`
	NewChat        bool   `json:"new_chat,omitempty"`
}

// nativeIDPattern is the id shape the extension accepts: letters, digits,
// hyphen and underscore (ChatGPT's newer file ids contain underscores).
// No dots, slashes or query characters can reach a URL.
var nativeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func validNativeID(s string) bool { return nativeIDPattern.MatchString(s) }

// ValidateOp checks op against the fixed set and its arguments against
// their exact shape: required fields present, others empty.
func ValidateOp(op Op, a OpArgs) error {
	switch op {
	case OpChatGPTSend, OpClaudeAISend:
		switch {
		case a.Count != 0 || a.ID != "" || a.FileID != "":
			return fmt.Errorf("%s: unexpected argument", op)
		case strings.TrimSpace(a.Message) == "":
			return fmt.Errorf("%s: empty message", op)
		case len(a.Message) > MaxSendMessage:
			return fmt.Errorf("%s: message over %d bytes", op, MaxSendMessage)
		case !utf8.ValidString(a.Message):
			return fmt.Errorf("%s: message is not UTF-8", op)
		case a.ConversationID != "" && !validNativeID(a.ConversationID):
			return fmt.Errorf("%s: invalid conversation id", op)
		case a.ConversationID != "" && a.NewChat:
			return fmt.Errorf("%s: new chat and a conversation id together", op)
		}
		return nil
	case OpChatGPTClose, OpClaudeAIClose:
		if a != (OpArgs{ConversationID: a.ConversationID}) || !validNativeID(a.ConversationID) {
			return fmt.Errorf("%s: takes only a valid conversation id", op)
		}
		return nil
	case OpExtensionReload:
		if a != (OpArgs{}) {
			return fmt.Errorf("%s: takes no arguments", op)
		}
		return nil
	}
	if a.Message != "" || a.NewChat {
		return fmt.Errorf("%s: unexpected argument", op)
	}
	needCount, needID, needFile, allowConv := false, false, false, false
	switch op {
	case OpChatGPTList, OpClaudeAIList:
		needCount = true
	case OpChatGPTDetail, OpClaudeAIDetail:
		needID = true
	case OpChatGPTFile:
		needFile, allowConv = true, true
	case OpClaudeAIFile:
		needFile = true
	default:
		return fmt.Errorf("unknown operation %q", op)
	}
	if needCount != (a.Count != 0) || a.Count < 0 || a.Count > MaxListCount {
		return fmt.Errorf("%s: count must be between 1 and %d", op, MaxListCount)
	}
	if needID != (a.ID != "") || (needID && !validNativeID(a.ID)) {
		return fmt.Errorf("%s: invalid id", op)
	}
	if needFile != (a.FileID != "") || (needFile && !validNativeID(a.FileID)) {
		return fmt.Errorf("%s: invalid file id", op)
	}
	if a.ConversationID != "" && (!allowConv || !validNativeID(a.ConversationID)) {
		return fmt.Errorf("%s: invalid conversation id", op)
	}
	return nil
}

func (op Op) source() Source {
	if strings.HasPrefix(string(op), "claudeai.") {
		return SourceClaudeAI
	}
	return SourceChatGPT
}

func (op Op) file() bool { return op == OpChatGPTFile || op == OpClaudeAIFile }

func (op Op) send() bool { return op == OpChatGPTSend || op == OpClaudeAISend }

// NativeRequest is one request to the extension.
type NativeRequest struct {
	ID   int64  `json:"id"`
	Op   Op     `json:"op"`
	Args OpArgs `json:"args"`
}

// NativeError is an extension error: a fixed code and a short detail.
type NativeError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// NativeChunk is one piece of a file's bytes, base64.
type NativeChunk struct {
	Seq  int    `json:"seq"`
	Last bool   `json:"last"`
	Data string `json:"data"`
}

// NativeResponse is one frame from the extension. A JSON operation answers
// with one frame carrying Result; a file operation with Chunk frames, the
// last marked Last; a failure with one frame carrying Error.
type NativeResponse struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *NativeError    `json:"error,omitempty"`
	Chunk  *NativeChunk    `json:"chunk,omitempty"`
	MIME   string          `json:"mime,omitempty"`
	Size   int64           `json:"size,omitempty"`
}

// final reports whether r ends its request.
func (r NativeResponse) final() bool { return !r.OK || r.Chunk == nil || r.Chunk.Last }

// Typed failure classes of the live sources. Every error a live reader
// returns for an unavailable source is an *UnavailableError wrapping one of
// these.
var (
	ErrChromeNotRunning      = errors.New("chrome is not running")
	ErrExtensionNotConnected = errors.New("the Tincan Chrome extension is not connected")
	ErrNotLoggedIn           = errors.New("not logged in")
	ErrEndpointChanged       = errors.New("endpoint changed")
	ErrTimeout               = errors.New("timeout")
	ErrRejected              = errors.New("request rejected")
	ErrSourceFailed          = errors.New("request failed")
	// The send operations' page failures.
	ErrComposerNotFound = errors.New("message box not found")
	ErrSendFailed       = errors.New("send failed")
)

// errHostClosed means the host ended a request without a final frame.
var errHostClosed = errors.New("native host closed the connection")

// UnavailableError is a live source failure, phrased for the reply.
type UnavailableError struct {
	Source Source
	Kind   error
	Detail string
}

func siteOf(s Source) string {
	if s == SourceClaudeAI {
		return "claude.ai"
	}
	return "chatgpt.com"
}

func (e *UnavailableError) Error() string {
	site := siteOf(e.Source)
	var reason string
	switch e.Kind {
	case ErrChromeNotRunning:
		reason = "Chrome is not running"
	case ErrExtensionNotConnected:
		reason = "the Tincan Chrome extension is not connected (install it, then run tincan history install)"
	case ErrNotLoggedIn:
		reason = "not logged in to " + site + " in Chrome"
	case ErrEndpointChanged:
		reason = site + " changed its API"
	case ErrTimeout:
		reason = "Chrome did not answer in time"
	case ErrRejected:
		reason = "the extension rejected the request"
	case ErrComposerNotFound:
		reason = "no message box on the " + site + " page (the page may have changed)"
	case ErrSendFailed:
		reason = "the message could not be sent on " + site
	default:
		reason = site + " request failed"
	}
	if e.Detail != "" && e.Kind != ErrChromeNotRunning && e.Kind != ErrExtensionNotConnected && e.Kind != ErrNotLoggedIn && e.Kind != ErrTimeout {
		reason += " (" + e.Detail + ")"
	}
	return "source unavailable: " + string(e.Source) + ": " + reason
}

func (e *UnavailableError) Unwrap() error { return e.Kind }

func unavailable(s Source, kind error, detail string) error {
	return &UnavailableError{Source: s, Kind: kind, Detail: detail}
}

// fromNativeError maps an extension error code to a typed error.
func fromNativeError(s Source, ne *NativeError) error {
	detail := clip(ne.Message, 200)
	switch ne.Code {
	case "not_logged_in":
		return unavailable(s, ErrNotLoggedIn, detail)
	case "not_found":
		return fmt.Errorf("%s: %w", s, ErrNotFound)
	case "endpoint_changed":
		return unavailable(s, ErrEndpointChanged, detail)
	case "blocked":
		return unavailable(s, ErrEndpointChanged, "blocked: "+detail)
	case "bad_request":
		if detail == "unknown operation" {
			detail = "unknown operation; the loaded extension is older than this tincan, reload it once from chrome://extensions"
		}
		return unavailable(s, ErrRejected, detail)
	case "timeout":
		return unavailable(s, ErrTimeout, detail)
	case "composer_not_found":
		return unavailable(s, ErrComposerNotFound, detail)
	case "send_failed":
		return unavailable(s, ErrSendFailed, detail)
	case "unsupported":
		return unavailable(s, ErrRejected, "the extension needs an update: "+detail)
	}
	return unavailable(s, ErrSourceFailed, detail)
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return string(r)
}

// Channel carries one request to the extension and its response frames.
// recv returns true when the request is complete.
type Channel interface {
	Exchange(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error
}

// Client is the history service's side of the bridge.
type Client struct {
	Channel Channel
	// Timeout bounds one request; zero means 30s for JSON, 90s for files
	// and SendClientTimeout for sends.
	Timeout time.Duration
	// MaxJSON caps a JSON result; zero means 32 MiB.
	MaxJSON int
	// MaxFile caps a file; zero means MaxImageBytes.
	MaxFile int
}

// NewClient returns a client for the local native host socket.
func NewClient() *Client {
	return &Client{Channel: &SocketChannel{Path: DefaultSocketPath()}}
}

var requestSeq atomic.Int64

func (c *Client) timeout(op Op) time.Duration {
	switch {
	case c.Timeout > 0:
		return c.Timeout
	case op.send():
		return SendClientTimeout
	case op.file():
		return 90 * time.Second
	}
	return 30 * time.Second
}

func (c *Client) exchange(ctx context.Context, op Op, args OpArgs, recv func(NativeResponse) (bool, error)) error {
	src := op.source()
	if err := ValidateOp(op, args); err != nil {
		return unavailable(src, ErrRejected, err.Error())
	}
	if c.Channel == nil {
		return unavailable(src, ErrExtensionNotConnected, "")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout(op))
	defer cancel()
	req := NativeRequest{ID: requestSeq.Add(1), Op: op, Args: args}
	err := c.Channel.Exchange(ctx, req, recv)
	if err == nil {
		return nil
	}
	var ue *UnavailableError
	switch {
	case errors.As(err, &ue), errors.Is(err, ErrNotFound):
		return err
	case errors.Is(err, ErrChromeNotRunning):
		return unavailable(src, ErrChromeNotRunning, "")
	case errors.Is(err, ErrExtensionNotConnected), errors.Is(err, errHostClosed), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return unavailable(src, ErrExtensionNotConnected, "")
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return unavailable(src, ErrTimeout, "")
	case errors.Is(err, context.Canceled):
		return err
	}
	return unavailable(src, ErrSourceFailed, err.Error())
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Request runs a JSON operation and returns its result.
func (c *Client) Request(ctx context.Context, op Op, args OpArgs) (json.RawMessage, error) {
	if op.file() {
		return nil, unavailable(op.source(), ErrRejected, "file operation needs File")
	}
	limit := c.MaxJSON
	if limit <= 0 {
		limit = 32 << 20
	}
	var out json.RawMessage
	err := c.exchange(ctx, op, args, func(r NativeResponse) (bool, error) {
		if r.Error != nil || !r.OK {
			if r.Error == nil {
				r.Error = &NativeError{Code: "internal", Message: "failure without error"}
			}
			return true, fromNativeError(op.source(), r.Error)
		}
		if r.Chunk != nil || len(r.Result) == 0 {
			return true, unavailable(op.source(), ErrEndpointChanged, "unexpected response frame")
		}
		if len(r.Result) > limit {
			return true, unavailable(op.source(), ErrSourceFailed, fmt.Sprintf("result over %d bytes", limit))
		}
		out = r.Result
		return true, nil
	})
	return out, err
}

// SendResult is what a send operation reports: the conversation the
// message went to and when it was submitted. It carries no reply text;
// the web agent reads the reply through the detail operation.
type SendResult struct {
	ConversationID string `json:"conversation_id"`
	URL            string `json:"url"`
	// SubmittedAt is when the extension clicked send, in Unix
	// milliseconds (zero if the extension did not say).
	SubmittedAt int64 `json:"submitted_at"`
}

// Submitted returns SubmittedAt as a time, zero when unknown.
func (r SendResult) Submitted() time.Time {
	if r.SubmittedAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(r.SubmittedAt).UTC()
}

// Send types message into src's page (a new chat, or conversation convID)
// in a background tab the extension opens and returns as soon as the
// message is submitted and the conversation id is known. It does not wait
// for the reply: read it with the detail operation, then call Close.
func (c *Client) Send(ctx context.Context, src Source, message, convID string, newChat bool) (SendResult, error) {
	op := OpChatGPTSend
	if src == SourceClaudeAI {
		op = OpClaudeAISend
	}
	raw, err := c.Request(ctx, op, OpArgs{Message: message, ConversationID: convID, NewChat: newChat})
	if err != nil {
		return SendResult{}, err
	}
	var r SendResult
	if err := json.Unmarshal(raw, &r); err != nil || !validNativeID(r.ConversationID) {
		return SendResult{}, unavailable(src, ErrEndpointChanged, "unexpected send answer")
	}
	return r, nil
}

// Close closes the tab a send to src's conversation convID left open. It
// never touches a tab the extension did not open for a send.
func (c *Client) Close(ctx context.Context, src Source, convID string) error {
	op := OpChatGPTClose
	if src == SourceClaudeAI {
		op = OpClaudeAIClose
	}
	_, err := c.Request(ctx, op, OpArgs{ConversationID: convID})
	return err
}

// File runs a file operation and reassembles its chunks, enforcing the
// size cap, chunk order and the announced size.
func (c *Client) File(ctx context.Context, op Op, args OpArgs) ([]byte, string, error) {
	src := op.source()
	if !op.file() {
		return nil, "", unavailable(src, ErrRejected, "not a file operation")
	}
	limit := c.MaxFile
	if limit <= 0 {
		limit = MaxImageBytes
	}
	var buf []byte
	var mime string
	var size int64 = -1
	next := 0
	bad := func(format string, a ...any) (bool, error) {
		return true, unavailable(src, ErrSourceFailed, fmt.Sprintf(format, a...))
	}
	err := c.exchange(ctx, op, args, func(r NativeResponse) (bool, error) {
		if r.Error != nil || !r.OK {
			if r.Error == nil {
				r.Error = &NativeError{Code: "internal", Message: "failure without error"}
			}
			return true, fromNativeError(src, r.Error)
		}
		if r.Chunk == nil {
			return true, unavailable(src, ErrEndpointChanged, "file answer without chunks")
		}
		if r.Chunk.Seq != next {
			return bad("chunk %d out of order, want %d", r.Chunk.Seq, next)
		}
		next++
		if size < 0 {
			size, mime = r.Size, r.MIME
			if size <= 0 || size > int64(limit) {
				return bad("file of %d bytes is over the %d byte cap", size, limit)
			}
			buf = make([]byte, 0, size)
		}
		if base64.StdEncoding.DecodedLen(len(r.Chunk.Data)) > limit-len(buf)+3 {
			return bad("file over the %d byte cap", limit)
		}
		b, err := base64.StdEncoding.DecodeString(r.Chunk.Data)
		if err != nil {
			return bad("bad chunk encoding")
		}
		if int64(len(buf)+len(b)) > size {
			return bad("file larger than its announced %d bytes", size)
		}
		buf = append(buf, b...)
		if !r.Chunk.Last {
			return false, nil
		}
		if int64(len(buf)) != size {
			return bad("file of %d bytes, announced %d", len(buf), size)
		}
		return true, nil
	})
	if err != nil {
		return nil, "", err
	}
	return buf, mime, nil
}

// NativeDir is the directory holding the native host socket and wrapper:
// $TINCAN_HISTORY_NATIVE_DIR or ~/.config/tincan/history-native.
func NativeDir() string { return configPath("TINCAN_HISTORY_NATIVE_DIR", "history-native") }

// DefaultSocketPath is the native host's socket.
func DefaultSocketPath() string { return filepath.Join(NativeDir(), "host.sock") }

// SocketChannel reaches the native host over its unix socket, one
// connection per request.
type SocketChannel struct {
	Path string
	// ChromeRunning tells "Chrome is not running" apart from "extension not
	// connected" when no host answers; nil uses a process check.
	ChromeRunning func() bool
}

// Exchange implements Channel.
func (s *SocketChannel) Exchange(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", s.Path)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		running := s.ChromeRunning
		if running == nil {
			running = chromeRunning
		}
		if !running() {
			return ErrChromeNotRunning
		}
		return ErrExtensionNotConnected
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := WriteMessage(conn, req, maxServiceRequest); err != nil {
		return ctxErr(ctx, err)
	}
	for {
		body, err := ReadMessage(conn, MaxChromeMessage)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ctxErr(ctx, errHostClosed)
			}
			return ctxErr(ctx, err)
		}
		var r NativeResponse
		if err := json.Unmarshal(body, &r); err != nil {
			return fmt.Errorf("native host sent malformed JSON: %w", err)
		}
		if r.ID != req.ID && r.ID != 0 {
			return fmt.Errorf("native host answered request %d, want %d", r.ID, req.ID)
		}
		done, err := recv(r)
		if err != nil || done {
			return err
		}
	}
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// chromeRunning checks for a Chrome process. When it cannot tell it says
// yes, so the answer falls back to "extension not connected".
func chromeRunning() bool {
	var names []string
	switch runtime.GOOS {
	case "darwin":
		names = []string{"Google Chrome"}
	case "linux":
		names = []string{"chrome", "google-chrome", "chromium", "chromium-browser"}
	default:
		return true
	}
	for _, n := range names {
		err := exec.Command("pgrep", "-x", n).Run()
		if err == nil {
			return true
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return true
		}
	}
	return false
}

// ListenSocket listens on a unix socket at path with mode 0600, creating
// its directory with mode 0700 (and tightening an existing one). A stale
// socket at path is replaced; any other file is left alone and refused.
func ListenSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// NativeHost is the process Chrome starts for the extension. It bridges
// the history service (unix socket) and the extension (In and Out, the
// native messaging pipe).
type NativeHost struct {
	SocketPath string
	In         io.Reader
	Out        io.Writer
	// RequestTimeout bounds one relayed request; zero means 2 minutes.
	RequestTimeout time.Duration
	// SendTimeout bounds one relayed send; zero means SendHostTimeout.
	SendTimeout time.Duration
	// ExtensionDir is the unpacked extension's directory on disk. When set,
	// a hello from an unpacked extension whose files differ from it gets
	// a reload request. ReloadStatePath (default reload-state.json beside
	// the socket) remembers the last request so a reload that does not
	// help is not repeated in a loop.
	ExtensionDir    string
	ReloadStatePath string
	// Log receives one line per reload decision (discarded when nil;
	// stdout belongs to Chrome).
	Log io.Writer

	outMu   sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan []byte
	seq     int64
}

// Run serves until Chrome closes In or ctx ends, then removes the socket.
func (h *NativeHost) Run(ctx context.Context) error {
	if h.RequestTimeout <= 0 {
		h.RequestTimeout = 2 * time.Minute
	}
	if h.SendTimeout <= 0 {
		h.SendTimeout = SendHostTimeout
	}
	h.pending = map[int64]chan []byte{}
	ln, err := ListenSocket(h.SocketPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer os.Remove(h.SocketPath)
	defer ln.Close()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	readErr := make(chan error, 1)
	go func() { readErr <- h.readChrome(); cancel() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serve(ctx, conn)
		}
	}()
	<-ctx.Done()
	select {
	case err := <-readErr:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	default:
		return nil
	}
}

// readChrome routes frames from the extension to the waiting request.
func (h *NativeHost) readChrome() error {
	for {
		body, err := ReadMessage(h.In, MaxChromeMessage)
		if err != nil {
			return err
		}
		var head struct {
			ID    int64  `json:"id"`
			Hello *Hello `json:"hello"`
		}
		if json.Unmarshal(body, &head) != nil {
			continue
		}
		if head.Hello != nil {
			h.onHello(*head.Hello)
			continue
		}
		h.mu.Lock()
		ch := h.pending[head.ID]
		h.mu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- body:
		default:
			// The requester is not keeping up; drop the request.
			h.forget(head.ID)
			close(ch)
		}
	}
}

func (h *NativeHost) forget(id int64) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

func (h *NativeHost) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, err := ReadMessage(conn, maxServiceRequest)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var req NativeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		_ = WriteMessage(conn, NativeResponse{Error: &NativeError{Code: "bad_request", Message: "malformed request"}}, MaxHostMessage)
		return
	}
	if err := ValidateOp(req.Op, req.Args); err != nil || req.Op == OpExtensionReload {
		msg := "extension.reload is not relayed"
		if err != nil {
			msg = err.Error()
		}
		_ = WriteMessage(conn, NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request", Message: msg}}, MaxHostMessage)
		return
	}
	clientID := req.ID
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.seq++
	req.ID = h.seq
	h.pending[req.ID] = ch
	h.mu.Unlock()
	defer h.forget(req.ID)

	h.outMu.Lock()
	err = WriteMessage(h.Out, req, MaxHostMessage)
	h.outMu.Unlock()
	if err != nil {
		_ = WriteMessage(conn, NativeResponse{ID: clientID, Error: &NativeError{Code: "http_error", Message: "could not reach the extension"}}, MaxHostMessage)
		return
	}
	// A closed service connection ends the wait.
	gone := make(chan struct{})
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		close(gone)
	}()
	limit := h.RequestTimeout
	if req.Op.send() {
		limit = max(limit, h.SendTimeout)
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		select {
		case body, ok := <-ch:
			if !ok {
				return
			}
			var r NativeResponse
			if err := json.Unmarshal(body, &r); err != nil {
				return
			}
			r.ID = clientID
			if err := WriteMessage(conn, r, MaxChromeMessage); err != nil || r.final() {
				return
			}
		case <-timer.C:
			_ = WriteMessage(conn, NativeResponse{ID: clientID, Error: &NativeError{Code: "timeout", Message: "the extension did not answer"}}, MaxHostMessage)
			return
		case <-gone:
			return
		case <-ctx.Done():
			return
		}
	}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
