package relay

// Failover (R7-R10): an ask to a web chat teammate whose site is out of
// messages, and that nothing has typed into a site yet, moves to another
// web chat teammate that is online, signed in and not limited. It keeps
// its id, sender and chain; the asker gets one relay note naming the
// teammate that took it. A move happens at two points only, both before
// anything reaches a site: when the ask is queued (on send, and on each
// sweep) to a target already limited, and when the target hands back an
// ask it claimed but did not send (POST /v1/requests/{id}/handback). With
// nowhere to go the ask stays with its target, held back until the limit
// resets, and the asker is told it waits, unless the reset is after the
// ask would expire: then it is failed at once with a reply saying so.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// FailoverFile is the owner's failover policy in the relay state dir:
// {"exclude": ["perplexity-web"]} names web agents failover never moves an
// ask to. A missing file excludes none; an unreadable or invalid one stops
// every move until it is fixed.
const FailoverFile = "failover.json"

// Relay note kinds for failover, beside the store's: the ask moved to
// another teammate (one note per move, its episode the move count), or it
// waits for its target's limit to reset (one per reset time).
const (
	noteMoved     = "moved"
	noteLimitWait = "limit_wait"
)

// MoveChecker is an optional Preparer extension: it runs the send checks
// (loop, hop, approval and kind holds) on an existing request for a new
// target, without charging the sender's rate. A move it refuses does not
// happen. Without it only the loop check runs.
type MoveChecker interface {
	CheckMove(ctx context.Context, req envelope.Request, to string) error
}

// failoverIneligible says why req may not move off its target, or "" when
// it may. It is a pure function of the stored request, whether it was
// ever claimed (claimed: a web agent may already have typed it into its
// site), and the kinds of its sender and target. An ask never moves when
// it is to or from dot-web, duet-web or any agent outside the six web chat kinds
// (the target), from a council, was ever held, is not an ask, is part of a
// group, came from a room mention (the reply posts to the room as the
// mentioned member), carries attachments, names a conversation, has a
// clarification exchange or was resumed, follows an answered request (its
// body carries a teammate's reply), comes from a guest (the owner listed its
// teammates by name), or may have been sent already.
func failoverIneligible(req envelope.Request, claimed bool, fromKind, toKind string) string {
	first, _, _ := strings.Cut(req.Body, "\n")
	switch {
	case !onboard.IsWebChatKind(toKind):
		return "the target is not a web chat teammate"
	case onboard.IsHeldWebKind(fromKind) || fromKind == onboard.KindCouncil:
		return "the asker is " + fromKind
	case req.Kind != envelope.KindAsk:
		return "it is a " + string(req.Kind)
	case req.WasHeld:
		return "it was held for the owner's approval"
	case req.FollowOf != "":
		return "it is a follow-on"
	case req.FromGuest:
		return "it comes from a guest"
	case req.Group != "":
		return "it is part of a group"
	case req.RoomPostID != 0:
		return "it came from a room mention"
	case len(req.Attachments) > 0:
		return "it carries attachments"
	case strings.HasPrefix(strings.ToLower(strings.TrimSpace(first)), "conversation:"):
		return "it names a conversation"
	case len(req.Exchanges) > 0 || req.Resumed:
		return "it has a clarification exchange"
	case claimed:
		return "it may already have been sent"
	}
	return ""
}

// failoverExcluded reads the owner's failover.json. Each pass reads it
// once, so an edit takes effect on the next pass. A missing file excludes
// none. An unreadable or invalid one fails closed: ok is false, so no ask
// moves until it is fixed, and the problem is logged once per distinct
// problem.
func (s *Server) failoverExcluded() (names []string, ok bool) {
	path := s.cfg.FailoverPath
	if path == "" {
		return nil, true
	}
	names, err := readFailover(path)
	s.failoverMu.Lock()
	defer s.failoverMu.Unlock()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != s.failoverErr {
		if msg != "" {
			log.Printf("%s: %v; failover moves no ask until it is fixed", path, err)
		}
		s.failoverErr = msg
	}
	return names, err == nil
}

// readFailover parses failover.json: one object whose only key, exclude,
// lists agent names. Anything else, a top-level null included, is invalid.
func readFailover(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<16+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<16 {
		return nil, errors.New("too large")
	}
	// A pointer so a top-level null, which would decode into a zero struct
	// as an empty list, is told apart and fails closed.
	var in *struct {
		Exclude []string `json:"exclude"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return nil, fmt.Errorf("invalid: %w", err)
	}
	if in == nil {
		return nil, errors.New("invalid: null, not an object")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid: more than one object")
	}
	for _, n := range in.Exclude {
		if !identity.ValidName(n) {
			return nil, fmt.Errorf("invalid agent name %q", n)
		}
	}
	return in.Exclude, nil
}

// limitFacts is what failover needs about the roster, read once per pass:
// each agent's kind and, for a web agent that reported one, its usage
// limit's reset time while that is ahead, and when anything is limited,
// the teammates an ask could move to.
type limitFacts struct {
	kinds   map[string]string
	limited map[string]time.Time
	// targets are the web chat teammates that are online, last observed
	// signed in, not limited and not excluded by the owner, by least
	// backlog and then name; none while failover.json is invalid.
	// failoverTargets filters them per ask.
	targets []string
	// backlog is each target's queued and claimed requests.
	backlog map[string]int
}

// sortTargets orders f.targets by least backlog and then name.
func (f *limitFacts) sortTargets() {
	slices.SortFunc(f.targets, func(a, b string) int {
		if f.backlog[a] != f.backlog[b] {
			return f.backlog[a] - f.backlog[b]
		}
		return strings.Compare(a, b)
	})
}

// took counts an ask just moved to name in its backlog, so the next ask
// in the same pass goes to whichever target is now least busy.
func (f *limitFacts) took(name string) {
	f.backlog[name]++
	f.sortTargets()
}

// failoverFacts reads limitFacts. ok is false when nothing is limited (the
// common case), so a sweep does no more and targets is left empty.
func (s *Server) failoverFacts(ctx context.Context) (limitFacts, bool, error) {
	statuses, err := s.store.WebStatuses(ctx)
	if err != nil {
		return limitFacts{}, false, err
	}
	agents, err := s.dir.Agents(ctx)
	if err != nil {
		return limitFacts{}, false, err
	}
	now := s.cfg.Now()
	f := limitFacts{kinds: map[string]string{}, limited: map[string]time.Time{}}
	// signedIn: the agent's last observation of its site, from its
	// current identity, was signed in (an agent that never reported one is
	// not known to be).
	signedIn := map[string]bool{}
	for _, a := range agents {
		f.kinds[a.Name] = a.Kind
		if a.Guest.Set() {
			continue // never a target, whatever its kind says
		}
		var t envelope.Target
		statuses[a.Name].Apply(a, &t, now)
		if !t.LimitedUntil.IsZero() {
			f.limited[a.Name] = t.LimitedUntil
		}
		signedIn[a.Name] = !t.WebStatusObservedAt.IsZero() && t.SignedOutSite == ""
	}
	if len(f.limited) == 0 {
		return f, false, nil
	}
	stats, err := s.store.QueueStats(ctx)
	if err != nil {
		return limitFacts{}, false, err
	}
	excluded, ok := s.failoverExcluded()
	if !ok {
		return f, true, nil
	}
	s.mu.Lock()
	for name, kind := range f.kinds {
		if !onboard.IsWebChatKind(kind) || slices.Contains(excluded, name) || !signedIn[name] || !f.limited[name].IsZero() || !s.polledRecently(s.lastPoll[name], now) {
			continue
		}
		f.targets = append(f.targets, name)
	}
	s.mu.Unlock()
	f.backlog = map[string]int{}
	for _, name := range f.targets {
		f.backlog[name] = stats[name].Queued + stats[name].Claimed
	}
	f.sortTargets()
	return f, true, nil
}

// failoverTargets are the teammates req could move to, best first: f's
// targets other than req's asker, current target or a target it moved
// from.
func failoverTargets(req envelope.Request, f limitFacts) []string {
	var out []string
	for _, name := range f.targets {
		if name != req.To && name != req.From && !slices.Contains(req.MovedFrom, name) {
			out = append(out, name)
		}
	}
	return out
}

// checkMove runs the policy's move checks for req to to, or only the loop
// check when the preparer has none.
func (s *Server) checkMove(ctx context.Context, req envelope.Request, to string) error {
	if mc, ok := s.preparer().(MoveChecker); ok {
		return mc.CheckMove(ctx, req, to)
	}
	if slices.Contains(req.Chain, to) || req.From == to {
		return errors.New("the move would loop back to the asker's chain")
	}
	return nil
}

// failoverOutcome is what failover did with one ask.
type failoverOutcome struct {
	moved bool
	// failed: the hold would have outlived the request, so the relay
	// closed it failed and told the asker instead.
	failed bool
	req    envelope.Request // the request as it stands after
	until  time.Time        // the old target's reset time
}

// failover moves req, an eligible ask to a target limited until until, to
// the best target that passes the move checks, or else holds it back on
// its target until until. claimed says req is held by its target's live
// claim (a handback). heldUntil is when a queued req is already held back
// to (zero for none): when that is not before until (to the millisecond
// the store keeps), the hold and its note are already in place and no
// write is made. It tells the asker either way, wakes the new
// target after a move, and records the audit event. It returns
// ErrWrongState when the request changed under it (a Deliver or Claim won).
func (s *Server) failover(ctx context.Context, req envelope.Request, f limitFacts, until, heldUntil time.Time, claimed bool) (failoverOutcome, error) {
	now := s.cfg.Now()
	from := req.To
	reason := envelope.WebLimited
	if claimed {
		reason = "handed back"
	}
	for _, to := range failoverTargets(req, f) {
		if err := s.checkMove(ctx, req, to); err != nil {
			continue
		}
		moved, err := s.store.Move(ctx, req.ID, from, to, claimed)
		if err != nil {
			return failoverOutcome{}, err
		}
		s.record(ctx, "moved", moved.ID, moved.TraceID, "relay", store.DetailJSON(map[string]any{"from": from, "to": to, "reason": reason}))
		note := fmt.Sprintf("%s is out of messages until %s; %s took your request. %s starts without any earlier conversation you had with %s, so this request has only its own text. Do not send it again.",
			from, client.UTCClock(until, now), to, to, from)
		s.noteAndTell(ctx, moved, noteMoved, int64(len(moved.MovedFrom)), note, now)
		s.hub.notify(requestKey(moved.ID))
		s.hub.notify(inboxKey(to))
		if s.events != nil {
			s.events.Queued(ctx, moved)
		}
		return failoverOutcome{moved: true, req: moved, until: until}, nil
	}
	// A hold keeps the request's own expiry, so one that ends at or after it
	// would let the ask lapse unsent with the asker still waiting. Fail it
	// now instead, while the asker can still do something about it. This
	// runs before the existing-hold shortcut, so a hold already saved past
	// the expiry (an older relay allowed one) is failed too.
	expires, err := s.store.RequestExpiry(ctx, req.ID)
	if err != nil {
		return failoverOutcome{}, err
	}
	if !until.Before(expires) {
		return s.failOutlivedHold(ctx, req, until, now, claimed)
	}
	if !claimed && !heldUntil.IsZero() && heldUntil.UnixMilli() >= until.UnixMilli() {
		return failoverOutcome{req: req, until: until}, nil
	}
	held, err := s.store.HoldBack(ctx, req.ID, from, until, claimed)
	if err != nil {
		return failoverOutcome{}, err
	}
	note := fmt.Sprintf("%s is out of messages until %s and no other web chat teammate can take this request now, so it waits and goes to %s then. If it can't wait, cancel it and ask another teammate.",
		from, client.UTCClock(until, now), from)
	s.noteAndTell(ctx, held, noteLimitWait, until.UnixMilli(), note, now)
	if claimed {
		s.hub.notify(requestKey(held.ID))
	}
	return failoverOutcome{req: held, until: until}, nil
}

// failOutlivedHold closes req, an ask its limited target cannot take
// before until and no teammate can take now, failed with a reply that says
// why, since until is after req would expire. The reply goes through the
// one reply path, as req's target, so the asker's waiters, held poll and
// reply wake hear of it like any other reply. Its write keeps HoldBack's
// conditions in the same transaction (store.FailHeld): req must still be
// queued, or for a handback (claimed) still under its live claim, so a
// claim that won after the caller read req is not closed under its
// claimant; that returns ErrWrongState.
func (s *Server) failOutlivedHold(ctx context.Context, req envelope.Request, until, now time.Time, claimed bool) (failoverOutcome, error) {
	body := fmt.Sprintf("%s is out of messages until %s, after this ask would expire; ask another teammate or re-ask after the reset.",
		req.To, client.UTCClock(until, now))
	rep := envelope.Reply{Status: envelope.StatusFailed, Body: body}
	if _, err := s.recordReplyWith(ctx, req.ID, req.To, rep, false, func(rep envelope.Reply) (envelope.Reply, error) {
		return s.store.FailHeld(ctx, req.ID, req.To, claimed, rep)
	}); err != nil {
		return failoverOutcome{}, err
	}
	s.record(ctx, "limit_outlives", req.ID, req.TraceID, "relay", store.DetailJSON(map[string]any{"agent": req.To, "until": until.UTC()}))
	return failoverOutcome{failed: true, req: req, until: until}, nil
}

// failoverQueued is failover for an ask just queued: when its target is a
// limited web chat teammate and the ask may move, it moves or is held
// back, and the request as it now stands is returned with true when it
// moved (the new target is already woken).
func (s *Server) failoverQueued(ctx context.Context, req envelope.Request) (envelope.Request, bool) {
	// Most asks are not to a web chat teammate; they skip the roster read.
	to, _, err := s.dir.Agent(ctx, req.To)
	if err != nil {
		log.Printf("failover %s: %v", req.ID, err)
		return req, false
	}
	if !onboard.IsWebChatKind(to.Kind) {
		return req, false
	}
	f, _, err := s.failoverFacts(ctx)
	if err != nil {
		log.Printf("failover %s: %v", req.ID, err)
		return req, false
	}
	until := f.limited[req.To]
	if until.IsZero() || failoverIneligible(req, false, f.kinds[req.From], f.kinds[req.To]) != "" {
		return req, false
	}
	out, err := s.failover(ctx, req, f, until, time.Time{}, false)
	if err != nil {
		if !errors.Is(err, store.ErrWrongState) {
			log.Printf("failover %s: %v", req.ID, err)
		}
		return req, false
	}
	return out.req, out.moved
}

// failoverSweep tries again, on each sweep, for every queued ask to a
// limited web chat teammate: a teammate may have come online or its limit
// passed since the ask was held back. First it releases held-back asks
// whose target is no longer limited, and holds those whose target now
// resets sooner only until then, waking the targets of released asks.
func (s *Server) failoverSweep(ctx context.Context) {
	f, limited, err := s.failoverFacts(ctx)
	if err != nil {
		log.Printf("failover sweep: %v", err)
		return
	}
	released, err := s.store.ReleaseHolds(ctx, f.limited)
	if err != nil {
		log.Printf("failover sweep: %v", err)
	}
	for _, name := range released {
		s.hub.notify(inboxKey(name))
	}
	if !limited {
		return
	}
	for name, until := range f.limited {
		if !onboard.IsWebChatKind(f.kinds[name]) {
			continue
		}
		cands, err := s.store.MoveCandidates(ctx, name)
		if err != nil {
			log.Printf("failover sweep %s: %v", name, err)
			continue
		}
		for _, c := range cands {
			if failoverIneligible(c.Request, c.Claimed, f.kinds[c.Request.From], f.kinds[name]) != "" {
				continue
			}
			out, err := s.failover(ctx, c.Request, f, until, c.DeliverAfter, false)
			if err != nil && !errors.Is(err, store.ErrWrongState) {
				log.Printf("failover sweep %s: %v", c.Request.ID, err)
			}
			if out.moved {
				f.took(out.req.To)
			}
		}
	}
}

// handleHandback takes back an ask its target claimed but did not send
// because its site is out of messages: {"reason": "limited", "until":
// <reset time>}. Only the claimant may hand an ask back, under its live
// claim. The reset time is capped at the limit the agent last reported,
// and an agent that reported none gets 409. An ask that may not move,
// including one an earlier claim on lapsed, gets 409 and stays claimed, so
// the agent answers it itself. An eligible one moves, or with nowhere to
// go is requeued on its target, held back until the reset.
func (s *Server) handleHandback(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	var in struct {
		Reason string    `json:"reason"`
		Until  time.Time `json:"until"`
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := d.Decode(&in)
	if err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("expected one handback object")
		}
	}
	now := s.cfg.Now()
	if err == nil && (in.Reason != envelope.WebLimited || !in.Until.After(now) || in.Until.After(now.Add(MaxWebLimit))) {
		err = errors.New(`a handback needs reason "limited" and an until within 8 days`)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	req, status, err := s.store.Request(r.Context(), id)
	if err == nil && req.To != name {
		err = store.ErrNotFound
	}
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if status != envelope.StatusClaimed {
		writeErr(w, http.StatusConflict, errors.New("only a claimed request can be handed back"))
		return
	}
	f, _, err := s.failoverFacts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// This claim is the handback's own, vouched unsent; an earlier claim
	// that lapsed is not, and its claimant may have sent the ask.
	lapsed, err := s.store.ClaimLapsed(r.Context(), id)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	reported := f.limited[name]
	why := failoverIneligible(req, lapsed, f.kinds[req.From], f.kinds[name])
	if why == "" && reported.IsZero() {
		why = "this agent has reported no usage limit"
	}
	if why != "" {
		s.record(r.Context(), "handed_back", id, req.TraceID, name, store.DetailJSON(map[string]any{"outcome": "refused"}))
		writeErr(w, http.StatusConflict, errors.New("not moved: "+why))
		return
	}
	until := in.Until
	if until.After(reported) {
		until = reported
	}
	out, err := s.failover(r.Context(), req, f, until, time.Time{}, true)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	res := client.HandbackResult{Outcome: client.HandbackRequeued, Until: until.UTC()}
	switch {
	case out.moved:
		res = client.HandbackResult{Outcome: client.HandbackMoved, To: out.req.To}
	case out.failed:
		res = client.HandbackResult{Outcome: client.HandbackFailed, Until: until.UTC()}
	}
	s.record(r.Context(), "handed_back", id, req.TraceID, name, store.DetailJSON(map[string]any{"outcome": res.Outcome}))
	writeJSON(w, http.StatusOK, res)
}
