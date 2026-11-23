package relay

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

const DefaultTaskBriefing = "07:00"

func (s *Server) sweepTaskBriefing(ctx context.Context) {
	if s.cfg.TaskBriefing == "off" {
		return
	}
	now := s.cfg.Now()
	local := now.In(s.cfg.TaskBriefingZone)
	if local.Format("15:04") < s.cfg.TaskBriefing {
		return
	}
	if _, err := s.queueTaskBriefing(ctx, now, local.Format(time.DateOnly)); err != nil {
		log.Printf("task briefing: %v", err)
	}
}

func (s *Server) queueTaskBriefing(ctx context.Context, now time.Time, day string) (bool, error) {
	roster, err := s.dir.Agents(ctx)
	if err != nil {
		return false, err
	}
	if len(s.eligibleTaskHosts(roster)) == 0 {
		return false, nil
	}
	return s.store.QueueTaskBriefing(ctx, day, now.In(s.cfg.TaskZone).Format(time.DateOnly))
}

func (s *Server) handleTaskBriefingNow(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	queued, err := s.queueTaskBriefing(r.Context(), s.cfg.Now(), "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if queued {
		s.drainTaskNotices(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]bool{"queued": queued})
}

// taskBriefingMessage renders a snapshot safe for fallback delivery.
func taskBriefingMessage(b store.TaskBriefing, zone string) string {
	var msg strings.Builder
	msg.WriteString("Morning briefing.")
	writeTasks := func(label string, tasks []store.BriefingTask) {
		if len(tasks) == 0 {
			return
		}
		fmt.Fprintf(&msg, " %s: ", label)
		for i, t := range tasks {
			if i > 0 {
				msg.WriteString(", ")
			}
			fmt.Fprintf(&msg, "task #%d (%s", t.ID, t.Status)
			if due := client.FormatTaskDue(envelope.Task{TaskDue: t.Due, DueZone: zone}); due != "" {
				fmt.Fprintf(&msg, ", due %s", due)
			}
			if t.Assignee != "" {
				fmt.Fprintf(&msg, ", with %s", envelope.TaskWho(t.Assignee))
			}
			if t.Blocker != "" && label != "Due today or overdue" {
				fmt.Fprintf(&msg, ", %s", t.Blocker)
			}
			msg.WriteByte(')')
		}
		msg.WriteByte('.')
	}
	writeTasks("Due today or overdue", b.Due)
	var returned, waiting []store.BriefingTask
	for _, task := range b.Waiting {
		if task.Status == envelope.TaskReturned {
			returned = append(returned, task)
		} else {
			waiting = append(waiting, task)
		}
	}
	writeTasks("Returned for review", returned)
	writeTasks("Waiting on input or assignment", waiting)
	writeTasks("Open work to follow up", b.Active)
	writeTasks("Done since the last briefing", b.Done)
	if b.SmallAsksAnswered > 0 {
		fmt.Fprintf(&msg, " Small asks answered since last briefing: %d.", b.SmallAsksAnswered)
	}
	if b.Suggested != 0 {
		fmt.Fprintf(&msg, " Suggested today: task #%d.", b.Suggested)
	}
	separator := " Unconfirmed task news: "
	for _, n := range b.Missed {
		// An outbox snapshot written before briefing supersession can
		// still contain briefing entries.
		if n.Kind == envelope.TaskNoticeBriefing {
			continue
		}
		msg.WriteString(separator)
		separator = ", "
		switch {
		case n.Kind == envelope.TaskNoticeReminder:
			fmt.Fprintf(&msg, "task #%d (reminder)", n.TaskID)
		case n.Status != "":
			fmt.Fprintf(&msg, "task #%d (%s)", n.TaskID, n.Status)
		default:
			fmt.Fprintf(&msg, "task #%d (status update)", n.TaskID)
		}
	}
	if separator == ", " {
		msg.WriteByte('.')
	}
	return msg.String()
}
