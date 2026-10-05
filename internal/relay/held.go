package relay

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// MaxHeld caps the claims one held header lists.
const MaxHeld = 10

// heldKey is the context key of a request's *heldWriter.
const heldKey ctxKey = 2

// heldWriter adds client.HeldHeader to the response of an authenticated
// agent call: the asks that agent has claimed and not replied to. The
// header is filled when the response is written, after the handler ran, so
// a reply or claim in this same call is already counted. Every response
// shape carries it (lists, 204s, errors) without changing a body, and
// clients that predate it ignore it.
type heldWriter struct {
	http.ResponseWriter
	s     *Server
	ctx   context.Context
	agent string // set by Server.agent once the caller is resolved
	wrote bool
}

func (h *heldWriter) WriteHeader(code int) {
	if !h.wrote {
		h.wrote = true
		h.setHeld()
	}
	h.ResponseWriter.WriteHeader(code)
}

func (h *heldWriter) Write(b []byte) (int, error) {
	if !h.wrote {
		h.WriteHeader(http.StatusOK)
	}
	return h.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer.
func (h *heldWriter) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// setHeld runs one indexed query for the caller's open claims. A failed
// query is logged and leaves the header off.
func (h *heldWriter) setHeld() {
	if h.agent == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 2*time.Second)
	defer cancel()
	held, err := h.s.store.HeldClaims(ctx, h.agent, MaxHeld)
	if err != nil {
		log.Printf("held claims for %s: %v", h.agent, err)
		return
	}
	if len(held) == 0 {
		return
	}
	raw, err := json.Marshal(held)
	if err != nil {
		return
	}
	h.Header().Set(client.HeldHeader, string(raw))
}

// withHeld wraps every agent API response in a heldWriter.
func (s *Server) withHeld(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hw := &heldWriter{ResponseWriter: w, s: s, ctx: r.Context()}
		next.ServeHTTP(hw, r.WithContext(context.WithValue(r.Context(), heldKey, hw)))
	})
}

// markHeld records the resolved caller on the request's heldWriter.
func markHeld(r *http.Request, agent string) {
	if hw, ok := r.Context().Value(heldKey).(*heldWriter); ok {
		hw.agent = agent
	}
}
