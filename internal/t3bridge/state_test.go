package t3bridge

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStateStoreRoundTripAndPrune(t *testing.T) {
	now := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)
	st := NewStateStore(filepath.Join(t.TempDir(), "t3-state.json"))
	st.now = func() time.Time { return now }

	if err := st.Update(func(d *StateData) error {
		d.Cursors["thread.run_settled"] = "cur-1"
		d.Asks["old-done"] = DispatchedAsk{RequestID: "old-done", Done: true, UpdatedAt: now.Add(-8 * 24 * time.Hour)}
		d.Asks["active-1"] = DispatchedAsk{RequestID: "active-1", ThreadID: "thr-1", UpdatedAt: now}
		d.Watches["thr-9"] = []ThreadWatch{{ThreadID: "thr-9", NotifyAgent: "grokbot", CreatedAt: now}}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	snap, err := st.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Cursors["thread.run_settled"] != "cur-1" {
		t.Fatalf("cursor lost: %+v", snap.Cursors)
	}
	if _, ok := snap.Asks["old-done"]; ok {
		t.Fatal("stale completed ask was not pruned")
	}
	if snap.Asks["active-1"].ThreadID != "thr-1" {
		t.Fatalf("active ask lost: %+v", snap.Asks)
	}
	if len(snap.Watches["thr-9"]) != 1 {
		t.Fatalf("watch lost: %+v", snap.Watches)
	}
}

func TestRouteStoreRoundTripAndPrune(t *testing.T) {
	now := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)
	rs := NewRouteStore(filepath.Join(t.TempDir(), "t3-routes.json"))
	rs.now = func() time.Time { return now }

	if err := rs.Record("req-1", "thr-a", "grokbot"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rt, ok, err := rs.Lookup("req-1")
	if err != nil || !ok || rt.ThreadID != "thr-a" {
		t.Fatalf("Lookup = %+v, %v, %v", rt, ok, err)
	}
	if err := rs.MarkDelivered("req-1", 2); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	rt, ok, err = rs.Lookup("req-1")
	if err != nil || !ok || rt.DeliveredGen != 2 {
		t.Fatalf("DeliveredGen = %d, want 2", rt.DeliveredGen)
	}

	// Advance clock past retention and record a new entry to trigger prune.
	now = now.Add(8 * 24 * time.Hour)
	if err := rs.Record("req-2", "thr-b", "muse"); err != nil {
		t.Fatalf("Record req-2: %v", err)
	}
	if _, ok, _ := rs.Lookup("req-1"); ok {
		t.Fatal("expired route req-1 was not pruned")
	}
}
