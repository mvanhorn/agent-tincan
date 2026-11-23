package relay_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// One request through every v0.6.0 lifecycle feature at once: an urgent ask
// held for the owner (#69, #73), approved, claimed with a progress note (#70),
// sent back to the asker with needs_input (#74), answered, then replied, and
// found by search along the way (#72). A fan-out group (#71) counts a member
// waiting in needs_input as still open.
func TestCombinedRequestLifecycle(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}},"hold_ttl":"1h"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	ctx := t.Context()
	asker, muse, admin := m.Client(t, "grokbot"), m.Client(t, "muse"), m.Client(t, "admin")

	req, err := asker.Send(ctx, "muse", "book dinner for seven", envelope.KindAsk, "", true)
	if err != nil || req.Status != envelope.StatusHeld {
		t.Fatalf("send = %+v, %v", req, err)
	}
	if hits, err := muse.Search(ctx, "dinner", 20); err != nil || len(hits) != 0 {
		t.Fatalf("target searched a held request: %+v, %v", hits, err)
	}
	if hits, err := asker.Search(ctx, "dinner", 20); err != nil || len(hits) != 1 || hits[0].Status != envelope.StatusHeld {
		t.Fatalf("sender search = %+v, %v", hits, err)
	}
	if err := admin.Raw(ctx, "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}

	claim := func() envelope.Request {
		t.Helper()
		in, err := muse.Poll(ctx, time.Second)
		if err != nil || len(in.Requests) != 1 || in.Requests[0].ID != req.ID || !in.Requests[0].Urgent {
			t.Fatalf("poll = %+v, %v", in, err)
		}
		got, err := muse.Claim(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	claim()
	if err := muse.Progress(ctx, req.ID, "checking tables"); err != nil {
		t.Fatal(err)
	}
	res, err := asker.Get(ctx, req.ID, 0)
	if err != nil || res.Status != envelope.StatusClaimed || res.Progress == nil || res.Progress.Note != "checking tables" {
		t.Fatalf("progress = %+v, %v", res, err)
	}
	if hits, err := muse.Search(ctx, "dinner", 20); err != nil || len(hits) != 1 || hits[0].Status != envelope.StatusClaimed {
		t.Fatalf("approved, progress-noted request not found: %+v, %v", hits, err)
	}

	if _, err := muse.Reply(ctx, req.ID, "Which restaurant?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	res, err = asker.Get(ctx, req.ID, 0)
	if err != nil || res.Status != envelope.StatusNeedsInput || res.Reply == nil || res.Reply.Body != "Which restaurant?" {
		t.Fatalf("needs_input = %+v, %v", res, err)
	}
	// The owner approved the ask, not the answer: it waits for the owner again.
	if answered, err := asker.Answer(ctx, req.ID, "Nopa"); err != nil || answered.Status != envelope.StatusHeld {
		t.Fatalf("answer = %+v, %v; want held again", answered, err)
	}
	if err := admin.Raw(ctx, "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	again := claim()
	if len(again.Exchanges) != 1 || again.Exchanges[0].Answer != "Nopa" {
		t.Fatalf("resumed request = %+v", again)
	}
	res, err = asker.Get(ctx, req.ID, 0)
	if err != nil || res.Status != envelope.StatusClaimed || res.Progress != nil {
		t.Fatalf("resumed request kept the last round's progress: %+v, %v", res, err)
	}
	if _, err := muse.Reply(ctx, req.ID, "booked Nopa at seven", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	res, err = asker.Get(ctx, req.ID, 0)
	if err != nil || res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.Body != "booked Nopa at seven" {
		t.Fatalf("final = %+v, %v", res, err)
	}
	if hits, err := asker.Search(ctx, "booked", 20); err != nil || len(hits) != 1 || hits[0].RequestID != req.ID {
		t.Fatalf("reply search = %+v, %v", hits, err)
	}

	// Fan-out (to ungated teammates): one member asks for input, the other
	// answers.
	g, err := muse.SendGroup(ctx, []string{"instinct", "grokbot"}, "pick a wine", envelope.KindAsk, "", nil, false)
	if err != nil || len(g.Results) != 2 {
		t.Fatalf("group = %+v, %v", g, err)
	}
	for _, e := range g.Results {
		c := m.Client(t, e.Request.To)
		if _, err := c.Claim(ctx, e.Request.ID); err != nil {
			t.Fatal(err)
		}
		status, body := envelope.StatusAnswered, "a pinot"
		if e.Request.To == "instinct" {
			status, body = envelope.StatusNeedsInput, "red or white?"
		}
		if _, err := c.Reply(ctx, e.Request.ID, body, status); err != nil {
			t.Fatal(err)
		}
	}
	g, err = muse.GetGroup(ctx, g.Group, 0)
	if err != nil || g.Outcome != "partial" || g.ExitCode() != 2 {
		t.Fatalf("group with a needs_input member = %+v, %v", g, err)
	}
	for _, e := range g.Results {
		if e.Request.To == "instinct" && e.Status != envelope.StatusNeedsInput {
			t.Fatalf("instinct = %s", e.Status)
		}
	}
}
