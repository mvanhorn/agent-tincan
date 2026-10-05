package client

import (
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestSignedOutRenderingAndPromptReturn(t *testing.T) {
	now := time.Now()
	target := &envelope.Target{SignedOutSite: "copilot.com", SignedOutSince: now.Add(-2 * time.Hour), WebHost: "remote-mac"}
	if got := (AgentInfo{Target: *target}).SignedOutField(now); got != `signed_out="copilot.com since 2h"` {
		t.Fatal(got)
	}
	// No configured HTTP client: known sign-out must not attempt an inline GET.
	r := &Relay{}
	result, err := r.asked(t.Context(), sent{Request: envelope.Request{ID: "held-id", To: "web", Status: envelope.StatusHeld}, Target: target}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	text := FormatResult(result)
	for _, want := range []string{"remote-mac", "personal Microsoft account", "without restarting Chrome", "held-id", "owner's approval"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if got := (AgentInfo{}).SignedOutField(now); got != "" {
		t.Fatal(got)
	}
}
