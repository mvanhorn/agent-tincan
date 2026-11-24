package gateway

import (
	"net/http"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/webhooksign"
)

// ApplyWebhookHeaders attaches Standard Webhooks signing headers and
// X-MCP-Subscription-Id to req. webhook-id is always set to eventID.
func ApplyWebhookHeaders(req *http.Request, secret, subscriptionID, eventID string, ts time.Time, body []byte) error {
	if _, err := webhooksign.SetHeaders(req, secret, eventID, ts, body); err != nil {
		return err
	}
	if subscriptionID != "" {
		req.Header.Set("X-MCP-Subscription-Id", subscriptionID)
	}
	return nil
}
