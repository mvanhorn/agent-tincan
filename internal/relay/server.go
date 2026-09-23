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
	"net/http"
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
	RequestTTL    time.Duration // how long an unanswered request lives
	PollHold      time.Duration // max time a long-poll is held
	DeliveryLease time.Duration // how long a delivered request waits for a claim
	ClaimLease    time.Duration // how long a claim lasts before the request is requeued
	MaxWait       time.Duration // cap on get-reply waits
	SweepEvery    time.Duration
	Now           func() time.Time
}

func (c *Config) defaults() {
	if c.RequestTTL == 0 {
		c.RequestTTL = 24 * time.Hour
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
}

// Preparer fills in a new request's chain fields and applies policy. The
// default starts a new chain; package policy supplies the real one.
type Preparer interface {
	Prepare(ctx context.Context, req *envelope.Request) error
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
	cfg    Config
	dir    *identity.Directory
	store  *store.Store
	hub    *hub
	prep   Preparer
	events Events
	wake   WakeNamer
	conn   Connector

	mu       sync.Mutex
	lastPoll map[string]time.Time
	polling  map[string]int // long-polls currently held open, per agent
}

// New builds a relay server.
func New(dir *identity.Directory, st *store.Store, cfg Config) *Server {
	cfg.defaults()
	return &Server{cfg: cfg, dir: dir, store: st, hub: newHub(), prep: newChain{}, lastPoll: map[string]time.Time{}, polling: map[string]int{}}
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
	mux.HandleFunc("POST /v1/requests/{id}/reply", s.handleReply)
	mux.HandleFunc("GET /v1/requests/{id}", s.handleGet)
	mux.HandleFunc("POST /v1/requests/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /v1/agents", s.handleAgents)
	mux.HandleFunc("GET /v1/whoami", s.handleWhoAmI)
	mux.HandleFunc("POST /v1/join", s.handleJoin)
	s.adminRoutes(mux)
	return limitBodies(mux)
}

// adminRoutes registers the routes served on both the tailnet API (for admin
// devices) and the local admin socket. /v1/agents is added by the caller.
func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/admin/invite", s.handleInvite)
	mux.HandleFunc("POST /v1/admin/remove", s.handleRemove)
	mux.HandleFunc("PUT /v1/agents/{name}/kind", s.handleSetKind)
	mux.HandleFunc("POST /v1/admin/connect", s.handleConnect)
	mux.HandleFunc("GET /v1/trace/{trace}", s.handleTrace)
	mux.HandleFunc("GET /v1/trace", s.handleRecent)
	mux.HandleFunc("GET /v1/admin/audit/verify", s.handleVerify)
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

func limitBodies(h http.Handler) http.Handler {
	max := client.Defaults(client.RelayAPI).MaxBodyBytes
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client.LimitBody(w, r, max)
		h.ServeHTTP(w, r)
	})
}

// Run sweeps expired requests and leases until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
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
		}
		s.record(ctx, event, t.ID, t.TraceID, "relay", "")
		s.hub.notify(requestKey(t.ID))
		if t.Status == envelope.StatusQueued {
			s.hub.notify(inboxKey(t.To))
			// An agent woken by the relay has no poller to see the requeue,
			// so wake it again; pollers are skipped by the waker itself.
			req := envelope.Request{ID: t.ID, TraceID: t.TraceID, From: t.From, To: t.To}
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
	return res.Name
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
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "kind": a.Kind})
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
	if !s.isAgent(r.Context(), req.To) {
		writeErr(w, http.StatusNotFound, errors.New("no such agent: "+req.To))
		return
	}
	if err := s.prep.Prepare(r.Context(), &req); err != nil {
		s.record(r.Context(), "rejected", "", req.TraceID, from, store.DetailJSON(map[string]any{"to": req.To, "reason": err.Error()}))
		writeErr(w, statusFor(err), err)
		return
	}
	req, err = s.store.Enqueue(r.Context(), req, s.cfg.RequestTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.hub.notify(inboxKey(req.To))
	s.record(r.Context(), "queued", req.ID, req.TraceID, from, store.DetailJSON(map[string]any{"to": req.To, "hop": req.Hop, "chain": req.Chain}))
	if s.events != nil {
		s.events.Queued(r.Context(), req)
	}
	writeJSON(w, http.StatusCreated, req)
}

// handlePoll holds a long-poll until requests for the caller, or unseen
// replies to its own requests that it asked for, are waiting. No poll marks a
// reply seen: replies=take and replies=keep both return unseen replies, and
// a taking client acknowledges them through POST /v1/replies/ack once it has
// handed them to its agent. Without a replies param (every client that
// predates replies) they are left out and do not end the hold, as with
// replies=none, since such a client would drop them. peek=1 only reports
// what is waiting, and counts replies only with replies=keep or take.
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
	for {
		s.touch(name)
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
			n, err := s.store.CountQueued(r.Context(), name)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if n > 0 || len(reps) > 0 {
				out := map[string]any{"waiting": n + len(reps) + more, "queued": n}
				if replies != client.RepliesNone {
					out["replies"] = emptyIfNil(reps)
				}
				if more > 0 {
					out["replies_remaining"] = more
				}
				writeJSON(w, http.StatusOK, out)
				return
			}
			select {
			case <-wake:
				continue
			case <-deadline.C:
				w.WriteHeader(http.StatusNoContent)
				return
			case <-r.Context().Done():
				return
			}
		}
		reqs, err := s.store.Deliver(r.Context(), name, 20, s.cfg.DeliveryLease)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if len(reqs) > 0 || len(reps) > 0 {
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
			writeJSON(w, http.StatusOK, out)
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			s.touch(name)
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

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
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("body must be {\"ids\": [...]}: %w", err))
		return
	}
	if len(in.IDs) > maxAckIDs {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("at most %d ids per ack", maxAckIDs))
		return
	}
	if err := s.store.MarkRepliesSeen(r.Context(), name, in.IDs); err != nil {
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
	id := r.PathValue("id")
	rep, err = s.store.Reply(r.Context(), id, name, rep)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.hub.notify(requestKey(id))
	s.record(r.Context(), "replied", id, "", name, store.DetailJSON(map[string]any{"status": rep.Status}))
	// The asker learns of the reply from a held poll now, or from a wake
	// once the waker's grace period shows it went unread.
	if req, _, err := s.store.Request(r.Context(), id); err == nil {
		s.hub.notify(inboxKey(req.From))
		if rp, ok := s.events.(Replier); ok {
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
		if res.Done() || wait == 0 {
			writeJSON(w, http.StatusOK, res)
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			writeJSON(w, http.StatusOK, res)
			return
		case <-r.Context().Done():
			return
		}
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

// SetWakeNamer installs the wake method lookup for the agent list.
func (s *Server) SetWakeNamer(w WakeNamer) { s.wake = w }

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
	now := s.cfg.Now()
	out := make([]client.AgentInfo, 0, len(agents))
	s.mu.Lock()
	for _, a := range agents {
		last := s.lastPoll[a.Name]
		info := client.AgentInfo{Name: a.Name, LastPoll: last, Online: !last.IsZero() && now.Sub(last) < s.cfg.PollHold+30*time.Second, Wake: "none", Kind: a.Kind}
		if s.wake != nil {
			info.Wake = s.wake.WakeMethod(a.Name)
		}
		out = append(out, info)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
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
	if err := s.dir.Remove(r.Context(), s.remote(r), in.Name); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
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

func (s *Server) touch(agent string) {
	s.mu.Lock()
	s.lastPoll[agent] = s.cfg.Now()
	s.mu.Unlock()
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
	steps, err := s.store.RecentTraces(r.Context(), limit)
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
