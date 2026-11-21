package relay_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

func TestRecoveredAskReachesHostThroughTaskNotice(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "grokbot")
	ctx := t.Context()
	const body = "check the phone line\nprivate request detail"
	req, err := m.grok.Send(ctx, "muse", body, envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.muse.Reply(ctx, req.ID, "private reply detail", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	m.Server.Sweep(ctx)
	tasks, _, view, err := client.HostTasks(ctx, m.grok, false)
	if err != nil || !view.Served || len(tasks) != 1 {
		t.Fatalf("host list = %+v, %+v, %v", tasks, view, err)
	}
	notice := read(t, m.grok)
	if len(notice) != 1 || notice[0].TaskNoticeID != tasks[0].ID || notice[0].TaskNoticeStatus != envelope.TaskOpen {
		t.Fatalf("host inbox = %+v", notice)
	}
	m.wantNoTaskText(t, "private request detail", "private reply detail")
	task, history, err := client.HostTask(ctx, m.grok, tasks[0].ID)
	if err != nil || task.Detail != body || len(history) != 1 || !strings.Contains(history[0].Body, "from grokbot to muse") {
		t.Fatalf("host task = %+v, %+v, %v", task, history, err)
	}
	if _, _, err := client.Task(ctx, m.instinct, false, task.ID); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("task for unrelated non-host: %v", err)
	}
	// An eligible host can read the task, but its mark grants no wider search.
	m.host(t, "scout")
	if hits, err := m.scout.Search(ctx, "phone line", 20); err != nil || len(hits) != 0 {
		t.Fatalf("host searched unrelated request: %+v, %v", hits, err)
	}
}

func TestRecoveredOutsideTextIsWithheldAndCarriesItsSource(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "scout")
	ctx := t.Context()
	req, err := m.grok.Send(client.WithAskSource(ctx, "mail"), "muse", "confidential outside text", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, m.muse, req.ID)
	continued, err := m.muse.Send(ctx, "instinct", "forwarded confidential data", envelope.KindAsk, req.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.instinct.Reply(ctx, continued.ID, "failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := m.muse.Reply(ctx, req.ID, "failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	tasks, _, view, err := client.HostTasks(ctx, m.scout, false)
	if err != nil || !view.Served || len(tasks) != 2 {
		t.Fatalf("host list = %+v, %+v, %v", tasks, view, err)
	}
	for _, task := range tasks {
		one, history, err := client.HostTask(ctx, m.scout, task.ID)
		if err != nil || one.Status != envelope.TaskProposed || one.Source != "mail" || !one.Withheld ||
			one.Detail != "" || one.Title != "" || one.Key != "" || len(history) != 0 {
			t.Fatalf("host task = %+v, %+v, %v", one, history, err)
		}
		if _, err := client.AssignTask(ctx, m.scout, false, task.ID, "muse"); !client.IsStatus(err, http.StatusNotFound) {
			t.Fatalf("assign of proposed task = %v", err)
		}
	}
	m.wantNoTaskText(t, "confidential outside text", "forwarded confidential data")
}

func TestRecoveredApprovedGuestAskStillNeedsTaskApproval(t *testing.T) {
	m, guest := guestMesh(t, relay.Config{})
	ctx := t.Context()
	host := m.Client(t, "instinct")
	if _, err := m.Client(t, "admin").SetTaskHost(ctx, "instinct", true); err != nil {
		t.Fatal(err)
	}
	req := deliveredGuestAsk(t, m, guest, "private guest text")
	if _, err := m.Client(t, "muse").Reply(ctx, req.ID, "failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	tasks, _, view, err := client.HostTasks(ctx, host, false)
	if err != nil || !view.Served || len(tasks) != 1 || !tasks[0].Withheld || tasks[0].Status != envelope.TaskProposed || tasks[0].Source != "guest" {
		t.Fatalf("host list = %+v, %+v, %v", tasks, view, err)
	}
	if _, _, err := client.Tasks(ctx, guest, false, false); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("guest task list = %v", err)
	}
}

func TestTaskRecoverySkipsRoomMentionAndItsChildren(t *testing.T) {
	m := roomMesh(t)
	ctx := t.Context()
	post, err := client.PostToRoom(ctx, m.Client(t, "grokbot"), "launch", "@muse private room text")
	if err != nil || len(post.Post.Mentions) != 1 {
		t.Fatalf("post = %+v, %v", post, err)
	}
	muse := m.Client(t, "muse")
	req := claimOne(t, muse, post.Post.Mentions[0].RequestID)
	instinct := m.Client(t, "instinct")
	child, err := muse.Send(ctx, "instinct", "from the private room ask", envelope.KindAsk, req.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instinct.Reply(ctx, child.ID, "failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(ctx, req.ID, "failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	if tasks, err := m.Store.Tasks(ctx, "", 0, 10, true); err != nil || len(tasks) != 0 {
		t.Fatalf("room asks were auto-filed: %+v, %v", tasks, err)
	}
}

func TestTaskRecoverySkipsAnActiveRepeat(t *testing.T) {
	m := newScheduleMesh(t)
	ctx := t.Context()
	m.create(t, m.grok, m.clk.now().Add(time.Minute), 0)
	m.create(t, m.grok, m.clk.now().Add(2*time.Minute), time.Hour)
	m.clk.advance(time.Minute)
	m.Server.Sweep(ctx)
	sent := m.sentTo(t, "muse")
	if len(sent) != 1 {
		t.Fatalf("one-shot asks = %+v", sent)
	}
	if _, err := m.muse.Reply(ctx, sent[0].ID, "failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	m.clk.advance(time.Minute)
	m.Server.Sweep(ctx)
	repeating := m.sentTo(t, "muse")
	if len(repeating) != 1 {
		t.Fatalf("repeat asks = %+v", repeating)
	}
	if _, err := m.muse.Reply(ctx, repeating[0].ID, "failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	if tasks, err := m.Store.Tasks(ctx, "", 0, 10, true); err != nil || len(tasks) != 1 || tasks[0].Key != sent[0].ID {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}

	m.clk.advance(time.Hour)
	m.Server.Sweep(ctx)
	repeating = m.sentTo(t, "muse")
	if len(repeating) != 1 {
		t.Fatalf("next repeat asks = %+v", repeating)
	}
	// Removing a teammate ends the repeat as well as cancelling its ask.
	// The request has a final outcome even though cancellation happens first.
	if err := m.admin.Remove(ctx, "muse"); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	if tasks, err := m.Store.Tasks(ctx, "", 0, 10, true); err != nil || len(tasks) != 2 || tasks[1].Key != repeating[0].ID {
		t.Fatalf("cancelled repeat's final ask = %+v, %v", tasks, err)
	}
}

func TestRecoveredAskAssignmentKeepsOriginalAskerInApprovalPolicy(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "scout")
	ctx := t.Context()
	req, err := m.grok.Send(ctx, "muse", "carry these instructions", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.muse.Reply(ctx, req.ID, "failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	tasks, _, _, err := client.HostTasks(ctx, m.scout, false)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("host tasks = %+v, %v", tasks, err)
	}
	m.gate(t, `{"gate":{"instinct":{"from":["grokbot"]}},"hold_ttl":"1h"}`)
	task, err := client.AssignTask(ctx, m.scout, false, tasks[0].ID, "instinct")
	if err != nil || task.AskStatus != envelope.StatusHeld {
		t.Fatalf("assignment carrying held original text = %+v, %v", task, err)
	}
}

func TestRecoveredAskSentWhileGuestAskIsOpenIsProposed(t *testing.T) {
	m, guest := guestMesh(t, relay.Config{})
	ctx := t.Context()
	guestReq := guestAsk(t, guest, "private guest instructions")
	approve(t, m, guestReq.ID)
	muse := m.Client(t, "muse")
	in, err := muse.Poll(ctx, 0)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].ID != guestReq.ID {
		t.Fatalf("guest request poll = %+v, %v", in, err)
	}
	forwarded, err := muse.Send(ctx, "instinct", "text read before claiming", envelope.KindAsk, "", false)
	if err != nil || forwarded.FromGuest || forwarded.Status == envelope.StatusHeld {
		t.Fatalf("ordinary send rules changed: %+v, %v", forwarded, err)
	}
	if _, err := m.Client(t, "instinct").Reply(ctx, forwarded.ID, "failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	tasks, err := m.Store.Tasks(ctx, "", 0, 10, true)
	if err != nil || len(tasks) != 1 || tasks[0].Key != forwarded.ID || tasks[0].Status != envelope.TaskProposed || tasks[0].Source != "guest" {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
}
