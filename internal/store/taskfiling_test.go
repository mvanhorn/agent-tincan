package store

import (
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

func TestClassifyAsk(t *testing.T) {
	for _, tc := range []struct {
		name             string
		change           func(*envelope.Request)
		fromKind, toKind string
		openGuest        bool
		want             askFiling
	}{
		{name: "plain", want: workTask},
		{name: "group", change: func(req *envelope.Request) { req.Group = "group-1" }, want: workTask},
		{name: "first hop", change: func(req *envelope.Request) { req.Then = &envelope.Then{To: "idle"} }, want: workTask},
		{name: "held work", change: func(req *envelope.Request) { req.Status = envelope.StatusHeld }, want: workTask},
		{name: "task attempt", change: func(req *envelope.Request) { req.TaskID = 1 }},
		{name: "task repeat", change: func(req *envelope.Request) { req.TaskID = 1; req.NoParent = true }},
		{name: "notify", change: func(req *envelope.Request) { req.Kind = envelope.KindNotify }},
		{name: "ping", change: func(req *envelope.Request) { req.Kind = envelope.KindPing }},
		{name: "notes", toKind: onboard.KindNotes, want: quietTask},
		{name: "history", toKind: onboard.KindHistory, want: quietTask},
		{name: "pass", fromKind: onboard.KindPass, want: quietTask},
		{name: "pass seed", change: func(req *envelope.Request) { req.Group = envelope.PassGroupPrefix + "1" }},
		{name: "council to", toKind: onboard.KindCouncil, want: quietTask},
		{name: "council from", fromKind: onboard.KindCouncil, want: quietTask},
		{name: "room mention", change: func(req *envelope.Request) { req.RoomPostID = 1 }, want: quietTask},
		{name: "room descendant", change: func(req *envelope.Request) { req.SkipTaskRecovery = true }, want: quietTask},
		{name: "follow-on hop", change: func(req *envelope.Request) { req.FollowOf = "first" }, want: quietTask},
		{name: "scheduled ask", change: func(req *envelope.Request) { req.NoParent = true }, want: quietTask},
		{name: "relay ask", change: func(req *envelope.Request) { req.From = TaskRelay }},
		{name: "guest ask", change: func(req *envelope.Request) { req.FromGuest = true }},
		{name: "ask on behalf of a guest", openGuest: true},
		{name: "outside ask", change: func(req *envelope.Request) { req.RecoverySource = "mail" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := envelope.Request{From: "one", To: "two", Kind: envelope.KindAsk}
			if tc.change != nil {
				tc.change(&req)
			}
			if got := classifyAsk(req, tc.fromKind, tc.toKind, tc.openGuest); got != tc.want {
				t.Fatalf("classifyAsk = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFiledAsksDoNotUseExplicitTaskQuota(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := t.Context()
	for range MaxOpenTasks {
		req, err := s.EnqueueFilingTasks(ctx, envelope.Request{
			From: "codex", To: "muse", Kind: envelope.KindAsk, Body: "Check the line", Hop: 1,
			Chain: []string{"codex"},
		}, time.Hour, false)
		if err != nil || req.TaskID == 0 {
			t.Fatalf("file ask = %+v, %v", req, err)
		}
	}
	addTask(t, s, envelope.Task{Title: "Explicit task", Filer: "codex"})
}
