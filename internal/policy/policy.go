// Package policy fills in a request's chain and stops runaway loops between
// trusted agents. It never restricts who may ask whom among teammates: they
// trust each other. It enforces the hop limit, rejects cycles, rate-limits
// each sender as a backstop against an agent stuck starting new chains, and
// holds asks for the owner's approval (approval.json, by default asks to the
// kinds in holdByDefault, and by default a guest's ask and what follows from
// it). The relay refuses a guest's send before it gets here unless the guest
// is listed for the target.
package policy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
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
	sender, _, err := p.st.AgentByName(ctx, req.From)
	if err != nil {
		return reject(http.StatusServiceUnavailable, err)
	}
	parent, ok, err := p.parent(ctx, req, sender)
	if err != nil {
		return err
	}
	// A session label files an ask under the session's own claim, but never
	// loosens a check: when the agent-wide rule would continue a claim the
	// session-scoped one does not, the ask keeps that claim's chain and hop
	// for the cycle, hop, and allowlist checks, and only starts its own
	// trace.
	guard, guarded := parent, ok
	if !ok && req.ParentID == "" && req.FromSession != "" && !req.NoParent && !sender.Guest.Set() {
		if guard, guarded, err = p.st.OpenClaim(ctx, req.From); err != nil {
			return err
		}
	}
	if req.Kind == envelope.KindPing && guarded {
		return reject(http.StatusBadRequest, errors.New("ping cannot continue a request chain"))
	}
	if ok {
		req.ParentID, req.TraceID, req.Hop = parent.ID, parent.TraceID, parent.Hop+1
		// A follow-on's sender is already in the chain, as the first
		// request's sender; the agent that answered joins it, so a follow-on
		// back to that agent is a cycle.
		last := req.From
		if req.FollowOf != "" {
			last = parent.To
		}
		req.Chain = append(slices.Clone(parent.Chain), last)
	} else if guarded {
		req.ParentID, req.TraceID, req.Hop = "", "", guard.Hop+1
		req.Chain = append(slices.Clone(guard.Chain), req.From)
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
	if err := p.markGuest(ctx, req, sender, parent, ok); err != nil {
		p.Refund(*req)
		return err
	}
	// Ahead of Approval.Held, so an approval.json entry cannot lift it. The
	// file's TTL and notify still apply.
	noHold := sender.Guest.Set() && sender.Guest.NoHold
	if req.FromGuest && !req.GuestApproved && !noHold {
		req.Status = envelope.StatusHeld
		req.HoldTTL, req.ApprovalNotify = p.cfg.Approval.guestHold(req.To)
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
	// A follow-on has no parent here: the owner's approval of the request it
	// follows covers that request, not whatever was queued to run after it.
	p.holdByKind(ctx, req, parent, ok && req.FollowOf == "")
	return nil
}

// markGuest sets FromGuest on a request a guest sent, and on one sent while
// the sender holds any claim a guest's ask led to, however the request found
// its parent: the parent, the session guard's claim, or several claims the
// relay cannot pick between. The store's count covers the last two, and a
// parent named to get away from the marked claim. A named parent the sender
// only had delivered is the one case the count misses, so it is read too.
// GuestApproved says the owner approved all of them. A follow-on never
// inherits that, for the reason holdByKind gets no parent for it. The body
// line is added here, once, for clients that ignore the field: first and with
// its text framed on what a guest wrote, so it can choose no conversation or
// form and cannot pass for another request, and through markedBody on what a
// teammate wrote. A relay fire (NoParent) reads no claims: its text was fixed
// when it was scheduled, and a schedule cannot be made while a marked ask is
// open with its asker or changed after, so what the asker holds at fire time
// did not lead to it.
func (p *Policy) markGuest(ctx context.Context, req *envelope.Request, sender identity.Agent, parent envelope.Request, hasParent bool) error {
	if sender.Guest.Set() {
		req.FromGuest, req.GuestApproved = true, false
	} else if !req.NoParent {
		marked, unapproved, err := p.st.OpenGuestClaims(ctx, req.From)
		if err != nil {
			return reject(http.StatusServiceUnavailable, err)
		}
		req.FromGuest = marked > 0 || (hasParent && parent.FromGuest)
		req.GuestApproved = req.FromGuest && unapproved == 0 && (!hasParent || !parent.FromGuest || parent.GuestApproved) && req.FollowOf == ""
	}
	switch {
	case sender.Guest.Set():
		req.Body = envelope.GuestPrefix + envelope.GuestText(req.Body)
	case req.FromGuest:
		req.Body = p.markedBody(ctx, *req)
	}
	return nil
}

// structuredKinds parse a request's body as a form, so they get no body line.
// They are programs, not models, and what they send on is marked in turn.
var structuredKinds = map[string]bool{onboard.KindCouncil: true, onboard.KindPass: true, onboard.KindNotes: true, onboard.KindHistory: true}

// markedBody is the body of a marked request a teammate wrote, with the
// guest line where it keeps the teammate's first-line directive working: a
// web teammate reads a threading line only as the first line. A failed kind
// lookup keeps the line.
func (p *Policy) markedBody(ctx context.Context, req envelope.Request) string {
	if kind, err := p.kindOf(ctx, req.To); err == nil && structuredKinds[kind] {
		return req.Body
	}
	if first, rest, _ := strings.Cut(req.Body, "\n"); envelope.IsThreadingLine(first) {
		return first + "\n" + envelope.GuestPrefix + rest
	}
	return envelope.GuestPrefix + req.Body
}

// ErrMoveHeld refuses moving a request to a target whose asks are held
// for the owner: the owner approved nothing for that target.
var ErrMoveHeld = errors.New("asks to this agent wait for the owner's approval")

// CheckMove runs the send checks on an existing request for a new target,
// to, as failover moves it there (relay.MoveChecker): the chain must not
// loop back to to, the hop limit still holds, and neither approval.json
// nor a kind's default hold would hold an ask to to. A move that would be
// held is refused rather than held. The sender's rate is not charged: the
// request was already sent.
func (p *Policy) CheckMove(ctx context.Context, req envelope.Request, to string) error {
	if slices.Contains(req.Chain, to) || req.From == to {
		return reject(http.StatusConflict, fmt.Errorf("%w: %s is already in %v", ErrCycle, to, req.Chain))
	}
	if req.Hop > p.cfg.HopLimit {
		return reject(http.StatusConflict, fmt.Errorf("%w: hop %d exceeds %d", ErrHopLimit, req.Hop, p.cfg.HopLimit))
	}
	moved := req
	moved.To, moved.Status = to, ""
	held, err := p.cfg.Approval.Held(&moved)
	if err != nil {
		return reject(http.StatusServiceUnavailable, err)
	}
	if !held {
		p.holdByKind(ctx, &moved, envelope.Request{}, false)
		held = moved.Status == envelope.StatusHeld
	}
	if held {
		return reject(http.StatusConflict, fmt.Errorf("%w: %s", ErrMoveHeld, to))
	}
	return nil
}

// holdByDefault lists the target kinds whose asks and notifies wait for the
// owner's approval even with no approval.json entry. A council sends the ask
// on to every model vendor on the team. dot-web types each request into the
// owner's dot DM as the owner, and a dot acts through the owner's connected
// apps; duet-web does the same in the owner's Duet channel with Claude. A pass sends one of the owner's chats to another vendor. Replies
// and answers never pass through Prepare, so replies to a dot's own asks
// are not held.
var holdByDefault = map[string]bool{onboard.KindCouncil: true, onboard.KindDotWeb: true, onboard.KindDuetWeb: true, onboard.KindPass: true}

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
			// Only a dot's or a Duet channel's hold is lifted: an approved
			// council asking another council must not start it without
			// its own approval.
			if target, err := p.kindOf(ctx, req.To); err == nil && onboard.IsHeldWebKind(target) {
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
// chain by leaving the parent blank. A sender that names a session continues
// only a claim made from that session or with none (see Prepare). A relay
// fire (NoParent) has no parent, whatever the sender holds. A
// follow-on's parent is the request it follows, answered and addressed to
// someone else, which is why FollowOf is relay-only: a client's parent_id
// keeps both checks. A guest has none: the relay refuses its parent_id, and
// the only claims it can hold are the relay's notices.
func (p *Policy) parent(ctx context.Context, req *envelope.Request, sender identity.Agent) (envelope.Request, bool, error) {
	if req.NoParent {
		return envelope.Request{}, false, nil
	}
	if sender.Guest.Set() {
		return envelope.Request{}, false, nil
	}
	if req.FollowOf != "" {
		parent, _, err := p.st.Request(ctx, req.FollowOf)
		if errors.Is(err, store.ErrNotFound) {
			return envelope.Request{}, false, reject(http.StatusBadRequest, ErrBadParent)
		}
		return parent, err == nil, err
	}
	if req.ParentID == "" {
		if req.FromSession != "" {
			return p.st.OpenClaimFor(ctx, req.From, req.FromSession)
		}
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

// Charge takes one slot of sender's per-minute send rate for something that
// is not a request, such as a room post, and fails with ErrRateLimited (429)
// when none is left.
func (p *Policy) Charge(sender string) error {
	_, err := p.rate(sender, false)
	return err
}

// Gated reports whether asks to target can be held by approval.json, which
// keeps it out of rooms. The default holds by kind (council, dot-web) are
// the relay's to check.
func (p *Policy) Gated(target string) (bool, error) { return p.cfg.Approval.Gated(target) }

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
