//go:build unix

package history

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A request on a connection an unpacked build's host accepted is refused,
// and never reaches the extension, once a store build's host serves:
// accepting it earlier does not let it through.
func TestUnpackedHostRefusesForwardAfterStoreHostAppears(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	out := &lockedBuffer{}
	h := &NativeHost{Origin: "chrome-extension://" + DefaultExtensionID + "/", SocketPath: filepath.Join(dir, "host.sock"), Out: out, pending: map[int64]chan []byte{}, RequestTimeout: time.Second}
	ctx := context.WithValue(t.Context(), servingSocketKey{}, h.SocketPath)

	// The store build's host comes up on another profile's socket.
	storeSock := filepath.Join(dir, "host-work.sock")
	ln, err := net.Listen("unix", storeSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if err := os.WriteFile(storeSock+".origin", []byte("chrome-extension://"+StoreExtensionID+"/"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, b := net.Pipe()
	defer a.Close()
	go h.serve(ctx, b)
	_ = a.SetDeadline(time.Now().Add(3 * time.Second))
	if err := WriteMessage(a, NativeRequest{ID: 7, Op: "claudeai.list", Args: OpArgs{Count: 5}}, MaxHostMessage); err != nil {
		t.Fatal(err)
	}
	body, err := ReadMessage(a, MaxHostMessage)
	if err != nil {
		t.Fatal(err)
	}
	var r NativeResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Error == nil || !strings.Contains(r.Error.Message, "handed its socket") || r.ID != 7 {
		t.Fatalf("response %s", body)
	}
	if out.String() != "" {
		t.Fatal("the request reached the extension")
	}
}

// A connection whose socket was handed over is closed, not left waiting.
func TestServeClosesConnectionWhenSocketHandedOver(t *testing.T) {
	h := &NativeHost{Out: &lockedBuffer{}, pending: map[int64]chan []byte{}}
	ctx, cancel := context.WithCancel(t.Context())
	a, b := net.Pipe()
	defer a.Close()
	done := make(chan struct{})
	go func() { h.serve(ctx, b); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serve kept a handed-over connection open")
	}
}
