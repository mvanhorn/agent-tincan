package history

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An install from before the store listing gains the store origin once,
// without losing the origins it had.
func TestEnsureStoreOrigin(t *testing.T) {
	home := t.TempDir()
	dir, _ := NativeManifestDir("linux", home)
	if changed, err := EnsureStoreOrigin("linux", home); err != nil || changed {
		t.Fatalf("no manifest: changed %v err %v", changed, err)
	}
	_ = os.MkdirAll(dir, 0o755)
	old := HostManifest{Name: NativeHostName, Path: "/x/native-host", Type: "stdio",
		AllowedOrigins: []string{"chrome-extension://" + DefaultExtensionID + "/"}}
	b, _ := json.Marshal(old)
	path := filepath.Join(dir, NativeHostName+".json")
	_ = os.WriteFile(path, b, 0o644)
	if changed, err := EnsureStoreOrigin("linux", home); err != nil || !changed {
		t.Fatalf("first run: changed %v err %v", changed, err)
	}
	m := readManifest(t, path)
	if len(m.AllowedOrigins) != 2 || m.AllowedOrigins[1] != "chrome-extension://"+StoreExtensionID+"/" || m.Path != "/x/native-host" {
		t.Fatalf("manifest %+v", m)
	}
	if changed, _ := EnsureStoreOrigin("linux", home); changed {
		t.Fatal("second run changed the manifest again")
	}
}

// With both builds installed, the unpacked build's host waits while the
// store build's host serves, and takes over when it goes away.
func TestUnpackedHostYieldsToStoreHost(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	storeRecheck := storeYieldRecheck
	storeYieldRecheck = 20 * time.Millisecond
	t.Cleanup(func() { storeYieldRecheck = storeRecheck })

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	_ = os.WriteFile(sock+".origin", []byte("chrome-extension://"+StoreExtensionID+"/"), 0o600)
	if !storeHostAlive(sock) {
		t.Fatal("a live store host should be seen")
	}
	h := &NativeHost{Origin: "chrome-extension://" + DefaultExtensionID + "/", SocketPath: sock}
	if h.fromStore() {
		t.Fatal("unpacked origin read as store")
	}
	_ = ln.Close()
	if storeHostAlive(sock) {
		t.Fatal("a closed store host should not be seen as live")
	}
	if !strings.Contains((&NativeHost{Origin: "chrome-extension://" + StoreExtensionID + "/"}).Origin, StoreExtensionID) {
		t.Fatal("store origin")
	}
}
