package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two relays' worth of connections on one database, each racing to take the
// same resource: exactly one wins, and every loser is told who did.
func TestAcquireLeaseConcurrentOneHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	a, c := open(t, path)
	b, _ := open(t, path)
	ctx := context.Background()
	var names []string
	for i := range 8 {
		names = append(names, fmt.Sprintf("agent-%d", i))
	}
	joinAll(t, a, c, names...)
	var won atomic.Int32
	var winner atomic.Value
	losers := make(chan *LeaseHeldError, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		st := a
		if i%2 == 1 {
			st = b
		}
		wg.Go(func() {
			_, err := st.AcquireLease(ctx, name, "repo/*", "", time.Hour, 32)
			var held *LeaseHeldError
			switch {
			case err == nil:
				won.Add(1)
				winner.Store(name)
			case errors.As(err, &held):
				losers <- held
			default:
				t.Errorf("%s: %v", name, err)
			}
		})
	}
	wg.Wait()
	close(losers)
	if won.Load() != 1 {
		t.Fatalf("%d agents hold the lease", won.Load())
	}
	for held := range losers {
		if held.Lease.Holder != winner.Load() {
			t.Fatalf("told %q holds it, want %q", held.Lease.Holder, winner.Load())
		}
	}
}

// A conflict hands back the lease in the way: its holder and its expiry.
func TestAcquireLeaseConflictNamesHolderAndExpiry(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse", "codex")
	got, err := s.AcquireLease(ctx, "muse", "docs/guide", "rewriting", time.Hour, 32)
	if err != nil || got.Renewed || got.Lease.Holder != "muse" || got.Lease.Note != "rewriting" {
		t.Fatalf("acquire = %+v, %v", got, err)
	}
	_, err = s.AcquireLease(ctx, "codex", "docs/guide", "", time.Hour, 32)
	var held *LeaseHeldError
	if !errors.As(err, &held) || held.Lease.Holder != "muse" || held.Lease.Resource != "docs/guide" || !held.Lease.Expires.Equal(c.t.Add(time.Hour)) {
		t.Fatalf("conflict = %v (%+v)", err, held)
	}
}

// A prefix conflicts with what it covers, whichever came first, and only
// that. Interior stars are literal, and the relay never reads a path.
func TestLeasesOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
		{"a/*", "a/b", true},
		{"a/*", "a/b/c", true},
		{"a/*", "ab", false},
		{"a/*", "a", false},
		{"a/*", "a/", true},
		{"a/*", "a/b/*", true},
		{"a/*", "b/*", false},
		{"a/*", "a/*", true},
		{"a*", "ab", true},
		{"a/**", "a/b", true},
		{"a/**", "a/*", true},
		{"a*b", "a/b", false},
		{"a*b", "a*b", true},
		{"*", "anything", true},
	}
	for _, tc := range cases {
		if got := LeasesOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("LeasesOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := LeasesOverlap(tc.b, tc.a); got != tc.want {
			t.Errorf("LeasesOverlap(%q, %q) = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}

// Overlap refuses a teammate in both orders, end to end through the table.
func TestAcquireLeasePrefixConflictsBothOrders(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse", "codex")
	if _, err := s.AcquireLease(ctx, "muse", "a/*", "", time.Hour, 32); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease(ctx, "codex", "x/y", "", time.Hour, 32); err != nil {
		t.Fatal(err)
	}
	var held *LeaseHeldError
	if _, err := s.AcquireLease(ctx, "codex", "a/b", "", time.Hour, 32); !errors.As(err, &held) {
		t.Fatalf("exact under a held prefix: %v", err)
	}
	if _, err := s.AcquireLease(ctx, "muse", "x/*", "", time.Hour, 32); !errors.As(err, &held) || held.Lease.Resource != "x/y" {
		t.Fatalf("prefix over a held exact: %v", err)
	}
	if _, err := s.AcquireLease(ctx, "codex", "ab", "", time.Hour, 32); err != nil {
		t.Fatalf("a/* must not cover ab: %v", err)
	}
}

// An agent's own leases never conflict with each other.
func TestAcquireLeaseOwnOverlapIsAllowed(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse")
	for _, res := range []string{"a/*", "a/b"} {
		if _, err := s.AcquireLease(ctx, "muse", res, "", time.Hour, 32); err != nil {
			t.Fatalf("%s: %v", res, err)
		}
	}
}

// A lease lapses with no call from its holder: reads skip it at once, a
// teammate can take the resource, and the lapse is reported exactly once,
// to whichever of the sweep and an acquire clears the row.
func TestExpiredLeaseCanBeTaken(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse", "codex")
	if _, err := s.AcquireLease(ctx, "muse", "a/*", "", time.Hour, 32); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	if live, err := s.Leases(ctx); err != nil || len(live) != 0 {
		t.Fatalf("lapsed lease listed: %+v %v", live, err)
	}
	got, err := s.AcquireLease(ctx, "codex", "a/b", "", time.Hour, 32)
	if err != nil || got.Lease.Holder != "codex" || got.Renewed {
		t.Fatalf("take over = %+v, %v", got, err)
	}
	if len(got.Lapsed) != 1 || got.Lapsed[0].Holder != "muse" || got.Lapsed[0].Resource != "a/*" {
		t.Fatalf("lapsed = %+v", got.Lapsed)
	}
	if swept, err := s.ExpireLeases(ctx); err != nil || len(swept) != 0 {
		t.Fatalf("swept again: %+v %v", swept, err)
	}
}

// The clock is read once the write lock is held. A lease that lapses while an
// acquire waits for that lock frees its resource, and the new lease's expiry
// starts from after the wait, so a short lease is not already over when
// granted. The wait can be for this store's one connection or for another
// connection on the file.
func TestAcquireLeaseReadsClockAfterWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, c := open(t, path)
	other, _ := open(t, path)
	ctx := context.Background()
	joinAll(t, s, c, "muse", "codex")
	var waited atomic.Int64 // read by the acquire while the test moves it
	s.SetClock(func() time.Time { return c.now().Add(time.Duration(waited.Load())) })
	for name, holder := range map[string]*Store{"same store": s, "other connection": other} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.AcquireLease(ctx, "muse", name, "", time.Minute, 32); err != nil {
				t.Fatal(err)
			}
			tx, err := holder.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `DELETE FROM work_leases WHERE resource = 'no such lease'`); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			var got LeaseGrant
			go func() {
				var err error
				got, err = s.AcquireLease(ctx, "codex", name, "", time.Second, 32)
				done <- err
			}()
			time.Sleep(100 * time.Millisecond) // let the acquire reach the lock
			waited.Add(int64(time.Hour))
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("acquire past a lease that lapsed in the wait: %v", err)
			}
			if want := s.now().Add(time.Second); !got.Lease.Expires.Equal(want) {
				t.Fatalf("expires %v, want %v", got.Lease.Expires, want)
			}
			if len(got.Lapsed) != 1 || got.Lapsed[0].Holder != "muse" {
				t.Fatalf("lapsed = %+v", got.Lapsed)
			}
			if _, err := s.ReleaseLease(ctx, name, "codex"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A holder whose lease lapsed gets a fresh lease, not a renewal.
func TestExpiredOwnLeaseIsNotARenewal(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse")
	if _, err := s.AcquireLease(ctx, "muse", "a", "old", time.Hour, 32); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Hour)
	got, err := s.AcquireLease(ctx, "muse", "a", "", time.Hour, 32)
	if err != nil || got.Renewed || got.Lease.Note != "" || !got.Lease.Created.Equal(c.t) {
		t.Fatalf("after lapse = %+v, %v", got, err)
	}
}

// The sweep clears lapsed rows and returns them. A refused acquire rolls
// back, so what it cleared stays for the sweep to report.
func TestExpireLeases(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse", "codex")
	for _, step := range []struct {
		holder, res string
		ttl         time.Duration
	}{{"muse", "gone", time.Minute}, {"muse", "kept", time.Hour}} {
		if _, err := s.AcquireLease(ctx, step.holder, step.res, "", step.ttl, 32); err != nil {
			t.Fatal(err)
		}
	}
	c.advance(time.Minute)
	if _, err := s.AcquireLease(ctx, "codex", "kept", "", time.Hour, 32); err == nil {
		t.Fatal("took a live lease")
	}
	swept, err := s.ExpireLeases(ctx)
	if err != nil || len(swept) != 1 || swept[0].Resource != "gone" {
		t.Fatalf("swept = %+v, %v", swept, err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM work_leases`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d, %v", rows, err)
	}
}

// Acquiring what you hold renews it: a new expiry, the old start, and the
// old note unless a new one is given.
func TestRenewLease(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse")
	first, err := s.AcquireLease(ctx, "muse", "a", "note", time.Hour, 32)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(30 * time.Minute)
	got, err := s.AcquireLease(ctx, "muse", "a", "", 2*time.Hour, 32)
	if err != nil || !got.Renewed || got.Lease.Note != "note" || !got.Lease.Created.Equal(first.Lease.Created) || !got.Lease.Expires.Equal(c.t.Add(2*time.Hour)) {
		t.Fatalf("renew = %+v, %v", got, err)
	}
	got, err = s.AcquireLease(ctx, "muse", "a", "new", time.Hour, 32)
	if err != nil || got.Lease.Note != "new" {
		t.Fatalf("renew with note = %+v, %v", got, err)
	}
}

// The cap counts live leases: the 33rd is refused, a renewal at the cap is
// not, and a lapsed lease frees its slot.
func TestAcquireLeaseLimit(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse", "codex")
	for i := range 32 {
		ttl := time.Hour
		if i == 0 {
			ttl = time.Minute
		}
		if _, err := s.AcquireLease(ctx, "muse", fmt.Sprintf("r/%d", i), "", ttl, 32); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AcquireLease(ctx, "muse", "r/33", "", time.Hour, 32); !errors.Is(err, ErrLeaseLimit) {
		t.Fatalf("33rd lease: %v", err)
	}
	if got, err := s.AcquireLease(ctx, "muse", "r/1", "", time.Hour, 32); err != nil || !got.Renewed {
		t.Fatalf("renew at the cap: %+v %v", got, err)
	}
	if _, err := s.AcquireLease(ctx, "codex", "other", "", time.Hour, 32); err != nil {
		t.Fatalf("the cap is per agent: %v", err)
	}
	c.advance(time.Minute)
	if _, err := s.AcquireLease(ctx, "muse", "r/33", "", time.Hour, 32); err != nil {
		t.Fatalf("after a lapse: %v", err)
	}
}

// Only the holder releases; an empty holder is the admin's release of any
// lease. A lapsed lease is gone, even to its holder.
func TestReleaseLease(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse", "codex")
	if _, err := s.AcquireLease(ctx, "muse", "a", "", time.Hour, 32); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseLease(ctx, "a", "codex"); !errors.Is(err, ErrNotLeaseHolder) {
		t.Fatalf("non-holder release: %v", err)
	}
	if got, err := s.ReleaseLease(ctx, "a", "muse"); err != nil || got.Holder != "muse" {
		t.Fatalf("holder release = %+v, %v", got, err)
	}
	if _, err := s.ReleaseLease(ctx, "a", "muse"); !errors.Is(err, ErrNoLease) {
		t.Fatalf("second release: %v", err)
	}
	if _, err := s.AcquireLease(ctx, "muse", "b", "", time.Hour, 32); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReleaseLease(ctx, "b", ""); err != nil || got.Holder != "muse" {
		t.Fatalf("admin release = %+v, %v", got, err)
	}
	if _, err := s.AcquireLease(ctx, "muse", "c", "", time.Minute, 32); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if _, err := s.ReleaseLease(ctx, "c", "muse"); !errors.Is(err, ErrNoLease) {
		t.Fatalf("lapsed release: %v", err)
	}
}

// Releasing names the lease exactly: a prefix release leaves what it would
// have covered alone.
func TestReleaseLeaseIsExact(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse")
	if _, err := s.AcquireLease(ctx, "muse", "a/b", "", time.Hour, 32); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseLease(ctx, "a/*", "muse"); !errors.Is(err, ErrNoLease) {
		t.Fatalf("prefix release: %v", err)
	}
	if live, err := s.Leases(ctx); err != nil || len(live) != 1 {
		t.Fatalf("live = %+v, %v", live, err)
	}
}

// Removing an agent takes its leases with it, so an agent joined later
// under the name inherits nothing, and a removed agent cannot take one.
func TestDeleteAgentClearsLeases(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	joinAll(t, s, c, "muse", "codex")
	for _, holder := range []string{"muse", "codex"} {
		if _, err := s.AcquireLease(ctx, holder, "of/"+holder, "", time.Hour, 32); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteAgent(ctx, "muse"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease(ctx, "muse", "x", "", time.Hour, 32); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("removed agent acquired: %v", err)
	}
	joinAll(t, s, c, "muse")
	live, err := s.Leases(ctx)
	if err != nil || len(live) != 1 || live[0].Holder != "codex" {
		t.Fatalf("live = %+v, %v", live, err)
	}
}

// A lease outlives its agent's rebind, which re-inserts the agent row, and
// a relay restart.
func TestLeaseSurvivesRebindAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, c := open(t, path)
	ctx := context.Background()
	joinAll(t, s, c, "muse")
	if _, err := s.AcquireLease(ctx, "muse", "a", "", time.Hour, 32); err != nil {
		t.Fatal(err)
	}
	joinAll(t, s, c, "muse")
	s.Close()
	again, c2 := open(t, path)
	c2.t = c.t
	live, err := again.Leases(ctx)
	if err != nil || len(live) != 1 || live[0].Holder != "muse" {
		t.Fatalf("live = %+v, %v", live, err)
	}
}
