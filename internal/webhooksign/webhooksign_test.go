package webhooksign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The reference vector from the Standard Webhooks specification's test
// suite. A receiver such as OpenBot verifies against exactly this.
func TestSignSpecVector(t *testing.T) {
	const (
		secret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
		id     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
		ts     = int64(1614265330)
		want   = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
	)
	body := []byte(`{"test": 2432232314}`)
	got, err := Sign(secret, id, ts, body)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if !Verify(secret, id, ts, body, "v1,bm90LXRoaXMtb25l "+want) {
		t.Fatal("Verify rejected the spec vector among several signatures")
	}
}

func TestSignAndVerify(t *testing.T) {
	rawKey := []byte("0123456789abcdef01234567")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(rawKey)
	eventID := "evt_test_12345"
	ts := int64(1732300000)
	body := []byte(`{"type":"event","eventId":"evt_test_12345","name":"inbox.request"}`)

	mac := hmac.New(sha256.New, rawKey)
	mac.Write([]byte("evt_test_12345.1732300000." + string(body)))
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	got, err := Sign(secret, eventID, ts, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if !Verify(secret, eventID, ts, body, got) {
		t.Fatal("Verify rejected valid signature")
	}
	// Multiple space-separated signatures (rotation header).
	if !Verify(secret, eventID, ts, body, "v1,deadbeef "+got) {
		t.Fatal("Verify rejected valid rotated signature")
	}
	// Tampered body must fail.
	if Verify(secret, eventID, ts, append(body, ' '), got) {
		t.Fatal("Verify accepted tampered body")
	}
	// Raw base64 without whsec_ prefix produces the same signature.
	rawSecret := base64.StdEncoding.EncodeToString(rawKey)
	gotRaw, err := Sign(rawSecret, eventID, ts, body)
	if err != nil || gotRaw != want {
		t.Fatalf("raw base64 secret: got=%q err=%v want=%q", gotRaw, err, want)
	}
}

func TestParseSecretRejectsWeakOrMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"whsec_",
		"whsec_not-valid-base64!!!",
		"whsec_" + base64.StdEncoding.EncodeToString([]byte("too-short")),
		base64.StdEncoding.EncodeToString(make([]byte, 15)),
	} {
		if _, err := ParseSecret(bad); err == nil {
			t.Errorf("ParseSecret(%q) succeeded, want error", bad)
		}
	}
}

func TestSetHeaders(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":"tincan.request"}`)
	req := httptest.NewRequest(http.MethodPost, "https://example.com/hook", nil)
	sig, err := SetHeaders(req, secret, "msg_1", time.Unix(1732300123, 0), body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("webhook-id") != "msg_1" || req.Header.Get("webhook-timestamp") != "1732300123" || req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", req.Header)
	}
	if got := req.Header.Get("webhook-signature"); got != sig || !Verify(secret, "msg_1", 1732300123, body, got) {
		t.Fatalf("webhook-signature %q (returned %q) failed verification", got, sig)
	}
	if _, err := SetHeaders(req, secret, "", time.Now(), body); err == nil {
		t.Fatal("SetHeaders accepted an empty id")
	}
}
