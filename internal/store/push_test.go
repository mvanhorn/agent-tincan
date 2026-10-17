package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A page that subscribes again with the same endpoint updates its one row
// (new keys, device and time); another endpoint gets its own.
func TestPushSubscriptionUpsert(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	first, created, err := s.UpsertPushSubscription(ctx, PushSubscription{Endpoint: "https://web.push.apple.com/a", P256dh: "k1", Auth: "a1", Device: "iphone", Node: "nPHONE"})
	if err != nil || !created || first.ID == "" || !first.CreatedAt.Equal(c.t) {
		t.Fatalf("first = %+v %v %v", first, created, err)
	}
	c.t = c.t.Add(time.Hour)
	again, created, err := s.UpsertPushSubscription(ctx, PushSubscription{Endpoint: "https://web.push.apple.com/a", P256dh: "k2", Auth: "a2", Device: "iphone", Node: "nPHONE"})
	if err != nil || created || again.ID != first.ID || again.P256dh != "k2" || !again.UpdatedAt.Equal(c.t) || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("again = %+v %v %v", again, created, err)
	}
	if _, created, err := s.UpsertPushSubscription(ctx, PushSubscription{Endpoint: "https://fcm.googleapis.com/b", P256dh: "k", Auth: "a"}); err != nil || !created {
		t.Fatalf("second endpoint: %v %v", created, err)
	}
	subs, err := s.PushSubscriptions(ctx)
	if err != nil || len(subs) != 2 || subs[0].Endpoint != "https://web.push.apple.com/a" || subs[0].Auth != "a2" {
		t.Fatalf("subscriptions = %+v %v", subs, err)
	}
}

// Deleting by endpoint (the page unsubscribing) or by id (the push service
// answering 404 or 410) removes the row and reports its id.
func TestPushSubscriptionDelete(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	a, _, _ := s.UpsertPushSubscription(ctx, PushSubscription{Endpoint: "https://web.push.apple.com/a", P256dh: "k", Auth: "a"})
	b, _, _ := s.UpsertPushSubscription(ctx, PushSubscription{Endpoint: "https://web.push.apple.com/b", P256dh: "k", Auth: "a"})
	id, ok, err := s.DeletePushSubscription(ctx, "https://web.push.apple.com/a")
	if err != nil || !ok || id != a.ID {
		t.Fatalf("delete by endpoint = %q %v %v", id, ok, err)
	}
	if _, ok, err := s.DeletePushSubscription(ctx, "https://web.push.apple.com/a"); err != nil || ok {
		t.Fatalf("second delete = %v %v", ok, err)
	}
	if ok, err := s.DeletePushSubscriptionID(ctx, b.ID); err != nil || !ok {
		t.Fatalf("delete by id = %v %v", ok, err)
	}
	if subs, err := s.PushSubscriptions(ctx); err != nil || len(subs) != 0 {
		t.Fatalf("left = %+v %v", subs, err)
	}
}

// The push table is added in place: reopening a database migrates nothing
// twice and keeps the subscriptions.
func TestPushSubscriptionsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertPushSubscription(context.Background(), PushSubscription{Endpoint: "https://web.push.apple.com/a", P256dh: "k", Auth: "a"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if subs, err := s.PushSubscriptions(context.Background()); err != nil || len(subs) != 1 {
		t.Fatalf("after reopen = %+v %v", subs, err)
	}
}
