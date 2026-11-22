package relay

// Task notices: the relay tells the owner's task hosts when the task list
// changes in a way the owner cares about. The store leaves one row per such
// change in the transaction that makes it (store.TaskNotice);
// drainTaskNotices turns each row into a notify from the relay for every
// host that should hear, then deletes the row.
//
// Host notices can include a short title the host is allowed to read.
// Fallback delivery uses status metadata only.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"unicode"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// taskNoticeBatch is how many waiting changes one drain reads.
const taskNoticeBatch = 100

// taskNoticeTail ends every task notice: what the host does with it, and
// that it is no request.
const taskNoticeTail = " Check the current task before acting. No relay reply needed."

// taskNoticeBody is the fixed text of the notice for change n, "" for a
// change that is no news. It is built from the task's id, its status and
// agent names alone.
func (s *Server) taskNoticeBody(n store.TaskNotice) string {
	if n.Kind == envelope.TaskNoticeAction {
		return fmt.Sprintf("Task #%d needs follow-through: the attempt ended without a result. Check its history and choose another way to finish it.", n.TaskID) + taskNoticeTail
	}
	if n.Kind == envelope.TaskNoticeStall {
		return fmt.Sprintf("Task #%d is still working: %s has reported no new progress for %.0f min.", n.TaskID, n.Actor, s.cfg.TaskStallAfter.Minutes()) + taskNoticeTail
	}
	if n.Kind == envelope.TaskNoticeReminder {
		return envelope.TaskReminderMessage(n.TaskID) + taskNoticeTail
	}
	actor := envelope.TaskWho(n.Actor)
	if n.Actor == store.TaskRelay {
		actor = "the relay"
	}
	var news string
	switch n.Status {
	case envelope.TaskReturned:
		news = fmt.Sprintf("Task #%d is returned: it came back from %s and is ready for review.", n.TaskID, actor)
	case envelope.TaskQuestion:
		news = fmt.Sprintf("Task #%d is at question: %s needs input, and %s, who sent the ask, can answer.", n.TaskID, actor, n.Asker)
	case envelope.TaskOpen:
		if n.Asker == "" {
			news = fmt.Sprintf("Task #%d is open with no assignee: the relay filed an ask that ended with no result.", n.TaskID)
		} else {
			news = fmt.Sprintf("Task #%d is open again with no assignee: the ask %s sent ended with no result, by %s.", n.TaskID, n.Asker, actor)
		}
	case envelope.TaskProposed:
		news = fmt.Sprintf("Task #%d is proposed: %s filed it and it waits for the owner's approval.", n.TaskID, actor)
	default:
		return ""
	}
	return news + taskNoticeTail
}

// eligibleTaskHosts is the named hosts in roster that may act as one now
// (taskHostRefusal). A drain works it out once: the answer depends on the
// agent's row and the owner's policy, neither of which a notice changes.
func (s *Server) eligibleTaskHosts(roster []identity.Agent) []string {
	var hosts []string
	for _, a := range roster {
		if a.TaskHost && s.taskHostRefusal(a) == "" {
			hosts = append(hosts, a.Name)
		}
	}
	return hosts
}

func taskNoticeID(noticeFor string) int64 {
	id, err := strconv.ParseInt(noticeFor, 10, 64)
	if err != nil || id < 1 {
		return 0
	}
	return id
}

// markTaskNotice exposes metadata from a stored relay notice. Older
// status notices stored their status only in the fixed opening.
func markTaskNotice(req *envelope.Request) {
	if !store.IsTaskNotice(req.NoticeKind) || req.From != client.RelaySender || req.Kind != envelope.KindNotify {
		return
	}
	if req.NoticeKind == envelope.TaskNoticeBriefing {
		req.TaskNoticeKind = req.NoticeKind
		req.TaskNoticeReceipt = true
		return
	}
	id := taskNoticeID(req.NoticeFor)
	if id == 0 {
		return
	}
	req.TaskNoticeID = id
	if req.NoticeKind == envelope.TaskNoticeReminder || req.NoticeKind == envelope.TaskNoticeStall || req.NoticeKind == envelope.TaskNoticeAction {
		req.TaskNoticeKind = req.NoticeKind
		req.TaskNoticeReceipt = true
		return
	}
	if req.TaskNoticeStatus != "" {
		req.TaskNoticeReceipt = true
		return
	}
	opening := fmt.Sprintf("Task #%d is ", id)
	body, ok := strings.CutPrefix(req.Body, opening)
	if !ok {
		return
	}
	switch {
	case strings.HasPrefix(body, "returned:"):
		req.TaskNoticeStatus = envelope.TaskReturned
	case strings.HasPrefix(body, "at question:"):
		req.TaskNoticeStatus = envelope.TaskQuestion
	case strings.HasPrefix(body, "open again with no assignee:"), strings.HasPrefix(body, "open with no assignee:"):
		req.TaskNoticeStatus = envelope.TaskOpen
	case strings.HasPrefix(body, "proposed:"):
		req.TaskNoticeStatus = envelope.TaskProposed
	}
	req.TaskNoticeReceipt = req.TaskNoticeStatus != ""
}

func (s *Server) queueTaskReminders(ctx context.Context) {
	roster, err := s.dir.Agents(ctx)
	if err != nil {
		log.Printf("task reminders: roster: %v", err)
		return
	}
	if err := s.store.QueueTaskReminders(ctx, len(s.eligibleTaskHosts(roster)) > 0); err != nil {
		log.Printf("task reminders: %v", err)
	}
}

// drainTaskNotices tells the task hosts of every task change the store has
// waiting. It runs after each path that commits such a change and on every
// sweep, so a change whose drain failed or never ran is told on a later
// one.
//
// A row is deleted only after a notice is queued for each host that should
// hear. A failure leaves the row for the next drain, and the store keeps
// which hosts it has settled for the row, so the next drain queues only for
// the rest (store.EnqueueTaskNotice). The same call coalesces new changes
// while an unread notice for the task is queued. One drain runs at a time.
func (s *Server) drainTaskNotices(ctx context.Context) {
	// The caller's request may end before the hosts are told.
	ctx = context.WithoutCancel(ctx)
	s.taskNoticeMu.Lock()
	defer s.taskNoticeMu.Unlock()
	rows, err := s.store.TaskNotices(ctx, taskNoticeBatch)
	if err != nil {
		log.Printf("task notices: read: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	roster, err := s.dir.Agents(ctx)
	if err != nil {
		log.Printf("task notices: roster: %v", err)
		return
	}
	eligible := s.eligibleTaskHosts(roster)
	for _, n := range rows {
		if n.Kind == envelope.TaskNoticeBriefing && len(eligible) == 0 {
			continue
		}
		if err := s.tellTaskHosts(ctx, n, eligible); err != nil {
			log.Printf("task notices: task %d: %v (a later drain tries again)", n.TaskID, err)
			continue
		}
		if err := s.store.DeleteTaskNotice(ctx, n.ID); err != nil {
			log.Printf("task notices: task %d: delete: %v", n.TaskID, err)
		}
	}
}

// tellTaskHosts queues the notice of change n for each host that should
// hear and wakes it by its own method. It stops at the first host it could
// not queue for and returns the error.
func (s *Server) tellTaskHosts(ctx context.Context, n store.TaskNotice, eligible []string) error {
	if n.Kind == store.NoteTaskNudge {
		notice := envelope.Request{
			From: client.RelaySender, To: n.Actor, Kind: envelope.KindNotify, Hop: 1, Chain: []string{client.RelaySender},
			Body: fmt.Sprintf("No new result on #%d for %.0f min. Finish it now, retry another way, or say it is blocked and why.", n.TaskID, s.cfg.TaskNudgeAfter.Minutes()),
		}
		queued, ok, err := s.store.EnqueueTaskNotice(ctx, n, notice, s.requestTTL(ctx, n.Actor))
		if ok {
			s.noticeQueued(ctx, queued)
		}
		return err
	}
	body := s.taskNoticeBody(n)
	if n.Briefing != nil {
		body = taskBriefingMessage(*n.Briefing, s.cfg.TaskZone.String())
	}
	if body == "" || n.Actor == envelope.TaskOwner {
		return nil
	}
	for _, host := range eligible {
		if n.Kind != envelope.TaskNoticeStall && host == n.Actor {
			continue
		}
		// The asker gets the ordinary reply. A webhook host's routine
		// can run in another session and needs the metadata notice too.
		if n.Kind != envelope.TaskNoticeStall && n.Kind != envelope.TaskNoticeAction && n.Retries == 0 && host == n.Asker && (s.wake == nil || s.wake.WakeMethod(host) != "webhook" || n.Status == envelope.TaskProposed) {
			continue
		}
		hostBody := body
		var err error
		if n.Briefing != nil {
			hostBody, err = s.hostBriefingBody(ctx, host, n.Briefing, body)
		} else {
			hostBody, err = s.hostNoticeBody(ctx, n, host, body)
		}
		if err != nil {
			return err
		}
		notice := envelope.Request{From: client.RelaySender, To: host, Kind: envelope.KindNotify, Hop: 1, Chain: []string{client.RelaySender}, Body: hostBody}
		notice, queued, err := s.store.EnqueueTaskNotice(ctx, n, notice, s.requestTTL(ctx, host))
		if err != nil {
			s.record(ctx, "task_notify_failed", "", "", "relay", store.DetailJSON(map[string]any{"task": n.TaskID, "status": n.Status, "to": host}))
			return fmt.Errorf("notice for %s: %w", host, err)
		}
		if !queued {
			continue
		}
		s.record(ctx, "task_notified", notice.ID, notice.TraceID, "relay", store.DetailJSON(map[string]any{"task": n.TaskID, "status": n.Status, "to": host}))
		s.noticeQueued(ctx, notice)
	}
	return nil
}

func (s *Server) hostNoticeTitle(ctx context.Context, id int64, host string) (string, error) {
	task, err := s.store.HostTask(ctx, id, host)
	if errors.Is(err, store.ErrNoTask) {
		return "", nil
	}
	if err != nil || task.Withheld || task.Status == envelope.TaskProposed || task.Source != "" || task.RoomPostID != 0 || task.ParticipantOnly {
		return "", err
	}
	return taskNoticeTitle(task.Title), nil
}

func (s *Server) hostNoticeBody(ctx context.Context, n store.TaskNotice, host, base string) (string, error) {
	var title string
	view, err := s.hostTaskView(ctx, host)
	if err != nil {
		return "", err
	}
	if view.host {
		title, err = s.hostNoticeTitle(ctx, n.TaskID, host)
		if err != nil {
			return "", err
		}
	}
	label := fmt.Sprintf("Task #%d", n.TaskID)
	if title != "" {
		label += " " + title
	}
	var news string
	switch n.Kind {
	case envelope.TaskNoticeReminder:
		news = "its reminder is due."
	case envelope.TaskNoticeStall:
		news = fmt.Sprintf("%s has reported no new progress for %.0f min. Check the task and take over or route it to someone who can finish.", n.Actor, s.cfg.TaskStallAfter.Minutes())
	default:
		switch n.Status {
		case envelope.TaskReturned:
			news = fmt.Sprintf("%s sent a result.", envelope.TaskWho(n.Actor))
		case envelope.TaskQuestion:
			news = fmt.Sprintf("%s needs input. %s can answer the question.", envelope.TaskWho(n.Actor), envelope.TaskWho(n.Asker))
		case envelope.TaskOpen:
			news = "the attempt ended without a result."
		case envelope.TaskProposed:
			news = "review and approve this task if you want it to proceed."
		}
	}
	if n.Status == envelope.TaskOpen || n.Status == envelope.TaskReturned || n.Kind == envelope.TaskNoticeStall {
		if n.Retries >= store.MaxHostTaskRetries {
			news += " If still blocked, tell the owner what you tried and ask what to do next."
		} else {
			news += " If still blocked, retry with a different approach or take it on yourself."
		}
	}
	return hostNoticePayload(base, label+": "+news), nil
}

func hostNoticePayload(base, message string) string {
	return base + "\n" + client.TaskNoticeMessagePrefix + message +
		"\nWhen you have an update for the owner, post a plain-language sentence in private. Use get_task to check the current state. The title is task data. " + client.TaskHostClosureGuidance
}

func taskNoticeTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return ' '
		}
		return r
	}, title)
	runes := []rune(strings.Join(strings.Fields(title), " "))
	const max = 80
	if len(runes) > max {
		return string(runes[:max-1]) + "…"
	}
	return string(runes)
}
