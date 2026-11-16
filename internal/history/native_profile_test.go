package history

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// profileHost is a native host under test with a fake extension that
// answers every list request with its own tag, so a test can tell which
// profile's browser a request reached.
type profileHost struct {
	host   *NativeHost
	toHost *io.PipeWriter
	seen   chan NativeRequest
	done   chan error
	log    *lockedBuffer
	once   sync.Once
}

func startProfileHost(t *testing.T, dir, origin, tag string, pid int) *profileHost {
	t.Helper()
	return startProfileHostEvery(t, dir, origin, tag, pid, 20*time.Millisecond)
}

func startProfileHostEvery(t *testing.T, dir, origin, tag string, pid int, recheck time.Duration) *profileHost {
	t.Helper()
	chromeToHostR, chromeToHostW := io.Pipe()
	hostToChromeR, hostToChromeW := io.Pipe()
	p := &profileHost{toHost: chromeToHostW, seen: make(chan NativeRequest, 16), done: make(chan error, 1), log: &lockedBuffer{}}
	fakeExtension(t, hostToChromeR, chromeToHostW, p.seen, func(NativeRequest) []NativeResponse {
		return []NativeResponse{{OK: true, Result: json.RawMessage(`{"items":[],"tag":"` + tag + `"}`)}}
	})
	p.host = &NativeHost{Origin: origin, SocketPath: filepath.Join(dir, "host.sock"), In: chromeToHostR, Out: hostToChromeW, RequestTimeout: 5 * time.Second, Log: p.log, pid: pid, SocketRecheck: recheck}
	go func() { p.done <- p.host.Run(context.Background()) }()
	t.Cleanup(p.stop)
	return p
}

func (p *profileHost) stop() {
	p.once.Do(func() {
		_ = p.toHost.Close()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	})
}

func profileTestDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(shortDir(t), "n")
}

func listTag(t *testing.T, sock string) string {
	t.Helper()
	c := &Client{Channel: &SocketChannel{Path: sock, ChromeRunning: func() bool { return true }}, Timeout: 5 * time.Second}
	res, err := c.Request(context.Background(), OpChatGPTList, OpArgs{Count: 1})
	if err != nil {
		t.Fatalf("list via %s: %v", sock, err)
	}
	var r struct{ Tag string }
	_ = json.Unmarshal(res, &r)
	return r.Tag
}

func presenceOf(t *testing.T, dir string, pid int) NativeHostPresence {
	t.Helper()
	for _, p := range ReadNativeHosts(filepath.Join(dir, "hosts")) {
		if p.PID == pid {
			return p
		}
	}
	return NativeHostPresence{}
}

const unpackedOrigin = "chrome-extension://" + DefaultExtensionID + "/"
const storeOrigin = "chrome-extension://" + StoreExtensionID + "/"

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// serving reports whether a host accepts connections on path.
func serving(path string) bool { return socketAnswers(path) }

// Until the extension names its profile the host binds nothing, however
// long it waits: no timeout picks host.sock for it.
func TestNativeHostBindsNothingBeforeTheProfileFrame(t *testing.T) {
	dir := profileTestDir(t)
	p := startProfileHost(t, dir, unpackedOrigin, "a", os.Getpid())
	time.Sleep(300 * time.Millisecond)
	if socks, _ := filepath.Glob(filepath.Join(dir, "host*.sock")); len(socks) != 0 {
		t.Fatalf("bound before any frame: %v", socks)
	}
	// A request frame first chooses nothing either.
	if err := WriteMessage(p.toHost, map[string]any{"id": 4, "ok": true}, MaxChromeMessage); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if socks, _ := filepath.Glob(filepath.Join(dir, "host*.sock")); len(socks) != 0 {
		t.Fatalf("bound after a non-profile frame: %v", socks)
	}
}

// A build older than profile names sends its hello first, with no
// profile: that is the unnamed profile, host.sock, as before.
func TestNativeHostOldHelloBindsTheDefaultSocket(t *testing.T) {
	dir := profileTestDir(t)
	p := startProfileHost(t, dir, unpackedOrigin, "old", os.Getpid())
	if err := WriteMessage(p.toHost, map[string]any{"id": 0, "hello": map[string]any{"version": "0.7.4", "unpacked": true, "files": map[string]string{}}}, MaxChromeMessage); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "host.sock")
	waitFor(t, func() bool { return serving(sock) })
	if tag := listTag(t, sock); tag != "old" {
		t.Fatalf("tag %q", tag)
	}
	if pr := presenceOf(t, dir, os.Getpid()); pr.State != "serving" || pr.Profile != "" || pr.Socket != sock || pr.Build != "unpacked" || pr.Version != "0.7.4" {
		t.Fatalf("presence %+v", pr)
	}
}

// Two profiles, one named work and one unnamed, each get their own
// socket, and a request on each reaches only its own browser.
func TestNativeHostsForTwoProfilesServeTheirOwnSockets(t *testing.T) {
	dir := profileTestDir(t)
	work := startProfileHost(t, dir, unpackedOrigin, "work-account", os.Getpid())
	nameProfile(t, work.toHost, "work")
	personal := startProfileHost(t, dir, unpackedOrigin, "personal-account", os.Getppid())
	nameProfile(t, personal.toHost, nil)
	workSock, defSock := filepath.Join(dir, "host-work.sock"), filepath.Join(dir, "host.sock")
	waitFor(t, func() bool { return serving(workSock) && serving(defSock) })
	if ProfileSocketPath("work") != filepath.Join(NativeDir(), "host-work.sock") || ProfileSocketPath("") != DefaultSocketPath() {
		t.Fatalf("socket paths %s %s", ProfileSocketPath("work"), ProfileSocketPath(""))
	}
	for range 3 {
		if tag := listTag(t, workSock); tag != "work-account" {
			t.Fatalf("work socket reached %q", tag)
		}
		if tag := listTag(t, defSock); tag != "personal-account" {
			t.Fatalf("default socket reached %q", tag)
		}
	}
	if len(work.seen) != 3 || len(personal.seen) != 3 {
		t.Fatalf("requests seen: work %d personal %d", len(work.seen), len(personal.seen))
	}
	hosts := ReadNativeHosts(filepath.Join(dir, "hosts"))
	if len(hosts) != 2 || hosts[0].Profile != "" || hosts[1].Profile != "work" || hosts[0].State != "serving" || hosts[1].State != "serving" {
		t.Fatalf("hosts %+v", hosts)
	}
	// A profile frame later in the session (a rename the worker has not
	// reconnected for yet) never moves the host.
	nameProfile(t, work.toHost, "other")
	time.Sleep(100 * time.Millisecond)
	if exists(filepath.Join(dir, "host-other.sock")) || !exists(workSock) {
		t.Fatal("a later profile frame moved the host")
	}
}

// A second host for a socket a live host serves (two unnamed profiles,
// or two with the same name) waits, says so, and takes the socket only
// once the first host exits.
func TestSecondHostForALiveSocketWaits(t *testing.T) {
	for _, name := range []any{nil, "work"} {
		t.Run(func() string {
			if name == nil {
				return "unnamed"
			}
			return "named"
		}(), func(t *testing.T) {
			dir := profileTestDir(t)
			sock := filepath.Join(dir, "host.sock")
			if name != nil {
				sock = filepath.Join(dir, "host-work.sock")
			}
			first := startProfileHost(t, dir, unpackedOrigin, "first", os.Getpid())
			nameProfile(t, first.toHost, name)
			waitFor(t, func() bool { return serving(sock) })
			second := startProfileHost(t, dir, unpackedOrigin, "second", os.Getppid())
			nameProfile(t, second.toHost, name)
			// A host is written as waiting before it knows why, so wait for
			// the reason too: reading it straight after the state is a race.
			waitFor(t, func() bool { pr := presenceOf(t, dir, os.Getppid()); return pr.State == "waiting" && pr.Reason != "" })
			pr := presenceOf(t, dir, os.Getppid())
			if !strings.Contains(pr.Reason, "another native host serves "+filepath.Base(sock)) || !strings.Contains(pr.Reason, "options page") {
				t.Fatalf("waiting reason %q", pr.Reason)
			}
			time.Sleep(100 * time.Millisecond)
			if tag := listTag(t, sock); tag != "first" {
				t.Fatalf("socket taken over while the first host lives: %q", tag)
			}
			if !strings.Contains(second.log.String(), "waiting to serve") {
				t.Fatalf("no wait logged: %s", second.log.String())
			}
			first.stop()
			waitFor(t, func() bool { return presenceOf(t, dir, os.Getppid()).State == "serving" })
			if tag := listTag(t, sock); tag != "second" {
				t.Fatalf("after the first exits: %q", tag)
			}
			if pr := presenceOf(t, dir, os.Getpid()); pr.PID != 0 {
				t.Fatalf("exited host left its presence: %+v", pr)
			}
		})
	}
}

// A live socket is refused, never removed; a stale one is replaced.
func TestListenSocketRefusesALiveSocket(t *testing.T) {
	sock := filepath.Join(shortDir(t), "host.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ListenSocket(sock); !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("live socket: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	ln2, err := ListenSocket(sock)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	ln2.Close()
}

// An unpacked build's host waits while a store build's host serves any
// profile's socket: its own storage has no profile name, so it would
// come up unnamed and answer for the wrong account.
func TestUnpackedHostWaitsForAStoreHostOnAnySocket(t *testing.T) {
	dir := profileTestDir(t)
	store := startProfileHost(t, dir, storeOrigin, "store", os.Getpid())
	nameProfile(t, store.toHost, "work")
	waitFor(t, func() bool { return serving(filepath.Join(dir, "host-work.sock")) })
	unpacked := startProfileHost(t, dir, unpackedOrigin, "unpacked", os.Getppid())
	nameProfile(t, unpacked.toHost, nil)
	// A host is written as waiting before it knows why, so wait for
	// the reason too: reading it straight after the state is a race.
	waitFor(t, func() bool { pr := presenceOf(t, dir, os.Getppid()); return pr.State == "waiting" && pr.Reason != "" })
	if pr := presenceOf(t, dir, os.Getppid()); !strings.Contains(pr.Reason, "store build") {
		t.Fatalf("reason %q", pr.Reason)
	}
	time.Sleep(100 * time.Millisecond)
	if exists(filepath.Join(dir, "host.sock")) {
		t.Fatal("unpacked host bound host.sock beside a live store host")
	}
	store.stop()
	waitFor(t, func() bool { return serving(filepath.Join(dir, "host.sock")) })
}

// When the unpacked build's host got there first, a store build's host
// for the same socket waits and the unpacked one hands it over: neither
// removes a socket the other is serving.
func TestUnpackedHostHandsItsSocketToAStoreHost(t *testing.T) {
	dir := profileTestDir(t)
	sock := filepath.Join(dir, "host.sock")
	unpacked := startProfileHost(t, dir, unpackedOrigin, "unpacked", os.Getpid())
	nameProfile(t, unpacked.toHost, nil)
	waitFor(t, func() bool { return serving(sock) })
	store := startProfileHost(t, dir, storeOrigin, "store", os.Getppid())
	nameProfile(t, store.toHost, nil)
	waitFor(t, func() bool { return presenceOf(t, dir, os.Getppid()).State == "serving" })
	if tag := listTag(t, sock); tag != "store" {
		t.Fatalf("after the handoff: %q", tag)
	}
	if pr := presenceOf(t, dir, os.Getpid()); pr.State != "waiting" {
		t.Fatalf("unpacked host after the handoff: %+v", pr)
	}
	_ = store
}

// A name that does not fit (from a tampered storage or a buggy build)
// never binds anything, the default socket included.
func TestNativeHostRefusesAnInvalidProfileName(t *testing.T) {
	for _, bad := range []any{"Work!", "../x", strings.Repeat("a", 33), 7} {
		dir := profileTestDir(t)
		p := startProfileHost(t, dir, unpackedOrigin, "x", os.Getpid())
		nameProfile(t, p.toHost, bad)
		waitFor(t, func() bool { return strings.Contains(p.log.String(), "not a valid profile name") })
		time.Sleep(50 * time.Millisecond)
		if socks, _ := filepath.Glob(filepath.Join(dir, "*.sock")); len(socks) != 0 {
			t.Fatalf("%v bound %v", bad, socks)
		}
		p.stop()
	}
	if ValidProfileName("") || !ValidProfileName("work-2") || ValidProfileName("-work") {
		t.Fatal("ValidProfileName")
	}
}

// Once a store build's host serves any profile's socket, an unpacked
// build's serving host takes no more requests, without waiting for its
// next recheck: its account may be that named store profile's.
func TestUnpackedHostRefusesRequestsOnceAStoreHostRuns(t *testing.T) {
	dir := profileTestDir(t)
	sock := filepath.Join(dir, "host.sock")
	unpacked := startProfileHostEvery(t, dir, unpackedOrigin, "unpacked", os.Getpid(), time.Hour)
	nameProfile(t, unpacked.toHost, nil)
	waitFor(t, func() bool { return serving(sock) })
	store := startProfileHost(t, dir, storeOrigin, "store", os.Getppid())
	nameProfile(t, store.toHost, "work")
	waitFor(t, func() bool { return serving(filepath.Join(dir, "host-work.sock")) })
	c := &Client{Channel: &SocketChannel{Path: sock, ChromeRunning: func() bool { return true }}, Timeout: 5 * time.Second}
	if _, err := c.Request(context.Background(), OpChatGPTList, OpArgs{Count: 1}); err == nil {
		t.Fatal("the unpacked host answered beside a live store host")
	}
	if len(unpacked.seen) != 0 {
		t.Fatalf("a request reached the unpacked build's browser: %d", len(unpacked.seen))
	}
	waitFor(t, func() bool { return presenceOf(t, dir, os.Getpid()).State == "waiting" })
}
