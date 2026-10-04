// Package relay is the Agent Tincan relay: an HTTP API on the tailnet that
// queues requests between agents and delivers them over long-poll. Every call
// is attributed to an agent by the tailnet node it came from.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// Config tunes the relay.
type Config struct {
	RequestTTL      time.Duration // how long an unanswered request lives
	NotesRequestTTL time.Duration // RequestTTL for requests to a notes-kind agent
	PollHold        time.Duration // max time a long-poll is held
	DeliveryLease   time.Duration // how long a delivered request waits for a claim
	ClaimLease      time.Duration // how long a claim lasts before the request is requeued
	MaxWait         time.Duration // cap on get-reply waits
	SweepEvery      time.Duration
	Now             func() time.Time
	Attachments     AttachmentConfig
	// Version is this relay's tincan build, reported to agents in the
	// roster and whoami so a client behind it stands out; "" hides it.
	Version string
	// WakeGrace is how long a relay-woken agent may go without polling
	// after a wake before it shows as unanswered; default DefaultWakeGrace.
	WakeGrace time.Duration
}

func (c *Config) defaults() {
	if c.RequestTTL == 0 {
		c.RequestTTL = 24 * time.Hour
	}
	if c.NotesRequestTTL == 0 {
		c.NotesRequestTTL = 30 * 24 * time.Hour
	}
	if c.PollHold == 0 {
		c.PollHold = client.DefaultPollHold
	}
	if c.DeliveryLease == 0 {
		c.DeliveryLease = 2 * time.Minute
	}
	if c.ClaimLease == 0 {
		c.ClaimLease = 30 * time.Minute
	}
	if c.MaxWait == 0 {
		c.MaxWait = client.DefaultPollHold
	}
	if c.SweepEvery == 0 {
		c.SweepEvery = 5 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.WakeGrace == 0 {
		c.WakeGrace = DefaultWakeGrace
	}
	c.Attachments.defaults()
}

// Preparer fills in a new request's chain fields and applies policy. The
// default starts a new chain; package policy supplies the real one.
type Preparer interface {
	Prepare(ctx context.Context, req *envelope.Request) error
}

// Refunder is an optional Preparer that gives back what Prepare took (the
// urgent allowance) when the relay then fails to queue the request.
type Refunder interface {
	Refund(req envelope.Request)
}

// Events lets other parts of the relay react to queue changes (wake, audit).
type Events interface {
	Queued(ctx context.Context, req envelope.Request)
}

// Requeuer is an optional Events extension for requests the sweep returned to
// the queue after a lease ran out. Without it the sweep falls back to Queued.
type Requeuer interface {
	Requeued(ctx context.Context, req envelope.Request)
}

// Replier is an optional Events extension told when a request gets its
// reply, so the asker can be woken to read it. req.From is the asker.
type Replier interface {
	Replied(ctx context.Context, req envelope.Request)
}

// Server is the relay.
type Server struct {
	cfg Config
	dir *identity.Directory
	// lookupAgent is how requestTTL reads the recipient's kind; it is
	// dir.Agent, replaced in tests to simulate a failed lookup.
	lookupAgent func(ctx context.Context, name string) (identity.Agent, bool, error)
	store       *store.Store
	hub         *hub
	prep        Preparer
	events      Events
	wake        WakeNamer
	conn        Connector
	dist        *dist
	blobs       string   // attachment directory, "" when attachments are off
	key         string   // relay key, proves this relay's identity to its agents (hello)
	urls        []string // addresses advertised to agents in whoami

	// upgrader installs the dist release over the relay binary; nil when off.
	upgrader SelfUpgrader

	mu       sync.Mutex
	lastPoll map[string]time.Time
	polling  map[string]int // long-polls currently held open, per agent
	// lastSeen is each agent's last call of any kind; persisted is when it
	// was last written to the store, which happens at most once per
	// persistEvery per agent.
	lastSeen  map[string]time.Time
	persisted map[string]time.Time
	// versions is the tincan build each agent last called with, from the
	// client's version header, loaded from the store at start and written
	// back whenever it changes.
	versions     map[string]string
	pollFeatures map[string]pollFeatures
	// started is when this relay process started. lastPoll is not
	// persisted, so a schedule agent that has not polled since then is
	// measured from here rather than flagged overdue by a restart.
	started time.Time
	// pollMu orders each poll-feature update with its store write, so two
	// overlapping polls persist in the order they changed the state.
	pollMu sync.Mutex
	// storedVersion is the build last written to the store for each agent,
	// and versionWritten when. The write is throttled like last-seen, so two
	// builds running under one name (an old listen or MCP process next to an
	// upgraded CLI) do not write to the store on every alternating call.
	storedVersion  map[string]string
	versionWritten map[string]time.Time

	// stopping is closed by Stop, when the relay begins to shut down.
	stopping chan struct{}
	stopOnce sync.Once
}

// persistEvery bounds how often an agent's activity is written to the store.
const persistEvery = time.Minute

// legacyPollWindow keeps mixed-version receivers from accepting unanswerable pings.
const legacyPollWindow = 24 * time.Hour

// pollFeaturesPersistEvery bounds how often a legacy poller's last-seen time
// is written; the in-memory value is always current.
const pollFeaturesPersistEvery = time.Hour

// pollFeatures intentionally excludes advertisements from non-poll calls.
// The legacy features column cannot establish whether a receiver supports ping.
type pollFeatures struct {
	Ping            bool
	LastUnsupported time.Time
}

// New builds a relay server.
func New(dir *identity.Directory, st *store.Store, cfg Config) *Server {
	cfg.defaults()
	s := &Server{cfg: cfg, dir: dir, store: st, hub: newHub(), prep: newChain{}, lastPoll: map[string]time.Time{}, polling: map[string]int{},
		lastSeen: map[string]time.Time{}, persisted: map[string]time.Time{}, versions: loadVersions(st), blobs: defaultAttachmentDir(st), key: loadRelayKey(st),
		versionWritten: map[string]time.Time{}, stopping: make(chan struct{}), started: cfg.Now()}
	s.lookupAgent = dir.Agent
	s.storedVersion = maps.Clone(s.versions)
	s.pollFeatures = map[string]pollFeatures{}
	states, err := st.PollFeatures(context.Background())
	if err != nil {
		log.Printf("poll features: %v", err)
	}
	for name, state := range states {
		var f pollFeatures
		if json.Unmarshal([]byte(state), &f) == nil {
			s.pollFeatures[name] = f
		}
	}
	return s
}

// loadVersions reads the build each agent last reported, so a restarted
// relay's roster shows them before the agents call again. A failed read is
// logged; versions are then learned again as agents call.
func loadVersions(st *store.Store) map[string]string {
	if st == nil {
		return map[string]string{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	vs, err := st.AgentVersions(ctx)
	if err != nil {
		log.Printf("agent versions: %v", err)
		return map[string]string{}
	}
	return vs
}

// versionRE is what a client's build name may look like before the relay
// records it: git describe output such as 0.5.2 or 0.5.2-3-gabcdef-dirty.
var versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// cleanVersion returns v when it is a plausible build name, else "".
func cleanVersion(v string) string {
	v = strings.TrimSpace(v)
	if !versionRE.MatchString(v) {
		return ""
	}
	return v
}

// SetPreparer installs the chain and policy step.
func (s *Server) SetPreparer(p Preparer) { s.prep = p }

// Connector links virtual agents (ChatGPT through the gateway).
type Connector interface {
	// Connect binds the virtual agent and returns a one-time login code and
	// the public URL to add as a connector.
	Connect(ctx context.Context, name string) (code, url string, err error)
	// Revoke drops every token the agent holds.
	Revoke(ctx context.Context, name string) error
}

// SetConnector enables `tincan connect`.
func (s *Server) SetConnector(c Connector) { s.conn = c }

// SetEvents installs queue event listeners.
func (s *Server) SetEvents(e Events) { s.events = e }

type newChain struct{}

func (newChain) Prepare(_ context.Context, req *envelope.Request) error {
	req.Hop, req.Chain, req.TraceID = 1, []string{req.From}, ""
	return nil
}

type ctxKey int

const localAdminKey ctxKey = 1

// Handler is the agent API served on the tailnet.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/send", s.handleSend)
	mux.HandleFunc("GET /v1/poll", s.handlePoll)
	mux.HandleFunc("POST /v1/replies/ack", s.handleAckReplies)
	mux.HandleFunc("POST /v1/requests/{id}/claim", s.handleClaim)
	mux.HandleFunc("POST /v1/requests/{id}/progress", s.handleProgress)
	mux.HandleFunc("POST /v1/requests/{id}/reply", s.handleReply)
	mux.HandleFunc("POST /v1/requests/{id}/answer", s.handleAnswer)
	mux.HandleFunc("GET /v1/requests/{id}", s.handleGet)
	mux.HandleFunc("GET /v1/groups/{id}", s.handleGroup)
	mux.HandleFunc("POST /v1/requests/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /v1/agents", s.handleAgents)
	mux.HandleFunc("GET /v1/whoami", s.handleWhoAmI)
	mux.HandleFunc("GET /v1/hello", s.handleHello)
	mux.HandleFunc("POST /v1/join", s.handleJoin)
	mux.HandleFunc("GET /v1/dist", s.handleDistManifest)
	mux.HandleFunc("GET /v1/dist/{name}", s.handleDistFile)
	mux.HandleFunc("POST "+uploadPath, s.handleUpload)
	s.adminRoutes(mux)
	return limitBodies(mux)
}

// adminRoutes registers the routes served on both the tailnet API (for admin
// devices) and the local admin socket. /v1/agents is added by the caller.
func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/held", s.handleHeld)
	mux.HandleFunc("POST /v1/admin/requests/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /v1/admin/requests/{id}/deny", s.handleDeny)
	mux.HandleFunc("POST /v1/admin/invite", s.handleInvite)
	mux.HandleFunc("GET /v1/admin/urls", s.handleAdminURLs)
	mux.HandleFunc("POST /v1/admin/remove", s.handleRemove)
	mux.HandleFunc("PUT /v1/agents/{name}/kind", s.handleSetKind)
	mux.HandleFunc("PUT /v1/agents/{name}/good-at", s.handleSetGoodAt)
	mux.HandleFunc("POST /v1/admin/connect", s.handleConnect)
	mux.HandleFunc("GET /v1/trace/{trace}", s.handleTrace)
	mux.HandleFunc("GET /v1/trace", s.handleRecent)
	mux.HandleFunc("GET /v1/search", s.handleSearch)
	mux.HandleFunc("GET /v1/admin/audit/verify", s.handleVerify)
	mux.HandleFunc("POST /v1/admin/relay/upgrade", s.handleSelfUpgrade)
	mux.HandleFunc("GET /v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /v1/attachments/{id}", s.handleApprovalFetch)
}

// AdminHandler serves only admin routes and treats every caller as the local
// admin. Serve it on a unix socket on the relay host.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agents", s.handleAgents)
	s.adminRoutes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), localAdminKey, true)))
	})
}

// limitBodies caps every request body at the API limit, except an
// attachment upload, whose handler applies its own larger cap.
func limitBodies(h http.Handler) http.Handler {
	max := client.Defaults(client.RelayAPI).MaxBodyBytes
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != uploadPath {
			client.LimitBody(w, r, max)
		}
		h.ServeHTTP(w, r)
	})
}

// Stop ends every held long poll and get-reply wait at once with the answer
// its hold deadline would give (nothing yet, poll again), and makes later
// ones answer without holding. The relay calls it first when it shuts down,
// so held calls do not keep the process alive. Request contexts stay live,
// so calls already in flight finish normally. Stop is idempotent.
func (s *Server) Stop() { s.stopOnce.Do(func() { close(s.stopping) }) }

// Run sweeps expired requests and leases, and applies attachment retention
// at start and every Attachments.SweepEvery, until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.SweepEvery)
	defer t.Stop()
	at := time.NewTicker(s.cfg.Attachments.SweepEvery)
	defer at.Stop()
	s.SweepAttachments(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		case <-at.C:
			s.SweepAttachments(ctx)
		}
	}
}

// Sweep runs one expiry and lease pass and wakes affected waiters.
func (s *Server) Sweep(ctx context.Context) {
	trs, err := s.store.Sweep(ctx)
	if err != nil {
		log.Printf("sweep: %v", err)
		return
	}
	for _, t := range trs {
		event := "requeued"
		if t.Status == envelope.StatusExpired {
			event = "expired"
			if t.Held {
				event = "hold_expired"
			}
		}
		s.record(ctx, event, t.ID, t.TraceID, "relay", "")
		s.hub.notify(requestKey(t.ID))
		if t.Status == envelope.StatusQueued {
			s.hub.notify(inboxKey(t.To))
			// An agent woken by the relay has no poller to see the requeue,
			// so wake it again; pollers are skipped by the waker itself.
			req := envelope.Request{ID: t.ID, TraceID: t.TraceID, From: t.From, To: t.To, Urgent: t.Urgent}
			if rq, ok := s.events.(Requeuer); ok {
				rq.Requeued(ctx, req)
			} else if s.events != nil {
				s.events.Queued(ctx, req)
			}
		}
	}
}

func isLocalAdmin(ctx context.Context) bool {
	v, _ := ctx.Value(localAdminKey).(bool)
	return v
}

func (s *Server) remote(r *http.Request) string {
	if isLocalAdmin(r.Context()) {
		return identity.LocalAdmin
	}
	return r.RemoteAddr
}

// agent attributes the request or writes an error and returns "". WhoIs
// picks the node; the client's X-Tincan-Agent header picks among that node's
// agents and is refused for a name bound elsewhere. A rebuilt machine that the
// directory re-admits on the way is audited as a "rebind".
func (s *Server) agent(w http.ResponseWriter, r *http.Request) string {
	res, err := s.dir.ResolveAgent(r.Context(), r.RemoteAddr, r.Header.Get(client.AgentHeader))
	if err != nil {
		code := http.StatusForbidden
		if errors.Is(err, identity.ErrRebindCheckFailed) {
			code = http.StatusServiceUnavailable
		}
		writeErr(w, code, err)
		return ""
	}
	if rb := res.Rebind; rb != nil {
		log.Printf("rebind: %s moved from %s (%s) to %s (%s)", rb.Agent, rb.OldNodeName, rb.OldNode, rb.NewNodeName, rb.NewNode)
		s.record(r.Context(), "rebind", "", "", rb.Agent, store.DetailJSON(map[string]any{
			"agent": rb.Agent, "old_node": rb.OldNode, "old_node_name": rb.OldNodeName, "new_node": rb.NewNode, "new_node_name": rb.NewNodeName,
		}))
	}
	s.seen(r.Context(), res.Name, r.Header.Get(client.VersionHeader))
	if r.Method == http.MethodGet && r.URL.Path == "/v1/poll" {
		supported := false
		for f := range strings.SplitSeq(r.Header.Get(client.FeaturesHeader), ",") {
			if strings.TrimSpace(f) == "ping" {
				supported = true
			}
		}
		now := s.cfg.Now()
		s.pollMu.Lock()
		defer s.pollMu.Unlock()
		s.mu.Lock()
		old := s.pollFeatures[res.Name]
		state := old
		if supported {
			state.Ping = true
		} else {
			state.LastUnsupported = now
		}
		s.pollFeatures[res.Name] = state
		// Persist only what changes the gate: support appearing, or a legacy
		// poll after a quiet spell. Every poll hitting the database would put
		// a write on the hot path.
		persist := state.Ping != old.Ping || (!supported && (old.LastUnsupported.IsZero() || now.Sub(old.LastUnsupported) > pollFeaturesPersistEvery))
		s.mu.Unlock()
		if persist {
			encoded, _ := json.Marshal(state)
			if err := s.store.SetPollFeatures(r.Context(), res.Name, string(encoded)); err != nil {
				log.Printf("poll features: %v", err)
			}
		}
	}
	return res.Name
}

// seen records that agent called the relay, running the tincan build named
// by version ("" from a client that predates the header). The in-memory
// time moves on every call; the store is written at most once per
// persistEvery so that activity survives a restart without a write per
// request. The build is written only when it changes, so an upgrade shows in
// the roster at once and an unchanged one costs nothing. A failed write is
// logged and never fails the request.
func (s *Server) seen(ctx context.Context, agent, version string) {
	now := s.cfg.Now()
	version = cleanVersion(version)
	s.mu.Lock()
	if now.After(s.lastSeen[agent]) {
		s.lastSeen[agent] = now
	}
	last, ok := s.persisted[agent]
	write := !ok || now.Sub(last) >= persistEvery
	if write {
		s.persisted[agent] = now
	}
	if version != "" {
		s.versions[agent] = version
	}
	vw, wrote := s.versionWritten[agent]
	writeVersion := version != "" && s.storedVersion[agent] != version && (!wrote || now.Sub(vw) >= persistEvery)
	if writeVersion {
		s.storedVersion[agent] = version
		s.versionWritten[agent] = now
	}
	s.mu.Unlock()
	if !write && !writeVersion {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if write {
		if err := s.store.TouchAgent(ctx, agent, now); err != nil {
			log.Printf("last seen for %s: %v", agent, err)
		}
	}
	if writeVersion {
		if err := s.store.SetAgentVersion(ctx, agent, version); err != nil {
			log.Printf("version for %s: %v", agent, err)
		}
	}
}

// handleWhoAmI tells the calling agent who the relay thinks it is. tincan
// rejoin calls it to confirm a rebuilt machine was re-admitted.
func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	a, _, err := s.dir.Agent(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]any{"name": name, "kind": a.Kind, "relay_key": s.key, "relay_urls": s.urls}
	if s.cfg.Version != "" {
		out["relay_version"] = s.cfg.Version
	}
	if v := s.upgradeFor(r); v != "" {
		out["upgrade_available"] = v
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	from := s.agent(w, r)
	if from == "" {
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	req, err := envelope.ParseSend(raw, from, envelope.DefaultMaxBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Attachments) > 0 && s.blobs == "" {
		writeErr(w, http.StatusBadRequest, errAttachmentsOff)
		return
	}
	if !s.isAgent(r.Context(), req.To) {
		writeErr(w, http.StatusNotFound, errors.New("no such agent: "+req.To))
		return
	}
	if req.Kind == envelope.KindPing {
		s.mu.Lock()
		state := s.pollFeatures[req.To]
		capable, version := state.Ping && (state.LastUnsupported.IsZero() || s.cfg.Now().Sub(state.LastUnsupported) > legacyPollWindow), s.versions[req.To]
		s.mu.Unlock()
		if !capable {
			if version == "" {
				version = "unknown"
			}
			writeErr(w, http.StatusConflict, fmt.Errorf("%s runs tincan %s and has not advertised ping support; use ask", req.To, version))
			return
		}
	}
	if err := s.prep.Prepare(r.Context(), &req); err != nil {
		s.record(r.Context(), "rejected", "", req.TraceID, from, store.DetailJSON(map[string]any{"to": req.To, "reason": err.Error()}))
		writeErr(w, statusFor(err), err)
		return
	}
	prepared := req
	req, err = s.store.Enqueue(r.Context(), req, s.requestTTL(r.Context(), req.To))
	if err != nil {
		if rf, ok := s.prep.(Refunder); ok {
			rf.Refund(prepared)
		}
		if errors.Is(err, store.ErrGroupFull) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, attachmentStatus(err, http.StatusInternalServerError), err)
		return
	}
	if req.Status == envelope.StatusHeld {
		s.record(r.Context(), "held", req.ID, req.TraceID, from, store.DetailJSON(map[string]any{"to": req.To, "hop": req.Hop, "chain": req.Chain}))
		s.notifyApproval(r.Context(), req)
		writeJSON(w, http.StatusCreated, req)
		return
	}
	s.hub.notify(inboxKey(req.To))
	s.record(r.Context(), "queued", req.ID, req.TraceID, from, store.DetailJSON(map[string]any{"to": req.To, "hop": req.Hop, "chain": req.Chain}))
	if s.events != nil {
		s.events.Queued(r.Context(), req)
	}
	writeJSON(w, http.StatusCreated, envelope.SendResponse{Request: req, Target: s.recipientTarget(r.Context(), req.To)})
}

// recipientTarget is the schedule facts for agent on wake method schedule,
// its last wake for an agent the relay wakes, and nil otherwise. A failed
// join-time lookup measures from the relay's start.
func (s *Server) recipientTarget(ctx context.Context, agent string) *envelope.Target {
	every := s.checkEvery(agent)
	if every <= 0 {
		return s.recipientWake(ctx, agent)
	}
	s.mu.Lock()
	last := s.lastPoll[agent]
	s.mu.Unlock()
	var joined time.Time
	if last.IsZero() {
		// Only an agent that has never polled is measured from its join.
		if a, ok, err := s.lookupAgent(ctx, agent); err == nil && ok {
			joined = a.JoinedAt
		}
	}
	return s.scheduleTarget(every, last, joined, s.cfg.Now())
}

// recipientWake is the last wake of a relay-woken agent for a send
// response, nil when the relay has not woken it.
func (s *Server) recipientWake(ctx context.Context, agent string) *envelope.Target {
	wr, ok := s.wake.(WakeReporter)
	if !ok || !relayWoken(s.wake.WakeMethod(agent)) {
		return nil
	}
	wk, ok := wr.LastWake(agent)
	if !ok {
		return nil
	}
	if a, found, err := s.dir.Agent(ctx, agent); err == nil && found && wk.At.Before(a.JoinedAt) {
		return nil // sent to an earlier agent of the same name
	}
	s.mu.Lock()
	last := s.lastPoll[agent]
	s.mu.Unlock()
	if last.Before(wk.At) {
		// No poll since the wake in this run of the relay: one persisted
		// before a restart may still have answered it.
		if p, err := s.store.AgentLastPoll(ctx, agent); err == nil && p.After(last) {
			last = p
		}
	}
	t := s.wakeTarget(wk, last, s.cfg.Now())
	return &t
}

// handlePoll holds a long-poll until requests for the caller, or unseen
// replies to its own requests that it asked for, are waiting. No poll marks a
// reply seen: replies=take and replies=keep both return unseen replies, and
// a taking client acknowledges them through POST /v1/replies/ack once it has
// handed them to its agent. Without a replies param (every client that
// predates replies) they are left out and do not end the hold, as with
// replies=none, since such a client would drop them. peek=1 only reports
// what is waiting, and counts replies only with replies=keep or take; it
// names the oldest queued requests (id and sender) so a channel can say who
// is waiting, and changes no request's state or any reply's seen state.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	hold := durationParam(r, "hold", s.cfg.PollHold, s.cfg.PollHold)
	peek := r.URL.Query().Get("peek") == "1"
	replies := r.URL.Query().Get("replies")
	switch replies {
	case "":
		replies = client.RepliesNone
	case client.RepliesTake, client.RepliesKeep, client.RepliesNone:
	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("replies must be %q, %q, or %q", client.RepliesTake, client.RepliesKeep, client.RepliesNone))
		return
	}
	s.mu.Lock()
	s.polling[name]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.polling[name]--
		s.mu.Unlock()
	}()
	deadline := time.NewTimer(hold)
	defer deadline.Stop()
	persist := true // the store copy is written once, when the poll starts
	for {
		s.touch(r.Context(), name, persist)
		persist = false
		wake := s.hub.wait(inboxKey(name))
		var reps []envelope.Result
		var more int
		if replies != client.RepliesNone {
			var err error
			if reps, more, err = s.unseenReplies(r.Context(), name); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
		}
		if peek {
			// Report what is waiting without delivering it, so a listener
			// can nudge the agent and the agent's own check_inbox still
			// receives it. waiting counts the replies it was asked for.
			n, pings, err := s.store.CountQueuedWithPings(r.Context(), name)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if n > 0 || len(reps) > 0 {
				out := map[string]any{"waiting": n + len(reps) + more, "queued": n}
				if pings > 0 {
					out["pings"] = pings
				}
				if n > 0 {
					urgent, err := s.store.CountUrgentQueued(r.Context(), name)
					if err != nil {
						writeErr(w, http.StatusInternalServerError, err)
						return
					}
					if urgent > 0 {
						out["urgent"] = urgent
					}
					pending, err := s.store.PendingRequests(r.Context(), name, MaxPeekPending)
					if err != nil {
						writeErr(w, http.StatusInternalServerError, err)
						return
					}
					out["pending"] = pending
				}
				if replies != client.RepliesNone {
					out["replies"] = emptyIfNil(reps)
				}
				if more > 0 {
					out["replies_remaining"] = more
				}
				if v := s.upgradeFor(r); v != "" {
					out["upgrade_available"] = v
				}
				writeJSON(w, http.StatusOK, out)
				return
			}
			select {
			case <-wake:
				continue
			case <-deadline.C:
			case <-s.stopping:
			case <-r.Context().Done():
				return
			}
			if v := s.upgradeFor(r); v != "" {
				out := map[string]any{"upgrade_available": v}
				out["waiting"], out["queued"] = 0, 0
				writeJSON(w, http.StatusOK, out)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		reqs, err := s.store.Deliver(r.Context(), name, 20, s.cfg.DeliveryLease)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if len(reqs) > 0 || len(reps) > 0 {
			for i := range reqs {
				reqs[i].RedactFor(name)
			}
			for _, q := range reqs {
				s.record(r.Context(), "delivered", q.ID, q.TraceID, name, "")
			}
			out := map[string]any{"requests": emptyIfNil(reqs)}
			if len(reps) > 0 {
				out["replies"] = reps
			}
			if more > 0 {
				out["replies_remaining"] = more
			}
			if v := s.upgradeFor(r); v != "" {
				out["upgrade_available"] = v
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		select {
		case <-wake:
			continue
		case <-deadline.C:
		case <-s.stopping:
		case <-r.Context().Done():
			return
		}
		s.touch(r.Context(), name, s.wokenDuringPoll(name))
		if v := s.upgradeFor(r); v != "" {
			out := map[string]any{"upgrade_available": v}
			out["requests"] = []envelope.Request{}
			writeJSON(w, http.StatusOK, out)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
}

// MaxPeekPending caps how many queued requests one peek names, with a
// separate cap for pings, which are listed first. queued still counts them
// all.
const MaxPeekPending = 50

// MaxRepliesBytes bounds the request, reply, and parent bodies of the unseen replies
// one poll returns, so a response stays well under the client's 4 MiB read
// limit. A poll always returns at least one waiting reply; the rest wait for
// the next poll, after the caller acknowledges this batch.
const MaxRepliesBytes = 1 << 20

// unseenReplies returns the oldest of agent's unseen replies that fit in
// MaxRepliesBytes, and how many more are waiting beyond them.
func (s *Server) unseenReplies(ctx context.Context, agent string) ([]envelope.Result, int, error) {
	reps, err := s.store.UnseenReplies(ctx, agent)
	if err != nil {
		return nil, 0, err
	}
	cut := len(reps) == store.MaxUnseenReplies // the store may hold more
	size := 0
	for i, rep := range reps {
		n := len(rep.Request.Body)
		for _, ex := range rep.Exchanges {
			n += len(ex.Question) + len(ex.Answer)
		}
		for _, ex := range rep.Request.Exchanges {
			n += len(ex.Question) + len(ex.Answer)
		}
		if rep.Reply != nil {
			n += len(rep.Reply.Body)
		}
		if rep.Parent != nil {
			n += len(rep.Parent.Body)
		}
		if i > 0 && size+n > MaxRepliesBytes {
			reps, cut = reps[:i], true
			break
		}
		size += n
	}
	if !cut {
		return reps, 0, nil
	}
	total, err := s.store.CountUnseenReplies(ctx, agent)
	if err != nil {
		return nil, 0, err
	}
	return reps, max(total-len(reps), 0), nil
}

// handleAckReplies marks replies the caller has handed to its agent as
// seen. Ids that are not the caller's own requests, or that have no reply
// yet, are ignored.
func (s *Server) handleAckReplies(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	var in struct {
		IDs  []string            `json:"ids"`
		Acks []envelope.ReplyAck `json:"acks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("body must be {\"ids\": [...]}: %w", err))
		return
	}
	if len(in.IDs)+len(in.Acks) > maxAckIDs {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("at most %d ids per ack", maxAckIDs))
		return
	}
	if err := s.store.MarkRepliesSeen(r.Context(), name, in.IDs, in.Acks...); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxAckIDs caps one ack, well above the replies a poll returns.
const maxAckIDs = 500

// emptyIfNil keeps a JSON list a list rather than null.
func emptyIfNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	req, err := s.store.Claim(r.Context(), r.PathValue("id"), name, s.cfg.ClaimLease)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.hub.notify(requestKey(req.ID))
	s.record(r.Context(), "claimed", req.ID, req.TraceID, name, "")
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid progress body"))
		return
	}
	if len(in.Note) > envelope.MaxProgressNote {
		writeErr(w, http.StatusRequestEntityTooLarge, envelope.ErrBodyTooLarge)
		return
	}
	if strings.TrimSpace(in.Note) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("note is required"))
		return
	}
	id := r.PathValue("id")
	if err := s.store.SetProgress(r.Context(), id, name, in.Note, s.cfg.ClaimLease); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "progress", id, "", name, store.DetailJSON(map[string]any{"bytes": len(in.Note)}))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	rep, err := envelope.ParseReply(raw, envelope.DefaultMaxBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(rep.Attachments) > 0 && s.blobs == "" {
		writeErr(w, http.StatusBadRequest, errAttachmentsOff)
		return
	}
	id := r.PathValue("id")
	rep, err = s.store.Reply(r.Context(), id, name, rep)
	if err != nil {
		writeErr(w, attachmentStatus(err, statusFor(err)), err)
		return
	}
	s.hub.notify(requestKey(id))
	event := "replied"
	detail := map[string]any{"status": rep.Status}
	if rep.Status == envelope.StatusNeedsInput {
		event = "needs_input"
		detail = map[string]any{"question_bytes": len(rep.Body)}
	}
	s.record(r.Context(), event, id, "", name, store.DetailJSON(detail))
	// The asker learns of the reply from a held poll now, or from a wake
	// once the waker's grace period shows it went unread.
	if req, _, err := s.store.Request(r.Context(), id); err == nil {
		s.hub.notify(inboxKey(req.From))
		if rp, ok := s.events.(Replier); ok && req.Kind != envelope.KindPing {
			rp.Replied(r.Context(), req)
		}
	} else {
		log.Printf("reply %s: look up asker: %v", id, err)
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	id := r.PathValue("id")
	wait := durationParam(r, "wait", 0, s.cfg.MaxWait)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		wake := s.hub.wait(requestKey(id))
		res, err := s.store.Get(r.Context(), id, name)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		if res.Done() || res.Status == envelope.StatusNeedsInput || res.Status == envelope.StatusHeld || wait == 0 {
			writeJSON(w, http.StatusOK, res)
			return
		}
		select {
		case <-wake:
			continue
		case <-deadline.C:
		case <-s.stopping:
		case <-r.Context().Done():
			return
		}
		res, err = s.store.Get(r.Context(), id, name)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	id := r.PathValue("id")
	if err := s.store.Cancel(r.Context(), id, name); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.hub.notify(requestKey(id))
	s.record(r.Context(), "cancelled", id, "", name, "")
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": string(envelope.StatusCancelled)})
}

// WakeNamer reports an agent's wake method name. Set by package wake.
type WakeNamer interface{ WakeMethod(agent string) string }

// SetWakeNamer installs the wake method lookup for the agent list. If w
// also implements Scheduler, schedule agents' facts are reported too.
func (s *Server) SetWakeNamer(w WakeNamer) { s.wake = w }

// Scheduler reports how often an agent on wake method schedule checks its
// inbox, 0 for an agent on any other method. Set by package wake.
type Scheduler interface {
	CheckEvery(agent string) time.Duration
}

// ScheduleGrace is the allowance for a schedule agent's late check: the
// reply window a sender is shown is one interval plus it, and an agent is
// overdue after two intervals plus it without polling.
const ScheduleGrace = 5 * time.Minute

// WakeReporter reports the last wake the relay sent an agent, and whether
// there was one, and forgets it when the agent is removed. Unforget undoes
// Forget when the removal fails. Set by package wake through SetWakeNamer.
type WakeReporter interface {
	LastWake(agent string) (store.Wake, bool)
	Forget(agent string)
	Unforget(agent string)
}

// WakeResumer schedules the wakes an agent's waiting work calls for, as a
// restarted relay does. Set by package wake through SetWakeNamer.
type WakeResumer interface {
	RequestsWaiting(agent string)
	ReplyWaiting(agent string)
}

// DefaultWakeGrace is how long a woken agent has to poll before the roster
// shows it as unanswered.
const DefaultWakeGrace = 10 * time.Minute

// relayWoken reports whether the relay itself wakes agents on method.
func relayWoken(method string) bool { return method == "webhook" || method == "email" }

// wakeTarget is a relay-woken agent's last wake and whether it is
// unanswered: the send failed, or more than the wake grace has passed with
// no poll since it. Only a poll counts as a check-in; lastPoll is the later
// of the one in memory and the one persisted. The grace runs from the later
// of the wake and the relay's start, so a restart does not flag at once.
func (s *Server) wakeTarget(wk store.Wake, lastPoll, now time.Time) envelope.Target {
	t := envelope.Target{WokenAt: wk.At, WakeResult: wk.Result}
	if !lastPoll.IsZero() && !lastPoll.Before(wk.At) {
		return t
	}
	since := wk.At
	if s.started.After(since) {
		since = s.started
	}
	t.Unanswered = wk.Result != envelope.WakeOK || now.Sub(since) > s.cfg.WakeGrace
	return t
}

// checkEvery is agent's schedule interval, 0 when it is not on schedule.
func (s *Server) checkEvery(agent string) time.Duration {
	if sc, ok := s.wake.(Scheduler); ok {
		return sc.CheckEvery(agent)
	}
	return 0
}

// scheduleTarget computes a schedule agent's facts from its interval and
// its last inbox poll. An agent that has not polled since the relay
// started is measured from the later of its join and the relay's start.
// Other activity, such as sending asks, does not count as checking. Nil
// when every is not positive.
func (s *Server) scheduleTarget(every time.Duration, lastPoll, joined, now time.Time) *envelope.Target {
	if every <= 0 {
		return nil
	}
	since := lastPoll
	if since.IsZero() {
		since = joined
		if s.started.After(since) {
			since = s.started
		}
	}
	return &envelope.Target{
		CheckEverySeconds:  int(every / time.Second),
		ExpectReplySeconds: int((every + ScheduleGrace) / time.Second),
		Overdue:            now.Sub(since) > 2*every+ScheduleGrace,
	}
}

// handleAgents lists the roster for joined agents and for admin devices,
// which need not be joined (onboarding runs from an admin laptop).
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) && s.agent(w, r) == "" {
		return
	}
	agents, err := s.dir.Agents(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	persisted, err := s.store.AgentsLastSeen(r.Context())
	if err != nil {
		// The in-memory times still answer for this run of the relay.
		log.Printf("agents last seen: %v", err)
	}
	stats, err := s.store.QueueStats(r.Context())
	if err != nil {
		log.Printf("agents queue stats: %v", err)
	}
	now := s.cfg.Now()
	// Read the waker before taking s.mu, which it need not wait on.
	wakes := map[string]store.Wake{}
	if wr, ok := s.wake.(WakeReporter); ok {
		for _, a := range agents {
			// A wake from before the join was sent to an earlier agent of
			// the same name.
			if wk, ok := wr.LastWake(a.Name); ok && relayWoken(s.wake.WakeMethod(a.Name)) && !wk.At.Before(a.JoinedAt) {
				wakes[a.Name] = wk
			}
		}
	}
	var polls map[string]time.Time
	if len(wakes) > 0 {
		// A poll persisted before a restart may have answered a wake.
		if polls, err = s.store.AgentsLastPoll(r.Context()); err != nil {
			log.Printf("agents last poll: %v", err)
		}
	}
	out := make([]client.AgentInfo, 0, len(agents))
	s.mu.Lock()
	for _, a := range agents {
		last := s.lastPoll[a.Name]
		active := s.lastSeen[a.Name]
		if p := persisted[a.Name]; p.After(active) {
			active = p
		}
		info := client.AgentInfo{Name: a.Name, LastPoll: last, LastActive: active, Online: !last.IsZero() && now.Sub(last) < s.cfg.PollHold+30*time.Second, Wake: "none", Kind: a.Kind, GoodAt: a.GoodAt, Version: s.versions[a.Name]}
		if info.GoodAt == "" {
			info.GoodAt = onboard.StockGoodAt(a.Name, a.Kind)
		}
		stat := stats[a.Name]
		info.Queued, info.OldestQueued, info.Claimed = stat.Queued, stat.OldestQueued, stat.Claimed
		if s.wake != nil {
			info.Wake = s.wake.WakeMethod(a.Name)
			if t := s.scheduleTarget(s.checkEvery(a.Name), last, a.JoinedAt, now); t != nil {
				info.Target = *t
			}
			if wk, ok := wakes[a.Name]; ok {
				polled := last
				if p := polls[a.Name]; p.After(polled) {
					polled = p
				}
				info.Target = s.wakeTarget(wk, polled, now)
			}
		}
		out = append(out, info)
	}
	s.mu.Unlock()
	res := map[string]any{"agents": out}
	if s.cfg.Version != "" {
		res["relay_version"] = s.cfg.Version
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name, err := s.dir.Join(r.Context(), r.RemoteAddr, in.Code)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "joined", "", "", name, "")
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := knownKind(in.Kind); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	code, err := s.dir.InviteKind(r.Context(), s.remote(r), in.Name, in.Kind)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": in.Name, "kind": in.Kind, "code": code, "expires_in": identity.InviteTTL.String()})
}

// handleSetKind records a joined agent's runtime kind (admin only). An empty
// kind clears it.
func (s *Server) handleSetKind(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	if err := knownKind(in.Kind); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("name")
	if err := s.dir.SetKind(r.Context(), s.remote(r), name, in.Kind); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "kind", "", "", name, store.DetailJSON(map[string]any{"kind": in.Kind}))
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "kind": in.Kind})
}

// handleSetGoodAt records the owner's line saying what a joined agent is good
// at (admin only). An empty line clears it, so a kind with a stock line
// shows it again; a body without good_at is refused rather than read as a
// clear.
func (s *Server) handleSetGoodAt(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GoodAt *string `json:"good_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.GoodAt == nil {
		writeErr(w, http.StatusBadRequest, errors.New(`good_at is required; send "" to clear the line`))
		return
	}
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	name := r.PathValue("name")
	line, err := s.dir.SetGoodAt(r.Context(), s.remote(r), name, *in.GoodAt)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "good-at", "", "", name, store.DetailJSON(map[string]any{"good_at": line}))
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "good_at": line})
}

// knownKind accepts "" and the kinds onboarding can tailor a block to, so a
// typo is caught when it is set rather than silently ignored later.
func knownKind(kind string) error {
	if onboard.KnownKind(kind) {
		return nil
	}
	return fmt.Errorf("unknown kind %q (want one of %s)", kind, strings.Join(onboard.Kinds, ", "))
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Revoke tokens and cancel open requests before unbinding, so a failure
	// leaves the agent bound (and the removal retryable) rather than removed
	// with live tokens or still-deliverable requests.
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	if ok, err := s.dir.Has(r.Context(), in.Name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	} else if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%s: %w", in.Name, identity.ErrUnknownAgent))
		return
	}
	if s.conn != nil {
		if err := s.conn.Revoke(r.Context(), in.Name); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("revoke %s: %w", in.Name, err))
			return
		}
	}
	ids, err := s.store.CancelAllFor(r.Context(), in.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, id := range ids {
		s.hub.notify(requestKey(id))
	}
	wr, forgot := s.wake.(WakeReporter)
	if forgot {
		// Before the delete, so a wake that finishes meanwhile cannot
		// write the wake row back after the agent's row took it.
		wr.Forget(in.Name)
	}
	if err := s.dir.Remove(r.Context(), s.remote(r), in.Name); err != nil {
		if forgot {
			// The agent is still joined: give it back its wake state and
			// the nudge Forget cancelled.
			wr.Unforget(in.Name)
			if rs, ok := s.wake.(WakeResumer); ok {
				if s.QueuedCount(in.Name) > 0 {
					rs.RequestsWaiting(in.Name)
				}
				if s.UnseenReplies(in.Name) > 0 {
					rs.ReplyWaiting(in.Name)
				}
			}
		}
		writeErr(w, statusFor(err), err)
		return
	}
	// What the relay remembered about the name in memory goes with it, so
	// an agent joined later under the same name starts with a clean row.
	s.mu.Lock()
	delete(s.lastPoll, in.Name)
	delete(s.lastSeen, in.Name)
	delete(s.persisted, in.Name)
	delete(s.versions, in.Name)
	delete(s.pollFeatures, in.Name)
	delete(s.storedVersion, in.Name)
	delete(s.versionWritten, in.Name)
	s.mu.Unlock()
	s.record(r.Context(), "removed", "", "", in.Name, store.DetailJSON(map[string]any{"cancelled": len(ids)}))
	writeJSON(w, http.StatusOK, map[string]any{"removed": in.Name, "cancelled": len(ids)})
}

func (s *Server) isAgent(ctx context.Context, name string) bool {
	ok, err := s.dir.Has(ctx, name)
	return err == nil && ok
}

// record appends an audit entry. Audit failures are logged, never fatal: the
// relay keeps delivering if the log cannot be written.
func (s *Server) record(ctx context.Context, event, requestID, traceID, actor, detail string) {
	if traceID == "" && requestID != "" {
		if req, _, err := s.store.Request(ctx, requestID); err == nil {
			traceID = req.TraceID
		}
	}
	if err := s.store.Audit(ctx, store.AuditEvent{Event: event, RequestID: requestID, TraceID: traceID, Actor: actor, Detail: detail}); err != nil {
		log.Printf("audit %s %s: %v", event, requestID, err)
	}
}

// Online reports whether agent has polled recently enough that its poller
// will pick up a new request without a wake.
func (s *Server) Online(agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.polling[agent] > 0 {
		return true
	}
	last := s.lastPoll[agent]
	return !last.IsZero() && s.cfg.Now().Sub(last) < 5*time.Second
}

// QueuedCount returns how many requests to agent are still waiting to be
// delivered. The waker asks this when it re-checks a wake it skipped because
// agent looked online. A failed count reports one, since a spare nudge costs
// less than a stranded request.
func (s *Server) QueuedCount(agent string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := s.store.CountQueued(ctx, agent)
	if err != nil {
		log.Printf("queued requests for %s: %v", agent, err)
		return 1
	}
	return n
}

// UnseenReplies returns how many replies to agent's own requests it has not
// read yet. The waker asks this when a reply's grace period ends. A failed
// count reports one, since a spare nudge costs less than a missed reply.
func (s *Server) UnseenReplies(agent string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := s.store.CountUnseenReplies(ctx, agent)
	if err != nil {
		log.Printf("unseen replies for %s: %v", agent, err)
		return 1
	}
	return n
}

// touch records a poll by agent. The store copy tells a restarted relay
// that a woken agent checked in, so with persist it is written for an agent
// the relay wakes (webhook and email agents poll about once per wake) and
// never for one it does not, which polls constantly and is never judged
// unanswered. A poll persists once, at its start; later touches in the same
// poll only update memory.
// wokenDuringPoll reports whether the relay recorded a wake for agent after
// its open poll started (lastPoll still holds that start), so the poll's end
// is the check-in for that wake and must be stored as well as kept in memory.
func (s *Server) wokenDuringPoll(agent string) bool {
	wr, ok := s.wake.(WakeReporter)
	if !ok {
		return false
	}
	wk, ok := wr.LastWake(agent)
	if !ok {
		return false
	}
	s.mu.Lock()
	start := s.lastPoll[agent]
	s.mu.Unlock()
	return wk.At.After(start)
}

func (s *Server) touch(ctx context.Context, agent string, persist bool) {
	now := s.cfg.Now()
	s.mu.Lock()
	s.lastPoll[agent] = now
	if now.After(s.lastSeen[agent]) {
		s.lastSeen[agent] = now
	}
	s.mu.Unlock()
	if !persist || s.wake == nil || !relayWoken(s.wake.WakeMethod(agent)) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.TouchAgentPoll(ctx, agent, now); err != nil {
		log.Printf("last poll for %s: %v", agent, err)
	}
}

func durationParam(r *http.Request, key string, def, max time.Duration) time.Duration {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return def
	}
	d := time.Duration(secs) * time.Second
	if d > max {
		return max
	}
	return d
}

// StatusError lets a Preparer choose the HTTP status for a rejection.
type StatusError struct {
	Code int
	Err  error
}

func (e *StatusError) Error() string { return e.Err.Error() }
func (e *StatusError) Unwrap() error { return e.Err }

func statusFor(err error) int {
	var se *StatusError
	switch {
	case errors.As(err, &se):
		return se.Code
	case errors.Is(err, identity.ErrRebindCheckFailed):
		return http.StatusServiceUnavailable
	case errors.Is(err, store.ErrNotFound), errors.Is(err, identity.ErrUnknownAgent):
		return http.StatusNotFound
	case errors.Is(err, store.ErrForbidden), errors.Is(err, identity.ErrNotAdmin), errors.Is(err, identity.ErrNotJoined),
		errors.Is(err, identity.ErrAgentAmbiguous):
		return http.StatusForbidden
	case errors.Is(err, store.ErrWrongState):
		return http.StatusConflict
	case errors.Is(err, identity.ErrBadInvite):
		return http.StatusBadRequest
	}
	return http.StatusBadRequest
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// isAdmin reports whether the caller is the local admin socket or an admin
// device.
func (s *Server) isAdmin(r *http.Request) bool {
	return s.dir.IsAdmin(r.Context(), s.remote(r))
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	trace := r.PathValue("trace")
	steps, err := s.store.Trace(r.Context(), trace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !s.isAdmin(r) {
		name := s.agent(w, r)
		if name == "" {
			return
		}
		for i := range steps {
			steps[i].Request.RedactFor(name)
		}
		if !slices.Contains(store.Participants(steps), name) {
			writeErr(w, http.StatusNotFound, errors.New("no such trace"))
			return
		}
	}
	if len(steps) == 0 {
		writeErr(w, http.StatusNotFound, errors.New("no such trace"))
		return
	}
	events, err := s.store.AuditForTrace(r.Context(), trace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trace_id": trace, "steps": steps, "events": events})
}

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	limit := 20
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	steps, err := s.store.RecentTraces(r.Context(), limit, r.URL.Query().Get("exclude_pings") == "true")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"traces": steps})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	n, err := s.store.VerifyAudit(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "checked": n, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "checked": n})
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	if s.conn == nil {
		writeErr(w, http.StatusNotFound, errors.New("the ChatGPT gateway is not enabled on this relay (start it with --chatgpt-gateway)"))
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	code, url, err := s.conn.Connect(r.Context(), in.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.record(r.Context(), "connected", "", "", in.Name, "")
	writeJSON(w, http.StatusOK, map[string]string{"name": in.Name, "code": code, "url": url, "expires_in": "10m0s"})
}

func approvalPreview(body string) string {
	return string([]rune(body)[:min(200, len([]rune(body)))])
}

func (s *Server) handleHeld(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	s.Sweep(r.Context())
	reqs, err := s.store.Held(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Each entry also names the target's kind, so the owner sees a council ask
	// (which goes on to every model vendor) for what it is. The attachments'
	// names and sizes already ride on the request. Old clients ignore to_kind.
	type heldRequest struct {
		envelope.Request
		ToKind string `json:"to_kind,omitempty"`
	}
	out := make([]heldRequest, len(reqs))
	kinds := map[string]string{} // target -> kind, for the lookups that succeeded
	for i, req := range reqs {
		req.Body = approvalPreview(req.Body)
		out[i] = heldRequest{Request: req}
		kind, seen := kinds[req.To]
		if !seen {
			a, ok, err := s.lookupAgent(r.Context(), req.To)
			if err != nil {
				continue
			}
			if ok {
				kind = a.Kind
			}
			kinds[req.To] = kind
		}
		out[i].ToKind = kind
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	s.Sweep(r.Context())
	id := r.PathValue("id")
	ttl := s.cfg.RequestTTL
	if held, _, err := s.store.Request(r.Context(), id); err == nil {
		ttl = s.requestTTL(r.Context(), held.To)
	}
	req, err := s.store.Release(r.Context(), id, ttl)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "approved", req.ID, req.TraceID, s.remote(r), "")
	s.hub.notify(requestKey(req.ID))
	s.hub.notify(inboxKey(req.To))
	if s.events != nil {
		s.events.Queued(r.Context(), req)
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) handleDeny(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, envelope.DefaultMaxBody+1024)).Decode(&in); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(in.Reason) > envelope.DefaultMaxBody {
		writeErr(w, http.StatusBadRequest, envelope.ErrBodyTooLarge)
		return
	}
	s.Sweep(r.Context())
	req, err := s.store.DenyHeld(r.Context(), r.PathValue("id"), in.Reason)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "denied", req.ID, req.TraceID, s.remote(r), "")
	s.hub.notify(requestKey(req.ID))
	s.hub.notify(inboxKey(req.From))
	if replier, ok := s.events.(Replier); ok {
		replier.Replied(r.Context(), req)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "declined"})
}

// requestTTL is how long a request to agent lives unanswered: notes agents
// get NotesRequestTTL so an add survives the notes Mac being offline for
// days; every other kind, and an unknown agent, gets RequestTTL. A failed
// lookup is logged and gets NotesRequestTTL: keeping a non-notes request
// longer than needed is cheaper than expiring a note after a day.
func (s *Server) requestTTL(ctx context.Context, agent string) time.Duration {
	a, ok, err := s.lookupAgent(ctx, agent)
	if err != nil {
		log.Printf("request ttl: look up %s: %v (using notes ttl %v)", agent, err, s.cfg.NotesRequestTTL)
		return s.cfg.NotesRequestTTL
	}
	if ok && a.Kind == onboard.KindNotes {
		return s.cfg.NotesRequestTTL
	}
	return s.cfg.RequestTTL
}

func (s *Server) notifyApproval(ctx context.Context, held envelope.Request) {
	if held.ApprovalNotify == "" {
		return
	}
	if !s.isAgent(ctx, held.ApprovalNotify) {
		s.record(ctx, "approval_notify_failed", held.ID, held.TraceID, "relay", "notify agent is not joined")
		return
	}
	// This is a relay admin notice, not a forwarded agent request. It bypasses
	// the gate to avoid recursive notices, and grants no approval capability.
	// It carries no request text: the notified agent may itself be gated (even
	// the held target), and unapproved text must not reach it. The owner reads
	// the request with tincan held.
	req := envelope.Request{From: "relay", To: held.ApprovalNotify, Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"},
		Body: fmt.Sprintf("held for approval: %s -> %s (request %s). Owner: see it with tincan held, then run tincan approve %s or tincan deny %s. Only the owner may decide.", held.From, held.To, held.ID, held.ID, held.ID)}
	req, err := s.store.Enqueue(ctx, req, s.requestTTL(ctx, req.To))
	if err != nil {
		s.record(ctx, "approval_notify_failed", held.ID, held.TraceID, "relay", "")
		return
	}
	s.record(ctx, "approval_notified", held.ID, held.TraceID, "relay", store.DetailJSON(map[string]any{"notification_id": req.ID, "to": req.To}))
	s.record(ctx, "queued", req.ID, req.TraceID, "relay", store.DetailJSON(map[string]any{"held_id": held.ID, "to": req.To}))
	s.hub.notify(inboxKey(req.To))
	if s.events != nil {
		s.events.Queued(ctx, req)
	}
}

// Never-approved attachments remain private even after the hold ends.
func (s *Server) handleApprovalFetch(w http.ResponseWriter, r *http.Request) {
	rec, err := s.store.Attachment(r.Context(), r.PathValue("id"))
	if err == nil && rec.RequestID != "" {
		req, _, err := s.store.Request(r.Context(), rec.RequestID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if req.WasHeld && !req.Approved && !s.isAdmin(r) {
			name := s.agent(w, r)
			if name == "" {
				return
			}
			if name != req.From {
				writeErr(w, http.StatusNotFound, store.ErrNotFound)
				return
			}
		}
	}
	s.handleFetch(w, r)
}

func (s *Server) handleGroup(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	reqs, err := s.store.RequestsByGroup(r.Context(), name, r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if len(reqs) == 0 {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	members := make([]envelope.GroupMember, 0, len(reqs))
	for _, req := range reqs {
		members = append(members, envelope.GroupMember{ID: req.ID, To: req.To})
	}
	writeJSON(w, http.StatusOK, members)
}
