package relay

import (
	"errors"
	"net/http"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// workingOnEntry is one session's working-on line and when it was last
// reported.
type workingOnEntry struct {
	text string
	at   time.Time
}

// handleWorkingOn sets (PUT) or removes (DELETE) the calling session's
// working-on line. The agent is the authenticated caller, never a name in
// the body, and the line is filed under that agent and the session header,
// so no caller can write or remove another agent's line. The text passes
// the good-at line's checks, since every joined agent reads it on the
// roster. Lines live in memory only and lapse after SessionGrace without a
// refresh, so a session that closes without a DELETE still drops off.
func (s *Server) handleWorkingOn(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	sess, err := session(r)
	if err == nil && sess == "" {
		err = errors.New("a working-on line needs a session header")
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		delete(s.workingOn[name], sess)
		if len(s.workingOn[name]) == 0 {
			delete(s.workingOn, name)
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	err = decodeOne(r.Body, &in, "working-on object")
	var text string
	if err == nil {
		text, err = identity.CheckRosterLine("the working-on line", in.Text)
	}
	if err == nil && text == "" {
		err = errors.New("the working-on line is empty; DELETE removes it")
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	now := s.cfg.Now()
	s.mu.Lock()
	m := s.workingOn[name]
	if m == nil {
		m = map[string]workingOnEntry{}
		s.workingOn[name] = m
	}
	m[sess] = workingOnEntry{text: text, at: now}
	for k, e := range m {
		if now.Sub(e.at) >= SessionGrace {
			delete(m, k)
		}
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// currentWorkingOn is agent's most recently refreshed working-on line and
// how many of its other sessions have a live one, skipping lines quiet past
// SessionGrace. The caller holds s.mu.
func (s *Server) currentWorkingOn(agent string, now time.Time) (string, int) {
	var best workingOnEntry
	var bestSession string
	live := 0
	for k, e := range s.workingOn[agent] {
		if now.Sub(e.at) >= SessionGrace {
			// A session that went away without a DELETE leaves nothing
			// behind once its line has lapsed.
			delete(s.workingOn[agent], k)
			continue
		}
		live++
		// Ties go to the lower session id so the roster does not flicker
		// between two lines reported in the same instant.
		if live == 1 || e.at.After(best.at) || (e.at.Equal(best.at) && k < bestSession) {
			best, bestSession = e, k
		}
	}
	if live == 0 {
		return "", 0
	}
	return best.text, live - 1
}
