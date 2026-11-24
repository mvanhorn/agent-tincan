package relay

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// racingPrep queues the same keyed send itself while the relay prepares
// one, as a send that got past both key lookups and the lock would.
type racingPrep struct {
	st     *store.Store
	racer  envelope.Request
	queued string
}

func (p *racingPrep) Prepare(ctx context.Context, req *envelope.Request) error {
	req.Hop, req.Chain, req.TraceID = 1, []string{req.From}, ""
	won, err := p.st.Enqueue(ctx, p.racer, time.Hour)
	p.queued = won.ID
	return err
}

// TestSendIdempotencyKeyStoreRefusesDuplicate: whatever gets a second send
// under one key to the store, the store refuses it and the relay answers
// with the request that took the key, as for any retry.
func TestSendIdempotencyKeyStoreRefusesDuplicate(t *testing.T) {
	h := newHarness(t, Config{})
	racer, err := envelope.ParseSend([]byte(keyedBody("instinct", "deploy", "raced")), "grokbot", envelope.DefaultMaxBody)
	if err != nil {
		t.Fatal(err)
	}
	racer.Hop, racer.Chain = 1, []string{"grokbot"}
	p := &racingPrep{st: h.st, racer: racer}
	h.srv.SetPreparer(p)
	out := h.sendKeyed(grokAddr, "instinct", "deploy", "raced", http.StatusOK)
	if p.queued == "" || out.ID != p.queued {
		t.Fatalf("send that lost the race returned %s, want the racer's %s", out.ID, p.queued)
	}
	if got := h.delivered(instinctAddr); len(got) != 1 || got[0].ID != p.queued {
		t.Fatalf("recipient was handed %+v, want only the racer's request", got)
	}
}

// TestLockSendKey: sends under one sender's key run one at a time, a send
// that was waiting keeps the same lock after the holder lets go, other keys
// and other senders do not wait, and the entry goes with the last send.
func TestLockSendKey(t *testing.T) {
	s := &Server{}
	release := s.lockSendKey("grokbot", "k1")
	// Another key, and the same key from another sender, do not wait.
	s.lockSendKey("grokbot", "k2")()
	s.lockSendKey("muse", "k1")()

	var mu sync.Mutex
	holders, most := 0, 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			unlock := s.lockSendKey("grokbot", "k1")
			mu.Lock()
			holders++
			most = max(most, holders)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			holders--
			mu.Unlock()
			unlock()
		})
	}
	// Every waiter holds a reference before the first send lets go, so
	// none of them can find the entry gone and make a second lock.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		s.sendKeyMu.Lock()
		refs := s.sendKeys["grokbot\x00k1"].refs
		s.sendKeyMu.Unlock()
		if refs == 9 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refs = %d, want the holder and 8 waiters", refs)
		}
	}
	release()
	wg.Wait()
	if most != 1 {
		t.Fatalf("%d sends held one key at once, want 1", most)
	}
	if len(s.sendKeys) != 0 {
		t.Fatalf("entries left after every send finished: %v", s.sendKeys)
	}
}
