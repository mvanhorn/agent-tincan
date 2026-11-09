package relay

// Guests: a time-limited agent that can ask only the teammates it was invited
// for, and nothing else. A guest is a new kind of caller on a relay whose
// other callers trust each other fully, so everything here is default-deny:
// guestRoutes closes every route it does not name, including ones added
// later, and guestSendRefusal closes every send field it does not name. See
// docs/trust-model.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// MaxGuestTerm caps how long a guest invite can last.
const MaxGuestTerm = 30 * 24 * time.Hour

// guestRoutes are the only routes a guest may use. A route not named here
// answers 403 before its handler runs.
var guestRoutes = map[string]bool{
	"POST /v1/send":                 true,
	"GET /v1/poll":                  true,
	"POST /v1/replies/ack":          true,
	"GET /v1/requests/{id}":         true,
	"POST /v1/requests/{id}/claim":  true, // relay notices only, see handleClaim
	"POST /v1/requests/{id}/answer": true,
	"POST /v1/requests/{id}/cancel": true,
	"GET /v1/agents":                true, // listed teammates only, see guestRoster
	"GET /v1/whoami":                true,
	"GET /v1/hello":                 true,
	"GET /v1/capabilities":          true, // fixed flags; a client reads it before answer
}

// inviteGuest answers an invite that carries a guest's limits. Name and term
// are checked here, before the directory mints a code.
func (s *Server) inviteGuest(w http.ResponseWriter, r *http.Request, name, kind string, in client.GuestInvite) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	if kind != "" {
		writeErr(w, http.StatusBadRequest, errors.New("a guest takes no kind"))
		return
	}
	terms, err := s.guestTerms(r, name, in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	code, err := s.dir.InviteGuest(r.Context(), s.remote(r), name, terms)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "guest_invited", "", "", name, store.DetailJSON(map[string]any{"guest_asks": in.Ask, "guest_until": terms.Until, "no_hold": in.NoHold}))
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "code": code, "expires_in": identity.InviteTTL.String(),
		"guest_until": terms.Until, "guest_asks": in.Ask, "hold_alerts": s.holdAlerts(r)})
}

// guestTerms checks a guest's term and list and turns them into its terms,
// with the term starting now.
func (s *Server) guestTerms(r *http.Request, name string, in client.GuestInvite) (identity.GuestTerms, error) {
	term := time.Duration(in.ForSeconds) * time.Second
	if term <= 0 || term > MaxGuestTerm {
		return identity.GuestTerms{}, errors.New("a guest's term must be above zero and at most 30 days")
	}
	if len(in.Ask) == 0 {
		return identity.GuestTerms{}, errors.New("a guest must be listed for at least one teammate")
	}
	for _, to := range in.Ask {
		if err := s.guestTarget(r, name, to); err != nil {
			return identity.GuestTerms{}, err
		}
	}
	return identity.GuestTerms{Until: s.cfg.Now().Add(term).UTC().Truncate(time.Millisecond), Asks: in.Ask, NoHold: in.NoHold}, nil
}

// connectGuest answers a connect that carries a guest's limits. The caller
// has checked admin rights and that a gateway runs. A gateway guest is always
// held: anyone holding its token is on the open internet, with no tailnet
// account behind them, so --no-hold is not offered and a relay refuses it.
func (s *Server) connectGuest(w http.ResponseWriter, r *http.Request, name string, replace bool, in client.GuestInvite) {
	conn, ok := s.conn.(GuestConnector)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("this gateway does not take guests"))
		return
	}
	if replace || in.NoHold {
		writeErr(w, http.StatusBadRequest, errors.New("a gateway guest takes neither --replace nor --no-hold"))
		return
	}
	terms, err := s.guestTerms(r, name, in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	code, base, err := conn.ConnectGuest(r.Context(), name, terms)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	cards := make([]string, 0, len(in.Ask))
	for _, to := range in.Ask {
		cards = append(cards, base+"/a2a/"+to+"/.well-known/agent-card.json")
	}
	s.record(r.Context(), "guest_connected", "", "", name, store.DetailJSON(map[string]any{"guest_asks": in.Ask, "guest_until": terms.Until}))
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "code": code, "expires_in": "10m0s", "cards": cards, "unpublished": conn.Unpublished(in.Ask),
		"guest_until": terms.Until, "guest_asks": in.Ask, "hold_alerts": s.holdAlerts(r)})
}

// guestTarget says why guest name may not list to, nil when it may: to must be
// a joined teammate that is not a phone or a service. What those services
// retrieve is the owner's private data, and a --no-hold guest would read it
// with no one in between.
func (s *Server) guestTarget(r *http.Request, name, to string) error {
	if to == name {
		return fmt.Errorf("%s cannot be listed for itself", to)
	}
	a, ok, err := s.dir.Agent(r.Context(), to)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("no such teammate: %s", to)
	case a.Guest.Set():
		return fmt.Errorf("%s is a guest", to)
	case a.Kind == onboard.KindPhone, roomServiceKinds[a.Kind]:
		return fmt.Errorf("%s is a %s agent; a guest cannot ask it", to, a.Kind)
	}
	return nil
}

// holdAlerts reports whether a held ask would reach the owner on its own: a
// notify teammate in approval.json, or a phone subscribed to push.
func (s *Server) holdAlerts(r *http.Request) bool {
	if s.notifyDestination() != "" {
		return true
	}
	subs, err := s.store.PushSubscriptions(r.Context())
	return err == nil && len(subs) > 0
}

// guestKey is the context key of the guest behind a request.
const guestKey ctxKey = 3

// guestOf returns the guest behind r, set by guestGate.
func guestOf(ctx context.Context) (identity.Agent, bool) {
	a, ok := ctx.Value(guestKey).(identity.Agent)
	return a, ok
}

// callerKey is the context key of the gate's read of the caller.
const callerKey ctxKey = 4

// gateCaller is the gate's read of the caller, or why it failed.
type gateCaller struct {
	caller identity.Caller
	err    error
}

// resolve finds the agent behind r from the gate's read, so that a guest
// whose join lands after the gate passed this call as a non-guest's is not
// served as a guest with none of a guest's limits. Only the local admin
// socket, which has no gate, reads the caller here.
func (s *Server) resolve(r *http.Request) (identity.Resolved, error) {
	claimed := r.Header.Get(client.AgentHeader)
	if g, ok := r.Context().Value(callerKey).(gateCaller); ok {
		if g.err != nil {
			return identity.Resolved{}, g.err
		}
		return s.dir.ResolveCaller(r.Context(), g.caller, claimed)
	}
	return s.dir.ResolveAgent(r.Context(), r.RemoteAddr, claimed)
}

// errGuestRoute and errGuestEnded are a guest's refusals at the gate.
var (
	errGuestRoute = errors.New("guests may not use this route")
	errGuestEnded = errors.New("this guest's time has ended")
)

// guestGate runs ahead of every route. It resolves the caller, and for a
// guest answers 403 unless the matched pattern is in guestRoutes, so no
// handler sees a guest it was not written for, including handlers that never
// call Server.agent. Past its term a guest gets 403 from every route at once,
// without waiting for the sweep. Every call from a node carrying a guest is
// a guest's. Its read of the caller is the only one the call gets, so the
// handler judges the same caller it did (see resolve). A caller the gate
// cannot place is refused, since it may be a guest; only routes that say
// nothing about any caller stay open to it.
func (s *Server) guestGate(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		c, err := s.dir.Caller(r.Context(), r.RemoteAddr)
		var g identity.Agent
		guest := false
		if err == nil {
			g, guest, err = c.Guest(r.Header.Get(client.AgentHeader))
		}
		switch {
		case err != nil && !guest && callerFreeRoutes[pattern]:
		case err != nil:
			writeErr(w, http.StatusForbidden, err)
			return
		case !guest:
		case g.Guest.Over(s.cfg.Now()):
			writeErr(w, http.StatusForbidden, errGuestEnded)
			return
		case !guestRoutes[pattern]:
			writeErr(w, http.StatusForbidden, errGuestRoute)
			return
		}
		ctx := context.WithValue(r.Context(), callerKey, gateCaller{c, err})
		if guest {
			ctx = context.WithValue(ctx, guestKey, g)
		}
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

// guestTermOver refuses a guest whose term ended after the gate passed its
// call, and reports whether it did. A body can take as long as the server's
// read timeout, and the sweep does not come back for what arrives after it
// has run, so a handler that writes calls this once it has the body.
func (s *Server) guestTermOver(w http.ResponseWriter, r *http.Request) bool {
	g, guest := guestOf(r.Context())
	if !guest || !g.Guest.Over(s.cfg.Now()) {
		return false
	}
	writeErr(w, http.StatusForbidden, errGuestEnded)
	return true
}

// callerFreeRoutes answer the same to anyone, so a caller WhoIs cannot place
// may still use them.
var callerFreeRoutes = map[string]bool{"GET /v1/hello": true, "GET /v1/dist": true, "GET /v1/dist/{name}": true}

// guestSendFields are the only send fields a guest may set. kind is checked
// by value in guestSendRefusal.
var guestSendFields = map[string]bool{"to": true, "body": true, "kind": true}

// guestSendRefusal says why guest g may not send raw, parsed as req, nil when
// it may. It reads the raw JSON, not only the parsed request, so a field this
// relay's parser drops or learns later is closed too: any field outside
// guestSendFields refused when set, in any spelling Go's decoder would take.
// A real client sends some fields empty, which counts as not set. The target
// is checked against the list first, so a guest learns nothing about names
// it was not given.
func guestSendRefusal(g identity.Agent, raw []byte, req envelope.Request) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for k, v := range fields {
		if !guestSendFields[strings.ToLower(k)] && !emptyJSON(v) {
			return fmt.Errorf("a guest's send takes only to and body, not %s", k)
		}
	}
	if !slices.Contains(g.Guest.Asks, req.To) {
		return errors.New("this guest may not ask " + req.To)
	}
	if req.Kind != envelope.KindAsk || req.Then != nil || req.Group != "" || req.Urgent || req.ParentID != "" || len(req.Attachments) > 0 {
		return errors.New("a guest's send takes only to and body")
	}
	return nil
}

// emptyJSON reports whether a raw value is what an unset field looks like.
func emptyJSON(v json.RawMessage) bool {
	switch string(bytes.TrimSpace(v)) {
	case "null", `""`, "false", "0", "[]", "{}":
		return true
	}
	return false
}

// guestRoster is what a guest sees of the team: the teammates it may ask,
// each with its name, kind, good-at line and whether it is online. It is
// built field by field, so a field the roster gains later stays closed.
func (s *Server) guestRoster(ctx context.Context, g identity.Agent) ([]client.AgentInfo, error) {
	full, err := s.rosterWith(ctx, false, false)
	if err != nil {
		return nil, err
	}
	out := []client.AgentInfo{}
	for _, a := range full {
		if slices.Contains(g.Guest.Asks, a.Name) {
			out = append(out, client.AgentInfo{Name: a.Name, Online: a.Online, Wake: "none", Kind: a.Kind, GoodAt: a.GoodAt})
		}
	}
	return out, nil
}

// guestAsker reports whether asker is a guest on roster.
func guestAsker(roster []identity.Agent, asker string) bool {
	return slices.ContainsFunc(roster, func(a identity.Agent) bool { return a.Name == asker && a.Guest.Set() })
}

// noticeRoster is the roster a note to asker may suggest from: never a
// guest, and for a guest asker only the teammates it may ask.
func noticeRoster(roster []identity.Agent, asker string) []identity.Agent {
	var listed []string
	guest := false
	for _, a := range roster {
		if a.Name == asker && a.Guest.Set() {
			listed, guest = a.Guest.Asks, true
		}
	}
	return slices.DeleteFunc(slices.Clone(roster), func(a identity.Agent) bool {
		return a.Guest.Set() || (guest && !slices.Contains(listed, a.Name))
	})
}

// sweepGuests closes out each guest past its term as a removal does: tokens
// revoked first, then open requests cancelled, with one audit row. The row
// stays, so every call from its machine keeps answering 403, its name stays
// taken until tincan remove, and it never counts as a teammate. A step that
// fails leaves the guest for the next sweep.
func (s *Server) sweepGuests(ctx context.Context) {
	guests, err := s.store.GuestsEnding(ctx, s.cfg.Now())
	if err != nil {
		log.Printf("guests: %v", err)
		return
	}
	for _, g := range guests {
		if s.conn != nil {
			if err := s.conn.Revoke(ctx, g.Name); err != nil {
				log.Printf("guest %s: revoke: %v (sweep will retry)", g.Name, err)
				continue
			}
		}
		ids, err := s.store.CancelAllFor(ctx, g.Name)
		if err != nil {
			log.Printf("guest %s: cancel: %v (sweep will retry)", g.Name, err)
			continue
		}
		for _, id := range ids {
			s.hub.notify(requestKey(id))
		}
		if err := s.store.MarkGuestEnded(ctx, g.Name); err != nil {
			log.Printf("guest %s: %v (sweep will retry)", g.Name, err)
			continue
		}
		s.record(ctx, "guest_expired", "", "", g.Name, store.DetailJSON(map[string]any{"cancelled": len(ids), "guest_until": g.Guest.Until.UTC()}))
	}
}
