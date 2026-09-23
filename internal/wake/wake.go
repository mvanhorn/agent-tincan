// Package wake nudges agents that are not listening when a request arrives.
//
// Delivery never depends on wake: requests always wait in the relay queue.
// Wake only prompts an agent to go look. Findings from the live spike decide
// the methods:
//
//   - webhook (relay-side): POST a short prompt to a URL, with an optional
//     bearer token or HMAC signature. Grok Bot, Hermes, OpenClaw.
//   - email (relay-side): send a short email through AgentMail. Instinct,
//     which wakes on email but cannot keep a background listener alive.
//   - wait (agent-side): the agent keeps `tincan wait` running in the
//     background and its runtime starts a turn when it exits. Muse.
//   - channel (agent-side): the tincan MCP server pushes requests into a
//     running Claude Code session.
//   - command (agent-side): `tincan listen --exec` runs a command.
//   - none: the agent checks at the start of each turn. ChatGPT.
//
// A reply to an agent's own request also wakes a relay-side agent, after a
// grace period that lets an inline wait read it first, and only if the reply
// is still unseen when the grace period ends.
//
// Wake messages carry only counts and an instruction, never request or reply
// text.
// URLs, addresses, and keys live in the relay-local wake config and are never
// served to agents.
package wake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// Method names.
const (
	Webhook = "webhook"
	Email   = "email"
	Wait    = "wait"
	Channel = "channel"
	Command = "command"
	None    = "none"
)

// Target is one agent's wake settings in the relay-local config.
type Target struct {
	Method string `json:"method"`

	// webhook
	URL         string `json:"url,omitempty"`
	BearerToken string `json:"bearer_token,omitempty"`
	// HMACSecret signs the body as X-Hub-Signature-256 (GitHub scheme). Hermes.
	HMACSecret string `json:"hmac_secret,omitempty"`

	// email (AgentMail)
	EmailTo       string `json:"email_to,omitempty"`
	AgentMailFrom string `json:"agentmail_inbox,omitempty"` // sending inbox, e.g. mvhgrokbot@agentmail.to
	AgentMailKey  string `json:"agentmail_key,omitempty"`

	// MaxPerHour caps relay-side wakes (e.g. e2b resumes). Default 12.
	MaxPerHour int `json:"max_per_hour,omitempty"`
}

// Config maps agent name to Target.
type Config map[string]Target

// LoadConfig reads the relay-local wake config. A missing file means every
// agent is wake=none. The file must not be readable by other users.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s holds secrets and must be chmod 600 (is %o)", path, fi.Mode().Perm())
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, t := range c {
		switch t.Method {
		case Webhook:
			if t.URL == "" {
				return nil, fmt.Errorf("wake %s: webhook needs url", name)
			}
		case Email:
			if t.EmailTo == "" || t.AgentMailFrom == "" || t.AgentMailKey == "" {
				return nil, fmt.Errorf("wake %s: email needs email_to, agentmail_inbox, agentmail_key", name)
			}
		case Wait, Channel, Command, None:
		default:
			return nil, fmt.Errorf("wake %s: unknown method %q", name, t.Method)
		}
	}
	return c, nil
}

// DefaultReplyGrace is how long a reply may sit unread before its asker is
// woken for it.
const DefaultReplyGrace = time.Minute

// Options tunes a Waker.
type Options struct {
	Debounce     time.Duration // coalesce a burst into one nudge; default 3s
	RetryDelay   time.Duration // wait before the single retry; default 5s
	AgentMailAPI string        // default https://api.agentmail.to/v0
	HTTP         *http.Client
	Online       func(agent string) bool // skip request wakes for agents already polling
	// ReplyGrace is how long a reply may go unread before the asker is
	// woken; default DefaultReplyGrace.
	ReplyGrace time.Duration
	// UnseenReplies counts the replies agent has not read yet. A nudge
	// that finds none for a reply-only wake is dropped, since the asker
	// already read it inline.
	UnseenReplies func(agent string) int
	Now           func() time.Time
}

// nudge is one agent's pending wake.
type nudge struct {
	requests int         // requests queued since the last nudge
	replies  int         // replies landed since the last nudge
	timer    *time.Timer // fires the nudge
	due      time.Time   // when timer fires (wall clock)
}

// Waker implements relay.Events, relay.Requeuer, relay.Replier and
// relay.WakeNamer.
type Waker struct {
	cfg   Config
	opts  Options
	audit *store.Store

	mu      sync.Mutex
	pending map[string]*nudge      // agents with a nudge scheduled
	sent    map[string][]time.Time // relay-side wakes in the last hour
	wg      sync.WaitGroup
}

// New builds a Waker. audit may be nil.
func New(cfg Config, audit *store.Store, opts Options) *Waker {
	if opts.Debounce == 0 {
		opts.Debounce = 3 * time.Second
	}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = 5 * time.Second
	}
	if opts.ReplyGrace == 0 {
		opts.ReplyGrace = DefaultReplyGrace
	}
	if opts.AgentMailAPI == "" {
		opts.AgentMailAPI = "https://api.agentmail.to/v0"
	}
	if opts.HTTP == nil {
		opts.HTTP = client.New(client.APIClient)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Waker{cfg: cfg, opts: opts, audit: audit, pending: map[string]*nudge{}, sent: map[string][]time.Time{}}
}

// WakeMethod implements relay.WakeNamer: agents see only the method name.
func (w *Waker) WakeMethod(agent string) string {
	if t, ok := w.cfg[agent]; ok && t.Method != "" {
		return t.Method
	}
	return None
}

// Queued implements relay.Events. Relay-side methods schedule a debounced
// nudge; agent-side methods need nothing from the relay.
func (w *Waker) Queued(_ context.Context, req envelope.Request) {
	w.schedule(req.To, true)
}

// Requeued implements relay.Requeuer. A requeue means the agent's own
// delivery or claim lease ran out, so a recent poll does not prove a poller
// holds the request; relay-side methods are nudged without the online check.
func (w *Waker) Requeued(_ context.Context, req envelope.Request) {
	w.schedule(req.To, false)
}

// Replied implements relay.Replier: it schedules a nudge for the asker,
// req.From, once the reply grace period ends. There is no online check,
// because the asker's session may have ended seconds before the reply; the
// unseen count at fire time decides instead.
func (w *Waker) Replied(_ context.Context, req envelope.Request) {
	w.ReplyWaiting(req.From)
}

// ReplyWaiting schedules a reply nudge for agent once the reply grace period
// ends, like Replied. A restarted relay calls it for each agent that still
// holds unseen replies, since the old process kept its timers in memory.
func (w *Waker) ReplyWaiting(agent string) {
	if !w.relaySide(agent) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nudgeFor(agent).replies++
	w.arm(agent, w.opts.ReplyGrace)
}

// schedule debounces a relay-side nudge for agent. checkOnline skips agents
// whose poller already has the request.
func (w *Waker) schedule(agent string, checkOnline bool) {
	if !w.relaySide(agent) {
		return
	}
	if checkOnline && w.opts.Online != nil && w.opts.Online(agent) {
		return // its poller already has it
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nudgeFor(agent).requests++
	w.arm(agent, w.opts.Debounce)
}

// relaySide reports whether the relay itself wakes agent.
func (w *Waker) relaySide(agent string) bool {
	t, ok := w.cfg[agent]
	return ok && (t.Method == Webhook || t.Method == Email)
}

// nudgeFor returns agent's pending nudge, creating it. Caller holds w.mu.
func (w *Waker) nudgeFor(agent string) *nudge {
	p := w.pending[agent]
	if p == nil {
		p = &nudge{}
		w.pending[agent] = p
	}
	return p
}

// arm makes agent's nudge fire within d. A nudge already due sooner keeps
// its time, so a burst coalesces and a reply never delays a request; one due
// later is pulled in, so a request never waits out a reply's grace period.
// Caller holds w.mu.
func (w *Waker) arm(agent string, d time.Duration) {
	p := w.pending[agent]
	due := time.Now().Add(d)
	if p.timer != nil {
		if !due.Before(p.due) {
			return
		}
		if !p.timer.Stop() {
			return // already firing; it will pick up these counts
		}
		w.wg.Done() // the stopped timer's callback will never run
	}
	p.due = due
	w.wg.Add(1)
	p.timer = time.AfterFunc(d, func() {
		defer w.wg.Done()
		w.fire(agent)
	})
}

// Flush waits for scheduled nudges (tests and shutdown).
func (w *Waker) Flush() { w.wg.Wait() }

func (w *Waker) fire(agent string) {
	w.mu.Lock()
	p := w.pending[agent]
	delete(w.pending, agent)
	w.mu.Unlock()
	if p == nil {
		return
	}
	replies := p.replies
	if w.opts.UnseenReplies != nil {
		replies = w.opts.UnseenReplies(agent)
	}
	if p.requests == 0 && replies == 0 {
		return // every reply was already read inline
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w.mu.Lock()
	allowed := w.allow(agent)
	w.mu.Unlock()
	if !allowed {
		w.record(ctx, "wake_skipped", agent, "hourly wake budget used up; requests and replies stay queued")
		return
	}
	msg := WaitingMessage(p.requests, replies)
	err := w.send(ctx, agent, msg)
	if err != nil {
		time.Sleep(w.opts.RetryDelay)
		err = w.send(ctx, agent, msg)
	}
	if err != nil {
		log.Printf("wake %s: %v", agent, err)
		w.record(ctx, "wake_failed", agent, err.Error())
		return
	}
	detail := fmt.Sprintf("%s, %d waiting", w.cfg[agent].Method, p.requests)
	if replies > 0 {
		detail += fmt.Sprintf(", %d unseen replies", replies)
	}
	w.record(ctx, "woke", agent, detail)
}

// allow applies the hourly budget. Caller holds w.mu.
func (w *Waker) allow(agent string) bool {
	max := w.cfg[agent].MaxPerHour
	if max == 0 {
		max = 12
	}
	now := w.opts.Now()
	var recent []time.Time
	for _, t := range w.sent[agent] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= max {
		w.sent[agent] = recent
		return false
	}
	w.sent[agent] = append(recent, now)
	return true
}

// Message is the wake text for n waiting requests.
func Message(n int) string {
	return fmt.Sprintf("Agent Tincan: %d %s from your teammates waiting. Run check_inbox (or `tincan inbox`) to pick them up, then reply to each.", n, plural(n, "request", "requests"))
}

// WaitingMessage is the only text a wake ever carries: counts of waiting
// requests and of unseen replies to the agent's own requests, and what to
// run. It never includes request or reply content.
func WaitingMessage(requests, replies int) string {
	switch {
	case replies == 0:
		return Message(requests)
	case requests == 0 && replies == 1:
		return "Agent Tincan: 1 reply to your request is waiting. Run check_inbox (or `tincan inbox`) to read it."
	case requests == 0:
		return fmt.Sprintf("Agent Tincan: %d replies to your requests are waiting. Run check_inbox (or `tincan inbox`) to read them.", replies)
	}
	then := "reply to each"
	if requests == 1 {
		then = "reply to it"
	}
	return fmt.Sprintf("Agent Tincan: %d %s from your teammates and %d %s waiting. Run check_inbox (or `tincan inbox`) to read the %s and pick up the %s, then %s.",
		requests, plural(requests, "request", "requests"), replies, plural(replies, "reply to your request", "replies to your requests"),
		plural(replies, "reply", "replies"), plural(requests, "request", "requests"), then)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (w *Waker) send(ctx context.Context, agent, msg string) error {
	t := w.cfg[agent]
	switch t.Method {
	case Webhook:
		// text mirrors message for runtimes that read text (OpenClaw /hooks/wake).
		body, _ := json.Marshal(map[string]string{"source": "agent-tincan", "message": msg, "text": msg})
		req, err := http.NewRequestWithContext(ctx, "POST", t.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if t.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+t.BearerToken)
		}
		if t.HMACSecret != "" {
			m := hmac.New(sha256.New, []byte(t.HMACSecret))
			m.Write(body)
			req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		return w.do(req)
	case Email:
		// The subject stays fixed for replies too: standing instructions
		// match on it.
		body, _ := json.Marshal(map[string]any{"to": t.EmailTo, "subject": "Agent Tincan: requests waiting", "text": msg})
		u := fmt.Sprintf("%s/inboxes/%s/messages/send", w.opts.AgentMailAPI, url.PathEscape(t.AgentMailFrom))
		req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+t.AgentMailKey)
		return w.do(req)
	}
	return nil
}

func (w *Waker) do(req *http.Request) error {
	resp, err := w.opts.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", req.URL.Host, resp.Status)
	}
	return nil
}

func (w *Waker) record(ctx context.Context, event, agent, detail string) {
	if w.audit == nil {
		return
	}
	if err := w.audit.Audit(ctx, store.AuditEvent{Event: event, Actor: agent, Detail: detail}); err != nil {
		log.Printf("audit %s: %v", event, err)
	}
}
