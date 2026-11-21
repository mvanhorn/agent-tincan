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
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func newBriefingMesh(t *testing.T, at time.Time, schedule string) (*taskMesh, relay.Config) {
	t.Helper()
	clk := &scheduleClock{t: at}
	cfg := relay.Config{Now: clk.now, TaskBriefing: schedule, TaskBriefingZone: time.UTC}
	m := testrelay.New(t, cfg)
	m.Store.SetClock(clk.now)
	return &taskMesh{scheduleMesh: &scheduleMesh{
		Mesh: m, clk: clk, grok: m.Client(t, "grokbot"), muse: m.Client(t, "muse"), admin: m.Client(t, "admin"),
	}, instinct: m.Client(t, "instinct")}, cfg
}

// takeBriefing acknowledges the other news in this poll and returns the
// briefing so the test can choose whether to acknowledge it.
func takeBriefing(t *testing.T, host *client.Relay) envelope.Request {
	t.Helper()
	var briefing envelope.Request
	for _, req := range read(t, host) {
		if _, err := host.Claim(t.Context(), req.ID); err != nil {
			t.Fatal(err)
		}
		if req.TaskNoticeKind == envelope.TaskNoticeBriefing {
			if briefing.ID != "" {
				t.Fatalf("received two briefings: %+v and %+v", briefing, req)
			}
			briefing = req
		} else if req.TaskNoticeReceipt {
			if err := client.TaskNoticesTold(t.Context(), host, []string{req.ID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if briefing.ID == "" {
		t.Fatal("no briefing was delivered")
	}
	return briefing
}

func taskDay(t *testing.T, m *taskMesh, day string) int64 {
	t.Helper()
	result, err := client.AddTask(t.Context(), m.admin, true, client.TaskAdd{Title: "Private title", Detail: "Private detail", Due: day})
	if err != nil {
		t.Fatal(err)
	}
	return result.Task.ID
}

func assertBriefing(t *testing.T, req envelope.Request, want ...string) string {
	t.Helper()
	message := client.TaskNoticeOwnerMessage(req)
	if message == "" || !req.TaskNoticeReceipt || strings.Contains(req.Body, "Private") || strings.Contains(message, "Private") {
		t.Fatalf("briefing metadata = %+v, message = %q", req, message)
	}
	for _, part := range want {
		if !strings.Contains(message, part) {
			t.Fatalf("briefing %q lacks %q", message, part)
		}
	}
	return message
}

func TestTaskBriefingSectionsAndRestart(t *testing.T) {
	start := time.Date(2026, 11, 21, 14, 58, 0, 0, time.UTC)
	m, cfg := newBriefingMesh(t, start, "15:00")
	m.host(t, "muse")
	ctx := t.Context()
	today := taskDay(t, m, "2026-11-21")
	oldest := taskDay(t, m, "2026-11-18")
	middle := taskDay(t, m, "2026-11-19")
	newest := taskDay(t, m, "2026-11-20")
	returned := m.returnedTask(t, "Private returned title")
	proposed := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Private outside title", "source": "mail"}).Task.ID
	question := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Private question title", "assignee": "instinct"}).Task.ID
	m.finish(t, m.instinct, question, "Private question", envelope.StatusNeedsInput)
	done := taskDay(t, m, "")
	if _, err := client.DecideTask(ctx, m.admin, client.TaskDone, done, "Private comment"); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(ctx)
	for _, n := range m.notices(t, "muse") {
		if strings.Contains(n.Body, "Morning briefing.") {
			t.Fatalf("briefing before its configured time: %+v", n)
		}
	}

	m.clk.advance(2 * time.Minute)
	m.Server.Sweep(ctx)
	b := takeBriefing(t, m.muse)
	message := assertBriefing(t, b,
		fmt.Sprintf("Due today or overdue: task #%d (open, due 2026-11-18), task #%d (open, due 2026-11-19), task #%d (open, due 2026-11-20).", oldest, middle, newest),
		fmt.Sprintf("Suggested today: task #%d.", oldest),
		fmt.Sprintf("task #%d (returned", returned),
		fmt.Sprintf("task #%d (proposed", proposed),
		fmt.Sprintf("task #%d (question", question),
		fmt.Sprintf("Done since the last briefing: task #%d (done", done),
	)
	if strings.Contains(message, fmt.Sprintf("task #%d (", today)) {
		t.Fatalf("fourth due task in briefing: %q", message)
	}
	if err := client.TaskNoticesTold(ctx, m.muse, []string{b.ID}); err != nil {
		t.Fatal(err)
	}
	restarted := relay.New(m.Dir, m.Store, cfg)
	restarted.Sweep(ctx)
	if got := read(t, m.muse); len(got) != 0 {
		t.Fatalf("restart delivered a second briefing: %+v", got)
	}
}

func TestTaskBriefingOffEmptyAndOnDemand(t *testing.T) {
	start := time.Date(2026, 11, 21, 14, 58, 0, 0, time.UTC)
	m, cfg := newBriefingMesh(t, start, "off")
	ctx := t.Context()
	m.host(t, "muse")
	if queued, err := client.TaskBriefingNow(ctx, m.admin); err != nil || queued {
		t.Fatalf("empty manual briefing = %v, %v", queued, err)
	}
	if _, err := client.TaskBriefingNow(ctx, m.muse); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("manual briefing as an agent = %v", err)
	}

	cfg.TaskBriefing = "15:00"
	m.clk.advance(2 * time.Minute)
	relay.New(m.Dir, m.Store, cfg).Sweep(ctx)
	m.wantNotices(t, "muse", 0)
	m.clk.advance(23*time.Hour + 59*time.Minute)

	oldReturned := m.returnedTask(t, "Private first return")
	m.returnedTask(t, "Private second return")
	done := taskDay(t, m, "")
	if _, err := client.DecideTask(ctx, m.admin, client.TaskDone, done, ""); err != nil {
		t.Fatal(err)
	}
	m.clk.advance(time.Minute)
	m.Server.Sweep(ctx) // Automatic delivery is off even with news to report.
	m.wantNotices(t, "muse", 2)
	if queued, err := client.TaskBriefingNow(ctx, m.admin); err != nil || !queued {
		t.Fatalf("manual briefing = %v, %v", queued, err)
	}
	missedTold := takeBriefing(t, m.muse)
	assertBriefing(t, missedTold,
		fmt.Sprintf("Suggested today: task #%d.", oldReturned),
		fmt.Sprintf("Done since the last briefing: task #%d (done", done))
	if err := client.TaskNoticesTold(ctx, m.muse, []string{missedTold.ID}); err != nil {
		t.Fatal(err)
	}

	firstDue := taskDay(t, m, "2026-11-22 9am")
	taskDay(t, m, "2026-11-22 3pm")
	relay.New(m.Dir, m.Store, cfg).Sweep(ctx)
	daily := takeBriefing(t, m.muse)
	assertBriefing(t, daily,
		fmt.Sprintf("Done since the last briefing: task #%d (done", done),
		fmt.Sprintf("Suggested today: task #%d.", firstDue))
	if err := client.TaskNoticesTold(ctx, m.muse, []string{daily.ID}); err != nil {
		t.Fatal(err)
	}
	m.clk.advance(24 * time.Hour)
	relay.New(m.Dir, m.Store, cfg).Sweep(ctx)
	nextDay := takeBriefing(t, m.muse)
	if message := client.TaskNoticeOwnerMessage(nextDay); strings.Contains(message, "Done since") {
		t.Fatalf("previously briefed done task repeated: %q", message)
	}
}

func TestTaskBriefingIncludesYesterdaysUnconfirmedReminder(t *testing.T) {
	start := time.Date(2026, 11, 21, 6, 50, 0, 0, time.UTC) // Yesterday in the task zone.
	m, cfg := newBriefingMesh(t, start, "off")
	m.host(t, "muse")
	ctx := t.Context()
	due := taskDay(t, m, start.Format(time.RFC3339))
	m.Server.Sweep(ctx)
	reminders := read(t, m.muse)
	if len(reminders) != 1 || reminders[0].TaskNoticeKind != envelope.TaskNoticeReminder {
		t.Fatalf("reminders = %+v", reminders)
	}
	if _, err := m.muse.Claim(ctx, reminders[0].ID); err != nil {
		t.Fatal(err)
	}
	m.clk.advance(8*time.Hour + 10*time.Minute)
	cfg.TaskBriefing = "15:00"
	relay.New(m.Dir, m.Store, cfg).Sweep(ctx)
	briefing := takeBriefing(t, m.muse)
	assertBriefing(t, briefing, fmt.Sprintf("Unconfirmed task news: task #%d (reminder)", due))
}

func TestTaskBriefingFiltersUnconfirmedNotices(t *testing.T) {
	for _, tc := range []struct {
		name       string
		decision   string
		ack        bool
		clearHost  bool
		openNotice bool
		wantMissed bool
	}{
		{name: "done", decision: client.TaskDone},
		{name: "dropped", decision: client.TaskDrop},
		{name: "one host confirmed", ack: true},
		{name: "host mark cleared", clearHost: true},
		{name: "still unconfirmed", wantMissed: true},
		{name: "task reopened", openNotice: true, wantMissed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
			m, _ := newBriefingMesh(t, start, "off")
			m.host(t, "muse")
			if tc.ack {
				m.host(t, "grokbot")
			}
			ctx := t.Context()
			var id int64
			switch {
			case tc.decision != "":
				id = m.returnedTask(t, "Private returned title")
			case tc.openNotice:
				id = taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Private title", "assignee": "instinct"}).Task.ID
				m.finish(t, m.instinct, id, "Private reply", envelope.StatusFailed)
			default:
				id = taskDay(t, m, start.Format(time.RFC3339))
				m.Server.Sweep(ctx)
			}
			notices := read(t, m.muse)
			if len(notices) != 1 || notices[0].TaskNoticeID != id {
				t.Fatalf("notices = %+v", notices)
			}
			if _, err := m.muse.Claim(ctx, notices[0].ID); err != nil {
				t.Fatal(err)
			}
			if tc.ack {
				reminders := read(t, m.grok)
				if len(reminders) != 1 || reminders[0].TaskNoticeKind != envelope.TaskNoticeReminder {
					t.Fatalf("reminders = %+v", reminders)
				}
				if _, err := m.grok.Claim(ctx, reminders[0].ID); err != nil {
					t.Fatal(err)
				}
				m.clk.advance(relay.DefaultTaskNoticeAckTimeout)
				m.Server.Sweep(ctx)
				retries := read(t, m.grok)
				if len(retries) != 1 || retries[0].ID == reminders[0].ID || retries[0].TaskNoticeID != id {
					t.Fatalf("reminder retry = %+v", retries)
				}
				if _, err := m.grok.Claim(ctx, retries[0].ID); err != nil {
					t.Fatal(err)
				}
				if err := client.TaskNoticesTold(ctx, m.grok, []string{retries[0].ID}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.decision != "" {
				if _, err := client.DecideTask(ctx, m.admin, tc.decision, id, "Private decision"); err != nil {
					t.Fatal(err)
				}
			}
			recipient := m.muse
			if tc.clearHost {
				m.host(t, "grokbot")
				if ok, err := m.Store.SetAgentTaskHost(ctx, "muse", false); err != nil || !ok {
					t.Fatalf("clear task host = %v, %v", ok, err)
				}
				recipient = m.grok
			}
			taskDay(t, m, "2026-11-21") // Keep the briefing nonempty independently of the reminder.
			if queued, err := client.TaskBriefingNow(ctx, m.admin); err != nil || !queued {
				t.Fatalf("queue briefing = %v, %v", queued, err)
			}
			message := assertBriefing(t, takeBriefing(t, recipient), "Morning briefing.")
			want := fmt.Sprintf("Unconfirmed task news: task #%d (reminder).", id)
			if tc.openNotice {
				want = fmt.Sprintf("Unconfirmed task news: task #%d (open).", id)
			}
			if tc.wantMissed && !strings.Contains(message, want) ||
				!tc.wantMissed && strings.Contains(message, "Unconfirmed task news:") {
				t.Fatalf("want unconfirmed = %v, briefing = %q", tc.wantMissed, message)
			}
		})
	}
}

func TestTaskBriefingSupersedesStagedBriefing(t *testing.T) {
	m, _ := newBriefingMesh(t, time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC), "off")
	m.host(t, "muse")
	taskDay(t, m, "2026-11-21")
	ctx := t.Context()
	if queued, err := m.Store.QueueTaskBriefing(ctx, "", "2026-11-21"); err != nil || !queued {
		t.Fatalf("stage briefing = %v, %v", queued, err)
	}
	if queued, err := client.TaskBriefingNow(ctx, m.admin); err != nil || !queued {
		t.Fatalf("queue replacement briefing = %v, %v", queued, err)
	}
	assertBriefing(t, takeBriefing(t, m.muse), "Morning briefing.")
}

func TestTaskBriefingRecognizesReceiptsFromBeforeNoticeIDs(t *testing.T) {
	start := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	m, _ := newBriefingMesh(t, start, "off")
	m.host(t, "muse")
	m.host(t, "grokbot")
	ctx := t.Context()
	taskDay(t, m, start.Format(time.RFC3339))
	m.Server.Sweep(ctx)
	for _, host := range []*client.Relay{m.grok, m.muse} {
		notices := read(t, host)
		if len(notices) != 1 || notices[0].TaskNoticeKind != envelope.TaskNoticeReminder {
			t.Fatalf("reminders = %+v", notices)
		}
		if _, err := host.Claim(ctx, notices[0].ID); err != nil {
			t.Fatal(err)
		}
		if host == m.grok {
			if err := client.TaskNoticesTold(ctx, host, []string{notices[0].ID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := m.Store.DB().ExecContext(ctx, `UPDATE task_notice_delivery SET notice_id = NULL`); err != nil {
		t.Fatal(err)
	}
	// A past receipt still confirms the matching notice after its host is cleared.
	if ok, err := m.Store.SetAgentTaskHost(ctx, "grokbot", false); err != nil || !ok {
		t.Fatalf("clear task host = %v, %v", ok, err)
	}
	if queued, err := client.TaskBriefingNow(ctx, m.admin); err != nil || !queued {
		t.Fatalf("queue briefing = %v, %v", queued, err)
	}
	if message := assertBriefing(t, takeBriefing(t, m.muse), "Morning briefing."); strings.Contains(message, "Unconfirmed task news") {
		t.Fatalf("confirmed reminder repeated: %q", message)
	}
}

func TestTaskBriefingSupersedesOutstandingBriefings(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		name := "cancelled before delivery"
		if claimed {
			name = "claimed without a receipt"
		}
		t.Run(name, func(t *testing.T) {
			m, _ := newBriefingMesh(t, time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC), "off")
			m.host(t, "muse")
			m.gate(t, `{"gate":{},"notify":"instinct"}`)
			taskDay(t, m, "2026-11-21")
			ctx := t.Context()
			queue := func() {
				t.Helper()
				if queued, err := client.TaskBriefingNow(ctx, m.admin); err != nil || !queued {
					t.Fatalf("queue briefing = %v, %v", queued, err)
				}
			}

			queue()
			if claimed {
				takeBriefing(t, m.muse)
			} else {
				// Remove and restore the host mark to cancel the briefing
				// while it is still queued.
				if ok, err := m.Store.SetAgentTaskHost(ctx, "muse", false); err != nil || !ok {
					t.Fatalf("clear task host = %v, %v", ok, err)
				}
				m.host(t, "muse")
			}

			queue()
			latest := takeBriefing(t, m.muse)
			if message := assertBriefing(t, latest, "Morning briefing."); strings.Contains(message, "Unconfirmed task news") {
				t.Fatalf("old briefing repeated: %q", message)
			}
			if err := client.TaskNoticesTold(ctx, m.muse, []string{latest.ID}); err != nil {
				t.Fatal(err)
			}
			m.clk.advance(relay.DefaultTaskNoticeAckTimeout)
			m.Server.Sweep(ctx)
			if got := read(t, m.muse); len(got) != 0 {
				t.Fatalf("old briefing was retried: %+v", got)
			}
			if in, err := m.instinct.Poll(ctx, 0); err != nil || len(in.Requests) != 0 {
				t.Fatalf("fallback inbox = %+v, %v", in, err)
			}
		})
	}
}

func TestTaskBriefingRetriesAndFallsBack(t *testing.T) {
	start := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	m, _ := newBriefingMesh(t, start, "off")
	m.host(t, "muse")
	m.gate(t, `{"gate":{},"notify":"instinct"}`)
	ctx := t.Context()
	id := taskDay(t, m, "2026-11-21")
	queued, err := client.TaskBriefingNow(ctx, m.admin)
	if err != nil || !queued {
		t.Fatalf("queue briefing = %v, %v", queued, err)
	}
	var initial string
	for attempt := range 3 {
		req := takeBriefing(t, m.muse)
		message := assertBriefing(t, req, fmt.Sprintf("task #%d (open, due 2026-11-21)", id))
		if attempt == 0 {
			initial = message
		} else if message != "Re-send of the same task news: "+initial {
			t.Fatalf("retried briefing = %q", message)
		}
		m.clk.advance(relay.DefaultTaskNoticeAckTimeout)
		m.Server.Sweep(ctx)
	}
	in, err := m.instinct.Poll(ctx, 0)
	if err != nil || len(in.Requests) != 1 || !strings.Contains(in.Requests[0].Body, "Task briefing: muse did not acknowledge") ||
		!strings.Contains(in.Requests[0].Body, initial) || strings.Contains(in.Requests[0].Body, "Private") {
		t.Fatalf("fallback briefing inbox = %+v, %v", in, err)
	}
}

func TestTaskBriefingDefaultsToTheTaskZone(t *testing.T) {
	start := time.Date(2026, 11, 21, 6, 59, 0, 0, time.UTC)
	m, cfg := newBriefingMesh(t, start, "off")
	m.host(t, "muse")
	taskDay(t, m, "2026-11-21")
	cfg.TaskBriefing, cfg.TaskBriefingZone, cfg.TaskZone = "", nil, time.UTC
	srv := relay.New(m.Dir, m.Store, cfg)
	srv.Sweep(t.Context())
	m.wantNotices(t, "muse", 0)
	m.clk.advance(time.Minute)
	srv.Sweep(t.Context())
	assertBriefing(t, takeBriefing(t, m.muse), "Morning briefing.")
}

func TestTaskBriefingWithholdsProposedDeadlineUntilApproval(t *testing.T) {
	m, _ := newBriefingMesh(t, time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC), "off")
	m.host(t, "muse")
	ctx := t.Context()
	result, err := client.AddTask(ctx, m.grok, false, client.TaskAdd{
		Title: "Private outside title", Source: "mail", Due: "2001-02-03",
	})
	if err != nil || result.Task.Status != envelope.TaskProposed {
		t.Fatalf("file outside task = %+v, %v", result, err)
	}
	id := result.Task.ID
	if queued, err := client.TaskBriefingNow(ctx, m.admin); err != nil || !queued {
		t.Fatalf("queue briefing = %v, %v", queued, err)
	}
	req := takeBriefing(t, m.muse)
	message := assertBriefing(t, req, fmt.Sprintf("Waiting on you: task #%d (proposed)", id))
	if strings.Contains(message, "2001-02-03") || strings.Contains(message, "Suggested today:") || strings.Contains(message, "Due today or overdue:") {
		t.Fatalf("briefing exposed a withheld deadline: %q", message)
	}
	if err := client.TaskNoticesTold(ctx, m.muse, []string{req.ID}); err != nil {
		t.Fatal(err)
	}

	if _, err := client.DecideTask(ctx, m.admin, client.TaskApprove, id, ""); err != nil {
		t.Fatal(err)
	}
	if queued, err := client.TaskBriefingNow(ctx, m.admin); err != nil || !queued {
		t.Fatalf("queue briefing after approval = %v, %v", queued, err)
	}
	assertBriefing(t, takeBriefing(t, m.muse), fmt.Sprintf("Due today or overdue: task #%d (open, due 2001-02-03)", id))
}
