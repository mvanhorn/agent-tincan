package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Every Chrome profile with the extension starts its own native host, and
// Chrome never tells a host which profile started it. So the extension
// names its profile (an owner-chosen name kept in that profile's
// chrome.storage.local) in the first frame it posts, and the host picks
// its socket from that: no name serves host.sock, as every host did
// before profiles had names, and a name serves host-<name>.sock. A web
// agent started with --profile <name> dials only that socket, so a request
// for one account is never typed into another account's site.

// profileNamePattern is the shape of a profile name, checked the same way
// by the extension's options page.
var profileNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidProfileName reports whether name may name a Chrome profile's native
// host socket. The empty name (an unnamed profile) is not a name.
func ValidProfileName(name string) bool { return profileNamePattern.MatchString(name) }

// ErrBadProfileName is the refusal for a profile name that does not fit.
var ErrBadProfileName = errors.New("a Chrome profile name is 1 to 32 lowercase letters, digits and dashes, starting with a letter or digit")

// ProfileSocketPath is the native host socket of the Chrome profile named
// name, DefaultSocketPath for the unnamed profile. name must be empty or
// pass ValidProfileName.
func ProfileSocketPath(name string) string { return profileSocket(DefaultSocketPath(), name) }

func profileSocket(defaultPath, name string) string {
	if name == "" {
		return defaultPath
	}
	return filepath.Join(filepath.Dir(defaultPath), "host-"+name+".sock")
}

// NewClientFor returns a client for the native host of the Chrome profile
// named profile (the unnamed profile's host.sock when empty). It never
// falls back to another profile's socket.
func NewClientFor(profile string) *Client {
	return &Client{Channel: &SocketChannel{Path: ProfileSocketPath(profile)}}
}

// ErrSocketInUse: a live native host already answers on the socket.
var ErrSocketInUse = errors.New("a running native host already answers on this socket")

// DefaultSocketRecheck is how often a waiting host checks again whether
// it may serve its socket, and how often an unpacked build's serving host
// checks whether a store build's host wants it.
const DefaultSocketRecheck = 30 * time.Second

func (h *NativeHost) socketRecheck() time.Duration {
	if h.SocketRecheck > 0 {
		return h.SocketRecheck
	}
	return DefaultSocketRecheck
}

// profileChoice is the profile the extension named in its first frame;
// bad means the frame's profile was not a string or null.
type profileChoice struct {
	name string
	bad  bool
}

// profileFrameChoice reads the profile out of the first frame: a profile
// frame ({"id":0,"profile":...}) or, from a build older than profile
// names, its hello. It reports false for any other frame, which never
// chooses a socket.
func profileFrameChoice(id int64, profile json.RawMessage, hello *Hello) (profileChoice, bool) {
	if hello != nil {
		return profileChoice{name: hello.Profile}, true
	}
	if id != 0 || len(profile) == 0 {
		return profileChoice{}, false
	}
	var name *string
	if json.Unmarshal(profile, &name) != nil {
		return profileChoice{bad: true}, true
	}
	if name == nil {
		return profileChoice{}, true
	}
	return profileChoice{name: *name}, true
}

// NativeHostPresence is what a native host records about itself in
// NativeHostsDir, so tincan doctor can list the hosts and the profile each
// serves.
type NativeHostPresence struct {
	PID     int    `json:"pid"`
	Profile string `json:"profile"`
	Origin  string `json:"origin"`
	// Build is store or unpacked.
	Build   string `json:"build"`
	Version string `json:"version,omitempty"`
	Socket  string `json:"socket"`
	// State is serving, or waiting while another host holds the socket
	// (Reason says which).
	State  string    `json:"state"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since"`
}

// NativeHostsDir holds one presence file per running native host.
func NativeHostsDir() string { return filepath.Join(NativeDir(), "hosts") }

// ReadNativeHosts lists the presence files in dir whose host is still
// running, by profile then pid. A file left by a host that died without
// cleaning up is removed.
func ReadNativeHosts(dir string) []NativeHostPresence {
	entries, _ := os.ReadDir(dir)
	var out []NativeHostPresence
	for _, e := range entries {
		pid, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var p NativeHostPresence
		if json.Unmarshal(b, &p) != nil || p.PID != pid {
			continue
		}
		if !processAlive(pid) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Profile != out[j].Profile {
			return out[i].Profile < out[j].Profile
		}
		return out[i].PID < out[j].PID
	})
	return out
}

func (h *NativeHost) hostsDir() string { return filepath.Join(filepath.Dir(h.SocketPath), "hosts") }

func (h *NativeHost) build() string {
	if h.fromStore() {
		return "store"
	}
	return "unpacked"
}

// myPID is the process id the host records (h.pid overrides it: tests run
// several hosts in one process).
func (h *NativeHost) myPID() int {
	if h.pid != 0 {
		return h.pid
	}
	return os.Getpid()
}

func (h *NativeHost) presencePath() string {
	return filepath.Join(h.hostsDir(), strconv.Itoa(h.myPID())+".json")
}

// setPresence applies update to the host's presence record and writes it
// when update reports a change (nothing before the host has chosen its
// socket).
func (h *NativeHost) setPresence(update func(*NativeHostPresence) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.presence == nil || !update(h.presence) {
		return
	}
	b, _ := json.Marshal(h.presence)
	if err := os.MkdirAll(h.hostsDir(), 0o700); err == nil {
		_ = writeFileAtomic(h.presencePath(), b, 0o600)
	}
}

func (h *NativeHost) startPresence(profile, socket string) {
	h.mu.Lock()
	p := &NativeHostPresence{PID: h.myPID(), Profile: profile, Origin: h.Origin, Build: h.build(), Socket: socket, State: "waiting"}
	if h.hello != nil {
		p.Version = h.hello.Version
	}
	h.presence = p
	h.mu.Unlock()
	h.setPresence(func(*NativeHostPresence) bool { return true })
}

func (h *NativeHost) removePresence() {
	h.mu.Lock()
	h.presence = nil
	h.mu.Unlock()
	_ = os.Remove(h.presencePath())
}

func (h *NativeHost) setState(state, reason string) {
	h.setPresence(func(p *NativeHostPresence) bool {
		if p.State == state && p.Reason == reason {
			return false
		}
		p.State, p.Reason, p.Since = state, reason, orNow(h.Now)
		return true
	})
}

// socketAnswers reports whether a host accepts connections on path.
func socketAnswers(path string) bool {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// liveStoreSocket is a socket in dir that a live store-build host serves,
// or "".
func liveStoreSocket(dir string) string {
	socks, _ := filepath.Glob(filepath.Join(dir, "host*.sock"))
	for _, s := range socks {
		if storeHostAlive(s) {
			return s
		}
	}
	return ""
}

// storeHostWaiting reports whether a running store-build host is waiting
// for socket.
func storeHostWaiting(hostsDir, socket string, self int) bool {
	for _, p := range ReadNativeHosts(hostsDir) {
		if p.Build == "store" && p.State == "waiting" && p.Socket == socket && p.PID != self {
			return true
		}
	}
	return false
}

// unpackedMustYield is why an unpacked build's host may not serve socket
// (or must stop serving it), "" when it may. With both builds installed the
// store build serves: chrome.storage.local belongs to one build, so an
// unpacked build beside a named store build would come up unnamed and
// serve the unnamed profile's socket from the wrong account.
func (h *NativeHost) unpackedMustYield(socket string) string {
	if h.fromStore() {
		return ""
	}
	if s := liveStoreSocket(filepath.Dir(socket)); s != "" {
		return "the store build's host serves " + filepath.Base(s) + ", and an unpacked build's host waits while one runs"
	}
	if storeHostWaiting(h.hostsDir(), socket, h.myPID()) {
		return "the store build's host is waiting for " + filepath.Base(socket)
	}
	return ""
}

// trySocket takes socket when this host may serve it, returning the
// function that releases it; otherwise it says why not. A socket another
// host serves is never removed: two unnamed profiles, or two with the same
// name, leave the second host waiting rather than taking over silently.
func (h *NativeHost) trySocket(socket string) (func(), string) {
	if why := h.unpackedMustYield(socket); why != "" {
		return nil, why
	}
	release, ok, err := tryLockSocket(socket)
	if err != nil {
		return nil, "cannot lock " + filepath.Base(socket) + ": " + err.Error()
	}
	if !ok || socketAnswers(socket) {
		if ok {
			release()
		}
		if h.fromStore() && !storeHostAlive(socket) {
			if b, err := os.ReadFile(socket + ".origin"); err == nil && string(b) != "" {
				return nil, "the unpacked build's host serves " + filepath.Base(socket) + " and hands it to the store build"
			}
		}
		return nil, "another native host serves " + filepath.Base(socket) + profileHint(socket)
	}
	return release, ""
}

func profileHint(socket string) string {
	if filepath.Base(socket) == "host.sock" {
		return " (two Chrome profiles are unnamed: name one in the extension's options page)"
	}
	return " (two Chrome profiles have the same name: rename one in the extension's options page)"
}

// waitForSocket returns once this host holds socket, with the function
// that releases it, or nil when ctx ends first. While it waits it says so
// in the log (once per reason) and in its presence file.
func (h *NativeHost) waitForSocket(ctxDone <-chan struct{}, socket string) func() {
	logged := ""
	for {
		release, why := h.trySocket(socket)
		if release != nil {
			return release
		}
		h.setState("waiting", why)
		if why != logged {
			h.logf("waiting to serve %s: %s", socket, why)
			logged = why
		}
		select {
		case <-ctxDone:
			return nil
		case <-time.After(h.socketRecheck()):
		}
	}
}

func refuseProfile(c profileChoice) string {
	name := "a non-text value"
	if !c.bad {
		name = fmt.Sprintf("%q", clip(c.name, 64))
	}
	return "the extension names its Chrome profile " + name + ", which is not a valid profile name (" + ErrBadProfileName.Error() + "); serving no socket. Fix the name in the extension's options page."
}
