package relay_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

func TestReturnedTaskSweep(t *testing.T) {
	for _, scenario := range []string{"host closes", "host unavailable", "asker follows up", "owner asked", "worker returned"} {
		t.Run(scenario, func(t *testing.T) {
			const window = 10 * time.Minute
			m := newTaskMeshWithConfig(t, relay.Config{TaskBriefing: "off", TaskReturnedCloseAfter: window})
			m.host(t, "muse")
			ctx := t.Context()
			const title, result = "Confirm opening hours", "Open from 9 to 5."
			var id int64
			var requestID string
			if scenario == "owner asked" {
				phone := m.JoinOnMachineOf(t, "admin", "phone")
				m.clk.advance(2 * time.Second)
				task := taskDo(t, phone, "POST", "/v1/admin/tasks", map[string]string{"title": title, "assignee": "muse"}).Task
				id = task.ID
				m.finish(t, m.muse, id, result, envelope.StatusAnswered)
			} else {
				worker, recipient := m.muse, "muse"
				if scenario == "worker returned" {
					worker, recipient = m.instinct, "instinct"
				}
				req, err := m.scout.Send(ctx, recipient, title, envelope.KindAsk, "", false)
				if err != nil || req.TaskID == 0 {
					t.Fatalf("send = %+v, %v", req, err)
				}
				id = req.TaskID
				requestID = req.ID
				claimOne(t, worker, req.ID)
				if _, err := worker.Reply(ctx, req.ID, result, envelope.StatusAnswered); err != nil {
					t.Fatal(err)
				}
			}

			m.clk.advance(window / 2)
			m.Server.Sweep(ctx)
			wantNotices := 0
			if scenario == "worker returned" {
				wantNotices = 1 // The original result notice.
			}
			m.wantNotices(t, "muse", wantNotices)
			if got := m.task(t, id); got.Status != envelope.TaskReturned {
				t.Fatalf("task before window elapsed = %+v", got)
			}
			switch scenario {
			case "host unavailable":
				if ok, err := m.Store.SetAgentTaskHost(ctx, "muse", false); err != nil || !ok {
					t.Fatalf("clear host = %v, %v", ok, err)
				}
			case "asker follows up":
				req, err := m.scout.Send(client.WithAskAbout(ctx, requestID), "muse", "Do these hours apply on Saturday?", envelope.KindAsk, "", false)
				if err != nil || req.TaskID != 0 {
					t.Fatalf("follow-up = %+v, %v", req, err)
				}
				claimOne(t, m.muse, req.ID)
				if _, err := m.muse.Reply(ctx, req.ID, "Yes.", envelope.StatusAnswered); err != nil {
					t.Fatal(err)
				}
			}
			m.clk.advance(window / 2)
			m.Server.Sweep(ctx)
			if scenario == "host unavailable" {
				m.wantNotices(t, "muse", 0)
				if got := m.task(t, id); got.Status != envelope.TaskDone || got.Result != result {
					t.Fatalf("sweep completion = %+v", got)
				}
				if ev := m.lastEvent(t, id); ev.Body != "returned, asker did not follow up" {
					t.Fatalf("closure history = %+v", ev)
				}
				return
			}
			if got := m.task(t, id); got.Status != envelope.TaskReturned {
				t.Fatalf("task after window elapsed = %+v", got)
			}
			if scenario == "asker follows up" {
				m.wantNotices(t, "muse", 0)
				m.clk.advance(window / 2)
				m.Server.Sweep(ctx)
			}
			if scenario == "owner asked" || scenario == "worker returned" {
				m.wantNotices(t, "muse", wantNotices)
				return
			}
			m.Server.Sweep(ctx)
			m.wantNotices(t, "muse", 1)
			m.wantNoTaskText(t, title, result)
			in := read(t, m.muse)
			if len(in) != 1 || in[0].TaskNoticeID != id || in[0].TaskNoticeKind != envelope.TaskNoticeReturnedClose ||
				!strings.Contains(client.TaskNoticeOwnerMessage(in[0]), title+": The asker has not followed up.") ||
				!strings.Contains(in[0].Body, "decide_task done") {
				t.Fatalf("closure notice = %+v", in)
			}
			if task, err := client.HostCloseAnsweredTask(ctx, m.muse, id, "The answer confirms the hours."); err != nil || task.Status != envelope.TaskDone {
				t.Fatalf("host completion = %+v, %v", task, err)
			}
		})
	}
}
