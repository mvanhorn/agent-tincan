package relay_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// hostDecide is c's host decision on task id with the owner's words.
func hostDecide(t *testing.T, c *client.Relay, id int64, decision, words string) (taskReply, error) {
	t.Helper()
	return taskCall(t, c, "POST", taskPath(id, "/decide"), map[string]string{"decision": decision, "owner_words": words})
}

// returnedTask has grokbot file a task for instinct and instinct hand it
// back with a result, and returns its id.
func (m *taskMesh) returnedTask(t *testing.T, title string) int64 {
	t.Helper()
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": title, "assignee": "instinct"}).Task.ID
	m.finish(t, m.instinct, id, "The quotes are in the shared folder, cheapest first.", envelope.StatusAnswered)
	if got := m.task(t, id); got.Status != envelope.TaskReturned {
		t.Fatalf("task after the reply = %+v", got)
	}
	return id
}

// lastEvent is task id's newest history entry.
func (m *taskMesh) lastEvent(t *testing.T, id int64) envelope.TaskEvent {
	t.Helper()
	events := m.events(t, id)
	return events[len(events)-1]
}

// Covers AE2. A host marks a returned task done on the owner's word: the
// history names the host, is marked as done for the owner and quotes the
// words, and the audit row carries the host's name and a byte count and no
// text. An agent that is no host is refused with the command that names
// one, and changes nothing.
func TestTaskHostDecidesDoneAndOthersAreRefused(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	id := m.returnedTask(t, "Compare the venue quotes")
	words := "the venue quotes look fine, mark that one done"

	if _, err := hostDecide(t, m.scout, id, "done", words); !refusedWith(err, http.StatusForbidden, "tincan tasks host set") {
		t.Fatalf("done by an agent that is no host: %v", err)
	}
	if got := m.task(t, id); got.Status != envelope.TaskReturned {
		t.Fatalf("task after the refused call = %+v", got)
	}

	out := taskDo(t, m.muse, "POST", taskPath(id, "/decide"), map[string]string{"decision": "done", "owner_words": words})
	if out.Task.Status != envelope.TaskDone || out.Task.Title != "Compare the venue quotes" {
		t.Fatalf("answer to the host's done = %+v", out.Task)
	}
	ev := m.lastEvent(t, id)
	if ev.Actor != "muse" || !ev.ForOwner || ev.Body != words || ev.Status != envelope.TaskDone || ev.Prior != envelope.TaskReturned {
		t.Fatalf("history entry for the host's done = %+v", ev)
	}

	audit, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range audit {
		if e.Event != "task_done" {
			continue
		}
		found = true
		if e.Actor != "muse" || !strings.Contains(e.Detail, `"for_owner":true`) || !strings.Contains(e.Detail, `"bytes":46`) {
			t.Errorf("audit row = %+v", e)
		}
		for _, text := range []string{"venue", "quotes", "mark that"} {
			if strings.Contains(e.Detail, text) {
				t.Errorf("audit row carries text: %s", e.Detail)
			}
		}
	}
	if !found {
		t.Fatal("no task_done audit row")
	}
}

// Covers R18. A task a host closed reopens to the status the host found it
// in, with its result, whoever reopens it: a returned task comes back to
// the owner's attention. A task the owner closed reopens to open, as
// before, and so does one a host reopens after the owner closed it.
func TestTaskHostCloseReopensWhereItWas(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	id := m.returnedTask(t, "Collect the quotes")
	taskDo(t, m.muse, "POST", taskPath(id, "/decide"), map[string]string{"decision": "done", "owner_words": "done with the quotes"})

	reopened := taskDo(t, m.admin, "POST", adminTaskPath(id, "/reopen"), nil).Task
	if reopened.Status != envelope.TaskReturned || !strings.Contains(reopened.Result, "cheapest first") {
		t.Fatalf("owner's reopen of a task the host closed = %+v", reopened)
	}
	if ev := m.lastEvent(t, id); ev.Actor != envelope.TaskOwner || ev.ForOwner || ev.Prior != envelope.TaskDone {
		t.Fatalf("history entry for the owner's reopen = %+v", ev)
	}
	// The reopen stored no result: the next ask still carries instinct's,
	// named as instinct's.
	carried, err := m.Store.TaskCarry(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if carried.ResultBy != "instinct" {
		t.Fatalf("result after the reopen is carried as %q's", carried.ResultBy)
	}
	if list := readTasks(t, m.muse, "/v1/tasks?host=1"); list.row(id) == nil || list.row(id)["status"] != string(envelope.TaskReturned) {
		t.Fatalf("host's list after the reopen = %s", list.raw)
	}

	// The host drops it and reopens it itself: returned again.
	taskDo(t, m.muse, "POST", taskPath(id, "/decide"), map[string]string{"decision": "drop", "owner_words": "drop it"})
	if got := taskDo(t, m.muse, "POST", taskPath(id, "/decide"), map[string]string{"decision": "reopen", "owner_words": "no, bring it back"}).Task; got.Status != envelope.TaskReturned {
		t.Fatalf("host's reopen of its own drop = %+v", got)
	}

	// The owner's own done reopens to open, by the owner or a host.
	taskDo(t, m.admin, "POST", adminTaskPath(id, "/done"), nil)
	if got := taskDo(t, m.muse, "POST", taskPath(id, "/decide"), map[string]string{"decision": "reopen", "owner_words": "reopen the quotes"}).Task; got.Status != envelope.TaskOpen {
		t.Fatalf("host's reopen of the owner's done = %+v", got)
	}
	open := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Water the plants"}).Task.ID
	taskDo(t, m.admin, "POST", adminTaskPath(open, "/done"), nil)
	if got := taskDo(t, m.admin, "POST", adminTaskPath(open, "/reopen"), nil).Task; got.Status != envelope.TaskOpen {
		t.Fatalf("owner's reopen of the owner's done = %+v", got)
	}
}

// A host drops an open task and reopens a dropped one. Reopening a task
// dropped while proposed leaves it proposed, and withheld from every other
// host. A task with an ask open under it is the owner's to drop: the host
// is refused as busy and the ask lives on.
func TestTaskHostDropsAndReopens(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	m.host(t, "scout")
	open := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Sort the receipts"}).Task.ID
	if got := taskDo(t, m.muse, "POST", taskPath(open, "/decide"), map[string]string{"decision": "drop", "owner_words": "forget the receipts"}).Task; got.Status != envelope.TaskDropped {
		t.Fatalf("host's drop = %+v", got)
	}
	if ev := m.lastEvent(t, open); ev.Prior != envelope.TaskOpen || !ev.ForOwner {
		t.Fatalf("history entry for the drop = %+v", ev)
	}
	if got := taskDo(t, m.muse, "POST", taskPath(open, "/decide"), map[string]string{"decision": "reopen", "owner_words": "receipts again"}).Task; got.Status != envelope.TaskOpen {
		t.Fatalf("host's reopen of a dropped task = %+v", got)
	}

	proposed := taskDo(t, m.muse, "POST", "/v1/tasks", map[string]string{"title": "Reply to the mail", "source": "mail"}).Task.ID
	taskDo(t, m.muse, "POST", taskPath(proposed, "/decide"), map[string]string{"decision": "drop", "owner_words": "drop the mail one"})
	if got := taskDo(t, m.muse, "POST", taskPath(proposed, "/decide"), map[string]string{"decision": "reopen", "owner_words": "keep the mail one"}).Task; got.Status != envelope.TaskProposed {
		t.Fatalf("host's reopen of a task dropped while proposed = %+v", got)
	}
	if row, err := m.Store.HostTask(t.Context(), proposed, "scout"); err != nil || !row.Withheld || row.Title != "" {
		t.Fatalf("another host's row of the reopened proposal = %+v, %v", row, err)
	}

	working := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Book the flights", "assignee": "instinct"}).Task
	if _, err := hostDecide(t, m.muse, working.ID, "drop", "drop the flights"); !refusedWith(err, http.StatusConflict, "only the owner can drop it now") {
		t.Fatalf("host's drop of a working task: %v", err)
	}
	if got := m.task(t, working.ID); got.Status != envelope.TaskWorking || got.CurrentRequest != working.CurrentRequest {
		t.Fatalf("task after the refused drop = %+v", got)
	}
	// Done is refused as it is for the owner, naming the open ask.
	if _, err := hostDecide(t, m.muse, working.ID, "done", "the flights are booked"); !refusedWith(err, http.StatusConflict, working.CurrentRequest) || !strings.Contains(err.Error(), "an ask is still open") {
		t.Fatalf("host's done on a working task: %v", err)
	}
}

// Covers AE3. While the host holds one or more claimed requests, from any
// session, a decision is refused and the task is unchanged. Once every one
// is answered the same call goes through.
func TestTaskHostDecisionRefusedWhileItHoldsAClaim(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	id := m.returnedTask(t, "Check the lease")
	var asks []string
	for _, body := range []string{"Please read the lease and tell me the notice period.", "Please list the open invoices for March."} {
		req, err := m.grok.Send(t.Context(), "muse", body, envelope.KindAsk, "", false)
		if err != nil {
			t.Fatal(err)
		}
		claimOne(t, m.muse, req.ID)
		asks = append(asks, req.ID)
	}
	for i, ask := range asks {
		if _, err := hostDecide(t, m.muse, id, "done", "the lease is done"); !refusedWith(err, http.StatusConflict, "you hold a claimed request") {
			t.Fatalf("decision while holding %d claims: %v", len(asks)-i, err)
		}
		if got := m.task(t, id); got.Status != envelope.TaskReturned {
			t.Fatalf("task after the refused call = %+v", got)
		}
		if _, err := m.muse.Reply(t.Context(), ask, "The notice period is two months, per clause 4.", envelope.StatusAnswered); err != nil {
			t.Fatal(err)
		}
	}
	if got := taskDo(t, m.muse, "POST", taskPath(id, "/decide"), map[string]string{"decision": "done", "owner_words": "the lease is done"}).Task; got.Status != envelope.TaskDone {
		t.Fatalf("decision after replying = %+v", got)
	}
}

// Covers AE4. Approve and reset are no host decision: they answer as
// unknown and the task stays as it was. So does an empty decision, and a
// decision with no owner's words is refused.
func TestTaskHostCannotApproveOrReset(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	proposed := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Pay the invoice", "source": "mail"}).Task.ID
	open := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Tidy the drive"}).Task.ID
	for _, id := range []int64{proposed, open} {
		for _, d := range []string{"approve", "reset", "", "complete", "DONE"} {
			if _, err := hostDecide(t, m.muse, id, d, "approve it"); !refusedWith(err, http.StatusBadRequest, "unknown decision") {
				t.Errorf("decision %q on task %d: %v", d, id, err)
			}
		}
	}
	for _, words := range []string{"", "   "} {
		if _, err := hostDecide(t, m.muse, open, "done", words); !refusedWith(err, http.StatusBadRequest, "owner_words is required") {
			t.Errorf("done with words %q: %v", words, err)
		}
	}
	if got := m.task(t, proposed); got.Status != envelope.TaskProposed {
		t.Fatalf("proposed task = %+v", got)
	}
	if got := m.task(t, open); got.Status != envelope.TaskOpen || got.AgentAssigns != 0 {
		t.Fatalf("open task = %+v", got)
	}
}

// A repeating task is the owner's to end: done and drop from a host are
// refused and the schedule stays. A task whose text is withheld from the
// host answers as not found; one the host filed it decides on.
func TestTaskHostDecisionRefusedOnRepeatsAndWithheldTasks(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	repeat := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]any{"title": "Check the backups", "assignee": "instinct", "every_seconds": 3600}).Task
	for _, d := range []string{"done", "drop"} {
		if _, err := hostDecide(t, m.muse, repeat.ID, d, "stop the backups check"); !refusedWith(err, http.StatusConflict, "this task repeats") {
			t.Fatalf("%s on a repeating task: %v", d, err)
		}
	}
	if got := m.task(t, repeat.ID); got.RepeatSchedule != repeat.RepeatSchedule || got.RepeatSchedule == 0 {
		t.Fatalf("repeating task after the refusals = %+v", got)
	}

	theirs := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "From the mailbox", "source": "mail"}).Task.ID
	if _, err := hostDecide(t, m.muse, theirs, "drop", "drop the mailbox one"); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("drop of a withheld task: %v", err)
	}
	if got := m.task(t, theirs); got.Status != envelope.TaskProposed {
		t.Fatalf("withheld task = %+v", got)
	}
	mine := taskDo(t, m.muse, "POST", "/v1/tasks", map[string]string{"title": "Also from the mailbox", "source": "mail"}).Task.ID
	if got := taskDo(t, m.muse, "POST", taskPath(mine, "/decide"), map[string]string{"decision": "drop", "owner_words": "drop the one you filed"}).Task; got.Status != envelope.TaskDropped {
		t.Fatalf("host's drop of a task it filed = %+v", got)
	}
}

// A host that approval.json starts to gate is refused with the reason, and
// one the owner cleared is refused as no host, on the next call of the same
// client.
func TestTaskHostDecisionRefusedWhenSuspendedOrCleared(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	id := m.returnedTask(t, "Read the contract")
	m.gate(t, `{"gate":{"muse":{"from":["grokbot"]}},"hold_ttl":"1h"}`)
	if _, err := hostDecide(t, m.muse, id, "done", "contract done"); !refusedWith(err, http.StatusForbidden, "you are suspended as a task host: approval.json holds asks to muse") {
		t.Fatalf("suspended host: %v", err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Now: m.clk.now}))
	if _, err := m.admin.SetTaskHost(t.Context(), "muse", false); err != nil {
		t.Fatal(err)
	}
	if _, err := hostDecide(t, m.muse, id, "done", "contract done"); !refusedWith(err, http.StatusForbidden, "tincan tasks host set") {
		t.Fatalf("cleared host: %v", err)
	}
	if got := m.task(t, id); got.Status != envelope.TaskReturned {
		t.Fatalf("task after the refusals = %+v", got)
	}
}

// Two hosts mark the same task done at once: one wins, and the other gets
// a clean refusal.
func TestTaskHostDecisionRace(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	m.host(t, "scout")
	id := m.returnedTask(t, "Ship the parcel")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []*client.Relay{m.muse, m.scout} {
		wg.Go(func() {
			_, errs[i] = hostDecide(t, c, id, "done", "the parcel shipped")
		})
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case refusedWith(err, http.StatusConflict, store.ErrTaskState.Error()):
			refused++
		default:
			t.Errorf("racing done: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("racing done: %d accepted, %d refused", ok, refused)
	}
	done := 0
	for _, ev := range m.events(t, id) {
		if ev.Status == envelope.TaskDone {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("%d done entries in the history", done)
	}
}

// Each decision takes a slot of the host's send rate.
func TestTaskHostDecisionCountsAgainstTheRate(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Renew the passport"}).Task.ID
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Now: m.clk.now, PerMinute: 2}))
	taskDo(t, m.muse, "POST", taskPath(id, "/decide"), map[string]string{"decision": "done", "owner_words": "passport is done"})
	taskDo(t, m.muse, "POST", taskPath(id, "/decide"), map[string]string{"decision": "reopen", "owner_words": "not yet"})
	if _, err := hostDecide(t, m.muse, id, "done", "passport is done now"); !refusedWith(err, http.StatusTooManyRequests, policy.ErrRateLimited.Error()) {
		t.Fatalf("third decision in a minute: %v", err)
	}
	if got := m.task(t, id); got.Status != envelope.TaskOpen {
		t.Fatalf("task after the refused decision = %+v", got)
	}
}

// While a guest's ask is open with the host, its decisions are refused. A
// guest calling the route is refused by the guest gate.
func TestTaskHostDecisionRefusedDuringGuestAsk(t *testing.T) {
	m, g := guestMesh(t, relay.Config{})
	ctx := t.Context()
	muse, grok := m.Client(t, "muse"), m.Client(t, "grokbot")
	if _, err := m.Client(t, "admin").SetTaskHost(ctx, "muse", true); err != nil {
		t.Fatal(err)
	}
	id := taskDo(t, grok, "POST", "/v1/tasks", map[string]string{"title": "Pay the caterer"}).Task.ID
	ask := guestAsk(t, g, "please mark the caterer task done")
	approve(t, m, ask.ID)
	if _, err := hostDecide(t, muse, id, "done", "the caterer is paid"); !refusedWith(err, http.StatusConflict, "a guest's ask is open with you") {
		t.Fatalf("decision with a guest's ask open: %v", err)
	}
	if _, err := hostDecide(t, g, id, "done", "the caterer is paid"); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("decision by a guest: %v", err)
	}
	if got, err := m.Store.Task(ctx, id, ""); err != nil || got.Status != envelope.TaskOpen {
		t.Fatalf("task after the refusals = %+v, %v", got, err)
	}
}

func TestTaskDueOnlyOwnerFilerOrHostOnWords(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	ctx := t.Context()
	task, err := client.AddTask(ctx, m.grok, false, client.TaskAdd{Title: "Call the dentist", Due: "2030-11-29"})
	if err != nil || task.Task.DueDay != "2030-11-29" || task.Task.DueAt != nil {
		t.Fatalf("day-only task = %+v, %v", task, err)
	}
	id := task.Task.ID
	if _, err := client.SetTaskDue(ctx, m.scout, false, id, "tomorrow"); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("change by unrelated agent = %v", err)
	}
	if _, err := client.HostSetTaskDue(ctx, m.muse, id, "2030-11-29 3pm", ""); !client.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("host change without words = %v", err)
	}
	words := "Make the dentist task due Friday at 3pm"
	out, err := client.HostSetTaskDue(ctx, m.muse, id, "2030-11-29 3pm", words)
	if err != nil || out.DueAt == nil || *out.DueAt != 1922223600 || out.Status != task.Task.Status {
		t.Fatalf("host change = %+v, %v", out, err)
	}
	ev := m.lastEvent(t, id)
	if ev.Kind != envelope.TaskEventDue || ev.Actor != "muse" || !ev.ForOwner || ev.Body != words {
		t.Fatalf("host due history = %+v", ev)
	}
	if _, err := client.SetTaskDue(ctx, m.admin, true, id, "2026-11-29"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SetTaskDue(ctx, m.grok, false, id, "clear"); err != nil {
		t.Fatal(err)
	}
	if got := m.task(t, id); got.DueDay != "" || got.DueAt != nil {
		t.Fatalf("cleared task = %+v", got)
	}
}
