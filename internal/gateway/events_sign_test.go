package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStandardWebhooksSignAndVerify(t *testing.T) {
	// Known test vector: 24-byte key base64-encoded with whsec_ prefix.
	rawKey := []byte("0123456789abcdef01234567")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(rawKey)
	eventID := "evt_test_12345"
	ts := int64(1732300000)
	body := []byte(`{"type":"event","eventId":"evt_test_12345","name":"inbox.request"}`)

	mac := hmac.New(sha256.New, rawKey)
	mac.Write([]byte("evt_test_12345.1732300000." + string(body)))
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	got, err := SignWebhook(secret, eventID, ts, body)
	if err != nil {
		t.Fatalf("SignWebhook: %v", err)
	}
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if !VerifyWebhookSignature(secret, eventID, ts, body, got) {
		t.Fatal("VerifyWebhookSignature rejected valid signature")
	}
	// Multiple space-separated signatures (rotation header).
	if !VerifyWebhookSignature(secret, eventID, ts, body, "v1,deadbeef "+got) {
		t.Fatal("VerifyWebhookSignature rejected valid rotated signature")
	}
	// Tampered body must fail.
	if VerifyWebhookSignature(secret, eventID, ts, append(body, ' '), got) {
		t.Fatal("VerifyWebhookSignature accepted tampered body")
	}
	// Raw base64 without whsec_ prefix produces the same signature.
	rawSecret := base64.StdEncoding.EncodeToString(rawKey)
	gotRaw, err := SignWebhook(rawSecret, eventID, ts, body)
	if err != nil || gotRaw != want {
		t.Fatalf("raw base64 secret: got=%q err=%v want=%q", gotRaw, err, want)
	}
}

func TestParseSigningSecretRejectsWeakOrMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"whsec_",
		"whsec_not-valid-base64!!!",
		"whsec_" + base64.StdEncoding.EncodeToString([]byte("too-short")),
		base64.StdEncoding.EncodeToString(make([]byte, 15)),
	} {
		if _, err := ParseSigningSecret(bad); err == nil {
			t.Errorf("ParseSigningSecret(%q) succeeded, want error", bad)
		}
	}
}

func TestApplyWebhookHeaders(t *testing.T) {
	secret, err := GenerateSigningSecret()
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
	if !VerifyWebhookSignature(secret, "evt_abc", 1732300123, body, sig) {
		t.Fatalf("webhook-signature %q failed verification", sig)
	}
}
