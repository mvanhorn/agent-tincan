package pass

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/textfit"
)

// DefaultAgentName is the pass service's Tincan name.
const DefaultAgentName = "pass"

// GuardVersion is the first tincan release whose web agents refuse every
// asker's conversation: continuation of a chat a pass opened. The service
// passes only to a destination that last called the relay with it or a
// later release, since the asking agent learns the new chat's id.
const GuardVersion = "0.18.0"

// DefaultReplyWait is how long the service waits for the destination to
// open the new chat.
const DefaultReplyWait = 15 * time.Minute

// DefaultRenewEvery is how often a waiting pass posts a progress note,
// which renews its claim well inside the relay's 30-minute lease.
const DefaultRenewEvery = 5 * time.Minute

// replyTimeout bounds sending one reply or progress note.
const replyTimeout = 30 * time.Second

// maxForwardBytes caps the destination's failure text a failed pass
// passes on. Web agents fill failures from fixed templates; a failure is
// never the site's answer.
const maxForwardBytes = 300

// Service is the pass teammate: it polls the relay as its own agent,
// checks each request's relay-recorded chain against the pass and history
// allowlists, reads the one chat the request names, checks it against the
// owner's history window, and asks the destination web chat teammate to
// open a new chat with it, as a child of the request grouped pass-<id>. It
// replies with the new conversation's id only, never the chat's text.
type Service struct {
	Relay *client.Relay
	// Reader returns the live reader for a chat site.
	Reader func(history.Source) (history.Reader, bool)
	// Allowlist returns the agents allowed to ask for a pass
	// (pass-allow.txt), and HistoryAllowlist those allowed to read
	// history (history-allow.txt). Every agent in a request's chain must
	// be on both; an error declines.
	Allowlist, HistoryAllowlist func() ([]string, error)
	// WindowFile is the owner's history window file, reread for every
	// request; a passed chat must have been updated inside its days.
	// Empty means the default window. A file that is not valid fails the
	// request.
	WindowFile string
	// Hold is the long-poll hold (client.DefaultPollHold when zero).
	Hold time.Duration
	// ReplyWait bounds the wait for the destination (DefaultReplyWait
	// when zero), and RenewEvery spaces the progress notes meanwhile
	// (DefaultRenewEvery when zero).
	ReplyWait, RenewEvery time.Duration
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// Log receives one line per request and per error (stderr when nil).
	Log io.Writer
}

func (s *Service) logf(format string, args ...any) {
	w := s.Log
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "tincan pass: "+format+"\n", args...)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Run refuses to serve unless the relay stores kind pass for this agent:
// only then does the relay hold agents' passes for the owner's approval.
// It then polls and handles requests until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	me, err := s.Relay.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("pass: could not confirm this agent's kind with the relay: %w", err)
	}
	if me.Kind != onboard.KindPass {
		return fmt.Errorf("pass serve refuses to run: the relay stores kind %q for %s, not %q, so it would not hold agents' passes for the owner's approval. "+
			"Upgrade the relay first, then on an admin device run: tincan kind %s %s", me.Kind, me.Name, onboard.KindPass, me.Name, onboard.KindPass)
	}
	return history.RunPolling(ctx, s.PollOnce, s.logf)
}

// PollOnce waits up to Hold for requests and handles each one serially.
func (s *Service) PollOnce(ctx context.Context) (int, error) {
	return history.PollAndHandle(ctx, s.Relay, s.Hold, "pass-serve", s.handleSafely)
}

func (s *Service) replyWait() time.Duration {
	if s.ReplyWait > 0 {
		return s.ReplyWait
	}
	return DefaultReplyWait
}

// handleSafely handles one request so that nothing it does, a panic
// included, stops the loop, and keeps the service online meanwhile.
func (s *Service) handleSafely(ctx context.Context, req envelope.Request) {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.replyWait()+5*time.Minute)
	defer cancel()
	defer s.Relay.KeepPresence(hctx, client.Presence{Logf: s.logf})()
	defer func() {
		if p := recover(); p != nil {
			s.logf("request %s: panic: %v", req.ID, p)
			s.reply(hctx, req, "The pass agent hit an internal error handling this request.", envelope.StatusFailed)
		}
	}()
	s.Handle(hctx, req)
}

// Handle claims one request and passes the chat it names.
func (s *Service) Handle(ctx context.Context, req envelope.Request) {
	claimed, err := s.Relay.Claim(ctx, req.ID)
	if err != nil {
		s.logf("request %s from %s: claim failed: %v", req.ID, req.From, err)
		return
	}
	if claimed.ID != "" {
		req = claimed
	}

	// Access is checked from relay-set fields only. Passing a chat reads
	// it, so the history allowlist applies as well as the pass one.
	for _, check := range []struct {
		list        func() ([]string, error)
		agent, what string
	}{
		{s.Allowlist, "pass", "pass the owner's chats to another chat site"},
		{s.HistoryAllowlist, "history", "read the owner's conversation history"},
	} {
		if reason := history.ChainDenied(check.list, req, check.agent, check.what, s.logf); reason != "" {
			s.logf("request %s from %s (chain %v): declined: %s", req.ID, req.From, req.Chain, reason)
			s.reply(ctx, req, reason, envelope.StatusDeclined)
			return
		}
	}

	r, err := ParseRequest(req.Body)
	if err != nil {
		s.reply(ctx, req, "Nothing was passed: "+err.Error()+".", envelope.StatusFailed)
		return
	}

	// A bad window file fails closed.
	window, err := s.window()
	if err != nil {
		s.logf("request %s: window file %s: %v", req.ID, s.WindowFile, err)
		s.reply(ctx, req, fmt.Sprintf("Nothing was passed: the pass agent could not use the history window file (%s), so it passes nothing until the owner fixes it.", filepath.Base(s.WindowFile)), envelope.StatusFailed)
		return
	}

	roster, err := s.Relay.Agents(ctx)
	if err != nil {
		s.logf("request %s: roster: %v", req.ID, err)
		s.reply(ctx, req, "Nothing was passed: the pass agent could not read the team roster.", envelope.StatusFailed)
		return
	}

	// A redelivered request whose seed already went out waits for that
	// seed rather than sending the chat again.
	group := envelope.PassGroupPrefix + req.ID
	seedID, dst, err := s.sentSeed(ctx, group, roster, r.To)
	if err != nil {
		s.logf("request %s: group %s: %v", req.ID, group, err)
		s.reply(ctx, req, "Nothing was passed: the pass agent could not check whether this pass already went out.", envelope.StatusFailed)
		return
	}
	if seedID == "" {
		if seedID, dst = s.send(ctx, req, r, roster, window, group); seedID == "" {
			return
		}
	} else {
		s.logf("request %s: redelivered, waiting for the seed %s it already sent", req.ID, seedID)
	}

	res, done := s.wait(ctx, req, seedID, r.To)
	switch {
	case !done:
		s.reply(ctx, req, fmt.Sprintf("Not passed yet: %s did not open the chat within %s. The new chat may still open in %s; the owner can check with tincan get %s.", r.To, s.replyWait(), history.SourceLabel(dst), seedID), envelope.StatusFailed)
		return
	case res.Status != envelope.StatusAnswered:
		why := string(res.Status)
		if res.Reply != nil {
			why, _ = textfit.Truncate(res.Reply.Body, maxForwardBytes)
		}
		// The seed went out, so the chat may have reached the site even
		// though the destination did not confirm it.
		s.logf("request %s: %s did not confirm the new chat: %s", req.ID, r.To, res.Status)
		s.reply(ctx, req, fmt.Sprintf("The chat was sent to %s, but it did not confirm the new chat (%s). %s", r.To, res.Status, why), envelope.StatusFailed)
		return
	}
	body := ""
	if res.Reply != nil {
		body = res.Reply.Body
	}
	newID, ok := NewConversation(body, dst)
	if !ok {
		s.reply(ctx, req, fmt.Sprintf("%s answered without naming its conversation; the owner can find the new chat in %s.", r.To, history.SourceLabel(dst)), envelope.StatusFailed)
		return
	}
	s.logf("request %s from %s: passed %s %s to %s (%s)", req.ID, req.From, r.Source, r.ID, r.To, newID)
	s.reply(ctx, req, fmt.Sprintf("Passed the %s conversation %s to %s. New %s: %s\nThe owner continues it in the browser; agents cannot continue it.",
		history.SourceLabel(r.Source), r.ID, r.To, history.ConversationName(dst), newID), envelope.StatusAnswered)
}

// sentSeed finds the seed this pass already sent under group, with its
// destination's site; "" when none was sent.
func (s *Service) sentSeed(ctx context.Context, group string, roster []client.AgentInfo, to string) (string, history.Source, error) {
	members, err := s.Relay.GroupMembers(ctx, group)
	if client.IsStatus(err, http.StatusNotFound) || (err == nil && len(members) == 0) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return members[0].ID, siteOf(roster, to), nil
}

// send checks the destination, reads the chat, checks it against the
// window, and sends the seed as a child of req under group. It returns
// the seed's id and the destination's site, or "" having replied.
func (s *Service) send(ctx context.Context, req envelope.Request, r Request, roster []client.AgentInfo, window history.Window, group string) (string, history.Source) {
	// The destination, before anything is read.
	dst, err := Destination(roster, r.To, s.now())
	if err != nil {
		s.reply(ctx, req, "Nothing was passed: "+err.Error()+".", envelope.StatusFailed)
		return "", ""
	}
	if err := Guarded(roster, r.To); err != nil {
		s.reply(ctx, req, "Nothing was passed: "+err.Error()+".", envelope.StatusFailed)
		return "", ""
	}

	rd, ok := s.Reader(r.Source)
	if !ok {
		s.reply(ctx, req, fmt.Sprintf("Nothing was passed: %s cannot be read on this machine.", history.SourceLabel(r.Source)), envelope.StatusFailed)
		return "", ""
	}
	conv, err := Read(ctx, rd, r.ID, window)
	if err != nil {
		s.logf("request %s: read %s %s: %v", req.ID, r.Source, r.ID, err)
		q := history.Query{Source: r.Source, Mode: history.ModeConversation, ConversationID: r.ID}
		s.reply(ctx, req, "Nothing was passed. "+history.ReadFailure(q, err), envelope.StatusFailed)
		return "", ""
	}
	if err := InWindow(conv, window, s.now()); err != nil {
		s.logf("request %s: %s %s: %v", req.ID, r.Source, r.ID, err)
		s.reply(ctx, req, "Nothing was passed: "+err.Error()+".", envelope.StatusDeclined)
		return "", ""
	}
	seed, err := Pack(conv, Budget(dst))
	if err != nil {
		s.reply(ctx, req, "Nothing was passed: "+err.Error()+".", envelope.StatusFailed)
		return "", ""
	}
	sent, err := s.Relay.SendGrouped(ctx, r.To, seed, envelope.KindAsk, req.ID, group)
	if err != nil {
		s.logf("request %s: send to %s: %v", req.ID, r.To, err)
		s.reply(ctx, req, fmt.Sprintf("Nothing was passed: the relay refused the ask to %s: %v.", r.To, err), envelope.StatusFailed)
		return "", ""
	}
	s.logf("request %s from %s: sent %s %s to %s as %s (%d bytes)", req.ID, req.From, r.Source, r.ID, r.To, sent.ID, len(seed))
	return sent.ID, dst
}

// wait waits for the seed's final state, renewing req's claim with a
// progress note every RenewEvery. done is false when ReplyWait ran out.
func (s *Service) wait(ctx context.Context, req envelope.Request, seedID, to string) (client.Result, bool) {
	every := s.RenewEvery
	if every <= 0 {
		every = DefaultRenewEvery
	}
	deadline := s.now().Add(s.replyWait())
	note := fmt.Sprintf("Waiting for %s to open the new chat", to)
	s.progress(ctx, req.ID, note)
	last := s.now()
	for {
		left := deadline.Sub(s.now())
		if left <= 0 || ctx.Err() != nil {
			return client.Result{}, false
		}
		res, err := s.Relay.Get(ctx, seedID, client.ClampWait(left))
		if err == nil && res.Done() {
			return res, true
		}
		if err != nil {
			s.logf("request %s: seed %s: %v", req.ID, seedID, err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
		if s.now().Sub(last) >= every {
			s.progress(ctx, req.ID, note)
			last = s.now()
		}
	}
}

func (s *Service) progress(ctx context.Context, id, note string) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)
	defer cancel()
	if err := s.Relay.Progress(pctx, id, note); err != nil {
		s.logf("request %s: progress note failed: %v", id, err)
	}
}

func (s *Service) reply(ctx context.Context, req envelope.Request, body string, status envelope.Status) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)
	defer cancel()
	if _, err := s.Relay.ReplyFinal(rctx, req.ID, body, status, nil, true); err != nil { // a service reply is the whole answer
		s.logf("request %s: reply failed: %v", req.ID, err)
	}
}

func (s *Service) window() (history.Window, error) {
	if s.WindowFile == "" {
		return history.Window{}, nil
	}
	return history.LoadWindow(s.WindowFile)
}

// InWindow checks that c was last updated inside the window's days
// (w's zero fields are the defaults). A chat whose time cannot be read is
// refused, since its age cannot be checked.
func InWindow(c history.Conversation, w history.Window, now time.Time) error {
	if w.MaxAge <= 0 {
		w.MaxAge = history.DefaultWindow().MaxAge
	}
	updated := lastUpdate(c)
	if updated.IsZero() {
		return errors.New("the chat's last update time could not be read, so it cannot be checked against the owner's history window")
	}
	if updated.Before(now.Add(-w.MaxAge)) {
		return fmt.Errorf("the chat was last updated %s, outside the owner's history window of %d days", updated.UTC().Format("2006-01-02"), int(w.MaxAge.Hours()/24))
	}
	return nil
}

// lastUpdate is c's last update time: its own, or its newest message's
// when that is later. Zero when neither could be read.
func lastUpdate(c history.Conversation) time.Time {
	updated := c.UpdatedAt
	for _, m := range c.Messages {
		if m.Time.After(updated) {
			updated = m.Time
		}
	}
	return updated
}

// siteOf is the site of the web chat agent to on the roster, "" when it is
// not one.
func siteOf(roster []client.AgentInfo, to string) history.Source {
	return agentSite(rosterAgent(roster, to))
}

// Guarded checks that the destination to runs GuardVersion or later, so
// it refuses continuations of the chat a pass opens. The error says what
// to upgrade.
func Guarded(roster []client.AgentInfo, to string) error {
	v := versionOf(roster, to)
	if client.AtLeast(v, GuardVersion) {
		return nil
	}
	if v == "" {
		v = "an unknown build"
	}
	return fmt.Errorf("%s runs tincan %s, which does not refuse agents' continuations of a passed chat. Upgrade it to %s or later (a release build) and restart it", to, v, GuardVersion)
}

// versionOf is the build agent to last called the relay with.
func versionOf(roster []client.AgentInfo, to string) string {
	if a := rosterAgent(roster, to); a != nil {
		return a.Version
	}
	return ""
}
