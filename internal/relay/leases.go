package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

const (
	// LeaseMaxTTL is the longest a work lease lasts between renewals.
	LeaseMaxTTL = 24 * time.Hour
	// LeaseDefaultTTL is how long a lease lasts when the caller names no
	// time.
	LeaseDefaultTTL = time.Hour
	// LeaseMaxPerAgent caps one agent's live leases.
	LeaseMaxPerAgent = 32
)

func clientLease(l store.Lease) client.Lease {
	return client.Lease{Resource: l.Resource, Holder: l.Holder, Note: l.Note, CreatedAt: l.Created.UTC(), ExpiresAt: l.Expires.UTC()}
}

// leaseTTL is the lease time a caller's ttl_seconds asks for, or an error
// when it is negative or past the cap. It compares seconds, since a huge
// value would overflow a Duration and look short.
func leaseTTL(seconds int64) (time.Duration, error) {
	switch {
	case seconds == 0:
		return LeaseDefaultTTL, nil
	case seconds < 0 || seconds > int64(LeaseMaxTTL/time.Second):
		return 0, fmt.Errorf("ttl_seconds must be between 1 and %d", int64(LeaseMaxTTL/time.Second))
	}
	return time.Duration(seconds) * time.Second, nil
}

// handleLeaseAcquire takes or renews a lease. The holder is the
// authenticated caller. A new lease answers 201 and a renewal 200. A
// teammate's overlapping lease answers 409 naming its holder and expiry.
func (s *Server) handleLeaseAcquire(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	var in struct {
		Resource   string `json:"resource"`
		TTLSeconds int64  `json:"ttl_seconds"`
		Note       string `json:"note"`
	}
	if err := decodeOne(r.Body, &in, "lease object"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	resource, err := identity.CheckRosterLine("the lease resource", in.Resource)
	if err == nil && resource == "" {
		err = errors.New("the lease resource is empty")
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	note, err := identity.CheckRosterLine("the lease note", in.Note)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ttl, err := leaseTTL(in.TTLSeconds)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	grant, err := s.store.AcquireLease(r.Context(), name, resource, note, ttl, LeaseMaxPerAgent)
	var held *store.LeaseHeldError
	switch {
	case errors.As(err, &held):
		writeJSON(w, http.StatusConflict, map[string]any{"error": held.Error(), "resource": held.Lease.Resource, "holder": held.Lease.Holder, "expires_at": held.Lease.Expires.UTC()})
		return
	case errors.Is(err, store.ErrLeaseLimit):
		writeErr(w, http.StatusConflict, fmt.Errorf("%s holds %d live leases, the most an agent can", name, LeaseMaxPerAgent))
		return
	case errors.Is(err, store.ErrNoAgent):
		writeErr(w, http.StatusNotFound, errors.New("unknown agent"))
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.recordLapsed(r.Context(), grant.Lapsed)
	event, code := "lease_acquired", http.StatusCreated
	if grant.Renewed {
		event, code = "lease_renewed", http.StatusOK
	}
	s.record(r.Context(), event, "", "", name, store.DetailJSON(map[string]any{"resource": resource, "holder": name, "ttl_seconds": int64(ttl / time.Second), "note_bytes": len(grant.Lease.Note)}))
	out := clientLease(grant.Lease)
	out.Renewed = grant.Renewed
	writeJSON(w, code, out)
}

// handleLeaseRelease drops the lease on exactly the named resource. Only its
// holder or an admin may; an admin's release of a teammate's lease is the one
// way to clear a lease its holder cannot, short of waiting for it to lapse.
// An admin device must ask for that with "admin": true, so an agent that runs
// on one still releases as itself, is refused a teammate's lease, and is
// named in the audit row. The local admin socket has no agent to act as.
func (s *Server) handleLeaseRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Resource string `json:"resource"`
		Admin    bool   `json:"admin"`
	}
	if err := decodeOne(r.Body, &in, "lease object"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	admin := s.isAdmin(r) && (in.Admin || isLocalAdmin(r.Context()))
	name := ""
	if !admin {
		if name = s.agent(w, r); name == "" {
			return
		}
	}
	if in.Resource == "" {
		writeErr(w, http.StatusBadRequest, errors.New("the lease resource is empty"))
		return
	}
	l, err := s.store.ReleaseLease(r.Context(), in.Resource, name)
	switch {
	case errors.Is(err, store.ErrNoLease):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, store.ErrNotLeaseHolder):
		writeErr(w, http.StatusForbidden, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	detail := map[string]any{"resource": l.Resource, "holder": l.Holder, "reason": "holder"}
	actor := name
	if admin {
		detail["reason"], actor = "admin", s.remote(r)
		if !isLocalAdmin(r.Context()) {
			detail["device"] = s.callerNode(r).Name
		}
	}
	s.record(r.Context(), "lease_released", "", "", actor, store.DetailJSON(detail))
	writeJSON(w, http.StatusOK, clientLease(l))
}

// handleLeases lists the live leases to any joined agent or admin.
func (s *Server) handleLeases(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) && s.agent(w, r) == "" {
		return
	}
	leases, err := s.store.Leases(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]client.Lease, 0, len(leases))
	for _, l := range leases {
		out = append(out, clientLease(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{"leases": out})
}

// leasesOf is name's live leases, read so a removal can audit each one. The
// store drops them with the agent whether or not this read works.
func (s *Server) leasesOf(ctx context.Context, name string) []store.Lease {
	leases, err := s.store.Leases(ctx)
	if err != nil {
		log.Printf("leases: %v", err)
		return nil
	}
	return slices.DeleteFunc(leases, func(l store.Lease) bool { return l.Holder != name })
}

// expireLeases clears lapsed leases on the sweep.
func (s *Server) expireLeases(ctx context.Context) {
	lapsed, err := s.store.ExpireLeases(ctx)
	if err != nil {
		log.Printf("leases: expire: %v", err)
		return
	}
	s.recordLapsed(ctx, lapsed)
}

func (s *Server) recordLapsed(ctx context.Context, lapsed []store.Lease) {
	for _, l := range lapsed {
		s.record(ctx, "lease_expired", "", "", "relay", store.DetailJSON(map[string]any{"resource": l.Resource, "holder": l.Holder}))
	}
}
