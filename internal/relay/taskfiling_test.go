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

func TestWorkAskFilesAndFollowsReply(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply envelope.Status
		want  envelope.TaskStatus
	}{
		{"answered", envelope.StatusAnswered, envelope.TaskReturned},
		{"needs input", envelope.StatusNeedsInput, envelope.TaskQuestion},
		{"failed", envelope.StatusFailed, envelope.TaskOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTaskMesh(t)
			m.host(t, "scout")
			ctx := t.Context()
			const body = "  Check the line  \nprivate request detail"
			sent, err := m.grok.SendResult(ctx, "muse", body, envelope.KindAsk, "", false)
			if err != nil || sent.TaskID == 0 {
				t.Fatalf("send = %+v, %v, want a task ID", sent, err)
			}
			task := m.task(t, sent.TaskID)
			if task.Title != "Check the line" || task.Detail != body || task.Filer != "grokbot" || task.Assignee != "muse" ||
				task.CurrentRequest != sent.ID || task.Status != envelope.TaskWorking {
				t.Fatalf("task at send = %+v", task)
			}
			if _, err := client.DropTask(ctx, m.grok, false, task.ID, "drop"); !client.IsStatus(err, http.StatusConflict) {
				t.Fatalf("drop with attempt open: %v", err)
			}
			claimOne(t, m.muse, sent.ID)
			const reply = "private reply detail"
			if _, err := m.muse.Reply(ctx, sent.ID, reply, tc.reply); err != nil {
				t.Fatal(err)
			}
			m.Server.Sweep(ctx)
			tasks, err := m.Store.Tasks(ctx, "", 0, 10, true)
			if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID || tasks[0].Status != tc.want {
				t.Fatalf("tasks = %+v, %v", tasks, err)
			}
			if tc.reply == envelope.StatusAnswered && (tasks[0].Result != reply || tasks[0].ResultRequest != sent.ID) {
				t.Fatalf("returned task = %+v", tasks[0])
			}
			res, err := m.grok.Get(ctx, sent.ID, 0)
			if err != nil || res.Request.TaskID != task.ID || res.Request.ID != sent.ID || res.Reply == nil ||
				res.Reply.Body != reply || res.Status != tc.reply || res.Reply.From != "muse" {
				t.Fatalf("asker result = %+v, %v", res, err)
			}
			notice := read(t, m.scout)
			if len(notice) != 1 || notice[0].TaskNoticeID != task.ID || notice[0].TaskNoticeStatus != tc.want {
				t.Fatalf("host inbox = %+v", notice)
			}
			m.wantNoTaskText(t, "Check the line", "private request detail", reply)
		})
	}
}

func TestWorkAskUnderClaimRecordsHistory(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "scout")
	task := m.add(t, envelope.Task{Title: "parent work", Filer: "grokbot"})
	attempt := m.assign(t, task.ID, "grokbot", "muse")
	claimOne(t, m.muse, attempt.ID)
	ctx := t.Context()
	child, err := m.muse.Send(ctx, "instinct", "  Check this  \nprivate detail", envelope.KindAsk, "", false)
	if err != nil || child.TaskID != 0 {
		t.Fatalf("child = %+v, %v", child, err)
	}
	tasks, err := m.Store.Tasks(ctx, "", 0, 10, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	history := m.events(t, task.ID)
	if got := history[len(history)-1]; got.Body != "muse asked instinct: Check this" || got.Actor != "muse" {
		t.Fatalf("child history = %+v", got)
	}
}

func TestWorkAskFilingModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		host, want bool
	}{
		{name: "default with no host"},
		{name: "explicit auto with host", mode: "auto", host: true, want: true},
		{name: "off with host", mode: "off", host: true},
		{name: "on without host", mode: "on", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTaskMeshWithConfig(t, relay.Config{FileAllAsks: tc.mode})
			if tc.host {
				m.host(t, "scout")
			}
			ctx := t.Context()
			req, err := m.grok.Send(ctx, "muse", "Check the line", envelope.KindAsk, "", false)
			if err != nil || (req.TaskID != 0) != tc.want {
				t.Fatalf("send = %+v, %v, want a task: %v", req, err, tc.want)
			}
			if _, err := m.muse.Reply(ctx, req.ID, "ok", envelope.StatusAnswered); err != nil {
				t.Fatal(err)
			}
			m.Server.Sweep(ctx)
			tasks, err := m.Store.Tasks(ctx, "", 0, 10, true)
			if err != nil || (len(tasks) == 1) != tc.want {
				t.Fatalf("tasks = %+v, %v", tasks, err)
			}
		})
	}
}

func TestHeldWorkAskTextStaysPrivate(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "scout")
	m.host(t, "muse")
	m.gate(t, `{"gate":{"muse":{"from":["grokbot"]}},"hold_ttl":"1h"}`)
	ctx := t.Context()
	req, err := m.grok.Send(ctx, "muse", "Private while held", envelope.KindAsk, "", false)
	if err != nil || req.Status != envelope.StatusHeld || req.TaskID == 0 {
		t.Fatalf("send = %+v, %v", req, err)
	}
	checkWithheld := func() {
		t.Helper()
		task, history, err := client.HostTask(ctx, m.scout, req.TaskID)
		if err != nil || !task.Withheld || task.Title != "" || task.Detail != "" || len(history) != 0 {
			t.Fatalf("other host sees %+v, %+v, %v", task, history, err)
		}
	}
	checkWithheld()
	if _, _, err := client.HostTask(ctx, m.muse, req.TaskID); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("held target sees task: %v", err)
	}
	if err := m.admin.Raw(ctx, http.MethodPost, "/v1/admin/requests/"+req.ID+"/deny", map[string]string{"reason": "private reason"}, nil); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	checkWithheld()
	tasks, err := m.Store.Tasks(ctx, "", 0, 10, true)
	if err != nil || len(tasks) != 1 || tasks[0].Status != envelope.TaskOpen {
		t.Fatalf("after denial = %+v, %v", tasks, err)
	}
}

func TestWorkAskFilingSkipsExcludedSends(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "scout")
	ctx := t.Context()
	for _, tc := range []struct {
		name, target, source, group string
		kind                        envelope.Kind
	}{
		{name: "notify", target: "muse", kind: envelope.KindNotify},
		{name: "outside", target: "muse", source: "mail", kind: envelope.KindAsk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := m.grok.SendGrouped(client.WithAskSource(ctx, tc.source), tc.target, "Check the line", tc.kind, "", tc.group)
			if err != nil || req.TaskID != 0 {
				t.Fatalf("send = %+v, %v", req, err)
			}
		})
	}
	tasks, err := m.Store.Tasks(ctx, "", 0, 10, true)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
}

func TestWorkAskHistoryTruncatesFirstLine(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "scout")
	task := m.add(t, envelope.Task{Title: "parent work", Filer: "grokbot"})
	attempt := m.assign(t, task.ID, "grokbot", "muse")
	claimOne(t, m.muse, attempt.ID)
	req, err := m.muse.Send(t.Context(), "instinct", strings.Repeat("日", 100)+"\nprivate detail", envelope.KindAsk, "", false)
	if err != nil || req.TaskID != 0 {
		t.Fatalf("send = %+v, %v", req, err)
	}
	history := m.events(t, task.ID)
	want := "muse asked instinct: " + strings.Repeat("日", 58) + "…"
	if got := history[len(history)-1]; got.Kind != envelope.TaskEventAsked || got.Body != want {
		t.Fatalf("event = %+v, want %q", got, want)
	}
}

func TestQuietAnswerIsHiddenAndCountedOnce(t *testing.T) {
	m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off"})
	m.host(t, "scout")
	ctx := t.Context()
	if err := m.admin.SetKind(ctx, "muse", "notes"); err != nil {
		t.Fatal(err)
	}
	req, err := m.grok.Send(ctx, "muse", "Notes lookup", envelope.KindAsk, "", false)
	if err != nil || req.TaskID == 0 || !m.task(t, req.TaskID).Quiet {
		t.Fatalf("quiet ask = %+v, %v", req, err)
	}
	if _, err := m.muse.Reply(ctx, req.ID, "private lookup result", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	if inbox := read(t, m.scout); len(inbox) != 0 {
		t.Fatalf("quiet answer sent a notice: %+v", inbox)
	}
	if tasks, _, _, err := client.HostTasks(ctx, m.scout, false); err != nil || len(tasks) != 0 {
		t.Fatalf("default list = %+v, %v", tasks, err)
	}
	if tasks, _, _, err := client.HostTasks(ctx, m.scout, false, true); err != nil || len(tasks) != 1 ||
		tasks[0].ID != req.TaskID || !tasks[0].Quiet {
		t.Fatalf("quiet list = %+v, %v", tasks, err)
	}
	task, history, err := client.HostTask(ctx, m.scout, req.TaskID)
	if err != nil || !task.Quiet || task.Status != envelope.TaskReturned || !task.Withheld || task.Title != "" || task.Detail != "" || task.Result != "" || len(history) != 0 {
		t.Fatalf("quiet task = %+v, %+v, %v", task, history, err)
	}
	original, _, err := client.Task(ctx, m.grok, false, req.TaskID)
	if err != nil || original.Result != "private lookup result" {
		t.Fatalf("lookup result for asker = %+v, %v", original, err)
	}
	day := m.clk.now().Format(time.DateOnly)
	if queued, err := m.Store.QueueTaskBriefing(ctx, day, day); err != nil || !queued {
		t.Fatalf("quiet briefing = %v, %v", queued, err)
	}
	m.Server.Sweep(ctx)
	got := read(t, m.scout)
	if len(got) != 1 || got[0].TaskNoticeKind != envelope.TaskNoticeBriefing ||
		got[0].Body != "Morning briefing. Small asks answered since last briefing: 1." {
		t.Fatalf("briefing = %+v", got)
	}
	m.clk.advance(24 * time.Hour)
	tomorrow := m.clk.now().Format(time.DateOnly)
	if queued, err := m.Store.QueueTaskBriefing(ctx, tomorrow, tomorrow); err != nil || queued {
		t.Fatalf("briefing repeated quiet count = %v, %v", queued, err)
	}
}

func TestQuietAskEscalates(t *testing.T) {
	for _, name := range []string{"failed", "declined", "denied", "expired", "stalled clarification"} {
		t.Run(name, func(t *testing.T) {
			m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off", NotesRequestTTL: 6 * time.Hour})
			m.host(t, "scout")
			ctx := t.Context()
			if name == "denied" {
				m.gate(t, `{"gate":{"muse":{"from":["grokbot"]}},"hold_ttl":"1h"}`)
			}
			if err := m.admin.SetKind(ctx, "muse", "notes"); err != nil {
				t.Fatal(err)
			}
			req, err := m.grok.Send(ctx, "muse", "Notes lookup", envelope.KindAsk, "", false)
			if err != nil || req.TaskID == 0 || !m.task(t, req.TaskID).Quiet {
				t.Fatalf("quiet ask = %+v, %v", req, err)
			}
			want := envelope.TaskOpen
			switch name {
			case "denied":
				if err := m.admin.Raw(ctx, http.MethodPost, "/v1/admin/requests/"+req.ID+"/deny", nil, nil); err != nil {
					t.Fatal(err)
				}
			case "expired":
				m.clk.advance(7 * time.Hour)
			case "stalled clarification":
				claimOne(t, m.muse, req.ID)
				if _, err := m.muse.Reply(ctx, req.ID, "Which chat?", envelope.StatusNeedsInput); err != nil {
					t.Fatal(err)
				}
				m.clk.advance(4*time.Hour - time.Second)
				m.Server.Sweep(ctx)
				if task := m.task(t, req.TaskID); !task.Quiet || task.Status != envelope.TaskQuestion {
					t.Fatalf("before cutoff = %+v", task)
				}
				if inbox := read(t, m.scout); len(inbox) != 0 {
					t.Fatalf("early question notice = %+v", inbox)
				}
				m.clk.advance(time.Second)
				want = envelope.TaskQuestion
			default:
				if _, err := m.muse.Reply(ctx, req.ID, "private reason", envelope.Status(name)); err != nil {
					t.Fatal(err)
				}
			}
			m.Server.Sweep(ctx)
			m.Server.Sweep(ctx)
			tasks, _, _, err := client.HostTasks(ctx, m.scout, false)
			if err != nil || len(tasks) != 1 || tasks[0].ID != req.TaskID || tasks[0].Quiet || tasks[0].Status != want {
				t.Fatalf("escalated list = %+v, %v", tasks, err)
			}
			got := read(t, m.scout)
			if len(got) != 1 || got[0].TaskNoticeID != req.TaskID || got[0].TaskNoticeStatus != want {
				t.Fatalf("escalated notices = %+v", got)
			}
			m.wantNoTaskText(t, "Notes lookup", "private reason", "Which chat?")
			day := m.clk.now().Format(time.DateOnly)
			if _, err := m.Store.QueueTaskBriefing(ctx, day, day); err != nil {
				t.Fatal(err)
			}
			rows, err := m.Store.TaskNotices(ctx, 10)
			if err != nil || len(rows) != 1 || rows[0].Briefing == nil ||
				len(rows[0].Briefing.Waiting) != 1 || rows[0].Briefing.Waiting[0].ID != req.TaskID {
				t.Fatalf("escalated briefing = %+v, %v", rows, err)
			}
			if name == "stalled clarification" {
				res, err := m.grok.Get(ctx, req.ID, 0)
				if err != nil || res.Status != envelope.StatusNeedsInput {
					t.Fatalf("quiet escalation closed the original request: %+v, %v", res, err)
				}
			}
		})
	}
}

func TestScheduledAskAndFollowOnFileQuietTasks(t *testing.T) {
	m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off"})
	m.host(t, "scout")
	m.create(t, m.grok, m.clk.now().Add(time.Minute), 0)
	m.clk.advance(time.Minute)
	m.Server.Sweep(t.Context())
	scheduled := m.sentTo(t, "muse")
	if len(scheduled) != 1 || scheduled[0].TaskID == 0 || !m.task(t, scheduled[0].TaskID).Quiet {
		t.Fatalf("scheduled asks = %+v", scheduled)
	}
	ctx := t.Context()
	res, err := m.grok.AskThen(ctx, "muse", "Find the receipt", "", envelope.Then{To: "instinct", Body: "File the receipt"}, 0, false)
	if err != nil || res.Request.TaskID == 0 || m.task(t, res.Request.TaskID).Quiet {
		t.Fatalf("first ask = %+v, %v", res, err)
	}
	if _, err := m.muse.Reply(ctx, res.Request.ID, "receipt 42", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	followOn := m.sentTo(t, "instinct")
	if len(followOn) != 1 || followOn[0].TaskID == 0 || !m.task(t, followOn[0].TaskID).Quiet {
		t.Fatalf("follow-on asks = %+v", followOn)
	}
}

func TestRoomAskTasksKeepRoomAccess(t *testing.T) {
	m := roomMesh(t)
	ctx := t.Context()
	admin, poster, target, host := m.Client(t, "admin"), m.Client(t, "grokbot"), m.Client(t, "muse"), m.Client(t, "instinct")
	if _, err := admin.SetTaskHost(ctx, "instinct", true); err != nil {
		t.Fatal(err)
	}
	const message = "private room question"
	post, err := client.PostToRoom(ctx, poster, "launch", "@muse "+message)
	if err != nil || len(post.Post.Mentions) != 1 {
		t.Fatalf("post = %+v, %v", post, err)
	}
	req := claimOne(t, target, post.Post.Mentions[0].RequestID)
	if req.TaskID == 0 {
		t.Fatal("room mention has no task")
	}
	if _, err := target.Reply(ctx, req.ID, "private room reply", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	withheld := func() {
		t.Helper()
		task, history, err := client.HostTask(ctx, host, req.TaskID)
		if err != nil || !task.Quiet || !task.Withheld || task.Title != "" || task.Detail != "" || task.Result != "" || len(history) != 0 {
			t.Fatalf("room task visible outside room = %+v, %+v, %v", task, history, err)
		}
	}
	withheld()
	if _, err := client.AddRoomMember(ctx, admin, "launch", "instinct"); err != nil {
		t.Fatal(err)
	}
	task, _, err := client.HostTask(ctx, host, req.TaskID)
	if err != nil || task.Withheld || !strings.Contains(task.Detail, message) || task.Result != "private room reply" {
		t.Fatalf("room task for member = %+v, %v", task, err)
	}
	if _, err := client.AssignTask(ctx, host, false, task.ID, "muse"); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("assignment would forward room text: %v", err)
	}
	if _, err := client.RemoveRoomMember(ctx, admin, "launch", "instinct"); err != nil {
		t.Fatal(err)
	}
	withheld()
}

func TestPrivatePassSeedFilesNothing(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "scout")
	ctx := t.Context()
	req, err := m.grok.SendGrouped(ctx, "muse", "private packed chat", envelope.KindAsk, "", "pass-test")
	if err != nil || req.TaskID != 0 {
		t.Fatalf("pass seed = %+v, %v", req, err)
	}
	if _, err := m.scout.Get(ctx, req.ID, 0); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("unrelated host can read seed: %v", err)
	}
	if _, err := m.muse.Reply(ctx, req.ID, "seed failed", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	tasks, _, _, err := client.HostTasks(ctx, m.scout, true, true)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("host tasks = %+v, %v", tasks, err)
	}
}

func TestWorkAskWithTwoClaimsUsesOnlyVerifiedParent(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "scout")
	m.muse.SetSession("same-session")
	ctx := t.Context()
	first := m.add(t, envelope.Task{Title: "first", Filer: "grokbot"})
	second := m.add(t, envelope.Task{Title: "second", Filer: "grokbot"})
	firstAsk := m.assign(t, first.ID, "grokbot", "muse")
	claimOne(t, m.muse, firstAsk.ID)
	secondAsk := m.assign(t, second.ID, "grokbot", "muse")
	claimOne(t, m.muse, secondAsk.ID)
	firstEvents, secondEvents := len(m.events(t, first.ID)), len(m.events(t, second.ID))
	unrelated, err := m.muse.Send(ctx, "instinct", "Separate ask", envelope.KindAsk, "", false)
	if err != nil || unrelated.TaskID == 0 || unrelated.ParentID != "" {
		t.Fatalf("unrelated ask = %+v, %v", unrelated, err)
	}
	child, err := m.muse.Send(ctx, "instinct", "Follow this task", envelope.KindAsk, firstAsk.ID, false)
	if err != nil || child.TaskID != 0 || child.ParentID != firstAsk.ID {
		t.Fatalf("verified child = %+v, %v", child, err)
	}
	if history := m.events(t, first.ID); len(history) != firstEvents+1 || history[len(history)-1].Body != "muse asked instinct: Follow this task" {
		t.Fatalf("first history = %+v", history)
	}
	if history := m.events(t, second.ID); len(history) != secondEvents {
		t.Fatalf("second history = %+v", history)
	}
}
