package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// AgentHeader names which of the calling machine's agents a request comes
// from. The relay honors it only for an agent bound to that machine.
const AgentHeader = "X-Tincan-Agent"

// VersionHeader carries the tincan build the client runs on every relay
// call, so the roster can show which agents are behind.
const VersionHeader = "X-Tincan-Version"

// FeaturesHeader advertises capabilities supported by this client.
const FeaturesHeader = "X-Tincan-Features"

// PlatformHeader carries the client's os_arch (for example darwin_arm64), so
// the relay only announces a release it holds a binary for.
const PlatformHeader = "X-Tincan-Platform"

// Version is the tincan build this process runs, sent as VersionHeader by
// every relay client made after it is set. main sets it from the link-time
// version; it stays "" (and the header is left out) in other programs.
var Version string

// Result mirrors the relay's view of one request.
type Result = envelope.Result

// MaxInlineWait caps every inline wait below common MCP tool-call timeouts.
const MaxInlineWait = 20 * time.Second

// ClampWait bounds d to [0, MaxInlineWait].
func ClampWait(d time.Duration) time.Duration { return min(max(d, 0), MaxInlineWait) }

// AgentInfo is one joined agent as the relay reports it.
type AgentInfo struct {
	UpgradeAvailable string    `json:"upgrade_available,omitempty"`
	Name             string    `json:"name"`
	Online           bool      `json:"online"`
	LastPoll         time.Time `json:"last_poll,omitzero"` // last long-poll since the relay started
	// LastActive is the agent's last call of any kind (send, reply, get,
	// poll), kept across relay restarts.
	LastActive time.Time `json:"last_active,omitzero"`
	Wake       string    `json:"wake"`
	Kind       string    `json:"kind,omitempty"` // agent runtime (hermes, codex, ...), empty when unknown
	// GoodAt is the line saying what the agent is good at: the owner's line,
	// else the stock line for a service or product-tool kind (or, with no
	// stored kind, for the product its name matches). Empty for hosting
	// shapes, Hermes and OpenClaw until the owner sets one, and from relays
	// that predate it.
	GoodAt string `json:"good_at,omitempty"`
	// Version is the tincan build the agent last called the relay with,
	// empty when it has not called since the relay learned to record it,
	// or runs a client that predates the version header.
	Version      string    `json:"version,omitempty"`
	Queued       int       `json:"queued,omitempty"`
	OldestQueued time.Time `json:"oldest_queued_at,omitzero"`
	Claimed      int       `json:"claimed,omitempty"`
	// Target carries the relay's schedule facts for an agent on wake method
	// schedule, or its last wake for a webhook or email agent the relay has
	// woken, and is left out otherwise and by older relays. Its fields sit
	// at the top level of each roster entry.
	envelope.Target
}

// Roster is the relay's agent list with what the relay says about itself.
type Roster struct {
	Agents []AgentInfo `json:"agents"`
	// RelayVersion is the tincan build the relay runs, "" from a relay
	// that predates it.
	RelayVersion string `json:"relay_version,omitempty"`
}

// State is "online" or "offline".
func (a AgentInfo) State() string {
	if a.Online {
		return "online"
	}
	return "offline"
}

// LastSeen says how long before now the agent last called the relay (the
// newer of LastPoll and LastActive), as "last seen 12m ago", or "never seen"
// for an agent that has not called the relay. A wait or listen loop that died
// shows up here as a growing age.
func (a AgentInfo) LastSeen(now time.Time) string {
	last := a.LastPoll
	if a.LastActive.After(last) {
		last = a.LastActive
	}
	if last.IsZero() {
		return "never seen"
	}
	return "last seen " + ageAgo(now.Sub(last))
}

func (a AgentInfo) Backlog(now time.Time) string {
	var parts []string
	if a.Queued > 0 {
		queued := fmt.Sprintf("%d queued", a.Queued)
		if !a.OldestQueued.IsZero() {
			queued += " (oldest " + QueueAge(now.Sub(a.OldestQueued)) + ")"
		}
		parts = append(parts, queued)
	}
	if a.Claimed > 0 {
		parts = append(parts, fmt.Sprintf("%d claimed", a.Claimed))
	}
	return strings.Join(parts, ", ")
}

// QueueAge is how long the oldest queued request has waited, in whole
// minutes, hours, or (from two days) days: "14m", "23h", "3d".
func QueueAge(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

// sentStatus is a fresh send's status: held when the relay is holding it for
// the owner's approval, queued otherwise.
func sentStatus(req envelope.Request) envelope.Status {
	if req.Status == envelope.StatusHeld {
		return envelope.StatusHeld
	}
	return envelope.StatusQueued
}

// DistManifest lists the release binaries a relay serves for tincan upgrade.
type DistManifest struct {
	Version string     `json:"version"`
	Files   []DistFile `json:"files"`
}

// DistFile is one release binary and its sha256, hex-encoded.
type DistFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// SHA256Of returns the checksum the manifest lists for name, or "".
func (m DistManifest) SHA256Of(name string) string {
	for _, f := range m.Files {
		if f.Name == name {
			return f.SHA256
		}
	}
	return ""
}

// APIError is a non-2xx response from the relay.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("relay: %s (HTTP %d)", e.Message, e.Code) }

// Relay talks to a tincan relay.
type Relay struct {
	groupsMu sync.Mutex
	groups   map[string]cachedGroup
	baseMu   sync.RWMutex
	base     string
	api      *http.Client
	polls    *http.Client
	agent    string // sent as AgentHeader when set
	version  string // sent as VersionHeader when set

	// key is the relay key from the saved config. When the relay stops
	// answering at base, the client looks for the peer that proves it
	// holds this key and moves there (see relocate).
	key   string
	known []string // relay's advertised addresses, tried first
	// configFile is the config file that relay info (LearnRelayKey) and a
	// found address (relocate) are written back to; "" writes nothing.
	configFile string
	findMu     sync.Mutex
	lastFind   time.Time
	finding    *relocation                                     // the running search; nil when none runs
	lastListed int                                             // IPv4 netmap addresses in the last FindRelay
	searched   bool                                            // a FindRelay has listed candidates
	lastSource string                                          // "localapi", "cli", "netmap", or ""
	findRelays func(ctx context.Context, base string) []string // tests replace it
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
	return &Relay{base: base, api: api, polls: polls, version: Version}, nil
}

// NewRelayFor returns a client for the saved config at ConfigPath(). It
// names the configured agent on every call, so several agents on one
// machine (each with its own TINCAN_CONFIG) are told apart. What the client
// learns about the relay (its key and addresses, a new address after a
// move) is written back to that file.
func NewRelayFor(c Config) (*Relay, error) {
	return NewRelayForFile(c, ConfigPath())
}

// NewRelayForFile is NewRelayFor for a config loaded from path (a service's
// --config), which is then the file relay info and a found address are
// written back to. With TINCAN_RELAY set nothing is written to any file:
// the environment overrides the saved relay for this process only.
func NewRelayForFile(c Config, path string) (*Relay, error) {
	r, err := NewRelay(c.Relay, c.Proxy)
	if err != nil {
		return nil, err
	}
	r.agent = c.Agent
	r.key = c.RelayKey
	r.known = c.RelayURLs
	if os.Getenv("TINCAN_RELAY") == "" {
		r.configFile = path
	}
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
	return &Relay{base: "http://tincan-admin", api: c, polls: c, version: Version}
}

// Base is the relay URL this client talks to.
func (r *Relay) Base() string {
	r.baseMu.RLock()
	defer r.baseMu.RUnlock()
	return r.base
}

// Send queues a request. parent is the request this one continues, or "".
// The urgent flag prioritizes time-critical requests.
func (r *Relay) Send(ctx context.Context, to, body string, kind envelope.Kind, parent string, urgent bool) (envelope.Request, error) {
	out, err := r.send(ctx, map[string]any{"to": to, "body": body, "kind": kind, "parent_id": parent, "urgent": urgent})
	return out.Request, err
}

// sent is the relay's send response.
type sent = envelope.SendResponse

func (r *Relay) send(ctx context.Context, in map[string]any) (sent, error) {
	var out sent
	err := r.call(ctx, r.api, "POST", "/v1/send", in, &out)
	return out, err
}

// asked finishes an ask whose send returned s: without a wait it reports
// the request as sent, otherwise it waits up to wait for the reply. Either
// way the send response's target facts are carried onto the result.
func (r *Relay) asked(ctx context.Context, s sent, wait time.Duration) (Result, error) {
	if wait <= 0 {
		return Result{Request: s.Request, Status: sentStatus(s.Request), Target: s.Target}, nil
	}
	res, err := r.Get(ctx, s.ID, wait)
	if err != nil {
		return res, err
	}
	res.Target = s.Target
	return res, nil
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
func (r *Relay) Ask(ctx context.Context, to, body, parent string, wait time.Duration, urgent bool) (Result, error) {
	s, err := r.send(ctx, map[string]any{"to": to, "body": body, "kind": envelope.KindAsk, "parent_id": parent, "urgent": urgent})
	if err != nil {
		return Result{}, err
	}
	return r.asked(ctx, s, wait)
}

// What a poll does with unseen replies to this agent's own requests. No
// poll marks a reply seen; a client that shows replies to its agent
// acknowledges them afterwards with AckReplies. A poll that names none of
// these (every client that predates replies) is treated as RepliesNone.
const (
	RepliesTake = "take" // return them for the agent to read, then AckReplies (check_inbox)
	RepliesKeep = "keep" // return them to count or announce, never acked (wait, peek)
	RepliesNone = "none" // leave them out; they do not end the hold (channel)
)

// Inbox is what one poll picked up: requests addressed to this agent, and
// replies to requests it sent that it has not seen yet.
type Inbox struct {
	UpgradeAvailable string             `json:"upgrade_available,omitempty"`
	Requests         []envelope.Request `json:"requests"`
	Replies          []Result           `json:"replies,omitempty"`
	// RepliesRemaining counts unseen replies left out of this poll to keep
	// the response small. They come with a later poll once these are acked.
	RepliesRemaining int `json:"replies_remaining,omitempty"`
}

// Empty reports whether nothing arrived.
func (in Inbox) Empty() bool { return len(in.Requests) == 0 && len(in.Replies) == 0 }

// ReplyIDs returns the request ids of the replies in the inbox, for
// AckReplies.
func (in Inbox) ReplyIDs() []string {
	ids := make([]string, len(in.Replies))
	for i, r := range in.Replies {
		ids[i] = r.Request.ID
	}
	return ids
}

// ReplyAcks identifies the exact reply generations returned by this poll.
func (in Inbox) ReplyAcks() []envelope.ReplyAck {
	acks := make([]envelope.ReplyAck, 0, len(in.Replies))
	for _, r := range in.Replies {
		if r.Reply != nil {
			acks = append(acks, envelope.ReplyAck{ID: r.Request.ID, Generation: r.Reply.Generation})
		}
	}
	return acks
}

// Waiting is what a peek saw without taking anything.
type Waiting struct {
	Pings            int    `json:"pings,omitempty"`
	UpgradeAvailable string `json:"upgrade_available,omitempty"`
	Total            int    `json:"waiting"` // queued requests plus unseen replies
	Queued           int    `json:"queued"`
	// Urgent counts the queued requests marked urgent, beyond the ones
	// Pending can list. A relay that predates it leaves it zero.
	Urgent  int      `json:"urgent,omitempty"`
	Replies []Result `json:"replies,omitempty"`
	// Pending names the oldest queued requests (id and sender, no body).
	// A relay that predates it leaves it empty.
	Pending []envelope.Pending `json:"pending,omitempty"`
}

// Poll waits up to hold for requests addressed to this agent or unseen
// replies to its own requests. It takes the requests. The replies stay
// unseen until the caller, having shown them to its agent, passes
// in.ReplyAcks() to AckReplies. An empty Inbox means nothing arrived in time.
func (r *Relay) Poll(ctx context.Context, hold time.Duration) (Inbox, error) {
	return r.PollReplies(ctx, hold, RepliesTake)
}

// PollReplies is Poll with a choice of what happens to unseen replies
// (RepliesTake, RepliesKeep, or RepliesNone).
func (r *Relay) PollReplies(ctx context.Context, hold time.Duration, replies string) (Inbox, error) {
	var out Inbox
	path := fmt.Sprintf("/v1/poll?hold=%d&replies=%s", int(hold.Seconds()), url.QueryEscape(replies))
	err := r.call(ctx, r.polls, "GET", path, nil, &out)
	return out, err
}

// AckReplies marks the replies to the requests in ids as seen, once the
// agent has been shown them. The relay ignores ids that are not this
// agent's own requests. Generation acknowledgements only mark the matching
// reply seen. Empty ids and acks make no call.
func (r *Relay) AckReplies(ctx context.Context, ids []string, acks ...envelope.ReplyAck) error {
	// A reply with no generation came from a relay that predates generations
	// (or was stored before it learned them). Acknowledge it by id, which
	// every relay understands; an older relay ignores the acks field.
	var gen []envelope.ReplyAck
	for _, ack := range acks {
		if ack.Generation == 0 {
			ids = append(ids, ack.ID)
		} else {
			gen = append(gen, ack)
		}
	}
	if len(ids) == 0 && len(gen) == 0 {
		return nil
	}
	body := map[string]any{"ids": ids}
	if len(gen) > 0 {
		body["acks"] = gen
	}
	return r.call(ctx, r.api, "POST", "/v1/replies/ack", body, nil)
}

// Peek waits up to hold for requests or unseen replies without taking
// either, and reports what is waiting. Listeners use it so the agent's own
// check still gets them.
func (r *Relay) Peek(ctx context.Context, hold time.Duration) (Waiting, error) {
	var out Waiting
	path := fmt.Sprintf("/v1/poll?peek=1&hold=%d&replies=%s", int(hold.Seconds()), RepliesKeep)
	err := r.call(ctx, r.polls, "GET", path, nil, &out)
	return out, err
}

// Claim marks a request as being handled by this agent.
func (r *Relay) Claim(ctx context.Context, id string) (envelope.Request, error) {
	var out envelope.Request
	err := r.call(ctx, r.api, "POST", "/v1/requests/"+url.PathEscape(id)+"/claim", nil, &out)
	return out, err
}

// Progress posts a note and renews this agent's claim lease.
func (r *Relay) Progress(ctx context.Context, id, note string) error {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("check relay capabilities: %w", err)
	}
	if !caps.Progress {
		return errors.New("this relay does not support progress notes (upgrade the relay)")
	}
	return r.call(ctx, r.api, "POST", "/v1/requests/"+url.PathEscape(id)+"/progress", map[string]string{"note": note}, nil)
}

// Reply answers a request.
func (r *Relay) Reply(ctx context.Context, id, body string, status envelope.Status) (envelope.Reply, error) {
	return r.ReplyAttached(ctx, id, body, status, nil)
}

// Cancel withdraws a request this agent sent.
func (r *Relay) Cancel(ctx context.Context, id string) error {
	return r.call(ctx, r.api, "POST", "/v1/requests/"+url.PathEscape(id)+"/cancel", nil, nil)
}

// Agents lists joined agents.
func (r *Relay) Agents(ctx context.Context) ([]AgentInfo, error) {
	ro, err := r.Roster(ctx)
	return ro.Agents, err
}

// Roster lists joined agents along with the relay's own build.
func (r *Relay) Roster(ctx context.Context) (Roster, error) {
	var out Roster
	err := r.call(ctx, r.api, "GET", "/v1/agents", nil, &out)
	return out, err
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

// SetGoodAt records the owner's line saying what an agent is good at; ""
// clears it (admin devices only). It returns the line as the relay stored
// it, trimmed, so "" means the line was cleared.
func (r *Relay) SetGoodAt(ctx context.Context, name, line string) (string, error) {
	var out struct {
		GoodAt string `json:"good_at"`
	}
	err := r.call(ctx, r.api, "PUT", "/v1/agents/"+url.PathEscape(name)+"/good-at", map[string]string{"good_at": line}, &out)
	return out.GoodAt, err
}

// Remove unbinds an agent (admin devices only).
func (r *Relay) Remove(ctx context.Context, name string) error {
	return r.call(ctx, r.api, "POST", "/v1/admin/remove", map[string]string{"name": name}, nil)
}

// Dist fetches the relay's release manifest.
func (r *Relay) Dist(ctx context.Context) (DistManifest, error) {
	var m DistManifest
	err := r.call(ctx, r.api, "GET", "/v1/dist", nil, &m)
	return m, err
}

// MaxDistBytes caps a release download. A var so tests can lower it.
var MaxDistBytes int64 = 512 << 20

// DistDownloadTimeout bounds one release download.
const DistDownloadTimeout = 10 * time.Minute

// DownloadDist streams the named release file from the relay into w.
func (r *Relay) DownloadDist(ctx context.Context, name string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, "GET", r.Base()+"/v1/dist/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	r.headers(req)
	resp, err := r.withTimeout(DistDownloadTimeout).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, MaxDistBytes+1))
	if err != nil {
		return err
	}
	if n > MaxDistBytes {
		return fmt.Errorf("%s is larger than %d bytes", name, MaxDistBytes)
	}
	return nil
}

// Raw performs an arbitrary JSON call; used by commands that add endpoints
// (trace) without growing this client for each one.
func (r *Relay) Raw(ctx context.Context, method, path string, in, out any) error {
	return r.call(ctx, r.api, method, path, in, out)
}

func (r *Relay) call(ctx context.Context, c *http.Client, method, path string, in, out any) error {
	err := r.callOnce(ctx, c, method, path, in, out)
	if err != nil && r.relocate(ctx, err) {
		return r.callOnce(ctx, c, method, path, in, out)
	}
	return err
}

func (r *Relay) callOnce(ctx context.Context, c *http.Client, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.Base()+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	r.headers(req)
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

// headers names this client's agent and build on a relay request.
func (r *Relay) headers(req *http.Request) {
	if r.agent != "" {
		req.Header.Set(AgentHeader, r.agent)
	}
	req.Header.Set(FeaturesHeader, "ping")
	if r.version != "" {
		req.Header.Set(VersionHeader, r.version)
	}
	req.Header.Set(PlatformHeader, runtime.GOOS+"_"+runtime.GOARCH)
}

// IsStatus reports whether err is a relay error with the given HTTP code.
func IsStatus(err error, code int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}

// GroupResult is the combined view of independently sent requests.
type GroupResult struct {
	Outcome string       `json:"outcome"`
	Group   string       `json:"group"`
	Results []GroupEntry `json:"results"`
}

// The local group record exists so a group can be followed up against a
// relay without the groups route. It is bounded: at most maxCachedGroups
// entries, each dropped once its requests would have expired on the relay.
const (
	maxCachedGroups = 64
	groupCacheTTL   = 24 * time.Hour
)

type cachedGroup struct {
	g  GroupResult
	at time.Time
}

// cachedGroupLocked returns the cached group id; r.groupsMu must be held.
func (r *Relay) cachedGroupLocked(id string) (GroupResult, bool) {
	c, ok := r.groups[id]
	if !ok || time.Since(c.at) > groupCacheTTL {
		delete(r.groups, id)
		return GroupResult{}, false
	}
	return c.g, true
}

// cacheGroupLocked stores g, dropping expired groups and then the oldest
// ones past maxCachedGroups; r.groupsMu must be held.
func (r *Relay) cacheGroupLocked(g GroupResult) {
	if r.groups == nil {
		r.groups = map[string]cachedGroup{}
	}
	at := time.Now()
	if c, ok := r.groups[g.Group]; ok {
		at = c.at // keep the send time; polls refresh the contents only
	}
	r.groups[g.Group] = cachedGroup{g: g, at: at}
	for id, c := range r.groups {
		if time.Since(c.at) > groupCacheTTL {
			delete(r.groups, id)
		}
	}
	for len(r.groups) > maxCachedGroups {
		oldest := ""
		for id, c := range r.groups {
			if oldest == "" || c.at.Before(r.groups[oldest].at) {
				oldest = id
			}
		}
		delete(r.groups, oldest)
	}
}

// GroupEntry retains a request's last known result and any polling error.
type GroupEntry struct {
	Result
	Error string `json:"error,omitempty"`
}

// ExitCode summarizes the group: pending takes precedence over failures.
func (g GroupResult) ExitCode() int {
	code := 0
	for _, r := range g.Results {
		if !r.Done() {
			return 2
		}
		if r.Status != envelope.StatusAnswered {
			code = 1
		}
	}
	return code
}

func (g *GroupResult) summarize() {
	g.Outcome = "answered"
	switch g.ExitCode() {
	case 1:
		g.Outcome = "failed"
	case 2:
		g.Outcome = "pending"
		for _, r := range g.Results {
			if r.Done() {
				g.Outcome = "partial"
				break
			}
		}
	}
}

// NormalizeTargets trims and deduplicates targets before any send or upload.
func NormalizeTargets(targets []string, self string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if !safeAgent.MatchString(target) || target == self {
			return nil, fmt.Errorf("invalid target %q (cannot ask yourself)", target)
		}
		if !seen[target] {
			out = append(out, target)
			seen[target] = true
		}
	}
	if len(out) == 0 || len(out) > 8 {
		return nil, errors.New("ask requires 1 to 8 distinct targets")
	}
	return out, nil
}

// newGroupID is a fresh group id for a batch of requests.
func newGroupID() string { return "group-" + rand.Text() }

// SendGroup sends ordinary requests, uploading separate attachments per target.
// Rejected sends remain visible as failed entries in this client's local record.
func (r *Relay) SendGroup(ctx context.Context, targets []string, body string, kind envelope.Kind, parent string, attachPaths []string, urgent bool) (GroupResult, error) {
	targets, err := NormalizeTargets(targets, r.agent)
	if err != nil {
		return GroupResult{}, err
	}
	g := GroupResult{Group: newGroupID()}
	for _, target := range targets {
		req := envelope.Request{To: target, Body: body, Kind: kind, ParentID: parent, Group: g.Group, Urgent: urgent}
		ups, err := r.UploadFiles(ctx, attachPaths)
		if err == nil {
			for _, up := range ups {
				req.Attachments = append(req.Attachments, envelope.Attachment{ID: up.ID})
			}
			var sent envelope.Request
			err = r.call(ctx, r.api, "POST", "/v1/send", req, &sent)
			if err == nil {
				req = sent
			}
		}
		res := Result{Request: req, Status: sentStatus(req)}
		if err != nil {
			res.Status = envelope.StatusFailed
			res.Reply = &envelope.Reply{From: target, Status: envelope.StatusFailed, Body: err.Error()}
		}
		g.Results = append(g.Results, GroupEntry{Result: res})
	}
	g.summarize()
	r.groupsMu.Lock()
	r.cacheGroupLocked(g)
	r.groupsMu.Unlock()
	return g, nil
}

// GetGroup resolves a relay group, falling back to this client's local record
// when talking to an older relay.
func (r *Relay) GetGroup(ctx context.Context, id string, wait time.Duration) (GroupResult, error) {
	r.groupsMu.Lock()
	g, local := r.cachedGroupLocked(id)
	r.groupsMu.Unlock()
	if local {
		g.Results = append([]GroupEntry(nil), g.Results...)
	}
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return g, err
	}
	if caps.Groups {
		var members []envelope.GroupMember
		err = r.call(ctx, r.api, "GET", "/v1/groups/"+url.PathEscape(id), nil, &members)
		if err != nil && (!local || !IsStatus(err, http.StatusNotFound)) {
			return g, err
		}
		g.Group = id
		for _, member := range members {
			found, replace := false, -1
			for i, entry := range g.Results {
				if entry.Request.ID == member.ID {
					found = true
					break
				}
				if entry.Request.To == member.To && entry.Request.ID == "" && entry.Status == envelope.StatusFailed {
					replace = i
				}
			}
			if found {
				continue
			}
			entry := GroupEntry{Result: Result{Request: envelope.Request{ID: member.ID, To: member.To}}}
			if replace >= 0 {
				g.Results[replace] = entry
			} else {
				g.Results = append(g.Results, entry)
			}
		}
	} else if !local {
		return g, errors.New("group unavailable: this older relay requires the original client process; use individual request ids")
	}
	return r.WaitGroup(ctx, g, wait)
}

// WaitGroup polls all requests concurrently within one shared wait budget.
func (r *Relay) WaitGroup(ctx context.Context, g GroupResult, wait time.Duration) (GroupResult, error) {
	g.Results = append([]GroupEntry(nil), g.Results...)
	var wg sync.WaitGroup
	for i, res := range g.Results {
		if res.Request.ID == "" {
			continue
		}
		wg.Go(func() {
			next, err := r.Get(ctx, res.Request.ID, ClampWait(wait))
			if err != nil {
				g.Results[i].Error = err.Error()
				return
			}
			g.Results[i] = GroupEntry{Result: next}
		})
	}
	wg.Wait()
	r.groupsMu.Lock()
	// A finished result is final: a late poll that still saw it pending must
	// not undo it. Unfinished states may move back (a lapsed lease requeues).
	prev, _ := r.cachedGroupLocked(g.Group)
	for i, res := range g.Results {
		if res.Request.ID == "" {
			continue
		}
		for _, cached := range prev.Results {
			if cached.Request.ID == res.Request.ID && (res.Error != "" || (cached.Done() && !res.Done())) {
				g.Results[i].Result = cached.Result
				break
			}
		}
	}
	g.summarize()
	cached := g
	cached.Results = append([]GroupEntry(nil), g.Results...)
	r.cacheGroupLocked(cached)
	r.groupsMu.Unlock()
	return g, nil
}
