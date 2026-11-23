package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func TestGatewayEventsCheck(t *testing.T) {
	exp := time.Date(2026, 11, 23, 19, 0, 0, 0, time.UTC)

	t.Run("not_enabled_returns_nil", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not enabled", http.StatusNotFound)
		}))
		defer srv.Close()
		r := client.NewRelayHTTP(srv.URL, srv.Client())
		if got := gatewayEventsCheck(context.Background(), r); got != nil {
			t.Fatalf("got %+v, want nil when gateway disabled", got)
		}
	})

	t.Run("healthy_subscription_ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"subscriptions": []map[string]any{{
					"id":         "sub_1",
					"agent":      "chatgpt",
					"event_name": "inbox.request",
					"callback":   "https://chatgpt.com/events",
					"expires_at": exp,
				}},
			})
		}))
		defer srv.Close()
		r := client.NewRelayHTTP(srv.URL, srv.Client())
		got := gatewayEventsCheck(context.Background(), r)
		if got == nil || got.Status != "ok" || !strings.Contains(got.Detail, "chatgpt inbox.request -> https://chatgpt.com/events") {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("failing_subscription_warns", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"subscriptions": []map[string]any{{
					"id":                   "sub_2",
					"agent":                "chatgpt",
					"event_name":           "request.replied",
					"callback":             "https://chatgpt.com/events",
					"expires_at":           exp,
					"consecutive_failures": 3,
					"last_error":           "HTTP 503",
				}},
			})
		}))
		defer srv.Close()
		r := client.NewRelayHTTP(srv.URL, srv.Client())
		got := gatewayEventsCheck(context.Background(), r)
		if got == nil || got.Status != "warn" || !strings.Contains(got.Detail, "failures=3") {
			t.Fatalf("got %+v", got)
		}
	})
}
