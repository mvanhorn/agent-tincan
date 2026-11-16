package relay_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// taskNoticeEnd is how every task notice ends.
const taskNoticeEnd = "Tell the owner when you next can. This is news from the relay, not a request to act; no reply needed."

// sentNotice is one task notice as the relay stored it.
type sentNotice struct {
	ID, From, Kind, Body string
	Status               envelope.Status
}

// noticeWakes records whom the relay announced a queued request for.
type noticeWakes struct {
	mu sync.Mutex
	to []string
}

func (w *noticeWakes) Queued(_ context.Context, req envelope.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.to = append(w.to, req.To)
}

func (w *noticeWakes) count(agent string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, to := range w.to {
		if to == agent {
			n++
		}
	}
	return n
}

// joinHost joins name on machine's machine and names it a task host.
func (m *taskMesh) joinHost(t *testing.T, machine, name string) *client.Relay {
	t.Helper()
	c := m.JoinOnMachineOf(t, machine, name)
	m.clk.advance(2 * time.Second)
	m.host(t, name)
	return c
}

// notices returns every task notice ever queued for agent, oldest first.
func (m *taskMesh) notices(t *testing.T, agent string) []sentNotice {
	t.Helper()
	rows, err := m.Store.DB().Query(`SELECT id, from_agent, kind, body, status FROM requests WHERE to_agent = ? AND notice_kind = 'task' ORDER BY rowid`, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []sentNotice
	for rows.Next() {
		var n sentNotice
		var status string
		if err := rows.Scan(&n.ID, &n.From, &n.Kind, &n.Body, &status); err != nil {
			t.Fatal(err)
		}
		n.Status = envelope.Status(status)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// wantNotices fails unless agent has been queued exactly n task notices.
func (m *taskMesh) wantNotices(t *testing.T, agent string, n int) []sentNotice {
	t.Helper()
	got := m.notices(t, agent)
	if len(got) != n {
		t.Fatalf("%s has %d task notices, want %d: %+v", agent, len(got), n, got)
	}
	return got
}

// wantNotice fails unless agent has been queued exactly one task notice, a
// notify from the relay about task id whose text holds every part and the
// fixed ending. It returns the notice.
func (m *taskMesh) wantNotice(t *testing.T, agent string, id int64, parts ...string) sentNotice {
	t.Helper()
	n := m.wantNotices(t, agent, 1)[0]
	if n.From != "relay" || n.Kind != string(envelope.KindNotify) {
		t.Fatalf("notice for %s = %+v, want a notify from relay", agent, n)
	}
	for _, part := range append([]string{fmt.Sprintf("Task #%d ", id), taskNoticeEnd}, parts...) {
		if !strings.Contains(n.Body, part) {
			t.Fatalf("notice for %s = %q, want it to hold %q", agent, n.Body, part)
		}
	}
	return n
}

// waiting is how many task changes are still in the outbox.
func (m *taskMesh) waiting(t *testing.T) int {
	t.Helper()
	rows, err := m.Store.TaskNotices(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// wantNoTaskText fails when any task notice, or any audit row, holds one of
// texts.
func (m *taskMesh) wantNoTaskText(t *testing.T, texts ...string) {
	t.Helper()
	rows, err := m.Store.DB().Query(`SELECT body FROM requests WHERE notice_kind = 'task'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seen []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, body)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	events, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		seen = append(seen, e.Detail)
	}
	for _, s := range seen {
		for _, text := range texts {
			if strings.Contains(s, text) {
				t.Fatalf("%q carries the task text %q", s, text)
			}
		}
	}
}

// read has c poll, which hands it its queued notices, and returns the task
// notices among what it was handed.
func read(t *testing.T, c *client.Relay) []envelope.Request {
	t.Helper()
	in, err := c.Poll(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []envelope.Request
	for _, req := range in.Requests {
		if req.From == client.RelaySender && strings.Contains(req.Body, taskNoticeEnd) {
			out = append(out, req)
		}
	}
	return out
}

// A task the owner assigned from the owner page comes back: each of two
// hosts is queued its own notice, announced so its wake method runs, with
// the task's id, status and the agent that answered, and none of the
// task's title, detail or result. A teammate that is no host hears nothing.
func TestTaskNoticeWhenOwnerAssignedTaskReturns(t *testing.T) {
	m := newTaskMesh(t)
	wakes := &noticeWakes{}
	m.Server.SetEvents(wakes)
	pilot := m.joinHost(t, "grokbot", "pilot")
	m.joinHost(t, "muse", "keeper")
	phone := m.JoinOnMachineOf(t, "admin", "phone")
	m.clk.advance(2 * time.Second)

	task := taskDo(t, phone, "POST", "/v1/admin/tasks", map[string]string{"title": "Renew the passport", "detail": "Form DS-82, before March", "assignee": "instinct"}).Task
	if task.Status != envelope.TaskWorking || task.Assignee != "instinct" {
		t.Fatalf("the owner's task = %+v", task)
	}
	for _, host := range []string{"pilot", "keeper"} {
		m.wantNotices(t, host, 0) // the owner's own add and assign are no news
	}
	m.finish(t, m.instinct, task.ID, "Posted today, tracking 9400-1", envelope.StatusAnswered)

	for _, host := range []string{"pilot", "keeper"} {
		n := m.wantNotice(t, host, task.ID, "returned", "instinct", "waits on the owner")
		if n.Status != envelope.StatusQueued {
			t.Fatalf("notice for %s is %s, want queued", host, n.Status)
		}
		if got := wakes.count(host); got != 1 {
			t.Fatalf("%s was announced %d queued requests, want 1", host, got)
		}
	}
	for _, other := range []string{"grokbot", "instinct", "muse", "scout", "phone"} {
		m.wantNotices(t, other, 0)
	}
	m.wantNoTaskText(t, "passport", "DS-82", "March", "Posted", "9400")
	if n := m.waiting(t); n != 0 {
		t.Fatalf("%d changes left in the outbox", n)
	}
	if got := read(t, pilot); len(got) != 1 || got[0].Kind != envelope.KindNotify {
		t.Fatalf("pilot's poll handed it %+v, want its one notice", got)
	}
	if got := m.audited(t, "task_notified"); len(got) != 2 {
		t.Fatalf("task_notified audit rows = %v, want one per host", got)
	}
}

// An assignee that needs input makes a question notice, which names the
// agent that can answer: the one that sent the ask.
func TestTaskNoticeForAQuestionNamesTheAsker(t *testing.T) {
	m := newTaskMesh(t)
	m.joinHost(t, "instinct", "pilot")
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Cut the release", "assignee": "muse"}).Task.ID
	m.finish(t, m.muse, id, "Which branch, main or the hotfix one?", envelope.StatusNeedsInput)
	if got := m.task(t, id); got.Status != envelope.TaskQuestion {
		t.Fatalf("task = %+v, want a question", got)
	}
	m.wantNotice(t, "pilot", id, "question", "muse needs input", "grokbot, who sent the ask, can answer")
	m.wantNoTaskText(t, "release", "branch", "hotfix")
}

// A task that falls back to open with no reply makes a notice for the
// release, whichever way its ask ended.
func TestTaskNoticeWhenATaskFallsBackToOpen(t *testing.T) {
	const gateMuse = `{"gate":{"muse":{"from":"*"}},"hold_ttl":"1h"}`
	cases := []struct {
		name string
		gate string
		end  func(t *testing.T, m *taskMesh, ask string)
		by   string
	}{
		{name: "expired", by: "the relay", end: func(t *testing.T, m *taskMesh, ask string) {
			claimOne(t, m.muse, ask)
			m.clk.advance(25 * time.Hour)
			m.Server.Sweep(t.Context())
		}},
		{name: "hold timed out", gate: gateMuse, by: "the relay", end: func(t *testing.T, m *taskMesh, _ string) {
			m.clk.advance(61 * time.Minute)
			m.Server.Sweep(t.Context())
		}},
		{name: "cancelled by its asker", by: "grokbot", end: func(t *testing.T, m *taskMesh, ask string) {
			if err := m.grok.Cancel(t.Context(), ask); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "assignee removed", by: "the relay", end: func(t *testing.T, m *taskMesh, ask string) {
			claimOne(t, m.muse, ask)
			if err := m.admin.Remove(t.Context(), "muse"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "failed", by: "muse", end: func(t *testing.T, m *taskMesh, ask string) {
			claimOne(t, m.muse, ask)
			if _, err := m.muse.Reply(t.Context(), ask, "the venue is closed for good", envelope.StatusFailed); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "declined", by: "muse", end: func(t *testing.T, m *taskMesh, ask string) {
			if _, err := m.muse.Reply(t.Context(), ask, "not mine to book", envelope.StatusDeclined); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTaskMesh(t)
			m.joinHost(t, "instinct", "pilot")
			if tc.gate != "" {
				m.gate(t, tc.gate)
			}
			task := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Book the venue", "assignee": "muse"}).Task
			m.wantNotices(t, "pilot", 0)
			tc.end(t, m, task.CurrentRequest)
			m.wantOpen(t, task.ID, "")
			m.wantNotice(t, "pilot", task.ID, "open again with no assignee", "the ask grokbot sent ended with no result, by "+tc.by+".")
			m.wantNoTaskText(t, "venue", "closed for good", "not mine")
			if n := m.waiting(t); n != 0 {
				t.Fatalf("%d changes left in the outbox", n)
			}
		})
	}
}

// The owner's own changes from an admin device are no news for a host: a
// denial of a held task ask, and a reassign that takes a task from its
// assignee.
func TestTaskNoticeNotSentForTheOwnersChanges(t *testing.T) {
	t.Run("denial of a held ask", func(t *testing.T) {
		m := newTaskMesh(t)
		m.joinHost(t, "instinct", "pilot")
		m.gate(t, `{"gate":{"muse":{"from":"*"}},"hold_ttl":"1h"}`)
		task := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Summarise the contract", "assignee": "muse"}).Task
		if task.AskStatus != envelope.StatusHeld {
			t.Fatalf("task = %+v, want a held ask", task)
		}
		if err := m.admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+task.CurrentRequest+"/deny", map[string]string{"reason": "not now"}, nil); err != nil {
			t.Fatal(err)
		}
		m.wantOpen(t, task.ID, "denied: not now")
		m.wantNotices(t, "pilot", 0)
		if n := m.waiting(t); n != 0 {
			t.Fatalf("%d changes left in the outbox, so a later drain could still tell a host", n)
		}
	})
	t.Run("reassign", func(t *testing.T) {
		m := newTaskMesh(t)
		m.joinHost(t, "instinct", "pilot")
		phone := m.JoinOnMachineOf(t, "admin", "phone")
		m.clk.advance(2 * time.Second)
		id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Book the venue", "assignee": "muse"}).Task.ID
		first := m.task(t, id).CurrentRequest
		got := taskDo(t, phone, "POST", adminTaskPath(id, "/assign"), map[string]string{"to": "instinct"}).Task
		if got.Assignee != "instinct" || got.CurrentRequest == first {
			t.Fatalf("task after the owner's reassign = %+v", got)
		}
		if _, st, err := m.Store.Request(t.Context(), first); err != nil || st != envelope.StatusCancelled {
			t.Fatalf("the ask the owner took the task from is %s, %v", st, err)
		}
		m.wantNotices(t, "pilot", 0)
		if n := m.waiting(t); n != 0 {
			t.Fatalf("%d changes left in the outbox", n)
		}
	})
}

// A request the sweep closes with its held acknowledgement returns the
// task, though no reply handler ran, and the host hears of it.
func TestTaskNoticeWhenTheSweepClosesAHeldAcknowledgement(t *testing.T) {
	m := newTaskMesh(t)
	m.joinHost(t, "instinct", "pilot")
	task := m.add(t, envelope.Task{Filer: "grokbot", Title: "write the report"})
	req := m.assign(t, task.ID, "grokbot", "muse")
	if _, err := m.muse.Claim(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	if rep, err := m.muse.Reply(t.Context(), req.ID, "Working on it, full report will follow.", envelope.StatusAnswered); err != nil || !rep.KeptOpen {
		t.Fatalf("acknowledgement = %+v, %v", rep, err)
	}
	m.wantNotices(t, "pilot", 0)
	m.clk.advance(relay.DefaultClaimLease + time.Minute)
	m.Server.Sweep(t.Context())
	if got := m.task(t, task.ID); got.Status != envelope.TaskReturned {
		t.Fatalf("task after the claim lapsed = %+v", got)
	}
	m.wantNotice(t, "pilot", task.ID, "returned", "came back from muse")
	m.wantNoTaskText(t, "report", "Working on it")
}

// A task filed with a source waits for the owner as proposed. The hosts
// hear of it, but the host that filed it does not.
func TestTaskNoticeForAProposedTaskSkipsItsFiler(t *testing.T) {
	m := newTaskMesh(t)
	pilot := m.joinHost(t, "instinct", "pilot")
	m.joinHost(t, "muse", "keeper")
	task := taskDo(t, pilot, "POST", "/v1/tasks", map[string]string{"title": "Reply to the vendor", "source": "mail", "assignee": "scout"}).Task
	if task.Status != envelope.TaskProposed {
		t.Fatalf("task with a source = %+v", task)
	}
	m.wantNotice(t, "keeper", task.ID, "proposed", "pilot filed it", "waits for the owner's approval")
	m.wantNotices(t, "pilot", 0)
	m.wantNoTaskText(t, "vendor")
	// The owner's approval is the owner's own change.
	taskDo(t, m.admin, "POST", adminTaskPath(task.ID, "/approve"), nil)
	m.wantNotices(t, "keeper", 1)
	m.wantNotices(t, "pilot", 0)
}

// A host that sent the ask gets the reply as its asker and no notice; a
// host that caused a change gets none either. Another host hears of both.
func TestTaskNoticeSkipsTheAskerAndTheActor(t *testing.T) {
	m := newTaskMesh(t)
	pilot := m.joinHost(t, "instinct", "pilot")
	keeper := m.joinHost(t, "grokbot", "keeper")
	id := taskDo(t, pilot, "POST", "/v1/tasks", map[string]string{"title": "Book the venue", "assignee": "muse"}).Task.ID
	ask := m.task(t, id).CurrentRequest
	m.finish(t, m.muse, id, "Booked for the 4th.", envelope.StatusAnswered)

	m.wantNotices(t, "pilot", 0)
	in, err := pilot.Poll(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Replies) != 1 || in.Replies[0].Request.ID != ask || in.Replies[0].Reply == nil || in.Replies[0].Reply.Body != "Booked for the 4th." {
		t.Fatalf("pilot's poll = %+v, want the reply to its ask", in)
	}
	m.wantNotice(t, "keeper", id, "returned", "muse")
	if got := read(t, keeper); len(got) != 1 {
		t.Fatalf("keeper read %d notices, want 1", len(got))
	}

	// keeper, a host, assigns the task on and then cancels its own ask: it
	// caused the release and is not told. pilot is.
	next := taskDo(t, keeper, "POST", taskPath(id, "/assign"), map[string]string{"to": "scout"}).Task.CurrentRequest
	if err := keeper.Cancel(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	m.wantOpen(t, id, "cancelled the ask")
	m.wantNotices(t, "keeper", 1)
	m.wantNotice(t, "pilot", id, "open again", "the ask keeper sent ended with no result, by keeper.")
}

// While a host has an unread notice about a task, later changes to that
// task queue it no more and keep nothing back for later. Once it has read
// the notice, the next change queues another.
func TestTaskNoticeOneUnreadPerTaskAndHost(t *testing.T) {
	m := newTaskMesh(t)
	pilot := m.joinHost(t, "instinct", "pilot")
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Book the venue", "assignee": "muse"}).Task.ID
	m.finish(t, m.muse, id, "Booked for the 4th.", envelope.StatusAnswered)
	first := m.wantNotice(t, "pilot", id, "returned")

	// Second change, first notice unread: the task goes out again and fails.
	taskDo(t, m.grok, "POST", taskPath(id, "/assign"), map[string]string{"to": "scout"})
	m.finish(t, m.scout, id, "the venue cancelled on us", envelope.StatusFailed)
	m.wantOpen(t, id, "failed")
	if got := m.wantNotices(t, "pilot", 1); got[0].ID != first.ID || got[0].Status != envelope.StatusQueued {
		t.Fatalf("pilot's notices after a second change = %+v, want the first alone, unread", got)
	}
	if n := m.waiting(t); n != 0 {
		t.Fatalf("%d changes kept back in the outbox", n)
	}
	// Another task's change is told while this one's notice is unread.
	other := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Order the cake", "assignee": "muse"}).Task.ID
	m.finish(t, m.muse, other, "Ordered.", envelope.StatusAnswered)
	m.wantNotices(t, "pilot", 2)

	if got := read(t, pilot); len(got) != 2 {
		t.Fatalf("pilot read %d notices, want 2", len(got))
	}
	// Nothing was kept back: a sweep after the read queues nothing.
	m.Server.Sweep(t.Context())
	m.wantNotices(t, "pilot", 2)

	// Third change to the first task, the first notice read.
	taskDo(t, m.grok, "POST", taskPath(id, "/assign"), map[string]string{"to": "muse"})
	m.finish(t, m.muse, id, "Booked the other hall.", envelope.StatusAnswered)
	got := m.wantNotices(t, "pilot", 3)
	if last := got[2]; last.Status != envelope.StatusQueued || !strings.Contains(last.Body, fmt.Sprintf("Task #%d is returned", id)) {
		t.Fatalf("notice after the read = %+v, want a new one for the return", last)
	}
	m.wantNoTaskText(t, "venue", "Booked", "cake", "hall")
}

// A host the owner's approval rules now gate is suspended and hears
// nothing, and nothing is kept for when it is a host again. A teammate
// that was never named hears nothing.
func TestTaskNoticeNotSentToSuspendedHostOrNonHost(t *testing.T) {
	m := newTaskMesh(t)
	m.joinHost(t, "instinct", "pilot")
	m.joinHost(t, "muse", "keeper")
	m.gate(t, `{"gate":{"keeper":{"from":["scout"]}},"hold_ttl":"1h"}`)
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Book the venue", "assignee": "muse"}).Task.ID
	m.finish(t, m.muse, id, "Booked for the 4th.", envelope.StatusAnswered)
	m.wantNotice(t, "pilot", id, "returned")
	for _, other := range []string{"keeper", "grokbot", "instinct", "muse", "scout"} {
		m.wantNotices(t, other, 0)
	}
	// The gate goes: keeper is a host again and is told of later changes
	// only.
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Now: m.clk.now}))
	m.Server.Sweep(t.Context())
	m.wantNotices(t, "keeper", 0)
	taskDo(t, m.grok, "POST", taskPath(id, "/assign"), map[string]string{"to": "scout"})
	m.finish(t, m.scout, id, "no hall is free", envelope.StatusFailed)
	m.wantNotice(t, "keeper", id, "open again")
}

// A drain that queues one host's notice and then fails for the next leaves
// the change in the outbox. The next drain queues the notice it could not,
// and none again for the host that already has one, though that host has
// read it by then.
func TestTaskNoticeDrainRetryDoesNotTellAHostTwice(t *testing.T) {
	m := newTaskMesh(t)
	clients := map[string]*client.Relay{"pilot": m.joinHost(t, "instinct", "pilot"), "keeper": m.joinHost(t, "muse", "keeper")}
	// The hosts in the order the relay tells them.
	var order []string
	roster, err := m.Dir.Agents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range roster {
		if a.TaskHost {
			order = append(order, a.Name)
		}
	}
	if len(order) != 2 {
		t.Fatalf("hosts = %v", order)
	}
	told, failed := order[0], order[1]
	// The store refuses the second host's notice.
	if _, err := m.Store.DB().Exec(`CREATE TRIGGER refuse_notice BEFORE INSERT ON requests WHEN NEW.notice_kind = 'task' AND NEW.to_agent = '` + failed + `'
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Book the venue", "assignee": "scout"}).Task.ID
	m.finish(t, m.scout, id, "Booked for the 4th.", envelope.StatusAnswered)
	first := m.wantNotice(t, told, id, "returned")
	m.wantNotices(t, failed, 0)
	if n := m.waiting(t); n != 1 {
		t.Fatalf("%d changes in the outbox after a failed drain, want the one it could not finish", n)
	}
	if got := m.audited(t, "task_notify_failed"); len(got) != 1 || !strings.Contains(got[0], failed) {
		t.Fatalf("task_notify_failed audit rows = %v", got)
	}
	// A drain that fails again changes nothing.
	m.Server.Sweep(t.Context())
	m.wantNotices(t, told, 1)
	m.wantNotices(t, failed, 0)

	if got := read(t, clients[told]); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("%s read %+v, want its notice", told, got)
	}
	if _, err := m.Store.DB().Exec(`DROP TRIGGER refuse_notice`); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(t.Context())
	m.wantNotice(t, failed, id, "returned")
	if got := m.wantNotices(t, told, 1); got[0].ID != first.ID {
		t.Fatalf("%s's notices after the retry = %+v, want only the first", told, got)
	}
	if n := m.waiting(t); n != 0 {
		t.Fatalf("%d changes left in the outbox after the retry", n)
	}
	m.Server.Sweep(t.Context())
	m.wantNotices(t, told, 1)
	m.wantNotices(t, failed, 1)
}
