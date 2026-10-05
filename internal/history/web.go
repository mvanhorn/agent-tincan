package history

// Web agents make ChatGPT (chatgpt.com), Claude (claude.ai), Grok
// (grok.com), Gemini (gemini.google.com), Perplexity (www.perplexity.ai)
// and Copilot (copilot.com) teammates: a request's body is typed into the
// owner's logged-in site through the Tincan Chrome extension, and the
// reply comes back as the answer, with generated images attached from
// ChatGPT, Grok and Gemini and, on a site whose answers cite the web, its
// source links. See docs/adapters/web-agents.md.

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

// webClockSkew is how much earlier than the extension's submitted_at the
// site may date the new message and still have it count as this send's.
const webClockSkew = 2 * time.Minute

// maxWebReplyBytes caps a web agent's reply: the answer text with its
// truncation notice, sources list, notes and conversation footer. Image
// attachment lines are added after it, inside the relay's body limit.
const maxWebReplyBytes = 64 << 10

// WebSites are the sites a web agent can front, in the site table's order.
var WebSites = siteSources()

// WebAgentName is the default agent name for a site's web agent ("" for a
// site outside the table).
func WebAgentName(site Source) string {
	if s := siteFor(site); s != nil {
		return s.agent
	}
	return ""
}

// ParseWebSite maps a --site value to its source.
func ParseWebSite(s string) (Source, error) {
	site, err := lookupSite(Source(s))
	if err != nil {
		return "", err
	}
	return site.source, nil
}

// DefaultWebAllowlistPath is a web agent's allowlist file.
func DefaultWebAllowlistPath(agent string) string { return configPath("", agent+"-allow.txt") }

// DefaultWebJournalPath is where a web agent journals the requests it
// has sent.
func DefaultWebJournalPath(agent string) string { return configPath("", agent+"-journal.json") }

// DefaultWebStatePath is where a web agent remembers each asker's
// conversation.
func DefaultWebStatePath(agent string) string { return configPath("", agent+"-state.json") }

// siteLabel names a site in replies and logs (the source name for a site
// outside the table).
func siteLabel(s Source) string {
	if site := siteFor(s); site != nil {
		return site.label
	}
	return string(s)
}

// WebAgent is the web agent service: it polls the relay as its own
// identity, checks each request's relay-set chain against the allowlist,
// sends the body to the site through the extension, and replies with the
// assistant's answer and its images. Requests run one at a time.
type WebAgent struct {
	Relay *client.Relay
	// Site is one of WebSites; requests to an agent with any other site
	// fail.
	Site Source
	// Name is this agent's name, used in replies and logs.
	Name   string
	Native *Client
	// Allowlist returns the agents allowed to use this agent, consulted
	// for every request. An error declines the request.
	Allowlist func() ([]string, error)
	// StatePath holds each asker's last conversation id (0600).
	StatePath string
	// Thread is the one conversation of a site that has one (a dot's DM
	// thread, from --thread). Every request goes there.
	Thread string
	// JournalPath is the send journal (0600): each request this agent
	// has sent, so a requeued request is never sent twice. Empty turns
	// the journal off.
	JournalPath string
	// UsedPath is the used list (0600): every conversation this agent
	// sends into, so history leaves them out of the owner's own
	// conversations. Empty turns it off.
	UsedPath string
	// JournalRetention is how long a journal entry is kept
	// (DefaultWebJournalRetention when zero).
	JournalRetention time.Duration
	// TempDir is where per-request image dirs are made (os.TempDir when
	// empty).
	TempDir        string
	Hold           time.Duration
	RequestTimeout time.Duration
	// PollInterval, when set, reads the conversation at this fixed
	// interval while waiting for the reply instead of on
	// DefaultWebPollSchedule (tests use it).
	PollInterval time.Duration
	// ClaudeStablePolls and ClaudeStableFor override the site's
	// text-stability rule (claude.ai's DefaultClaudeStablePolls and
	// DefaultClaudeStableFor, Gemini's DefaultGeminiStablePolls and
	// DefaultGeminiStableFor, Copilot's DefaultCopilotStablePolls and
	// DefaultCopilotStableFor; ChatGPT and Grok have none): a reply the site does not
	// mark finished counts as finished once the same text is read on this
	// many consecutive polls, spanning at least this long. Zero keeps the
	// site's rule.
	ClaudeStablePolls int
	ClaudeStableFor   time.Duration
	// PresenceInterval is how often the agent refreshes its relay presence
	// while it handles a request (client.DefaultPresenceInterval when
	// zero).
	PresenceInterval time.Duration
	Log              io.Writer

	// OutPath, on a one-thread site (dots), turns on the outbound
	// watcher: the dot's "@tincan ask <agent>" messages become asks from
	// this agent, and their answers are typed back into the DM. It is the
	// watcher's 0600 state file (DefaultDotOutPath). Empty turns it off.
	OutPath string
	// SendAllowlist returns the agents the dot may ask, consulted for
	// every outbound ask; an error refuses them all. Nil allows any
	// joined agent.
	SendAllowlist func() ([]string, error)
	// SendAllowlistPath names the send allowlist file in replies.
	SendAllowlistPath string
	// Teach types the setup message into the DM once more, even when the
	// thread was taught before (--teach).
	Teach bool
	// WatchInterval is how often the watcher reads the DM when idle
	// (DefaultDotWatchInterval when zero).
	WatchInterval time.Duration

	// clock paces the reply wait and the watcher (the real clock when
	// nil).
	clock webClock

	// mu is the one send path: it is held for the whole of an inbound
	// request (send, reply wait and tab close) and for each watcher tick,
	// so inbound and outbound never type into the site at once and an
	// outbound reply is never typed while an inbound request waits.
	mu             sync.Mutex
	sendGeneration uint64
	// watch is the outbound watcher's state; only the watcher uses it.
	watch dotWatch
	auth  webAuthState
}

func (w *WebAgent) logf(format string, args ...any) {
	out := w.Log
	if out == nil {
		out = os.Stderr
	}
	_, _ = fmt.Fprintf(out, "tincan web %s: "+format+"\n", append([]any{w.Name}, args...)...)
}

// Run polls and handles requests until ctx is cancelled; see Service.Run.
// On a dot with OutPath set, the outbound watcher runs beside it.
func (w *WebAgent) Run(ctx context.Context) error {
	actx, stopAuth := context.WithCancel(ctx)
	authDone := make(chan struct{})
	go func() { defer close(authDone); w.runWebStatus(actx) }()
	defer func() { stopAuth(); <-authDone }()
	if w.watching() {
		wctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			w.runDotWatcher(wctx)
		}()
		defer func() {
			cancel()
			<-done
		}()
	}
	return RunPolling(ctx, w.PollOnce, w.logf)
}

// PollOnce waits up to Hold for requests and handles each one serially.
func (w *WebAgent) PollOnce(ctx context.Context) (int, error) {
	return PollAndHandle(ctx, w.Relay, w.Hold, "web-serve", w.handleSafely)
}

func (w *WebAgent) handleSafely(ctx context.Context, req envelope.Request) {
	timeout := w.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultWebRequestTimeout
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	defer w.keepPresence(hctx)()
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
// "conversation: <id>" (an id, or a conversation URL on a site in the
// table) on the first line. Everything else is the message, sent as is.
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

// dropNewChatLine returns body trimmed, less a first line that is exactly
// "new chat" or "new chat:" (any case, surrounding space trimmed). Any
// other first line, a "conversation:" one included, is kept.
func dropNewChatLine(body string) string {
	first, rest, _ := strings.Cut(body, "\n")
	if head := strings.ToLower(strings.TrimSpace(first)); head == "new chat" || head == "new chat:" {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(body)
}

// conversationRef reads a conversation id, or a conversation URL on any
// site in the table.
func conversationRef(ref string) (string, bool) {
	if validNativeID(ref) {
		return ref, true
	}
	u, err := url.Parse(ref)
	if err != nil || u.Scheme != "https" {
		return "", false
	}
	for _, s := range webSites {
		if u.Host != s.host {
			continue
		}
		if m := s.convPath.FindStringSubmatch(u.Path); m != nil {
			return m[1], true
		}
	}
	return "", false
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
	site, err := lookupSite(w.Site)
	if err != nil {
		w.logf("request %s from %s: %v", req.ID, req.From, err)
		w.reply(ctx, req, fmt.Sprintf("The %s agent is not set up for a known site: %v.", w.Name, err), envelope.StatusFailed, nil)
		return
	}
	label := site.label

	// 1. Access, from relay-set fields only.
	if reason := ChainDenied(w.Allowlist, req, w.Name, "send messages to "+label+" as the owner", w.logf); reason != "" {
		w.logf("request %s from %s (chain %v): declined: %s", req.ID, req.From, req.Chain, reason)
		w.reply(ctx, req, reason, envelope.StatusDeclined, nil)
		return
	}

	// 2. Threading and the message. A site with one thread takes the
	// body as the message, less a leading bare "new chat" line (a
	// council prompt starts with one); a "conversation:" line is text
	// to it.
	var wr webRequest
	if site.oneThread {
		thread, ok := site.canonical(w.Thread)
		if !ok {
			w.reply(ctx, req, fmt.Sprintf("Nothing was sent to %s: the %s agent has no valid thread; run it with --thread <id>, the id in https://%s/dots/<id>.", label, w.Name, site.host), envelope.StatusFailed, nil)
			return
		}
		wr = webRequest{mode: threadConversation, convID: thread, message: dropNewChatLine(req.Body)}
		if wr.message == "" {
			w.reply(ctx, req, fmt.Sprintf("Nothing was sent to %s: there is no message to send.", label), envelope.StatusFailed, nil)
			return
		}
	} else {
		var err error
		if wr, err = parseWebRequest(req.Body); err != nil {
			w.reply(ctx, req, fmt.Sprintf("Nothing was sent to %s: %v. %s", label, err, threadingHelp), envelope.StatusFailed, nil)
			return
		}
	}
	if wr.mode == threadConversation && !site.oneThread {
		id, ok := site.canonical(wr.convID)
		if !ok {
			w.reply(ctx, req, fmt.Sprintf("Nothing was sent to %s: %q is not a %s conversation id. %s", label, wr.convID, label, threadingHelp), envelope.StatusFailed, nil)
			return
		}
		wr.convID = id
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
	w.sendGeneration++
	images := len(req.Attachments) > 0
	var j webJournal
	if images {
		var err error
		j, err = w.loadInputJournal()
		if err != nil {
			w.reply(ctx, req, "Nothing was sent: the image send journal could not be read safely. Repair it before retrying.", envelope.StatusFailed, nil)
			return
		}
	} else {
		j = w.loadJournal()
	}
	if e, ok := j.Requests[req.ID]; ok {
		if e.State == webSendIntent {
			if images {
				if confirmed, ok := w.reconcileInput(ctx, wr.message, req.Attachments, e); ok {
					if w.journalStrict(req.ID, confirmed) == nil {
						w.resume(ctx, req, wr, confirmed)
						return
					}
				}
			}
			w.reply(ctx, req, "Image submission is uncertain. Files may already have reached the vendor. This request will not be sent again automatically; inspect the conversation before a new ask.", envelope.StatusFailed, nil)
			return
		}
		w.resume(ctx, req, wr, e)
		return
	}
	if images {
		if err := w.Native.requireImageInput(ctx, w.Site); err != nil {
			w.reply(ctx, req, fmt.Sprintf("Nothing was sent for %q: %v. No attachments were downloaded or uploaded.", req.Attachments[0].Name, err), envelope.StatusFailed, nil)
			return
		}
		if err := validateInputMetadata(req.Attachments); err != nil {
			w.reply(ctx, req, fmt.Sprintf("Nothing was sent: %v. Use PNG/JPEG images within the limits, or send text alone.", err), envelope.StatusFailed, nil)
			return
		}
	}
	// The site rate-limited the account (or showed an anti-bot check) a
	// moment ago: it is not asked again until the cooldown ends.
	if left := w.Native.CooldownRemaining(w.Site); left > 0 {
		cerr := w.Native.cooldownError(w.Site)
		why := "rate-limited"
		if errors.Is(cerr, ErrBlocked) {
			why = "held back after an anti-bot check"
		}
		w.logf("request %s from %s: %s is %s for another %s; not sending", req.ID, req.From, label, why, left.Round(time.Second))
		w.reply(ctx, req, w.sendFailure(cerr, ""), envelope.StatusFailed, nil)
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
	anchor, err := w.anchorFor(ctx, convID, wr.message)
	var note string
	if errors.Is(err, errThreadTooLong) {
		if !remembered {
			w.reply(ctx, req, fmt.Sprintf("Nothing was sent to %s: the conversation %s is longer than tincan reads, so its reply could not be seen. Ask without a conversation: line, or with new chat, to start a new one.", label, convID), envelope.StatusFailed, nil)
			return
		}
		note = fmt.Sprintf("Your previous %s conversation (id %s) is longer than tincan reads, so this went to a new chat.", label, convID)
		delete(st.Conversations, req.From)
		convID, newChat, err = "", true, nil
		anchor = replyAnchor{message: wr.message}
	}
	if err != nil {
		w.logf("request %s from %s: reading the conversation before the send: %v; not sending", req.ID, req.From, err)
		w.reply(ctx, req, w.rateLimitReply(), envelope.StatusFailed, nil)
		return
	}
	var res SendResult
	intent := false
	if images {
		files, cleanup, inputErr := w.stageInputs(ctx, req.Attachments)
		defer cleanup()
		if inputErr != nil {
			w.reply(ctx, req, fmt.Sprintf("Nothing was sent: %v.", inputErr), envelope.StatusFailed, nil)
			return
		}
		send := func() (SendResult, error) {
			return w.Native.sendImages(ctx, w.Site, OpArgs{Message: wr.message, ConversationID: convID, NewChat: newChat}, files, func() error {
				e := webSend{ConversationID: convID, PrevUserID: anchor.prevUser, Recorded: time.Now().UTC(), SubmittedAt: time.Now().UTC(), State: webSendIntent, Input: true}
				if err := w.journalStrict(req.ID, e); err != nil {
					return errors.New("could not record image send intent; nothing was submitted")
				}
				intent = true
				return nil
			})
		}
		res, err = send()
		w.observeAuth(err)
		var ue *UnavailableError
		if remembered && convID != "" && errors.Is(err, ErrNotFound) && errors.As(err, &ue) && !ue.Clicked && !ue.Uploaded {
			note = fmt.Sprintf("Your previous %s conversation was not found, so this went to a new chat.", label)
			convID, newChat = "", true
			anchor = replyAnchor{message: wr.message}
			res, err = send() // new connection, token and transfer; no uncertain replay
			w.observeAuth(err)
		}
	} else {
		res, err = w.authSend(ctx, w.Site, wr.message, convID, newChat)
	}
	if !images && remembered && convID != "" && errors.Is(err, ErrNotFound) {
		note = fmt.Sprintf("Your previous %s conversation (id %s) was not found, so this went to a new chat.", label, convID)
		delete(st.Conversations, req.From)
		convID = ""
		anchor = replyAnchor{message: wr.message}
		res, err = w.authSend(ctx, w.Site, wr.message, "", true)
	}
	if err != nil {
		if !images {
			w.logf("request %s from %s: send: %v", req.ID, req.From, err)
		}
		if images {
			reason := fmt.Sprintf("Nothing was submitted for %s: image transfer failed before sending.", inputNames(req.Attachments))
			if intent {
				reason = "Image submission is uncertain. Files may already have reached the vendor. Do not retry automatically; inspect the conversation first."
			}
			var ue *UnavailableError
			if errors.As(err, &ue) && !ue.Clicked {
				reason = fmt.Sprintf("No message was submitted for %s: %s.", inputNames(req.Attachments), ue.Detail)
				if ue.Uploaded {
					reason += " Files may already have reached the vendor; their deletion cannot be guaranteed."
				}
			}
			w.reply(ctx, req, reason, envelope.StatusFailed, nil)
		} else {
			w.reply(ctx, req, w.sendFailure(err, convID), envelope.StatusFailed, nil)
		}
		return
	}
	// The send left its tab open so the site can finish the reply; it is
	// closed once the reply is read or the wait gives up, on a context of
	// its own because ctx may be spent by then.
	defer w.closeTab(ctx, res.ConversationID)
	anchor.since = res.Submitted()
	// A send that confirmed its message in the conversation (dots.send)
	// names it: the wait binds to that message from the start.
	if id := res.MessageID; id != "" && len(id) <= 128 && !strings.ContainsFunc(id, unicode.IsControl) {
		anchor.bound = id
	}
	entry := webSend{ConversationID: res.ConversationID, PrevUserID: anchor.prevUser, SubmittedAt: anchor.since, UserMessageID: anchor.bound, Recorded: time.Now().UTC(), State: webSendSent, Input: images}
	if images {
		if err := w.journalStrict(req.ID, entry); err != nil {
			w.reply(ctx, req, "The image message was submitted, but its confirmation could not be saved. Do not retry automatically; inspect the conversation.", envelope.StatusFailed, nil)
			return
		}
	} else {
		w.journal(req.ID, entry)
	}
	if site.oneThread {
		// One fixed thread: no used list (not a history source) and no
		// per-asker memory.
		w.answer(ctx, req, anchor, entry, note)
		return
	}
	if w.UsedPath != "" {
		if err := recordWebUsed(w.UsedPath, res.ConversationID, time.Now()); err != nil {
			w.logf("used list %s: %v", w.UsedPath, err)
		}
	}
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
	rr, err := w.readReply(ctx, convID, userID, raw)
	if err != nil {
		w.logf("request %s from %s: reading the reply in %s: %v", req.ID, req.From, convID, err)
		w.reply(ctx, req, w.waitFailure(err, convID), envelope.StatusFailed, nil)
		return
	}
	text, conv, generated, lost := rr.text, rr.conv, rr.generated, rr.lost
	if lost > 0 && w.site().noteLostImages {
		what := "image"
		if lost > 1 {
			what = "images"
		}
		lostNote := fmt.Sprintf("%s's reply had %d %s that could not be attached; open the conversation to see them.", label, lost, what)
		if note != "" {
			note += "\n\n" + lostNote
		} else {
			note = lostNote
		}
	}
	// Everything after the answer text (the sources list, any note and
	// the conversation footer) is kept whole: the answer text is what
	// gives way to keep the reply inside the cap.
	var tail string
	if w.site().sources {
		if sources := sourcesFooter(rr.sources); sources != "" {
			tail += "\n\n" + sources
		}
	}
	if note != "" {
		tail += "\n\n" + note
	}
	tail += fmt.Sprintf("\n\n%s: %s", convName(w.Site), convID)
	body, truncated := capReplyTo(text, maxWebReplyBytes-len(tail))
	if truncated {
		// Room for the notice comes out of the text too; its numbers are
		// at most len(text), so this length is an upper bound.
		const notice = "\n\n(reply truncated: showing %d of %d bytes)"
		body, _ = capReplyTo(text, maxWebReplyBytes-len(tail)-len(fmt.Sprintf(notice, len(text), len(text))))
		body += fmt.Sprintf(notice, len(body), len(text))
	}
	n := imageCount(conv)
	missing := w.missingImagesNote(generated, n)
	if missing != "" {
		n = generated
	}
	if strings.TrimSpace(text) == "" && n > 0 {
		what := "an image"
		if n > 1 {
			what = "images"
		}
		body = fmt.Sprintf("(%s replied with %s and no text)", label, what)
	}
	body += tail

	ids := w.attachImages(ctx, req, conv, &body)
	if missing != "" {
		body += "\n" + missing
	}
	e.UserMessageID, e.State = userID, webSendAnswered
	w.journal(req.ID, e)
	w.logf("request %s from %s: answered (conversation %s, %d attachments)", req.ID, req.From, convID, len(ids))
	w.reply(ctx, req, body, envelope.StatusAnswered, ids)
}

// capReplyTo caps the reply text at n bytes (none when n is negative).
func capReplyTo(s string, n int) (string, bool) {
	n = max(n, 0)
	s = strings.TrimSpace(s)
	if s == "" {
		return "(the reply was empty)", false
	}
	if len(s) <= n {
		return s, false
	}
	return capBytes(s, n), true
}

// webSource is one web source an answer cites. n, when set, is its
// number in the site's own list (the answer's [n] markers name it).
type webSource struct {
	n          int
	title, url string
}

// maxReplySources caps the sources listed after an answer; the rest are
// counted in an "(and N more)" line.
const maxReplySources = 10

// maxSourceTitle caps one source's title in bytes.
const maxSourceTitle = 200

// maxSourceURL is the longest source URL listed; a longer one is left out
// (its number still counts), so the whole list stays well inside
// maxWebReplyBytes.
const maxSourceURL = 2048

// sourcesFooter formats an answer's sources: "Sources:", then one
// "- <title> <url>" line per source ("- [n] <title> <url>" when the site
// numbers them), in the site's order. Only http and https URLs without
// credentials are listed, each once (its first occurrence, with that
// number), and none over maxSourceURL bytes; titles are put on one line
// and capped. After maxReplySources the rest are counted. It is "" when
// there is nothing to list.
func sourcesFooter(srcs []webSource) string {
	seen := map[string]bool{}
	var lines []string
	more := 0
	for _, s := range srcs {
		u, err := url.Parse(strings.TrimSpace(s.url))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			continue
		}
		link := u.String()
		if len(link) > maxSourceURL || seen[link] {
			continue
		}
		seen[link] = true
		if len(lines) == maxReplySources {
			more++
			continue
		}
		title := capBytes(strings.Join(strings.Fields(s.title), " "), maxSourceTitle)
		line := "-"
		if s.n > 0 {
			line += fmt.Sprintf(" [%d]", s.n)
		}
		if title != "" {
			line += " " + title
		}
		lines = append(lines, line+" "+link)
	}
	if len(lines) == 0 {
		return ""
	}
	if more > 0 {
		lines = append(lines, fmt.Sprintf("(and %d more)", more))
	}
	return "Sources:\n" + strings.Join(lines, "\n")
}

// rateLimitReply is the answer while the site rate-limits the account.
func (w *WebAgent) rateLimitReply() string { return rateLimitMessage(w.Site) + "." }

func (w *WebAgent) sendFailure(err error, convID string) string {
	label := siteLabel(w.Site)
	var ue *UnavailableError
	if _, ok := rateLimited(err); ok {
		return w.rateLimitReply() + clickedNote(err, w.Site)
	}
	switch {
	case errors.Is(err, ErrPaused):
		return "Nothing was sent: " + label + " is paused; unpause it in ChatGPT and ask again."
	case w.oneThread() && errors.Is(err, ErrNotFound):
		return fmt.Sprintf("Nothing was sent: %s's thread %s was not found; check the --thread the %s agent runs with.", label, convID, w.Name)
	case w.oneThread() && errors.Is(err, ErrTimeout) && clickedNote(err, w.Site) != "":
		return fmt.Sprintf("Sorry, the message was sent to %s, but it did not show up in the conversation in time; ask for the reply later instead of sending it again.", label)
	case errors.Is(err, ErrNotFound):
		return fmt.Sprintf("No %s conversation with id %s was found. Start a new one with \"new chat\" on the first line.", label, convID)
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("Sorry, %s did not finish answering in time. The message may still have been sent.", label)
	case errors.As(err, &ue):
		return "Sorry, " + ue.Error() + "." + clickedNote(err, w.Site)
	}
	return fmt.Sprintf("Sending to %s failed.", label)
}

// clickedNote is what a send failure adds when it came after the send
// button was clicked (a redirect to a sign-in page or an anti-bot check
// mid-send): the message may be in the conversation already, and sending
// it again could post it twice.
func clickedNote(err error, src Source) string {
	var ue *UnavailableError
	if !errors.As(err, &ue) || !ue.Clicked {
		return ""
	}
	return fmt.Sprintf(" The send button had already been clicked, so the message may have been sent; check %s before sending it again.", theConv(src))
}

// site is the agent's table entry, nil for a site outside the table
// (Handle fails those requests before anything reads it).
func (w *WebAgent) site() *webSite { return siteFor(w.Site) }

// oneThread reports whether the agent's site serves one fixed conversation.
func (w *WebAgent) oneThread() bool { s := w.site(); return s != nil && s.oneThread }

func (w *WebAgent) live() *live { return w.site().reader(w.Native, nil).live() }

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
	if _, ok := rateLimited(err); ok {
		// The message went through before the site started refusing reads:
		// sending it again would duplicate it and add to the rate limit.
		return fmt.Sprintf("Sorry, %s is rate-limiting this account right now. The message was sent to %s (conversation %s); ask for the reply later instead of sending it again.", siteLimiter(w.Site), label, convID)
	}
	switch {
	case errors.Is(err, errOrphaned) && w.oneThread():
		return fmt.Sprintf("Sorry, another message was sent to %s (conversation %s) before it answered this one, so there is no reply to return.", label, convID)
	case errors.Is(err, errOrphaned):
		return fmt.Sprintf("Sorry, another message was sent in the %s conversation %s before this one was answered, so there is no reply to return.", label, convID)
	case errors.Is(err, errThreadTooLong):
		return fmt.Sprintf("Sorry, the message was sent to %s, but the conversation %s is now longer than tincan reads, so the reply could not be read. Open the conversation to see it, and start a new chat next time.", label, convID)
	case errors.Is(err, errReplyUnreadable):
		return fmt.Sprintf("Sorry, the message was sent to %s (conversation %s) and it answered, but the reply could not be read.", label, convID)
	}
	if errors.As(err, &ue) && !errors.Is(err, ErrTimeout) {
		return fmt.Sprintf("Sorry, %s. The message was sent to %s (conversation %s), but the reply could not be read.", ue.Error(), label, convID)
	}
	if w.oneThread() {
		return fmt.Sprintf("Sorry, %s did not answer in time. The message was sent to %s (conversation %s); ask for the reply later instead of sending it again.", label, label, convID)
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
// the message is found by its text and time alone. A rate limit is
// returned instead: sending now would only add to it. So is
// errThreadTooLong: the reply could never be seen.
func (w *WebAgent) anchorFor(ctx context.Context, convID, message string) (replyAnchor, error) {
	a := replyAnchor{message: message}
	if convID == "" {
		return a, nil
	}
	raw, err := w.authRequest(ctx, w.live().detailOp, OpArgs{ID: convID})
	if err == nil {
		var nodes []webNode
		if nodes, err = w.nodes(raw); err == nil {
			for i := len(nodes) - 1; i >= 0; i-- {
				if nodes[i].user {
					a.prevUser = nodes[i].id
					break
				}
			}
			return a, nil
		}
	}
	if _, ok := rateLimited(err); ok || errors.Is(err, errThreadTooLong) {
		return a, err
	}
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNotLoggedIn) {
		w.logf("conversation %s: reading it before the send: %v", convID, err)
	}
	return a, nil
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
	// sources is how many source links the message carries (Copilot),
	// so a text-stability wait also waits for links that come after the
	// text.
	sources int
	// finished: the site marks this message finished.
	finished bool
	// hidden: the site does not show this message (ChatGPT's
	// is_visually_hidden_from_conversation). It carries no reply text or
	// images, but its endTurn still counts.
	hidden bool
	// endTurn: the site marks the whole turn over on this message
	// (ChatGPT: end_turn true, or finish_details without end_turn false;
	// grok.com: a finished response).
	endTurn bool
	// limited: the answer ended on the account's rate or plan limit
	// (grok.com's stream errors).
	limited bool
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
	// limited: a message in this request's turn ended on the account's
	// rate or plan limit and the turn has no usable reply (no answer text
	// and no images). A finished answer that also carries a limit error is
	// delivered as any other answer.
	limited bool
	// sig fingerprints the reply, for the stability check.
	sig string
}

func (w *WebAgent) nodes(raw json.RawMessage) ([]webNode, error) { return w.site().nodes(raw) }

func (w *WebAgent) progress(raw json.RawMessage, a replyAnchor) (replyProgress, error) {
	nodes, err := w.nodes(raw)
	if err != nil {
		return replyProgress{}, err
	}
	return progressOf(nodes, a), nil
}

// progressOf finds this request's user message on the branch (a.bound, or
// the first message after a.prevUser that a.matches) and judges its turn:
// every message after it and before the next user turn. The reply is the
// turn's last visible answer text plus the images of its visible messages
// (a generated image arrives in a tool message). The turn is finished when
// any message in it, hidden or not, ends the turn, or when its last
// visible message is a finished answer.
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
	var leaf, answer *webNode
	images, sources, ended, limited := 0, 0, false, false
	for i := b + 1; i < end; i++ {
		n := &nodes[i]
		ended = ended || n.endTurn
		limited = limited || n.limited
		if n.hidden {
			continue
		}
		leaf = n
		images += n.images
		sources += n.sources
		if n.reply && strings.TrimSpace(n.text) != "" {
			answer = n
		}
	}
	if leaf == nil && !ended {
		p.orphaned = later
		p.limited = limited
		return p
	}
	var id, text string
	if answer != nil {
		id, text = answer.id, answer.text
	}
	p.found = text != "" || images > 0
	p.limited = limited && !p.found
	p.sig = fmt.Sprintf("%s\n%d\n%d\n%s", id, images, sources, text)
	p.finished = ended || (p.found && leaf.reply && leaf.finished)
	return p
}

// chatgptNodes reads the current_node branch. A user turn is a visible user
// message with text or images (the ones parseChatGPTDetail makes turns
// of); an answer is a visible assistant text message to everyone,
// finished when its status is finished_successfully, finish_details is
// present, or end_turn is true, and end_turn is not false. Hidden
// messages are kept for their end of turn: an image turn ends on a hidden,
// empty assistant message with end_turn true, after a visible tool
// message with the image and a reasoning recap with end_turn false.
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
		text, pointers := m.parts()
		textual := m.Content.ContentType == "text" || m.Content.ContentType == "multimodal_text"
		n := webNode{id: m.ID, text: text, at: m.CreateTime.Time, hidden: m.Metadata.Hidden}
		if !n.hidden {
			n.images = len(pointers)
		}
		switch m.Author.Role {
		case "user":
			if n.hidden {
				continue
			}
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
			n.reply = !n.hidden && textual && (m.Recipient == "" || m.Recipient == "all")
			details := len(m.Metadata.FinishDetails) > 0 && string(m.Metadata.FinishDetails) != "null"
			endTurn := m.EndTurn != nil && *m.EndTurn
			notEnd := m.EndTurn != nil && !*m.EndTurn
			n.finished = !notEnd && (m.Status == "finished_successfully" || details || endTurn)
			n.endTurn = endTurn || (details && !notEnd)
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

// errReplyUnreadable: the finished read could not be turned into a reply.
var errReplyUnreadable = errors.New("the reply could not be read")

// errThreadTooLong: the conversation is longer than the extension reads,
// so its newest turns (this request's) cannot be seen.
var errThreadTooLong = errors.New("the conversation is longer than tincan reads")

// webReply is the answer read from the finished conversation: its text,
// its fetched images (conv), how many images it had before any failed to
// fetch (generated), how many could not be fetched (lost), and the web
// sources it cites.
type webReply struct {
	text            string
	conv            []Conversation
	generated, lost int
	sources         []webSource
}

// readReply builds the answer from the finished read: the reply to this
// request's user message (userID), with its generated images fetched
// through the file operation.
func (w *WebAgent) readReply(ctx context.Context, convID, userID string, raw json.RawMessage) (webReply, error) {
	l := w.live()
	th, err := l.parseDetail(convID, raw)
	if err != nil {
		return webReply{}, fmt.Errorf("%w: %v", errReplyUnreadable, err)
	}
	i := -1
	for j, t := range th.turns {
		if t.promptID == userID {
			i = j
		}
	}
	if i < 0 {
		return webReply{}, fmt.Errorf("%w: this request's turn is not in the conversation", errReplyUnreadable)
	}
	t := th.turns[i]
	r := webReply{text: t.reply.Text, sources: t.replySources}
	if len(t.replyImages) == 0 {
		return r, nil
	}
	c := []Conversation{{Source: w.Site, ID: convID, Messages: []Message{{Role: RoleAssistant, Images: t.replyImages}}}}
	c = l.resolve(ctx, c)
	r.lost = max(len(t.replyImages)-imageCount(c), 0)
	capImages(c)
	r.conv, r.generated = c, len(t.replyImages)
	return r, nil
}

// missingImagesNote is the note on a reply whose generated images could
// not all be fetched from the site ("" when nothing is missing or the
// site does not note it).
func (w *WebAgent) missingImagesNote(generated, fetched int) string {
	if !w.site().noteMissingImages || fetched >= min(generated, MaxImages) {
		return ""
	}
	if fetched == 0 {
		return "The images could not be attached."
	}
	return fmt.Sprintf("%d of the images could not be attached.", min(generated, MaxImages)-fetched)
}

func imageCount(conv []Conversation) int {
	n := 0
	for _, c := range conv {
		for _, m := range c.Messages {
			n += len(m.Images)
		}
	}
	return n
}

// attachImages saves and uploads the reply's images and appends a line to
// body saying what was actually attached.
func (w *WebAgent) attachImages(ctx context.Context, req envelope.Request, conv []Conversation, body *string) []string {
	if imageCount(conv) == 0 {
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
