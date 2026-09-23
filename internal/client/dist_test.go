package client_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func distServer(t *testing.T, h http.HandlerFunc) *client.Relay {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	r, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A refused download surfaces as an APIError carrying the relay's message.
func TestDownloadDistForbidden(t *testing.T) {
	r := distServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"stranger: not a joined agent"}`))
	})
	var buf bytes.Buffer
	err := r.DownloadDist(context.Background(), "tincan_linux_amd64", &buf)
	if !client.IsStatus(err, http.StatusForbidden) || !strings.Contains(err.Error(), "not a joined agent") {
		t.Fatalf("err = %v, want a 403 APIError with the relay's message", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote %d bytes from an error response", buf.Len())
	}
}

// A file over MaxDistBytes is refused rather than written in full.
func TestDownloadDistOversized(t *testing.T) {
	old := client.MaxDistBytes
	client.MaxDistBytes = 16
	t.Cleanup(func() { client.MaxDistBytes = old })
	r := distServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 64))
	})
	var buf bytes.Buffer
	err := r.DownloadDist(context.Background(), "tincan_linux_amd64", &buf)
	if err == nil || !strings.Contains(err.Error(), "larger than 16 bytes") {
		t.Fatalf("err = %v, want a size error", err)
	}
	if buf.Len() > 17 {
		t.Fatalf("wrote %d bytes, want at most the limit plus one", buf.Len())
	}

	// Exactly at the limit is fine.
	r = distServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 16))
	})
	buf.Reset()
	if err := r.DownloadDist(context.Background(), "tincan_linux_amd64", &buf); err != nil || buf.Len() != 16 {
		t.Fatalf("at the limit: err = %v, wrote %d", err, buf.Len())
	}
}
