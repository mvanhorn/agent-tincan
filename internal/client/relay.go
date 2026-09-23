package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// AgentHeader names which of the calling machine's agents a request comes
// from. The relay honors it only for an agent bound to that machine.
const AgentHeader = "X-Tincan-Agent"

// Result mirrors the relay's view of one request.
type Result = envelope.Result

// MaxInlineWait caps every inline wait below common MCP tool-call timeouts.
const MaxInlineWait = 20 * time.Second

// ClampWait bounds d to [0, MaxInlineWait].
func ClampWait(d time.Duration) time.Duration { return min(max(d, 0), MaxInlineWait) }

// AgentInfo is one joined agent as the relay reports it.
type AgentInfo struct {
	Name     string    `json:"name"`
	Online   bool      `json:"online"`
	LastPoll time.Time `json:"last_poll,omitzero"`
	Wake     string    `json:"wake"`
	Kind     string    `json:"kind,omitempty"` // agent runtime (hermes, codex, ...), empty when unknown
}

// State is "online" or "offline".
func (a AgentInfo) State() string {
	if a.Online {
		return "online"
	}
	return "offline"
}

// APIError is a non-2xx response from the relay.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("relay: %s (HTTP %d)", e.Message, e.Code) }

// Relay talks to a tincan relay.
type Relay struct {
	base  string
	api   *http.Client
	polls *http.Client
	agent string // sent as AgentHeader when set
}

// NewRelay returns a client for the relay at base (for example
// "http://tincan-relay"). A non-empty proxy overrides HTTP_PROXY for relay
// traffic, which Muse needs because its default proxy cannot reach the
// tailnet.
func NewRelay(base, proxy string) (*Relay, error) {
	base = strings.TrimRight(base, "/")
	if _, err := url.Parse(base); err != nil || base == "" {
		return nil, fmt.Errorf("bad relay URL %q", base)
	}
	api, polls := New(APIClient), New(PollClient)
	if proxy != "" {
		pu, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("bad proxy URL: %w", err)
		}
		for _, c := range []*http.Client{api, polls} {
			if tr, ok := c.Transport.(*http.Transport); ok {
				tr.Proxy = http.ProxyURL(pu)
			}
		}
	}
	return &Relay{base: base, api: api, polls: polls}, nil
}

// NewRelayFor returns a client for a saved config. It names the configured
// agent on every call, so several agents on one machine (each with its own
// TINCAN_CONFIG) are told apart.
func NewRelayFor(c Config) (*Relay, error) {
	r, err := NewRelay(c.Relay, c.Proxy)
	if err != nil {
		return nil, err
	}
	r.agent = c.Agent
	return r, nil
}

// NewRelayHTTP builds a client over a caller-supplied http.Client (the
// gateway uses an in-process transport).
func NewRelayHTTP(base string, c *http.Client) *Relay {
	return &Relay{base: strings.TrimRight(base, "/"), api: c, polls: c}
}

// NewRelaySocket talks to the relay's local admin socket (on the relay host).
func NewRelaySocket(path string) *Relay {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
	c := &http.Client{Timeout: 30 * time.Second, Transport: tr}
	return &Relay{base: "http://tincan-admin", api: c, polls: c}
}

// Base is the relay URL this client talks to.
func (r *Relay) Base() string { return r.base }

// Send queues a request. parent is the request this one continues, or "".
func (r *Relay) Send(ctx context.Context, to, body string, kind envelope.Kind, parent string) (envelope.Request, error) {
	var out envelope.Request
	err := r.call(ctx, r.api, "POST", "/v1/send", map[string]any{"to": to, "body": body, "kind": kind, "parent_id": parent}, &out)
	return out, err
}

// Get returns a request's state, waiting up to wait for it to finish.
func (r *Relay) Get(ctx context.Context, id string, wait time.Duration) (Result, error) {
	var out Result
	c := r.api
	if wait > 0 {
		c = r.polls
	}
	err := r.call(ctx, c, "GET", fmt.Sprintf("/v1/requests/%s?wait=%d", url.PathEscape(id), int(wait.Seconds())), nil, &out)
	return out, err
}

// Ask sends a request and waits up to wait for the reply. If the reply is not
// in yet, the returned Result has the request id and a non-final status.
func (r *Relay) Ask(ctx context.Context, to, body, parent string, wait time.Duration) (Result, error) {
	req, err := r.Send(ctx, to, body, envelope.KindAsk, parent)
	if err != nil {
		return Result{}, err
	}
	if wait <= 0 {
		return Result{Request: req, Status: envelope.StatusQueued}, nil
	}
	return r.Get(ctx, req.ID, wait)
}

// Poll waits up to hold for requests addressed to this agent. An empty slice
// means nothing arrived in time.
func (r *Relay) Poll(ctx context.Context, hold time.Duration) ([]envelope.Request, error) {
	var out struct {
		Requests []envelope.Request `json:"requests"`
	}
	err := r.call(ctx, r.polls, "GET", fmt.Sprintf("/v1/poll?hold=%d", int(hold.Seconds())), nil, &out)
	return out.Requests, err
}

// Peek waits up to hold for requests without taking them, and returns how
// many are waiting. Listeners use it so the agent's own check still gets them.
func (r *Relay) Peek(ctx context.Context, hold time.Duration) (int, error) {
	var out struct {
		Waiting int `json:"waiting"`
	}
	err := r.call(ctx, r.polls, "GET", fmt.Sprintf("/v1/poll?peek=1&hold=%d", int(hold.Seconds())), nil, &out)
	return out.Waiting, err
}

// Claim marks a request as being handled by this agent.
func (r *Relay) Claim(ctx context.Context, id string) (envelope.Request, error) {
	var out envelope.Request
	err := r.call(ctx, r.api, "POST", "/v1/requests/"+url.PathEscape(id)+"/claim", nil, &out)
	return out, err
}

// Reply answers a request.
func (r *Relay) Reply(ctx context.Context, id, body string, status envelope.Status) (envelope.Reply, error) {
	var out envelope.Reply
	err := r.call(ctx, r.api, "POST", "/v1/requests/"+url.PathEscape(id)+"/reply", map[string]any{"body": body, "status": status}, &out)
	return out, err
}

// Cancel withdraws a request this agent sent.
func (r *Relay) Cancel(ctx context.Context, id string) error {
	return r.call(ctx, r.api, "POST", "/v1/requests/"+url.PathEscape(id)+"/cancel", nil, nil)
}

// Agents lists joined agents.
func (r *Relay) Agents(ctx context.Context) ([]AgentInfo, error) {
	var out struct {
		Agents []AgentInfo `json:"agents"`
	}
	err := r.call(ctx, r.api, "GET", "/v1/agents", nil, &out)
	return out.Agents, err
}

// Join binds this machine to the agent name behind code.
func (r *Relay) Join(ctx context.Context, code string) (string, error) {
	var out struct {
		Name string `json:"name"`
	}
	err := r.call(ctx, r.api, "POST", "/v1/join", map[string]string{"code": code}, &out)
	return out.Name, err
}

// Invite creates a one-time join code (admin devices only).
func (r *Relay) Invite(ctx context.Context, name string) (string, error) {
	return r.InviteKind(ctx, name, "")
}

// InviteKind creates a join code that also records the agent's kind (hermes,
// codex, ...), applied when the code is used. An empty kind is Invite.
func (r *Relay) InviteKind(ctx context.Context, name, kind string) (string, error) {
	var out struct {
		Code string `json:"code"`
	}
	in := map[string]string{"name": name}
	if kind != "" {
		in["kind"] = kind
	}
	err := r.call(ctx, r.api, "POST", "/v1/admin/invite", in, &out)
	return out.Code, err
}

// SetKind records a joined agent's kind; "" clears it (admin devices only).
func (r *Relay) SetKind(ctx context.Context, name, kind string) error {
	return r.call(ctx, r.api, "PUT", "/v1/agents/"+url.PathEscape(name)+"/kind", map[string]string{"kind": kind}, nil)
}

// Remove unbinds an agent (admin devices only).
func (r *Relay) Remove(ctx context.Context, name string) error {
	return r.call(ctx, r.api, "POST", "/v1/admin/remove", map[string]string{"name": name}, nil)
}

// Raw performs an arbitrary JSON call; used by commands that add endpoints
// (trace) without growing this client for each one.
func (r *Relay) Raw(ctx context.Context, method, path string, in, out any) error {
	return r.call(ctx, r.api, method, path, in, out)
}

func (r *Relay) call(ctx context.Context, c *http.Client, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.agent != "" {
		req.Header.Set(AgentHeader, r.agent)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return &APIError{Code: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// IsStatus reports whether err is a relay error with the given HTTP code.
func IsStatus(err error, code int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}
