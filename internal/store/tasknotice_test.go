package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// pendingNotices returns the outbox with the row ids zeroed, so a test
// compares what each row says.
func pendingNotices(t *testing.T, s *Store) []TaskNotice {
	t.Helper()
	rows, err := s.TaskNotices(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		rows[i].ID = 0
		if rows[i].Kind == NoteTask {
			rows[i].Kind = ""
		}
	}
	return rows
}

// hostNotice is the notice the relay would queue for host.
func hostNotice(host string) envelope.Request {
	return envelope.Request{From: "relay", To: host, Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: "Task news."}
}

// Each change that is news for a host leaves one outbox row naming the
// task, its new status, who caused the change and who sent the ask. A
// change that is no news leaves none.
func TestTaskNoticeRowForEveryNewsChange(t *testing.T) {
	ctx := context.Background()
	answered := envelope.Reply{Status: envelope.StatusAnswered, Body: "the answer"}
	row := func(status envelope.TaskStatus, actor string) []TaskNotice {
		return []TaskNotice{{Status: status, Actor: actor, Asker: "codex"}}
	}
	cases := []struct {
		name string
		held bool
		end  func(t *testing.T, s *Store, c *clock, req envelope.Request) error
		want []TaskNotice
	}{
		{name: "answered", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			_, err := s.Reply(ctx, req.ID, "muse", answered)
			return err
		}, want: row(envelope.TaskReturned, "muse")},
		{name: "answered by email", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.ReplyByEmail(ctx, "<m1>", req.ID, "muse", 0, answered)
			return err
		}, want: row(envelope.TaskReturned, "muse")},
		{name: "closed with its held acknowledgement", end: func(t *testing.T, s *Store, c *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			if err := s.HoldAck(ctx, req.ID, "muse", "on it", 30*time.Minute, 30*time.Minute); err != nil {
				return err
			}
			c.advance(31 * time.Minute)
			reps, err := s.CloseHeldAcks(ctx, func(string, string) bool { return true })
			if len(reps) != 1 {
				t.Fatalf("close held acks = %+v", reps)
			}
			return err
		}, want: row(envelope.TaskReturned, "muse")},
		{name: "reply replaces a held acknowledgement", end: func(t *testing.T, s *Store, c *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			if err := s.HoldAck(ctx, req.ID, "muse", "on it", 30*time.Minute, 30*time.Minute); err != nil {
				return err
			}
			c.advance(31 * time.Minute)
			if _, err := s.CloseHeldAcks(ctx, func(string, string) bool { return true }); err != nil {
				return err
			}
			_, err := s.Reply(ctx, req.ID, "muse", answered)
			return err
		}, want: append(row(envelope.TaskReturned, "muse"), row(envelope.TaskReturned, "muse")...)},
		{name: "failed", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			_, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusFailed, Body: "no access"})
			return err
		}, want: row(envelope.TaskOpen, "muse")},
		{name: "declined", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusDeclined, Body: "not mine"})
			return err
		}, want: row(envelope.TaskOpen, "muse")},
		{name: "failed while held back", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.FailHeld(ctx, req.ID, "muse", false, envelope.Reply{Status: envelope.StatusFailed, Body: "out of messages"})
			return err
		}, want: row(envelope.TaskOpen, "muse")},
		{name: "needs input", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			_, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"})
			return err
		}, want: row(envelope.TaskQuestion, "muse")},
		{name: "question answered by the sender", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which branch?"}); err != nil {
				return err
			}
			_, err := s.Answer(ctx, req.ID, "codex", "main")
			return err
		}, want: row(envelope.TaskQuestion, "muse")}, // back to working is no news
		{name: "cancelled by the sender", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.Cancel(ctx, req.ID, "codex")
			return err
		}, want: row(envelope.TaskOpen, "codex")},
		{name: "target removed", end: func(t *testing.T, s *Store, _ *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			_, err := s.CancelAllFor(ctx, "muse")
			return err
		}, want: row(envelope.TaskOpen, TaskRelay)},
		{name: "expired while claimed", end: func(t *testing.T, s *Store, c *clock, req envelope.Request) error {
			claimAsk(t, s, req.ID, "muse")
			c.advance(2 * time.Hour)
			_, err := s.Sweep(ctx)
			return err
		}, want: row(envelope.TaskOpen, TaskRelay)},
		{name: "hold timed out", held: true, end: func(_ *testing.T, s *Store, c *clock, _ envelope.Request) error {
			c.advance(2 * time.Hour)
			_, err := s.Sweep(ctx)
			return err
		}, want: row(envelope.TaskOpen, TaskRelay)},
		{name: "denied by the owner", held: true, end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.DenyHeld(ctx, req.ID, "not this one")
			return err
		}, want: row(envelope.TaskOpen, envelope.TaskOwner)},
		{name: "taken back by the owner", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.ReleaseTask(ctx, req.TaskID, envelope.TaskOwner, "reassigned to scout")
			return err
		}, want: row(envelope.TaskOpen, envelope.TaskOwner)},
		{name: "moved by the owner's fallback", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, err := s.RerouteUntaken(ctx, req.ID, "muse", "scout")
			return err
		}},
		{name: "dropped with its ask open", end: func(_ *testing.T, s *Store, _ *clock, req envelope.Request) error {
			_, _, err := s.DropTask(ctx, req.TaskID, envelope.TaskOwner, "")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := open(t, ":memory:")
			task, req := taskUnderAsk(t, s, tc.held)
			if got := pendingNotices(t, s); len(got) != 0 {
				t.Fatalf("outbox after an open add and an assign = %+v, want none", got)
			}
			if err := tc.end(t, s, c, req); err != nil {
				t.Fatal(err)
			}
			want := slices.Clone(tc.want)
			for i := range want {
				want[i].TaskID = task.ID
			}
			if got := pendingNotices(t, s); !slices.Equal(got, want) {
				t.Fatalf("outbox = %+v, want %+v", got, want)
			}
		})
	}
}

// A task filed as proposed leaves a row naming its filer, with no asker.
// The owner's approval, a done, a drop and a reopen are no news.
func TestTaskNoticeRowForProposedTask(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	task := addTask(t, s, envelope.Task{Filer: "codex", Title: "reply to the landlord", Status: envelope.TaskProposed, Source: "mail"})
	want := []TaskNotice{{TaskID: task.ID, Status: envelope.TaskProposed, Actor: "codex"}}
	if got := pendingNotices(t, s); !slices.Equal(got, want) {
		t.Fatalf("outbox = %+v, want %+v", got, want)
	}
	// A second add under the same key stores no task and so no row.
	keyed := addTask(t, s, envelope.Task{Filer: "codex", Title: "call the bank", Status: envelope.TaskProposed, Source: "mail", Key: "bank"})
	want = append(want, TaskNotice{TaskID: keyed.ID, Status: envelope.TaskProposed, Actor: "codex"})
	if _, created, err := s.AddTask(ctx, envelope.Task{Filer: "codex", Title: "call the bank", Status: envelope.TaskProposed, Source: "mail", Key: "bank"}); err != nil || created {
		t.Fatalf("second add under the key: created %v, %v", created, err)
	}
	if _, err := s.SetTaskStatus(ctx, task.ID, envelope.TaskOpen, envelope.TaskOwner, "", envelope.TaskProposed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MarkTaskDone(ctx, task.ID, envelope.TaskOwner, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReopenTask(ctx, task.ID, envelope.TaskOwner, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DropTask(ctx, task.ID, envelope.TaskOwner, ""); err != nil {
		t.Fatal(err)
	}
	if got := pendingNotices(t, s); !slices.Equal(got, want) {
		t.Fatalf("outbox after the owner's decisions = %+v, want %+v", got, want)
	}
}

// The outbox row and the task's status entry are one transaction: when
// either write fails, neither is stored and the reply is refused.
func TestTaskNoticeRowCommitsWithTheStatusEntry(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, breakIt string }{
		{"the outbox write fails", `CREATE TRIGGER no_notice BEFORE INSERT ON task_notices BEGIN SELECT RAISE(ABORT, 'no outbox'); END`},
		{"the status entry fails", `CREATE TRIGGER no_entry BEFORE INSERT ON task_events WHEN NEW.status = 'returned' BEGIN SELECT RAISE(ABORT, 'no entry'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := open(t, ":memory:")
			task, req := taskUnderAsk(t, s, false)
			claimAsk(t, s, req.ID, "muse")
			if _, err := s.DB().Exec(tc.breakIt); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "the answer"}); err == nil {
				t.Fatal("reply stored though one of its task writes failed")
			}
			if got, err := s.Task(ctx, task.ID, ""); err != nil || got.Status != envelope.TaskWorking || got.Result != "" {
				t.Fatalf("task = %+v, %v, want it still working", got, err)
			}
			if ev := lastTaskEvent(t, s, task.ID); ev.Kind != envelope.TaskEventAssigned {
				t.Fatalf("last history entry = %+v, want the assign", ev)
			}
			if got := pendingNotices(t, s); len(got) != 0 {
				t.Fatalf("outbox = %+v, want none", got)
			}
			if _, st, err := s.Request(ctx, req.ID); err != nil || st != envelope.StatusClaimed {
				t.Fatalf("request is %s, %v, want it still claimed", st, err)
			}
		})
	}
}

// A host is queued one notice per outbox row at most, and none while an
// unread notice about the same task waits for it. Once it has read that
// one, the next change queues another. Deleting a row takes its record of
// settled hosts with it.
func TestEnqueueTaskNoticeOncePerRowAndUnreadTask(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	task := addTask(t, s, envelope.Task{Filer: "codex", Title: "reply to the landlord", Status: envelope.TaskProposed, Source: "mail"})
	other := addTask(t, s, envelope.Task{Filer: "codex", Title: "call the bank", Status: envelope.TaskProposed, Source: "mail"})
	rows, err := s.TaskNotices(ctx, 100)
	if err != nil || len(rows) != 2 {
		t.Fatalf("outbox = %+v, %v", rows, err)
	}
	first, err := s.TaskNotices(ctx, 1)
	if err != nil || len(first) != 1 || first[0] != rows[0] {
		t.Fatalf("oldest row = %+v, %v, want %+v", first, err, rows[0])
	}
	enqueue := func(n TaskNotice, host string) bool {
		t.Helper()
		req, queued, err := s.EnqueueTaskNotice(ctx, n, hostNotice(host), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if queued != (req.ID != "") {
			t.Fatalf("queued %v with request %+v", queued, req)
		}
		return queued
	}
	queuedFor := func(host string) int {
		t.Helper()
		ids, err := s.QueuedIDs(ctx, host)
		if err != nil {
			t.Fatal(err)
		}
		return len(ids)
	}
	if !enqueue(rows[0], "pilot") {
		t.Fatal("first notice for pilot was not queued")
	}
	if enqueue(rows[0], "pilot") {
		t.Fatal("the same row queued pilot a second notice")
	}
	if !enqueue(rows[0], "scout") {
		t.Fatal("another host's notice for the row was not queued")
	}
	// Another task's change is its own notice.
	if !enqueue(rows[1], "pilot") {
		t.Fatalf("notice about task %d was not queued while one about task %d is unread", other.ID, task.ID)
	}
	if got := queuedFor("pilot"); got != 2 {
		t.Fatalf("pilot has %d queued notices, want 2", got)
	}

	// A second change to the first task, while pilot's notice is unread.
	if _, err := s.DB().Exec(`INSERT INTO task_notices (task_id, status, actor, asker, created_at) VALUES (?, 'open', 'relay', 'codex', 0)`, task.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = s.TaskNotices(ctx, 100)
	if err != nil || len(rows) != 3 {
		t.Fatalf("outbox = %+v, %v", rows, err)
	}
	second := rows[2]
	if enqueue(second, "pilot") {
		t.Fatal("a second notice was queued while the first is unread")
	}
	// pilot reads its notices. The row it was passed over for stays
	// settled, so nothing was kept back for it.
	if got, err := s.Deliver(ctx, "pilot", 10, time.Minute); err != nil || len(got) != 2 {
		t.Fatalf("pilot's poll = %+v, %v", got, err)
	}
	if enqueue(second, "pilot") {
		t.Fatal("the change pilot was passed over for was queued after it read the first")
	}
	if err := s.DeleteTaskNotice(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	var told int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM task_notice_told WHERE notice_id = ?`, second.ID).Scan(&told); err != nil || told != 0 {
		t.Fatalf("settled hosts kept for a deleted row = %d, %v", told, err)
	}
	if rows, err = s.TaskNotices(ctx, 100); err != nil || len(rows) != 2 {
		t.Fatalf("outbox after the delete = %+v, %v", rows, err)
	}

	// The next change queues another, now that the first is read.
	if _, err := s.DB().Exec(`INSERT INTO task_notices (task_id, status, actor, asker, created_at) VALUES (?, 'returned', 'muse', 'codex', 0)`, task.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.TaskNotices(ctx, 100)
	third := rows[len(rows)-1]
	if !enqueue(third, "pilot") {
		t.Fatal("no notice for the change after pilot read the first")
	}
	// A notice that ran out unread no longer stands in for the next one.
	c.advance(2 * time.Hour)
	if _, err := s.DB().Exec(`INSERT INTO task_notices (task_id, status, actor, asker, created_at) VALUES (?, 'open', 'relay', 'codex', 0)`, task.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.TaskNotices(ctx, 100)
	if !enqueue(rows[len(rows)-1], "pilot") {
		t.Fatal("no notice though the unread one had expired")
	}
}

// The outbox tables migrate onto an existing relay database, keep its
// tasks, and opening again changes nothing and keeps a waiting row.
func TestTaskNoticesMigrateOnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := context.Background()
	task, req := taskUnderAsk(t, s, false)
	// A database from before task notices: drop what this version added.
	if _, err := s.DB().Exec(`DROP TABLE task_notice_told; DROP TABLE task_notices;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, _ = open(t, path)
	if got, err := s.Task(ctx, task.ID, ""); err != nil || got.Title != "ship it" || got.CurrentRequest != req.ID {
		t.Fatalf("task after the upgrade = %+v, %v", got, err)
	}
	if _, err := s.Reply(ctx, req.ID, "muse", envelope.Reply{Status: envelope.StatusAnswered, Body: "the answer"}); err != nil {
		t.Fatal(err)
	}
	want := []TaskNotice{{TaskID: task.ID, Status: envelope.TaskReturned, Actor: "muse", Asker: "codex"}}
	if got := pendingNotices(t, s); !slices.Equal(got, want) {
		t.Fatalf("outbox after the upgrade = %+v, want %+v", got, want)
	}
	rows, _ := s.TaskNotices(ctx, 100)
	if _, queued, err := s.EnqueueTaskNotice(ctx, rows[0], hostNotice("pilot"), time.Hour); err != nil || !queued {
		t.Fatalf("enqueue = %v, %v", queued, err)
	}
	s.Close()
	for range 2 {
		s, _ = open(t, path)
		if err := s.migrateTaskNotices(); err != nil {
			t.Fatalf("migrating again: %v", err)
		}
		if got := pendingNotices(t, s); !slices.Equal(got, want) {
			t.Fatalf("outbox after opening again = %+v, want %+v", got, want)
		}
		if _, queued, err := s.EnqueueTaskNotice(ctx, rows[0], hostNotice("pilot"), time.Hour); err != nil || queued {
			t.Fatalf("enqueue after opening again = %v, %v, want pilot still settled", queued, err)
		}
		s.Close()
	}
}

func TestTaskReminderIsStampedOnlyWhenQueued(t *testing.T) {
	s, clk := open(t, ":memory:")
	ctx := t.Context()
	at := clk.now().Unix()
	due := envelope.TaskDue{DueDay: clk.now().Format(time.DateOnly), DueAt: &at}
	task := addTask(t, s, envelope.Task{Title: "Due now", Filer: "muse", TaskDue: due})
	if err := s.QueueTaskReminders(ctx, false); err != nil {
		t.Fatal(err)
	}
	if rows := pendingNotices(t, s); len(rows) != 0 {
		t.Fatalf("outbox with no eligible host = %+v", rows)
	}
	if err := s.QueueTaskReminders(ctx, true); err != nil {
		t.Fatal(err)
	}
	rows, err := s.TaskNotices(ctx, 10)
	if err != nil || len(rows) != 1 || rows[0].Kind != envelope.TaskNoticeReminder {
		t.Fatalf("staged reminder = %+v, %v", rows, err)
	}
	checkStamp := func(want bool) {
		t.Helper()
		var got bool
		if err := s.DB().QueryRow(`SELECT reminded_at IS NOT NULL FROM tasks WHERE id = ?`, task.ID).Scan(&got); err != nil || got != want {
			t.Fatalf("reminded_at stamped = %v, %v, want %v", got, err, want)
		}
	}
	checkStamp(false)
	n := rows[0]
	req, queued, err := s.EnqueueTaskNotice(ctx, n, hostNotice("pilot"), time.Hour)
	if err != nil || !queued || req.NoticeKind != envelope.TaskNoticeReminder {
		t.Fatalf("enqueue reminder = %+v, %v, %v", req, queued, err)
	}
	checkStamp(true)
	if err := s.DeleteTaskNotice(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueTaskReminders(ctx, true); err != nil {
		t.Fatal(err)
	}
	if rows := pendingNotices(t, s); len(rows) != 0 {
		t.Fatalf("outbox after enqueue = %+v", rows)
	}
}

func TestTaskNoticeKindMigrationPreservesPendingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	task := addTask(t, s, envelope.Task{Title: "Pending", Filer: "muse", Status: envelope.TaskProposed})
	rows, err := s.TaskNotices(t.Context(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("outbox = %+v, %v", rows, err)
	}
	n := rows[0]
	if _, _, err := s.EnqueueTaskNotice(t.Context(), n, hostNotice("pilot"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`ALTER TABLE task_notices RENAME TO task_notices_new;
		CREATE TABLE task_notices (
			id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL REFERENCES tasks(id),
			status TEXT NOT NULL, actor TEXT NOT NULL, asker TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL
		);
		INSERT INTO task_notices SELECT id, task_id, status, actor, asker, created_at FROM task_notices_new;
		DROP TABLE task_notices_new;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, _ = open(t, path)
	rows, err = s.TaskNotices(t.Context(), 10)
	if err != nil || len(rows) != 1 || rows[0] != n || rows[0].TaskID != task.ID {
		t.Fatalf("migrated outbox = %+v, %v", rows, err)
	}
	if _, queued, err := s.EnqueueTaskNotice(t.Context(), rows[0], hostNotice("pilot"), time.Hour); err != nil || queued {
		t.Fatalf("already settled host queued again = %v, %v", queued, err)
	}
}
