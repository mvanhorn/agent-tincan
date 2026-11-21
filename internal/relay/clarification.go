package relay

import (
	"context"
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
	id := r.PathValue("id")
	var req envelope.Request
	if _, guest := guestOf(r.Context()); guest {
		req, err = s.store.AnswerAsGuest(r.Context(), id, name, in.Body)
	} else {
		var check store.AnswerCheck
		var notify string
		if check, notify, err = s.checkAnswer(r.Context(), id, name); err == nil {
			req, err = s.store.AnswerChecked(r.Context(), id, name, in.Body, check)
			req.ApprovalNotify = notify
		}
	}
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "answered_input", req.ID, req.TraceID, name, store.DetailJSON(map[string]any{"answer_bytes": len(in.Body)}))
	s.hub.notify(requestKey(req.ID))
	if req.Status == envelope.StatusHeld {
		s.record(r.Context(), "held", req.ID, req.TraceID, name, store.DetailJSON(map[string]any{"to": req.To, "hop": req.Hop, "chain": req.Chain, "answer": true}))
		s.notifyApproval(r.Context(), req)
		writeJSON(w, http.StatusOK, req)
		return
	}
	s.hub.notify(inboxKey(req.To))
	if s.events != nil {
		s.events.Queued(r.Context(), req)
	}
	writeJSON(w, http.StatusOK, req)
}

// AnswerChecker applies the owner's holds to a teammate's answer to a
// request's question (policy.CheckAnswer): it sets the request's
// contributors and, when the owner's rules hold it, its held status, TTL
// and notify.
type AnswerChecker interface {
	CheckAnswer(ctx context.Context, req *envelope.Request) error
}

// checkAnswer runs the preparer's AnswerChecker on request id as name would
// answer it. A request name did not send, or one not waiting on a question,
// gets no check: the store refuses the answer itself.
func (s *Server) checkAnswer(ctx context.Context, id, name string) (store.AnswerCheck, string, error) {
	ac, ok := s.preparer().(AnswerChecker)
	if !ok {
		return store.AnswerCheck{}, "", nil
	}
	req, status, err := s.store.Request(ctx, id)
	if err != nil || req.From != name || status != envelope.StatusNeedsInput {
		return store.AnswerCheck{}, "", nil
	}
	if err := ac.CheckAnswer(ctx, &req); err != nil {
		return store.AnswerCheck{}, "", err
	}
	return store.AnswerCheck{Contributors: req.Contributors, Held: req.Status == envelope.StatusHeld, HoldTTL: req.HoldTTL}, req.ApprovalNotify, nil
}
