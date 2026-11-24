package relay_test

import (
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// TestGuestSendIdempotencyKey: the client puts a key on a guest's send too,
// and a guest's retry gets its own held ask back, without the target facts
// a guest is never shown.
func TestGuestSendIdempotencyKey(t *testing.T) {
	m, g := guestMesh(t, relay.Config{})
	first := guestAsk(t, g, "hello from outside")
	if first.Status != envelope.StatusHeld {
		t.Fatalf("guest ask %+v, want it held", first)
	}
	var key string
	if err := m.Store.DB().QueryRow(`SELECT idempotency_key FROM requests WHERE id = ?`, first.ID).Scan(&key); err != nil || key == "" {
		t.Fatalf("guest's send stored key %q (%v), want the client's key", key, err)
	}
	var again envelope.SendResponse
	if err := g.Raw(t.Context(), "POST", "/v1/send", map[string]any{"to": "muse", "body": "hello from outside", "idempotency_key": key}, &again); err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.Status != envelope.StatusHeld || again.Target != nil {
		t.Fatalf("guest retry returned %+v, want the held original %s with no target", again, first.ID)
	}
	if n := requestEvents(t, m, "held", first.ID); n != 1 {
		t.Fatalf("%d held events for a guest's send and its retry, want 1", n)
	}
	// The key is the guest's own: a teammate's send under it is its own.
	if err := m.Client(t, "grokbot").Raw(t.Context(), "POST", "/v1/send", map[string]any{"to": "muse", "body": "hello from outside", "idempotency_key": key}, nil); err != nil {
		t.Fatalf("teammate send under the guest's key: %v", err)
	}
	var n int
	if err := m.Store.DB().QueryRow(`SELECT COUNT(*) FROM requests WHERE idempotency_key = ?`, key).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d requests under the key (%v), want the guest's and the teammate's", n, err)
	}
}
