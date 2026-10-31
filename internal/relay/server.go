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
	"path/filepath"
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
	"github.com/mvanhorn/agent-tincan/internal/phone"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// Config tunes the relay.
type Config struct {
	RequestTTL      time.Duration // how long an unanswered request lives
	NotesRequestTTL time.Duration // RequestTTL for requests to a notes-kind agent
	PollHold        time.Duration // max time a long-poll is held
	DeliveryLease   time.Duration // how long a delivered request waits for a claim
	ClaimLease      time.Duration // how long a claim lasts before the request is requeued
	// UrgentClaimLease replaces ClaimLease for an urgent request, when
	// shorter, at claim and at each progress note; default
	// DefaultUrgentClaimLease.
	UrgentClaimLease time.Duration
	MaxWait          time.Duration // cap on get-reply waits
	SweepEvery       time.Duration
	Now              func() time.Time
	Attachments      AttachmentConfig
	// Version is this relay's tincan build, reported to agents in the
	// roster and whoami so a client behind it stands out; "" hides it.
	Version string
	// WakeGrace is how long a relay-woken agent may go without polling
	// after a wake before it shows as unanswered and the waker sends the
	// same webhook or email again; default DefaultWakeGrace.
	WakeGrace time.Duration
	// UrgentWakeGrace is the shorter grace the waker uses while an urgent
	// request is queued, and how long an urgent request waits in silence
	// before its asker is told; default DefaultUrgentWakeGrace.
	UrgentWakeGrace time.Duration
	// OwnerNoticeAfter is how many wakes in a row a relay-woken agent may
	// leave unanswered before the owner is told, once per silent episode,
	// through the approval policy's notify destination; default
	// DefaultOwnerNoticeAfter.
	OwnerNoticeAfter int
	// EmailPollEvery is how often the relay polls an opted-in agent's
	// AgentMail inbox for replies to its request emails while the agent's
	// window is open; default DefaultEmailPollEvery.
	EmailPollEvery time.Duration
	// FailoverPath is the owner's failover.json, the web agents failover
	// never moves an ask to; "" reads it beside the store's database, and
	// an in-memory store has none.
	FailoverPath string
	// ParsePass reads an ask to a pass-kind agent into the chat and
	// destination it names, as the pass service will, for the held
	// listing and the pass_sent audit event. The relay cannot import the
	// pass package (the history package's tests run a relay), so tincan
	// relay supplies it; nil leaves the parsed fields out.
	ParsePass func(body string) (PassRef, error)
}

// PassRef is the chat a pass request names and where it would go.
type PassRef struct {
	Source         string `json:"source"`
	ConversationID string `json:"conversation_id"`
	To             string `json:"to"`
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
		c.ClaimLease = DefaultClaimLease
	}
	if c.UrgentClaimLease == 0 {
		c.UrgentClaimLease = DefaultUrgentClaimLease
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
	if c.UrgentWakeGrace == 0 {
		c.UrgentWakeGrace = DefaultUrgentWakeGrace
	}
	if c.OwnerNoticeAfter == 0 {
		c.OwnerNoticeAfter = DefaultOwnerNoticeAfter
	}
	if c.EmailPollEvery == 0 {
		c.EmailPollEvery = DefaultEmailPollEvery
	}
	c.Attachments.defaults()
}

// DefaultClaimLease is how long a claim lasts without a reply or progress
// note before the request is requeued. It is also how long after a start
// the relay waits before it calls any agent stuck.
const DefaultClaimLease = 30 * time.Minute

// DefaultUrgentClaimLease is how long a claim on an urgent request lasts
// without a reply or progress note before the request is requeued (and its
// target woken again).
const DefaultUrgentClaimLease = 10 * time.Minute

// urgentLease is the claim lease for an urgent request: UrgentClaimLease,
// or ClaimLease when that is shorter.
func (c Config) urgentLease() time.Duration {
	return min(c.UrgentClaimLease, c.ClaimLease)
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
	wakeSettings WakeSettings
	cfg          Config
	dir          *identity.Directory
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
	// emailTagKey signs request-email reply tags (EmailTagKeyFile). It is
	// never served; nil means no tags.
	emailTagKey []byte

	// upgrader installs the dist release over the relay binary; nil when off.
	upgrader SelfUpgrader

	// statsMu guards statsCache and is held across the store read it
	// fills from, so take it without s.mu.
	statsMu    sync.Mutex
	statsCache map[time.Duration]statsEntry

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
	// sessions is when each agent's sessions (SessionHeader) last called,
	// so a poll can tell a session that is still running from one that is
	// gone. In memory only; see replyScope.
	sessions map[string]map[string]time.Time
	// projects is when each agent's project labels (client.ProjectHeader)
	// last called, so a send can tell the asker whether a session of the
	// project it named is live. It never decides delivery: that goes by
	// age, in the store. In memory only.
	projects map[string]map[string]time.Time
	// socketWake is when each agent's sessions last polled advertising
	// client.SocketWakeFeature, so a send can tell the asker the target
	// wakes by socket. A session's entry goes when it polls without the
	// feature, and lapses after SessionGrace. In memory only.
	socketWake map[string]map[string]time.Time
	// workingOn is each agent's sessions' working-on lines (tincan mcp
	// --share-title), by session, lapsing after SessionGrace without a
	// refresh. In memory only; see handleWorkingOn.
	workingOn map[string]map[string]workingOnEntry
	// hostStatus is whether each agent's sessions' hosts say they are
	// working, by session, lapsing after SessionGrace without a refresh.
	// In memory only; see handleHostStatus.
	hostStatus map[string]map[string]hostStatusEntry
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
	// silent counts each relay-woken agent's unanswered wakes in its
	// current silent episode, for the owner's wake notice.
	silent map[string]silentEpisode

	// failoverMu guards failoverErr, the failover.json problem last
	// logged, so each is logged once.
	failoverMu  sync.Mutex
	failoverErr string

	// emailMu serializes email reply polls and guards inboxes,
	// emailCursor and emailRetry: the agents opted in to request emails,
	// how far back each one's next poll looks, and the listed messages
	// that could not be decided yet, retried on their own (see
	// emailreplies.go).
	emailMu     sync.Mutex
	inboxes     []EmailInbox
	emailCursor map[string]time.Time
	emailRetry  map[string]map[string]wake.MailMessage

	// nonces are the decision nonces handed to the phone page with each
	// held request (see phone.go).
	nonces *nonceBook
	// push sends held-ask alerts to subscribed admin phones; nil when off.
	push *Pusher

	// firing is the follow-ons a fire is working on now (see followups.go).
	firing map[string]bool

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
		versionWritten: map[string]time.Time{}, sessions: map[string]map[string]time.Time{}, projects: map[string]map[string]time.Time{}, socketWake: map[string]map[string]time.Time{}, workingOn: map[string]map[string]workingOnEntry{}, hostStatus: map[string]map[string]hostStatusEntry{}, silent: map[string]silentEpisode{}, nonces: newNonceBook(), firing: map[string]bool{}, stopping: make(chan struct{}), started: cfg.Now(), emailCursor: map[string]time.Time{}, emailRetry: map[string]map[string]wake.MailMessage{}}
	s.statsCache = map[time.Duration]statsEntry{}
	s.lookupAgent = dir.Agent
	if s.cfg.FailoverPath == "" && st != nil && st.Path() != "" {
		s.cfg.FailoverPath = filepath.Join(filepath.Dir(st.Path()), FailoverFile)
	}
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
func (s *Server) SetPreparer(p Preparer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prep = p
}

func (s *Server) preparer() Preparer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prep
}

// Connector links virtual agents (ChatGPT through the gateway).
type Connector interface {
	// Connect binds the virtual agent and returns a one-time login code and
	// the public URL to add as a connector. Without replace it refuses a
	// name held by a real tailnet node.
	Connect(ctx context.Context, name string, replace bool) (code, url string, err error)
	// Revoke drops every token the agent holds.
	Revoke(ctx context.Context, name string) error
}

// GuestConnector is a Connector that can also bind a gateway guest, which an
// A2A client always is.
type GuestConnector interface {
	// ConnectGuest binds a new virtual guest on terms and returns a one-time
	// login code and the gateway's base URL.
	ConnectGuest(ctx context.Context, name string, terms identity.GuestTerms) (code, base string, err error)
	// Unpublished returns the names a2a.json does not publish.
	Unpublished(names []string) []string
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
	return s.withHeld(limitBodies(s.guestGate(s.routes().ServeMux)))
}

// routeMux is a ServeMux that remembers what is registered on it, so a test
// can walk every route as a guest.
type routeMux struct {
	*http.ServeMux
	patterns []string
}

func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.HandleFunc(pattern, h)
}

// routes builds the agent API's routes.
func (s *Server) routes() *routeMux {
	mux := &routeMux{ServeMux: http.NewServeMux()}
	mux.HandleFunc("POST /v1/send", s.handleSend)
	mux.HandleFunc("GET /v1/poll", s.handlePoll)
	mux.HandleFunc("POST /v1/replies/ack", s.handleAckReplies)
	mux.HandleFunc("POST /v1/requests/{id}/claim", s.handleClaim)
	mux.HandleFunc("POST /v1/requests/{id}/progress", s.handleProgress)
	mux.HandleFunc("POST /v1/requests/{id}/reply", s.handleReply)
	mux.HandleFunc("POST /v1/requests/{id}/answer", s.handleAnswer)
	mux.HandleFunc("POST /v1/requests/{id}/handback", s.handleHandback)
	mux.HandleFunc("GET /v1/requests/{id}", s.handleGet)
	mux.HandleFunc("GET /v1/groups/{id}", s.handleGroup)
	mux.HandleFunc("POST /v1/requests/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /v1/agents", s.handleAgents)
	mux.HandleFunc("PUT /v1/agents/self/web-status", s.handleWebStatus)
	mux.HandleFunc("PUT /v1/agents/self/working-on", s.handleWorkingOn)
	mux.HandleFunc("DELETE /v1/agents/self/working-on", s.handleWorkingOn)
	mux.HandleFunc("PUT /v1/agents/self/status", s.handleHostStatus)
	mux.HandleFunc("POST /v1/watches", s.handleWatch)
	mux.HandleFunc("POST /v1/leases", s.handleLeaseAcquire)
	mux.HandleFunc("POST /v1/schedules", s.handleCreateSchedule)
	mux.HandleFunc("GET /v1/rooms", s.handleRooms)
	mux.HandleFunc("GET /v1/rooms/{room}/posts", s.handleRoomRead)
	mux.HandleFunc("POST /v1/rooms/{room}/posts", s.handleRoomPost)
	mux.HandleFunc("GET /v1/whoami", s.handleWhoAmI)
	mux.HandleFunc("GET /v1/hello", s.handleHello)
	mux.HandleFunc("POST /v1/join", s.handleJoin)
	mux.HandleFunc("GET /v1/dist", s.handleDistManifest)
	mux.HandleFunc("GET /v1/dist/{name}", s.handleDistFile)
	mux.HandleFunc("POST "+uploadPath, s.handleUpload)
	s.adminRoutes(mux)
	return mux
}

// adminRoutes registers the routes served on both the tailnet API (for admin
// devices) and the local admin socket. /v1/agents is added by the caller.
func (s *Server) adminRoutes(mux interface {
	HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request))
}) {
	mux.HandleFunc("GET /v1/admin/wake-config", s.handleWakeSettings)
	mux.HandleFunc("PUT /v1/admin/wake-config/{agent}", s.handleWakeSettings)
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
	mux.HandleFunc("GET /v1/stats", s.handleStats)
	mux.HandleFunc("GET /v1/admin/audit/verify", s.handleVerify)
	mux.HandleFunc("GET /v1/admin/wakes/{agent}", s.handleWakes)
	mux.HandleFunc("POST /v1/admin/relay/upgrade", s.handleSelfUpgrade)
	mux.HandleFunc("GET /v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /v1/leases", s.handleLeases)
	mux.HandleFunc("DELETE /v1/leases", s.handleLeaseRelease)
	mux.HandleFunc("GET /v1/schedules", s.handleSchedules)
	mux.HandleFunc("DELETE /v1/schedules/{id}", s.handleCancelSchedule)
	mux.HandleFunc("GET /v1/attachments/{id}", s.handleApprovalFetch)
	mux.HandleFunc("GET "+phone.Prefix, s.handlePhone)
	mux.HandleFunc("GET /v1/admin/push/key", s.handlePushKey)
	mux.HandleFunc("PUT /v1/admin/push/subscription", s.handlePushSubscribe)
	mux.HandleFunc("DELETE /v1/admin/push/subscription", s.handlePushUnsubscribe)
	mux.HandleFunc("GET /v1/admin/rooms", s.handleAdminRooms)
	mux.HandleFunc("POST /v1/admin/rooms", s.handleCreateRoom)
	mux.HandleFunc("POST /v1/admin/rooms/{room}/members", s.handleRoomMember)
	mux.HandleFunc("DELETE /v1/admin/rooms/{room}/members", s.handleRoomMember)
}

// AdminHandler serves only admin routes and treats every caller as the local
// admin. Serve it on a unix socket on the relay host.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agents", s.handleAgents)
	mux.HandleFunc("PUT /v1/agents/self/web-status", s.handleWebStatus)
	mux.HandleFunc("PUT /v1/agents/self/working-on", s.handleWorkingOn)
	mux.HandleFunc("DELETE /v1/agents/self/working-on", s.handleWorkingOn)
	mux.HandleFunc("PUT /v1/agents/self/status", s.handleHostStatus)
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

// Run sweeps expired requests and leases, applies attachment retention at
// start and every Attachments.SweepEvery, and, when agents are opted in to
// request emails (SetEmailInboxes), polls their inboxes for replies at
// start and every EmailPollEvery, until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.SweepEvery)
	defer t.Stop()
	at := time.NewTicker(s.cfg.Attachments.SweepEvery)
	defer at.Stop()
	// The email reply poll talks to AgentMail and can wait out a 429 for
	// minutes, so it runs on its own loop: expiry and lease sweeps never
	// wait behind it. Run returns only after that loop has stopped.
	if s.hasEmailInboxes() {
		var wg sync.WaitGroup
		wg.Go(func() { s.pollEmailRepliesEvery(ctx) })
		defer wg.Wait()
	}
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

// pollEmailRepliesEvery polls for email replies at start and then every
// EmailPollEvery until ctx ends.
func (s *Server) pollEmailRepliesEvery(ctx context.Context) {
	mt := time.NewTicker(s.cfg.EmailPollEvery)
	defer mt.Stop()
	for {
		s.PollEmailReplies(ctx)
		select {
		case <-ctx.Done():
			return
		case <-mt.C:
		}
	}
}

// Sweep runs one expiry and lease pass and wakes affected waiters.
func (s *Server) Sweep(ctx context.Context) {
	s.notifyWebStatus(ctx)
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
		s.tellSwept(ctx, t)
		if t.Status == envelope.StatusQueued {
			s.hub.notify(inboxKey(t.To))
			// An agent woken by the relay has no poller to see the requeue,
			// so wake it again; pollers are skipped by the waker itself.
			s.wakeRequeued(ctx, envelope.Request{ID: t.ID, TraceID: t.TraceID, From: t.From, To: t.To, Urgent: t.Urgent})
		}
	}
	s.wakeFallenBack(ctx)
	s.sweepGuests(ctx)
	s.failoverSweep(ctx)
	s.sweepFollowOns(ctx)
	if err := s.store.ExpireWatches(ctx); err != nil {
		log.Printf("watches: expire: %v", err)
	}
	s.checkWatches(ctx, "")
	s.expireLeases(ctx)
	s.fireSchedules(ctx)
}

// wakeRequeued wakes req's target for a request it may have looked past: a
// recent poll does not prove anything holds it.
func (s *Server) wakeRequeued(ctx context.Context, req envelope.Request) {
	if rq, ok := s.events.(Requeuer); ok {
		rq.Requeued(ctx, req)
	} else if s.events != nil {
		s.events.Queued(ctx, req)
	}
}

// wakeFallenBack tells the targets of requests that just opened to any
// session. An agent woken by the relay polled when the ask arrived and was
// handed nothing, and that poll ended its follow-ups, so nothing else would
// send it back. The wake carries no label, like any other.
func (s *Server) wakeFallenBack(ctx context.Context) {
	reqs, err := s.store.FallenBack(ctx, SessionGrace)
	if err != nil {
		log.Printf("sweep: project fallback: %v", err)
		return
	}
	for _, req := range reqs {
		s.hub.notify(inboxKey(req.To))
		s.wakeRequeued(ctx, req)
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
	res, err := s.resolve(r)
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
	sess, sessErr := session(r)
	if sessErr == nil {
		s.touchSession(res.Name, sess)
	}
	if label, err := project(r); err == nil {
		s.touchProject(res.Name, label)
	}
	markHeld(r, res.Name)
	if r.Method == http.MethodGet && r.URL.Path == "/v1/poll" {
		supported, socketWake := false, false
		for f := range strings.SplitSeq(r.Header.Get(client.FeaturesHeader), ",") {
			switch strings.TrimSpace(f) {
			case "ping":
				supported = true
			case client.SocketWakeFeature:
				socketWake = true
			}
		}
		now := s.cfg.Now()
		s.pollMu.Lock()
		defer s.pollMu.Unlock()
		s.mu.Lock()
		if sessErr == nil && sess != "" {
			s.noteSocketWake(res.Name, sess, socketWake, now)
		}
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
	out := map[string]any{"name": name, "kind": a.Kind}
	// The key and addresses are how a client recognizes a moved relay, and a
	// guest is not to hold them.
	if a.Guest.Set() {
		out["guest_until"] = a.Guest.Until.UTC()
	} else {
		out["relay_key"], out["relay_urls"] = s.key, s.urls
	}
	if s.cfg.Version != "" {
		out["relay_version"] = s.cfg.Version
	}
	if v := s.upgradeFor(r); v != "" {
		out["upgrade_available"] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// errPhoneTakesNoRequests refuses a send addressed to a phone agent.
var errPhoneTakesNoRequests = errors.New("phone agents ask; they do not take requests")

// errGuestTakesNoRequests refuses a send addressed to a guest.
var errGuestTakesNoRequests = errors.New("guests ask; they do not take requests")

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
	if s.guestTermOver(w, r) {
		return
	}
	req, err := envelope.ParseSend(raw, from, envelope.DefaultMaxBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	guest, fromGuest := guestOf(r.Context())
	if fromGuest {
		if err := guestSendRefusal(guest, raw, req); err != nil {
			writeErr(w, http.StatusForbidden, err)
			return
		}
	}
	if len(req.Attachments) > 0 && s.blobs == "" {
		writeErr(w, http.StatusBadRequest, errAttachmentsOff)
		return
	}
	to, err := s.checkSendTarget(r.Context(), req.To)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	// Kinds can change after the invite, so a listed service is refused here
	// too.
	if fromGuest && roomServiceKinds[to.Kind] {
		writeErr(w, http.StatusForbidden, errors.New("this guest may not ask "+req.To))
		return
	}
	if req.Then != nil {
		if err := s.checkThen(r.Context(), to, req.Then.To); err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
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
	if req.FromSession, err = session(r); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req, err = s.submit(r.Context(), req); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	res := envelope.SendResponse{Request: req}
	if !fromGuest {
		// How a teammate wakes and what it last did are not a guest's to read.
		res.Target = s.withProjectMatch(s.recipientTarget(r.Context(), req.To), req)
	}
	writeJSON(w, http.StatusCreated, res)
}

// withProjectMatch adds to t whether a session of the project req names is
// live, read after submit so a request that moved reports nothing. A held
// request reports nothing either: no session can take it, and its fallback
// clock starts at approval. A relay too new to know its sessions adds nothing
// rather than say none is live.
func (s *Server) withProjectMatch(t *envelope.Target, req envelope.Request) *envelope.Target {
	if req.Project == "" || req.Status == envelope.StatusHeld {
		return t
	}
	now := s.cfg.Now()
	s.mu.Lock()
	seen, ok := s.projects[req.To][req.Project]
	s.mu.Unlock()
	match := envelope.ProjectNone
	switch {
	case ok && now.Sub(seen) < SessionGrace:
		match = envelope.ProjectLive
	case now.Sub(s.started) < SessionGrace:
		return t
	}
	if t == nil {
		t = &envelope.Target{}
	}
	t.ProjectMatch = match
	return t
}

// checkSendTarget returns the agent a send to name goes to, refusing unless
// it is a joined agent that takes requests. The errors carry their HTTP
// status (statusFor).
func (s *Server) checkSendTarget(ctx context.Context, name string) (identity.Agent, error) {
	to, ok, err := s.dir.Agent(ctx, name)
	if err != nil || !ok {
		return to, &StatusError{Code: http.StatusNotFound, Err: errors.New("no such agent: " + name)}
	}
	if to.Guest.Set() {
		// Guests ask; only the relay's own notices reach one.
		return to, &StatusError{Code: http.StatusBadRequest, Err: fmt.Errorf("%s: %w", name, errGuestTakesNoRequests)}
	}
	if to.Kind == onboard.KindPhone {
		// The phone page never reads an inbox, so a send to it would sit
		// unanswered. Replies to its own asks, and the relay's own notices,
		// do not come through here.
		return to, &StatusError{Code: http.StatusBadRequest, Err: fmt.Errorf("%s: %w", name, errPhoneTakesNoRequests)}
	}
	return to, nil
}

// submit takes a new request from req.From through every step a send
// takes: Prepare (chain, loop, hop and rate checks, the owner's holds),
// Enqueue, then either the hold's audit row and approval notice, or the
// target's held poll, the audit row and the wake. A send and a room
// mention both go through it, so a mention is exactly an ask. Its errors
// carry their HTTP status (statusFor).
func (s *Server) submit(ctx context.Context, req envelope.Request) (envelope.Request, error) {
	prep := s.preparer()
	if err := prep.Prepare(ctx, &req); err != nil {
		s.record(ctx, "rejected", "", req.TraceID, req.From, store.DetailJSON(map[string]any{"to": req.To, "reason": err.Error()}))
		return envelope.Request{}, err
	}
	prepared := req
	req, err := s.store.Enqueue(ctx, req, s.requestTTL(ctx, req.To))
	if err != nil {
		if rf, ok := prep.(Refunder); ok {
			rf.Refund(prepared)
		}
		if errors.Is(err, store.ErrGroupFull) {
			return envelope.Request{}, &StatusError{Code: http.StatusBadRequest, Err: err}
		}
		return envelope.Request{}, &StatusError{Code: attachmentStatus(err, http.StatusInternalServerError), Err: err}
	}
	s.recordPassSent(ctx, req)
	if req.Status == envelope.StatusHeld {
		s.record(ctx, "held", req.ID, req.TraceID, req.From, store.DetailJSON(map[string]any{"to": req.To, "hop": req.Hop, "chain": req.Chain}))
		s.notifyApproval(ctx, req)
		return req, nil
	}
	s.record(ctx, "queued", req.ID, req.TraceID, req.From, store.DetailJSON(map[string]any{"to": req.To, "hop": req.Hop, "chain": req.Chain}))
	// An ask to a web agent that is out of messages moves before anyone
	// is told of it; the move wakes the new target itself.
	moved := false
	if req, moved = s.failoverQueued(ctx, req); !moved {
		s.hub.notify(inboxKey(req.To))
		if s.events != nil {
			s.events.Queued(ctx, req)
		}
	}
	return req, nil
}

// recipientTarget is the schedule facts for agent on wake method schedule,
// its last wake for an agent the relay wakes, and how it wakes and whether
// it is listening for every agent whose wake method the relay knows; nil
// when it has none of these. A failed join-time lookup measures from the
// relay's start.
func (s *Server) recipientTarget(ctx context.Context, agent string) *envelope.Target {
	t := s.recipientActivity(ctx, agent)
	if via, listening := s.wakeVia(agent); via != "" {
		if t == nil {
			t = &envelope.Target{}
		}
		t.WakeVia, t.Listening = via, listening
	}
	facts, err := s.store.WebStatuses(ctx)
	if err == nil {
		if a, ok, err := s.dir.Agent(ctx, agent); err == nil && ok {
			if v, exists := facts[agent]; exists {
				if t == nil {
					t = &envelope.Target{}
				}
				v.Apply(a, t, s.cfg.Now())
			}
		}
	}
	return t
}

// wakeVia is how the relay reaches agent: "socket" while one of its
// sessions is still peeking with the socket wake advert, else its
// configured wake method, and "" with no wake config. listening is the
// roster's online test. A session whose socket kept failing stops peeking
// and keeps itself live with whoami, which carries no advert, so the
// socket record counts only as long as a poll would, not for SessionGrace.
func (s *Server) wakeVia(agent string) (via string, listening bool) {
	if s.wake != nil {
		via = s.wake.WakeMethod(agent)
	}
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.socketWake[agent] {
		if s.polledRecently(t, now) {
			via = envelope.WakeViaSocket
			break
		}
	}
	return via, s.polledRecently(s.lastPoll[agent], now)
}

func (s *Server) recipientActivity(ctx context.Context, agent string) *envelope.Target {
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
	last := s.lastPollSince(agent, wk.At)
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
	sess, err := session(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	label, err := project(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	scope := store.QueueScope{Project: label, Grace: SessionGrace}
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
			if reps, more, err = s.unseenReplies(r.Context(), name, s.replyScope(name, sess)); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
		}
		if peek {
			// Report what is waiting without delivering it, so a listener
			// can nudge the agent and the agent's own check_inbox still
			// receives it. waiting counts the replies it was asked for.
			n, pings, err := s.store.CountQueuedWithPingsFor(r.Context(), name, scope)
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
					urgent, err := s.store.CountUrgentQueuedFor(r.Context(), name, scope)
					if err != nil {
						writeErr(w, http.StatusInternalServerError, err)
						return
					}
					if urgent > 0 {
						out["urgent"] = urgent
					}
					pending, err := s.store.PendingRequestsFor(r.Context(), name, scope, MaxPeekPending)
					if err != nil {
						writeErr(w, http.StatusInternalServerError, err)
						return
					}
					out["pending"] = pending
				}
				if hidden, ids := s.hiddenQueued(r.Context(), name, scope, n); hidden > 0 {
					out["hidden"], out["hidden_ids"] = hidden, ids
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
			v := s.upgradeFor(r)
			hidden, ids := s.hiddenQueued(r.Context(), name, scope, 0)
			if v == "" && hidden == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			out := map[string]any{"waiting": 0, "queued": 0}
			if v != "" {
				out["upgrade_available"] = v
			}
			if hidden > 0 {
				out["hidden"], out["hidden_ids"] = hidden, ids
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		reqs, err := s.store.DeliverFor(r.Context(), name, scope, 20, s.cfg.DeliveryLease)
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
// MaxRepliesBytes, and how many more are waiting beyond them, as scope sees
// them.
func (s *Server) unseenReplies(ctx context.Context, agent string, scope store.ReplyScope) ([]envelope.Result, int, error) {
	reps, err := s.store.UnseenRepliesFor(ctx, agent, scope)
	if err != nil {
		return nil, 0, err
	}
	if err := s.redactRooms(ctx, reps, agent); err != nil {
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
	total, err := s.store.CountUnseenRepliesFor(ctx, agent, scope)
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

// maxSession bounds a SessionHeader value.
const maxSession = 64

// SessionGrace is how long a session may go without calling the relay
// before its unread replies open to the agent's other sessions. A channel
// MCP server peeks every DefaultPollHold, so a live window stays well
// inside it; a window that closed or restarted its MCP server (and so took
// a new session id) stops calling, and after SessionGrace whichever window
// polls next gets the reply instead of nobody.
const SessionGrace = 10 * time.Minute

// touchSession records that agent's session called the relay now, and
// forgets the agent's sessions that have been quiet past SessionGrace.
func (s *Server) touchSession(agent, session string) {
	if session == "" {
		return
	}
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.sessions[agent]
	if m == nil {
		m = map[string]time.Time{}
		s.sessions[agent] = m
	}
	m[session] = now
	for k, t := range m {
		if now.Sub(t) >= SessionGrace {
			delete(m, k)
		}
	}
}

// touchProject records that one of agent's sessions advertising label called
// the relay now, and forgets the agent's labels quiet past SessionGrace.
func (s *Server) touchProject(agent, label string) {
	if label == "" {
		return
	}
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.projects[agent]
	if m == nil {
		m = map[string]time.Time{}
		s.projects[agent] = m
	}
	m[label] = now
	for k, t := range m {
		if now.Sub(t) >= SessionGrace {
			delete(m, k)
		}
	}
}

// hiddenQueued is how many of agent's queued requests a peek that saw seen of
// them left out for naming another project, and up to MaxPeekPending of their
// ids, so a client can tell a request still queued from one that left. Fewer
// ids than the count tells it the list is not complete. A failed count reads
// as none: the peek still reports what it saw.
func (s *Server) hiddenQueued(ctx context.Context, agent string, scope store.QueueScope, seen int) (int, []string) {
	total, _, err := s.store.CountQueuedWithPings(ctx, agent)
	if err != nil {
		log.Printf("peek: count queued: %v", err)
		return 0, nil
	}
	ids, err := s.store.HiddenIDsFor(ctx, agent, scope, MaxPeekPending)
	if err != nil {
		log.Printf("peek: list hidden: %v", err)
	}
	return max(total-seen, len(ids)), ids
}

// noteSocketWake records whether agent's session advertised socket wake on
// a poll at now, and forgets the agent's socket-wake sessions quiet past
// SessionGrace. The caller holds s.mu.
func (s *Server) noteSocketWake(agent, session string, on bool, now time.Time) {
	m := s.socketWake[agent]
	if on {
		if m == nil {
			m = map[string]time.Time{}
			s.socketWake[agent] = m
		}
		m[session] = now
	} else {
		delete(m, session)
	}
	for k, t := range m {
		if now.Sub(t) >= SessionGrace {
			delete(m, k)
		}
	}
	if len(m) == 0 {
		delete(s.socketWake, agent)
	}
}

// replyScope is which of agent's unseen replies session sees: its own,
// those to requests sent with no session, and those to sessions that have
// not called within SessionGrace. Session activity is not persisted, so
// for SessionGrace after the relay starts every other session counts as
// live rather than gone.
func (s *Server) replyScope(agent, session string) store.ReplyScope {
	scope := store.ReplyScope{Session: session}
	if session == "" {
		return scope
	}
	now := s.cfg.Now()
	if now.Sub(s.started) < SessionGrace {
		scope.AllLive = true
		return scope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.sessions[agent] {
		if k != session && now.Sub(t) < SessionGrace {
			scope.Live = append(scope.Live, k)
		}
	}
	slices.Sort(scope.Live)
	return scope
}

// session returns the caller's SessionHeader, "" when it sends none. A
// session only routes replies and files asks inside the caller's agent;
// identity still comes from Server.agent alone. A malformed value is an
// error rather than ignored, so a bad TINCAN_SESSION does not look like
// sessions failing to scope.
func session(r *http.Request) (string, error) {
	v := r.Header.Get(client.SessionHeader)
	if len(v) > maxSession {
		return "", fmt.Errorf("%s is longer than %d bytes", client.SessionHeader, maxSession)
	}
	for _, c := range v {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return "", fmt.Errorf("%s may use only letters, digits, '.', '_', and '-'", client.SessionHeader)
		}
	}
	return v, nil
}

// project returns the caller's ProjectHeader, "" when it sends none. Like a
// session, a label only routes inside the caller's agent; identity still
// comes from Server.agent alone. A malformed value is an error rather than
// ignored, so a bad label does not look like routing failing.
func project(r *http.Request) (string, error) {
	v := r.Header.Get(client.ProjectHeader)
	if err := envelope.ValidProject(v); err != nil {
		return "", fmt.Errorf("%s: %w", client.ProjectHeader, err)
	}
	return v, nil
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	sess, err := session(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, guest := guestOf(r.Context()); guest {
		q, _, err := s.store.Request(r.Context(), r.PathValue("id"))
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		if q.To != name || q.From != client.RelaySender {
			writeErr(w, http.StatusForbidden, errGuestRoute)
			return
		}
	}
	label, err := project(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req, err := s.store.ClaimLeasesFrom(r.Context(), r.PathValue("id"), name, sess, store.QueueScope{Project: label, Grace: SessionGrace}, s.cfg.ClaimLease, s.cfg.urgentLease())
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
	if err := s.store.SetProgressLeases(r.Context(), id, name, in.Note, s.cfg.ClaimLease, s.cfg.urgentLease()); err != nil {
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
	rep, err = s.recordReply(r.Context(), r.PathValue("id"), name, rep, "", 0)
	if err != nil {
		writeErr(w, attachmentStatus(err, statusFor(err)), err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// recordReply stores agent's reply rep to request id and tells everyone who
// waits on it: the request's own waiters, the audit log, the asker's held
// poll and the asker's reply wake. It is the one path for every reply, so
// a reply over tincan (handleReply, emailMessageID "") and a reply by email
// (emailMessageID is the AgentMail message it came in) cannot drift apart.
// An email reply is stored with its email_replies row in one transaction
// (store.ReplyByEmail) and audited with via "email"; its tag, sender and
// text never reach the audit log.
func (s *Server) recordReply(ctx context.Context, id, agent string, rep envelope.Reply, emailMessageID string, round int) (envelope.Reply, error) {
	return s.recordReplyWith(ctx, id, agent, rep, emailMessageID != "", func(rep envelope.Reply) (envelope.Reply, error) {
		if emailMessageID == "" {
			return s.store.Reply(ctx, id, agent, rep)
		}
		return s.store.ReplyByEmail(ctx, emailMessageID, id, agent, round, rep)
	})
}

// recordReplyWith is recordReply with the store write given as write, for
// a reply that needs its own conditions checked in the write's transaction
// (failOutlivedHold). byEmail audits it with via "email".
func (s *Server) recordReplyWith(ctx context.Context, id, agent string, rep envelope.Reply, byEmail bool, write func(envelope.Reply) (envelope.Reply, error)) (envelope.Reply, error) {
	rep.NoMirror = !s.mayMirror(ctx, id, agent)
	rep, err := write(rep)
	if err != nil {
		return envelope.Reply{}, err
	}
	s.hub.notify(requestKey(id))
	event := "replied"
	detail := map[string]any{"status": rep.Status}
	if rep.Status == envelope.StatusNeedsInput {
		event = "needs_input"
		detail = map[string]any{"question_bytes": len(rep.Body)}
	}
	if byEmail {
		detail["via"] = "email"
	}
	s.record(ctx, event, id, "", agent, store.DetailJSON(detail))
	// The asker learns of the reply from a held poll now, or from a wake
	// once the waker's grace period shows it went unread.
	if req, _, err := s.store.Request(ctx, id); err == nil {
		s.hub.notify(inboxKey(req.From))
		if rp, ok := s.events.(Replier); ok && req.Kind != envelope.KindPing {
			rp.Replied(ctx, req)
		}
		if req.RoomPostID != 0 {
			s.auditMirror(ctx, rep)
		}
	} else {
		log.Printf("reply %s: look up asker: %v", id, err)
	}
	// The reply may have freed the replying agent, if someone watches it.
	s.checkWatches(ctx, agent)
	if rep.Status == envelope.StatusAnswered {
		s.fireFollowOn(ctx, id)
	}
	return rep, nil
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
		res, err := s.getAs(r.Context(), id, name)
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
		res, err = s.getAs(r.Context(), id, name)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
}

// getAs is the store's Get with a mention's room text hidden from a reader
// that may not read the room now (redactRooms).
func (s *Server) getAs(ctx context.Context, id, reader string) (envelope.Result, error) {
	res, err := s.store.Get(ctx, id, reader)
	if err != nil {
		return res, err
	}
	one := []envelope.Result{res}
	err = s.redactRooms(ctx, one, reader)
	return one[0], err
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	id := r.PathValue("id")
	cancel := s.store.Cancel
	if g, guest := guestOf(r.Context()); guest && strings.HasPrefix(g.NodeID, identity.VirtualPrefix) {
		cancel = s.store.CancelUntaken
	}
	if err := cancel(r.Context(), id, name); err != nil {
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
// Forget when the removal fails. Joined clears Forget's marker when the
// name is bound again, so request follow-ups can be scheduled. Set by
// package wake through SetWakeNamer.
type WakeReporter interface {
	LastWake(agent string) (store.Wake, bool)
	Forget(agent string)
	Unforget(agent string)
	Joined(agent string)
}

// WakeResumer schedules the wakes an agent's waiting work calls for, as a
// restarted relay does. Set by package wake through SetWakeNamer.
type WakeResumer interface {
	RequestsWaiting(agent string)
	ReplyWaiting(agent string)
}

// DefaultWakeGrace is how long a woken agent has to poll before the roster
// shows it as unanswered and the waker sends the same path again.
const DefaultWakeGrace = 10 * time.Minute

// DefaultUrgentWakeGrace is the wake grace while an urgent request to the
// agent is queued.
const DefaultUrgentWakeGrace = 2 * time.Minute

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
	t.Unanswered = !envelope.WakeAccepted(wk.Result) || now.Sub(since) > s.cfg.WakeGrace
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
// which need not be joined (onboarding runs from an admin laptop). Admin
// devices also get each agent's node fields. The caller's own entry is
// marked self. An admin device is not attributed here, so a device that
// never joined still gets the roster; its entry is only looked up.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	admin := s.isAdmin(r)
	var self string
	if admin {
		self = s.dir.BoundAgent(r.Context(), s.remote(r), r.Header.Get(client.AgentHeader))
	} else if self = s.agent(w, r); self == "" {
		return
	}
	var out []client.AgentInfo
	var err error
	if g, guest := guestOf(r.Context()); guest {
		out, err = s.guestRoster(r.Context(), g)
	} else {
		out, err = s.roster(r.Context(), admin)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for i := range out {
		out[i].Self = out[i].Name == self
	}
	res := map[string]any{"agents": out}
	if s.cfg.Version != "" {
		res["relay_version"] = s.cfg.Version
	}
	writeJSON(w, http.StatusOK, res)
}

// roster builds every joined agent's roster entry, with node fields only
// for an admin caller. Only the directory read can fail it; the other
// reads are logged and the entries carry what the relay still knows.
func (s *Server) roster(ctx context.Context, admin bool) ([]client.AgentInfo, error) {
	out, err := s.rosterWith(ctx, admin, false)
	if err != nil {
		return nil, err
	}
	s.addStats(ctx, out)
	return out, nil
}

// rosterWith builds the roster. Display callers take a failed queue read
// as no backlog; with needQueue it returns the error instead, for callers
// that act on the counts.
func (s *Server) rosterWith(ctx context.Context, admin, needQueue bool) ([]client.AgentInfo, error) {
	agents, err := s.dir.Agents(ctx)
	if err != nil {
		return nil, err
	}
	persisted, err := s.store.AgentsLastSeen(ctx)
	if err != nil {
		// The in-memory times still answer for this run of the relay.
		log.Printf("agents last seen: %v", err)
	}
	stats, err := s.store.QueueStats(ctx)
	if err != nil {
		if needQueue {
			return nil, fmt.Errorf("queue stats: %w", err)
		}
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
		if polls, err = s.store.AgentsLastPoll(ctx); err != nil {
			log.Printf("agents last poll: %v", err)
		}
	}
	webFacts, err := s.store.WebStatuses(ctx)
	if err != nil {
		log.Printf("agents web status: %v", err)
	}
	quietUntil := s.started.Add(s.cfg.ClaimLease)
	out := make([]client.AgentInfo, 0, len(agents))
	s.mu.Lock()
	for _, a := range agents {
		// Teammates never see a guest on the roster; an admin does.
		if a.Guest.Set() && !admin {
			continue
		}
		last := s.lastPoll[a.Name]
		active := s.lastSeen[a.Name]
		if p := persisted[a.Name]; p.After(active) {
			active = p
		}
		info := client.AgentInfo{Name: a.Name, LastPoll: last, LastActive: active, Online: s.polledRecently(last, now), Wake: "none", Kind: a.Kind, GoodAt: a.GoodAt, Version: s.versions[a.Name], GuestUntil: a.Guest.Until}
		if info.GoodAt == "" {
			info.GoodAt = onboard.StockGoodAt(a.Name, a.Kind)
		}
		// Only an admin device sees which node an agent runs on; a virtual
		// agent has none to show.
		if admin && !strings.HasPrefix(a.NodeID, identity.VirtualPrefix) {
			info.NodeID, info.NodeName, info.NodeUser = a.NodeID, a.NodeName, a.NodeUser
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
		webFacts[a.Name].Apply(a, &info.Target, now)
		info.WorkingOn, info.WorkingOnOthers = s.currentWorkingOn(a.Name, now)
		applyStatus(&info, computeStatus(statusFacts{online: info.Online, wake: info.Wake, stat: stat, target: info.Target, hostBusy: s.hostBusy(a.Name, now)}, now, quietUntil))
		// The same rule for a guest named as the asker: still busy, for no
		// one a teammate may see.
		if !admin && guestAsker(agents, info.BusyFor) {
			info.BusyFor = ""
		}
		out = append(out, info)
	}
	s.mu.Unlock()
	return out, nil
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
	if wr, ok := s.wake.(WakeReporter); ok {
		wr.Joined(name)
	}
	joined := ""
	if a, ok, err := s.dir.Agent(r.Context(), name); err == nil && ok && a.Guest.Set() {
		joined = store.DetailJSON(map[string]any{"guest_until": a.Guest.Until.UTC()})
	}
	s.record(r.Context(), "joined", "", "", name, joined)
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		// NewOnly refuses a name bound to another machine instead of
		// minting a code that would move that agent (the phone page).
		NewOnly bool `json:"new_only"`
		// Guest makes it a guest invite. An older relay ignores it, so a
		// client sends it only after the guests capability.
		Guest *client.GuestInvite `json:"guest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Guest != nil {
		s.inviteGuest(w, r, in.Name, in.Kind, *in.Guest)
		return
	}
	if err := knownKind(in.Kind); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	invite := s.dir.InviteKind
	if in.NewOnly {
		invite = s.dir.InviteNew
	}
	code, err := invite(r.Context(), s.remote(r), in.Name, in.Kind)
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
	leases := s.leasesOf(r.Context(), in.Name)
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
	delete(s.sessions, in.Name)
	delete(s.projects, in.Name)
	delete(s.socketWake, in.Name)
	delete(s.workingOn, in.Name)
	delete(s.hostStatus, in.Name)
	s.mu.Unlock()
	s.record(r.Context(), "removed", "", "", in.Name, store.DetailJSON(map[string]any{"cancelled": len(ids)}))
	for _, l := range leases {
		s.record(r.Context(), "lease_released", "", "", in.Name, store.DetailJSON(map[string]any{"resource": l.Resource, "holder": l.Holder, "reason": "removed"}))
	}
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

// UrgentQueuedCount returns how many of agent's queued requests are urgent.
// The waker asks it to pick the grace for a request follow-up. A failed
// count reports one, since a spare nudge costs less than a late urgent one.
func (s *Server) UrgentQueuedCount(agent string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := s.store.CountUrgentQueued(ctx, agent)
	if err != nil {
		log.Printf("urgent queued requests for %s: %v", agent, err)
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

// QueuedIDs lists the ids of agent's requests still waiting to be
// delivered, oldest first: the requests QueuedCount counts. The waker names
// them in a woke audit row.
func (s *Server) QueuedIDs(agent string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.store.QueuedIDs(ctx, agent)
}

// UnseenReplyIDs lists the ids of agent's own requests whose replies it has
// not read yet: the replies UnseenReplies counts. The waker names them in a
// woke audit row.
func (s *Server) UnseenReplyIDs(agent string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.store.UnseenReplyIDs(ctx, agent)
}

// LastPoll is agent's last inbox poll, including peek: the later of this
// process's memory and the poll persisted for a restart. The waker uses it
// as proof of life for request follow-up. The store is read only when this
// process has not seen a poll.
func (s *Server) LastPoll(agent string) time.Time {
	return s.lastPollSince(agent, time.Time{})
}

// lastPollSince is agent's last poll for judging a wake at since. Memory
// answers when it already has a poll at or after since (or any poll, when
// since is zero). The store is read only when memory cannot: this process
// has never recorded a poll, or its poll is still older than the wake.
func (s *Server) lastPollSince(agent string, since time.Time) time.Time {
	s.mu.Lock()
	last := s.lastPoll[agent]
	s.mu.Unlock()
	if !last.IsZero() && (since.IsZero() || !last.Before(since)) {
		return last
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if p, err := s.store.AgentLastPoll(ctx, agent); err == nil && p.After(last) {
		return p
	}
	return last
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
	case errors.Is(err, store.ErrNotFound), errors.Is(err, identity.ErrUnknownAgent), errors.Is(err, store.ErrNoRoom):
		return http.StatusNotFound
	case errors.Is(err, store.ErrForbidden), errors.Is(err, identity.ErrNotAdmin), errors.Is(err, identity.ErrNotJoined),
		errors.Is(err, identity.ErrAgentAmbiguous), errors.Is(err, store.ErrNotRoomMember):
		return http.StatusForbidden
	case errors.Is(err, store.ErrWrongState), errors.Is(err, store.ErrRoomExists), errors.Is(err, identity.ErrNameTaken),
		errors.Is(err, identity.ErrGuestTier), errors.Is(err, identity.ErrGuestNode):
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

// decodeOne decodes exactly one JSON object from r into v, rejecting
// unknown fields and anything after the object, so a small agent-facing
// body cannot carry fields or values the relay would silently drop. The
// trailing-data error reads "expected one <noun>".
func decodeOne(r io.Reader, v any, noun string) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("expected one " + noun)
	}
	return nil
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
			steps[i].RedactPassFor(name)
		}
		if !slices.Contains(store.Participants(steps), name) {
			writeErr(w, http.StatusNotFound, errors.New("no such trace"))
			return
		}
		if err := s.redactRooms(r.Context(), steps, name); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
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
	steps, err := s.store.RecentTraces(r.Context(), limit, r.URL.Query().Get("exclude_pings") == "true", r.URL.Query().Get("from"))
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
		writeErr(w, http.StatusNotFound, errors.New("the MCP gateway is not enabled on this relay (start the relay with --chatgpt-gateway)"))
		return
	}
	var in struct {
		Name string `json:"name"`
		// Replace lets the connect take a name held by a real tailnet node.
		// Optional: an older CLI omits it and gets the refusal.
		Replace bool `json:"replace,omitempty"`
		// Guest makes this a gateway guest. A client sends it only to a relay
		// that advertises gateway_guests.
		Guest *client.GuestInvite `json:"guest,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	if in.Guest != nil {
		s.connectGuest(w, r, in.Name, in.Replace, *in.Guest)
		return
	}
	code, url, err := s.conn.Connect(r.Context(), in.Name, in.Replace)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The roster shows only stored kinds, and a gateway agent has no shell to
	// run tincan kind from, so connecting a product name (sesame, chatgpt)
	// stores the kind that name implies when the agent has none yet. A kind
	// the owner already set is kept. The kind is only a roster label, so a
	// failure to store it is logged and the connect still succeeds.
	if kind := onboard.RuntimeKind(in.Name); kind != "" {
		if a, ok, err := s.dir.Agent(r.Context(), in.Name); err != nil {
			log.Printf("connect %s: read kind: %v", in.Name, err)
		} else if ok && a.Kind == "" {
			if err := s.dir.SetKind(r.Context(), s.remote(r), in.Name, kind); err != nil {
				log.Printf("connect %s: store kind %s: %v", in.Name, kind, err)
			}
		}
	}
	s.record(r.Context(), "connected", "", "", in.Name, "")
	writeJSON(w, http.StatusOK, map[string]string{"name": in.Name, "code": code, "url": url, "expires_in": "10m0s"})
}

func approvalPreview(body string) string {
	return string([]rune(body)[:min(200, len([]rune(body)))])
}

// heldTextBudget is how much of the held listing long text may take on the
// wire. A client, however old, fails to decode a response past its limit and
// lists nothing, so the rest of the limit is left to the other fields. One
// follow-on always fits, so the oldest is always whole.
const heldTextBudget = client.MaxResponseBytes - 1<<20

// errHeldTextCut refuses approving text the listing could not show.
var errHeldTextCut = errors.New("this request's whole text is not shown yet: decide older held requests, then list again")

// heldCut sets each held request's follow-on and returns the ids of those
// whose long text, a follow-on's or guest's body and any then, did not fit
// heldTextBudget. reqs is oldest first and everything after the first that
// does not fit is cut too, so deciding a request never cuts one the owner
// has already read.
func (s *Server) heldCut(ctx context.Context, reqs []envelope.Request) map[string]bool {
	cut := map[string]bool{}
	spent := 0
	for i := range reqs {
		req := &reqs[i]
		if then, ok, err := s.store.FollowOnOf(ctx, req.ID); err != nil {
			log.Printf("held %s: follow-on: %v", req.ID, err)
		} else if ok {
			req.Then = &then
		}
		var texts []string
		if req.FollowOf != "" || req.FromGuest {
			texts = append(texts, req.Body)
		}
		if req.Then != nil {
			texts = append(texts, req.Then.Body)
		}
		if len(texts) == 0 {
			continue
		}
		for _, text := range texts {
			wire, _ := json.Marshal(text) // the size it has in the response
			spent += len(wire)
		}
		if spent > heldTextBudget {
			cut[req.ID] = true
		}
	}
	return cut
}

// cutHeldText shortens text the listing has no room for and says so.
func cutHeldText(text string) string {
	short := approvalPreview(text)
	return fmt.Sprintf("%s\n\n[%d more bytes not shown, and it cannot be approved yet: decide older held requests, then list again to read the rest.]", short, len(text)-len(short))
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
	// names and sizes already ride on the request. An ask to a pass agent
	// also carries the chat and destination parsed from its whole body, as
	// the pass service will read them, or why it will not. Old clients
	// ignore to_kind, pass and pass_error.
	// A tailnet caller also gets one approve and one deny nonce per entry,
	// bound to its device; a browser (the phone page) must send the nonce
	// back to decide. Every tailnet caller gets them, not only browser-marked
	// ones: over plain http a browser's same-origin GET carries no Origin and
	// no Sec-Fetch-*, so the listing cannot tell the page from the CLI. The
	// local admin socket has no device and gets none; old clients ignore them.
	type heldRequest struct {
		envelope.Request
		ToKind         string    `json:"to_kind,omitempty"`
		Pass           *PassRef  `json:"pass,omitempty"`
		PassError      string    `json:"pass_error,omitempty"`
		ApproveNonce   string    `json:"approve_nonce,omitempty"`
		DenyNonce      string    `json:"deny_nonce,omitempty"`
		NonceExpiresAt time.Time `json:"nonce_expires_at,omitzero"`
	}
	var node string
	if !isLocalAdmin(r.Context()) {
		node = s.callerNode(r).ID
	}
	now := s.cfg.Now()
	out := make([]heldRequest, len(reqs))
	cut := s.heldCut(r.Context(), reqs)
	kinds := map[string]string{} // target -> kind, for the lookups that succeeded
	for i, req := range reqs {
		full := req.Body
		// A follow-on's reply starts past any preview, and approving
		// releases it, so the owner reads the whole body, or is told it is
		// cut and cannot approve it yet. The same goes for anything a guest
		// wrote: nothing past a preview goes unread.
		switch {
		case req.FollowOf == "" && !req.FromGuest:
			req.Body = approvalPreview(req.Body)
		case cut[req.ID]:
			req.Body = cutHeldText(req.Body)
		}
		if cut[req.ID] && req.Then != nil {
			req.Then.Body = cutHeldText(req.Then.Body)
		}
		out[i] = heldRequest{Request: req}
		if node != "" {
			out[i].ApproveNonce, out[i].NonceExpiresAt = s.nonces.issue(nonceKey{id: req.ID, decision: decisionApprove, node: node}, now)
			out[i].DenyNonce, _ = s.nonces.issue(nonceKey{id: req.ID, decision: decisionDeny, node: node}, now)
		}
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
		if kind == onboard.KindPass && s.cfg.ParsePass != nil {
			if p, err := s.cfg.ParsePass(full); err != nil {
				out[i].PassError = err.Error()
			} else {
				out[i].Pass = &p
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// recordPassSent audits a pass seed (a request grouped pass-<id>) as
// pass_sent with its target and size, never its text. When it continues
// an ask to a pass agent, the chat's source and id come from that ask.
func (s *Server) recordPassSent(ctx context.Context, req envelope.Request) {
	if !strings.HasPrefix(req.Group, envelope.PassGroupPrefix) {
		return
	}
	detail := map[string]any{"to": req.To, "bytes": len(req.Body)}
	if req.ParentID != "" && s.cfg.ParsePass != nil {
		if parent, _, err := s.store.Request(ctx, req.ParentID); err == nil {
			if a, ok, err := s.lookupAgent(ctx, parent.To); err == nil && ok && a.Kind == onboard.KindPass {
				if p, err := s.cfg.ParsePass(parent.Body); err == nil {
					detail["source"], detail["conversation_id"] = p.Source, p.ConversationID
				}
			}
		}
	}
	s.record(ctx, "pass_sent", req.ID, req.TraceID, req.From, store.DetailJSON(detail))
}

// decisionBody is the body of an approve or deny. Reason is the owner's
// note on a denial. Nonce is required from a browser (the phone page): one
// the held listing issued to this device for this request and decision.
type decisionBody struct {
	Reason string `json:"reason"`
	Nonce  string `json:"nonce"`
}

// readDecision reads an approve or deny body, which the CLI may leave
// empty. It writes the error and returns false when the body is bad.
func readDecision(w http.ResponseWriter, r *http.Request) (decisionBody, bool) {
	var in decisionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, envelope.DefaultMaxBody+1024)).Decode(&in); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, err)
		return in, false
	}
	if len(in.Reason) > envelope.DefaultMaxBody {
		writeErr(w, http.StatusBadRequest, envelope.ErrBodyTooLarge)
		return in, false
	}
	return in, true
}

// handleApprove releases a held request. A browser must send a nonce from
// the held listing, and gets the final state back, not an error, when the
// hold had already ended (a second tap, another device, the CLI).
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	page := browserMarked(r)
	var in decisionBody
	if page {
		var ok bool
		if in, ok = readDecision(w, r); !ok {
			return
		}
	}
	s.Sweep(r.Context())
	id := r.PathValue("id")
	reqs, err := s.store.Held(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if s.heldCut(r.Context(), reqs)[id] {
		writeErr(w, http.StatusPreconditionFailed, errHeldTextCut)
		return
	}
	if page && !s.takeDecisionNonce(r, id, decisionApprove, in.Nonce) {
		s.writeDecided(w, r, id)
		return
	}
	ttl := s.cfg.RequestTTL
	if held, _, err := s.store.Request(r.Context(), id); err == nil {
		ttl = s.requestTTL(r.Context(), held.To)
	}
	req, err := s.store.Release(r.Context(), id, ttl)
	if err != nil {
		s.writeDecisionErr(w, r, id, page, err)
		return
	}
	s.nonces.void(req.ID)
	s.record(r.Context(), "approved", req.ID, req.TraceID, s.remote(r), s.deviceDetail(r))
	s.hub.notify(requestKey(req.ID))
	s.hub.notify(inboxKey(req.To))
	if s.events != nil {
		s.events.Queued(r.Context(), req)
	}
	if page {
		writeJSON(w, http.StatusOK, pageDecision{ID: req.ID, Status: envelope.StatusQueued})
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// handleDeny declines a held request with the owner's reason; a browser
// needs a nonce as for handleApprove.
func (s *Server) handleDeny(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	in, ok := readDecision(w, r)
	if !ok {
		return
	}
	page := browserMarked(r)
	s.Sweep(r.Context())
	id := r.PathValue("id")
	if page && !s.takeDecisionNonce(r, id, decisionDeny, in.Nonce) {
		s.writeDecided(w, r, id)
		return
	}
	req, err := s.store.DenyHeld(r.Context(), id, in.Reason)
	if err != nil {
		s.writeDecisionErr(w, r, id, page, err)
		return
	}
	s.nonces.void(req.ID)
	s.record(r.Context(), "denied", req.ID, req.TraceID, s.remote(r), s.deviceDetail(r))
	s.hub.notify(requestKey(req.ID))
	s.hub.notify(inboxKey(req.From))
	if replier, ok := s.events.(Replier); ok {
		replier.Replied(r.Context(), req)
	}
	if page {
		writeJSON(w, http.StatusOK, pageDecision{ID: req.ID, Status: envelope.StatusDeclined})
		return
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

// notifyApproval tells the owner a request is held: through the approval
// policy's notify teammate, then by push to every subscribed admin phone.
// Neither carries the request's text, and neither can change the hold.
func (s *Server) notifyApproval(ctx context.Context, held envelope.Request) {
	s.notifyOperator(ctx, held)
	s.queuePush()
}

// notifyOperator sends the approval policy's notify teammate a relay
// notice naming the held request.
func (s *Server) notifyOperator(ctx context.Context, held envelope.Request) {
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

// WakeExport is an agent's wake history for a window, as GET
// /v1/admin/wakes/{agent} returns it. It never carries a wake URL, token or
// key: only what the audit rows already hold.
type WakeExport struct {
	Agent string      `json:"agent"`
	Since time.Time   `json:"since"`
	Until time.Time   `json:"until"`
	Wakes []WakeEntry `json:"wakes"`
	// Truncated is set when the window held more wakes than one export
	// lists, by count (maxWakeExportWakes) or by encoded size
	// (maxWakeExportBytes, since each wake carries all its request ids):
	// the wakes listed are the oldest, each whole, and a later since shows
	// the rest.
	Truncated bool `json:"truncated,omitempty"`
}

// maxWakeExportWakes bounds the wakes one export lists, and so its memory,
// its lookups and its response size. Tests lower it.
var maxWakeExportWakes = 1000

// maxWakeExportBytes bounds an export's encoded size, comfortably below the
// 4 MiB a client reads of a response. An export stops before the wake that
// would pass it, but always lists at least one wake. Tests lower it.
var maxWakeExportBytes = 3 << 20

// pollTime is when the poll an audit row records happened: the time in a
// polled row written after its wake call ended ("polled at <time>, ..."),
// otherwise the row's own time.
func pollTime(e store.AuditEvent) time.Time {
	if rest, ok := strings.CutPrefix(e.Detail, "polled at "); ok {
		if ts, _, _ := strings.Cut(rest, ","); ts != "" {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				return t.UTC()
			}
		}
	}
	return e.At
}

// WakeEntry is one wake the relay sent, failed to send, or skipped.
type WakeEntry struct {
	At    time.Time `json:"at"`
	Event string    `json:"event"`          // woke, wake_failed or wake_skipped
	Path  string    `json:"path,omitempty"` // webhook or email
	// Status is the HTTP status the wake endpoint answered: exact when the
	// relay recorded it, "2xx" for an older woke row, empty when unknown.
	Status string `json:"status,omitempty"`
	// Reply is the endpoint's reply summary for a woke row, or
	// ReplyNotRecorded when the row has none.
	Reply string `json:"reply,omitempty"`
	Error string `json:"error,omitempty"` // why a wake failed or was skipped
	// NextPoll is when the agent next polled, before the relay's next wake.
	// For an older woke row, written before polls were audited, it is the
	// agent's next delivery or claim, and NextVia names which.
	NextPoll *time.Time `json:"next_poll,omitempty"`
	NextVia  string     `json:"next_via,omitempty"` // poll, delivered or claimed
	// RequestIDs are the requests a woke row says the wake covered: its
	// request_id when it names one, else the ids in its ", ids: " detail
	// segment. Rows written before ids were recorded have none.
	RequestIDs []string `json:"request_ids,omitempty"`
}

// ReplyNotRecorded is a woke row's reply when the relay kept no summary.
const ReplyNotRecorded = "not recorded"

var wakeEvents = []string{"woke", "wake_failed", "wake_skipped"}

// handleWakes exports an agent's wakes in [since, until] (admin).
func (s *Server) handleWakes(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	agent := r.PathValue("agent")
	q := r.URL.Query()
	since, err := time.Parse(time.RFC3339Nano, q.Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("since must be an RFC 3339 time"))
		return
	}
	until := s.cfg.Now()
	if v := q.Get("until"); v != "" {
		if until, err = time.Parse(time.RFC3339Nano, v); err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("until must be an RFC 3339 time"))
			return
		}
	}
	ctx := r.Context()
	rows, err := s.store.AuditForActor(ctx, agent, since, until, maxWakeExportWakes, wakeEvents...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	path := ""
	if s.wake != nil {
		if m := s.wake.WakeMethod(agent); relayWoken(m) {
			path = m
		}
	}
	out := WakeExport{Agent: agent, Since: since.UTC(), Until: until.UTC(), Wakes: []WakeEntry{}, Truncated: len(rows) == maxWakeExportWakes}
	head, err := json.Marshal(out)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	size := len(head) + len(`,"truncated":true`)
	for i, e := range rows {
		entry := wakeEntry(e, path)
		// The next activity must come before the relay's next sent wake,
		// which may fall after until. A skipped wake sent nothing, so it
		// does not end the search.
		var next int64
		for _, n := range rows[i+1:] {
			if n.Event == "woke" || n.Event == "wake_failed" {
				next = n.Seq
				break
			}
		}
		if next == 0 {
			n, found, err := s.store.FirstAuditAfter(ctx, agent, e.Seq, 0, "woke", "wake_failed")
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if found {
				next = n.Seq
			}
		}
		activity := []string{store.EventPolled}
		if e.Event == "woke" && entry.Status == "2xx" {
			// Written before polled rows existed: the next delivery or claim
			// is the best sign of a check-in.
			activity = append(activity, "delivered", "claimed")
		}
		n, found, err := s.store.FirstAuditAfter(ctx, agent, e.Seq, next, activity...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if found {
			at := pollTime(n)
			entry.NextPoll, entry.NextVia = &at, n.Event
			if n.Event == store.EventPolled {
				entry.NextVia = "poll"
			}
		}
		b, err := json.Marshal(entry)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		size += len(b) + 1 // and its comma
		if len(out.Wakes) > 0 && size > maxWakeExportBytes {
			out.Truncated = true
			break
		}
		out.Wakes = append(out.Wakes, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// failedStatus finds the status in a wake_failed reason ("host returned 500
// Internal Server Error").
var failedStatus = regexp.MustCompile(`returned (\d{3})\b`)

// wakeEntry reads one wake audit row. A woke detail is "<path>, HTTP
// <code>, <n> waiting[, <n> unseen replies][, ids: <id>,<id>...][, response:
// <summary>]" (a request-email wake has its own counts in place of the
// waiting ones), the
// path being the method or "fallback <n>: <method>", and a wake_failed
// reason sent on a fallback ends in "(fallback <n>: <method>)"; rows
// written before the status code was recorded lack the HTTP part. path is
// the agent's current wake method, for rows that do not name one. A wake
// that covered one request carries its id in request_id instead of an ids
// segment.
func wakeEntry(e store.AuditEvent, path string) WakeEntry {
	entry := WakeEntry{At: e.At, Event: e.Event, Path: path}
	if e.RequestID != "" {
		entry.RequestIDs = []string{e.RequestID}
	}
	switch e.Event {
	case "woke":
		head := e.Detail
		entry.Reply = ReplyNotRecorded
		if i := strings.Index(head, ", response: "); i >= 0 {
			if reply := strings.TrimSpace(head[i+len(", response: "):]); reply != "" {
				entry.Reply = reply
			}
			head = head[:i]
		}
		entry.Status = "2xx" // a woke row is only written for a 2xx answer
		for j, part := range strings.Split(head, ", ") {
			if j == 0 && (relayWoken(part) || strings.HasPrefix(part, "fallback ")) {
				entry.Path = part
			}
			if code, ok := strings.CutPrefix(part, "HTTP "); ok {
				entry.Status = code
			}
			if ids, ok := strings.CutPrefix(part, "ids: "); ok && ids != "" {
				entry.RequestIDs = strings.Split(ids, ",")
			}
		}
	case "wake_failed":
		entry.Error = e.Detail
		if i := strings.LastIndex(e.Detail, " (fallback "); i >= 0 && strings.HasSuffix(e.Detail, ")") {
			entry.Path = e.Detail[i+2 : len(e.Detail)-1]
		}
		if m := failedStatus.FindStringSubmatch(e.Detail); m != nil {
			entry.Status = m[1]
		}
	default:
		entry.Error = e.Detail
	}
	return entry
}
