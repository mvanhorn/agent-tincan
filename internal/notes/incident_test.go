package notes

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// The incident replayed. The search is computed, the relay
// is unreachable for polls and replies past the claim lease, the relay
// requeues the request and queues a lease notice, and then the relay
// comes back. The asker gets exactly one reply, listing the family note
// first; the lease notice is closed unsent; the helper ran once.
func TestIncidentReplay(t *testing.T) {
	r := newRig(t, relay.Config{ClaimLease: 150 * time.Millisecond})
	family, _ := putIncidentLibrary(t, r.helper)
	r.replyFail.Store(true)
	req := r.send(t, "grokbot", `note: {"op":"search","query":"`+incidentQuery+`"}`)
	r.poll(t, 1)
	searches := len(r.helper.callsOf(t, "search"))

	// The outage outlasts the lease: polls fail too, and the relay
	// requeues the claim and queues a notice for the asker.
	r.pollFail.Store(true)
	if _, err := r.svc.PollOnce(t.Context()); err == nil {
		t.Fatal("poll got through the outage")
	}
	r.redeliver(t)
	events, err := r.mesh.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(e store.AuditEvent) bool { return e.Event == "relay_note" && e.RequestID == req.ID }) {
		t.Fatal("the relay noted no lapsed lease")
	}

	// Recovery: the redelivered request gets the stored reply.
	r.replyFail.Store(false)
	r.pollFail.Store(false)
	r.poll(t, 1)
	res := r.get(t, "grokbot", req.ID)
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, `1. "Family Alaska Airlines Numbers" (id `+family.ID+`), matches 8 of 10 words`) {
		t.Fatalf("status %s reply %+v", res.Status, res.Reply)
	}
	if got := len(r.helper.callsOf(t, "search")); got != searches {
		t.Fatalf("searches = %d after recovery, want %d: the search ran again", got, searches)
	}
	in, err := r.mesh.Client(t, "grokbot").Poll(t.Context(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range in.Requests {
		if n.From == "relay" {
			t.Fatalf("the asker was handed the superseded lease notice: %s", n.ID)
		}
	}
	events, err = r.mesh.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(e store.AuditEvent) bool { return e.Event == "notice_superseded" }) {
		t.Fatal("the lease notice was not closed by the reply")
	}
	r.assertSpoolEmpty(t)
}
