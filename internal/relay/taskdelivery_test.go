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

func TestTaskNoticeReceiptsAreScopedAndAtomic(t *testing.T) {
	m := newTaskMesh(t)
	pilot := m.joinHost(t, "instinct", "pilot")
	other := m.joinHost(t, "grokbot", "other")
	pilot.SetSession("tincan-wake")
	ctx := t.Context()
	taskDo(t, m.muse, "POST", "/v1/tasks", map[string]string{"title": "Withheld", "source": "mail"})
	id := m.wantNotices(t, "pilot", 1)[0].ID

	if err := client.TaskNoticesTold(ctx, pilot, []string{id}); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("receipt before claim = %v", err)
	}
	if _, err := pilot.Claim(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := client.TaskNoticesTold(ctx, m.muse, []string{id}); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("receipt as a non-host = %v", err)
	}
	if err := client.TaskNoticesTold(ctx, other, []string{id}); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("receipt for another host's notice = %v", err)
	}
	if err := client.TaskNoticesTold(ctx, pilot, []string{id, "missing"}); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("receipt with an unknown notice = %v", err)
	}
	if got := m.audited(t, "task_notice_told"); len(got) != 0 {
		t.Fatalf("atomic receipt wrote partial audit: %v", got)
	}
	if got := m.audited(t, "claimed"); len(got) != 1 || !strings.Contains(got[0], `"session":"tincan-wake"`) {
		t.Fatalf("claim audit = %v", got)
	}
	for range 2 {
		if err := client.TaskNoticesTold(ctx, pilot, []string{id}); err != nil {
			t.Fatal(err)
		}
	}
	m.clk.advance(relay.DefaultTaskNoticeAckTimeout)
	m.Server.Sweep(ctx)
	m.wantNotices(t, "pilot", 1)
	if got := m.audited(t, "task_notice_told"); len(got) != 1 {
		t.Fatalf("receipt audit = %v", got)
	}
}

func TestTaskNoticeRetriesAndFallsBackAfterThirdAttempt(t *testing.T) {
	m := newTaskMesh(t)
	pilot := m.joinHost(t, "instinct", "pilot")
	m.gate(t, `{"gate":{},"notify":"scout"}`)
	wakes := &noticeWakes{}
	m.Server.SetEvents(wakes)
	ctx := t.Context()
	task := taskDo(t, m.muse, "POST", "/v1/tasks", map[string]string{"title": "Private title", "source": "mail"}).Task
	initial := m.wantNotice(t, "pilot", task.ID, "proposed")
	for attempt := 1; attempt <= 3; attempt++ {
		got := read(t, pilot)
		if len(got) != 1 || got[0].Body != initial.Body || !got[0].TaskNoticeReceipt {
			t.Fatalf("attempt %d inbox = %+v", attempt, got)
		}
		if _, err := pilot.Claim(ctx, got[0].ID); err != nil {
			t.Fatal(err)
		}
		m.clk.advance(relay.DefaultTaskNoticeAckTimeout - time.Second)
		m.Server.Sweep(ctx)
		m.wantNotices(t, "pilot", attempt)
		m.clk.advance(time.Second)
		// A second sweep cannot issue another attempt from the same claim.
		m.Server.Sweep(ctx)
		m.Server.Sweep(ctx)
		if err := client.TaskNoticesTold(ctx, pilot, []string{got[0].ID}); !client.IsStatus(err, http.StatusConflict) {
			t.Fatalf("receipt for settled attempt %d = %v", attempt, err)
		}
	}
	notices := m.wantNotices(t, "pilot", 3)
	if len(map[string]bool{notices[0].ID: true, notices[1].ID: true, notices[2].ID: true}) != 3 || wakes.count("pilot") != 3 {
		t.Fatalf("attempts did not receive new IDs and wakes: %+v", notices)
	}
	in, err := m.scout.Poll(ctx, 0)
	if err != nil || len(in.Requests) != 1 {
		t.Fatalf("fallback inbox = %+v, %v", in, err)
	}
	fallback := in.Requests[0]
	want := fmt.Sprintf("Task #%d: pilot did not confirm delivery", task.ID)
	if !strings.Contains(fallback.Body, want) || strings.Contains(fallback.Body, "Private title") || fallback.TaskNoticeReceipt || wakes.count("scout") != 1 {
		t.Fatalf("fallback = %+v", fallback)
	}
	if len(m.audited(t, "task_notice_retried")) != 2 || len(m.audited(t, "task_notice_exhausted")) != 1 || len(m.audited(t, "task_notice_fallback")) != 1 {
		t.Fatal("missing task delivery audit events")
	}
	if got, _, err := client.Task(ctx, m.muse, false, task.ID); err != nil || got.Status != envelope.TaskProposed {
		t.Fatalf("delivery changed the task: %+v, %v", got, err)
	}
}
