package relay

import (
	"errors"
	"net/http"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

func (s *Server) handleTaskDue(w http.ResponseWriter, r *http.Request) {
	actor := s.agent(w, r)
	if actor != "" {
		s.setTaskDue(w, r, actor)
	}
}

func (s *Server) handleAdminTaskDue(w http.ResponseWriter, r *http.Request) {
	if s.adminOnly(w, r) {
		s.setTaskDue(w, r, envelope.TaskOwner)
	}
}

func (s *Server) setTaskDue(w http.ResponseWriter, r *http.Request, actor string) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var due envelope.TaskDue
	if !readTaskBody(w, r, &due, false) {
		return
	}
	ctx := r.Context()
	view := taskView{viewer: actor, roomAccess: s.mayReadRooms}
	if actor != envelope.TaskOwner {
		if _, err := view.whole(ctx, s.store, id); err != nil {
			writeTaskErr(w, err)
			return
		}
	}
	if err := due.Validate(s.cfg.TaskZone); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, err := s.store.SetTaskDue(ctx, id, due, actor, "", false)
	if errors.Is(err, store.ErrTaskDueFiler) {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	if actor == envelope.TaskOwner {
		actor = ""
	}
	s.recordTask(r, "task_due", actor, id, nil)
	if t.RoomPostID != 0 && actor != "" {
		s.writeTaskNow(w, r, view, id, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, taskReply{Task: t})
}
