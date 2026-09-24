package history

// Web agents make ChatGPT (chatgpt.com) and Claude (claude.ai) teammates:
// a request's body is typed into the owner's logged-in site through the
// Tincan Chrome extension, and the reply comes back as the answer, with
// generated images attached. See docs/adapters/web-agents.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// DefaultWebRequestTimeout bounds one web agent request: the send, the
// wait for the reply to finish, and the reads after it.
const DefaultWebRequestTimeout = 8 * time.Minute

// DefaultWebPollInterval is how often the conversation is read while the
// reply is being written.
const DefaultWebPollInterval = 2 * time.Second

// webClockSkew is how much earlier than the extension's submitted_at the
// site may date the new message and still have it count as this send's.
const webClockSkew = 2 * time.Minute

// maxWebReplyBytes caps the reply text, leaving room in the relay's body
// limit for the footer and notes.
const maxWebReplyBytes = 64 << 10

// WebSites are the sites a web agent can front.
var WebSites = []Source{SourceChatGPT, SourceClaudeAI}

// WebAgentName is the default agent name for a site's web agent.
func WebAgentName(site Source) string {
	if site == SourceClaudeAI {
		return "claude-web"
	}
	return "chatgpt-web"
}

// ParseWebSite maps a --site value to its source.
func ParseWebSite(s string) (Source, error) {
	switch Source(s) {
	case SourceChatGPT, SourceClaudeAI:
		return Source(s), nil
	}
	return "", fmt.Errorf("unknown site %q (want chatgpt or claude-ai)", s)
}

// DefaultWebAllowlistPath is a web agent's allowlist file.
func DefaultWebAllowlistPath(agent string) string { return configPath("", agent+"-allow.txt") }

// DefaultWebStatePath is where a web agent remembers each asker's
// conversation.
func DefaultWebStatePath(agent string) string { return configPath("", agent+"-state.json") }

func siteLabel(s Source) string {
	if s == SourceClaudeAI {
		return "claude.ai"
	}
	return "ChatGPT"
}

// WebAgent is the web agent service: it polls the relay as its own
// identity, checks each request's relay-set chain against the allowlist,
// sends the body to the site through the extension, and replies with the
// assistant's answer and its images. Requests run one at a time.
type WebAgent struct {
	Relay *client.Relay
	// Site is SourceChatGPT or SourceClaudeAI.
	Site Source
	// Name is this agent's name, used in replies and logs.
	Name   string
	Native *Client
	// Allowlist returns the agents allowed to use this agent, consulted
	// for every request. An error declines the request.
	Allowlist func() ([]string, error)
	// StatePath holds each asker's last conversation id (0600).
	StatePath string
	// TempDir is where per-request image dirs are made (os.TempDir when
	// empty).
	TempDir        string
	Hold           time.Duration
	RequestTimeout time.Duration
	// PollInterval is how often the conversation is read while waiting
	// for the reply (DefaultWebPollInterval when zero).
	PollInterval time.Duration
	Log          io.Writer

	mu sync.Mutex
}

func (w *WebAgent) logf(format string, args ...any) {
	out := w.Log
	if out == nil {
		out = os.Stderr
	}
	_, _ = fmt.Fprintf(out, "tincan web %s: "+format+"\n", append([]any{w.Name}, args...)...)
}

// Run polls and handles requests until ctx is cancelled; see Service.Run.
func (w *WebAgent) Run(ctx context.Context) error { return runPolling(ctx, w.PollOnce, w.logf) }

// PollOnce waits up to Hold for requests and handles each one serially.
func (w *WebAgent) PollOnce(ctx context.Context) (int, error) {
	return pollAndHandle(ctx, w.Relay, w.Hold, w.handleSafely)
}

func (w *WebAgent) handleSafely(ctx context.Context, req envelope.Request) {
	timeout := w.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultWebRequestTimeout
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			w.logf("request %s: panic: %v", req.ID, p)
			w.reply(hctx, req, fmt.Sprintf("The %s agent hit an internal error handling this request.", w.Name), envelope.StatusFailed, nil)
		}
	}()
	w.Handle(hctx, req)
}

func (w *WebAgent) reply(ctx context.Context, req envelope.Request, body string, status envelope.Status, ids []string) {
	replyDetached(ctx, w.Relay, req, body, status, ids, w.logf)
}

// webThread is how a request picks its conversation.
type webThread int

const (
	// threadContinue continues the asker's last conversation, or starts
	// one.
	threadContinue webThread = iota
	threadNew
	threadConversation
)

type webRequest struct {
	mode    webThread
	convID  string
	message string
}

var convURLPattern = regexp.MustCompile(`^/(?:g/[A-Za-z0-9_-]+/)?(?:c|chat)/([A-Za-z0-9][A-Za-z0-9_-]{0,127})/?$`)

const threadingHelp = `Put "new chat" or "conversation: <id>" on the first line to choose the conversation, and the message after it.`

// parseWebRequest reads the optional threading line: "new chat" or
// "conversation: <id>" (an id, or a chatgpt.com or claude.ai conversation
// URL) on the first line. Everything else is the message, sent as is.
func parseWebRequest(body string) (webRequest, error) {
	first, rest, _ := strings.Cut(body, "\n")
	head := strings.ToLower(strings.TrimSpace(first))
	r := webRequest{mode: threadContinue, message: strings.TrimSpace(body)}
	switch {
	case head == "new chat" || head == "new chat:":
		r = webRequest{mode: threadNew, message: strings.TrimSpace(rest)}
	case strings.HasPrefix(head, "conversation:"):
		ref := strings.TrimSpace(strings.TrimSpace(first)[len("conversation:"):])
		id, ok := conversationRef(ref)
		if !ok {
			return webRequest{}, fmt.Errorf("%q is not a conversation id", ref)
		}
		r = webRequest{mode: threadConversation, convID: id, message: strings.TrimSpace(rest)}
	}
	if r.message == "" {
		return webRequest{}, errors.New("there is no message to send")
	}
	return r, nil
}

func conversationRef(ref string) (string, bool) {
	if validNativeID(ref) {
		return ref, true
	}
	u, err := url.Parse(ref)
	if err != nil || u.Scheme != "https" || (u.Host != "chatgpt.com" && u.Host != "claude.ai") {
		return "", false
	}
	m := convURLPattern.FindStringSubmatch(u.Path)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// webState is the state file: each asker's last conversation id. It holds
// ids only, never messages.
type webState struct {
	Conversations map[string]webMemory `json:"conversations"`
}

type webMemory struct {
	ID      string    `json:"id"`
	Updated time.Time `json:"updated"`
}

func (w *WebAgent) loadState() webState {
	st := webState{Conversations: map[string]webMemory{}}
	if w.StatePath == "" {
		return st
	}
	b, err := os.ReadFile(w.StatePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			w.logf("state %s: %v (starting fresh)", w.StatePath, err)
		}
		return st
	}
	if err := json.Unmarshal(b, &st); err != nil {
		w.logf("state %s: %v (starting fresh)", w.StatePath, err)
		st = webState{}
	}
	if st.Conversations == nil {
		st.Conversations = map[string]webMemory{}
	}
	for k, m := range st.Conversations {
		if !validNativeID(m.ID) {
			delete(st.Conversations, k)
		}
	}
	return st
}

func (w *WebAgent) saveState(st webState) {
	if w.StatePath == "" {
		return
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(w.StatePath), 0o700)
	}
	if err == nil {
		err = writeFileAtomic(w.StatePath, append(b, '\n'), 0o600)
	}
	if err != nil {
		w.logf("state %s: %v", w.StatePath, err)
	}
}

// Handle claims and answers one request.
func (w *WebAgent) Handle(ctx context.Context, req envelope.Request) {
	if _, err := w.Relay.Claim(ctx, req.ID); err != nil {
		w.logf("request %s from %s: claim failed: %v", req.ID, req.From, err)
		return
	}
	label := siteLabel(w.Site)

	// 1. Access, from relay-set fields only.
	if reason := chainDenied(w.Allowlist, req, w.Name, "send messages to "+label+" as Matt", w.logf); reason != "" {
		w.logf("request %s from %s (chain %v): declined: %s", req.ID, req.From, req.Chain, reason)
		w.reply(ctx, req, reason, envelope.StatusDeclined, nil)
		return
	}

	// 2. Threading and the message.
	wr, err := parseWebRequest(req.Body)
	if err != nil {
		w.reply(ctx, req, fmt.Sprintf("Nothing was sent to %s: %v. %s", label, err, threadingHelp), envelope.StatusFailed, nil)
		return
	}
	if len(wr.message) > MaxSendMessage {
		w.reply(ctx, req, fmt.Sprintf("Nothing was sent to %s: the message is %d bytes, over the %d byte limit.", label, len(wr.message), MaxSendMessage), envelope.StatusFailed, nil)
		return
	}

	// 3. Send, one request at a time.
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.loadState()
	convID, newChat, remembered := "", false, false
	switch wr.mode {
	case threadConversation:
		convID = wr.convID
	case threadNew:
		newChat = true
	default:
		if m, ok := st.Conversations[req.From]; ok {
			convID, remembered = m.ID, true
		}
	}
	anchor := w.anchorFor(ctx, convID, wr.message)
	res, err := w.Native.Send(ctx, w.Site, wr.message, convID, newChat)
	var note string
	if remembered && errors.Is(err, ErrNotFound) {
		note = fmt.Sprintf("Your previous %s conversation (id %s) was not found, so this went to a new chat.", label, convID)
		delete(st.Conversations, req.From)
		convID = ""
		anchor = replyAnchor{message: wr.message}
		res, err = w.Native.Send(ctx, w.Site, wr.message, "", true)
	}
	if err != nil {
		w.logf("request %s from %s: send: %v", req.ID, req.From, err)
		w.reply(ctx, req, w.sendFailure(err, convID), envelope.StatusFailed, nil)
		return
	}
	// The send left its tab open so the site can finish the reply; it is
	// closed once the reply is read or the wait gives up, on a context of
	// its own because ctx may be spent by then.
	defer w.closeTab(ctx, res.ConversationID)
	st.Conversations[req.From] = webMemory{ID: res.ConversationID, Updated: time.Now().UTC()}
	w.saveState(st)

	// 4. Wait for the reply to finish, reading the conversation through
	// the detail operation, then build the answer from that read.
	anchor.since = res.Submitted()
	raw, err := w.waitReply(ctx, res.ConversationID, anchor)
	if err != nil {
		w.logf("request %s from %s: waiting for the reply in %s: %v", req.ID, req.From, res.ConversationID, err)
		w.reply(ctx, req, w.waitFailure(err, res.ConversationID), envelope.StatusFailed, nil)
		return
	}
	text, conv := w.readReply(ctx, res.ConversationID, raw)
	body, truncated := capReply(text)
	if truncated {
		body += fmt.Sprintf("\n\n(reply truncated: showing %d of %d bytes)", len(body), len(text))
	}
	if note != "" {
		body += "\n\n" + note
	}
	body += fmt.Sprintf("\n\n%s conversation: %s", label, res.ConversationID)

	ids := w.attachImages(ctx, req, conv, &body)
	w.logf("request %s from %s: answered (conversation %s, %d attachments)", req.ID, req.From, res.ConversationID, len(ids))
	w.reply(ctx, req, body, envelope.StatusAnswered, ids)
}

func capReply(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(the reply was empty)", false
	}
	if len(s) <= maxWebReplyBytes {
		return s, false
	}
	return capBytes(s, maxWebReplyBytes), true
}

func (w *WebAgent) sendFailure(err error, convID string) string {
	label := siteLabel(w.Site)
	var ue *UnavailableError
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Sprintf("No %s conversation with id %s was found. Start a new one with \"new chat\" on the first line.", label, convID)
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("Sorry, %s did not finish answering in time. The message may still have been sent.", label)
	case errors.As(err, &ue):
		return "Sorry, " + ue.Error() + "."
	}
	return fmt.Sprintf("Sending to %s failed.", label)
}

func (w *WebAgent) live() *live {
	if w.Site == SourceClaudeAI {
		return NewClaudeAI(w.Native).live()
	}
	return NewChatGPT(w.Native).live()
}

// closeTab asks the extension to close the tab the send left open for
// convID. It runs on a fresh context so it also happens after a timeout.
func (w *WebAgent) closeTab(ctx context.Context, convID string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := w.Native.Close(cctx, w.Site, convID); err != nil {
		w.logf("conversation %s: closing the tab: %v", convID, err)
	}
}

func (w *WebAgent) waitFailure(err error, convID string) string {
	label := siteLabel(w.Site)
	var ue *UnavailableError
	if errors.As(err, &ue) && !errors.Is(err, ErrTimeout) {
		return fmt.Sprintf("Sorry, %s. The message was sent to %s (conversation %s), but the reply could not be read.", ue.Error(), label, convID)
	}
	return fmt.Sprintf("Sorry, %s did not finish answering in time. The message was sent; the conversation is %s.", label, convID)
}

// replyAnchor identifies the message this request sent, so a finished
// reply to an earlier message is never taken for this one's.
type replyAnchor struct {
	// prevUser is the id of the conversation's last user message before
	// the send ("" for a new chat, or when it could not be read).
	prevUser string
	// since is when the extension clicked send (zero if unknown).
	since time.Time
	// message is the text sent. needText makes a match on it required,
	// for a continued conversation whose previous state is unknown.
	message  string
	needText bool
}

// anchorFor reads the conversation's last user message before a send into
// an existing conversation.
func (w *WebAgent) anchorFor(ctx context.Context, convID, message string) replyAnchor {
	a := replyAnchor{message: message}
	if convID == "" {
		return a
	}
	raw, err := w.Native.Request(ctx, w.live().detailOp, OpArgs{ID: convID})
	if err == nil {
		var p replyProgress
		if p, err = w.progress(raw, replyAnchor{}); err == nil {
			a.prevUser = p.userID
			return a
		}
	}
	if !errors.Is(err, ErrNotFound) {
		w.logf("conversation %s: reading it before the send: %v", convID, err)
	}
	a.needText = true
	return a
}

// accepts reports whether a user message is the one this request sent.
func (a replyAnchor) accepts(id, text string, at time.Time) bool {
	if a.prevUser != "" && id == a.prevUser {
		return false
	}
	if !a.since.IsZero() && !at.IsZero() && at.Before(a.since.Add(-webClockSkew)) {
		return false
	}
	return !a.needText || sameMessage(text, a.message)
}

// sameMessage compares a sent message with what the site stored, ignoring
// whitespace and the markdown marks editors rewrite.
func sameMessage(a, b string) bool {
	norm := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) || strings.ContainsRune("#*`>_-", r) {
				return -1
			}
			return r
		}, s)
	}
	return norm(a) == norm(b)
}

// replyProgress is what one detail read says about the reply.
type replyProgress struct {
	// userID is the id of the last user message on the current branch.
	userID string
	// found: an assistant reply to this request's message is there.
	found bool
	// finished: the site marks that reply finished.
	finished bool
	// sig fingerprints the reply, for the stability check.
	sig string
}

func (w *WebAgent) progress(raw json.RawMessage, a replyAnchor) (replyProgress, error) {
	if w.Site == SourceClaudeAI {
		return claudeProgress(raw, a)
	}
	return chatgptProgress(raw, a)
}

// chatgptProgress: the reply is the last message on the current_node
// branch, an assistant message to everyone after this request's user
// message. It is finished when its status is finished_successfully,
// finish_details is present, or end_turn is true, and end_turn is not
// false.
func chatgptProgress(raw json.RawMessage, a replyAnchor) (replyProgress, error) {
	var d cgDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return replyProgress{}, err
	}
	if d.Mapping == nil {
		return replyProgress{}, errors.New("no mapping")
	}
	var path []*cgMessage
	for _, m := range d.path() {
		if !m.Metadata.Hidden {
			path = append(path, m)
		}
	}
	var p replyProgress
	u := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Author.Role == "user" {
			u = i
			break
		}
	}
	if u < 0 {
		return p, nil
	}
	um := path[u]
	p.userID = um.ID
	text, _ := um.parts()
	if !a.accepts(um.ID, text, um.CreateTime.Time) || u == len(path)-1 {
		return p, nil
	}
	leaf := path[len(path)-1]
	if leaf.Author.Role != "assistant" || (leaf.Recipient != "" && leaf.Recipient != "all") {
		return p, nil
	}
	// Reasoning and tool-call messages are not the answer.
	if ct := leaf.Content.ContentType; ct != "text" && ct != "multimodal_text" {
		return p, nil
	}
	reply, pointers := leaf.parts()
	p.found = strings.TrimSpace(reply) != "" || len(pointers) > 0
	p.sig = fmt.Sprintf("%s\n%d\n%s", leaf.ID, len(pointers), reply)
	details := len(leaf.Metadata.FinishDetails) > 0 && string(leaf.Metadata.FinishDetails) != "null"
	endTurn := leaf.EndTurn != nil && *leaf.EndTurn
	notEnd := leaf.EndTurn != nil && !*leaf.EndTurn
	p.finished = p.found && !notEnd && (leaf.Status == "finished_successfully" || details || endTurn)
	return p, nil
}

// claudeProgress: the reply is the last message on the current branch,
// from the assistant, after this request's human message. It is finished
// when it carries a stop_reason; otherwise waitReply waits for its text
// to stay the same across two reads.
func claudeProgress(raw json.RawMessage, a replyAnchor) (replyProgress, error) {
	var c caConversation
	if err := json.Unmarshal(raw, &c); err != nil {
		return replyProgress{}, err
	}
	path := c.path()
	var p replyProgress
	u := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Sender == "human" {
			u = i
			break
		}
	}
	if u < 0 {
		return p, nil
	}
	hm := path[u]
	p.userID = hm.UUID
	if !a.accepts(hm.UUID, hm.text(), hm.CreatedAt.Time) || u == len(path)-1 {
		return p, nil
	}
	leaf := path[len(path)-1]
	if leaf.Sender != "assistant" {
		return p, nil
	}
	reply := leaf.text()
	p.found = strings.TrimSpace(reply) != "" || len(leaf.images()) > 0
	p.sig = fmt.Sprintf("%s\n%d\n%s", leaf.UUID, len(leaf.images()), reply)
	p.finished = p.found && strings.TrimSpace(leaf.StopReason) != ""
	return p, nil
}

// waitReply reads the conversation every PollInterval until the reply to
// this request's message is finished, and returns that read. claude.ai
// replies without a stop_reason count as finished when the same reply is
// read twice in a row. Errors that will not clear (logged out, the API
// changed, the extension gone) end the wait at once; others are retried
// until ctx ends.
func (w *WebAgent) waitReply(ctx context.Context, convID string, a replyAnchor) (json.RawMessage, error) {
	every := w.PollInterval
	if every <= 0 {
		every = DefaultWebPollInterval
	}
	op := w.live().detailOp
	prev := ""
	for {
		raw, err := w.Native.Request(ctx, op, OpArgs{ID: convID})
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		switch {
		case err == nil:
			p, perr := w.progress(raw, a)
			if perr != nil {
				return nil, unavailable(w.Site, ErrEndpointChanged, "unexpected conversation shape")
			}
			if p.finished || (w.Site == SourceClaudeAI && p.found && p.sig == prev) {
				return raw, nil
			}
			prev = ""
			if p.found {
				prev = p.sig
			}
		case errors.Is(err, ErrNotLoggedIn), errors.Is(err, ErrEndpointChanged), errors.Is(err, ErrExtensionNotConnected), errors.Is(err, ErrChromeNotRunning), errors.Is(err, ErrRejected):
			return nil, err
		default:
			w.logf("conversation %s: detail: %v (retrying)", convID, err)
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// readReply builds the answer from the finished read: the last turn's
// reply text, with its generated images fetched through the file
// operation.
func (w *WebAgent) readReply(ctx context.Context, convID string, raw json.RawMessage) (text string, conv []Conversation) {
	l := w.live()
	th, err := l.parseDetail(convID, raw)
	if err != nil || len(th.turns) == 0 {
		return "", nil
	}
	t := th.turns[len(th.turns)-1]
	if len(t.replyImages) == 0 {
		return t.reply.Text, nil
	}
	c := []Conversation{{Source: w.Site, ID: convID, Messages: []Message{{Role: RoleAssistant, Images: t.replyImages}}}}
	c = l.resolve(ctx, c)
	capImages(c)
	return t.reply.Text, c
}

// attachImages saves and uploads the reply's images and appends a line to
// body saying what was actually attached.
func (w *WebAgent) attachImages(ctx context.Context, req envelope.Request, conv []Conversation, body *string) []string {
	n := 0
	for _, c := range conv {
		for _, m := range c.Messages {
			n += len(m.Images)
		}
	}
	if n == 0 {
		return nil
	}
	base := w.TempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "tincan-web-req-")
	if err != nil {
		w.logf("request %s: image dir: %v", req.ID, err)
		*body += "\nThe images could not be attached."
		return nil
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := SaveImages(dir, conv); err != nil {
		w.logf("request %s: save images: %v", req.ID, err)
		*body += "\nThe images could not be attached."
		return nil
	}
	ups, err := w.Relay.UploadFiles(ctx, imagePaths(conv))
	switch {
	case errors.Is(err, client.ErrAttachmentsUnsupported):
		*body += "\nThe images could not be attached: this relay does not support attachments."
		return nil
	case err != nil:
		w.logf("request %s: upload images: %v", req.ID, err)
		*body += "\nThe images could not be attached: the upload to the relay failed."
		return nil
	}
	ids := client.AttachmentIDs(ups)
	*body += "\n" + imagesLine(len(ids))
	return ids
}
