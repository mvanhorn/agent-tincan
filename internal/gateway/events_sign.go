package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// StandardWebhooksPrefix is the optional key prefix defined by the
	// Standard Webhooks specification.
	StandardWebhooksPrefix = "whsec_"

	minSigningSecretBytes = 16
	maxSigningSecretBytes = 1024
)

// ErrInvalidSigningSecret indicates a missing, malformed, or too-short signing secret.
var ErrInvalidSigningSecret = errors.New("invalid webhook signing secret")

// ParseSigningSecret decodes a Standard Webhooks signing secret. It accepts
// "whsec_<base64>" as well as raw base64 (standard or unpadded) and requires
// at least 16 decoded bytes.
func ParseSigningSecret(raw string) ([]byte, error) {
	if raw == "" {
		return nil, fmt.Errorf("%w: secret is required", ErrInvalidSigningSecret)
	}
	trimmed := strings.TrimPrefix(raw, StandardWebhooksPrefix)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty key after %s prefix", ErrInvalidSigningSecret, StandardWebhooksPrefix)
	}
	key, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(trimmed)
		if err != nil {
			return nil, fmt.Errorf("%w: base64 decode failed", ErrInvalidSigningSecret)
		}
	}
	if len(key) < minSigningSecretBytes {
		return nil, fmt.Errorf("%w: decoded secret must be at least %d bytes", ErrInvalidSigningSecret, minSigningSecretBytes)
	}
	if len(key) > maxSigningSecretBytes {
		return nil, fmt.Errorf("%w: decoded secret exceeds %d bytes", ErrInvalidSigningSecret, maxSigningSecretBytes)
	}
	return key, nil
}

// GenerateSigningSecret creates a fresh Standard Webhooks secret ("whsec_<base64>").
func GenerateSigningSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return StandardWebhooksPrefix + base64.StdEncoding.EncodeToString(b), nil
}

// SignWebhook computes the Standard Webhooks v1 HMAC-SHA256 signature header
// value ("v1,<base64>") over "${eventID}.${ts}.${body}".
func SignWebhook(secret, eventID string, ts int64, body []byte) (string, error) {
	if eventID == "" {
		return "", errors.New("webhook eventId is required")
	}
	key, err := ParseSigningSecret(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(eventID))
	mac.Write([]byte{'.'})
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyWebhookSignature reports whether sigHeader contains a valid v1
// Standard Webhooks signature for (secret, eventID, ts, body).
func VerifyWebhookSignature(secret, eventID string, ts int64, body []byte, sigHeader string) bool {
	want, err := SignWebhook(secret, eventID, ts, body)
	if err != nil {
		return false
	}
	wantBytes := []byte(want)
	for part := range strings.FieldsSeq(sigHeader) {
		if hmac.Equal([]byte(part), wantBytes) {
			return true
		}
	}
	return false
}

// ApplyWebhookHeaders attaches Standard Webhooks signing headers and
// X-MCP-Subscription-Id to req. webhook-id is always set to eventID.
func ApplyWebhookHeaders(req *http.Request, secret, subscriptionID, eventID string, ts time.Time, body []byte) error {
	unixTS := ts.Unix()
	sig, err := SignWebhook(secret, eventID, unixTS, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", eventID)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(unixTS, 10))
	req.Header.Set("webhook-signature", sig)
	if subscriptionID != "" {
		req.Header.Set("X-MCP-Subscription-Id", subscriptionID)
	}
	return nil
}
