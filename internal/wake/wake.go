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
// Wake messages carry only a count and an instruction, never request text.
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

// Options tunes a Waker.
type Options struct {
	Debounce     time.Duration // coalesce a burst into one nudge; default 3s
	RetryDelay   time.Duration // wait before the single retry; default 5s
	AgentMailAPI string        // default https://api.agentmail.to/v0
	HTTP         *http.Client
	Online       func(agent string) bool // skip wakes for agents already polling
	Now          func() time.Time
}

// Waker implements relay.Events and relay.WakeNamer.
type Waker struct {
	cfg   Config
	opts  Options
	audit *store.Store

	mu      sync.Mutex
	pending map[string]int         // requests waiting for the next nudge
	timers  map[string]*time.Timer // debounce timers
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
	if opts.AgentMailAPI == "" {
		opts.AgentMailAPI = "https://api.agentmail.to/v0"
	}
	if opts.HTTP == nil {
		opts.HTTP = client.New(client.APIClient)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Waker{cfg: cfg, opts: opts, audit: audit, pending: map[string]int{}, timers: map[string]*time.Timer{}, sent: map[string][]time.Time{}}
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
	t, ok := w.cfg[req.To]
	if !ok || (t.Method != Webhook && t.Method != Email) {
		return
	}
	if w.opts.Online != nil && w.opts.Online(req.To) {
		return // its poller already has it
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending[req.To]++
	if _, scheduled := w.timers[req.To]; scheduled {
		return
	}
	agent := req.To
	w.wg.Add(1)
	w.timers[agent] = time.AfterFunc(w.opts.Debounce, func() {
		defer w.wg.Done()
		w.fire(agent)
	})
}

// Flush waits for scheduled nudges (tests and shutdown).
func (w *Waker) Flush() { w.wg.Wait() }

func (w *Waker) fire(agent string) {
	w.mu.Lock()
	n := w.pending[agent]
	delete(w.pending, agent)
	delete(w.timers, agent)
	allowed := w.allow(agent)
	w.mu.Unlock()
	if n == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !allowed {
		w.record(ctx, "wake_skipped", agent, "hourly wake budget used up; requests stay queued")
		return
	}
	err := w.send(ctx, agent, n)
	if err != nil {
		time.Sleep(w.opts.RetryDelay)
		err = w.send(ctx, agent, n)
	}
	if err != nil {
		log.Printf("wake %s: %v", agent, err)
		w.record(ctx, "wake_failed", agent, err.Error())
		return
	}
	w.record(ctx, "woke", agent, fmt.Sprintf("%s, %d waiting", w.cfg[agent].Method, n))
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

// Message is the only text a wake ever carries.
func Message(n int) string {
	noun := "request"
	if n != 1 {
		noun = "requests"
	}
	return fmt.Sprintf("Agent Tincan: %d %s from your teammates waiting. Run check_inbox (or `tincan inbox`) to pick them up, then reply to each.", n, noun)
}

func (w *Waker) send(ctx context.Context, agent string, n int) error {
	t := w.cfg[agent]
	switch t.Method {
	case Webhook:
		// text mirrors message for runtimes that read text (OpenClaw /hooks/wake).
		msg := Message(n)
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
		body, _ := json.Marshal(map[string]any{"to": t.EmailTo, "subject": "Agent Tincan: requests waiting", "text": Message(n)})
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
