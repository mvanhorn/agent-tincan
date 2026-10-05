// Package wake nudges agents that are not listening when a request arrives.
//
// Delivery never depends on wake: requests always wait in the relay queue.
// Wake only prompts an agent to go look. Findings from the live spike decide
// the methods:
//
//   - webhook (relay-side): POST a short prompt to a URL, with an optional
//     bearer token or HMAC signature. Grok Bot, Hermes. With format
//     "openclaw" the body is an OpenClaw gateway /hooks/agent payload.
//   - email (relay-side): send a short email through AgentMail. Instinct,
//     which wakes on email but cannot keep a background listener alive.
//   - wait (agent-side): the agent keeps `tincan wait` running in the
//     background and its runtime starts a turn when it exits. Muse.
//   - channel (agent-side): the tincan MCP server pushes requests into a
//     running Claude Code session.
//   - command (agent-side): `tincan listen --exec` runs a command.
//   - none: the agent checks at the start of each turn. ChatGPT.
//   - schedule (agent-side): the agent checks on its own cron every
//     interval; the relay reports the interval and a missed check.
//
// A reply to an agent's own request also wakes a relay-side agent, after a
// grace period that lets an inline wait read it first, and only if the reply
// is still unseen when the grace period ends. A reply still unseen after its
// nudge is nudged again on the ReplyRetries schedule. A request that stays
// queued after a relay-side wake, with no poll since that wake, is nudged
// again after WakeGrace, within MaxPerHour, until the agent polls, the
// queue is empty, or the agent is removed. While an urgent request is
// queued the follow-up comes after UrgentWakeGrace instead. A later 2xx
// does not move the last recorded wake while the agent is still silent.
// Each silent follow-up tells the relay (Options.Unanswered), which lets the
// askers know once per request.
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
	"crypto/rand"
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
	// Schedule (agent-side): the agent checks its inbox on its own cron
	// every Target.Every. The relay sends no wake; it reports the interval
	// and whether the agent has missed its checks.
	Schedule = "schedule"
)

// Webhook body formats.
const (
	// FormatGeneric (the default) posts source, message and text.
	FormatGeneric = ""
	// FormatOpenClaw posts an OpenClaw gateway POST /hooks/agent payload:
	// message, name, agentId and deliver, authenticated by the hook token
	// as a bearer header, with an Idempotency-Key the retry reuses.
	FormatOpenClaw = "openclaw"
)

// Target is one agent's wake settings in the relay-local config.
type Target struct {
	Method string `json:"method"`

	// webhook
	URL         string `json:"url,omitempty"`
	BearerToken string `json:"bearer_token,omitempty"`
	// HMACSecret signs the body as X-Hub-Signature-256 (GitHub scheme). Hermes.
	HMACSecret string `json:"hmac_secret,omitempty"`
	// Format selects the webhook body: FormatGeneric or FormatOpenClaw.
	Format string `json:"format,omitempty"`
	// AgentID is the OpenClaw agent id sent as agentId (format openclaw).
	// Empty lets the gateway resolve its default agent.
	AgentID string `json:"agent_id,omitempty"`
	// Deliver is OpenClaw's deliver flag (format openclaw). Nil means
	// false: the run's output is not announced, since the agent replies
	// through Agent Tincan.
	Deliver *bool `json:"deliver,omitempty"`

	// email (AgentMail)
	EmailTo       string `json:"email_to,omitempty"`
	AgentMailFrom string `json:"agentmail_inbox,omitempty"` // sending inbox, e.g. mvhgrokbot@agentmail.to
	AgentMailKey  string `json:"agentmail_key,omitempty"`

	// schedule: a Go duration such as "5m", the agent's own check interval.
	Every string `json:"every,omitempty"`

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
		if t.Format != FormatGeneric && (t.Method != Webhook || t.Format != FormatOpenClaw) {
			return nil, fmt.Errorf("wake %s: unknown format %q (webhook supports \"openclaw\")", name, t.Format)
		}
		switch t.Method {
		case Webhook:
			if t.URL == "" {
				return nil, fmt.Errorf("wake %s: webhook needs url", name)
			}
			if t.Format == FormatOpenClaw {
				if err := checkOpenClaw(t); err != nil {
					return nil, fmt.Errorf("wake %s: %w", name, err)
				}
			}
		case Email:
			if t.EmailTo == "" || t.AgentMailFrom == "" || t.AgentMailKey == "" {
				return nil, fmt.Errorf("wake %s: email needs email_to, agentmail_inbox, agentmail_key", name)
			}
		case Schedule:
			if t.Interval() <= 0 {
				return nil, fmt.Errorf("wake %s: schedule needs every, a positive duration such as \"5m\" (got %q)", name, t.Every)
			}
		case Wait, Channel, Command, None:
		default:
			return nil, fmt.Errorf("wake %s: unknown method %q", name, t.Method)
		}
	}
	return c, nil
}

// checkOpenClaw validates an OpenClaw hook target against what the gateway
// accepts: a hook token in a header (never the query string), no HMAC.
func checkOpenClaw(t Target) error {
	if t.BearerToken == "" {
		return errors.New("format openclaw needs bearer_token (the gateway's hooks.token)")
	}
	if t.HMACSecret != "" {
		return errors.New("format openclaw authenticates with bearer_token; OpenClaw does not check hmac_secret, remove it")
	}
	u, err := url.Parse(t.URL)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if u.Query().Has("token") {
		return errors.New("OpenClaw rejects a token query parameter; remove it from url and use bearer_token")
	}
	return nil
}

// DefaultWakeGrace is how long a relay-side request wake may go without a
// poll before the waker sends the same path again. It matches
// relay.DefaultWakeGrace.
const DefaultWakeGrace = 10 * time.Minute

// DefaultUrgentWakeGrace is WakeGrace while the agent has an urgent request
// queued: a time-critical ask is re-woken sooner. It matches
// relay.DefaultUrgentWakeGrace.
const DefaultUrgentWakeGrace = 2 * time.Minute

// DefaultReplyGrace is how long a reply may sit unread before its asker is
// woken for it.
const DefaultReplyGrace = time.Minute

// DefaultReplyRetries is the follow-up schedule for a reply that stays unseen
// after its nudge. A woken session may fail to read it (Hermes once woke
// while its MCP connection was still coming back), so the relay tries again. Each delay counts from the previous nudge.
var DefaultReplyRetries = []time.Duration{5 * time.Minute, 20 * time.Minute, time.Hour}

// Options tunes a Waker.
type Options struct {
	Debounce     time.Duration // coalesce a burst into one nudge; default 3s
	RetryDelay   time.Duration // wait before the single retry; default 5s
	AgentMailAPI string        // default https://api.agentmail.to/v0
	HTTP         *http.Client
	Online       func(agent string) bool // skip request wakes for agents already polling
	// Queued counts agent's requests still waiting to be delivered. With
	// it, a wake skipped because agent looked online is re-checked after
	// OnlineRecheck and sent if a request is still waiting: a session that
	// polled a moment ago may have ended without taking it.
	Queued        func(agent string) int
	OnlineRecheck time.Duration // default 30s
	// ReplyGrace is how long a reply may go unread before the asker is
	// woken; default DefaultReplyGrace.
	ReplyGrace time.Duration
	// UnseenReplies counts the replies agent has not read yet. A nudge
	// that finds none for a reply-only wake is dropped, since the asker
	// already read it inline.
	UnseenReplies func(agent string) int
	// ReplyRetries is the follow-up schedule after a nudge that finds
	// replies still unseen: each step waits its delay, re-checks
	// UnseenReplies, and nudges again only if some remain. The schedule
	// ends when the replies are seen or the steps run out. Nil means
	// DefaultReplyRetries; an empty slice turns follow-ups off.
	ReplyRetries []time.Duration
	// WakeGrace is how long after a request wake the waker waits to send
	// again while work is still queued and the agent has not polled;
	// default DefaultWakeGrace. A non-positive value after New turns
	// request follow-up off (tests).
	WakeGrace time.Duration
	// UrgentWakeGrace replaces WakeGrace for a follow-up armed while
	// UrgentQueued reports an urgent request still waiting, when it is
	// shorter; default DefaultUrgentWakeGrace. A negative value turns the
	// shorter grace off. Follow-ups share MaxPerHour either way.
	UrgentWakeGrace time.Duration
	// UrgentQueued counts agent's queued requests that are urgent. Nil
	// means none are.
	UrgentQueued func(agent string) int
	// Unanswered is told when a request follow-up fires and agent, woken at
	// wk, has still not polled and still has requests queued: a full grace
	// went by in silence. The relay uses it to tell those requests' askers.
	Unanswered func(agent string, wk store.Wake)
	// LastPoll is the agent's last inbox poll, including peek. A poll at
	// or after the last recorded wake is proof of life and stops request
	// follow-up. Nil is treated as never polled.
	LastPoll func(agent string) time.Time
	Now      func() time.Time
}

// nudge is one agent's pending wake.
type nudge struct {
	requests int         // requests queued since the last nudge
	replies  int         // replies landed since the last nudge
	timer    *time.Timer // fires the nudge
	due      time.Time   // when timer fires (wall clock)
	retry    int         // next step of Options.ReplyRetries to schedule
	recheck  bool        // a request wake was skipped as online; count Queued at fire time
	followUp bool        // --wake-grace request follow-up; poll/queue stop checks apply
	urgent   bool        // an urgent request was queued since the last nudge
}

// Waker implements relay.Events, relay.Requeuer, relay.Replier,
// relay.WakeNamer and relay.WakeReporter.
type Waker struct {
	cfg   Config
	opts  Options
	audit *store.Store

	mu      sync.Mutex
	pending map[string]*nudge      // agents with a nudge scheduled
	sent    map[string][]time.Time // relay-side wakes in the last hour
	// replyGen counts each agent's fresh replies. A nudge in flight
	// captures it, and its follow-up is dropped if a fresh reply arrived
	// meanwhile, since that reply restarted the schedule. It lives on the
	// Waker, not the nudge, because fire removes the nudge it sends.
	replyGen map[string]uint64
	// last is each relay-woken agent's last real wake send, kept in the
	// store too so a restarted relay can still tell a woken agent that
	// never checked in. Skipped wakes leave it alone.
	last map[string]store.Wake
	// removed is when each agent was last removed (Forget). A wake whose
	// send started before then belonged to the removed agent and is not
	// remembered when it finishes. Joined leaves this cutoff in place so
	// an in-flight send of the former agent is not recorded for the new
	// one.
	removed map[string]time.Time
	// unbound is set by Forget until Joined or Unforget. followUpLater
	// does not re-arm while the name is unbound.
	unbound map[string]struct{}
	// rememberMu makes keeping a last wake, store write included, atomic
	// with Forget, so a removal cannot slip between the check and the write.
	rememberMu sync.Mutex
	wg         sync.WaitGroup
	// stopped is set by Stop; no nudge is scheduled after it. ctx ends
	// the nudges in flight when Stop is called.
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
}

// New builds a Waker. audit may be nil.
func New(cfg Config, audit *store.Store, opts Options) *Waker {
	if opts.Debounce == 0 {
		opts.Debounce = 3 * time.Second
	}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = 5 * time.Second
	}
	if opts.OnlineRecheck == 0 {
		opts.OnlineRecheck = 30 * time.Second
	}
	if opts.ReplyGrace == 0 {
		opts.ReplyGrace = DefaultReplyGrace
	}
	if opts.ReplyRetries == nil {
		opts.ReplyRetries = DefaultReplyRetries
	}
	if opts.WakeGrace == 0 {
		opts.WakeGrace = DefaultWakeGrace
	}
	if opts.UrgentWakeGrace == 0 {
		opts.UrgentWakeGrace = DefaultUrgentWakeGrace
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
	last := map[string]store.Wake{}
	if audit != nil {
		ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
		if stored, err := audit.LastWakes(ctx); err != nil {
			log.Printf("load last wakes: %v", err)
		} else {
			last = stored
		}
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Waker{cfg: cfg, opts: opts, audit: audit, pending: map[string]*nudge{}, sent: map[string][]time.Time{}, replyGen: map[string]uint64{},
		last: last, removed: map[string]time.Time{}, unbound: map[string]struct{}{}, ctx: ctx, cancel: cancel}
}

// LastWake implements relay.WakeReporter: the last wake the relay sent
// agent, and whether there was one.
func (w *Waker) LastWake(agent string) (store.Wake, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	l, ok := w.last[agent]
	return l, ok
}

// Forget implements relay.WakeReporter: it drops agent's last wake, for an
// agent the owner removed, and the nudge still waiting for its timer. A wake
// already being sent is not remembered when it finishes. The relay calls it
// before deleting the agent, whose store row takes the wake row with it.
func (w *Waker) Forget(agent string) {
	w.rememberMu.Lock() // a last wake being written finishes first
	defer w.rememberMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removed[agent] = w.opts.Now()
	w.unbound[agent] = struct{}{}
	delete(w.last, agent)
	if p := w.pending[agent]; p != nil {
		if p.timer != nil && p.timer.Stop() {
			w.wg.Done() // the stopped timer's callback will never run
		}
		delete(w.pending, agent)
	}
	w.replyGen[agent]++ // a follow-up of a nudge in flight is dropped too
}

// Unforget implements relay.WakeReporter: it undoes Forget for an agent
// whose removal failed and so is still joined. Wakes are recorded for it
// again, and its last wake is reloaded from the store, which still holds it.
func (w *Waker) Unforget(agent string) {
	w.rememberMu.Lock()
	defer w.rememberMu.Unlock()
	var wk store.Wake
	var found bool
	if w.audit != nil {
		ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
		if stored, err := w.audit.LastWakes(ctx); err != nil {
			log.Printf("load last wake %s: %v", agent, err)
		} else {
			wk, found = stored[agent]
		}
		cancel()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.removed, agent)
	delete(w.unbound, agent)
	if old, ok := w.last[agent]; found && (!ok || wk.At.After(old.At)) {
		w.last[agent] = wk
	}
}

// Joined implements relay.WakeReporter: the name is bound again, so
// wake-grace follow-ups can be scheduled. The Forget cutoff stays, so an
// in-flight send that started before the removal is not recorded for this
// agent.
func (w *Waker) Joined(agent string) {
	w.rememberMu.Lock()
	defer w.rememberMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.unbound, agent)
}

// WakeMethod implements relay.WakeNamer: agents see only the method name.
func (w *Waker) WakeMethod(agent string) string {
	if t, ok := w.cfg[agent]; ok && t.Method != "" {
		return t.Method
	}
	return None
}

// Interval is a schedule target's check interval, 0 when Every is empty or
// does not parse.
func (t Target) Interval() time.Duration {
	d, err := time.ParseDuration(t.Every)
	if err != nil {
		return 0
	}
	return d
}

// CheckEvery implements relay.Scheduler: a schedule agent's check interval,
// 0 for an agent on any other method.
func (w *Waker) CheckEvery(agent string) time.Duration {
	if t, ok := w.cfg[agent]; ok && t.Method == Schedule {
		return max(t.Interval(), 0)
	}
	return 0
}

// Queued implements relay.Events. Relay-side methods schedule a debounced
// nudge; agent-side methods need nothing from the relay.
func (w *Waker) Queued(_ context.Context, req envelope.Request) {
	w.schedule(req.To, true, req.Urgent)
}

// Requeued implements relay.Requeuer. A requeue means the agent's own
// delivery or claim lease ran out, so a recent poll does not prove a poller
// holds the request; relay-side methods are nudged without the online check.
func (w *Waker) Requeued(_ context.Context, req envelope.Request) {
	w.schedule(req.To, false, req.Urgent)
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
	p := w.nudgeFor(agent)
	p.replies++
	p.retry = 0 // a fresh reply earns the full follow-up schedule
	w.replyGen[agent]++
	w.arm(agent, w.opts.ReplyGrace)
}

// RequestsWaiting schedules a debounced nudge for agent's queued requests.
// A restarted relay calls it for each agent that still has requests waiting,
// since a nudge the old process had scheduled died with it. The waiting
// requests are counted when the nudge fires, so one a poller took by then
// wakes nobody.
func (w *Waker) RequestsWaiting(agent string) {
	if !w.relaySide(agent) || w.opts.Queued == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.nudgeFor(agent)
	p.recheck = true
	p.followUp = false
	w.arm(agent, w.opts.Debounce)
}

// schedule debounces a relay-side nudge for agent. checkOnline skips agents
// whose poller already has the request.
func (w *Waker) schedule(agent string, checkOnline, urgent bool) {
	if !w.relaySide(agent) {
		return
	}
	if !urgent && checkOnline && w.opts.Online != nil && w.opts.Online(agent) {
		// Its poller should have it, but a session that just polled may be
		// ending: look again later and wake only if it is still waiting.
		if w.opts.Queued != nil {
			w.mu.Lock()
			defer w.mu.Unlock()
			p := w.nudgeFor(agent)
			p.recheck = true
			p.followUp = false
			w.arm(agent, w.opts.OnlineRecheck)
		}
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.nudgeFor(agent)
	p.requests++
	p.followUp = false
	p.urgent = p.urgent || urgent
	if urgent {
		w.arm(agent, 0)
	} else {
		w.arm(agent, w.opts.Debounce)
	}
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
	if w.stopped {
		return
	}
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

// Flush waits for scheduled nudges (tests).
func (w *Waker) Flush() { w.wg.Wait() }

// Stop drops the nudges still waiting for their timers, ends the ones being
// sent, and waits for them to return. Nothing is scheduled afterwards. The
// relay calls it on shutdown; a restarted relay schedules wakes again for
// every request still queued and every reply still unseen.
func (w *Waker) Stop() {
	w.mu.Lock()
	w.stopped = true
	for _, p := range w.pending {
		if p.timer != nil && p.timer.Stop() {
			w.wg.Done() // the stopped timer's callback will never run
		}
	}
	w.mu.Unlock()
	w.cancel()
	w.wg.Wait()
}

func (w *Waker) fire(agent string) {
	w.mu.Lock()
	p := w.pending[agent]
	delete(w.pending, agent)
	gen := w.replyGen[agent]
	w.mu.Unlock()
	if p == nil {
		return
	}
	if p.followUp {
		w.applyFollowUpStops(agent, p)
		if p.requests > 0 && w.opts.Unanswered != nil {
			if wk, ok := w.LastWake(agent); ok {
				w.opts.Unanswered(agent, wk)
			}
		}
	} else if p.recheck && w.opts.Queued != nil {
		// Count what is still waiting: zero when a poller took it, and
		// the whole backlog when requests arrived after the recheck was set.
		if n := w.opts.Queued(agent); p.requests == 0 || n > p.requests {
			p.requests = n
		}
	}
	replies := p.replies
	if w.opts.UnseenReplies != nil {
		replies = w.opts.UnseenReplies(agent)
	}
	if p.requests == 0 && replies == 0 {
		return // every reply was already read inline
	}
	if replies > 0 && (!p.followUp || p.retry > 0 || p.replies > 0) {
		// Whatever this nudge does, check back later in case the woken
		// session cannot read the replies. A request-only follow-up does
		// not start that ladder just because unseen replies remain.
		defer w.retryLater(agent, p.retry, gen)
	}
	ctx, cancel := context.WithTimeout(w.ctx, 2*time.Minute)
	defer cancel()
	w.mu.Lock()
	allowed := w.allow(agent)
	w.mu.Unlock()
	if !allowed {
		w.record(ctx, "wake_skipped", agent, "hourly wake budget used up; requests and replies stay queued")
		w.followUpLater(agent, p.requests)
		return
	}
	urgent := 0
	if p.requests > 0 && w.opts.UrgentQueued != nil {
		urgent = w.opts.UrgentQueued(agent)
	} else if p.requests > 0 && p.urgent {
		urgent = 1
	}
	msg := UrgentWaitingMessage(p.requests, urgent, replies)
	key := nudgeKey() // the retry reuses it, so a lost response never runs two turns
	at := w.opts.Now()
	err := w.send(ctx, agent, msg, key)
	if err != nil {
		select {
		case <-time.After(w.opts.RetryDelay):
			at = w.opts.Now()
			err = w.send(ctx, agent, msg, key)
		case <-ctx.Done():
		}
	}
	if err != nil {
		// The log, the audit row and the last wake (which joined agents can
		// read) all get the safe reason: the raw error can carry the URL.
		reason := publicReason(err)
		log.Printf("wake %s: %s", agent, reason)
		w.record(ctx, "wake_failed", agent, reason)
		w.rememberSend(ctx, agent, store.Wake{At: at, Result: reason}, false)
		w.followUpLater(agent, p.requests)
		return
	}
	w.rememberSend(ctx, agent, store.Wake{At: at, Result: envelope.WakeOK}, true)
	detail := fmt.Sprintf("%s, %d waiting", w.cfg[agent].Method, p.requests)
	if replies > 0 {
		detail += fmt.Sprintf(", %d unseen replies", replies)
	}
	w.record(ctx, "woke", agent, detail)
	w.followUpLater(agent, p.requests)
}

// applyFollowUpStops drops the request count on a --wake-grace follow-up
// when the queue is empty or the agent has polled since the last recorded
// wake. Reply counts are left alone so a coalesced reply ladder still fires.
func (w *Waker) applyFollowUpStops(agent string, p *nudge) {
	if w.answered(agent) {
		p.requests = 0
		return
	}
	if w.opts.Queued == nil {
		return
	}
	n := w.opts.Queued(agent)
	if n == 0 {
		p.requests = 0
		return
	}
	p.requests = n
}

// followUpLater re-arms a request wake at WakeGrace, or UrgentWakeGrace
// while an urgent request is queued, while the episode is still silent and
// queued. Caller must not hold w.mu.
func (w *Waker) followUpLater(agent string, requests int) {
	if requests == 0 || w.opts.WakeGrace <= 0 || !w.relaySide(agent) || w.opts.Queued == nil {
		return
	}
	if w.opts.Queued(agent) == 0 || w.answered(agent) {
		return
	}
	grace := w.followUpGrace(agent)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	if _, unbound := w.unbound[agent]; unbound {
		return
	}
	p := w.nudgeFor(agent)
	p.followUp = true
	w.arm(agent, grace)
}

// followUpGrace is how long the next request follow-up waits: WakeGrace,
// or UrgentWakeGrace when it is shorter and an urgent request is queued.
func (w *Waker) followUpGrace(agent string) time.Duration {
	if w.opts.UrgentWakeGrace <= 0 || w.opts.UrgentWakeGrace >= w.opts.WakeGrace || w.opts.UrgentQueued == nil {
		return w.opts.WakeGrace
	}
	if w.opts.UrgentQueued(agent) == 0 {
		return w.opts.WakeGrace
	}
	return w.opts.UrgentWakeGrace
}

// pollTime is agent's last poll, or zero when LastPoll is unset or it has
// never polled.
func (w *Waker) pollTime(agent string) time.Time {
	if w.opts.LastPoll == nil {
		return time.Time{}
	}
	return w.opts.LastPoll(agent)
}

// silent reports a last recorded wake that the agent has not polled since.
func (w *Waker) silent(agent string) bool {
	wk, ok := w.LastWake(agent)
	if !ok {
		return false
	}
	return w.pollTime(agent).Before(wk.At)
}

// answered reports a poll at or after the last recorded wake.
func (w *Waker) answered(agent string) bool {
	wk, ok := w.LastWake(agent)
	if !ok {
		return false
	}
	poll := w.pollTime(agent)
	return !poll.IsZero() && !poll.Before(wk.At)
}

// rememberSend keeps a send result. A later send while still silent keeps
// the first wake time so unanswered stays dated from the start of the
// episode. The result is updated: a failed follow-up is stored, and a
// later 2xx replaces that failure, so a restart does not keep showing the
// earlier error. A 2xx that is already ok is not written again. A send that
// started before the recorded wake is dropped: a slow failure must not
// overwrite a newer successful send.
func (w *Waker) rememberSend(ctx context.Context, agent string, wk store.Wake, okSend bool) {
	if old, ok := w.LastWake(agent); ok && wk.At.Before(old.At) {
		return
	}
	if w.silent(agent) {
		if old, ok := w.LastWake(agent); ok {
			wk.At = old.At
			if okSend && old.Result == envelope.WakeOK {
				return
			}
		}
	}
	w.remember(ctx, agent, wk)
}

// retryLater schedules step of the reply follow-up schedule for agent, which
// re-checks UnseenReplies when it fires. Without UnseenReplies the waker
// cannot tell a read reply from an unread one, so it never follows up. gen is
// agent's reply generation when the nudge fired; if a fresh reply has arrived
// since, it already restarted the schedule, so this stale step does nothing.
func (w *Waker) retryLater(agent string, step int, gen uint64) {
	if w.opts.UnseenReplies == nil || step >= len(w.opts.ReplyRetries) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.replyGen[agent] != gen {
		return
	}
	p := w.nudgeFor(agent)
	p.retry = max(p.retry, step+1)
	w.arm(agent, w.opts.ReplyRetries[step])
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
	return fmt.Sprintf("Agent Tincan: %d %s from your teammates waiting. Run check_inbox (or `tincan inbox`) before other work to pick them up, then reply to each.", n, plural(n, "request", "requests"))
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
	return fmt.Sprintf("Agent Tincan: %d %s from your teammates and %d %s waiting. Run check_inbox (or `tincan inbox`) before other work to read the %s and pick up the %s, then %s.",
		requests, plural(requests, "request", "requests"), replies, plural(replies, "reply to your request", "replies to your requests"),
		plural(replies, "reply", "replies"), plural(requests, "request", "requests"), then)
}

// UrgentWaitingMessage is WaitingMessage when urgent of the waiting
// requests are urgent: it says so and that they come before anything else
// this turn. It still carries only counts.
func UrgentWaitingMessage(requests, urgent, replies int) string {
	msg := WaitingMessage(requests, replies)
	if urgent <= 0 || requests <= 0 {
		return msg
	}
	switch {
	case requests == 1:
		return msg + " It is URGENT: run check_inbox first and handle it before anything else this turn."
	case urgent == 1:
		return msg + " 1 of them is URGENT: run check_inbox first and handle it before anything else this turn."
	}
	return msg + fmt.Sprintf(" %d of them are URGENT: run check_inbox first and handle them before anything else this turn.", min(urgent, requests))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// nudgeKey is a fresh idempotency key for one nudge.
func nudgeKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "agent-tincan-" + hex.EncodeToString(b)
}

// webhookBody is the JSON a webhook wake posts.
func webhookBody(t Target, msg string) []byte {
	if t.Format == FormatOpenClaw {
		// OpenClaw's POST /hooks/agent: message starts the turn, name labels
		// it in gateway logs, agentId routes it. deliver false keeps the
		// run's output out of the main session; the agent replies through
		// Agent Tincan. sessionMode is left at its default, isolated, which
		// matches a fresh-session agent. source lets hooks.mappings match on
		// it and text keeps a mistaken /hooks/wake URL working; the gateway
		// ignores both on /hooks/agent.
		b := map[string]any{"source": "agent-tincan", "message": msg, "text": msg, "name": "Agent Tincan", "deliver": t.Deliver != nil && *t.Deliver}
		if t.AgentID != "" {
			b["agentId"] = t.AgentID
		}
		body, _ := json.Marshal(b)
		return body
	}
	// text mirrors message for runtimes that read text.
	body, _ := json.Marshal(map[string]string{"source": "agent-tincan", "message": msg, "text": msg})
	return body
}

func (w *Waker) send(ctx context.Context, agent, msg, key string) error {
	t := w.cfg[agent]
	switch t.Method {
	case Webhook:
		body := webhookBody(t, msg)
		req, err := http.NewRequestWithContext(ctx, "POST", t.URL, bytes.NewReader(body))
		if err != nil {
			return &sendError{reason: "invalid webhook URL", err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		if t.Format == FormatOpenClaw {
			req.Header.Set("Idempotency-Key", key)
		}
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
			return &sendError{reason: "invalid AgentMail URL", err: err}
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
		return &sendError{reason: transportReason(req, err), err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		msg := fmt.Sprintf("%s returned %s", req.URL.Host, resp.Status)
		return &sendError{reason: msg, err: errors.New(msg)}
	}
	return nil
}

// sendError is a failed wake send: err in full for the relay's log, and
// reason, which names the host and the failure but never the URL's path,
// query or userinfo, for what agents and the audit log can see.
type sendError struct {
	reason string
	err    error
}

func (e *sendError) Error() string { return e.err.Error() }
func (e *sendError) Unwrap() error { return e.err }

// publicReason is the safe reason for a failed send: a webhook URL can
// carry a token in its query or userinfo, so a raw error never leaves the
// relay's log.
func publicReason(err error) string {
	if se, ok := errors.AsType[*sendError](err); ok {
		return se.reason
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err.Error()
	}
	return "wake send failed"
}

// transportReason is "POST to host failed: <cause>" for a send that got no
// response. The cause is the error inside the *url.Error, which holds the
// whole URL; what is left (a dial or TLS error, a timeout) names at most the
// host and port.
func transportReason(req *http.Request, err error) string {
	cause := err
	if ue, ok := errors.AsType[*url.Error](err); ok {
		cause = ue.Err
		// A redirect can leave a *url.Error for the target inside.
		if inner, ok := errors.AsType[*url.Error](cause); ok {
			cause = inner.Err
		}
	}
	return fmt.Sprintf("%s to %s failed: %v", req.Method, req.URL.Host, cause)
}

// auditTimeout bounds one wake audit write.
const auditTimeout = 10 * time.Second

// remember keeps wk as agent's last wake, in memory and in the store, unless
// a newer wake is already kept or the agent was removed after the send
// started. The time is when the send that decided the result started, so a
// poll the wake itself set off never predates it.
func (w *Waker) remember(ctx context.Context, agent string, wk store.Wake) {
	w.rememberMu.Lock()
	defer w.rememberMu.Unlock()
	w.mu.Lock()
	if gone, ok := w.removed[agent]; ok && !wk.At.After(gone) {
		w.mu.Unlock()
		return
	}
	if old, ok := w.last[agent]; ok && !wk.At.After(old.At) {
		if !wk.At.Equal(old.At) || wk.Result == old.Result {
			w.mu.Unlock()
			return
		}
	}
	w.last[agent] = wk
	w.mu.Unlock()
	if w.audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := w.audit.SetLastWake(ctx, agent, wk); err != nil {
		log.Printf("last wake %s: %v", agent, err)
	}
}

func (w *Waker) record(ctx context.Context, event, agent, detail string) {
	if w.audit == nil {
		return
	}
	// The audit write outlives a nudge cut short by Stop, within its own
	// bound so it cannot hold Stop up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := w.audit.Audit(ctx, store.AuditEvent{Event: event, Actor: agent, Detail: detail}); err != nil {
		log.Printf("audit %s: %v", event, err)
	}
}
