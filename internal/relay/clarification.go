package relay

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	if s.guestTermOver(w, r) {
		return
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := envelope.ValidateInput(in.Body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	answer := s.store.Answer
	if _, guest := guestOf(r.Context()); guest {
		answer = s.store.AnswerAsGuest
	}
	req, err := answer(r.Context(), r.PathValue("id"), name, in.Body)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "answered_input", req.ID, req.TraceID, name, store.DetailJSON(map[string]any{"answer_bytes": len(in.Body)}))
	s.hub.notify(requestKey(req.ID))
	s.hub.notify(inboxKey(req.To))
	if s.events != nil {
		s.events.Queued(r.Context(), req)
	}
	writeJSON(w, http.StatusOK, req)
}
