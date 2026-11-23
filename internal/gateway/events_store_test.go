package gateway

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/webhooksign"
)

func openTestEventStore(t *testing.T, now func() time.Time) *EventStore {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	es, err := NewEventStore(st.DB(), now)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestEventStoreIsolationRefreshExpireAndDelete(t *testing.T) {
	now := time.Date(2026, 11, 22, 19, 0, 0, 0, time.UTC)
	es := openTestEventStore(t, func() time.Time { return now })
	ctx := context.Background()

	secretA, _ := webhooksign.GenerateSecret()
	subA, err := es.CreateSubscription(ctx, EventSubscription{
		ID:            "sub_a1",
		Agent:         "chatgpt",
		ClientID:      "client-a",
		EventName:     EventInboxRequest,
		Filters:       map[string]any{"urgent_only": true},
		CallbackURL:   "https://hooks.example.com/cb?token=sensitive-query",
		SigningSecret: secretA,
	}, 2*time.Hour)
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if !subA.ExpiresAt.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("expires_at = %v, want %v", subA.ExpiresAt, now.Add(2*time.Hour))
	}

	// Agent B cannot refresh or delete Agent A's subscription.
	if _, err := es.RefreshSubscription(ctx, "sub_a1", "sesame", "client-a", time.Hour); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("agent B refresh: err=%v, want ErrSubscriptionNotFound", err)
	}
	if deleted, err := es.DeleteSubscription(ctx, "sub_a1", "sesame", "client-a"); err != nil || deleted {
		t.Fatalf("agent B delete: deleted=%v err=%v, want false,nil", deleted, err)
	}
	// Same agent with a different client_id also cannot touch it.
	if _, err := es.RefreshSubscription(ctx, "sub_a1", "chatgpt", "client-other", time.Hour); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("other client refresh: err=%v, want ErrSubscriptionNotFound", err)
	}
	if deleted, err := es.DeleteSubscription(ctx, "sub_a1", "chatgpt", "client-other"); err != nil || deleted {
		t.Fatalf("other client delete: deleted=%v err=%v, want false,nil", deleted, err)
	}

	// Summary never leaks signing secret or query string.
	summary, err := es.Summary(ctx)
	if err != nil || len(summary) != 1 {
		t.Fatalf("Summary: len=%d err=%v", len(summary), err)
	}
	if strings.Contains(summary[0].Callback, "sensitive-query") {
		t.Fatalf("Summary leaked callback query string: %q", summary[0].Callback)
	}
	if summary[0].Callback != "https://hooks.example.com/cb" {
		t.Fatalf("Summary callback = %q", summary[0].Callback)
	}

	// Owner refreshes and extends beyond MaxSubTTL -> clamped to MaxSubTTL.
	now = now.Add(time.Hour)
	refreshed, err := es.RefreshSubscription(ctx, "sub_a1", "chatgpt", "client-a", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("RefreshSubscription: %v", err)
	}
	if !refreshed.ExpiresAt.Equal(now.Add(MaxSubTTL)) {
		t.Fatalf("refreshed expires_at = %v, want %v", refreshed.ExpiresAt, now.Add(MaxSubTTL))
	}

	// Advance past expiry: HasActiveSubscription becomes false and SweepExpired removes it.
	now = refreshed.ExpiresAt.Add(time.Second)
	has, err := es.HasActiveSubscription(ctx, "chatgpt")
	if err != nil || has {
		t.Fatalf("HasActiveSubscription after expiry = %v, %v; want false", has, err)
	}
	swept, err := es.SweepExpired(ctx)
	if err != nil || swept != 1 {
		t.Fatalf("SweepExpired = %d, %v; want 1", swept, err)
	}
}

func TestEventStorePerAgentCap(t *testing.T) {
	now := time.Date(2026, 11, 22, 19, 0, 0, 0, time.UTC)
	es := openTestEventStore(t, func() time.Time { return now })
	ctx := context.Background()
	secret, _ := webhooksign.GenerateSecret()

	for i := range MaxSubsPerAgent {
		_, err := es.CreateSubscription(ctx, EventSubscription{
			ID:            fmt.Sprintf("sub_%d", i),
			Agent:         "chatgpt",
			ClientID:      "client-a",
			EventName:     EventInboxRequest,
			CallbackURL:   "https://hooks.example.com/cb",
			SigningSecret: secret,
		}, time.Hour)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	// 17th active subscription for chatgpt must fail.
	_, err := es.CreateSubscription(ctx, EventSubscription{
		ID:            "sub_overflow",
		Agent:         "chatgpt",
		ClientID:      "client-a",
		EventName:     EventInboxRequest,
		CallbackURL:   "https://hooks.example.com/cb",
		SigningSecret: secret,
	}, time.Hour)
	if !errors.Is(err, ErrTooManySubscriptions) {
		t.Fatalf("17th sub err = %v, want ErrTooManySubscriptions", err)
	}

	// Another agent still has its own quota.
	if _, err := es.CreateSubscription(ctx, EventSubscription{
		ID:            "sub_other_agent",
		Agent:         "sesame",
		ClientID:      "client-b",
		EventName:     EventInboxRequest,
		CallbackURL:   "https://hooks.example.com/cb",
		SigningSecret: secret,
	}, time.Hour); err != nil {
		t.Fatalf("other agent sub failed: %v", err)
	}

	// After expiry, chatgpt can subscribe again.
	now = now.Add(2 * time.Hour)
	if _, err := es.CreateSubscription(ctx, EventSubscription{
		ID:            "sub_after_expiry",
		Agent:         "chatgpt",
		ClientID:      "client-a",
		EventName:     EventInboxRequest,
		CallbackURL:   "https://hooks.example.com/cb",
		SigningSecret: secret,
	}, time.Hour); err != nil {
		t.Fatalf("sub after expiry failed: %v", err)
	}
}
