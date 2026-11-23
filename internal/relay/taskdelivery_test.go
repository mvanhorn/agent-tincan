package relay_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
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
	initialMessage := fmt.Sprintf("Task #%d: review and approve this task if you want it to proceed.", task.ID)
	for attempt := 1; attempt <= 3; attempt++ {
		got := read(t, pilot)
		if len(got) != 1 || got[0].Body != initial.Body || !got[0].TaskNoticeReceipt {
			t.Fatalf("attempt %d inbox = %+v", attempt, got)
		}
		wantMessage := initialMessage
		if attempt > 1 {
			wantMessage = "Re-send of the same task news: " + initialMessage
		}
		if message := client.TaskNoticeOwnerMessage(got[0]); message != wantMessage {
			t.Fatalf("attempt %d owner message = %q, want %q", attempt, message, wantMessage)
		}
		if _, err := pilot.Claim(ctx, got[0].ID); err != nil {
			t.Fatal(err)
		}
		// A run that claims the notice and ends without posting skips the acknowledgment.
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
	want := fmt.Sprintf("Task #%d: pilot did not acknowledge this task notice", task.ID)
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

func TestTaskNoticeFallsBackWhenHostLosesEligibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status envelope.Status
		change func(*testing.T, *taskMesh)
	}{
		{"removed", envelope.StatusClaimed, func(t *testing.T, m *taskMesh) {
			if err := m.admin.Remove(t.Context(), "pilot"); err != nil {
				t.Fatal(err)
			}
		}},
		{"cleared", envelope.StatusDelivered, func(t *testing.T, m *taskMesh) {
			if _, err := m.admin.SetTaskHost(t.Context(), "pilot", false); err != nil {
				t.Fatal(err)
			}
			// Regranting the mark does not revive notices from the old grant.
			if _, err := m.admin.SetTaskHost(t.Context(), "pilot", true); err != nil {
				t.Fatal(err)
			}
		}},
		{"suspended", envelope.StatusQueued, func(t *testing.T, m *taskMesh) {
			m.gate(t, `{"gate":{"pilot":{"from":["muse"]}},"notify":"scout"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTaskMesh(t)
			pilot := m.joinHost(t, "instinct", "pilot")
			m.gate(t, `{"gate":{},"notify":"scout"}`)
			ctx := t.Context()

			taskDo(t, m.muse, "POST", "/v1/tasks", map[string]string{"title": "Already reported", "source": "mail"})
			confirmed := m.wantNotices(t, "pilot", 1)[0]
			if _, err := pilot.Claim(ctx, confirmed.ID); err != nil {
				t.Fatal(err)
			}
			if err := client.TaskNoticesTold(ctx, pilot, []string{confirmed.ID}); err != nil {
				t.Fatal(err)
			}

			task := taskDo(t, m.muse, "POST", "/v1/tasks", map[string]string{"title": "Private title", "source": "mail"}).Task
			notice := m.wantNotices(t, "pilot", 2)[1]
			switch tc.status {
			case envelope.StatusDelivered:
				if got := read(t, pilot); len(got) != 1 || got[0].ID != notice.ID {
					t.Fatalf("host inbox = %+v", got)
				}
			case envelope.StatusClaimed:
				if _, err := pilot.Claim(ctx, notice.ID); err != nil {
					t.Fatal(err)
				}
			}
			other := m.joinHost(t, "grokbot", "other")
			tc.change(t, m)

			// The next sweep sends the fallback before the receipt timeout.
			m.Server.Sweep(ctx)
			m.Server.Sweep(ctx)
			in, err := m.scout.Poll(ctx, 0)
			if err != nil || len(in.Requests) != 1 {
				t.Fatalf("fallback inbox = %+v, %v", in, err)
			}
			fallback := in.Requests[0]
			wantNews := fmt.Sprintf("Task #%d was proposed and is waiting for your approval.", task.ID)
			if !strings.Contains(fallback.Body, wantNews) || strings.Contains(fallback.Body, "Private title") || fallback.TaskNoticeReceipt {
				t.Fatalf("fallback = %+v", fallback)
			}
			if _, status, err := m.Store.Request(ctx, notice.ID); err != nil || status != envelope.StatusCancelled {
				t.Fatalf("old notice status = %q, %v", status, err)
			}
			if got := read(t, other); len(got) != 0 {
				t.Fatalf("other host inbox = %+v", got)
			}
			if got := m.audited(t, "task_notice_retried"); len(got) != 0 {
				t.Fatalf("retry audit = %v", got)
			}
			if got := m.audited(t, "task_notice_fallback"); len(got) != 1 {
				t.Fatalf("fallback audit = %v", got)
			}
		})
	}
}

func TestTaskFallbackStatusIsAdminOnly(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	ctx := t.Context()
	if got, err := m.grok.TaskFallback(ctx); err != nil || got != nil {
		t.Fatalf("ordinary agent task fallback = %+v, %v", got, err)
	}
	for _, set := range []bool{false, true} {
		if set {
			m.gate(t, `{"gate":{},"notify":"instinct"}`)
		}
		got, err := m.admin.TaskFallback(ctx)
		if err != nil || got == nil || got.NotifySet != set || len(got.Hosts) != 1 || got.Hosts[0] != "muse" {
			t.Fatalf("admin task fallback = %+v, %v", got, err)
		}
	}
}

func TestTaskNoticeFallbackWakesEmailAgent(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "muse")
	m.gate(t, `{"gate":{},"notify":"instinct"}`)
	ctx := t.Context()
	mail := make(chan map[string]string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/inboxes/relay@agentmail.to/messages/send" {
			t.Errorf("email request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var msg map[string]string
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mail <- msg
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	waker := wake.New(wake.Config{"instinct": {
		Method: wake.Email, EmailTo: "instinct@example.com", AgentMailFrom: "relay@agentmail.to",
		AgentMailKey: "test-key", IncludeRequests: true,
	}}, m.Store, wake.Options{
		Debounce: time.Millisecond, WakeGrace: -1, AgentMailAPI: srv.URL, HTTP: srv.Client(),
		Queued: m.Server.QueuedCount, OpenAsks: m.Server.OpenAsks, RequestTag: m.Server.RequestEmailTag,
	})
	m.Server.SetEvents(waker)
	m.Server.SetWakeSettings(waker)
	task := taskDo(t, m.grok, "POST", "/v1/tasks", map[string]string{"title": "Private outside title", "source": "mail"}).Task
	for range 3 {
		got := read(t, m.muse)
		if len(got) != 1 {
			t.Fatalf("host attempt = %+v", got)
		}
		if _, err := m.muse.Claim(ctx, got[0].ID); err != nil {
			t.Fatal(err)
		}
		m.clk.advance(relay.DefaultTaskNoticeAckTimeout)
		m.Server.Sweep(ctx)
	}
	waker.Flush()
	select {
	case msg := <-mail:
		if msg["to"] != "instinct@example.com" || !strings.Contains(msg["text"], "1 request waiting") ||
			strings.Contains(msg["text"], task.Title) {
			t.Fatalf("email wake = %+v", msg)
		}
	default:
		t.Fatal("no email wake was sent")
	}
	in, err := m.instinct.Poll(ctx, 0)
	if err != nil || len(in.Requests) != 1 || !strings.Contains(in.Requests[0].Body, fmt.Sprintf("Task #%d", task.ID)) ||
		strings.Contains(in.Requests[0].Body, task.Title) {
		t.Fatalf("fallback agent inbox = %+v, %v", in, err)
	}
}
