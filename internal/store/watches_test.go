package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// idleNotice is a watch's notice as the relay builds it.
func idleNotice(w Watch) envelope.Request {
	return envelope.Request{From: "relay", To: w.Watcher, Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: w.Target + " is idle now. No reply needed."}
}

// A second watch on the same target from the same watcher is the first
// one; another watcher gets its own.
func TestAddWatchDedupes(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	first, created, err := s.AddWatch(ctx, "claude-code", "codex", 24*time.Hour)
	if err != nil || !created || !first.Expires.Equal(c.t.Add(24*time.Hour)) {
		t.Fatalf("first = %+v %v %v", first, created, err)
	}
	again, created, err := s.AddWatch(ctx, "claude-code", "codex", 24*time.Hour)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("again = %+v %v %v", again, created, err)
	}
	if _, created, err := s.AddWatch(ctx, "grokbot", "codex", 24*time.Hour); err != nil || !created {
		t.Fatalf("another watcher: %v %v", created, err)
	}
	pending, err := s.PendingWatches(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v %v", pending, err)
	}
}

// Firing queues the notice and marks the watch in one transaction: a
// second fire, from a racing caller or after a restart, queues nothing.
func TestFireWatchOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := context.Background()
	w, _, err := s.AddWatch(ctx, "claude-code", "codex", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	fired := 0
	for range 4 {
		wg.Go(func() {
			_, ok, err := s.FireWatch(ctx, w, idleNotice(w), time.Hour)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				fired++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if fired != 1 {
		t.Fatalf("fired %d times", fired)
	}
	s.Close()
	reopened, _ := open(t, path)
	if _, ok, err := reopened.FireWatch(ctx, w, idleNotice(w), time.Hour); ok || err != nil {
		t.Fatalf("after a restart: %v %v", ok, err)
	}
	if n, err := reopened.CountQueued(ctx, "claude-code"); err != nil || n != 1 {
		t.Fatalf("watcher has %d notices, %v", n, err)
	}
	if pending, _ := reopened.PendingWatches(ctx); len(pending) != 0 {
		t.Fatalf("fired watch still pending: %+v", pending)
	}
	// Once fired, the same watcher can watch the same target again.
	if _, created, err := reopened.AddWatch(ctx, "claude-code", "codex", 24*time.Hour); err != nil || !created {
		t.Fatalf("watch again after firing: %v %v", created, err)
	}
}

// A watch that never fired lapses after its 24 hours, and the same watch
// can then be set again.
func TestWatchExpires(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	w, _, err := s.AddWatch(ctx, "claude-code", "codex", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(24 * time.Hour)
	if pending, _ := s.PendingWatches(ctx); len(pending) != 0 {
		t.Fatalf("expired watch pending: %+v", pending)
	}
	if _, ok, err := s.FireWatch(ctx, w, idleNotice(w), time.Hour); ok || err != nil {
		t.Fatalf("fired an expired watch: %v %v", ok, err)
	}
	if _, created, err := s.AddWatch(ctx, "claude-code", "codex", 24*time.Hour); err != nil || !created {
		t.Fatalf("watch again after expiry: %v %v", created, err)
	}
}

// Removing an agent drops the watches it set and the watches on it, so an
// agent joined again under the same name starts with none.
func TestDeleteAgentDropsWatches(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	for _, name := range []string{"codex", "claude-code", "grokbot"} {
		if err := s.PutAgent(ctx, identity.Agent{Name: name, NodeID: "n" + name, NodeName: name, JoinedAt: c.t}); err != nil {
			t.Fatal(err)
		}
	}
	for _, w := range [][2]string{{"claude-code", "codex"}, {"codex", "grokbot"}, {"claude-code", "grokbot"}} {
		if _, _, err := s.AddWatch(ctx, w[0], w[1], 24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteAgent(ctx, "codex"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingWatches(ctx)
	if err != nil || len(pending) != 1 || pending[0].Watcher != "claude-code" || pending[0].Target != "grokbot" {
		t.Fatalf("pending = %+v %v", pending, err)
	}
}
