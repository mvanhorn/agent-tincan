package history

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// DefaultAllowlist is who may read history when no allowlist file exists.
var DefaultAllowlist = []string{"grokbot", "claude-code", "codex"}

// DefaultAllowlistPath is the allowlist file beside the history config.
func DefaultAllowlistPath() string { return configPath("", "history-allow.txt") }

var agentName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// LoadAllowlist reads agent names from path: one or more per line,
// separated by spaces or commas, with # starting a comment. A missing file
// means DefaultAllowlist. Any name that is not a plain agent name is an
// error, so a typo never silently widens or narrows access.
func LoadAllowlist(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return slices.Clone(DefaultAllowlist), nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		for _, name := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if !agentName.MatchString(name) {
				return nil, fmt.Errorf("allowlist %s: %q is not an agent name", path, name)
			}
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// StaticAllowlist is an allowlist source that never changes.
func StaticAllowlist(names ...string) func() ([]string, error) {
	names = slices.Clone(names)
	return func() ([]string, error) { return names, nil }
}

// FileAllowlist rereads path for every request, so adding an agent takes
// effect without restarting the service.
func FileAllowlist(path string) func() ([]string, error) {
	return func() ([]string, error) { return LoadAllowlist(path) }
}

// Reply template caps.
const (
	maxPromptRunes   = 2000
	maxExcerptRunes  = 300
	maxReplyBytes    = 64 << 10
	maxPromptsInConv = 20
)

// DefaultRequestTimeout bounds the handling of one request.
const DefaultRequestTimeout = 3 * time.Minute

// clarifyExample is the one-line example in a clarifying reply.
const clarifyExample = `For example: "what was the last thing Matt asked ChatGPT? send the image"`

// Service is the history agent: it polls the relay as its own identity,
// checks each request's relay-set chain against the allowlist, turns the
// question into a Query with a tool-less extractor, reads the source, and
// replies from a fixed template with the images attached. Retrieved chat
// content never reaches the extractor or any other LLM.
type Service struct {
	Relay     *client.Relay
	Extractor Extractor
	Readers   map[Source]Reader
	// Allowlist returns the agents allowed to read history, consulted for
	// every request. An error declines the request.
	Allowlist func() ([]string, error)
	// TempDir is where per-request image dirs are made (os.TempDir when
	// empty).
	TempDir string
	// Hold is the long-poll hold (client.DefaultPollHold when zero).
	Hold time.Duration
	// RequestTimeout bounds one request (DefaultRequestTimeout when zero).
	RequestTimeout time.Duration
	// Log receives one line per request and per error (stderr when nil).
	Log io.Writer

	// onImageDir is a test hook called with each per-request image dir.
	onImageDir func(dir string)
}

func (s *Service) logf(format string, args ...any) {
	w := s.Log
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "tincan history: "+format+"\n", args...)
}

// Run polls and handles requests until ctx is cancelled. A request being
// handled when ctx is cancelled is still finished and answered, within
// RequestTimeout. Poll errors back off and retry; only a 403 (this agent
// is not joined) stops the loop.
func (s *Service) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := s.PollOnce(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case client.IsStatus(err, http.StatusForbidden):
			return err
		case err != nil:
			s.logf("poll failed: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
	}
}

// PollOnce waits up to Hold for requests, then handles each one serially.
// It returns how many requests it handled.
func (s *Service) PollOnce(ctx context.Context) (int, error) {
	hold := s.Hold
	if hold == 0 {
		hold = client.DefaultPollHold
	}
	in, err := s.Relay.PollReplies(ctx, hold, client.RepliesNone)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, req := range in.Requests {
		s.handleSafely(ctx, req)
		n++
	}
	return n, nil
}

// handleSafely handles one request so that nothing it does, including a
// panic, stops the loop. It keeps running after ctx is cancelled so a
// claimed request still gets its reply.
func (s *Service) handleSafely(ctx context.Context, req envelope.Request) {
	timeout := s.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			s.logf("request %s: panic: %v", req.ID, p)
			s.reply(hctx, req, "The history agent hit an internal error handling this request.", envelope.StatusFailed, nil)
		}
	}()
	s.Handle(hctx, req)
}

// Handle claims and answers one request.
func (s *Service) Handle(ctx context.Context, req envelope.Request) {
	if _, err := s.Relay.Claim(ctx, req.ID); err != nil {
		s.logf("request %s from %s: claim failed: %v", req.ID, req.From, err)
		return
	}

	// 1. Access, from relay-set fields only. The body is never consulted.
	if reason := s.denied(req); reason != "" {
		s.logf("request %s from %s (chain %v): declined: %s", req.ID, req.From, req.Chain, reason)
		s.reply(ctx, req, reason, envelope.StatusDeclined, nil)
		return
	}

	// 2. The question text, and only that, goes to the extractor.
	q, err := s.Extractor.Extract(ctx, req.Body)
	if err == nil {
		err = ValidateServiceQuery(q)
	}
	if errors.Is(err, ErrUnclearQuestion) {
		s.logf("request %s from %s: unclear question: %v", req.ID, req.From, err)
		s.reply(ctx, req, "I could not turn that into a history lookup. Please ask a clearer question naming ChatGPT, claude.ai, Codex or Claude Code. "+clarifyExample, envelope.StatusFailed, nil)
		return
	}
	if err != nil {
		s.logf("request %s from %s: extractor failed: %v", req.ID, req.From, err)
		s.reply(ctx, req, "The history agent could not process the question right now (its query step failed). Try again in a minute.", envelope.StatusFailed, nil)
		return
	}

	// 3. Read.
	r, ok := s.Readers[q.Source]
	if !ok {
		s.reply(ctx, req, fmt.Sprintf("The %s source is not available on this machine.", q.Source), envelope.StatusFailed, nil)
		return
	}
	var dir string
	if q.wantsImages() {
		base := s.TempDir
		if base == "" {
			base = os.TempDir()
		}
		d, err := os.MkdirTemp(base, "tincan-history-req-")
		if err != nil {
			s.logf("request %s: image dir: %v", req.ID, err)
			s.reply(ctx, req, "The history agent could not prepare a place for the images.", envelope.StatusFailed, nil)
			return
		}
		dir = d
		defer func() { _ = os.RemoveAll(dir) }()
		if s.onImageDir != nil {
			s.onImageDir(dir)
		}
	}
	convs, err := r.Read(ctx, q, Options{})
	if err != nil {
		s.logf("request %s: read %s: %v", req.ID, q.Source, err)
		s.reply(ctx, req, readFailure(q, err), envelope.StatusFailed, nil)
		return
	}

	// 4. Images, then the templated reply.
	var paths []string
	if dir != "" {
		if err := SaveImages(dir, convs); err != nil {
			s.logf("request %s: save images: %v", req.ID, err)
			s.reply(ctx, req, "The history agent found the conversation but could not prepare its images.", envelope.StatusFailed, nil)
			return
		}
		paths = imagePaths(convs)
	}
	// The images line is written only after the upload, from what was
	// actually attached, so it never claims images that did not arrive.
	body := renderReply(q, convs)
	var ids []string
	if len(paths) > 0 {
		ups, err := s.Relay.UploadFiles(ctx, paths)
		switch {
		case errors.Is(err, client.ErrAttachmentsUnsupported):
			body += "\n\nThe images could not be attached: this relay does not support attachments."
		case err != nil:
			s.logf("request %s: upload images: %v", req.ID, err)
			body += "\n\nThe images could not be attached: the upload to the relay failed."
		default:
			ids = client.AttachmentIDs(ups)
			body += "\n" + imagesLine(len(ids))
		}
	} else if q.wantsImages() && len(convs) > 0 {
		body += "\nNo images on this turn."
	}
	s.logf("request %s from %s: answered %s %s (%d conversations, %d attachments)", req.ID, req.From, q.Source, q.Mode, len(convs), len(ids))
	s.reply(ctx, req, body, envelope.StatusAnswered, ids)
}

// replyTimeout bounds sending one reply.
const replyTimeout = 30 * time.Second

// reply sends on its own context, detached from ctx's deadline, so a request
// that ran out of time (or panicked after it did) still gets its answer.
func (s *Service) reply(ctx context.Context, req envelope.Request, body string, status envelope.Status, ids []string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)
	defer cancel()
	if _, err := s.Relay.ReplyAttached(rctx, req.ID, body, status, ids); err != nil {
		s.logf("request %s: reply failed: %v", req.ID, err)
	}
}

// denied returns why req may not read history, or "" when every agent in
// its relay-set chain, and its sender, is on the allowlist.
func (s *Service) denied(req envelope.Request) string {
	if s.Allowlist == nil {
		return "Declined: the history agent has no allowlist configured."
	}
	allowed, err := s.Allowlist()
	if err != nil {
		s.logf("allowlist: %v", err)
		return "Declined: the history agent could not read its allowlist, so it is not answering anyone until that is fixed."
	}
	chain := slices.Clone(req.Chain)
	if req.From != "" && !slices.Contains(chain, req.From) {
		chain = append(chain, req.From)
	}
	if len(chain) == 0 {
		return "Declined: the request has no sender."
	}
	for _, a := range chain {
		if !slices.Contains(allowed, a) {
			if a == req.From {
				return fmt.Sprintf("Declined: %s is not on the history allowlist, so it cannot read Matt's conversation history. Matt can add it to the allowlist.", a)
			}
			return fmt.Sprintf("Declined: this request came through %s, which is not on the history allowlist, so it cannot read Matt's conversation history. Every agent in the chain must be allowed.", a)
		}
	}
	return ""
}

// readFailure phrases a reader error for the reply.
func readFailure(q Query, err error) string {
	var ue *UnavailableError
	switch {
	case errors.As(err, &ue):
		return "Sorry, " + ue.Error() + "."
	case errors.Is(err, ErrNotFound):
		return fmt.Sprintf("No %s conversation with id %s was found.", sourceLabel(q.Source), q.ConversationID)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("Reading %s history took too long.", sourceLabel(q.Source))
	}
	return fmt.Sprintf("Reading %s history failed.", sourceLabel(q.Source))
}

func sourceLabel(s Source) string {
	switch s {
	case SourceChatGPT:
		return "ChatGPT"
	case SourceClaudeAI:
		return "claude.ai"
	case SourceCodex:
		return "Codex"
	case SourceClaudeCode:
		return "Claude Code"
	}
	return string(s)
}

// imagePaths lists the saved images across convs, deduplicated, at most
// MaxImages.
func imagePaths(convs []Conversation) []string {
	var out []string
	for _, c := range convs {
		for _, m := range c.Messages {
			for _, im := range m.Images {
				if im.Path != "" && !slices.Contains(out, im.Path) && len(out) < MaxImages {
					out = append(out, im.Path)
				}
			}
		}
	}
	return out
}

// imagesLine states how many images were attached to the reply.
func imagesLine(n int) string {
	switch n {
	case 0:
		return "No images were attached."
	case 1:
		return "1 image attached."
	}
	return fmt.Sprintf("%d images attached.", n)
}

// renderReply fills the fixed reply template. Only fields come from the
// conversations; nothing in them is interpreted.
func renderReply(q Query, convs []Conversation) string {
	label := sourceLabel(q.Source)
	if len(convs) == 0 {
		if q.WithImages {
			msg := fmt.Sprintf("No %s turn with images found in the last %d conversations (up to %d days)", label, DefaultWindow().Max, int(DefaultWindow().MaxAge.Hours()/24))
			if q.Mode == ModeSearch {
				msg += " for: " + strings.Join(q.Terms, ", ")
			}
			return msg + "."
		}
		switch q.Mode {
		case ModeSearch:
			return fmt.Sprintf("No matching %s conversation in the recent window (the last %d conversations, up to %d days) for: %s.", label, DefaultWindow().Max, int(DefaultWindow().MaxAge.Hours()/24), strings.Join(q.Terms, ", "))
		default:
			return fmt.Sprintf("No matching %s conversation in the recent window (the last %d conversations, up to %d days).", label, DefaultWindow().Max, int(DefaultWindow().MaxAge.Hours()/24))
		}
	}
	var b bytes.Buffer
	for i, c := range convs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if len(convs) > 1 {
			fmt.Fprintf(&b, "%d. ", i+1)
		}
		title := oneLineText(c.Title)
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "%s conversation %q (id %s)\n", label, title, c.ID)
		if c.Cwd != "" {
			fmt.Fprintf(&b, "Working directory: %s\n", c.Cwd)
		}
		prompts := 0
		var lastReply string
		var when time.Time
		for _, m := range c.Messages {
			switch m.Role {
			case RoleUser:
				prompts++
				if prompts > maxPromptsInConv {
					continue
				}
				ts := m.Time
				if ts.IsZero() {
					ts = c.UpdatedAt
				}
				if when.IsZero() || ts.After(when) {
					when = ts
				}
				fmt.Fprintf(&b, "Matt asked%s:\n%s\n", stamp(ts), capRunes(m.Text, maxPromptRunes))
			case RoleAssistant:
				lastReply = m.Text
				if q.Mode == ModeConversation {
					fmt.Fprintf(&b, "Reply excerpt: %s\n", capRunes(oneLineText(m.Text), maxExcerptRunes))
					lastReply = ""
				}
			}
		}
		if prompts > maxPromptsInConv {
			fmt.Fprintf(&b, "(%d earlier prompts not shown)\n", prompts-maxPromptsInConv)
		}
		if prompts == 0 {
			fmt.Fprintf(&b, "Last updated%s\n", stamp(c.UpdatedAt))
		}
		if lastReply != "" {
			fmt.Fprintf(&b, "Reply excerpt: %s\n", capRunes(oneLineText(lastReply), maxExcerptRunes))
		}
		if b.Len() > maxReplyBytes {
			break
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if len(out) > maxReplyBytes {
		out = capBytes(out, maxReplyBytes) + "\n(reply truncated)"
	}
	return out
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " at " + t.Local().Format("2006-01-02 15:04 MST")
}

func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }

func capRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
