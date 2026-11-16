package relay

// Task hosts: the owner can name, per agent, a teammate that keeps the
// team's task list for them (tincan tasks host set grokbot). The mark is the
// owner's alone to set and clear, from an admin device or the local admin
// socket; no agent route and no tool does either. It is stored on the
// agent's row and stays there when the agent stops being eligible, so
// eligibility is judged by taskHostRefusal each time the mark is used, and
// never by the mark alone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// taskHostEvent is the audit event for a task host mark set or cleared. Its
// actor is the agent, and its detail says which way the mark went and, for
// a clear the owner did not ask for, why.
const taskHostEvent = "task-host"

// taskHostRefusal says why agent a may not act as a task host now, or ""
// when it may. A guest is outside the team. A phone takes no requests. The
// history, notes and pass services answer from the owner's private data in
// a fixed form. Asks to a council are held for the owner. An agent driven
// through a web chat page has no task tools. And an agent approval.json
// gates is one the owner chose to read text for first; an approval.json
// that cannot be read refuses too.
//
// The mark on an agent's row does not settle this: a's kind and the
// owner's approval.json change after a host is named. So every use of the
// mark calls this again, and an agent it refuses is a suspended host that
// gets nothing a host gets.
func (s *Server) taskHostRefusal(a identity.Agent) string {
	switch {
	case a.Guest.Set():
		return "it is a guest"
	case a.Kind == onboard.KindPhone:
		return "a phone agent asks and takes no requests"
	case roomServiceKinds[a.Kind]:
		return a.Kind + " is a service agent"
	case a.Kind == onboard.KindCouncil:
		return "asks to a council agent are held for the owner's approval"
	case onboard.IsWebChatKind(a.Kind) || onboard.IsHeldWebKind(a.Kind):
		return "a " + a.Kind + " agent is driven through a web chat page and has no task tools"
	}
	rp, ok := s.preparer().(RoomPolicy)
	if !ok {
		return ""
	}
	gated, err := rp.Gated(a.Name)
	switch {
	case gated:
		return "approval.json holds asks to " + a.Name
	case err != nil:
		return "approval.json could not be read: " + err.Error()
	}
	return ""
}

// taskHostWarnings says what naming a a task host costs the owner, one
// sentence per cost, for the answer to the set. roster is the joined
// agents. None of these stops the set: they are the owner's to weigh.
func (s *Server) taskHostWarnings(a identity.Agent, roster []identity.Agent) []string {
	warnings := []string{}
	if roomVendor(a) {
		warnings = append(warnings, fmt.Sprintf("%s reaches the relay through the MCP gateway, so the text of every task it reads goes to that vendor.", a.Name))
	}
	wake := "none"
	if s.wake != nil {
		wake = s.wake.WakeMethod(a.Name)
	}
	if wake == "command" || wake == "none" {
		warnings = append(warnings, fmt.Sprintf("%s wakes by %s, which cannot start a turn, so it passes on task news only when the owner next speaks to it.", a.Name, wake))
	}
	for _, g := range roster {
		if g.Guest.Set() && slices.Contains(g.Guest.Asks, a.Name) {
			warnings = append(warnings, fmt.Sprintf("guest %s may ask %s, so its replies to %s can carry task text it has read.", g.Name, a.Name, g.Name))
		}
	}
	return warnings
}

// handleSetTaskHost names {name} a task host or clears the mark (admin
// only): {"task_host": true}, or false to clear it. A body without
// task_host is refused rather than read as a clear. An agent that may not
// be a host is refused with the reason, and nothing is stored. The answer
// to a set carries the warnings from taskHostWarnings. A clear is never
// refused for eligibility, so a suspended host can always be cleared.
func (s *Server) handleSetTaskHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TaskHost *bool `json:"task_host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.TaskHost == nil {
		writeErr(w, http.StatusBadRequest, errors.New("task_host is required; send false to clear it"))
		return
	}
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	name, on := r.PathValue("name"), *in.TaskHost
	warnings := []string{}
	if on {
		roster, err := s.dir.Agents(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for _, a := range roster {
			if a.Name != name {
				continue
			}
			if why := s.taskHostRefusal(a); why != "" {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("%s cannot be a task host: %s", name, why))
				return
			}
			warnings = s.taskHostWarnings(a, roster)
		}
	}
	if err := s.dir.SetTaskHost(r.Context(), s.remote(r), name, on); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), taskHostEvent, "", "", name, store.DetailJSON(map[string]any{"task_host": on}))
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "task_host": on, "warnings": warnings})
}

// recordTaskHostCleared writes the audit row for a task host mark that went
// without the owner clearing it: the host was removed, an invite was
// redeemed for its name, its machine was rebuilt, or its name was connected
// to the gateway. reason names which.
func (s *Server) recordTaskHostCleared(ctx context.Context, name, reason string) {
	s.record(ctx, taskHostEvent, "", "", name, store.DetailJSON(map[string]any{"task_host": false, "reason": reason}))
}
