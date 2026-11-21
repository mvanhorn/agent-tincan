package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

func TestTaskRecoveryOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		want envelope.TaskStatus
	}{
		{"failed", envelope.TaskOpen},
		{"declined", envelope.TaskOpen},
		{"expired", envelope.TaskOpen},
		{"cancelled by the relay", envelope.TaskOpen},
		{"denied by the owner", envelope.TaskProposed},
		{"answered", ""},
		{"withdrawn", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := open(t, ":memory:")
			ctx := t.Context()
			req := envelope.Request{From: "asker", To: "worker", Kind: envelope.KindAsk, Body: "Check the mailroom\nRecord what you find."}
			if tc.want == envelope.TaskProposed {
				req.Status, req.HoldTTL = envelope.StatusHeld, time.Hour
			}
			req, err := s.Enqueue(ctx, req, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "expired":
				c.advance(time.Hour)
				_, err = s.Sweep(ctx)
			case "withdrawn":
				_, err = s.Cancel(ctx, req.ID, req.From)
			case "cancelled by the relay":
				_, err = s.CancelAllFor(ctx, req.To)
			case "denied by the owner":
				_, err = s.DenyHeld(ctx, req.ID, "private denial reason")
			default:
				_, err = s.Reply(ctx, req.ID, req.To, envelope.Reply{Status: envelope.Status(tc.name), Body: "private reply text"})
			}
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := s.RecoverTasks(ctx, 4*time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			tasks, err := s.Tasks(ctx, "", 0, 10, true)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(tasks) != 0 {
					t.Fatalf("unexpected tasks: %+v", tasks)
				}
				return
			}
			if len(tasks) != 1 {
				t.Fatalf("tasks = %+v", tasks)
			}
			task := tasks[0]
			if task.Status != tc.want || task.Filer != envelope.TaskRelayFiler || task.Assignee != "" || task.CurrentRequest != "" ||
				task.Title != "Check the mailroom" || task.Detail != req.Body || task.Key != req.ID {
				t.Fatalf("task = %+v", task)
			}
			events, err := s.TaskEvents(ctx, task.ID, 0, 10)
			if err != nil || len(events) != 1 || !strings.Contains(events[0].Body, "from asker to worker") || !strings.Contains(events[0].Body, tc.name) {
				t.Fatalf("task history = %+v, %v", events, err)
			}
			audit, err := s.AuditEvents(ctx)
			if err != nil || len(audit) != 1 || audit[0].Event != "task_auto_filed" || audit[0].RequestID != req.ID || audit[0].TraceID != "" ||
				strings.Contains(audit[0].Detail, "private") || strings.Contains(audit[0].Detail, "mailroom") {
				t.Fatalf("audit = %+v, %v", audit, err)
			}
			if _, err := s.VerifyAudit(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTaskRecoveryClarification(t *testing.T) {
	for _, answer := range []bool{false, true} {
		t.Run(fmt.Sprintf("answered=%t", answer), func(t *testing.T) {
			s, c := open(t, ":memory:")
			ctx := t.Context()
			req, err := s.Enqueue(ctx, envelope.Request{From: "asker", To: "worker", Kind: envelope.KindAsk, Body: "first line"}, 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Claim(ctx, req.ID, req.To, time.Hour); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reply(ctx, req.ID, req.To, envelope.Reply{Status: envelope.StatusNeedsInput, Body: "Where?"}); err != nil {
				t.Fatal(err)
			}
			c.advance(14 * time.Minute)
			if expired, err := s.RecoverTasks(ctx, 15*time.Minute); err != nil || len(expired) != 0 {
				t.Fatalf("early recovery = %+v %v", expired, err)
			}
			if answer {
				if _, err := s.Answer(ctx, req.ID, req.From, "Over there"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Reply(ctx, req.ID, req.To, envelope.Reply{Status: envelope.StatusAnswered, Body: "Done"}); err != nil {
					t.Fatal(err)
				}
			}
			c.advance(time.Minute)
			expired, err := s.RecoverTasks(ctx, 15*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := s.Tasks(ctx, "", 0, 10, true)
			if err != nil {
				t.Fatal(err)
			}
			if answer {
				if len(expired) != 0 || len(tasks) != 0 {
					t.Fatalf("answered clarification filed: %+v %+v", expired, tasks)
				}
				return
			}
			if len(expired) != 1 || expired[0] != req.ID || len(tasks) != 1 {
				t.Fatalf("clarification recovery = %+v %+v", expired, tasks)
			}
			if _, err := s.Answer(ctx, req.ID, req.From, "Too late"); !errors.Is(err, ErrWrongState) {
				t.Fatalf("late answer = %v", err)
			}
			events, err := s.TaskEvents(ctx, tasks[0].ID, 0, 10)
			if err != nil || len(events) != 1 || !strings.Contains(events[0].Body, "clarification timed out") {
				t.Fatalf("events = %+v, %v", events, err)
			}
		})
	}
}

func TestTaskRecoverySkipsInternalAndLinkedAsks(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := t.Context()
	for _, kind := range []string{"history", "notes", "pass", "council"} {
		if err := s.PutAgent(ctx, identity.Agent{Name: kind, NodeID: kind, Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	task := addTask(t, s, envelope.Task{Title: "already filed", Filer: "asker"})
	linked, err := attempt(s, task.ID, "asker", "worker", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, linked.ID, linked.To, envelope.Reply{Status: envelope.StatusFailed, Body: "failed"}); err != nil {
		t.Fatal(err)
	}
	requests := []envelope.Request{
		{From: "relay", To: "worker", Kind: envelope.KindAsk},
		{From: "asker", To: "worker", Kind: envelope.KindNotify},
		{From: "asker", To: "worker", Kind: envelope.KindPing},
		{From: "asker", To: "worker", Kind: envelope.KindAsk, Group: "pass-session"},
		{From: "council", To: "worker", Kind: envelope.KindAsk},
		{From: "asker", To: "history", Kind: envelope.KindAsk},
		{From: "asker", To: "notes", Kind: envelope.KindAsk},
		{From: "asker", To: "pass", Kind: envelope.KindAsk},
		{From: "asker", To: "council", Kind: envelope.KindAsk},
	}
	for _, req := range requests {
		req.Body = "internal"
		req, err = s.Enqueue(ctx, req, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reply(ctx, req.ID, req.To, envelope.Reply{Status: envelope.StatusFailed, Body: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecoverTasks(ctx, 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Tasks(ctx, "", 0, 10, true)
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
}

func TestTaskRecoveryWaitsForRetryAndFallback(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := t.Context()
	req := ask(t, s, "asker", "first", "try it")
	if _, err := s.RerouteUntaken(ctx, req.ID, "first", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, req.ID, "second", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverTasks(ctx, 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	if tasks, err := s.Tasks(ctx, "", 0, 10, true); err != nil || len(tasks) != 0 {
		t.Fatalf("early tasks = %+v, %v", tasks, err)
	}
	if _, err := s.Reply(ctx, req.ID, "second", envelope.Reply{Status: envelope.StatusFailed, Body: "failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverTasks(ctx, 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Tasks(ctx, "", 0, 10, true)
	if err != nil || len(tasks) != 1 || tasks[0].Key != req.ID {
		t.Fatalf("final tasks = %+v, %v", tasks, err)
	}
	events, err := s.TaskEvents(ctx, tasks[0].ID, 0, 10)
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Body, "to first") || !strings.Contains(events[0].Body, "Final target: second") {
		t.Fatalf("final history = %+v, %v", events, err)
	}
}

func TestTaskRecoveryCapPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := t.Context()
	for range MaxOpenTasks {
		addTask(t, s, envelope.Task{Title: "manual", Filer: "asker"})
	}
	for range maxRecoveredTasks {
		old := ask(t, s, "asker", "worker", "previous recovery")
		if _, err := s.Reply(ctx, old.ID, old.To, envelope.Reply{Status: envelope.StatusFailed, Body: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	for range maxRecoveredTasks / 20 {
		if _, err := s.RecoverTasks(ctx, 4*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	req := ask(t, s, "asker", "worker", "deferred")
	if _, err := s.Reply(ctx, req.ID, req.To, envelope.Reply{Status: envelope.StatusFailed, Body: "failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverTasks(ctx, 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ = open(t, path)
	if _, err := s.SetTaskStatus(ctx, MaxOpenTasks+1, envelope.TaskDropped, envelope.TaskOwner, "", envelope.TaskOpen); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.RecoverTasks(ctx, 4*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := s.Tasks(ctx, "asker", MaxOpenTasks+maxRecoveredTasks, 10, false)
	if err != nil || len(tasks) != 1 || tasks[0].Filer != envelope.TaskRelayFiler || tasks[0].Key != req.ID {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	if _, err := s.Task(ctx, tasks[0].ID, "unrelated"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("auto-filed ask entered shared pool: %v", err)
	}
}

func TestTaskRecoveryMigrationKeepsWaitingAsks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := t.Context()
	closed := ask(t, s, "asker", "worker", "already failed before upgrade")
	if _, err := s.Reply(ctx, closed.ID, closed.To, envelope.Reply{Status: envelope.StatusFailed, Body: "failed"}); err != nil {
		t.Fatal(err)
	}
	waiting := ask(t, s, "asker", "worker", "waiting for input at upgrade")
	if _, err := s.Claim(ctx, waiting.ID, waiting.To, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, waiting.ID, waiting.To, envelope.Reply{Status: envelope.StatusNeedsInput, Body: "Where?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(dropTaskRecoverySchema); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, c := open(t, path)
	c.advance(30 * time.Minute)
	if _, err := s.RecoverTasks(ctx, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Tasks(ctx, "", 0, 10, true)
	if err != nil || len(tasks) != 1 || tasks[0].Key != waiting.ID {
		t.Fatalf("tasks after upgrade = %+v, %v", tasks, err)
	}
}

// dropTaskRecoverySchema constructs fixtures that predate auto-filing.
const dropTaskRecoverySchema = `
DROP TRIGGER requests_task_recovery;
DROP TABLE task_recovery_requests;
ALTER TABLE requests DROP COLUMN recovery_source;
ALTER TABLE requests DROP COLUMN skip_task_recovery;
ALTER TABLE requests DROP COLUMN recovery_repeat;
`
