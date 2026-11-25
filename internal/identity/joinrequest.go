package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// Join request statuses. Pending and approved are open: an open request
// holds its names. The others are final.
const (
	JoinPending   = "pending"
	JoinApproved  = "approved"
	JoinDelivered = "delivered"
	JoinDenied    = "denied"
	JoinExpired   = "expired"
	JoinVoided    = "voided"
	JoinFailed    = "failed"
)

const (
	// JoinRequestTTL is how long a request lives after filing, whatever its
	// state: an approval not claimed by then is lost.
	JoinRequestTTL = 10 * time.Minute
	// JoinFinalKeep is how long a request in a final state keeps its slim
	// row (no names, no code), so the requester can read the outcome and the
	// hourly count survives a restart.
	JoinFinalKeep = time.Hour
	// MaxJoinAgents caps the names one request may carry.
	MaxJoinAgents = 8
	// MaxOpenJoinRequestsPerNode, MaxJoinRequestsPerHour and
	// MaxPendingJoinRequests are the filing caps: open requests per node,
	// requests filed per node in the last hour whatever their outcome, and
	// pending requests on the relay.
	MaxOpenJoinRequestsPerNode = 1
	MaxJoinRequestsPerHour     = 5
	MaxPendingJoinRequests     = 10
)

var (
	// ErrJoinNotFound means no join request has that id.
	ErrJoinNotFound = errors.New("no such join request")
	// ErrJoinState means the request is not in a state that allows the
	// operation; the request returned with it carries its current status.
	ErrJoinState = errors.New("the join request is not in a state that allows this")
	// ErrJoinWrongNode means the caller is not the node that filed the
	// request. Nothing about the request changed.
	ErrJoinWrongNode = errors.New("the join request was filed from another machine")
	// ErrJoinCap means filing would pass one of the caps; the wrapping error
	// says which.
	ErrJoinCap = errors.New("too many join requests")
	// ErrJoinRefused marks a claim refused because a requested name can no
	// longer be joined; see JoinRefusedError.
	ErrJoinRefused = errors.New("a requested name can no longer be joined")
)

// JoinRefusedError is the error a claim returns when a requested name can no
// longer be joined. It matches ErrJoinRefused and the reason (ErrNameTaken,
// ErrGuestNode) under errors.Is.
type JoinRefusedError struct {
	Name string
	Err  error
}

func (e *JoinRefusedError) Error() string {
	return fmt.Sprintf("%s: %v: %v", e.Name, ErrJoinRefused, e.Err)
}

func (e *JoinRefusedError) Unwrap() []error { return []error{ErrJoinRefused, e.Err} }

// RequestedAgent is one name a join request asks for, with its kind ("" for
// none).
type RequestedAgent struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// JoinRequest is an unjoined machine's request to join under new names. The
// node fields are what WhoIs reported at filing. Agents and Code are empty
// once the request is final.
type JoinRequest struct {
	ID       string
	NodeID   string
	NodeKey  string
	NodeName string
	NodeUser string
	Tags     []string
	Addrs    []netip.Addr
	Agents   []RequestedAgent
	// Code is the matching code shown on both screens. It is not a
	// credential.
	Code      string
	Status    string
	Filed     time.Time
	Expires   time.Time
	Decided   time.Time // when it was approved or denied, zero before
	DecidedBy string    // where it was approved or denied
	Closed    time.Time // when it reached a final state, zero while open
}

// Open reports whether r is pending or approved.
func (r JoinRequest) Open() bool { return r.Status == JoinPending || r.Status == JoinApproved }

// AsOf returns r as it stands at now: an open request past its expiry reads
// as expired even before the sweep has recorded it.
func (r JoinRequest) AsOf(now time.Time) JoinRequest {
	if r.Open() && !now.Before(r.Expires) {
		r.Status = JoinExpired
	}
	return r
}

// CanMoveTo reports whether r, read with AsOf, may move to status to by a
// decision or a claim: approve and deny need a pending request, delivered
// and failed an approved one, and void any open one. Expiry is the sweep's
// alone.
func (r JoinRequest) CanMoveTo(to string) bool {
	switch to {
	case JoinApproved, JoinDenied:
		return r.Status == JoinPending
	case JoinDelivered, JoinFailed:
		return r.Status == JoinApproved
	case JoinVoided:
		return r.Open()
	}
	return false
}

// JoinRequestStore persists join requests. Every method takes the time it
// judges expiry by. A method that changes a request returns it as decided,
// names and code included, while the stored row keeps neither once final; on
// ErrJoinState it returns the request as it stands.
type JoinRequestStore interface {
	// InsertJoinRequest stores r in one transaction with the caps: it is
	// refused with ErrJoinCap when r's node already has an open request, has
	// filed MaxJoinRequestsPerHour in the hour before now, or the relay holds
	// MaxPendingJoinRequests pending. Callers file through
	// Directory.FileJoinRequest, which checks the names first.
	InsertJoinRequest(ctx context.Context, r JoinRequest, now time.Time) error
	// JoinRequest returns the request with id, read with AsOf(now).
	JoinRequest(ctx context.Context, id string, now time.Time) (JoinRequest, bool, error)
	// PendingJoinRequests lists the pending, unexpired requests, oldest
	// first.
	PendingJoinRequests(ctx context.Context, now time.Time) ([]JoinRequest, error)
	// ApproveJoinRequest moves a pending request to approved, recording by
	// as where it was decided. It binds nothing.
	ApproveJoinRequest(ctx context.Context, id, by string, now time.Time) (JoinRequest, error)
	// DenyJoinRequest moves a pending request to denied, which is final.
	DenyJoinRequest(ctx context.Context, id, by string, now time.Time) (JoinRequest, error)
	// VoidJoinRequest moves an open request to voided, which is final.
	VoidJoinRequest(ctx context.Context, id string, now time.Time) (JoinRequest, error)
	// FailJoinRequest moves an approved request to failed, which is final.
	FailJoinRequest(ctx context.Context, id string, now time.Time) (JoinRequest, error)
	// ClaimJoinRequest delivers an approved, unexpired request to n, which
	// must be the recorded node with the recorded key: in one transaction it
	// writes an agent row on n for every requested name and moves the request
	// to delivered, so it binds every name or none. A name that already has
	// an agent row fails the whole claim. Callers claim through
	// Directory.ClaimJoinRequest, which re-checks the names first.
	ClaimJoinRequest(ctx context.Context, id string, n Node, now time.Time) (JoinRequest, error)
	// JoinNameHeld reports whether an open, unexpired request other than
	// exceptID asks for name.
	JoinNameHeld(ctx context.Context, name, exceptID string, now time.Time) (bool, error)
	// SweepJoinRequests moves every open request past its expiry to expired
	// and returns those, names included, for the audit log; it deletes final
	// rows closed JoinFinalKeep or more before now.
	SweepJoinRequests(ctx context.Context, now time.Time) ([]JoinRequest, error)
}

// JoinCapRefusal is the ErrJoinCap for the first cap the counts reach, nil
// when filing stays under every cap: open is the node's open requests, hour
// the requests it filed in the last hour, pending the relay's pending
// requests. Both stores judge their counts with it.
func JoinCapRefusal(open, hour, pending int) error {
	switch {
	case open >= MaxOpenJoinRequestsPerNode:
		return fmt.Errorf("this machine already has an open join request: %w", ErrJoinCap)
	case hour >= MaxJoinRequestsPerHour:
		return fmt.Errorf("this machine filed %d join requests in the last hour: %w", hour, ErrJoinCap)
	case pending >= MaxPendingJoinRequests:
		return fmt.Errorf("the relay already holds %d pending join requests: %w", pending, ErrJoinCap)
	}
	return nil
}

// FileJoinRequest files a request from node n for agents. Under the lock
// every name-taking path holds, it checks that each name is new: not an
// agent, not the name of an unexpired invite, and not asked for by another
// open request (ErrNameTaken). The store then enforces the caps
// (ErrJoinCap). The request returned carries its id and matching code. The
// relay checks which nodes may file before calling this.
func (d *Directory) FileJoinRequest(ctx context.Context, n Node, agents []RequestedAgent) (JoinRequest, error) {
	if len(agents) == 0 || len(agents) > MaxJoinAgents {
		return JoinRequest{}, fmt.Errorf("a join request names 1 to %d agents", MaxJoinAgents)
	}
	seen := map[string]bool{}
	for _, a := range agents {
		if !nameRE.MatchString(a.Name) {
			return JoinRequest{}, fmt.Errorf("agent name %q must be 1-32 lowercase letters, digits, or dashes", a.Name)
		}
		if err := checkKind(a.Kind); err != nil {
			return JoinRequest{}, err
		}
		if seen[a.Name] {
			return JoinRequest{}, fmt.Errorf("%s is named twice", a.Name)
		}
		seen[a.Name] = true
	}
	id, err := newJoinID()
	if err != nil {
		return JoinRequest{}, err
	}
	code, err := NewMatchCode()
	if err != nil {
		return JoinRequest{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.cfg.Now()
	for _, a := range agents {
		if err := d.newName(ctx, a.Name, "", now); err != nil {
			return JoinRequest{}, err
		}
	}
	r := JoinRequest{
		ID: id, NodeID: n.ID, NodeKey: n.Key, NodeName: n.Name, NodeUser: n.User, Tags: n.Tags, Addrs: n.Addrs,
		Agents: agents, Code: code, Status: JoinPending, Filed: now, Expires: now.Add(JoinRequestTTL),
	}
	if err := d.store.InsertJoinRequest(ctx, r, now); err != nil {
		return JoinRequest{}, err
	}
	return r, nil
}

// newName is ErrNameTaken unless name is new: no agent, no unexpired invite,
// and no open request other than exceptID asking for it. The caller holds
// d.mu.
func (d *Directory) newName(ctx context.Context, name, exceptID string, now time.Time) error {
	if _, ok, err := d.store.AgentByName(ctx, name); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s is an agent: %w", name, ErrNameTaken)
	}
	if ok, err := d.store.LiveInviteNamed(ctx, name, now); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s has an invite pending: %w", name, ErrNameTaken)
	}
	return d.refuseHeld(ctx, name, exceptID, now)
}

// refuseHeld is ErrNameTaken when an open request other than exceptID asks
// for name.
func (d *Directory) refuseHeld(ctx context.Context, name, exceptID string, now time.Time) error {
	held, err := d.store.JoinNameHeld(ctx, name, exceptID, now)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("%s is held by a pending join request: %w", name, ErrNameTaken)
	}
	return nil
}

// ClaimJoinRequest delivers request id to the calling node n. It refuses a
// node other than the recorded one with ErrJoinWrongNode and changes
// nothing. A request that is not approved answers ErrJoinState with its
// current status and checks nothing else. The recorded node calling with a
// changed key voids the request (ErrJoinState, status voided). Otherwise,
// under the lock every name-taking path holds, it re-checks every name as
// new (its own hold excepted) and refuses a node that carries a guest; a
// refusal binds nothing, fails the request and returns a JoinRefusedError
// naming the name. Then the store binds every name and marks the request
// delivered in one transaction. The request returned on success carries the
// delivered names and kinds.
func (d *Directory) ClaimJoinRequest(ctx context.Context, id string, n Node) (JoinRequest, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.cfg.Now()
	r, ok, err := d.store.JoinRequest(ctx, id, now)
	if err != nil {
		return JoinRequest{}, err
	}
	if !ok {
		return JoinRequest{}, ErrJoinNotFound
	}
	if r.NodeID != n.ID {
		return JoinRequest{}, ErrJoinWrongNode
	}
	if r.Status != JoinApproved {
		return r, ErrJoinState
	}
	if r.NodeKey != n.Key {
		v, err := d.store.VoidJoinRequest(ctx, id, now)
		if err != nil && !errors.Is(err, ErrJoinState) {
			return JoinRequest{}, err
		}
		return v, ErrJoinState
	}
	if refusal, err := d.claimRefusal(ctx, r, now); err != nil {
		return JoinRequest{}, err
	} else if refusal != nil {
		f, err := d.store.FailJoinRequest(ctx, id, now)
		if err != nil && !errors.Is(err, ErrJoinState) {
			return JoinRequest{}, err
		}
		return f, refusal
	}
	return d.store.ClaimJoinRequest(ctx, id, n, now)
}

// claimRefusal is the JoinRefusedError for the first of r's names that can no
// longer be joined, nil when all can. The caller holds d.mu.
func (d *Directory) claimRefusal(ctx context.Context, r JoinRequest, now time.Time) (*JoinRefusedError, error) {
	others, err := d.store.AgentsByNode(ctx, r.NodeID)
	if err != nil {
		return nil, err
	}
	for _, a := range r.Agents {
		if err := d.newName(ctx, a.Name, r.ID, now); err != nil {
			if !errors.Is(err, ErrNameTaken) {
				return nil, err
			}
			return &JoinRefusedError{Name: a.Name, Err: err}, nil
		}
		if carriesGuest(others) {
			return &JoinRefusedError{Name: a.Name, Err: fmt.Errorf("%s runs %s: %w", r.NodeName, others[0].Name, ErrGuestNode)}, nil
		}
	}
	return nil, nil
}

// NewMatchCode returns a matching code shaped XXX-XXX: six characters from
// the 32-character code alphabet, 30 bits from crypto/rand.
func NewMatchCode() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 0, 7)
	for i, c := range b {
		if i == 3 {
			out = append(out, '-')
		}
		out = append(out, codeAlphabet[int(c)%len(codeAlphabet)])
	}
	return string(out), nil
}

func newJoinID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
