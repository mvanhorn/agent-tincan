package relay_test

import (
	"fmt"
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
			if tc.want == envelope.TaskOpen {
				if len(notice) != 1 || notice[0].TaskNoticeKind != envelope.TaskNoticeAction || strings.Contains(notice[0].Body, "unassigned") {
					t.Fatalf("auto-filed failure did not give the host an action: %+v", notice)
				}
			} else if len(notice) != 1 || notice[0].TaskNoticeID != task.ID || notice[0].TaskNoticeStatus != tc.want ||
				!strings.Contains(client.TaskNoticeOwnerMessage(notice[0]), "Check the line") {
				t.Fatalf("host inbox = %+v", notice)
			}
			m.wantNoTaskText(t, "private request detail", reply)
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
	if tasks, _, _, err := client.HostTasks(ctx, m.scout, true, true); err != nil || len(tasks) != 1 ||
		tasks[0].ID != req.TaskID || !tasks[0].Quiet {
		t.Fatalf("quiet list = %+v, %v", tasks, err)
	}
	task, history, err := client.HostTask(ctx, m.scout, req.TaskID)
	if err != nil || !task.Quiet || task.Status != envelope.TaskDone || !task.Withheld || task.Title != "" || task.Detail != "" || task.Result != "" || len(history) != 0 {
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
		client.TaskNoticeOwnerMessage(got[0]) != "Morning briefing. Small asks answered since last briefing: 1." {
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
			if want == envelope.TaskOpen {
				if name == "denied" && len(got) != 0 || name != "denied" && (len(got) != 1 || got[0].TaskNoticeKind != envelope.TaskNoticeAction) {
					t.Fatalf("escalated notices = %+v", got)
				}
			} else if len(got) != 1 || got[0].TaskNoticeID != req.TaskID || got[0].TaskNoticeStatus != want {
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
	second := m.add(t, envelope.Task{Title: "second", Filer: "scout"})
	firstAsk := m.assign(t, first.ID, "grokbot", "muse")
	claimOne(t, m.muse, firstAsk.ID)
	secondAsk := m.assign(t, second.ID, "scout", "muse")
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
	m.gate(t, `{"gate":{"instinct":{"from":["scout"]}},"hold_ttl":"1h"}`)
	held, err := m.muse.Send(ctx, "instinct", "Separate ask still sees the approval gate", envelope.KindAsk, "", false)
	if err != nil || held.Status != envelope.StatusHeld || held.TaskID == 0 || held.ParentID != "" {
		t.Fatalf("ambiguous ask under approval rules = %+v, %v", held, err)
	}
	if len(m.events(t, first.ID)) != firstEvents+1 || len(m.events(t, second.ID)) != secondEvents {
		t.Fatal("held ambiguous ask changed a claimed task's history")
	}
}

func TestRoomTaskScopeSurvivesAmbiguousClaims(t *testing.T) {
	m := roomMesh(t)
	ctx := t.Context()
	admin, poster, asker, outsider := m.Client(t, "admin"), m.Client(t, "grokbot"), m.Client(t, "muse"), m.Client(t, "instinct")
	for _, name := range []string{"grokbot", "instinct"} {
		if _, err := admin.SetTaskHost(ctx, name, true); err != nil {
			t.Fatal(err)
		}
	}
	asker.SetSession("same-session")
	post, err := client.PostToRoom(ctx, poster, "launch", "@muse private room context")
	if err != nil || len(post.Post.Mentions) != 1 {
		t.Fatalf("room post = %+v, %v", post, err)
	}
	room := claimOne(t, asker, post.Post.Mentions[0].RequestID)
	other, err := outsider.Send(ctx, "muse", "Unrelated work", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	claimOne(t, asker, other.ID)
	// The two open claims make the parent ambiguous, but both still
	// contribute to the sensitivity of this ask's text.
	const body = "Follow up on the private room context"
	sent, err := asker.Send(ctx, "grokbot", body, envelope.KindAsk, "", false)
	if err != nil || sent.ParentID != "" || sent.TaskID == 0 || sent.TaskID == room.TaskID || sent.TaskID == other.TaskID {
		t.Fatalf("separate ask = %+v, %v", sent, err)
	}
	got, history, err := client.HostTask(ctx, outsider, sent.TaskID)
	if err != nil || !got.Withheld || got.Title != "" || got.Detail != "" || len(history) != 0 {
		t.Fatalf("outside host reads %+v, history %+v, %v", got, history, err)
	}
	for _, c := range []*client.Relay{poster, admin} {
		var got envelope.Task
		var err error
		if c == admin {
			got, _, err = client.Task(ctx, c, true, sent.TaskID)
		} else {
			got, _, err = client.HostTask(ctx, c, sent.TaskID)
		}
		if err != nil || got.Withheld || got.Detail != body {
			t.Fatalf("authorized task = %+v, %v", got, err)
		}
	}
	if _, err := client.CreateRoom(ctx, admin, "other"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"muse", "instinct"} {
		if _, err := client.AddRoomMember(ctx, admin, "other", name); err != nil {
			t.Fatal(err)
		}
	}
	secondPost, err := client.PostToRoom(ctx, outsider, "other", "@muse another private room")
	if err != nil || len(secondPost.Post.Mentions) != 1 {
		t.Fatalf("second room post = %+v, %v", secondPost, err)
	}
	claimOne(t, asker, secondPost.Post.Mentions[0].RequestID)
	multi, err := asker.Send(ctx, "grokbot", "Both rooms' context", envelope.KindAsk, "", false)
	if err != nil || multi.TaskID == 0 {
		t.Fatalf("multi-room ask = %+v, %v", multi, err)
	}
	got, _, err = client.HostTask(ctx, outsider, multi.TaskID)
	if err != nil || !got.Withheld {
		t.Fatalf("host from only one room reads multi-room task = %+v, %v", got, err)
	}
	got, _, err = client.HostTask(ctx, poster, multi.TaskID)
	if err != nil || got.Withheld || got.Detail != "Both rooms' context" {
		t.Fatalf("original recipient cannot read multi-room task = %+v, %v", got, err)
	}

}

func TestReferencedAskReturnsAsTaskComment(t *testing.T) {
	m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off"})
	m.host(t, "scout")
	ctx := t.Context()
	parent, err := m.grok.Send(ctx, "muse", "Call about opening hours", envelope.KindAsk, "", false)
	if err != nil || parent.TaskID == 0 {
		t.Fatalf("parent = %+v, %v", parent, err)
	}
	claimOne(t, m.muse, parent.ID)
	follow, err := m.grok.Send(ctx, "muse", "Check the status of ask "+parent.ID+".", envelope.KindAsk, "", false)
	if err != nil || follow.TaskID != 0 {
		t.Fatalf("follow-up = %+v, %v", follow, err)
	}
	if _, err := m.muse.Reply(ctx, follow.ID, "Still waiting on the call.", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	m.wantNotices(t, "scout", 0)
	got := m.task(t, parent.TaskID)
	if got.Status != envelope.TaskWorking || got.CurrentRequest != parent.ID || got.Result != "" {
		t.Fatalf("parent after follow-up = %+v", got)
	}
	history := m.events(t, parent.TaskID)
	if last := history[len(history)-1]; last.Kind != envelope.TaskEventComment || !strings.Contains(last.Body, "Still waiting on the call.") {
		t.Fatalf("follow-up answer missing from history: %+v", history)
	}
	if _, err := m.muse.Reply(ctx, parent.ID, "Open Tuesday.", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if got := m.task(t, parent.TaskID); got.Status != envelope.TaskReturned || got.Result != "Open Tuesday." {
		t.Fatalf("returned parent = %+v", got)
	}
	m.wantNotices(t, "scout", 1)
}

func TestCancelUntouchedAutoFiledAskDropsTaskQuietly(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("comment=%t", active), func(t *testing.T) {
			m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off"})
			m.host(t, "scout")
			ctx := t.Context()
			req, err := m.grok.Send(ctx, "muse", "Work to cancel", envelope.KindAsk, "", false)
			if err != nil || req.TaskID == 0 {
				t.Fatalf("send = %+v, %v", req, err)
			}
			want := envelope.TaskDropped
			if active {
				want = envelope.TaskOpen
				if _, err := m.Store.AppendTaskEvent(ctx, envelope.TaskEvent{TaskID: req.TaskID, Actor: "grokbot", Kind: envelope.TaskEventComment, Body: "Keep this"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.grok.Cancel(ctx, req.ID); err != nil {
				t.Fatal(err)
			}
			task := m.task(t, req.TaskID)
			history := m.events(t, req.TaskID)
			if task.Status != want || task.Assignee != "" || task.CurrentRequest != "" || history[len(history)-1].Status != want {
				t.Fatalf("cancelled task = %+v, history = %+v", task, history)
			}
			m.Server.Sweep(ctx)
			m.wantNotices(t, "scout", 0)
		})
	}
}

func TestWorkingTaskStallsOnlyAfterRepeatedProgress(t *testing.T) {
	m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off"})
	m.host(t, "scout")
	ctx := t.Context()
	req, err := m.grok.Send(ctx, "muse", "Check opening hours", envelope.KindAsk, "", false)
	if err != nil || req.TaskID == 0 {
		t.Fatalf("send = %+v, %v", req, err)
	}
	claimOne(t, m.muse, req.ID)
	for i := range 17 {
		note := "Waiting for the first call"
		if i >= 5 {
			note = "First call failed. Waiting for the second call"
		}
		note += " as of " + m.clk.now().Format("2006-01-02T15:04:05Z07:00")
		if err := m.muse.Progress(ctx, req.ID, note); err != nil {
			t.Fatal(err)
		}
		m.Server.Sweep(ctx)
		if i < 11 {
			m.wantNotices(t, "scout", 0)
		}
		var nudges int
		if err := m.Store.DB().QueryRow("SELECT COUNT(*) FROM requests WHERE notice_kind = 'task-nudge' AND to_agent = 'muse'").Scan(&nudges); err != nil {
			t.Fatal(err)
		}
		wantNudges := 0
		if i >= 3 {
			wantNudges = 1
		}
		if i >= 8 {
			wantNudges = 2
		}
		if nudges != wantNudges {
			t.Fatalf("after %d minutes, got %d nudges, want %d", i*5, nudges, wantNudges)
		}
		m.clk.advance(5 * time.Minute)
	}
	res, err := m.grok.Get(ctx, req.ID, 0)
	if err != nil || res.Status != envelope.StatusClaimed || res.RelayNote != nil {
		t.Fatalf("request after progress: %+v, %v", res, err)
	}
	if task := m.task(t, req.TaskID); task.Status != envelope.TaskWorking || task.CurrentRequest != req.ID {
		t.Fatalf("working task = %+v", task)
	}
	m.wantNotices(t, "scout", 1)
	in := read(t, m.scout)
	if len(in) != 1 || in[0].TaskNoticeKind != envelope.TaskNoticeStall ||
		!strings.Contains(client.TaskNoticeOwnerMessage(in[0]), "Check opening hours: muse has reported no new progress for 30 min.") {
		t.Fatalf("stall inbox = %+v", in)
	}
	m.wantNoTaskText(t, "First call failed", "Waiting for the second call")
	if _, err := client.HostRetryTask(ctx, m.scout, req.TaskID, "instinct", "Use the published phone line"); err != nil {
		t.Fatalf("host could not act on the stall: %v", err)
	}
	if old, err := m.grok.Get(ctx, req.ID, 0); err != nil || old.Status != envelope.StatusCancelled {
		t.Fatalf("stalled attempt after retry = %+v, %v", old, err)
	}
}

func TestTaskReferencesDoNotAttachBareNumbersOrRestrictedText(t *testing.T) {
	m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off"})
	m.host(t, "scout")
	ctx := t.Context()
	parent, err := m.grok.Send(ctx, "muse", "Check the delivery", envelope.KindAsk, "", false)
	if err != nil || parent.TaskID == 0 {
		t.Fatalf("parent = %+v, %v", parent, err)
	}
	claimOne(t, m.muse, parent.ID)
	bare, err := m.muse.Send(ctx, "instinct", fmt.Sprintf("Review PR #%d", parent.TaskID), envelope.KindAsk, "", false)
	if err != nil || bare.TaskID == 0 || bare.TaskID == parent.TaskID {
		t.Fatalf("bare issue number was treated as a task: %+v, %v", bare, err)
	}
	follow, err := m.grok.Send(ctx, "muse", fmt.Sprintf("Check task #%d again", parent.TaskID), envelope.KindAsk, "", false)
	if err != nil || follow.TaskID != 0 {
		t.Fatalf("explicit task label did not attach: %+v, %v", follow, err)
	}

	linked, err := m.scout.Send(client.WithAskAbout(ctx, parent.ID), "muse", "Can you confirm the latest status?", envelope.KindAsk, "", false)
	if err != nil || linked.TaskID != 0 {
		t.Fatalf("eligible host could not attach to a task in its view: %+v, %v", linked, err)
	}

	before := len(m.events(t, parent.TaskID))
	outside := client.WithAskSource(client.WithAskAbout(ctx, parent.ID), "mail")
	const private = "Outside message text"
	if _, err := m.grok.Send(outside, "muse", private, envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	if after := m.events(t, parent.TaskID); len(after) != before {
		t.Fatalf("outside ask changed parent history: %+v", after)
	}
	tasks, err := m.Store.Tasks(ctx, "", 0, 10, false)
	if err != nil || len(tasks) != 3 {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	task, history, err := client.HostTask(ctx, m.scout, tasks[2].ID)
	if err != nil || !task.Withheld || task.Status != envelope.TaskProposed || task.Title != "" || len(history) != 0 {
		t.Fatalf("outside follow-up was not withheld: %+v, %+v, %v", task, history, err)
	}
	m.wantNoTaskText(t, private)
}

func TestWorkingTaskWithoutProgressNudgesHostAtConfiguredThresholds(t *testing.T) {
	m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off", TaskNudgeAfter: 4 * time.Minute, TaskStallAfter: 9 * time.Minute})
	m.host(t, "scout")
	m.host(t, "muse")
	ctx := t.Context()
	task := m.add(t, envelope.Task{Filer: "grokbot", Title: "Check the supplier"})
	req := m.assign(t, task.ID, "grokbot", "muse")
	if _, err := client.HostRetryTask(ctx, m.scout, task.ID, "instinct", "Try the dispatch desk"); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("retry before stall: %v", err)
	}
	m.clk.advance(4*time.Minute - time.Second)
	m.Server.Sweep(ctx)
	if got := read(t, m.scout); len(got) != 0 {
		t.Fatalf("early host notice: %+v", got)
	}
	var count int
	if err := m.Store.DB().QueryRow("SELECT COUNT(*) FROM requests WHERE notice_kind = 'task-nudge'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("early nudge count = %d, %v", count, err)
	}
	m.clk.advance(time.Second)
	m.Server.Sweep(ctx)
	var nudge string
	if err := m.Store.DB().QueryRow("SELECT body FROM requests WHERE notice_kind = 'task-nudge' AND to_agent = 'muse'").Scan(&nudge); err != nil || !strings.Contains(nudge, "for 4 min") {
		t.Fatalf("assignee nudge = %q, %v", nudge, err)
	}
	m.clk.advance(5 * time.Minute)
	m.Server.Sweep(ctx)
	in := read(t, m.scout)
	if len(in) != 1 || in[0].TaskNoticeKind != envelope.TaskNoticeStall || !strings.Contains(in[0].Body, "for 9 min") {
		t.Fatalf("host action = %+v", in)
	}
	assigneeNotice := read(t, m.muse)
	if len(assigneeNotice) != 1 || assigneeNotice[0].TaskNoticeKind != envelope.TaskNoticeStall {
		t.Fatalf("host assignee did not receive a stall notice: %+v", assigneeNotice)
	}
	for _, delivery := range []struct {
		host *client.Relay
		id   string
	}{{m.scout, in[0].ID}, {m.muse, assigneeNotice[0].ID}} {
		if _, err := delivery.host.Claim(ctx, delivery.id); err != nil {
			t.Fatal(err)
		}
		if err := client.TaskNoticesTold(ctx, delivery.host, []string{delivery.id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.muse.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.muse.Progress(ctx, req.ID, "Supplier picked up and is checking stock."); err != nil {
		t.Fatal(err)
	}
	if _, err := client.HostCompleteTask(ctx, m.scout, task.ID, "Supplier has the parts in stock."); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("fresh progress did not invalidate the stall: %v", err)
	}
	m.clk.advance(9 * time.Minute)
	m.Server.Sweep(ctx)
	if got := read(t, m.scout); len(got) != 1 || got[0].TaskNoticeKind != envelope.TaskNoticeStall {
		t.Fatalf("second stall notice = %+v", got)
	}
	m.wantNotices(t, "scout", 2)

	closed, err := client.HostCompleteTask(ctx, m.scout, task.ID, "Supplier has the parts in stock.")
	if err != nil || closed.Status != envelope.TaskDone || closed.Result != "Supplier has the parts in stock." {
		t.Fatalf("host completion = %+v, %v", closed, err)
	}
	answered, err := m.grok.Get(ctx, req.ID, 0)
	if err != nil || answered.Status != envelope.StatusAnswered || answered.Reply == nil || answered.Reply.From != "scout" || answered.Reply.Body != closed.Result {
		t.Fatalf("host-authored answer = %+v, %v", answered, err)
	}
	if err := m.Store.DB().QueryRow("SELECT COUNT(*) FROM requests WHERE notice_kind = 'task-nudge' AND status != 'cancelled'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("nudge survived closure: %d, %v", count, err)
	}
	if reopened, err := client.DecideTask(ctx, m.admin, client.TaskReopen, task.ID, "More info needed"); err != nil || reopened.Status != envelope.TaskOpen {
		t.Fatalf("owner could not reopen host completion: %+v, %v", reopened, err)
	}
}

func TestSweepClosesPreviouslyReturnedQuietAnswer(t *testing.T) {
	m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off"})
	m.host(t, "scout")
	ctx := t.Context()
	req, err := m.grok.Send(client.WithQuietAsk(ctx, true), "muse", "Confirm the version", envelope.KindAsk, "", false)
	if err != nil || req.TaskID == 0 {
		t.Fatalf("quiet ask = %+v, %v", req, err)
	}
	if _, err := m.muse.Reply(ctx, req.ID, "Version confirmed.", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.DB().Exec("UPDATE tasks SET status = 'returned' WHERE id = ?", req.TaskID); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	got := m.task(t, req.TaskID)
	if got.Status != envelope.TaskDone || got.Result != "Version confirmed." {
		t.Fatalf("legacy quiet result = %+v", got)
	}
	m.wantNotices(t, "scout", 0)
}

func TestRoomTaskNoticeHasNoTitleEvenForMemberHost(t *testing.T) {
	m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off", UnansweredQuestionAfter: time.Minute})
	m.host(t, "scout")
	ctx := t.Context()
	if _, err := client.CreateRoom(ctx, m.admin, "topic"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"grokbot", "muse", "scout"} {
		if _, err := client.AddRoomMember(ctx, m.admin, "topic", name); err != nil {
			t.Fatal(err)
		}
	}
	const topic = "Private room topic"
	post, err := client.PostToRoom(ctx, m.grok, "topic", "@muse "+topic)
	if err != nil || len(post.Post.Mentions) != 1 {
		t.Fatalf("room mention = %+v, %v", post, err)
	}
	req := claimOne(t, m.muse, post.Post.Mentions[0].RequestID)
	if _, err := m.muse.Reply(ctx, req.ID, "Private room question", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	m.clk.advance(time.Minute)
	m.Server.Sweep(ctx)
	want := read(t, m.scout)
	if len(want) != 1 || want[0].TaskNoticeID != req.TaskID {
		t.Fatalf("room notice = %+v", want)
	}
	m.wantNoTaskText(t, topic, "Private room question")
}

func TestTaskWithReferencedRoomTextIsWithheldFromOtherHosts(t *testing.T) {
	m := roomMesh(t)
	ctx := t.Context()
	admin, poster, muse, host := m.Client(t, "admin"), m.Client(t, "grokbot"), m.Client(t, "muse"), m.Client(t, "instinct")
	if _, err := admin.SetTaskHost(ctx, "instinct", true); err != nil {
		t.Fatal(err)
	}
	post, err := client.PostToRoom(ctx, poster, "launch", "@muse Confirm a private room detail")
	if err != nil || len(post.Post.Mentions) != 1 {
		t.Fatalf("room mention = %+v, %v", post, err)
	}
	original := claimOne(t, muse, post.Post.Mentions[0].RequestID)
	req, err := poster.Send(ctx, "muse", "Private room detail from ask "+original.ID, envelope.KindAsk, "", false)
	if err != nil || req.TaskID == 0 || req.TaskID == original.TaskID {
		t.Fatalf("room follow-up was not filed separately: %+v, %v", req, err)
	}
	task, history, err := client.HostTask(ctx, host, req.TaskID)
	if err != nil || !task.Withheld || task.Title != "" || task.Detail != "" || len(history) != 0 {
		t.Fatalf("host saw private room context: %+v, %+v, %v", task, history, err)
	}
	if req, err := poster.Send(client.WithAskAbout(ctx, "task"), "muse", "Separate work", envelope.KindAsk, "", false); err != nil || req.TaskID == 0 {
		t.Fatalf("invalid reference should file a normal task: %+v, %v", req, err)
	}
}
