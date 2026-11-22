package relay

import (
	"net/http"

	"github.com/mvanhorn/agent-tincan/internal/adminui"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

// adminPage serves the setup console's embedded files.
var adminPage = adminui.Handler()

// handleAdminUI serves the setup console and its files, to admin devices
// only. adminRoutes registers routes without checking anything, so the
// check is here, static files included — the same shape as handlePhone.
func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	adminPage.ServeHTTP(w, r)
}

// handleKinds lists the agent kinds the setup console offers when inviting
// an agent, in recipe order (admin only). The console reads it instead of
// carrying its own copy, so the list never drifts from knownKind.
func (s *Server) handleKinds(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": onboard.Kinds})
}

// peerJSON is one tailnet node for the setup console's device list.
type peerJSON struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Tags   []string `json:"tags,omitempty"`
	User   string   `json:"user,omitempty"`
	Online bool     `json:"online"`
	Self   bool     `json:"self,omitempty"`
}

// handlePeers lists every tailnet node for the setup console's device
// list, so the owner can find machines to invite (admin only). The
// console matches entries against the roster's node ids itself.
func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	peers, err := s.dir.Peers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]peerJSON, 0, len(peers))
	for _, p := range peers {
		out = append(out, peerJSON{ID: p.ID, Name: p.Name, Tags: p.Tags, User: p.User, Online: p.Online, Self: p.Self})
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": out})
}
