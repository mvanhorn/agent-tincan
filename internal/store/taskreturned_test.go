package store

import (
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestSweepReturnedHostTasks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		asker      string
		host       bool
		followUp   bool
		legacy     bool
		wantClosed bool
	}{
		{name: "no host available", asker: "asker", host: true, wantClosed: true},
		{name: "stored before upgrade", asker: "asker", host: true, legacy: true, wantClosed: true},
		{name: "asker followed up", asker: "asker", host: true, followUp: true, wantClosed: true},
		{name: "owner asked", asker: envelope.TaskOwner, host: true},
		{name: "worker returned it", asker: "asker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := open(t, ":memory:")
			ctx := t.Context()
			joinAll(t, s, c, "asker", "worker")
			if tc.host {
				if ok, err := s.SetAgentTaskHost(ctx, "worker", true); err != nil || !ok {
					t.Fatalf("set host = %v, %v", ok, err)
				}
			}
			req, err := s.EnqueueFilingTasks(ctx, envelope.Request{
				From: tc.asker, To: "worker", Kind: envelope.KindAsk, Body: "Confirm the opening hours",
			}, time.Hour, false)
			if err != nil || req.TaskID == 0 {
				t.Fatalf("file ask = %+v, %v", req, err)
			}
			const result = "Open from 9 to 5."
			reply(t, s, req, envelope.StatusAnswered, result)
			if tc.legacy {
				if _, err := s.db.Exec(`UPDATE task_events SET returned_by_host = NULL WHERE task_id = ?`, req.TaskID); err != nil {
					t.Fatal(err)
				}
			} else if tc.host {
				if ok, err := s.SetAgentTaskHost(ctx, "worker", false); err != nil || !ok {
					t.Fatalf("clear host = %v, %v", ok, err)
				}
			}
			sweep := func(want envelope.TaskStatus) {
				t.Helper()
				if err := s.SweepReturnedHostTasks(ctx, time.Hour, false); err != nil {
					t.Fatal(err)
				}
				got, err := s.Task(ctx, req.TaskID, "")
				if err != nil || got.Status != want || got.Result != result {
					t.Fatalf("task = %+v, %v, want %s with its result", got, err, want)
				}
			}
			c.advance(30 * time.Minute)
			if tc.followUp {
				if _, err := s.AppendTaskEvent(ctx, envelope.TaskEvent{
					TaskID: req.TaskID, Actor: tc.asker, Kind: envelope.TaskEventComment, Body: "Please confirm Saturday.",
				}); err != nil {
					t.Fatal(err)
				}
			}
			c.advance(29 * time.Minute)
			sweep(envelope.TaskReturned)
			c.advance(time.Minute)
			if tc.followUp {
				sweep(envelope.TaskReturned)
				c.advance(30 * time.Minute)
			}
			if !tc.wantClosed {
				sweep(envelope.TaskReturned)
				return
			}
			sweep(envelope.TaskDone)
			history, err := s.TaskEvents(ctx, req.TaskID, 0, 20)
			if err != nil || len(history) == 0 {
				t.Fatalf("task history = %+v, %v", history, err)
			}
			if last := history[len(history)-1]; last.Actor != TaskRelay || last.Status != envelope.TaskDone || last.Body != "returned, asker did not follow up" {
				t.Fatalf("last history entry = %+v", last)
			}
		})
	}
}
