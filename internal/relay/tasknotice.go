package relay

// Task notices: the relay tells the owner's task hosts when the task list
// changes in a way the owner cares about. The store leaves one row per such
// change in the transaction that makes it (store.TaskNotice);
// drainTaskNotices turns each row into a notify from the relay for every
// host that should hear, then deletes the row.
//
// A notice names the task, its new status and the agents involved. It
// carries nothing a person or an agent wrote: no title, detail, result,
// comment or key. That is what lets it go out past the owner's approval
// rules, as the held-approval notice does. The host reads the task itself
// through the host view, which applies every rule about what it may read.

import (
	"context"
	"fmt"
	"log"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// taskNoticeBatch is how many waiting changes one drain reads.
const taskNoticeBatch = 100

// taskNoticeTail ends every task notice: what the host does with it, and
// that it is no request.
const taskNoticeTail = " Tell the owner when you next can. This is news from the relay, not a request to act; no reply needed."

// taskNoticeBody is the fixed text of the notice for change n, "" for a
// change that is no news. It is built from the task's id, its status and
// agent names alone.
func taskNoticeBody(n store.TaskNotice) string {
	actor := n.Actor
	if actor == store.TaskRelay {
		actor = "the relay"
	}
	var news string
	switch n.Status {
	case envelope.TaskReturned:
		news = fmt.Sprintf("Task #%d is returned: it came back from %s and waits on the owner.", n.TaskID, actor)
	case envelope.TaskQuestion:
		news = fmt.Sprintf("Task #%d is at question: %s needs input, and %s, who sent the ask, can answer.", n.TaskID, actor, n.Asker)
	case envelope.TaskOpen:
		news = fmt.Sprintf("Task #%d is open again with no assignee: the ask %s sent ended with no result, by %s.", n.TaskID, n.Asker, actor)
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

// taskNoticeHosts picks, from the eligible hosts, those that hear of change
// n: every one but the one that caused the change, which knows, and the one
// that sent the ask, which gets the reply itself. A change the owner made
// from an admin device goes to no one: the owner knows.
func taskNoticeHosts(n store.TaskNotice, eligible []string) []string {
	if n.Actor == envelope.TaskOwner {
		return nil
	}
	var hosts []string
	for _, name := range eligible {
		if name == n.Actor || name == n.Asker {
			continue
		}
		hosts = append(hosts, name)
	}
	return hosts
}

// drainTaskNotices tells the task hosts of every task change the store has
// waiting. It runs after each path that commits such a change and on every
// sweep, so a change whose drain failed or never ran is told on a later
// one.
//
// A row is deleted only after a notice is queued for each host that should
// hear. A failure leaves the row for the next drain, and the store keeps
// which hosts it has settled for the row, so the next drain queues only for
// the rest (store.EnqueueTaskNotice). The same call keeps a host to one
// unread notice per task. One drain runs at a time.
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
	body := taskNoticeBody(n)
	if body == "" {
		return nil
	}
	for _, host := range taskNoticeHosts(n, eligible) {
		// A relay notice, not a forwarded request: it bypasses the approval
		// rules as the held-approval notice does, and carries no task text.
		notice := envelope.Request{From: client.RelaySender, To: host, Kind: envelope.KindNotify, Hop: 1, Chain: []string{client.RelaySender}, Body: body}
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
