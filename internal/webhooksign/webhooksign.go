// Package webhooksign implements Standard Webhooks v1 signing
// (https://www.standardwebhooks.com): an HMAC-SHA256 over
// "<webhook-id>.<webhook-timestamp>.<body>", sent as
// "webhook-signature: v1,<base64>". The gateway signs MCP Events deliveries
// with it and the wake package signs standard-webhooks wakes.
package webhooksign

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
	// Prefix is the optional key prefix defined by the Standard Webhooks
	// specification.
	Prefix = "whsec_"

	minSecretBytes = 16
	maxSecretBytes = 1024
)

// ErrInvalidSecret indicates a missing, malformed, or too-short signing secret.
var ErrInvalidSecret = errors.New("invalid webhook signing secret")

// ParseSecret decodes a Standard Webhooks signing secret. It accepts
// "whsec_<base64>" as well as raw base64 (standard or unpadded) and requires
// at least 16 decoded bytes.
func ParseSecret(raw string) ([]byte, error) {
	if raw == "" {
		return nil, fmt.Errorf("%w: secret is required", ErrInvalidSecret)
	}
	trimmed := strings.TrimPrefix(raw, Prefix)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty key after %s prefix", ErrInvalidSecret, Prefix)
	}
	key, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(trimmed)
		if err != nil {
			return nil, fmt.Errorf("%w: base64 decode failed", ErrInvalidSecret)
		}
	}
	if len(key) < minSecretBytes {
		return nil, fmt.Errorf("%w: decoded secret must be at least %d bytes", ErrInvalidSecret, minSecretBytes)
	}
	if len(key) > maxSecretBytes {
		return nil, fmt.Errorf("%w: decoded secret exceeds %d bytes", ErrInvalidSecret, maxSecretBytes)
	}
	return key, nil
}

// GenerateSecret creates a fresh Standard Webhooks secret ("whsec_<base64>").
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return Prefix + base64.StdEncoding.EncodeToString(b), nil
}

// Sign computes the Standard Webhooks v1 HMAC-SHA256 signature header
// value ("v1,<base64>") over "${id}.${ts}.${body}".
func Sign(secret, id string, ts int64, body []byte) (string, error) {
	if id == "" {
		return "", errors.New("webhook id is required")
	}
	key, err := ParseSecret(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte{'.'})
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify reports whether sigHeader contains a valid v1 Standard Webhooks
// signature for (secret, id, ts, body). sigHeader may hold several
// space-separated signatures.
func Verify(secret, id string, ts int64, body []byte, sigHeader string) bool {
	want, err := Sign(secret, id, ts, body)
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

// SetHeaders signs body and sets Content-Type, webhook-id (id),
// webhook-timestamp and webhook-signature on req. It returns the signature
// header value, so a caller can keep it out of its logs.
func SetHeaders(req *http.Request, secret, id string, ts time.Time, body []byte) (string, error) {
	unixTS := ts.Unix()
	sig, err := Sign(secret, id, unixTS, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(unixTS, 10))
	req.Header.Set("webhook-signature", sig)
	return sig, nil
}
