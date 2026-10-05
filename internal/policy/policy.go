// Package policy fills in a request's chain and stops runaway loops between
// trusted agents. It never restricts who may ask whom: joined agents trust
// each other. It enforces the hop limit, rejects cycles, rate-limits each
// sender as a backstop against an agent stuck starting new chains, and holds
// asks for the owner's approval (approval.json, and by default asks to the
// kinds in holdByDefault).
package policy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

var (
	ErrCycle         = errors.New("request would loop back to an agent already in this chain")
	ErrHopLimit      = errors.New("request chain is too long")
	ErrBadParent     = errors.New("parent request is not one this agent is handling")
	ErrUrgentLimited = errors.New("urgent limit reached; send without --urgent")
	ErrRateLimited   = errors.New("too many requests from this agent; slow down")
)

// DefaultHopLimit is the longest allowed chain when Config.HopLimit is 0.
const DefaultHopLimit = 4

// Config tunes the policy.
type Config struct {
	Approval      *Approval
	UrgentPerHour int // max urgent requests per sender per hour; default 5
	HopLimit      int // longest allowed chain; default DefaultHopLimit
	PerMinute     int // max new requests per sender per minute; default 30
	Now           func() time.Time
}

// Policy implements relay.Preparer.
type Policy struct {
	st  *store.Store
	cfg Config
	// kindOf returns an agent's stored kind ("" for none or unknown).
	kindOf func(ctx context.Context, name string) (string, error)

	mu     sync.Mutex
	sent   map[string][]time.Time
	urgent map[string][]time.Time
}

// New builds a Policy over the relay store.
func New(st *store.Store, cfg Config) *Policy {
	if cfg.HopLimit == 0 {
		cfg.HopLimit = DefaultHopLimit
	}
	if cfg.PerMinute == 0 {
		cfg.PerMinute = 30
	}
	if cfg.UrgentPerHour == 0 {
		cfg.UrgentPerHour = 5
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	p := &Policy{st: st, cfg: cfg, sent: map[string][]time.Time{}, urgent: map[string][]time.Time{}}
	p.kindOf = func(ctx context.Context, name string) (string, error) {
		a, _, err := st.AgentByName(ctx, name)
		return a.Kind, err
	}
	return p
}

// Prepare sets TraceID, Hop, Chain, and ParentID, then applies the loop and
// rate checks.
func (p *Policy) Prepare(ctx context.Context, req *envelope.Request) error {
	parent, ok, err := p.parent(ctx, req)
	if err != nil {
		return err
	}
	if req.Kind == envelope.KindPing && ok {
		return reject(http.StatusBadRequest, errors.New("ping cannot continue a request chain"))
	}
	if ok {
		req.ParentID, req.TraceID, req.Hop = parent.ID, parent.TraceID, parent.Hop+1
		req.Chain = append(slices.Clone(parent.Chain), req.From)
	} else {
		req.ParentID, req.TraceID, req.Hop, req.Chain = "", "", 1, []string{req.From}
	}
	if slices.Contains(req.Chain, req.To) {
		return reject(http.StatusConflict, fmt.Errorf("%w: %s is already in %v", ErrCycle, req.To, req.Chain))
	}
	if req.Hop > p.cfg.HopLimit {
		return reject(http.StatusConflict, fmt.Errorf("%w: hop %d exceeds %d", ErrHopLimit, req.Hop, p.cfg.HopLimit))
	}
	at, err := p.rate(req.From, req.Urgent)
	if err != nil {
		return err
	}
	// Stamp the request with the slot it took, so Refund gives back this
	// send's slot and not a concurrent one's. Enqueue sets the real time.
	req.CreatedAt = at
	// A ping does no work and carries no body, so the gate never holds it.
	if req.Kind == envelope.KindPing {
		return nil
	}
	held, err := p.cfg.Approval.Held(req)
	if held {
		req.Status = envelope.StatusHeld
		return nil
	}
	if err != nil {
		p.Refund(*req)
		return reject(http.StatusServiceUnavailable, err)
	}
	p.holdByKind(ctx, req, parent, ok)
	return nil
}

// holdByDefault lists the target kinds whose asks and notifies wait for the
// owner's approval even with no approval.json entry. A council sends the ask
// on to every model vendor on the team. dot-web types each request into the
// owner's dot DM as the owner, and a dot acts through the owner's connected
// apps. Replies and answers never pass through Prepare, so replies to a
// dot's own asks are not held.
var holdByDefault = map[string]bool{onboard.KindCouncil: true, onboard.KindDotWeb: true}

// holdByKind holds a request to an agent whose kind is in holdByDefault and
// that approval.json does not name, even with no approval.json at all. An
// explicit entry wins, so {"from": []} holds nothing. A failed kind lookup
// holds rather than delivers.
//
// A council's asks (answer, review, chairman) continue a council question
// the owner already approved, so they are not held again by kind: when the
// parent went to a council-kind agent and was approved, the default hold is
// skipped. The convening ask to the council has no such parent and stays
// held.
func (p *Policy) holdByKind(ctx context.Context, req *envelope.Request, parent envelope.Request, hasParent bool) {
	entry, ttl, notify := p.cfg.Approval.holdDefaults(req.To)
	if entry {
		return
	}
	if hasParent && parent.Approved {
		if kind, err := p.kindOf(ctx, parent.To); err == nil && kind == onboard.KindCouncil {
			// Only a dot's hold is lifted: an approved council asking
			// another council must not start it without its own approval.
			if target, err := p.kindOf(ctx, req.To); err == nil && target == onboard.KindDotWeb {
				return
			}
		}
	}
	if kind, err := p.kindOf(ctx, req.To); err == nil && !holdByDefault[kind] {
		return
	}
	req.Status = envelope.StatusHeld
	req.HoldTTL = ttl
	req.ApprovalNotify = notify
}

// parent resolves the request's parent: the one it names, or else the
// request the sender currently has claimed. The model cannot opt out of a
// chain by leaving the parent blank.
func (p *Policy) parent(ctx context.Context, req *envelope.Request) (envelope.Request, bool, error) {
	if req.ParentID == "" {
		return p.st.OpenClaim(ctx, req.From)
	}
	parent, status, err := p.st.Request(ctx, req.ParentID)
	if errors.Is(err, store.ErrNotFound) {
		return envelope.Request{}, false, reject(http.StatusBadRequest, ErrBadParent)
	}
	if err != nil {
		return envelope.Request{}, false, err
	}
	if parent.To != req.From || (status != envelope.StatusClaimed && status != envelope.StatusDelivered) {
		return envelope.Request{}, false, reject(http.StatusBadRequest, ErrBadParent)
	}
	return parent, true, nil
}

// Refund returns the urgent slot req took in Prepare, for a send the relay
// then failed to queue (a bad attachment, say), so a failed send never
// spends the sender's urgent allowance.
func (p *Policy) Refund(req envelope.Request) {
	if !req.Urgent {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if i := slices.IndexFunc(p.urgent[req.From], req.CreatedAt.Equal); i >= 0 {
		p.urgent[req.From] = slices.Delete(p.urgent[req.From], i, i+1)
	}
}

func (p *Policy) rate(sender string, urgent bool) (time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	cutoff := now.Add(-time.Minute)
	recent := slices.DeleteFunc(p.sent[sender], func(t time.Time) bool { return !t.After(cutoff) })
	if len(recent) >= p.cfg.PerMinute {
		p.sent[sender] = recent
		return time.Time{}, reject(http.StatusTooManyRequests, ErrRateLimited)
	}
	if urgent {
		cutoff := now.Add(-time.Hour)
		recentUrgent := slices.DeleteFunc(p.urgent[sender], func(t time.Time) bool { return !t.After(cutoff) })
		p.urgent[sender] = recentUrgent
		if len(recentUrgent) >= p.cfg.UrgentPerHour {
			return time.Time{}, reject(http.StatusTooManyRequests, ErrUrgentLimited)
		}
		p.urgent[sender] = append(recentUrgent, now)
	}
	p.sent[sender] = append(recent, now)
	return now, nil
}

func reject(code int, err error) error { return &relay.StatusError{Code: code, Err: err} }

// NotifyDestination exposes the configured operator destination.
func (p *Policy) NotifyDestination() string { return p.cfg.Approval.NotifyDestination() }
