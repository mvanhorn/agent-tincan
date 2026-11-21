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

	"github.com/mvanhorn/agent-tincan/internal/envelope"
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

// taskHostGuestAsk is why a task host has its ordinary view while a
// guest's ask is open with it: what it read as a host could reach the guest
// in its reply.
const taskHostGuestAsk = "a guest's ask is open with you, so you have your ordinary view until it ends"

// taskView is how one caller reads tasks: as an admin, as an agent with its
// ordinary view, or as a task host with the host view. Every task an answer
// to an agent carries is read through one, so a task host is sent what the
// host view shows and nothing past it.
//
// The host view shows every task but one whose open ask is held for the
// owner with the host as its target. A task filed as proposed that the
// owner never approved is shown to a host that did not file it as a
// withheld row: id, status, filer, source and creation time (the store's
// HostTask).
type taskView struct {
	// viewer is the agent, "" for an admin, who reads every task.
	viewer string
	// host is set when the caller reads with the host view.
	host bool
	// refused is why a named task host does not have the host view now, ""
	// for a host that has it and for a caller that is no host.
	refused string
	// asked is set when the caller asked for the host view, so the answer
	// says whether it got it.
	asked bool
}

// hostTaskView is the view agent name reads tasks with now: the host view
// when the owner named it a task host, taskHostRefusal has no objection and
// no guest's ask is open with it, and its ordinary view otherwise. This is
// the one place that decides it; whether the caller asked for the host
// view is its caller's to weigh.
func (s *Server) hostTaskView(ctx context.Context, name string) (taskView, error) {
	view := taskView{viewer: name}
	a, ok, err := s.dir.Agent(ctx, name)
	if err != nil || !ok || !a.TaskHost {
		return view, err
	}
	if why := s.taskHostRefusal(a); why != "" {
		view.refused = "you are suspended as a task host: " + why
		return view, nil
	}
	open, err := s.store.OpenGuestAsks(ctx, name)
	if err != nil {
		return view, err
	}
	if open > 0 {
		view.refused = taskHostGuestAsk
		return view, nil
	}
	view.host = true
	return view, nil
}

// askedTaskView is the view for a read by agent name: the host view only
// when r asks for it with host=1 and hostTaskView grants it. A read that
// does not ask is the ordinary view for every caller, so a client that
// knows nothing of task hosts is answered as before. It writes the error
// and returns false when the view cannot be decided.
func (s *Server) askedTaskView(w http.ResponseWriter, r *http.Request, name string) (taskView, bool) {
	if r.URL.Query().Get("host") != "1" {
		return taskView{viewer: name}, true
	}
	view, err := s.hostTaskView(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return taskView{}, false
	}
	view.asked = true
	return view, true
}

// actingTaskView is the view a call that changes a task resolves the task
// through: hostTaskView for an agent, so a task host acts on what the host
// view shows it whole, and the admin's for viewer "".
func (s *Server) actingTaskView(ctx context.Context, viewer string) (taskView, error) {
	if viewer == "" {
		return taskView{}, nil
	}
	return s.hostTaskView(ctx, viewer)
}

// mark says in out, the answer to a read, whether the host view was served
// and, when a named host did not get it, why. It adds nothing to the answer
// to a read that did not ask.
func (v taskView) mark(out map[string]any) {
	if !v.asked {
		return
	}
	out["host"] = v.host
	if v.refused != "" {
		out["host_refused"] = v.refused
	}
}

// task reads task id as the view shows it. For a task host that may be a
// withheld row.
func (v taskView) task(ctx context.Context, st *store.Store, id int64) (envelope.Task, error) {
	if v.host {
		return st.HostTask(ctx, id, v.viewer)
	}
	return st.Task(ctx, id, v.viewer)
}

// whole is task for a caller about to act on the task: a row whose text is
// withheld answers as a task that does not exist, as any task the caller
// may not read does.
func (v taskView) whole(ctx context.Context, st *store.Store, id int64) (envelope.Task, error) {
	t, err := v.task(ctx, st, id)
	if err == nil && t.Withheld {
		return envelope.Task{}, store.ErrNoTask
	}
	return t, err
}

// tasks lists one page of the tasks the view shows, as store.Tasks does.
func (v taskView) tasks(ctx context.Context, st *store.Store, after int64, limit int, closed bool) ([]envelope.Task, error) {
	if v.host {
		return st.HostTasks(ctx, v.viewer, after, limit, closed)
	}
	return st.Tasks(ctx, v.viewer, after, limit, closed)
}

// closed counts the done and dropped tasks the view shows.
func (v taskView) closed(ctx context.Context, st *store.Store) (done, dropped int, err error) {
	if v.host {
		return st.ClosedHostTasks(ctx, v.viewer)
	}
	return st.ClosedTasks(ctx, v.viewer)
}

// hostMayActForOwner guards actions the named host takes from a direct chat.
// Work already open from a guest or a claimed request cannot authorize them.
func (s *Server) hostMayActForOwner(w http.ResponseWriter, r *http.Request, from string) bool {
	ctx := r.Context()
	a, named, err := s.dir.Agent(ctx, from)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	if !named || !a.TaskHost {
		writeErr(w, http.StatusForbidden, errNotTaskHost)
		return false
	}
	if why := s.taskHostRefusal(a); why != "" {
		writeErr(w, http.StatusForbidden, fmt.Errorf("you are suspended as a task host: %s; nothing was changed", why))
		return false
	}
	if open, err := s.store.OpenGuestAsks(ctx, from); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	} else if open > 0 {
		writeErr(w, http.StatusConflict, errTaskDecideDuringGuestAsk)
		return false
	}
	// Any claim in any session, counted, not the one-claim chain a schedule
	// reads.
	if claimed, err := s.store.HeldClaims(ctx, from, 1); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	} else if len(claimed) > 0 {
		writeErr(w, http.StatusConflict, errTaskDecideWhileClaimed)
		return false
	}
	return true
}

func (s *Server) hostAssignTask(w http.ResponseWriter, r *http.Request, view taskView, id int64, to, words string) {
	if to == "" {
		writeErr(w, http.StatusBadRequest, errors.New("assign needs to, the teammate to give the task to"))
		return
	}
	sess, err := session(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	task, err := view.task(ctx, s.store, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	if task.Status == envelope.TaskProposed {
		writeErr(w, http.StatusConflict, errors.New("the owner must approve this proposed task before assignment"))
		return
	}
	if task.Withheld {
		writeTaskErr(w, store.ErrNoTask)
		return
	}

	a := taskAssign{TaskID: id, From: view.viewer, To: to, Actor: envelope.TaskOwner, Session: sess, OwnerHost: true, OwnerWords: words}
	if task.CurrentRequest != "" {
		_, err = s.reassignTask(ctx, a)
	} else {
		_, err = s.assignTask(ctx, a)
	}
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	s.nonces.void(taskNonceID(id))
	s.recordTask(r, "task_assigned", view.viewer, id, map[string]any{"for_owner": true, "to": to, "bytes": len(words)})
	s.writeTaskNow(w, r, view, id, http.StatusOK, nil)
}

func (s *Server) handleHostAddTask(w http.ResponseWriter, r *http.Request, in taskAdd, from, sess string) {
	if !s.hostMayActForOwner(w, r, from) {
		return
	}
	if in.Source != "" {
		writeErr(w, http.StatusBadRequest, errors.New("file work from an outside source as a proposed task without mine"))
		return
	}
	if in.Assignee == "" {
		in.Assignee = envelope.TaskOwner
	}
	ctx := r.Context()
	if err := in.check(ctx, s); err != nil {
		writeTaskErr(w, err)
		return
	}
	if in.EverySeconds != 0 {
		if err := s.checkScheduleCaller(ctx, from, sess); err != nil {
			writeTaskErr(w, err)
			return
		}
	}
	if in.Assignee == envelope.TaskOwner {
		if rp, ok := s.preparer().(RoomPolicy); ok {
			if err := rp.Charge(from); err != nil {
				writeErr(w, statusFor(err), err)
				return
			}
		}
	}
	out, err := s.addTask(ctx, in, envelope.TaskOwner, from, nil, false, func(id int64) error {
		_, err := s.assignTask(ctx, taskAssign{TaskID: id, From: from, To: in.Assignee, Actor: envelope.TaskOwner, Session: sess, OwnerHost: true})
		return err
	})
	s.writeAddedTask(w, r, in, from, out, err)
}
