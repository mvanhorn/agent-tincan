package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// --notes-ttl defaults to 30 days, reaches the relay config, and must be
// positive.
func TestRelayNotesTTLFlag(t *testing.T) {
	cmd := relayCmd()
	if err := cmd.Flags().Parse(nil); err != nil {
		t.Fatal(err)
	}
	if d, err := cmd.Flags().GetDuration("notes-ttl"); err != nil || d != 30*24*time.Hour {
		t.Fatalf("default --notes-ttl = %v %v, want 720h", d, err)
	}
	if got := (relayFlags{notesTTL: 72 * time.Hour}).relayConfig().NotesRequestTTL; got != 72*time.Hour {
		t.Fatalf("relay config notes ttl = %v, want 72h", got)
	}
	bad := relayCmd()
	if err := bad.Flags().Parse([]string{"--notes-ttl", "-1h"}); err != nil {
		t.Fatal(err)
	}
	if err := bad.RunE(bad, nil); err == nil || !strings.Contains(err.Error(), "--notes-ttl") {
		t.Fatalf("negative --notes-ttl: %v", err)
	}
}

// --wake-grace defaults to 10 minutes, reaches the relay config, and must be
// positive.
func TestRelayWakeGraceFlag(t *testing.T) {
	cmd := relayCmd()
	if err := cmd.Flags().Parse(nil); err != nil {
		t.Fatal(err)
	}
	if d, err := cmd.Flags().GetDuration("wake-grace"); err != nil || d != 10*time.Minute {
		t.Fatalf("default --wake-grace = %v %v, want 10m", d, err)
	}
	if got := (relayFlags{notesTTL: time.Hour, wakeGrace: 2 * time.Minute}).relayConfig().WakeGrace; got != 2*time.Minute {
		t.Fatalf("relay config wake grace = %v, want 2m", got)
	}
	opts := wakerOptions(relayFlags{wakeGrace: 2 * time.Minute, replyGrace: 45 * time.Second}, new(relay.Server))
	if opts.WakeGrace != 2*time.Minute {
		t.Fatalf("waker WakeGrace = %v, want 2m", opts.WakeGrace)
	}
	if opts.LastPoll == nil {
		t.Fatal("waker LastPoll callback missing")
	}
	if wake.DefaultWakeGrace != relay.DefaultWakeGrace {
		t.Fatalf("waker default %v, relay default %v", wake.DefaultWakeGrace, relay.DefaultWakeGrace)
	}
	bad := relayCmd()
	if err := bad.Flags().Parse([]string{"--wake-grace", "0s"}); err != nil {
		t.Fatal(err)
	}
	if err := bad.RunE(bad, nil); err == nil || !strings.Contains(err.Error(), "--wake-grace") {
		t.Fatalf("zero --wake-grace: %v", err)
	}
}

// --urgent-wake-grace defaults to 2 minutes, reaches the relay config and
// the waker with the urgent count and the asker notice hook, and must be
// positive.
func TestRelayUrgentWakeGraceFlag(t *testing.T) {
	cmd := relayCmd()
	if err := cmd.Flags().Parse(nil); err != nil {
		t.Fatal(err)
	}
	if d, err := cmd.Flags().GetDuration("urgent-wake-grace"); err != nil || d != 2*time.Minute {
		t.Fatalf("default --urgent-wake-grace = %v %v, want 2m", d, err)
	}
	f := relayFlags{notesTTL: time.Hour, wakeGrace: 10 * time.Minute, urgentGrace: 90 * time.Second}
	if got := f.relayConfig().UrgentWakeGrace; got != 90*time.Second {
		t.Fatalf("relay config urgent wake grace = %v, want 90s", got)
	}
	opts := wakerOptions(f, new(relay.Server))
	if opts.UrgentWakeGrace != 90*time.Second || opts.WakeGrace != 10*time.Minute {
		t.Fatalf("waker graces = %v urgent, %v normal", opts.UrgentWakeGrace, opts.WakeGrace)
	}
	if opts.UrgentQueued == nil || opts.Unanswered == nil {
		t.Fatal("waker UrgentQueued or Unanswered callback missing")
	}
	if wake.DefaultUrgentWakeGrace != relay.DefaultUrgentWakeGrace {
		t.Fatalf("waker default %v, relay default %v", wake.DefaultUrgentWakeGrace, relay.DefaultUrgentWakeGrace)
	}
	bad := relayCmd()
	if err := bad.Flags().Parse([]string{"--urgent-wake-grace", "0s"}); err != nil {
		t.Fatal(err)
	}
	if err := bad.RunE(bad, nil); err == nil || !strings.Contains(err.Error(), "--urgent-wake-grace") {
		t.Fatalf("zero --urgent-wake-grace: %v", err)
	}
}

// --urgent-claim-lease defaults to 10 minutes, reaches the relay config, and
// must be positive.
func TestRelayUrgentClaimLeaseFlag(t *testing.T) {
	cmd := relayCmd()
	if err := cmd.Flags().Parse(nil); err != nil {
		t.Fatal(err)
	}
	if d, err := cmd.Flags().GetDuration("urgent-claim-lease"); err != nil || d != 10*time.Minute {
		t.Fatalf("default --urgent-claim-lease = %v %v, want 10m", d, err)
	}
	f := relayFlags{notesTTL: time.Hour, wakeGrace: time.Minute, urgentGrace: time.Minute, urgentLease: 5 * time.Minute}
	if got := f.relayConfig().UrgentClaimLease; got != 5*time.Minute {
		t.Fatalf("relay config urgent claim lease = %v, want 5m", got)
	}
	bad := relayCmd()
	if err := bad.Flags().Parse([]string{"--urgent-claim-lease", "0s"}); err != nil {
		t.Fatal(err)
	}
	if err := bad.RunE(bad, nil); err == nil || !strings.Contains(err.Error(), "--urgent-claim-lease") {
		t.Fatalf("zero --urgent-claim-lease: %v", err)
	}
}

// --owner-notice-after defaults to 3, reaches the relay config, and must be
// at least 1.
func TestRelayOwnerNoticeAfterFlag(t *testing.T) {
	cmd := relayCmd()
	if err := cmd.Flags().Parse(nil); err != nil {
		t.Fatal(err)
	}
	if n, err := cmd.Flags().GetInt("owner-notice-after"); err != nil || n != 3 {
		t.Fatalf("default --owner-notice-after = %v %v, want 3", n, err)
	}
	if got := (relayFlags{ownerNotice: 5}).relayConfig().OwnerNoticeAfter; got != 5 {
		t.Fatalf("relay config owner notice after = %v, want 5", got)
	}
	bad := relayCmd()
	if err := bad.Flags().Parse([]string{"--owner-notice-after", "0"}); err != nil {
		t.Fatal(err)
	}
	if err := bad.RunE(bad, nil); err == nil || !strings.Contains(err.Error(), "--owner-notice-after") {
		t.Fatalf("zero --owner-notice-after: %v", err)
	}
}

// The relay polls for email replies only from agents with include_requests
// on, in name order, each with its own address and inbox.
func TestEmailReplyInboxes(t *testing.T) {
	cfg := wake.Config{
		"instinct": {Method: wake.Email, EmailTo: "i@example.com", AgentMailFrom: "a@agentmail.to", AgentMailKey: "k1", IncludeRequests: true},
		"alpha":    {Method: wake.Email, EmailTo: "alpha@example.com", AgentMailFrom: "b@agentmail.to", AgentMailKey: "k2", IncludeRequests: true},
		"grokbot":  {Method: wake.Email, EmailTo: "g@example.com", AgentMailFrom: "a@agentmail.to", AgentMailKey: "k1"},
		"hermes":   {Method: wake.Webhook, URL: "https://h.example"},
	}
	got := emailReplyInboxes(cfg)
	if len(got) != 2 || got[0].Agent != "alpha" || got[0].Address != "alpha@example.com" || got[1].Agent != "instinct" || got[1].Address != "i@example.com" {
		t.Fatalf("inboxes = %+v, want alpha and instinct", got)
	}
	for _, in := range got {
		if in.Mail == nil {
			t.Fatalf("%s has no mailbox", in.Agent)
		}
	}
	if emailReplyInboxes(wake.Config{"grokbot": cfg["grokbot"]}) != nil {
		t.Fatal("an agent without include_requests is polled")
	}
}
