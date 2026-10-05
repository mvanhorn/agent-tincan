package store

import (
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"path/filepath"
	"testing"
	"time"
)

func TestWebStatusOrderingAndIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	a := identity.Agent{Name: "web", NodeID: "one", JoinedAt: now}
	if err := s.PutAgent(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	report := func(at time.Time, state string) {
		t.Helper()
		if err := s.SetWebStatus(t.Context(), a, envelope.WebStatus{Site: "chatgpt.com", State: state, ObservedAt: at}, now); err != nil {
			t.Fatal(err)
		}
	}
	report(now, "signed_out")
	report(now.Add(time.Second), "signed_out")
	facts, err := s.WebStatuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !facts[a.Name].Since.Equal(now) {
		t.Fatalf("since = %v", facts[a.Name].Since)
	}
	report(now.Add(2*time.Second), "authenticated")
	report(now.Add(time.Second), "signed_out")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	facts, err = s.WebStatuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !facts[a.Name].Since.IsZero() || !facts[a.Name].ObservedAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("clear tombstone lost: %+v", facts)
	}
	a.NodeID = "two"
	if err := s.PutAgent(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	facts, err = s.WebStatuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("replacement inherited health: %+v", facts)
	}
}

func TestWebStatusNoticeDeduplication(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	a := identity.Agent{Name: "web", NodeID: "one", JoinedAt: now}
	for _, agent := range []identity.Agent{a, {Name: "owner", NodeID: "two", JoinedAt: now}} {
		if err := s.PutAgent(t.Context(), agent); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetWebStatus(t.Context(), a, envelope.WebStatus{Site: "chatgpt.com", State: "signed_out", ObservedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	facts, err := s.WebStatuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		req, err := s.EnqueueWebNotice(t.Context(), a.Name, facts[a.Name], "owner", "metadata", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if (req.ID != "") != (i == 0) {
			t.Fatalf("notice %d: %+v", i, req)
		}
	}
	if err := s.SetWebStatus(t.Context(), a, envelope.WebStatus{Site: "chatgpt.com", State: "authenticated", ObservedAt: now.Add(time.Second)}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWebStatus(t.Context(), a, envelope.WebStatus{Site: "chatgpt.com", State: "signed_out", ObservedAt: now.Add(2 * time.Second)}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	facts, err = s.WebStatuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !facts[a.Name].Pending {
		t.Fatal("new episode did not enable notice")
	}
	if err := s.SetWebStatus(t.Context(), a, envelope.WebStatus{Site: "chatgpt.com", State: "authenticated", ObservedAt: now.Add(3 * time.Second)}, now); err != nil {
		t.Fatal(err)
	}
	req, err := s.EnqueueWebNotice(t.Context(), a.Name, facts[a.Name], "owner", "obsolete", time.Hour)
	if err != nil || req.ID != "" {
		t.Fatalf("recovered episode queued: %+v %v", req, err)
	}
}
