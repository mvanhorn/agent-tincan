package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// ownerDesk joins an agent on the mesh's admin device and makes it this
// test's saved config: the owner's own machine, an admin device with an
// agent of its own.
func ownerDesk(t *testing.T, m *testrelay.Mesh) {
	t.Helper()
	if m.URL("desk") == "" {
		m.JoinOnMachineOf(t, "admin", "desk")
	}
	useConfig(t, client.Config{Relay: m.URL("admin"), Agent: "desk"})
}

// taskNow reads task id as the owner sees it.
func taskNow(t *testing.T, m *testrelay.Mesh, id int64) envelope.Task {
	t.Helper()
	task, _, err := client.Task(t.Context(), m.Client(t, "admin"), true, id)
	if err != nil {
		t.Fatalf("task %d: %v", id, err)
	}
	return task
}

// tasksRun runs tincan tasks with args and fails unless it succeeds and
// prints every want.
func tasksRun(t *testing.T, args []string, want ...string) string {
	t.Helper()
	out, err := run(t, tasksCmd(), args...)
	if err != nil {
		t.Fatalf("tasks %v = %q, %v", args, out, err)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("tasks %v = %q, lacks %q", args, out, w)
		}
	}
	return out
}

// Each subcommand calls its route and prints the task id and its new status:
// one task added, assigned, returned and completed by the owner, then the
// other decisions and an agent's own add, comment and drop.
func TestTasksCommands(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ownerDesk(t, m)
	muse := m.Client(t, "muse")

	tasksRun(t, []string{"add", "Book", "the", "venue", "--detail", "for forty people", "--owner"}, "Added task #1, now open.")
	tasksRun(t, []string{"--owner"}, "Open (1)\n  #1 Book the venue (filed by the owner)")
	// The owner's task is not one the machine's agent filed or was handed,
	// but an open, unassigned task is readable by every agent.
	tasksRun(t, nil, "#1 Book the venue")

	out := tasksRun(t, []string{"assign", "1"}, "Task #1 is open: Book the venue", "tincan tasks assign 1 <teammate>", "muse ", "grokbot ")
	if got := taskNow(t, m, 1); got.Status != envelope.TaskOpen {
		t.Fatalf("listing the roster changed the task: %+v (%q)", got, out)
	}

	tasksRun(t, []string{"assign", "#1", "muse"}, "Task #1 is now working, assigned to muse.")
	inbox, err := muse.Poll(t.Context(), 0)
	if err != nil || len(inbox.Requests) != 1 || inbox.Requests[0].From != "desk" || !strings.Contains(inbox.Requests[0].Body, "Book the venue") {
		t.Fatalf("muse's inbox = %+v, %v", inbox, err)
	}

	tasksRun(t, []string{"comment", "1", "the", "hall,", "not", "the", "tent", "--owner"},
		"Commented on task #1, which is working.", "muse gets this one with the next attempt")

	ask := inbox.Requests[0].ID
	if _, err := muse.Claim(t.Context(), ask); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(t.Context(), ask, "booked for the 4th", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	out = tasksRun(t, []string{"show", "1", "--owner"}, "Task #1: Book the venue", "Status: returned", "for forty people", "booked for the 4th", "the owner comment: the hall, not the tent")
	if strings.Contains(out, "next attempt") {
		t.Fatalf("show = %q", out)
	}
	if out := tasksRun(t, []string{"comment", "1", "thanks", "--owner"}, "Commented on task #1, which is returned."); strings.Contains(out, "next attempt") {
		t.Fatalf("a comment with no ask open = %q", out)
	}

	tasksRun(t, []string{"done", "1", "--note", "good"}, "Task #1 is now done.")
	if out := tasksRun(t, []string{"--owner"}, "Done (1)"); strings.Contains(out, "Book the venue") {
		t.Fatalf("a done task is listed without --closed: %q", out)
	}
	tasksRun(t, []string{"--owner", "--closed"}, "Done (1)\n  #1 Book the venue")
	// Without --closed the done task is not read from the relay at all.
	if out := tasksRun(t, []string{"--owner", "--json"}); strings.Contains(out, "Book the venue") {
		t.Fatalf("tasks --json lists a closed task without --closed: %q", out)
	}
	tasksRun(t, []string{"--owner", "--closed", "--json"}, `"title": "Book the venue"`)
	tasksRun(t, []string{"reopen", "1"}, "Task #1 is now open.")
	tasksRun(t, []string{"drop", "1", "--owner", "--note", "no party"}, "Task #1 is now dropped.")

	tasksRun(t, []string{"add", "Call the bank", "--mine", "--owner"}, "Added task #2, now open.")
	if got := taskNow(t, m, 2); got.Assignee != envelope.TaskOwner || got.Filer != envelope.TaskOwner {
		t.Fatalf("the owner's own = %+v", got)
	}
	tasksRun(t, []string{"add", "Draft the invite", "--to", "muse", "--owner", "--key", "invite"}, "Added task #3, now working, assigned to muse.")
	tasksRun(t, []string{"add", "Draft the invite again", "--owner", "--key", "invite"}, "Task #3 already has that key and is working; nothing was added.")

	// An agent's own: filed as the agent, and text from outside the team
	// waits for the owner.
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	tasksRun(t, []string{"add", "Pay the invoice", "--source", "mail", "--to", "muse"}, "Added task #4, now proposed.", "nothing was sent")
	out = tasksRun(t, []string{"add", "Tidy the notes", "--json"})
	var res client.TaskResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Task.ID != 5 || res.Task.Filer != "grokbot" {
		t.Fatalf("add --json = %q, %v", out, err)
	}
	tasksRun(t, []string{"comment", "5", "by", "Friday"}, "Commented on task #5, which is open.")
	tasksRun(t, []string{"show", "5"}, "grokbot comment: by Friday")
	out = tasksRun(t, []string{"--json"})
	var list []envelope.Task
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) == 0 {
		t.Fatalf("tasks --json = %q, %v", out, err)
	}
	tasksRun(t, []string{"drop", "5"}, "Task #5 is now dropped.")
	if out, err := run(t, tasksCmd(), "show", "not-a-number"); err == nil {
		t.Fatalf("bad id = %q", out)
	}

	ownerDesk(t, m)
	tasksRun(t, []string{"approve", "4"}, "Task #4 is now open.")
	tasksRun(t, []string{"reset", "4"}, "Task #4 is open; agents can assign it again.")
}

// The relay host usually has no saved config: there the local admin socket
// makes every tasks command the owner's, with no flags.
func TestTasksOnRelayHostWithoutFlags(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	serveAdminSocket(t, m)
	tasksRun(t, []string{"add", "Water the plants"}, "Added task #1, now open.")
	tasksRun(t, nil, "#1 Water the plants (filed by the owner)")
	tasksRun(t, []string{"show", "1"}, "Task #1: Water the plants")
	tasksRun(t, []string{"assign", "1"}, "muse ")
	tasksRun(t, []string{"done", "1"}, "Task #1 is now done.")
	// The socket carries no agent, so nothing can be sent from here.
	if out, err := run(t, tasksCmd(), "add", "Feed the cat", "--to", "muse"); !errors.Is(err, errTaskNoAgent) {
		t.Fatalf("add --to on the relay host = %q, %v", out, err)
	}
	if list, _, err := client.Tasks(t.Context(), m.Client(t, "admin"), true, true); err != nil || len(list) != 1 {
		t.Fatalf("tasks = %+v, %v", list, err)
	}
}

// Against a relay without the capability every subcommand fails with the
// one unsupported message, and no task route is called.
func TestTasksRefuseOldRelay(t *testing.T) {
	var mu sync.Mutex
	var called []string
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"attachments":true,"schedules":true}`))
			return
		}
		if strings.Contains(r.URL.Path, "/tasks") {
			mu.Lock()
			called = append(called, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(old.Close)
	useConfig(t, client.Config{Relay: old.URL, Agent: "desk"})
	for _, args := range [][]string{
		{}, {"--owner"},
		{"add", "x"}, {"add", "x", "--owner", "--to", "muse"},
		{"show", "1"}, {"show", "1", "--owner"},
		{"assign", "1"}, {"assign", "1", "muse"},
		{"comment", "1", "x"}, {"comment", "1", "x", "--owner"},
		{"drop", "1"}, {"drop", "1", "--owner"},
		{"done", "1"}, {"approve", "1"}, {"reopen", "1"}, {"reset", "1"},
	} {
		out, err := run(t, tasksCmd(), args...)
		if !errors.Is(err, client.ErrTasksUnsupported) || !strings.Contains(err.Error(), "this relay has no task list") {
			t.Errorf("tasks %v = %q, %v", args, out, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(called) != 0 {
		t.Fatalf("task routes were called on an old relay: %v", called)
	}
}

// A task's text is teammates' text: on a terminal its control and bidi
// runes are made harmless wherever a task is printed.
func TestTasksOutputIsTerminalSafe(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	fakeTerminal(t, everyWriterIsTerminal)
	tasksRun(t, []string{"add", "wipe\x1b[2J the\u202e screen", "--detail", "line one\n\x1b[1Aline two\x07"}, "Added task #1")
	tasksRun(t, []string{"comment", "1", "ring\x07\x1b]0;title\x07"})
	for _, args := range [][]string{{"show", "1"}, {}} {
		out := tasksRun(t, args, "wipe [2J the screen")
		assertTerminalSafe(t, "tasks "+strings.Join(args, " "), out)
	}
	tasksRun(t, []string{"show", "1"}, "line one\n [1Aline two")

	// The roster an assign with no teammate prints is safe too, and so is
	// the task's title above it.
	ownerDesk(t, m)
	assertTerminalSafe(t, "tasks assign", tasksRun(t, []string{"assign", "1"}, "wipe [2J the screen"))
}

// Assigning sends an ask, which has to come from an agent. On a machine
// with none the command says so, points at the owner page, and nothing
// reaches the relay.
func TestTasksAssignNeedsAnAgent(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("admin")})
	tasksRun(t, []string{"add", "Book the venue", "--owner"}, "Added task #1, now open.")

	for _, args := range [][]string{{"assign", "1", "muse"}, {"add", "Another", "--owner", "--to", "muse"}} {
		out, err := run(t, tasksCmd(), args...)
		if !errors.Is(err, errTaskNoAgent) {
			t.Fatalf("tasks %v = %q, %v", args, out, err)
		}
		for _, want := range []string{"this machine has not joined", "nothing was changed", "owner page"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("tasks %v: %q lacks %q", args, err, want)
			}
		}
	}
	if got := taskNow(t, m, 1); got.Status != envelope.TaskOpen || got.Assignee != "" || got.CurrentRequest != "" {
		t.Fatalf("task after a refused assign = %+v", got)
	}
	if list, _, err := client.Tasks(t.Context(), m.Client(t, "admin"), true, true); err != nil || len(list) != 1 {
		t.Fatalf("tasks = %+v, %v", list, err)
	}
	if inbox, err := m.Client(t, "muse").Poll(t.Context(), 0); err != nil || len(inbox.Requests) != 0 {
		t.Fatalf("muse's inbox = %+v, %v", inbox, err)
	}
}

// The owner's decisions and the owner's view are refused by the relay on a
// machine that is not an admin device, and the command reports the refusal.
func TestTasksOwnerCommandsNeedAnAdminDevice(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	tasksRun(t, []string{"add", "Book the venue"}, "Added task #1, now open.")

	for _, args := range [][]string{
		{"done", "1"}, {"approve", "1"}, {"reopen", "1"}, {"reset", "1"}, {"assign", "1", "muse"}, {"assign", "1"},
		{"drop", "1", "--owner"}, {"comment", "1", "x", "--owner"}, {"show", "1", "--owner"}, {"--owner"}, {"add", "x", "--owner"},
	} {
		out, err := run(t, tasksCmd(), args...)
		if !client.IsStatus(err, http.StatusForbidden) || !strings.Contains(err.Error(), identity.ErrNotAdmin.Error()) {
			t.Errorf("tasks %v = %q, %v", args, out, err)
		}
	}
	if got := taskNow(t, m, 1); got.Status != envelope.TaskOpen || got.Assignee != "" {
		t.Fatalf("task after refused decisions = %+v", got)
	}
	if list, _, err := client.Tasks(t.Context(), m.Client(t, "admin"), true, true); err != nil || len(list) != 1 {
		t.Fatalf("tasks = %+v, %v", list, err)
	}
}

// tasks add --every files a task that repeats for the --to teammate, and
// says how to stop the repeat. It needs a teammate and a duration.
func TestTasksAddRepeating(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ownerDesk(t, m)
	for _, args := range [][]string{
		{"add", "Check the backups", "--every", "24h"},
		{"add", "Check the backups", "--mine", "--every", "24h"},
		{"add", "Check the backups", "--to", "muse", "--every", "daily"},
	} {
		if out, err := run(t, tasksCmd(), args...); err == nil {
			t.Fatalf("tasks %v = %q, want a refusal", args, out)
		}
	}
	if list, _, err := client.Tasks(t.Context(), m.Client(t, "admin"), true, true); err != nil || len(list) != 0 {
		t.Fatalf("refused adds stored %+v, %v", list, err)
	}
	tasksRun(t, []string{"add", "Check the backups", "--to", "muse", "--every", "24h"},
		"Added task #1, now working, assigned to muse.\nIt repeats every 24h0m0s for muse until it is done or dropped; stop only the repeat with tincan unschedule ")
	got := taskNow(t, m, 1)
	if got.RepeatEverySeconds != 86400 || got.RepeatTo != "muse" || got.Filer != "desk" {
		t.Fatalf("task = %+v", got)
	}
	tasksRun(t, nil, "#1 Check the backups (filed by desk, with muse, ask queued, repeats every 24h0m0s)")
	tasksRun(t, []string{"show", "1"}, fmt.Sprintf("Repeats: every 24h0m0s for muse (schedule #%d)", got.RepeatSchedule))
}

// tincan tasks host set names a task host and prints the relay's warnings,
// tincan agents and list_agents both show the mark, and clear removes it and
// says what stays. The relay's refusal for an agent that can never host
// reaches the owner, and a machine that is not an admin device is refused.
func TestTasksHostCommands(t *testing.T) {
	m := testrelay.NewWithAdminLogin(t, relay.Config{})
	ctx := t.Context()
	m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"grokbot"}, ForSeconds: 3600})
	useConfig(t, client.Config{Relay: m.URL("admin")})
	out := tasksRun(t, []string{"host", "set", "grokbot"},
		"\"grokbot\" is now a task host.\n",
		"Warning: grokbot wakes by none, which cannot start a turn",
		"Warning: guest friend may ask grokbot")
	if n := strings.Count(out, "Warning: "); n != 2 {
		t.Fatalf("host set printed %d warnings, want 2:\n%s", n, out)
	}
	if out, _ := run(t, agentsCmd()); !strings.Contains(out, " task_host=yes") {
		t.Fatalf("roster after set:\n%s", out)
	}

	srvT, cliT := mcp.NewInMemoryTransports()
	ss, err := mcpserver.New(m.Client(t, "muse"), "test").Connect(ctx, srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	listAgents := func() string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents"})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}
	if text := listAgents(); !strings.Contains(text, ", task_host=yes") || strings.Count(text, "task_host=") != 1 {
		t.Fatalf("list_agents after set:\n%s", text)
	}

	if err := m.Client(t, "admin").SetKind(ctx, "muse", "notes"); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, tasksCmd(), "host", "set", "muse"); err == nil || !strings.Contains(err.Error(), "muse cannot be a task host: notes is a service agent") {
		t.Fatalf("host set of a notes agent = %q, %v", out, err)
	}

	out = tasksRun(t, []string{"host", "clear", "grokbot"})
	if out != "\"grokbot\" is no longer a task host. Tasks it assigned earlier stay attributed to grokbot.\n" {
		t.Fatalf("host clear printed %q", out)
	}
	if out, _ := run(t, agentsCmd()); strings.Contains(out, "task_host=") {
		t.Fatalf("roster after clear:\n%s", out)
	}
	if text := listAgents(); strings.Contains(text, "task_host=") {
		t.Fatalf("list_agents after clear:\n%s", text)
	}

	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	for _, args := range [][]string{{"host", "set", "grokbot"}, {"host", "clear", "grokbot"}} {
		out, err := run(t, tasksCmd(), args...)
		if !client.IsStatus(err, http.StatusForbidden) || !strings.Contains(err.Error(), identity.ErrNotAdmin.Error()) {
			t.Errorf("tasks %v from a teammate = %q, %v", args, out, err)
		}
	}
}

// The roster marks a host that stopped being eligible as suspended, with
// the reason, in tincan agents.
func TestAgentsShowsSuspendedTaskHost(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("admin")})
	tasksRun(t, []string{"host", "set", "muse"}, "\"muse\" is now a task host.")
	if err := m.Client(t, "admin").SetKind(t.Context(), "muse", "notes"); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(t, agentsCmd()); !strings.Contains(out, ` task_host="suspended: notes is a service agent"`) {
		t.Fatalf("roster with a suspended host:\n%s", out)
	}
}

// On the relay host the local admin socket names a host with no flags, and
// --socket does the same from a named path.
func TestTasksHostThroughTheAdminSocket(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	serveAdminSocket(t, m)
	tasksRun(t, []string{"host", "set", "grokbot"}, "\"grokbot\" is now a task host.")
	if a, _, err := m.Dir.Agent(t.Context(), "grokbot"); err != nil || !a.TaskHost {
		t.Fatalf("grokbot after set on the relay host = %+v, %v", a, err)
	}
	tasksRun(t, []string{"host", "clear", "grokbot", "--socket", adminSocket(t, m)}, "\"grokbot\" is no longer a task host.")
	if a, _, err := m.Dir.Agent(t.Context(), "grokbot"); err != nil || a.TaskHost {
		t.Fatalf("grokbot after clear through --socket = %+v, %v", a, err)
	}
}

// A relay older than the task host route answers Go's plain "404 page not
// found", which tincan tasks host turns into an upgrade hint, for set and
// for clear, without asking the relay what it supports first. A JSON 404
// for an unknown agent passes through unchanged.
func TestTasksHostAgainstOlderRelay(t *testing.T) {
	var mu sync.Mutex
	var called []string
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every command asks who it is first; that call is not the test's.
		if r.URL.Path != "/v1/whoami" {
			mu.Lock()
			called = append(called, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(old.Close)
	useConfig(t, client.Config{Relay: old.URL})
	for _, verb := range []string{"set", "clear"} {
		out, err := run(t, tasksCmd(), "host", verb, "grokbot")
		if !client.IsStatus(err, http.StatusNotFound) || !strings.Contains(err.Error(), "older than this tincan and cannot store task hosts yet") || !strings.Contains(err.Error(), "upgrade the relay") {
			t.Errorf("tasks host %s against an older relay = %q, %v", verb, out, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"PUT /v1/agents/grokbot/task-host", "PUT /v1/agents/grokbot/task-host"}; !slices.Equal(called, want) {
		t.Fatalf("calls to the older relay = %v, want %v", called, want)
	}

	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("admin")})
	if out, err := run(t, tasksCmd(), "host", "set", "nobody"); !client.IsStatus(err, http.StatusNotFound) || strings.Contains(err.Error(), "upgrade") || !strings.Contains(err.Error(), identity.ErrUnknownAgent.Error()) {
		t.Fatalf("tasks host set of an unknown agent = %q, %v", out, err)
	}
}

// A task host reads the whole list from its shell with --host, as its
// list_tasks and get_task tools do; without --host it gets its ordinary view.
func TestTasksHostViewFromTheShell(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("admin")})
	tasksRun(t, []string{"host", "set", "muse"}, "\"muse\" is now a task host.")
	tasksRun(t, []string{"add", "--owner", "--mine", "Book the dentist"}, "Added task #1")

	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	if out := tasksRun(t, nil); strings.Contains(out, "Book the dentist") {
		t.Fatalf("ordinary view shows the owner's own task:\n%s", out)
	}
	tasksRun(t, []string{"--host"}, "You are the owner's task host", "Book the dentist")
	tasksRun(t, []string{"show", "1", "--host"}, "Book the dentist")
	for _, args := range [][]string{{"--host", "--owner"}, {"show", "1", "--host", "--owner"}} {
		if out, err := run(t, tasksCmd(), args...); err == nil || !strings.Contains(err.Error(), "--host is a task host's view") {
			t.Errorf("tasks %v = %q, %v", args, out, err)
		}
	}
}

// A task host marks a task done and reopens it from its shell with
// tincan tasks decide, on the owner's words; --words is required, and an
// agent that is no host is refused with the command that names one.
func TestTasksHostDecideFromTheShell(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("admin")})
	tasksRun(t, []string{"add", "--owner", "--mine", "Book the dentist"}, "Added task #1")

	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	if out, err := run(t, tasksCmd(), "decide", "1", "done", "--words", "booked it"); err == nil || !strings.Contains(err.Error(), "tincan tasks host set") {
		t.Fatalf("decide by an agent that is no host = %q, %v", out, err)
	}
	useConfig(t, client.Config{Relay: m.URL("admin")})
	tasksRun(t, []string{"host", "set", "muse"}, "\"muse\" is now a task host.")

	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	if out, err := run(t, tasksCmd(), "decide", "1", "done"); err == nil || !strings.Contains(err.Error(), "--words is required") {
		t.Fatalf("decide with no words = %q, %v", out, err)
	}
	tasksRun(t, []string{"decide", "#1", "done", "--words", "the dentist is booked"}, "Task #1 (Book the dentist) is now done", "reopen it the same way")
	if got := taskNow(t, m, 1); got.Status != envelope.TaskDone {
		t.Fatalf("task after decide = %+v", got)
	}
	tasksRun(t, []string{"show", "1", "--host"}, "muse status (done) for the owner, who said: the dentist is booked")
	tasksRun(t, []string{"decide", "1", "reopen", "--words", "not booked yet"}, "is now open")
	if out, err := run(t, tasksCmd(), "decide", "1", "approve", "--words", "approve it"); err == nil || !strings.Contains(err.Error(), "unknown decision") {
		t.Fatalf("decide approve = %q, %v", out, err)
	}
}

func TestTasksDueCommands(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ownerDesk(t, m)
	tasksRun(t, []string{"add", "Regular task", "--owner"}, "Added task #1")
	tasksRun(t, []string{"add", "Overdue task", "--owner", "--due", "2000-11-21"}, "Due 2000-11-21.")
	tasksRun(t, []string{"add", "Today's task", "--owner", "--due", "today"}, "Added task #3")
	out := tasksRun(t, []string{"--owner"}, "Overdue (1)\n  #2", "Due today (1)\n  #3", "Open (1)\n  #1")
	if strings.Index(out, "Overdue (1)") > strings.Index(out, "Due today (1)") || strings.Index(out, "Due today (1)") > strings.Index(out, "Open (1)") {
		t.Fatalf("due task order = %q", out)
	}
	out = tasksRun(t, []string{"--owner", "--json"}, "2000-11-21")
	var list []envelope.Task
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 3 || list[0].ID != 2 || list[1].ID != 3 {
		t.Fatalf("JSON due order = %s, %v", out, err)
	}
	tasksRun(t, []string{"show", "2", "--owner"}, "Overdue: 2000-11-21")
	tasksRun(t, []string{"due", "2", "2030-11-29", "3pm", "--owner"}, "Task #2 is due 2030-11-29 15:00 PST.")
	tasksRun(t, []string{"due", "2", "clear", "--owner"}, "Cleared the due date for task #2.")
	tasksRun(t, []string{"show", "2", "--owner"}, "changed from 2000-11-21 to 2030-11-29 15:00 PST", "changed from 2030-11-29 15:00 PST to no deadline")
}

func TestTasksBriefingNowCommand(t *testing.T) {
	m := testrelay.New(t, relay.Config{TaskBriefing: "off"})
	ownerDesk(t, m)
	ctx := t.Context()
	if _, err := m.Client(t, "admin").SetTaskHost(ctx, "muse", true); err != nil {
		t.Fatal(err)
	}
	tasksRun(t, []string{"briefing", "--now"}, "No briefing queued.")
	tasksRun(t, []string{"add", "Private deadline", "--owner", "--due", "2000-11-21"}, "Added task #1")
	tasksRun(t, []string{"briefing", "--now"}, "Queued a task briefing.")
	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	stdout, _, err := runSplit(t, inboxCmd(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var in inboxJSON
	if err := json.Unmarshal([]byte(stdout), &in); err != nil || !in.TaskHost || len(in.Requests) != 1 {
		t.Fatalf("briefing inbox = %s, %v", stdout, err)
	}
	req := in.Requests[0]
	if req.TaskNoticeKind != envelope.TaskNoticeBriefing || !req.Claimed ||
		!strings.Contains(req.OwnerMessage, "task #1 (open, due 2000-11-21)") ||
		!strings.Contains(in.TaskNoticePosting, "last tool call") || strings.Contains(stdout, "Private deadline") {
		t.Fatalf("briefing owner message = %s", stdout)
	}
	if err := client.TaskNoticesTold(ctx, m.Client(t, "muse"), []string{req.ID}); err != nil {
		t.Fatalf("acknowledge the claimed briefing: %v", err)
	}
}

func TestTasksDecideAssignCommand(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := t.Context()
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	if _, err := m.Client(t, "admin").SetTaskHost(ctx, "grokbot", true); err != nil {
		t.Fatal(err)
	}
	tasksRun(t, []string{"add", "Call the dentist"}, "Added task #1")
	tasksRun(t, []string{"decide", "1", "assign", "--to", "muse", "--words", "give 1 to muse"}, "Assigned task #1 to muse for the owner.")
	if got := taskNow(t, m, 1); got.Assignee != "muse" || got.AgentAssigns != 0 {
		t.Fatalf("host assigned task = %+v", got)
	}
}
