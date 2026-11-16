package relay_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// hostRead is a task listing or a task read decoded loosely, so a test sees
// every field the relay sent, with the answer's own JSON beside it.
type hostRead struct {
	Host        bool             `json:"host"`
	HostRefused string           `json:"host_refused"`
	Tasks       []map[string]any `json:"tasks"`
	Task        map[string]any   `json:"task"`
	Events      []map[string]any `json:"events"`
	Closed      *struct {
		Done    int `json:"done"`
		Dropped int `json:"dropped"`
	} `json:"closed"`
	raw string
}

// readTasks returns the answer to GET path as c gets it.
func readTasks(t *testing.T, c *client.Relay, path string) hostRead {
	t.Helper()
	var raw json.RawMessage
	if err := c.Raw(t.Context(), "GET", path, nil, &raw); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	out := hostRead{raw: string(raw)}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET %s: %v in %s", path, err, raw)
	}
	return out
}

// row is the listed task with id, nil when the listing has none.
func (h hostRead) row(id int64) map[string]any {
	for _, task := range h.Tasks {
		if task["id"] == float64(id) {
			return task
		}
	}
	return nil
}

// withheldFields are the only fields a withheld row may carry.
var withheldFields = []string{"id", "title", "status", "filer", "source", "created_at", "withheld"}

// wantWithheld fails unless row is the withheld row of task id: the allowed
// fields and no other, with no title.
func wantWithheld(t *testing.T, row map[string]any, id int64, status envelope.TaskStatus, filer, source string) {
	t.Helper()
	if row == nil {
		t.Fatalf("task %d is not in the answer", id)
	}
	for field := range row {
		if !slices.Contains(withheldFields, field) {
			t.Errorf("a withheld row carries %s = %v", field, row[field])
		}
	}
	if row["id"] != float64(id) || row["withheld"] != true || row["title"] != "" || row["status"] != string(status) || row["filer"] != filer || row["source"] != source || row["created_at"] == nil {
		t.Fatalf("withheld row = %v, want task %d %s filed by %s from %s with no title", row, id, status, filer, source)
	}
}

// host names agent a task host from the admin device.
func (m *taskMesh) host(t *testing.T, agent string) {
	t.Helper()
	if _, err := m.admin.SetTaskHost(t.Context(), agent, true); err != nil {
		t.Fatalf("set %s as task host: %v", agent, err)
	}
}

// A host that asks for the host view reads a task one teammate filed for
// another, in the list and whole. Asking is what gets it: without the
// parameter the host has its ordinary view, and an agent that is no host
// has its ordinary view with the parameter, in an answer that says so.
func TestTaskHostViewShowsEveryTaskWhenAsked(t *testing.T) {
	m := newTaskMesh(t)
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Book the venue", "detail": "for forty people", "assignee": "instinct"}).Task.ID
	m.host(t, "muse")

	list := readTasks(t, m.muse, "/v1/tasks?host=1")
	if row := list.row(id); !list.Host || list.HostRefused != "" || row == nil || row["title"] != "Book the venue" || row["withheld"] != nil {
		t.Fatalf("host's list = %s", list.raw)
	}
	one := readTasks(t, m.muse, taskPath(id, "")+"?host=1")
	if !one.Host || one.Task["title"] != "Book the venue" || one.Task["detail"] != "for forty people" || len(one.Events) == 0 {
		t.Fatalf("host's read of the task = %s", one.raw)
	}

	// The same host without the parameter: the ordinary view, and an answer
	// with no word about hosts.
	plain := readTasks(t, m.muse, "/v1/tasks")
	if plain.row(id) != nil || strings.Contains(plain.raw, `"host`) {
		t.Fatalf("host's list without the parameter = %s", plain.raw)
	}
	if _, err := taskCall(t, m.muse, "GET", taskPath(id, ""), nil); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("host's read without the parameter: %v", err)
	}

	// An agent that is no host sends the parameter and gets its ordinary
	// view.
	other := readTasks(t, m.scout, "/v1/tasks?host=1")
	if other.Host || other.row(id) != nil || strings.Contains(other.raw, "Book the venue") {
		t.Fatalf("list of an agent that is no host = %s", other.raw)
	}
	if _, err := taskCall(t, m.scout, "GET", taskPath(id, "")+"?host=1", nil); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("read by an agent that is no host: %v", err)
	}
	// What it may read anyway it reads as before, in an answer that says
	// the host view was not served.
	pool := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Anyone: water the plants"}).Task.ID
	if got := readTasks(t, m.scout, taskPath(pool, "")+"?host=1"); got.Host || got.Task["title"] != "Anyone: water the plants" {
		t.Fatalf("pool task read by an agent that is no host = %s", got.raw)
	}
}

// A task filed as proposed that the owner never approved is shown to a host
// as a row with its id, status, filer, source and age and nothing else:
// open, dropped, in the list and read alone, where its history is left out
// too. The host that filed it reads it whole. Once the owner approves it a
// host reads its text, and still does after it is dropped.
func TestTaskHostViewWithholdsNeverApprovedTask(t *testing.T) {
	m := newTaskMesh(t)
	secrets := []string{"Wire the deposit", "account 4471", "vendor-mail-7", "instinct", "the mail says it is urgent", "not ours after all"}
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{
		"title": "Wire the deposit to the vendor", "detail": "account 4471", "source": "mail", "key": "vendor-mail-7", "assignee": "instinct",
	}).Task.ID
	taskDo(t, m.grok, "POST", taskPath(id, "/comment"), map[string]string{"body": "the mail says it is urgent"})
	m.host(t, "muse")
	m.host(t, "grokbot")

	clean := func(what string, got hostRead) {
		t.Helper()
		if !got.Host {
			t.Fatalf("%s is not the host view: %s", what, got.raw)
		}
		for _, secret := range secrets {
			if strings.Contains(got.raw, secret) {
				t.Errorf("%s carries %q: %s", what, secret, got.raw)
			}
		}
	}
	check := func(status envelope.TaskStatus) {
		t.Helper()
		list := readTasks(t, m.muse, "/v1/tasks?closed=1&host=1")
		clean("the list", list)
		wantWithheld(t, list.row(id), id, status, "grokbot", "mail")
		one := readTasks(t, m.muse, taskPath(id, "")+"?host=1")
		clean("the read", one)
		wantWithheld(t, one.Task, id, status, "grokbot", "mail")
		if len(one.Events) != 0 {
			t.Fatalf("a withheld task comes with history: %s", one.raw)
		}
		// Later pages of its history are withheld as the first is.
		clean("a later page", readTasks(t, m.muse, taskPath(id, "")+"?host=1&after=1"))
	}
	check(envelope.TaskProposed)

	taskDo(t, m.grok, "POST", taskPath(id, "/drop"), map[string]string{"note": "not ours after all"})
	check(envelope.TaskDropped)
	// Without the closed ones it is counted and not listed.
	if open := readTasks(t, m.muse, "/v1/tasks?host=1"); open.row(id) != nil || open.Closed == nil || open.Closed.Dropped != 1 {
		t.Fatalf("host's list without closed = %s", open.raw)
	}

	// The host that filed it reads its own text.
	mine := readTasks(t, m.grok, "/v1/tasks?closed=1&host=1")
	if row := mine.row(id); !mine.Host || row == nil || row["title"] != "Wire the deposit to the vendor" || row["withheld"] != nil || row["key"] != "vendor-mail-7" || row["suggested_assignee"] != "instinct" {
		t.Fatalf("the filing host's list = %s", mine.raw)
	}
	if own := readTasks(t, m.grok, taskPath(id, "")+"?host=1"); own.Task["detail"] != "account 4471" || own.Task["withheld"] != nil || len(own.Events) != 3 {
		t.Fatalf("the filing host's read = %s", own.raw)
	}

	// The owner's reopen is no approval: still withheld.
	taskDo(t, m.admin, "POST", adminTaskPath(id, "/reopen"), nil)
	check(envelope.TaskProposed)

	// Approved, a host reads it, and keeps reading it once it is dropped.
	taskDo(t, m.admin, "POST", adminTaskPath(id, "/approve"), nil)
	shown := func(status envelope.TaskStatus) {
		t.Helper()
		list := readTasks(t, m.muse, "/v1/tasks?closed=1&host=1")
		if row := list.row(id); row == nil || row["title"] != "Wire the deposit to the vendor" || row["detail"] != "account 4471" || row["withheld"] != nil || row["status"] != string(status) {
			t.Fatalf("host's list of the approved task, %s = %s", status, list.raw)
		}
		if one := readTasks(t, m.muse, taskPath(id, "")+"?host=1"); one.Task["title"] != "Wire the deposit to the vendor" || len(one.Events) == 0 || !strings.Contains(one.raw, "the mail says it is urgent") {
			t.Fatalf("host's read of the approved task, %s = %s", status, one.raw)
		}
	}
	shown(envelope.TaskOpen)
	taskDo(t, m.admin, "POST", adminTaskPath(id, "/drop"), nil)
	shown(envelope.TaskDropped)
}

// A host that approval.json starts to gate is suspended: it gets its
// ordinary view, and the answer says why. And a task whose open ask is held
// for the owner with the host as its target stays out of the host view
// until the owner approves the ask.
func TestTaskHostViewSuspendedHostAndHeldAsk(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	other := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Renew the domain", "assignee": "instinct"}).Task.ID
	if got := readTasks(t, m.muse, "/v1/tasks?host=1"); !got.Host || got.row(other) == nil {
		t.Fatalf("host's list before the gate = %s", got.raw)
	}

	m.gate(t, `{"gate":{"muse":{"from":["grokbot"]}},"hold_ttl":"1h"}`)
	suspended := readTasks(t, m.muse, "/v1/tasks?host=1")
	if suspended.Host || suspended.row(other) != nil || !strings.Contains(suspended.HostRefused, "approval.json holds asks to muse") {
		t.Fatalf("suspended host's list = %s", suspended.raw)
	}
	if _, err := taskCall(t, m.muse, "GET", taskPath(other, "")+"?host=1", nil); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("suspended host's read: %v", err)
	}
	for _, action := range []struct {
		path string
		in   map[string]string
	}{{"/comment", map[string]string{"body": "any news?"}}, {"/assign", map[string]string{"to": "scout"}}} {
		if _, err := taskCall(t, m.muse, "POST", taskPath(other, action.path), action.in); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
			t.Fatalf("suspended host's %s: %v", action.path, err)
		}
	}

	held := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Summarise the contract", "assignee": "muse"}).Task
	if held.AskStatus != envelope.StatusHeld {
		t.Fatalf("task for the gated host = %+v, want a held ask", held)
	}
	// The gate goes and muse is a host again, with the ask still held.
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Now: m.clk.now}))
	list := readTasks(t, m.muse, "/v1/tasks?host=1")
	if !list.Host || list.row(other) == nil || list.row(held.ID) != nil || strings.Contains(list.raw, "Summarise the contract") {
		t.Fatalf("host's list while its ask is held = %s", list.raw)
	}
	if _, err := taskCall(t, m.muse, "GET", taskPath(held.ID, "")+"?host=1", nil); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("host's read of the task whose ask is held for it: %v", err)
	}
	if _, err := taskCall(t, m.muse, "POST", taskPath(held.ID, "/comment"), map[string]string{"body": "what is it?"}); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("host's comment on the task whose ask is held for it: %v", err)
	}

	if err := m.admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+held.CurrentRequest+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	if list := readTasks(t, m.muse, "/v1/tasks?host=1"); !list.Host || list.row(held.ID) == nil || list.row(held.ID)["title"] != "Summarise the contract" {
		t.Fatalf("host's list after the owner approved the ask = %s", list.raw)
	}
}

// While a guest's ask is open with a host, the host has its ordinary view
// and the answer says why. It ends with the guest's ask.
func TestTaskHostViewOffWhileGuestAskIsOpen(t *testing.T) {
	m, g := guestMesh(t, relay.Config{})
	ctx := t.Context()
	muse, grok := m.Client(t, "muse"), m.Client(t, "grokbot")
	if _, err := m.Client(t, "admin").SetTaskHost(ctx, "muse", true); err != nil {
		t.Fatal(err)
	}
	id := taskDo(t, grok, "POST", "/v1/tasks", map[string]string{"title": "Pay the caterer", "assignee": "instinct"}).Task.ID
	if got := readTasks(t, muse, "/v1/tasks?host=1"); !got.Host || got.row(id) == nil {
		t.Fatalf("host's list before the guest asked = %s", got.raw)
	}

	ask := guestAsk(t, g, "what is on the task list?")
	approve(t, m, ask.ID)
	during := readTasks(t, muse, "/v1/tasks?host=1")
	if during.Host || during.row(id) != nil || strings.Contains(during.raw, "Pay the caterer") || !strings.Contains(during.HostRefused, "a guest's ask is open with you") {
		t.Fatalf("host's list with a guest's ask open = %s", during.raw)
	}
	if _, err := taskCall(t, muse, "GET", taskPath(id, "")+"?host=1", nil); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("host's read with a guest's ask open: %v", err)
	}
	if _, err := taskCall(t, muse, "POST", taskPath(id, "/assign"), map[string]string{"to": "grokbot"}); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("host's assign with a guest's ask open: %v", err)
	}

	claimOne(t, muse, ask.ID)
	if _, err := muse.Reply(ctx, ask.ID, "ask the owner", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if got := readTasks(t, muse, "/v1/tasks?host=1"); !got.Host || got.HostRefused != "" || got.row(id) == nil {
		t.Fatalf("host's list after the guest's ask ended = %s", got.raw)
	}
}

// A host comments on and assigns a returned task it took no part in, as
// itself, and the task echoed back is the one it reads. The same calls on a
// task whose text is withheld from it answer as for a task that does not
// exist, and change nothing.
func TestTaskHostAssignsAndCommentsThroughTheHostView(t *testing.T) {
	m := newTaskMesh(t)
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Draft the invitation", "assignee": "instinct"}).Task.ID
	m.finish(t, m.instinct, id, "A first draft is attached.", envelope.StatusAnswered)
	comment, assign := taskPath(id, "/comment"), taskPath(id, "/assign")

	// Before it is a host, the task is not muse's to act on.
	if _, err := taskCall(t, m.muse, "POST", comment, map[string]string{"body": "shorter, please"}); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("comment before being a host: %v", err)
	}
	if _, err := taskCall(t, m.muse, "POST", assign, map[string]string{"to": "scout"}); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("assign before being a host: %v", err)
	}

	m.host(t, "muse")
	said := taskDo(t, m.muse, "POST", comment, map[string]string{"body": "shorter, please"})
	if said.Event == nil || said.Event.Actor != "muse" || said.Event.Kind != envelope.TaskEventComment || said.Task.Title != "Draft the invitation" || said.Task.Status != envelope.TaskReturned {
		t.Fatalf("host's comment = %+v", said)
	}
	given := taskDo(t, m.muse, "POST", assign, map[string]string{"to": "scout"}).Task
	if given.Status != envelope.TaskWorking || given.Assignee != "scout" || given.AgentAssigns != 2 || given.Title != "Draft the invitation" {
		t.Fatalf("host's assign = %+v", given)
	}
	// The ask is the host's own, and the history names it.
	events := m.events(t, id)
	if last := events[len(events)-1]; last.Kind != envelope.TaskEventAssigned || last.Actor != "muse" || last.Body != "scout" {
		t.Fatalf("history after the host's assign = %+v", last)
	}
	var from, to string
	if err := m.Store.DB().QueryRow(`SELECT from_agent, to_agent FROM requests WHERE id = ?`, given.CurrentRequest).Scan(&from, &to); err != nil || from != "muse" || to != "scout" {
		t.Fatalf("the ask under the task is from %q to %q, %v", from, to, err)
	}

	// A task whose text is withheld from the host.
	hidden := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Forward the invoice from the mailbox", "source": "mail"}).Task.ID
	before := len(m.events(t, hidden))
	for _, call := range []struct {
		path string
		in   map[string]string
	}{{"/comment", map[string]string{"body": "on it"}}, {"/assign", map[string]string{"to": "scout"}}} {
		_, err := taskCall(t, m.muse, "POST", taskPath(hidden, call.path), call.in)
		if !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) || strings.Contains(err.Error(), "invoice") {
			t.Fatalf("host's %s on a withheld task: %v", call.path, err)
		}
	}
	if got := m.task(t, hidden); got.Status != envelope.TaskProposed || got.AgentAssigns != 0 || len(m.events(t, hidden)) != before {
		t.Fatalf("withheld task after the host's refused calls = %+v", got)
	}
	// The same holds once it is dropped unapproved.
	taskDo(t, m.grok, "POST", taskPath(hidden, "/drop"), nil)
	if _, err := taskCall(t, m.muse, "POST", taskPath(hidden, "/comment"), map[string]string{"body": "why dropped?"}); !refusedWith(err, http.StatusNotFound, store.ErrNoTask.Error()) {
		t.Fatalf("host's comment on a withheld, dropped task: %v", err)
	}
}

// A host's assigns are agent assigns: they count toward the task's limit,
// and the one past it is refused.
func TestTaskHostAssignsCountTowardTheLimit(t *testing.T) {
	m := newTaskMesh(t)
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Find the receipt", "assignee": "instinct"}).Task.ID
	m.finish(t, m.instinct, id, "not found", envelope.StatusFailed)
	m.host(t, "muse")
	assign := taskPath(id, "/assign")
	for i := 1; i < store.MaxAgentAssigns; i++ {
		to, worker := "scout", m.scout
		if i%2 == 0 {
			to, worker = "instinct", m.instinct
		}
		if got := taskDo(t, m.muse, "POST", assign, map[string]string{"to": to}).Task; got.AgentAssigns != i+1 {
			t.Fatalf("assign %d = %+v", i+1, got)
		}
		m.finish(t, worker, id, "not found", envelope.StatusFailed)
	}
	if _, err := taskCall(t, m.muse, "POST", assign, map[string]string{"to": "scout"}); !refusedWith(err, http.StatusConflict, store.ErrTaskAssignLimit.Error()) {
		t.Fatalf("the host's assign past the limit: %v", err)
	}
	if n := m.asks(t, id); n != store.MaxAgentAssigns {
		t.Fatalf("%d requests under the task, want %d", n, store.MaxAgentAssigns)
	}
}

// The task a route echoes back to a task host is read through the host
// view, as a list and a read are: for a task whose text is withheld it is
// the row, never the text. To an agent that is no host the echo is the task
// whole, as it has always been.
func TestTaskEchoToAHostIsTheHostView(t *testing.T) {
	m := newTaskMesh(t)
	id := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Forward the invoice from the mailbox", "detail": "to accounts", "source": "mail", "key": "invoice-3"}).Task.ID
	m.host(t, "muse")
	echo := func(agent string) (map[string]any, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		if err := m.Server.WriteTaskNowAs(rec, httptest.NewRequestWithContext(t.Context(), "POST", taskPath(id, "/comment"), nil), agent, id); err != nil {
			t.Fatal(err)
		}
		var out struct {
			Task map[string]any `json:"task"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("echo to %s: %d %s (%v)", agent, rec.Code, rec.Body, err)
		}
		return out.Task, rec.Body.String()
	}
	row, raw := echo("muse")
	wantWithheld(t, row, id, envelope.TaskProposed, "grokbot", "mail")
	for _, secret := range []string{"invoice", "to accounts"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the echo to a host carries %q: %s", secret, raw)
		}
	}
	if whole, _ := echo("scout"); whole["title"] != "Forward the invoice from the mailbox" || whole["withheld"] != nil {
		t.Fatalf("the echo to an agent that is no host = %v", whole)
	}
}
