package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/webhooksign"
)

func TestApplyWebhookHeaders(t *testing.T) {
	secret, err := webhooksign.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":"verification","eventId":"evt_abc","challenge":"xyz"}`)
	ts := time.Unix(1732300123, 0).UTC()
	req := httptest.NewRequest(http.MethodPost, "https://example.com/hook", nil)
	if err := ApplyWebhookHeaders(req, secret, "sub_42", "evt_abc", ts, body); err != nil {
		t.Fatalf("ApplyWebhookHeaders: %v", err)
	}
	if got := req.Header.Get("webhook-id"); got != "evt_abc" {
		t.Fatalf("webhook-id = %q, want evt_abc", got)
	}
	if got := req.Header.Get("webhook-timestamp"); got != "1732300123" {
		t.Fatalf("webhook-timestamp = %q, want 1732300123", got)
	}
	if got := req.Header.Get("X-MCP-Subscription-Id"); got != "sub_42" {
		t.Fatalf("X-MCP-Subscription-Id = %q, want sub_42", got)
	}
	sig := req.Header.Get("webhook-signature")
	if !webhooksign.Verify(secret, "evt_abc", 1732300123, body, sig) {
		t.Fatalf("webhook-signature %q failed verification", sig)
	}
}
