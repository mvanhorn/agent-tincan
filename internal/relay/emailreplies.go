package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// Email replies.
//
// An agent with include_requests on gets each open ask in an email of its
// own whose subject carries "[tincan <id>.<tag>]" (wake/requestmail.go).
// It can answer by replying to that email. The relay polls the agent's
// AgentMail inbox (Run, every Config.EmailPollEvery) while the agent has an
// open ask and for emailReplyWindow after its last ask closed, and decides
// each received message once, first failing check deciding:
//
//   - already in email_replies: skipped;
//   - no "[tincan " in the subject: ignored and never fetched in full, so
//     other mail in a shared inbox is left alone;
//   - labelled spam or unauthenticated (never asked for, so only if
//     AgentMail sends it anyway): ignored;
//   - From is not the agent's email_to: a row, and no response email;
//   - the tag does not verify for the request, the agent and the current
//     clarification round, or the request does not exist: "out of date";
//   - the request is not open: "already answered, cancelled or expired";
//   - it is claimed under a live lease, or waiting on a clarification
//     asked over tincan: "being handled over tincan";
//   - no reply-stripped text, nothing left after tag lines are removed
//     (an attachment-only reply), or a "needs input:" first line: "not
//     supported by email";
//   - longer than the reply limit: "too long";
//   - otherwise the reply is recorded through recordReply, the same path
//     as a reply over tincan, with its email_replies row in the same
//     transaction. A first line starting "failed:" or "declined:" sets
//     that status and the marker is removed from the body.
//
// Every row except a wrong sender's owes the agent one email back in the
// thread, sent through AgentMail's reply call with an Idempotency-Key
// derived from the inbound message id after the row is committed, and
// retried every poll until it is sent. Response emails carry no request
// or reply text.
//
// The relay never deletes, marks read or relabels mail. The From address,
// the tag and mail text never go to the relay log, the audit log or an
// error.

// Mailbox is the AgentMail inbox an opted-in agent's request emails are
// sent from, and so where its replies arrive (wake.AgentMail).
type Mailbox interface {
	Received(ctx context.Context, after time.Time) ([]wake.MailMessage, error)
	Message(ctx context.Context, messageID string) (wake.MailMessage, error)
	Reply(ctx context.Context, messageID, idempotencyKey, text string) error
}

// EmailInbox is one agent with include_requests on: its name, its own mail
// address (wake.json email_to), which is the only sender whose replies
// count, and the inbox its request emails come from.
type EmailInbox struct {
	Agent   string
	Address string
	Mail    Mailbox
}

// DefaultEmailPollEvery is how often the relay polls an opted-in agent's
// inbox while its window is open.
const DefaultEmailPollEvery = 30 * time.Second

// emailReplyWindow is how long after an opted-in agent's last ask closed
// the relay keeps polling, and how far back the first poll after a start
// looks, so a reply sent while the relay was down is still seen.
const emailReplyWindow = 24 * time.Hour

// emailCursorOverlap is how far before a poll's start the next poll looks,
// so mail AgentMail timestamps a little before the relay's clock is not
// missed. email_replies drops what was already decided.
const emailCursorOverlap = 2 * time.Minute

// Email reply outcomes kept in email_replies; each but the wrong sender's
// has a response text in emailResponses.
const (
	emailWrongSender = "wrong_sender"
	emailOutOfDate   = "out_of_date"
	emailClosed      = "closed"
	emailClaimed     = "claimed"
	emailUnsupported = "unsupported"
	emailTooLong     = "too_long"
)

// emailResponses is the email back for each outcome. None carries request
// or reply text.
var emailResponses = map[string]string{
	store.EmailOutcomeRecorded: "Agent Tincan: recorded. Your reply is now your answer to this request, and your teammate has it.\n",
	emailOutOfDate: "Agent Tincan: not recorded: this email is out of date. The request has changed since this email was sent (or its tag is wrong). " +
		"Answer from the latest request email, or through tincan.\n",
	emailClosed:  "Agent Tincan: not recorded: this request was already answered, cancelled or expired. Nothing changed.\n",
	emailClaimed: "Agent Tincan: not recorded: this request is being handled over tincan. Answer it there.\n",
	emailUnsupported: "Agent Tincan: not recorded: not supported by email. An email reply can answer, fail or decline a request with text; " +
		"it cannot ask a clarifying question, and a reply with no text of its own (only attachments, or only quoted text) has nothing to record. " +
		"Use tincan for those.\n",
	emailTooLong: "Agent Tincan: not recorded: too long. An email reply can be at most 256 KiB; answer through tincan instead.\n",
}

// SetEmailInboxes installs the agents whose request-email replies the
// relay polls for. Call it before Run.
func (s *Server) SetEmailInboxes(inboxes []EmailInbox) {
	s.emailMu.Lock()
	defer s.emailMu.Unlock()
	s.inboxes = inboxes
}

func (s *Server) hasEmailInboxes() bool {
	s.emailMu.Lock()
	defer s.emailMu.Unlock()
	return len(s.inboxes) > 0
}

// PollEmailReplies runs one poll of every opted-in agent's inbox: it
// retries the response emails still owed, then, for each agent whose
// window is open, lists the mail received since its cursor and decides
// each message. Run calls it every EmailPollEvery.
func (s *Server) PollEmailReplies(ctx context.Context) {
	s.emailMu.Lock()
	defer s.emailMu.Unlock()
	for _, in := range s.inboxes {
		s.sendOwedResponses(ctx, in)
		open, err := s.emailWindowOpen(ctx, in.Agent)
		if err != nil {
			log.Printf("email replies %s: %v", in.Agent, err)
			continue
		}
		if open {
			s.pollInbox(ctx, in)
		}
	}
}

// emailWindowOpen reports whether agent has an open ask, or had one close
// within emailReplyWindow.
func (s *Server) emailWindowOpen(ctx context.Context, agent string) (bool, error) {
	asks, err := s.store.OpenAsks(ctx, agent)
	if err != nil || len(asks) > 0 {
		return len(asks) > 0, err
	}
	closed, err := s.store.LastAskClosed(ctx, agent)
	if err != nil || closed.IsZero() {
		return false, err
	}
	return s.cfg.Now().Sub(closed) < emailReplyWindow, nil
}

// pollInbox lists in's mail since its cursor and decides each message. The
// cursor, in memory only, starts emailReplyWindow before the relay started
// and moves on only when every message listed was decided, so a message
// that hit a passing failure is listed again. A message that keeps failing
// holds the cursor back for at most emailReplyWindow, so it cannot pin
// every later poll to an ever longer listing.
func (s *Server) pollInbox(ctx context.Context, in EmailInbox) {
	start := s.cfg.Now()
	after, ok := s.emailCursor[in.Agent]
	if !ok {
		after = s.started.Add(-emailReplyWindow)
	}
	msgs, err := in.Mail.Received(ctx, after)
	truncated := errors.Is(err, wake.ErrListTruncated)
	if err != nil && !truncated {
		log.Printf("email replies %s: list: %v", in.Agent, err)
		return
	}
	failed := false
	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		if !s.decideEmail(ctx, in, m) {
			failed = true
		}
	}
	switch {
	case failed:
		s.emailCursor[in.Agent] = later(after, start.Add(-emailReplyWindow))
	case truncated && len(msgs) > 0:
		// Listed oldest first: carry on after the last one read.
		s.emailCursor[in.Agent] = msgs[len(msgs)-1].Timestamp.Add(-time.Second)
	case !truncated:
		s.emailCursor[in.Agent] = start.Add(-emailCursorOverlap)
	}
}

// later returns the later of a and b.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// emailTagRE finds a request email's tag in a subject: "[tincan <id>.<tag>]".
var emailTagRE = regexp.MustCompile(`\[tincan ([A-Za-z0-9_-]{1,64})\.([A-Za-z0-9]{1,64})\]`)

// decideEmail decides message m from in's inbox and sends its response
// email. It reports false when it could not decide m for now (a store or
// AgentMail failure), so the caller lists it again next poll.
func (s *Server) decideEmail(ctx context.Context, in EmailInbox, m wake.MailMessage) bool {
	if m.MessageID == "" || !strings.Contains(m.Subject, "[tincan ") {
		return true // not a reply to a request email: never touched
	}
	if done, err := s.store.EmailDecided(ctx, m.MessageID); err != nil {
		log.Printf("email replies %s: %v", in.Agent, err)
		return false
	} else if done {
		return true
	}
	if m.HasLabel("spam") || m.HasLabel("unauthenticated") {
		return true
	}
	var id, tag string
	if sm := emailTagRE.FindStringSubmatch(m.Subject); sm != nil {
		id, tag = sm[1], sm[2]
	}
	if !sameAddress(m.From, in.Address) {
		return s.decide(ctx, in, m.MessageID, id, emailWrongSender, false)
	}
	outcome, rep, err := s.judgeEmail(ctx, in, m, id, tag)
	if err != nil {
		log.Printf("email replies %s: %v", in.Agent, err)
		return false
	}
	if outcome != store.EmailOutcomeRecorded {
		return s.decide(ctx, in, m.MessageID, id, outcome, true)
	}
	_, err = s.recordReply(ctx, id, in.Agent, rep, m.MessageID)
	switch {
	case err == nil:
		s.respond(ctx, in, m.MessageID, outcome)
		return true
	case errors.Is(err, store.ErrEmailDecided):
		return true
	case errors.Is(err, store.ErrLiveClaim):
		// Claimed since judgeEmail looked.
		return s.decide(ctx, in, m.MessageID, id, emailClaimed, true)
	case errors.Is(err, store.ErrWrongState):
		return s.decide(ctx, in, m.MessageID, id, emailClosed, true)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrForbidden):
		return s.decide(ctx, in, m.MessageID, id, emailOutOfDate, true)
	}
	log.Printf("email replies %s: record reply to %s: %v", in.Agent, id, err)
	return false
}

// judgeEmail runs the checks after the sender's: the tag, the request's
// state and the reply's text. It returns the outcome, and for
// EmailOutcomeRecorded the reply to store. An error means m could not be
// judged now.
func (s *Server) judgeEmail(ctx context.Context, in EmailInbox, m wake.MailMessage, id, tag string) (string, envelope.Reply, error) {
	if id == "" {
		return emailOutOfDate, envelope.Reply{}, nil
	}
	req, status, err := s.store.Request(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return emailOutOfDate, envelope.Reply{}, nil
	}
	if err != nil {
		return "", envelope.Reply{}, err
	}
	if req.To != in.Agent || req.Kind != envelope.KindAsk || !s.VerifyRequestEmailTag(tag, req) {
		return emailOutOfDate, envelope.Reply{}, nil
	}
	switch status {
	case envelope.StatusQueued, envelope.StatusDelivered, envelope.StatusClaimed:
	case envelope.StatusNeedsInput:
		// A clarification asked over tincan is waiting on the asker.
		return emailClaimed, envelope.Reply{}, nil
	default:
		return emailClosed, envelope.Reply{}, nil
	}
	if live, err := s.store.ClaimLive(ctx, id); err != nil {
		return "", envelope.Reply{}, err
	} else if live {
		return emailClaimed, envelope.Reply{}, nil
	}
	full, err := in.Mail.Message(ctx, m.MessageID)
	if err != nil {
		return "", envelope.Reply{}, err
	}
	if full.ExtractedText == nil {
		return emailUnsupported, envelope.Reply{}, nil
	}
	rep, ok := emailReply(*full.ExtractedText)
	if !ok {
		return emailUnsupported, envelope.Reply{}, nil
	}
	if len(rep.Body) > envelope.DefaultMaxBody {
		return emailTooLong, envelope.Reply{}, nil
	}
	return store.EmailOutcomeRecorded, rep, nil
}

// emailReply turns an email's reply-stripped text into a reply: lines
// holding a "[tincan " tag (quoted history a mail client kept) are
// removed, a first line starting "failed:" or "declined:" (any case) sets
// that status and loses the marker, and anything else is answered. It
// reports false for text email cannot record: nothing left, or a "needs
// input:" first line.
func emailReply(text string) (envelope.Reply, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.Contains(l, "[tincan ") {
			kept = append(kept, l)
		}
	}
	body := strings.TrimSpace(strings.Join(kept, "\n"))
	if body == "" {
		return envelope.Reply{}, false
	}
	if hasPrefixFold(body, "needs input:") {
		return envelope.Reply{}, false
	}
	rep := envelope.Reply{Status: envelope.StatusAnswered, Body: body}
	for _, st := range []envelope.Status{envelope.StatusFailed, envelope.StatusDeclined} {
		if marker := string(st) + ":"; hasPrefixFold(body, marker) {
			rep.Status, rep.Body = st, strings.TrimSpace(body[len(marker):])
		}
	}
	return rep, true
}

// hasPrefixFold reports whether s starts with the ASCII prefix p, ignoring
// case.
func hasPrefixFold(s, p string) bool {
	return len(s) >= len(p) && strings.EqualFold(s[:len(p)], p)
}

// sameAddress reports whether from, as AgentMail gives it (a bare address
// or "Name <address>"), is the address want, ignoring case.
func sameAddress(from, want string) bool {
	a, err := mail.ParseAddress(from)
	if err != nil {
		return false
	}
	w, err := mail.ParseAddress(want)
	if err != nil {
		return false
	}
	return strings.EqualFold(a.Address, w.Address)
}

// decide writes m's email_replies row with outcome and, when respond is
// set, sends its response. It reports false when the row could not be
// written.
func (s *Server) decide(ctx context.Context, in EmailInbox, messageID, requestID, outcome string, respond bool) bool {
	err := s.store.DecideEmail(ctx, messageID, in.Agent, requestID, outcome, respond)
	if errors.Is(err, store.ErrEmailDecided) {
		return true
	}
	if err != nil {
		log.Printf("email replies %s: %v", in.Agent, err)
		return false
	}
	if respond {
		s.respond(ctx, in, messageID, outcome)
	}
	return true
}

// respond sends the response email for a decided message and marks it
// sent. A failure is logged and left for the next poll, which sends it
// again with the same idempotency key.
func (s *Server) respond(ctx context.Context, in EmailInbox, messageID, outcome string) {
	text, ok := emailResponses[outcome]
	if !ok {
		return
	}
	if err := in.Mail.Reply(ctx, messageID, emailResponseKey(messageID), text); err != nil {
		log.Printf("email replies %s: response email not sent yet (%s): %v", in.Agent, outcome, err)
		return
	}
	if err := s.store.MarkEmailResponseSent(ctx, messageID); err != nil {
		log.Printf("email replies %s: %v", in.Agent, err)
	}
}

// sendOwedResponses retries in's response emails that were decided but not
// yet sent.
func (s *Server) sendOwedResponses(ctx context.Context, in EmailInbox) {
	owed, err := s.store.UnsentEmailResponses(ctx, in.Agent)
	if err != nil {
		log.Printf("email replies %s: %v", in.Agent, err)
		return
	}
	for _, e := range owed {
		s.respond(ctx, in, e.MessageID, e.Outcome)
	}
}

// emailResponseKey is the Idempotency-Key of the response to inbound
// message messageID: the same on every retry, so AgentMail sends it once.
func emailResponseKey(messageID string) string {
	sum := sha256.Sum256([]byte("tincan-email-response:" + messageID))
	return "tincan-" + hex.EncodeToString(sum[:16])
}
