package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// DefaultTaskNoticeAckTimeout is the time a host has to post a claimed
// notice and acknowledge delivery before the relay tries again.
const DefaultTaskNoticeAckTimeout = 10 * time.Minute

const taskNoticeAttempts = 3

func (s *Server) handleTaskNoticesTold(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	ctx := r.Context()
	view, err := s.hostTaskView(ctx, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !view.host {
		writeErr(w, http.StatusForbidden, errors.New("only an active task host can acknowledge task notices"))
		return
	}
	var in struct {
		NoticeIDs []string `json:"notice_ids"`
	}
	if err := decodeOne(r.Body, &in, "task notice receipt"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(in.NoticeIDs) == 0 || len(in.NoticeIDs) > client.MaxTaskNoticeReceipts {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("provide 1 to %d notice request IDs", client.MaxTaskNoticeReceipts))
		return
	}
	for _, id := range in.NoticeIDs {
		if id == "" || len(id) > 64 {
			writeErr(w, http.StatusBadRequest, errors.New("invalid task notice request ID"))
			return
		}
	}
	told, err := s.store.TellTaskNotices(ctx, name, in.NoticeIDs)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrWrongState):
			writeErr(w, statusFor(err), err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	for _, d := range told {
		s.record(context.WithoutCancel(ctx), "task_notice_told", d.Request.ID, d.Request.TraceID, name,
			store.DetailJSON(map[string]any{"task": taskNoticeID(d.Request.NoticeFor), "attempt": d.Attempt}))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) retryTaskNotices(ctx context.Context) {
	due, err := s.store.TaskNoticesDue(ctx, s.cfg.TaskNoticeAckTimeout, taskNoticeBatch)
	if err != nil {
		log.Printf("task notice receipts: %v", err)
		return
	}
	for _, d := range due {
		if err := s.retryTaskNotice(ctx, d); err != nil {
			log.Printf("task notice receipt %s: %v", d.Request.ID, err)
		}
	}
}

func (s *Server) retryTaskNotice(ctx context.Context, d store.TaskDelivery) error {
	old := d.Request
	taskID := taskNoticeID(old.NoticeFor)
	next := envelope.Request{From: client.RelaySender, Kind: envelope.KindNotify, Hop: 1, Chain: []string{client.RelaySender}}
	reason := "attempt limit"
	if d.Attempt < taskNoticeAttempts {
		host, found, err := s.dir.Agent(ctx, old.To)
		if err != nil {
			return err
		}
		if found && host.TaskHost && s.taskHostRefusal(host) == "" {
			next.To, next.NoticeKind, next.NoticeFor, next.Body = old.To, store.NoteTask, old.NoticeFor, old.Body
		} else {
			reason = "host no longer eligible"
		}
	}
	if next.To == "" {
		next.To = s.notifyDestination()
		old.From, old.Kind, old.NoticeKind = client.RelaySender, envelope.KindNotify, store.NoteTask
		markTaskNotice(&old)
		next.Body = fmt.Sprintf("Task #%d: %s did not confirm delivery of this task notice after %d attempts. %s",
			taskID, old.To, d.Attempt, client.TaskNoticeOwnerMessage(old))
	}
	var ttl time.Duration
	if next.To != "" {
		ttl = s.requestTTL(ctx, next.To)
	}
	next, advanced, err := s.store.AdvanceTaskNotice(ctx, d, next, ttl)
	if err != nil || !advanced {
		return err
	}
	detail := map[string]any{"task": taskID, "host": old.To, "attempt": d.Attempt}
	if next.NoticeKind == store.NoteTask {
		detail["next_request"] = next.ID
		s.record(ctx, "task_notice_retried", old.ID, old.TraceID, client.RelaySender, store.DetailJSON(detail))
		s.record(ctx, "task_notified", next.ID, next.TraceID, client.RelaySender,
			store.DetailJSON(map[string]any{"task": taskID, "to": next.To, "attempt": d.Attempt + 1, "previous_request": old.ID}))
		s.hub.notify(inboxKey(next.To))
		s.wakeRequeued(ctx, next)
		return nil
	}
	detail["reason"] = reason
	if next.ID != "" {
		detail["fallback_request"], detail["to"] = next.ID, next.To
	}
	s.record(ctx, "task_notice_exhausted", old.ID, old.TraceID, client.RelaySender, store.DetailJSON(detail))
	if next.ID != "" {
		s.record(ctx, "task_notice_fallback", next.ID, next.TraceID, client.RelaySender,
			store.DetailJSON(map[string]any{"task": taskID, "missed_request": old.ID, "to": next.To}))
		s.noticeQueued(ctx, next)
	}
	return nil
}
