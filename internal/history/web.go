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

// DefaultClaudeStablePolls and DefaultClaudeStableFor: a claude.ai reply
// with no stop_reason is finished once the same text is read on 4
// consecutive polls spanning at least 10 seconds, so a pause mid-reply is
// not taken for the end.
const (
	DefaultClaudeStablePolls = 4
	DefaultClaudeStableFor   = 10 * time.Second
)

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

// DefaultWebJournalPath is where a web agent journals the requests it
// has sent.
func DefaultWebJournalPath(agent string) string { return configPath("", agent+"-journal.json") }

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
	// JournalPath is the send journal (0600): each request this agent
	// has sent, so a requeued request is never sent twice. Empty turns
	// the journal off.
	JournalPath string
	// JournalRetention is how long a journal entry is kept
	// (DefaultWebJournalRetention when zero).
	JournalRetention time.Duration
	// TempDir is where per-request image dirs are made (os.TempDir when
	// empty).
	TempDir        string
	Hold           time.Duration
	RequestTimeout time.Duration
	// PollInterval is how often the conversation is read while waiting
	// for the reply (DefaultWebPollInterval when zero).
	PollInterval time.Duration
	// ClaudeStablePolls and ClaudeStableFor say when a claude.ai reply
	// with no stop_reason counts as finished: the same text on this many
	// consecutive polls, spanning at least this long
	// (DefaultClaudeStablePolls and DefaultClaudeStableFor when zero).
	ClaudeStablePolls int
	ClaudeStableFor   time.Duration
	Log               io.Writer

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

	// 3. Send, one request at a time. A request already in the journal
	// was sent before (the relay requeued it after a lost reply or a
	// crash): it is not sent again, only its reply is read.
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.loadJournal().Requests[req.ID]; ok {
		w.resume(ctx, req, wr, e)
		return
	}
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
	anchor.since = res.Submitted()
	entry := webSend{ConversationID: res.ConversationID, PrevUserID: anchor.prevUser, SubmittedAt: anchor.since, Recorded: time.Now().UTC(), State: webSendSent}
	w.journal(req.ID, entry)
	st.Conversations[req.From] = webMemory{ID: res.ConversationID, Updated: time.Now().UTC()}
	w.saveState(st)
	w.answer(ctx, req, anchor, entry, note)
}

// resume answers a request the journal says was already sent: it waits
// for (or re-reads) the reply in the journaled conversation and never
// sends the message again.
func (w *WebAgent) resume(ctx context.Context, req envelope.Request, wr webRequest, e webSend) {
	w.logf("request %s from %s: already sent to conversation %s (%s); reading the reply instead of sending again", req.ID, req.From, e.ConversationID, e.State)
	defer w.closeTab(ctx, e.ConversationID)
	anchor := replyAnchor{prevUser: e.PrevUserID, since: e.SubmittedAt, message: wr.message, bound: e.UserMessageID}
	w.answer(ctx, req, anchor, e, "")
}

// answer waits for the reply to this request's message in e's
// conversation, reading it through the detail operation, then builds and
// sends the answer from that read.
func (w *WebAgent) answer(ctx context.Context, req envelope.Request, anchor replyAnchor, e webSend, note string) {
	label := siteLabel(w.Site)
	convID := e.ConversationID
	raw, userID, err := w.waitReply(ctx, convID, anchor, func(id string) {
		e.UserMessageID = id
		w.journal(req.ID, e)
	})
	if err != nil {
		w.logf("request %s from %s: waiting for the reply in %s: %v", req.ID, req.From, convID, err)
		w.reply(ctx, req, w.waitFailure(err, convID), envelope.StatusFailed, nil)
		return
	}
	text, conv, err := w.readReply(ctx, convID, userID, raw)
	if err != nil {
		w.logf("request %s from %s: reading the reply in %s: %v", req.ID, req.From, convID, err)
		w.reply(ctx, req, w.waitFailure(err, convID), envelope.StatusFailed, nil)
		return
	}
	body, truncated := capReply(text)
	if truncated {
		body += fmt.Sprintf("\n\n(reply truncated: showing %d of %d bytes)", len(body), len(text))
	}
	if note != "" {
		body += "\n\n" + note
	}
	body += fmt.Sprintf("\n\n%s conversation: %s", label, convID)

	ids := w.attachImages(ctx, req, conv, &body)
	e.UserMessageID, e.State = userID, webSendAnswered
	w.journal(req.ID, e)
	w.logf("request %s from %s: answered (conversation %s, %d attachments)", req.ID, req.From, convID, len(ids))
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
	switch {
	case errors.Is(err, errOrphaned):
		return fmt.Sprintf("Sorry, another message was sent in the %s conversation %s before this one was answered, so there is no reply to return.", label, convID)
	case errors.Is(err, errReplyUnreadable):
		return fmt.Sprintf("Sorry, the message was sent to %s (conversation %s) and it answered, but the reply could not be read.", label, convID)
	}
	if errors.As(err, &ue) && !errors.Is(err, ErrTimeout) {
		return fmt.Sprintf("Sorry, %s. The message was sent to %s (conversation %s), but the reply could not be read.", ue.Error(), label, convID)
	}
	return fmt.Sprintf("Sorry, %s did not finish answering in time. The message was sent; the conversation is %s.", label, convID)
}

// replyAnchor identifies the message this request sent, so that neither a
// finished reply to an earlier message nor the reply to a later one (the
// owner typing in the same conversation meanwhile) is taken for this
// one's.
type replyAnchor struct {
	// prevUser is the id of the conversation's last user message before
	// the send ("" for a new chat, or when it could not be read).
	prevUser string
	// since is when the extension clicked send (zero if unknown).
	since time.Time
	// message is the text sent. This request's message is the first user
	// message after prevUser whose text matches it.
	message string
	// bound is the id of this request's user message once it has been
	// seen; from then on only a reply to that message counts.
	bound string
}

// anchorFor reads the conversation's last user message before a send into
// an existing conversation. When that read fails, prevUser stays empty and
// the message is found by its text and time alone.
func (w *WebAgent) anchorFor(ctx context.Context, convID, message string) replyAnchor {
	a := replyAnchor{message: message}
	if convID == "" {
		return a
	}
	raw, err := w.Native.Request(ctx, w.live().detailOp, OpArgs{ID: convID})
	if err == nil {
		var nodes []webNode
		if nodes, err = w.nodes(raw); err == nil {
			for i := len(nodes) - 1; i >= 0; i-- {
				if nodes[i].user {
					a.prevUser = nodes[i].id
					break
				}
			}
			return a
		}
	}
	if !errors.Is(err, ErrNotFound) {
		w.logf("conversation %s: reading it before the send: %v", convID, err)
	}
	return a
}

// matches reports whether user message n can be the one this request
// sent: its text is the sent text (compared the way the extension checks
// the composer) and it is not dated before the send, allowing for clock
// skew between Chrome and the site.
func (a replyAnchor) matches(n webNode) bool {
	if a.prevUser != "" && n.id == a.prevUser {
		return false
	}
	if !a.since.IsZero() && !n.at.IsZero() && n.at.Before(a.since.Add(-webClockSkew)) {
		return false
	}
	return sameMessage(n.text, a.message)
}

// sameMessage compares a sent message with what the site stored, ignoring
// whitespace and the markdown marks editors rewrite. It is the same
// normalization extension/send.js uses to verify the composer.
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

// webNode is one message on a conversation's current branch, reduced to
// what the reply wait needs.
type webNode struct {
	id string
	// user: a prompt turn (a user or human message with content).
	user bool
	// reply: an assistant message that can be the answer (text, to
	// everyone). Other assistant or tool messages are neither.
	reply  bool
	text   string
	at     time.Time
	images int
	// finished: the site marks this message finished.
	finished bool
}

// replyProgress is what one detail read says about the reply.
type replyProgress struct {
	// userID is the id of this request's user message, once found.
	userID string
	// found: an assistant reply to this request's message is there.
	found bool
	// finished: the site marks that reply finished.
	finished bool
	// orphaned: a later user turn follows this request's message with
	// nothing between them, so no reply to it will come.
	orphaned bool
	// sig fingerprints the reply, for the stability check.
	sig string
}

func (w *WebAgent) nodes(raw json.RawMessage) ([]webNode, error) {
	if w.Site == SourceClaudeAI {
		return claudeNodes(raw)
	}
	return chatgptNodes(raw)
}

func (w *WebAgent) progress(raw json.RawMessage, a replyAnchor) (replyProgress, error) {
	nodes, err := w.nodes(raw)
	if err != nil {
		return replyProgress{}, err
	}
	return progressOf(nodes, a), nil
}

// progressOf finds this request's user message on the branch (a.bound, or
// the first message after a.prevUser that a.matches) and its reply: the
// last message after it and before the next user turn, when that is an
// assistant answer.
func progressOf(nodes []webNode, a replyAnchor) replyProgress {
	var p replyProgress
	b := -1
	if a.bound != "" {
		for i, n := range nodes {
			if n.user && n.id == a.bound {
				b = i
				break
			}
		}
	} else {
		start := 0
		for i, n := range nodes {
			if a.prevUser != "" && n.id == a.prevUser {
				start = i + 1
			}
		}
		for i := start; i < len(nodes); i++ {
			if nodes[i].user && a.matches(nodes[i]) {
				b = i
				break
			}
		}
	}
	if b < 0 {
		return p
	}
	p.userID = nodes[b].id
	end, later := len(nodes), false
	for j := b + 1; j < len(nodes); j++ {
		if nodes[j].user {
			end, later = j, true
			break
		}
	}
	if end == b+1 {
		p.orphaned = later
		return p
	}
	leaf := nodes[end-1]
	if !leaf.reply {
		return p
	}
	p.found = strings.TrimSpace(leaf.text) != "" || leaf.images > 0
	p.sig = fmt.Sprintf("%s\n%d\n%s", leaf.id, leaf.images, leaf.text)
	p.finished = p.found && leaf.finished
	return p
}

// chatgptNodes reads the current_node branch. A user turn is a user
// message with text or images (the ones parseChatGPTDetail makes turns
// of); an answer is an assistant text message to everyone, finished when
// its status is finished_successfully, finish_details is present, or
// end_turn is true, and end_turn is not false.
func chatgptNodes(raw json.RawMessage) ([]webNode, error) {
	var d cgDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if d.Mapping == nil {
		return nil, errors.New("no mapping")
	}
	var out []webNode
	for _, m := range d.path() {
		if m.Metadata.Hidden {
			continue
		}
		text, pointers := m.parts()
		textual := m.Content.ContentType == "text" || m.Content.ContentType == "multimodal_text"
		n := webNode{id: m.ID, text: text, at: m.CreateTime.Time, images: len(pointers)}
		switch m.Author.Role {
		case "user":
			images := len(pointers)
			for _, at := range m.Metadata.Attachments {
				if strings.HasPrefix(at.MimeType, "image/") && validNativeID(at.ID) {
					images++
				}
			}
			if !textual || (text == "" && images == 0) {
				continue
			}
			n.user = true
		case "assistant":
			// Reasoning and tool-call messages are not the answer.
			n.reply = textual && (m.Recipient == "" || m.Recipient == "all")
			details := len(m.Metadata.FinishDetails) > 0 && string(m.Metadata.FinishDetails) != "null"
			endTurn := m.EndTurn != nil && *m.EndTurn
			notEnd := m.EndTurn != nil && !*m.EndTurn
			n.finished = !notEnd && (m.Status == "finished_successfully" || details || endTurn)
		}
		out = append(out, n)
	}
	return out, nil
}

// claudeNodes reads the current branch. Every assistant message can be the
// answer; it is finished when it carries a stop_reason, the only explicit
// completion field claude.ai's detail has. Without one, waitReply waits for
// its text to settle.
func claudeNodes(raw json.RawMessage) ([]webNode, error) {
	var c caConversation
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	var out []webNode
	for _, m := range c.path() {
		n := webNode{id: m.UUID, text: m.text(), at: m.CreatedAt.Time, images: len(m.images())}
		switch m.Sender {
		case "human":
			n.user = true
		case "assistant":
			n.reply = true
			n.finished = strings.TrimSpace(m.StopReason) != ""
		}
		out = append(out, n)
	}
	return out, nil
}

// errOrphaned: another message was sent after this request's with no reply
// in between.
var errOrphaned = errors.New("another message was sent in the conversation before this one was answered")

// waitReply reads the conversation every PollInterval until the reply to
// this request's message is finished, and returns that read and the id of
// this request's user message. A claude.ai reply without a stop_reason
// counts as finished once the same reply is read on ClaudeStablePolls
// consecutive polls spanning at least ClaudeStableFor. onBind, when set,
// is called once with the user message id when it is first seen. Errors
// that will not clear (logged out, the API changed, the extension gone)
// end the wait at once, as does a later user turn with no reply to this
// one; others are retried until ctx ends.
func (w *WebAgent) waitReply(ctx context.Context, convID string, a replyAnchor, onBind func(string)) (json.RawMessage, string, error) {
	every := w.PollInterval
	if every <= 0 {
		every = DefaultWebPollInterval
	}
	stablePolls := w.ClaudeStablePolls
	if stablePolls <= 0 {
		stablePolls = DefaultClaudeStablePolls
	}
	stableFor := w.ClaudeStableFor
	if stableFor <= 0 {
		stableFor = DefaultClaudeStableFor
	}
	op := w.live().detailOp
	prev, seen := "", 0
	var first time.Time
	for {
		raw, err := w.Native.Request(ctx, op, OpArgs{ID: convID})
		if cerr := ctx.Err(); cerr != nil {
			return nil, a.bound, cerr
		}
		switch {
		case err == nil:
			p, perr := w.progress(raw, a)
			if perr != nil {
				return nil, a.bound, unavailable(w.Site, ErrEndpointChanged, "unexpected conversation shape")
			}
			if p.userID != "" && a.bound == "" {
				a.bound = p.userID
				if onBind != nil {
					onBind(a.bound)
				}
			}
			if p.orphaned {
				return nil, a.bound, errOrphaned
			}
			if p.finished {
				return raw, a.bound, nil
			}
			switch {
			case !p.found || w.Site != SourceClaudeAI:
				prev, seen = "", 0
			case p.sig == prev:
				seen++
				if seen >= stablePolls && time.Since(first) >= stableFor {
					return raw, a.bound, nil
				}
			default:
				prev, seen, first = p.sig, 1, time.Now()
			}
		case errors.Is(err, ErrNotLoggedIn), errors.Is(err, ErrEndpointChanged), errors.Is(err, ErrExtensionNotConnected), errors.Is(err, ErrChromeNotRunning), errors.Is(err, ErrRejected):
			return nil, a.bound, err
		default:
			w.logf("conversation %s: detail: %v (retrying)", convID, err)
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, a.bound, ctx.Err()
		case <-t.C:
		}
	}
}

// errReplyUnreadable: the finished read could not be turned into a reply.
var errReplyUnreadable = errors.New("the reply could not be read")

// readReply builds the answer from the finished read: the reply to this
// request's user message (userID), with its generated images fetched
// through the file operation.
func (w *WebAgent) readReply(ctx context.Context, convID, userID string, raw json.RawMessage) (string, []Conversation, error) {
	l := w.live()
	th, err := l.parseDetail(convID, raw)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", errReplyUnreadable, err)
	}
	i := -1
	for j, t := range th.turns {
		if t.promptID == userID {
			i = j
		}
	}
	if i < 0 {
		return "", nil, fmt.Errorf("%w: this request's turn is not in the conversation", errReplyUnreadable)
	}
	t := th.turns[i]
	if len(t.replyImages) == 0 {
		return t.reply.Text, nil, nil
	}
	c := []Conversation{{Source: w.Site, ID: convID, Messages: []Message{{Role: RoleAssistant, Images: t.replyImages}}}}
	c = l.resolve(ctx, c)
	capImages(c)
	return t.reply.Text, c, nil
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
