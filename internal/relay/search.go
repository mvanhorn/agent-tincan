package relay

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/store"
)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	participant := ""
	if !s.isAdmin(r) {
		participant = s.agent(w, r)
		if participant == "" {
			return
		}
	}
	query := r.URL.Query().Get("q")
	if strings.TrimSpace(query) == "" || len(query) > 4096 {
		writeErr(w, http.StatusBadRequest, errors.New("q must contain search text and be at most 4096 bytes"))
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 50 {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be between 1 and 50"))
			return
		}
		limit = n
	}
	// An agent rooms exclude now finds no mention's text, as room_read
	// refuses it (redactRooms); the store checks membership itself.
	rooms := participant == "" || s.mayReadRooms(r.Context(), participant)
	hits, err := s.store.SearchAs(r.Context(), query, participant, rooms, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, errors.New("search failed"))
		return
	}
	s.record(r.Context(), "search", "", "", participant, store.DetailJSON(map[string]any{"count": len(hits)}))
	writeJSON(w, http.StatusOK, map[string]any{"results": hits})
}
