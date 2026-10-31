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
	"sync/atomic"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// AgentHeader names which of the calling machine's agents a request comes
// from. The relay honors it only for an agent bound to that machine.
const AgentHeader = "X-Tincan-Agent"

// HeldHeader is set on a relay response to an agent that holds claimed
// asks it has not replied to: a JSON list of envelope.Held. Older relays
// never send it and older clients ignore it.
const HeldHeader = "X-Tincan-Held"

// VersionHeader carries the tincan build the client runs on every relay
// call, so the roster can show which agents are behind.
const VersionHeader = "X-Tincan-Version"

// SessionHeader names the client session a call comes from, so several
// sessions sharing one agent (Claude Code windows on one machine, say) each
// get the replies to their own requests. It is a routing label inside the
// agent, never an identity. Old relays ignore it.
const SessionHeader = "X-Tincan-Session"

// ProjectHeader names the project the calling session works in, so an ask
// naming that project reaches it before the agent's other sessions. It is a
// routing label like SessionHeader, never an identity. Optional: a client
// that sends none takes such an ask after the fallback, and a relay that
// predates it ignores it.
const ProjectHeader = "X-Tincan-Project"

// FeaturesHeader advertises capabilities supported by this client.
const FeaturesHeader = "X-Tincan-Features"

// SocketWakeFeature is the FeaturesHeader value a client adds while it wakes
// its Claude Code session through the session's own socket, so the relay can
// tell an asker that the session will be prompted.
const SocketWakeFeature = "socket-wake"

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
	UpgradeAvailable string `json:"upgrade_available,omitempty"`
	Name             string `json:"name"`
	Online           bool   `json:"online"`
	// Status is the relay's one word for what the agent is doing (StatusBusy,
	// StatusIdle, StatusStuck, StatusLimited or StatusOffline), computed once
	// on the relay so every surface shows the same thing. StatusReason says
	// why a stuck agent is stuck ("claim lapsed") or until when a limited
	// one is limited ("until 14:00 UTC"); BusyFor names the asker whose request
	// a busy agent holds and BusySince when it claimed it. They never carry
	// request text. All are empty from relays that predate them.
	Status       string    `json:"status,omitempty"`
	StatusReason string    `json:"status_reason,omitempty"`
	BusyFor      string    `json:"busy_for,omitempty"`
	BusySince    time.Time `json:"busy_since,omitzero"`
	LastPoll     time.Time `json:"last_poll,omitzero"` // last long-poll since the relay started
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
	// WorkingOn is the title of the agent's most recently refreshed session
	// that shares its title (tincan mcp --share-title), and WorkingOnOthers
	// how many other sessions of the agent share one. The text is the
	// session's own description of itself, for reading only. Empty for an
	// agent that shares none, and from relays that predate it.
	WorkingOn       string `json:"working_on,omitempty"`
	WorkingOnOthers int    `json:"working_on_others,omitempty"`
	// Version is the tincan build the agent last called the relay with,
	// empty when it has not called since the relay learned to record it,
	// or runs a client that predates the version header.
	Version      string    `json:"version,omitempty"`
	Queued       int       `json:"queued,omitempty"`
	OldestQueued time.Time `json:"oldest_queued_at,omitzero"`
	Claimed      int       `json:"claimed,omitempty"`
	// Stats is the agent's record over the last 7 days, left out for an
	// agent with none and by relays that predate it.
	Stats AgentStats `json:"stats,omitzero"`
	// NodeID, NodeName and NodeUser name the Tailscale node the agent is
	// bound to: its stable node id, machine name, and owning login. The
	// relay fills them only for admin callers, and never for a virtual
	// agent (ChatGPT, Sesame), which has no node. Empty from older relays.
	NodeID   string `json:"node_id,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	NodeUser string `json:"node_user,omitempty"`
	// Self marks the caller's own entry, so an agent can tell itself from a
	// teammate when several of its windows share its name. The relay sets
	// it only on the entry of the agent the caller resolves to. False from
	// relays that predate it.
	Self bool `json:"self,omitempty"`
	// GuestUntil is when a guest's access ends. Only an admin's roster lists
	// guests, and whoami tells a guest its own. Zero for a teammate.
	GuestUntil time.Time `json:"guest_until,omitzero"`
	// Target carries the relay's schedule facts for an agent on wake method
	// schedule, or its last wake for a webhook or email agent the relay has
	// woken, and is left out otherwise and by older relays. Its fields sit
	// at the top level of each roster entry.
	envelope.Target
}

// AgentStats is an agent's record of asks finished in a window: counts and
// a duration, never text. Agent is set in a StatsReport and empty on a
// roster entry. MedianReplyMS is over answered asks, nil when none has a
// known reply time; a known median can be 0.
type AgentStats struct {
	Agent         string `json:"agent,omitempty"`
	Answered      int    `json:"answered"`
	Failed        int    `json:"failed"`
	Expired       int    `json:"expired"`
	MedianReplyMS *int64 `json:"median_reply_ms,omitempty"`
}

// StatsReport is GET /v1/stats: every joined agent's record for asks
// finished at or after Since.
type StatsReport struct {
	Since  time.Time    `json:"since"`
	Agents []AgentStats `json:"agents"`
}

// Finished counts the asks the record covers.
func (s AgentStats) Finished() int { return s.Answered + s.Failed + s.Expired }

// MedianReply is the median reply time for a person to read: "<1s", "45s",
// "3m", "2h". Empty when it is unknown.
func (s AgentStats) MedianReply() string {
	if s.MedianReplyMS == nil {
		return ""
	}
	d := time.Duration(*s.MedianReplyMS) * time.Millisecond
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return QueueAge(d)
}

// The statuses a relay reports in AgentInfo.Status. Busy means the agent
// holds live claimed work or its host says it is working; stuck means work
// waits on it and nothing is moving; limited means a web agent's site is
// out of messages until LimitedUntil; idle and offline split the rest by
// whether it is polling now.
const (
	StatusBusy    = "busy"
	StatusIdle    = "idle"
	StatusStuck   = "stuck"
	StatusLimited = "limited"
	StatusOffline = "offline"
)

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

// StatsField is the agent's record for a roster line, such as
// answered=12/13 median=3m, or "" for an agent with none.
func (a AgentInfo) StatsField() string {
	if a.Stats.Finished() == 0 {
		return ""
	}
	field := fmt.Sprintf("answered=%d/%d", a.Stats.Answered, a.Stats.Finished())
	if median := a.Stats.MedianReply(); median != "" {
		field += " median=" + median
	}
	return field
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
	session  string // sent as SessionHeader when set
	project  string // sent as ProjectHeader when set
	// socketWake adds SocketWakeFeature to FeaturesHeader. It is atomic
	// because the socket-wake loop flips it while other calls are in flight.
	socketWake atomic.Bool
	// roomsOK is set once the relay has advertised rooms (see roomCall).
	roomsOK atomic.Bool

	heldMu  sync.Mutex
	held    []envelope.Held // from the last response's HeldHeader
	heldSeq uint64          // responses seen, so a caller can tell a fresh list from an old one

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

	// proxy is the proxy relay traffic goes through, nil for the
	// environment's. proxyFile is the config a 407 reads again for fresher
	// credentials (see reloadProxy); "" reads nothing.
	proxyMu   sync.Mutex
	proxy     *url.URL
	proxyFile string

	// sentIDs are the requests this client sent, so a caller can tell a
	// reply to its own ask from one the relay shows every session.
	sentMu  sync.Mutex
	sentIDs map[string]bool

	// handbackAt is when Handback last read the relay's handback
	// capability, and handbackOK what it said.
	handbackMu sync.Mutex
	handbackAt time.Time
	handbackOK bool
}

// maxSentIDs bounds sentIDs. Past it the record starts over, and a reply
// to a forgotten ask is treated like one the relay shows every session.
const maxSentIDs = 4096

func (r *Relay) noteSent(id string) {
	if id == "" {
		return
	}
	r.sentMu.Lock()
	defer r.sentMu.Unlock()
	if r.sentIDs == nil || len(r.sentIDs) >= maxSentIDs {
		r.sentIDs = map[string]bool{}
	}
	r.sentIDs[id] = true
}

// SentHere reports whether this client sent request id.
func (r *Relay) SentHere(id string) bool {
	r.sentMu.Lock()
	defer r.sentMu.Unlock()
	return r.sentIDs[id]
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
	r := &Relay{base: base, api: New(APIClient), polls: New(PollClient), version: Version}
	if proxy != "" {
		pu, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("bad proxy URL: %w", err)
		}
		r.proxy = pu
	}
	for _, c := range []*http.Client{r.api, r.polls} {
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.Proxy = r.proxyFor
			tr.OnProxyConnectResponse = proxyConnectStatus
		}
	}
	return r, nil
}

// proxyFor is the relay transports' Proxy: the proxy this client was given
// or last reloaded, else the environment's.
func (r *Relay) proxyFor(req *http.Request) (*url.URL, error) {
	r.proxyMu.Lock()
	pu := r.proxy
	r.proxyMu.Unlock()
	if pu != nil {
		return pu, nil
	}
	return envProxy(req)
}

// envProxy is the environment's proxy for a request. Tests replace it: the
// standard library reads the environment only once.
var envProxy = http.ProxyFromEnvironment

// ThroughRemoteProxy says relay calls leave this machine through a proxy on
// another host, so the relay sees that host's tailnet node, not this
// machine's. A proxy on loopback stays here, as a userspace tailscaled's does.
// A proxy that cannot be read counts as remote.
func (r *Relay) ThroughRemoteProxy() bool {
	req, err := http.NewRequest("GET", r.Base(), nil)
	if err != nil {
		return true
	}
	pu, err := r.proxyFor(req)
	if err != nil {
		return true
	}
	if pu == nil || strings.EqualFold(pu.Hostname(), "localhost") {
		return false
	}
	ip := net.ParseIP(pu.Hostname())
	return ip == nil || !ip.IsLoopback()
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
// the environment overrides the saved relay for this process only. Relay
// traffic goes through c.DialProxy(), and a 407 reads path again for
// fresher proxy credentials.
func NewRelayForFile(c Config, path string) (*Relay, error) {
	r, err := NewRelay(c.Relay, c.DialProxy())
	if err != nil {
		return nil, err
	}
	r.proxyFile = path
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

// SendGrouped sends one request under group, a group id the caller chose
// rather than a fresh one (a pass's "pass-<id>", so a redelivered pass
// finds the seed it already sent), as a child of parent ("" lets the relay
// infer it). A relay that predates groups drops the group.
func (r *Relay) SendGrouped(ctx context.Context, to, body string, kind envelope.Kind, parent, group string) (envelope.SendResponse, error) {
	return r.send(ctx, map[string]any{"to": to, "body": body, "kind": kind, "parent_id": parent, "group": group})
}

// GroupMembers lists the requests this agent sent under group, oldest
// first, without waiting on them. The relay answers 404 for a group with
// none, and so does one that predates groups.
func (r *Relay) GroupMembers(ctx context.Context, group string) ([]envelope.GroupMember, error) {
	var members []envelope.GroupMember
	err := r.call(ctx, r.api, "GET", "/v1/groups/"+url.PathEscape(group), nil, &members)
	return members, err
}

// sent is the relay's send response.
type sent = envelope.SendResponse

func (r *Relay) send(ctx context.Context, in map[string]any) (sent, error) {
	if project := projectOf(ctx); project != "" {
		in["project"] = project
	}
	var out sent
	err := r.call(ctx, r.api, "POST", "/v1/send", in, &out)
	if err == nil {
		r.noteSent(out.ID)
	}
	return out, err
}

// asked finishes an ask whose send returned s: without a wait it reports
// the request as sent, otherwise it waits up to wait for the reply. Either
// way the send response's target facts are carried onto the result.
func (r *Relay) asked(ctx context.Context, s sent, wait time.Duration) (Result, error) {
	if wait <= 0 || (s.Target != nil && s.Target.SignedOutSite != "") {
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

// ErrThenUnsupported means the relay does not advertise follow-ons. Such a
// relay would silently drop the then field, so the client refuses to send.
var ErrThenUnsupported = errors.New("this relay does not support follow-on asks (upgrade the relay)")

// AskThen is Ask with a follow-on: once to answers, the relay sends then as
// a new ask from this agent, with the reply attached as data.
func (r *Relay) AskThen(ctx context.Context, to, body, parent string, then envelope.Then, wait time.Duration, urgent bool) (Result, error) {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("check relay capabilities: %w", err)
	}
	if !caps.Then {
		return Result{}, ErrThenUnsupported
	}
	s, err := r.send(ctx, map[string]any{"to": to, "body": body, "kind": envelope.KindAsk, "parent_id": parent, "urgent": urgent, "then": then})
	if err != nil {
		return Result{}, err
	}
	return r.asked(ctx, s, wait)
}

// ThenNote is what an asker is told about a follow-on it just queued, given
// the status its ask has come back with.
func ThenNote(to string, status envelope.Status) string {
	switch status {
	case envelope.StatusAnswered:
		return "This is answered, so the relay is sending your follow-on to " + to + ". If that is refused, you get a notice."
	case envelope.StatusFailed, envelope.StatusDeclined, envelope.StatusCancelled, envelope.StatusExpired:
		return "This ended without an answer, so your follow-on to " + to + " was dropped."
	}
	return "Once this is answered, the relay sends your follow-on to " + to + ". If that is refused, you get a notice. If this ends without an answer, the follow-on is dropped."
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
	// Hidden counts queued requests left out because they name another
	// project, so this peek does not show the agent's whole queue. A relay
	// that predates it leaves it zero.
	Hidden int `json:"hidden,omitempty"`
	// HiddenIDs names up to the first 50 of the Hidden requests, without
	// bodies, so a peek can tell one of them from a request that left the
	// queue.
	HiddenIDs []string `json:"hidden_ids,omitempty"`
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

// Stats reads every joined agent's record over the window since, such as
// 7d or 12h. An older relay answers its plain 404.
func (r *Relay) Stats(ctx context.Context, since string) (StatsReport, error) {
	var out StatsReport
	err := r.call(ctx, r.api, "GET", "/v1/stats?"+url.Values{"since": {since}}.Encode(), nil, &out)
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

// GuestInvite is what a guest invite asks the relay for.
type GuestInvite struct {
	Ask        []string `json:"ask"`
	ForSeconds int64    `json:"for_seconds"`
	NoHold     bool     `json:"no_hold,omitempty"`
}

// GuestInvited is the relay's answer to a guest invite. HoldAlerts says a
// held ask would reach the owner by a notify teammate or a push to a phone.
type GuestInvited struct {
	Code       string    `json:"code"`
	GuestUntil time.Time `json:"guest_until,omitzero"`
	GuestAsks  []string  `json:"guest_asks,omitempty"`
	HoldAlerts bool      `json:"hold_alerts,omitempty"`
}

// ErrGuestsUnsupported identifies a relay that would not keep a guest's
// limits. An older relay ignores them and mints a full teammate.
var ErrGuestsUnsupported = errors.New("this relay does not support guests (upgrade the relay); no code was issued")

// InviteGuest mints a join code for a guest (admin devices only). It asks
// first whether the relay knows guests, then checks that the relay echoed the
// limits, and returns no code if it did not.
func (r *Relay) InviteGuest(ctx context.Context, name string, in GuestInvite) (GuestInvited, error) {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return GuestInvited{}, fmt.Errorf("check relay capabilities: %w", err)
	}
	if !caps.Guests {
		return GuestInvited{}, ErrGuestsUnsupported
	}
	var out GuestInvited
	if err := r.call(ctx, r.api, "POST", "/v1/admin/invite", map[string]any{"name": name, "guest": in}, &out); err != nil {
		return GuestInvited{}, err
	}
	if out.GuestUntil.IsZero() {
		return GuestInvited{}, ErrGuestsUnsupported
	}
	return out, nil
}

// GuestConnected is the relay's answer to connecting a gateway guest. Cards
// are the A2A agent card URLs of the teammates it may ask, Unpublished the
// ones among them a2a.json does not publish yet.
type GuestConnected struct {
	Code        string    `json:"code"`
	Cards       []string  `json:"cards,omitempty"`
	Unpublished []string  `json:"unpublished,omitempty"`
	GuestUntil  time.Time `json:"guest_until,omitzero"`
	GuestAsks   []string  `json:"guest_asks,omitempty"`
	HoldAlerts  bool      `json:"hold_alerts,omitempty"`
}

// ConnectGuest binds a guest through the gateway and mints its login code
// (admin devices only), with InviteGuest's two checks: an older relay would
// ignore the limits and bind a full teammate.
func (r *Relay) ConnectGuest(ctx context.Context, name string, in GuestInvite) (GuestConnected, error) {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return GuestConnected{}, fmt.Errorf("check relay capabilities: %w", err)
	}
	if !caps.GatewayGuests {
		return GuestConnected{}, ErrGuestsUnsupported
	}
	var out GuestConnected
	if err := r.call(ctx, r.api, "POST", "/v1/admin/connect", map[string]any{"name": name, "guest": in}, &out); err != nil {
		return GuestConnected{}, err
	}
	if out.GuestUntil.IsZero() {
		return GuestConnected{}, ErrGuestsUnsupported
	}
	return out, nil
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

// ErrWatchesUnsupported identifies a relay that predates idle watches.
var ErrWatchesUnsupported = errors.New("relay does not support idle watches; upgrade the relay")

// WatchResult is the relay's answer to a watch: the agent watched and when
// the watch lapses if that agent is not idle by then.
type WatchResult struct {
	Agent     string    `json:"agent"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Text is what tincan watch and the watch tool print for w, the same on
// both: who is watched and when the watch lapses, on the local clock.
func (w WatchResult) Text(now time.Time) string {
	return fmt.Sprintf("You will get one relay notice when %s is next idle (or offline with nothing in progress). The watch lapses at %s if that has not happened.", w.Agent, clockTime(w.ExpiresAt, now))
}

// Watch asks the relay to send this agent one notice when agent is next
// idle (or offline with nothing in progress). Asking again while a watch is
// pending returns that watch. It checks the relay's capabilities first, so
// an older relay is refused with ErrWatchesUnsupported.
func (r *Relay) Watch(ctx context.Context, agent string) (WatchResult, error) {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return WatchResult{}, fmt.Errorf("check relay capabilities: %w", err)
	}
	if !caps.Watches {
		return WatchResult{}, ErrWatchesUnsupported
	}
	var out WatchResult
	err = r.call(ctx, r.api, "POST", "/v1/watches", map[string]string{"agent": agent}, &out)
	return out, err
}

// HandbackResult is what the relay did with a handed-back request:
// Outcome HandbackMoved, to the teammate To, HandbackRequeued, back on
// the caller's queue and not delivered again before Until, or
// HandbackFailed, closed failed by the relay because Until is after the
// request would expire (the asker has been told).
type HandbackResult struct {
	Outcome string    `json:"outcome"`
	To      string    `json:"to,omitempty"`
	Until   time.Time `json:"until,omitzero"`
}

// The outcomes of a handback.
const (
	HandbackMoved    = "moved"
	HandbackRequeued = "requeued"
	HandbackFailed   = "failed"
)

// ErrHandbackUnsupported means the relay predates handbacks: the web agent
// answers the request itself.
var ErrHandbackUnsupported = errors.New("this relay does not take handbacks (upgrade the relay)")

// handbackRecheck is how long a read of the relay's handback capability is
// trusted before Handback reads it again (the relay may be upgraded).
const handbackRecheck = 10 * time.Minute

// Handback gives back request id, which the caller (a web agent) claimed
// and did not send because its site is out of messages until until, so
// the relay can move it to another web agent. A 409 means the request may
// not move and is still the caller's to answer. It checks the relay's
// capabilities first, at most every handbackRecheck, so an older relay is
// refused with ErrHandbackUnsupported. A failed read is not kept.
func (r *Relay) Handback(ctx context.Context, id string, until time.Time) (HandbackResult, error) {
	ok, err := r.takesHandback(ctx)
	if err != nil {
		return HandbackResult{}, fmt.Errorf("check relay capabilities: %w", err)
	}
	if !ok {
		return HandbackResult{}, ErrHandbackUnsupported
	}
	var out HandbackResult
	err = r.call(ctx, r.api, "POST", "/v1/requests/"+url.PathEscape(id)+"/handback", map[string]any{"reason": envelope.WebLimited, "until": until.UTC()}, &out)
	return out, err
}

// takesHandback reports whether the relay advertises handbacks, from a
// read made within handbackRecheck when there is one.
func (r *Relay) takesHandback(ctx context.Context) (bool, error) {
	now := time.Now()
	r.handbackMu.Lock()
	if !r.handbackAt.IsZero() && now.Sub(r.handbackAt) < handbackRecheck {
		ok := r.handbackOK
		r.handbackMu.Unlock()
		return ok, nil
	}
	r.handbackMu.Unlock()
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return false, err
	}
	r.handbackMu.Lock()
	defer r.handbackMu.Unlock()
	r.handbackAt, r.handbackOK = now, caps.Handback
	return caps.Handback, nil
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
	resp, err := r.do(r.withTimeout(DistDownloadTimeout), func(int) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, "GET", r.Base()+"/v1/dist/"+url.PathEscape(name), nil)
	})
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

// noteHeld keeps the held claims a response reported, none when it carried
// no HeldHeader.
func (r *Relay) noteHeld(resp *http.Response) {
	var held []envelope.Held
	if v := resp.Header.Get(HeldHeader); v != "" {
		if err := json.Unmarshal([]byte(v), &held); err != nil {
			held = nil
		}
	}
	r.heldMu.Lock()
	r.held, r.heldSeq = held, r.heldSeq+1
	r.heldMu.Unlock()
}

// Held is what the relay's last response said this agent holds: asks it
// claimed and has not replied to. seq counts responses, so a caller can
// tell whether a call it made brought a fresh list.
func (r *Relay) Held() (held []envelope.Held, seq uint64) {
	r.heldMu.Lock()
	defer r.heldMu.Unlock()
	return r.held, r.heldSeq
}

// Raw performs an arbitrary JSON call; used by commands that add endpoints
// (trace) without growing this client for each one.
func (r *Relay) Raw(ctx context.Context, method, path string, in, out any) error {
	return r.call(ctx, r.api, method, path, in, out)
}

// socksRetry spaces the retries of a call whose SOCKS proxy could not
// connect to the relay, about 15 seconds in all: long enough for a sandbox's
// tailnet tunnel to come back after the sandbox wakes. Tests shorten it.
var socksRetry = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// call makes one relay call. A call the SOCKS proxy could not connect is
// made again on the socksRetry schedule while ctx allows; nothing reached
// the relay, so this is safe for a POST too. Any other failure, and a SOCKS
// failure that outlasts the schedule, goes on as before: a call that found
// nothing at the relay's address looks for a moved relay and, if it finds
// one, is made once more there.
//
// A call the proxy refused with 407 is made once more if the client now
// uses a different proxy than the refused attempt did (see proxyAuthRetry);
// otherwise, or if that fails too, it ends with a ProxyAuthError.
func (r *Relay) call(ctx context.Context, c *http.Client, method, path string, in, out any) error {
	err := r.proxyAuthRetry(func() error { return r.callOnce(ctx, c, method, path, in, out) })
	if IsProxyAuth(err) {
		return err
	}
	for _, delay := range socksRetry {
		if err == nil || !socksConnectFailed(err) {
			break
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		err = r.callOnce(ctx, c, method, path, in, out)
	}
	if err != nil && r.relocate(ctx, err) {
		return r.callOnce(ctx, c, method, path, in, out)
	}
	return err
}

// MaxResponseBytes is the most of a response a client reads. A longer one is
// cut off and fails to decode.
const MaxResponseBytes = 4 << 20

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
	r.noteHeld(resp)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
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
	features := "ping"
	if r.socketWake.Load() {
		features += "," + SocketWakeFeature
	}
	req.Header.Set(FeaturesHeader, features)
	if r.version != "" {
		req.Header.Set(VersionHeader, r.version)
	}
	req.Header.Set(PlatformHeader, runtime.GOOS+"_"+runtime.GOARCH)
	if r.session != "" {
		req.Header.Set(SessionHeader, r.session)
	}
	if r.project != "" {
		req.Header.Set(ProjectHeader, r.project)
	}
}

// SetSession names the session every later call comes from (SessionHeader);
// "" names none. Set it before the client is shared between goroutines.
func (r *Relay) SetSession(session string) { r.session = session }

// Session is the session set by SetSession.
func (r *Relay) Session() string { return r.session }

// SetProject names the project every later call comes from (ProjectHeader);
// "" names none. Set it before the client is shared between goroutines.
func (r *Relay) SetProject(project string) { r.project = project }

// projectKey carries the project a send names; see WithProject.
type projectKey struct{}

// WithProject returns a context whose sends name project: the ask is for a
// session of that project. Every way of sending an ask (plain, attached,
// notify, grouped) reads it, so none of them takes a parameter for it. ""
// names none.
func WithProject(ctx context.Context, project string) context.Context {
	return context.WithValue(ctx, projectKey{}, project)
}

func projectOf(ctx context.Context) string {
	project, _ := ctx.Value(projectKey{}).(string)
	return project
}

// AdvertiseSocketWake turns SocketWakeFeature on or off for every later
// call. It is safe to call while the client is in use.
func (r *Relay) AdvertiseSocketWake(on bool) { r.socketWake.Store(on) }

// ProxyAuthError is a relay call the proxy refused with HTTP 407: the proxy
// credentials have expired or are wrong, and the config offered nothing
// fresher. Its message names the proxy's host but never its credentials.
type ProxyAuthError struct {
	Proxy string // host:port, "" when the proxy came from the environment
	err   error
}

func (e *ProxyAuthError) Error() string {
	proxy := "the proxy"
	if e.Proxy != "" {
		proxy = "proxy " + e.Proxy
	}
	return proxy + " refused the relay call with HTTP 407 (Proxy Authentication Required): its credentials have expired or are wrong. " +
		"If this sandbox mints a new proxy password for every shell, run tincan rejoin with --proxy-credentials-from-env so each command takes the current password from HTTPS_PROXY, " +
		"or set TINCAN_PROXY to the proxy URL with current credentials (see docs/adapters/proxy-sandbox.md). " +
		"A running tincan wait, listen or MCP server keeps the password it started with: start it again from a shell with current credentials"
}

func (e *ProxyAuthError) Unwrap() error { return e.err }

// IsProxyAuth reports whether err is a ProxyAuthError.
func IsProxyAuth(err error) bool {
	_, ok := errors.AsType[*ProxyAuthError](err)
	return ok
}

// proxyAuthFailed reports whether the proxy refused a call with 407: as the
// response to a plain http:// relay call, or as the failed CONNECT of an
// https:// one. Go's transport reports a refused CONNECT as an error holding
// only the status text after the code, "Proxy Authentication Required", so
// that text is what is matched.
func proxyAuthFailed(err error) bool {
	return IsStatus(err, http.StatusProxyAuthRequired) || errors.Is(err, errProxyConnect407) ||
		(err != nil && strings.Contains(err.Error(), http.StatusText(http.StatusProxyAuthRequired)))
}

// errProxyConnect407 marks a CONNECT the proxy refused with 407, whatever
// reason phrase it gave.
var errProxyConnect407 = errors.New("proxy refused the tunnel: 407")

// proxyConnectStatus is the relay transports' OnProxyConnectResponse: it
// turns a 407 answer to CONNECT into errProxyConnect407, so the refusal is
// recognized by its status code rather than its wording.
func proxyConnectStatus(_ context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
	if res.StatusCode == http.StatusProxyAuthRequired {
		return errProxyConnect407
	}
	return nil
}

// proxyAuthRetry makes attempt and, if the proxy refused it with 407, reads
// the config again (reloadProxy) and makes it once more when the client now
// uses a different proxy than the refused attempt did. That is so whether
// this reload switched or a concurrent call's reload already had: either way
// the credentials that failed are no longer the ones in use. A 407 that
// stands ends as a ProxyAuthError. attempt must be safe to make twice.
func (r *Relay) proxyAuthRetry(attempt func() error) error {
	used := r.currentProxy()
	err := attempt()
	if !proxyAuthFailed(err) {
		return err
	}
	r.reloadProxy()
	if r.currentProxy() != used {
		err = attempt()
	}
	if proxyAuthFailed(err) {
		return &ProxyAuthError{Proxy: r.proxyHost(), err: err}
	}
	return err
}

// currentProxy is the proxy relay traffic now goes through, nil for the
// environment's. reloadProxy replaces it rather than changing it in place,
// so two reads are the same proxy exactly when they are the same pointer.
func (r *Relay) currentProxy() *url.URL {
	r.proxyMu.Lock()
	defer r.proxyMu.Unlock()
	return r.proxy
}

// reloadProxy reads the config file again after a 407 and switches to the
// proxy it names, if that differs from the one in use: a wrapper may have
// written fresh credentials there. A running process's own environment
// cannot have changed, so only the file (and TINCAN_PROXY) can offer
// anything new.
func (r *Relay) reloadProxy() {
	if r.proxyFile == "" {
		return
	}
	c, err := LoadConfigFrom(r.proxyFile)
	if err != nil || c.DialProxy() == "" {
		return
	}
	pu, err := url.Parse(c.DialProxy())
	if err != nil {
		return
	}
	r.proxyMu.Lock()
	defer r.proxyMu.Unlock()
	if r.proxy != nil && r.proxy.String() == pu.String() {
		return
	}
	r.proxy = pu
}

// proxyHost is the host:port of the proxy in use, "" for the environment's.
func (r *Relay) proxyHost() string {
	r.proxyMu.Lock()
	defer r.proxyMu.Unlock()
	if r.proxy == nil {
		return ""
	}
	return r.proxy.Host
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
	for _, targetName := range targets {
		var target *envelope.Target
		req := envelope.Request{To: targetName, Body: body, Kind: kind, ParentID: parent, Group: g.Group, Urgent: urgent, Project: projectOf(ctx)}
		ups, err := r.UploadFiles(ctx, attachPaths)
		if err == nil {
			for _, up := range ups {
				req.Attachments = append(req.Attachments, envelope.Attachment{ID: up.ID})
			}
			var sent envelope.SendResponse
			err = r.call(ctx, r.api, "POST", "/v1/send", req, &sent)
			if err == nil {
				req = sent.Request
				target = sent.Target
				r.noteSent(req.ID)
			}
		}
		res := Result{Request: req, Status: sentStatus(req), Target: target}
		if err != nil {
			res.Status = envelope.StatusFailed
			res.Reply = &envelope.Reply{From: targetName, Status: envelope.StatusFailed, Body: err.Error()}
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
			entry := GroupEntry{Request: envelope.Request{ID: member.ID, To: member.To}}
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
		if res.Request.ID == "" || (res.Target != nil && res.Target.SignedOutSite != "") {
			continue
		}
		wg.Go(func() {
			next, err := r.Get(ctx, res.Request.ID, ClampWait(wait))
			if err != nil {
				g.Results[i].Error = err.Error()
				return
			}
			next.Target = res.Target
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
