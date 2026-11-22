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
