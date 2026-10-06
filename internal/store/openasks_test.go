package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// OpenAsks lists only what a request email may carry: asks to the agent
// that are queued, delivered or claimed, urgent first and then oldest, with
// bodies and clarification exchanges. Held requests, pings, notifies,
// finished, paused and expired requests never come back.
func TestOpenAsksListsOnlyOpenAsks(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	enqueue := func(req envelope.Request, ttl time.Duration) envelope.Request {
		t.Helper()
		req.Hop, req.Chain = 1, []string{req.From}
		out, err := s.Enqueue(ctx, req, ttl)
		if err != nil {
			t.Fatal(err)
		}
		c.advance(time.Second)
		return out
	}
	old := enqueue(envelope.Request{From: "claude-code", To: "instinct", Kind: envelope.KindAsk, Body: "old ask"}, time.Hour)
	delivered := enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindAsk, Body: "delivered ask"}, time.Hour)
	claimed := enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindAsk, Body: "claimed ask"}, time.Hour)
	enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindAsk, Body: "HELDTEXT", Status: envelope.StatusHeld, HoldTTL: time.Hour}, time.Hour)
	enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindPing, Body: "PINGTEXT"}, time.Hour)
	enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindNotify, Body: "NOTIFYTEXT"}, time.Hour)
	answered := enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindAsk, Body: "ANSWEREDTEXT"}, time.Hour)
	paused := enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindAsk, Body: "PAUSEDTEXT"}, time.Hour)
	resumed := enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindAsk, Body: "resumed ask"}, time.Hour)
	enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindAsk, Body: "EXPIREDTEXT"}, 2*time.Second)
	enqueue(envelope.Request{From: "muse", To: "grokbot", Kind: envelope.KindAsk, Body: "OTHERAGENT"}, time.Hour)

	// Deliver hands out the two oldest; claim and finish the ones that need
	// another state.
	if _, err := s.Deliver(ctx, "instinct", 2, time.Minute); err != nil {
		t.Fatal(err)
	}
	urgent := enqueue(envelope.Request{From: "muse", To: "instinct", Kind: envelope.KindAsk, Body: "urgent ask", Urgent: true}, time.Hour)
	for _, id := range []string{claimed.ID, answered.ID, paused.ID, resumed.ID} {
		if _, err := s.Claim(ctx, id, "instinct", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Reply(ctx, answered.ID, "instinct", envelope.Reply{Status: envelope.StatusAnswered, Body: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, paused.ID, "instinct", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which one?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, resumed.ID, "instinct", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which house?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(ctx, resumed.ID, "muse", "the lake house"); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenAsks(ctx, "instinct")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	want := []string{urgent.ID, old.ID, delivered.ID, claimed.ID, resumed.ID}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("open asks = %v, want %v (urgent, then oldest)", ids, want)
	}
	byID := map[string]envelope.Request{}
	for _, r := range got {
		byID[r.ID] = r
		for _, leak := range []string{"HELDTEXT", "PINGTEXT", "NOTIFYTEXT", "ANSWEREDTEXT", "PAUSEDTEXT", "EXPIREDTEXT", "OTHERAGENT"} {
			if strings.Contains(r.Body, leak) {
				t.Fatalf("open asks carry %s: %+v", leak, r)
			}
		}
	}
	if byID[old.ID].Body != "old ask" || byID[old.ID].From != "claude-code" || byID[old.ID].Status != envelope.StatusDelivered {
		t.Fatalf("old ask = %+v", byID[old.ID])
	}
	if byID[delivered.ID].Status != envelope.StatusDelivered || byID[claimed.ID].Status != envelope.StatusClaimed || byID[urgent.ID].Status != envelope.StatusQueued {
		t.Fatalf("statuses = %s %s %s", byID[delivered.ID].Status, byID[claimed.ID].Status, byID[urgent.ID].Status)
	}
	if !byID[urgent.ID].Urgent {
		t.Fatal("urgent flag lost")
	}
	ex := byID[resumed.ID].Exchanges
	if len(ex) != 1 || ex[0].Question != "which house?" || ex[0].Answer != "the lake house" {
		t.Fatalf("resumed exchanges = %+v", ex)
	}
}
