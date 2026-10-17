package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/phone"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// handlePhone serves the phone page and its files, to admin devices only.
// adminRoutes registers routes without checking anything, so the check is
// here, static files included.
func (s *Server) handlePhone(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	phone.Handler().ServeHTTP(w, r)
}

// NonceTTL is how long a decision nonce from the held listing stays good.
const NonceTTL = 10 * time.Minute

// nonceKeep caps the live nonces per held request, decision and device.
// The page refreshes the held list every few seconds and each listing mints
// new nonces; a tap uses the newest one the page has, so only the last few
// need to work.
const nonceKeep = 8

// errStaleNonce refuses a decision from a browser whose nonce is missing,
// used, expired, or issued for another request, decision or device.
var errStaleNonce = errors.New("this approval button has expired; reload the held list and try again")

// nonceKey is what a decision nonce is bound to.
type nonceKey struct{ id, decision, node string }

// nonceBook holds the decision nonces handed to browsers with the held
// listing (KTD17). Only each nonce's SHA-256 is kept, in memory, so a
// restart voids them all and the page fetches new ones. Nonces are never
// logged or audited.
type nonceBook struct {
	mu     sync.Mutex
	grants map[string]nonceGrant // sha256 hex -> grant
	byKey  map[nonceKey][]string // hashes per key, oldest first
}

type nonceGrant struct {
	key     nonceKey
	expires time.Time
}

func newNonceBook() *nonceBook {
	return &nonceBook{grants: map[string]nonceGrant{}, byKey: map[nonceKey][]string{}}
}

func nonceHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// issue mints a nonce for key, good until now+NonceTTL, and drops expired
// ones and the oldest beyond nonceKeep for key.
func (b *nonceBook) issue(key nonceKey, now time.Time) (string, time.Time) {
	var buf [32]byte
	rand.Read(buf[:])
	raw := base64.RawURLEncoding.EncodeToString(buf[:])
	exp := now.Add(NonceTTL)
	b.mu.Lock()
	defer b.mu.Unlock()
	for h, g := range b.grants {
		if !now.Before(g.expires) {
			b.dropLocked(h)
		}
	}
	h := nonceHash(raw)
	b.grants[h] = nonceGrant{key: key, expires: exp}
	b.byKey[key] = append(b.byKey[key], h)
	for len(b.byKey[key]) > nonceKeep {
		b.dropLocked(b.byKey[key][0])
	}
	return raw, exp
}

// take uses up raw if it was issued for key and has not expired.
func (b *nonceBook) take(raw string, key nonceKey, now time.Time) bool {
	if raw == "" {
		return false
	}
	h := nonceHash(raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	g, ok := b.grants[h]
	if !ok || g.key != key || !now.Before(g.expires) {
		return false
	}
	b.dropLocked(h)
	return true
}

// void drops every nonce for request id: its hold has ended.
func (b *nonceBook) void(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, hs := range b.byKey {
		if key.id == id {
			for _, h := range hs {
				delete(b.grants, h)
			}
			delete(b.byKey, key)
		}
	}
}

func (b *nonceBook) dropLocked(h string) {
	g, ok := b.grants[h]
	if !ok {
		return
	}
	delete(b.grants, h)
	hs := b.byKey[g.key]
	for i, x := range hs {
		if x == h {
			hs = append(hs[:i], hs[i+1:]...)
			break
		}
	}
	if len(hs) == 0 {
		delete(b.byKey, g.key)
	} else {
		b.byKey[g.key] = hs
	}
}

// pageDecision is the relay's answer to an approve or deny from the page:
// the request's state after the call, and whether someone (another device,
// the CLI, an earlier tap) had already decided it.
type pageDecision struct {
	ID             string          `json:"id"`
	Status         envelope.Status `json:"status"`
	AlreadyDecided bool            `json:"already_decided,omitempty"`
}

// callerNode is the tailnet node behind r, "" when WhoIs cannot say.
func (s *Server) callerNode(r *http.Request) identity.Node {
	n, err := s.dir.Node(r.Context(), s.remote(r))
	if err != nil {
		return identity.Node{}
	}
	return n
}

// deviceDetail is the audit detail naming the admin device that decided a
// held request; empty for the local admin socket.
func (s *Server) deviceDetail(r *http.Request) string {
	if isLocalAdmin(r.Context()) {
		return ""
	}
	n := s.callerNode(r)
	if n.Name == "" {
		return ""
	}
	return store.DetailJSON(map[string]any{"device": n.Name})
}

// takeDecisionNonce uses up the nonce a browser sent for deciding id.
func (s *Server) takeDecisionNonce(r *http.Request, id, decision, nonce string) bool {
	n := s.callerNode(r)
	if n.ID == "" {
		return false
	}
	return s.nonces.take(nonce, nonceKey{id: id, decision: decision, node: n.ID}, s.cfg.Now())
}

// writeDecided answers a page decision on id that did not change it: the
// final state when the hold has already ended, else errStaleNonce.
func (s *Server) writeDecided(w http.ResponseWriter, r *http.Request, id string) {
	_, st, err := s.store.Request(r.Context(), id)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if st == envelope.StatusHeld {
		writeErr(w, http.StatusConflict, errStaleNonce)
		return
	}
	writeJSON(w, http.StatusOK, pageDecision{ID: id, Status: st, AlreadyDecided: true})
}
