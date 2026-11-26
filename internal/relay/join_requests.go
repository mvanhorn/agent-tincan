package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// SetSelfNode records the relay's own tailnet node. Until it is set, or
// when it has no MagicDNS name, every join request is refused.
func (s *Server) SetSelfNode(n identity.Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.self = n
}

func (s *Server) selfNode() identity.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.self
}

// Where a decision was taken, as the store and the audit log record it.
const (
	joinBySocket = "socket"
	joinByPage   = "page"
)

// Refusals specific to join requests.
var (
	errJoinVirtual   = errors.New("a gateway caller cannot file a join request")
	errJoinGuestNode = errors.New("this machine carries a guest; a guest joins with an invite")
	errJoinSharedIn  = errors.New("this machine is shared in from another tailnet; ask the owner for an invite")
	errJoinTailnet   = errors.New("this machine is not on the relay's own tailnet; ask the owner for an invite")
	errJoinChannel   = errors.New("approve or deny a join request on the phone page or with tincan approve-join / deny-join on the relay host")
	errJoinOwnNode   = errors.New("a join request cannot be decided from the machine that filed it")
	errJoinRelayHost = errors.New("this join request came from the relay host itself; approve or deny it on the phone page from another device")
	errJoinNoSelf    = errors.New("the relay does not know its own tailnet node, so it cannot tell whether this request came from the relay host; approve or deny it on the phone page from another device")
)

// joinKey names join request id in the hub and in the shared nonce book,
// where the prefix keeps its nonces from ever matching a held request's.
func joinKey(id string) string { return "join:" + id }

// joinAnswer is what the requester's routes answer: the request's status,
// and while it is open its code and names. Agents on a claim's answer are
// the names it bound, present only on the claim that delivered them.
type joinAnswer struct {
	ID        string                    `json:"id"`
	Status    string                    `json:"status"`
	Code      string                    `json:"code,omitempty"`
	Agents    []identity.RequestedAgent `json:"agents,omitempty"`
	FiledAt   time.Time                 `json:"filed_at,omitzero"`
	ExpiresAt time.Time                 `json:"expires_at,omitzero"`
}

func requesterView(r identity.JoinRequest) joinAnswer {
	a := joinAnswer{ID: r.ID, Status: r.Status, FiledAt: r.Filed, ExpiresAt: r.Expires}
	if r.Open() {
		a.Code, a.Agents = r.Code, r.Agents
	}
	return a
}

// joinDetail is the audit detail of a join request event: the node, the
// names and anything in extra. Never the code.
func joinDetail(r identity.JoinRequest, extra map[string]any) string {
	d := map[string]any{"join_request": r.ID, "node": r.NodeID, "node_name": r.NodeName}
	if r.NodeUser != "" {
		d["node_user"] = r.NodeUser
	}
	if len(r.Agents) > 0 {
		d["agents"] = r.Agents
	}
	maps.Copy(d, extra)
	return store.DetailJSON(d)
}

// joinCaller is the caller as the guest gate read it, or a fresh read.
func (s *Server) joinCaller(r *http.Request) (identity.Caller, error) {
	if g, ok := r.Context().Value(callerKey).(gateCaller); ok {
		return g.caller, g.err
	}
	return s.dir.Caller(r.Context(), r.RemoteAddr)
}

// joinNode is the calling node, or writes why it cannot be read.
func (s *Server) joinNode(w http.ResponseWriter, r *http.Request) (identity.Caller, bool) {
	c, err := s.joinCaller(r)
	if err != nil {
		code := http.StatusForbidden
		if errors.Is(err, identity.ErrLookupFailed) {
			code = http.StatusInternalServerError
		}
		writeErr(w, code, err)
		return c, false
	}
	return c, true
}

// onOwnTailnet reports whether n's MagicDNS name is a machine name under
// the relay's own tailnet. It fails closed: no name on either side, or a
// relay that does not know its own, is not on it.
func (s *Server) onOwnTailnet(n identity.Node) bool {
	_, suffix, ok := strings.Cut(s.selfNode().DNSName, ".")
	if !ok || suffix == "" {
		return false
	}
	host, rest, ok := strings.Cut(n.DNSName, ".")
	return ok && host != "" && strings.EqualFold(rest, suffix)
}

// fileRefusal says why node c may not file, with the HTTP status, or nil.
func (s *Server) fileRefusal(ctx context.Context, c identity.Caller) (int, error) {
	n := c.Node
	switch {
	case strings.HasPrefix(n.ID, identity.VirtualPrefix):
		return http.StatusForbidden, errJoinVirtual
	case identity.CarriesGuest(c.Agents):
		return http.StatusForbidden, errJoinGuestNode
	case n.SharedIn:
		return http.StatusForbidden, errJoinSharedIn
	case !s.onOwnTailnet(n):
		return http.StatusForbidden, errJoinTailnet
	}
	back, err := s.dir.Readmittable(ctx, n)
	if err != nil {
		return joinStatusFor(err), err
	}
	if len(back) > 0 {
		var names []string
		for _, a := range back {
			names = append(names, a.Name)
		}
		return http.StatusConflict, fmt.Errorf("%s looks like the rebuilt machine of %s; run tincan rejoin --name <agent> for each instead of filing a join request",
			n.Name, strings.Join(names, ", "))
	}
	return 0, nil
}

// handleFileJoinRequest files a join request from the calling machine.
func (s *Server) handleFileJoinRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Agents []identity.RequestedAgent `json:"agents"`
	}
	if err := decodeOne(r.Body, &in, "join request"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c, ok := s.joinNode(w, r)
	if !ok {
		return
	}
	if code, err := s.fileRefusal(r.Context(), c); err != nil {
		writeErr(w, code, err)
		return
	}
	for _, a := range in.Agents {
		if !onboard.RequestableKind(a.Kind) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("a join request cannot name kind %q (want one of %s, except %s, which joins with an invite)",
				a.Kind, strings.Join(onboard.Kinds, ", "), onboard.KindProxySandbox))
			return
		}
	}
	jr, err := s.dir.FileJoinRequest(r.Context(), c.Node, in.Agents)
	if err != nil {
		writeErr(w, joinStatusFor(err), err)
		return
	}
	s.record(r.Context(), "join_requested", "", "", r.RemoteAddr, joinDetail(jr, nil))
	s.queuePush()
	writeJSON(w, http.StatusOK, requesterView(jr))
}

// readOwnJoinRequest reads request id for the calling node n: 404 when
// there is none, 403 for another node. The recorded node calling with a new
// key voids an open request.
func (s *Server) readOwnJoinRequest(ctx context.Context, id string, n identity.Node) (identity.JoinRequest, error) {
	jr, found, err := s.store.JoinRequest(ctx, id, s.cfg.Now())
	if err != nil {
		return jr, err
	}
	if !found {
		return jr, identity.ErrJoinNotFound
	}
	if jr.NodeID != n.ID {
		return identity.JoinRequest{}, identity.ErrJoinWrongNode
	}
	if jr.Open() && jr.NodeKey != n.Key {
		v, err := s.store.VoidJoinRequest(ctx, id, s.cfg.Now())
		if err != nil && !errors.Is(err, identity.ErrJoinState) {
			return jr, err
		}
		if err == nil {
			s.joinVoided(ctx, v)
		}
		return v, nil
	}
	return jr, nil
}

// joinVoided records a request voided by a key change and wakes its
// waiters.
func (s *Server) joinVoided(ctx context.Context, jr identity.JoinRequest) {
	s.nonces.void(joinKey(jr.ID))
	s.record(ctx, "join_voided", "", "", "relay", joinDetail(jr, map[string]any{"reason": "node key changed"}))
	s.hub.notify(joinKey(jr.ID))
}

// handleJoinRequestStatus answers a request's status to the node that
// filed it. It changes nothing but a void on a changed key; with wait it
// holds a pending request until it is decided or the wait ends. It takes a
// hub waiter only for the caller's own pending request, which a decision,
// void, claim or the expiry sweep always notifies: an unknown, foreign or
// final id would leave its key in the hub for good.
func (s *Server) handleJoinRequestStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := s.joinNode(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	wait := durationParam(r, "wait", 0, s.cfg.MaxWait)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	// answered reads the request and answers it unless the read fails or it
	// should not be waited on.
	answered := func() bool {
		jr, err := s.readOwnJoinRequest(r.Context(), id, c.Node)
		if err != nil {
			writeErr(w, joinStatusFor(err), err)
			return true
		}
		if jr.Status != identity.JoinPending || wait == 0 {
			writeJSON(w, http.StatusOK, requesterView(jr))
			return true
		}
		return false
	}
	for {
		if answered() {
			return
		}
		// Read again after taking the waiter, so a decision that lands
		// between the two is never missed.
		wake := s.hub.wait(joinKey(id))
		if answered() {
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
		wait = 0 // one last read, then answer whatever it says
	}
}

// handleClaimJoinRequest delivers an approved request's agents to the node
// that filed it, once. A request that is not approved answers its status
// and binds nothing.
func (s *Server) handleClaimJoinRequest(w http.ResponseWriter, r *http.Request) {
	c, ok := s.joinNode(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	jr, err := s.dir.ClaimJoinRequest(r.Context(), id, c.Node)
	var refused *identity.JoinRefusedError
	switch {
	case errors.As(err, &refused):
		s.nonces.void(joinKey(id))
		s.record(r.Context(), "join_failed", "", "", "relay", joinDetail(jr, map[string]any{"refused": refused.Name, "reason": refused.Err.Error()}))
		s.hub.notify(joinKey(id))
		writeJSON(w, http.StatusConflict, map[string]string{"id": id, "status": identity.JoinFailed, "name": refused.Name, "error": err.Error()})
		return
	case errors.Is(err, identity.ErrJoinState):
		// A request this claim voided comes back with its names; one
		// already final comes back slim.
		if jr.Status == identity.JoinVoided && len(jr.Agents) > 0 {
			s.joinVoided(r.Context(), jr)
		}
		writeJSON(w, http.StatusOK, joinAnswer{ID: jr.ID, Status: jr.Status, FiledAt: jr.Filed, ExpiresAt: jr.Expires})
		return
	case err != nil:
		writeErr(w, joinStatusFor(err), err)
		return
	}
	s.hub.notify(joinKey(id))
	for _, a := range jr.Agents {
		if wr, ok := s.wake.(WakeReporter); ok {
			wr.Joined(a.Name)
		}
		s.record(r.Context(), "joined", "", "", a.Name, store.DetailJSON(map[string]any{"join_request": jr.ID, "node": jr.NodeID, "node_name": jr.NodeName}))
	}
	writeJSON(w, http.StatusOK, joinAnswer{ID: jr.ID, Status: jr.Status, Agents: jr.Agents, FiledAt: jr.Filed, ExpiresAt: jr.Expires})
}

// joinStatusFor is statusFor with a store failure as the relay's, not the
// caller's: only a request the caller got wrong answers 400.
func joinStatusFor(err error) int {
	switch {
	case errors.Is(err, identity.ErrJoinInvalid):
		return http.StatusBadRequest
	case errors.Is(err, identity.ErrJoinNotFound), errors.Is(err, identity.ErrJoinWrongNode), errors.Is(err, identity.ErrJoinCap),
		errors.Is(err, identity.ErrJoinState), errors.Is(err, identity.ErrNameTaken), errors.Is(err, identity.ErrRebindCheckFailed):
		return statusFor(err)
	}
	return http.StatusInternalServerError
}

// joinListing is one pending request as the owner sees it. The approve and
// deny nonces go to a tailnet admin only (the page), bound to its device.
type joinListing struct {
	ID             string                    `json:"id"`
	Code           string                    `json:"code"`
	NodeName       string                    `json:"node_name"`
	NodeUser       string                    `json:"node_user,omitempty"`
	Tags           []string                  `json:"tags,omitempty"`
	Addrs          []string                  `json:"addrs,omitempty"`
	Agents         []identity.RequestedAgent `json:"agents"`
	NodeAgents     []string                  `json:"node_agents,omitempty"`
	FiledAt        time.Time                 `json:"filed_at"`
	ExpiresAt      time.Time                 `json:"expires_at"`
	AgeSeconds     int                       `json:"age_seconds"`
	ApproveNonce   string                    `json:"approve_nonce,omitempty"`
	DenyNonce      string                    `json:"deny_nonce,omitempty"`
	NonceExpiresAt time.Time                 `json:"nonce_expires_at,omitzero"`
}

// handleJoinRequests lists the pending join requests for the owner.
func (s *Server) handleJoinRequests(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	s.sweepJoinRequests(r.Context())
	now := s.cfg.Now()
	reqs, err := s.store.PendingJoinRequests(r.Context(), now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// As for the held listing, every tailnet caller gets nonces: the page's
	// same-origin GET may carry no browser marks over plain http.
	var node string
	if !isLocalAdmin(r.Context()) {
		node = s.callerNode(r).ID
	}
	out := make([]joinListing, 0, len(reqs))
	for _, jr := range reqs {
		e := joinListing{ID: jr.ID, Code: jr.Code, NodeName: jr.NodeName, NodeUser: jr.NodeUser, Tags: jr.Tags, Agents: jr.Agents,
			FiledAt: jr.Filed, ExpiresAt: jr.Expires, AgeSeconds: int(now.Sub(jr.Filed) / time.Second)}
		for _, a := range jr.Addrs {
			e.Addrs = append(e.Addrs, a.String())
		}
		if agents, err := s.store.AgentsByNode(r.Context(), jr.NodeID); err == nil {
			for _, a := range agents {
				e.NodeAgents = append(e.NodeAgents, a.Name)
			}
		} else {
			log.Printf("join requests: agents on %s: %v", jr.NodeName, err)
		}
		if node != "" {
			e.ApproveNonce, e.NonceExpiresAt = s.nonces.issue(nonceKey{id: joinKey(jr.ID), decision: decisionApprove, node: node}, now)
			e.DenyNonce, _ = s.nonces.issue(nonceKey{id: joinKey(jr.ID), decision: decisionDeny, node: node}, now)
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

func (s *Server) handleApproveJoinRequest(w http.ResponseWriter, r *http.Request) {
	s.decideJoinRequest(w, r, decisionApprove)
}

func (s *Server) handleDenyJoinRequest(w http.ResponseWriter, r *http.Request) {
	s.decideJoinRequest(w, r, decisionDeny)
}

// joinDecision is the answer to an approve or deny.
type joinDecision struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	AlreadyDecided bool   `json:"already_decided,omitempty"`
	Error          string `json:"error,omitempty"`
}

// decideJoinRequest approves or denies a pending request. It takes the
// local admin socket, or an admin device's browser-marked call with a nonce
// from the listing; a tailnet call without browser marks is refused. Neither
// may decide a request filed from its own node: a tailnet caller's WhoIs
// node, or for the socket the relay host's.
func (s *Server) decideJoinRequest(w http.ResponseWriter, r *http.Request, decision string) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	socket := isLocalAdmin(r.Context())
	if !socket && !browserMarked(r) {
		writeErr(w, http.StatusForbidden, errJoinChannel)
		return
	}
	in, ok := readDecision(w, r)
	if !ok {
		return
	}
	s.sweepJoinRequests(r.Context())
	id := r.PathValue("id")
	jr, found, err := s.store.JoinRequest(r.Context(), id, s.cfg.Now())
	switch {
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	case !found:
		writeErr(w, http.StatusNotFound, identity.ErrJoinNotFound)
		return
	}
	by := joinBySocket
	extra := map[string]any{"via": joinBySocket}
	if socket {
		// Fail closed: a relay that does not know its own node cannot
		// tell whether the request came from its own host.
		switch self := s.selfNode(); {
		case self.ID == "":
			writeErr(w, http.StatusForbidden, errJoinNoSelf)
			return
		case self.ID == jr.NodeID:
			writeErr(w, http.StatusForbidden, errJoinRelayHost)
			return
		}
	} else {
		n := s.callerNode(r)
		if n.ID == "" || n.ID == jr.NodeID {
			writeErr(w, http.StatusForbidden, errJoinOwnNode)
			return
		}
		by = joinByPage + ":" + n.Name
		extra = map[string]any{"via": joinByPage, "device": n.Name}
		if !s.takeDecisionNonce(r, joinKey(id), decision, in.Nonce) {
			if jr.Status != identity.JoinPending {
				writeJSON(w, http.StatusOK, joinDecision{ID: id, Status: jr.Status, AlreadyDecided: true})
				return
			}
			writeErr(w, http.StatusConflict, errStaleNonce)
			return
		}
	}
	move, event := s.store.ApproveJoinRequest, "join_approved"
	if decision == decisionDeny {
		move, event = s.store.DenyJoinRequest, "join_denied"
	}
	got, err := move(r.Context(), id, by, s.cfg.Now())
	switch {
	case errors.Is(err, identity.ErrJoinState) && !socket:
		writeJSON(w, http.StatusOK, joinDecision{ID: id, Status: got.Status, AlreadyDecided: true})
		return
	case errors.Is(err, identity.ErrJoinState):
		writeJSON(w, http.StatusConflict, joinDecision{ID: id, Status: got.Status, Error: fmt.Sprintf("the join request is already %s", got.Status)})
		return
	case errors.Is(err, identity.ErrJoinNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.nonces.void(joinKey(id))
	s.record(r.Context(), event, "", "", s.remote(r), joinDetail(got, extra))
	s.hub.notify(joinKey(id))
	writeJSON(w, http.StatusOK, joinDecision{ID: id, Status: got.Status})
}

// sweepJoinRequests expires open requests past their time, audits each as
// join_expired, and wakes their waiters.
func (s *Server) sweepJoinRequests(ctx context.Context) {
	expired, err := s.store.SweepJoinRequests(ctx, s.cfg.Now())
	if err != nil {
		log.Printf("sweep: join requests: %v", err)
		return
	}
	for _, jr := range expired {
		s.nonces.void(joinKey(jr.ID))
		s.record(ctx, "join_expired", "", "", "relay", joinDetail(jr, nil))
		s.hub.notify(joinKey(jr.ID))
	}
}
