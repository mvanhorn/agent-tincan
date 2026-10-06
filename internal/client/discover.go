package client

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/paths"
)

// HelloService is what a tincan relay's /v1/hello names itself.
const HelloService = "agent-tincan-relay"

// HelloProof is the relay's answer to a hello nonce: HMAC-SHA256 of the
// nonce under the relay key, hex-encoded.
func HelloProof(key, nonce string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte("tincan-relay-hello:" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// findEvery bounds how often one client searches the tailnet for its relay.
var findEvery = 30 * time.Second

// probeWorkers bounds how many addresses one search probes at a time.
const probeWorkers = 16

// relocateFor bounds one search for a moved relay. The search runs on its
// own: a call whose deadline passes returns without waiting for it, so a
// timed-out inbox check still sets off a look for the relay.
var relocateFor = 15 * time.Second

// refreshFor bounds the whoami that refreshes relay info after a move. The
// search may have used up most of its time; the refresh gets its own.
const refreshFor = 5 * time.Second

// learnAfterMove refreshes relay info after a move. Tests replace it.
var learnAfterMove = LearnRelayKey

// lookupLocalNetmap lists tailnet IPv4s from Tailscale LocalAPI.
// Tests replace it.
var lookupLocalNetmap = localAPINetmap

// lookupCLINetmap lists tailnet IPv4s from `tailscale status --json`.
// Tests replace it.
var lookupCLINetmap = cliStatusNetmap

// SwapNetmapLookups replaces LocalAPI and CLI netmap listing for tests.
// Either argument may be nil to leave that lookup as it is. The returned
// function restores both.
func SwapNetmapLookups(local, cli func(context.Context) ([]string, error)) func() {
	oldLocal, oldCLI := lookupLocalNetmap, lookupCLINetmap
	if local != nil {
		lookupLocalNetmap = local
	}
	if cli != nil {
		lookupCLINetmap = cli
	}
	return func() {
		lookupLocalNetmap, lookupCLINetmap = oldLocal, oldCLI
	}
}

// relocate is called after a failed call. When the failure means nothing
// answered at the relay's address and the client knows the relay key, it
// starts a search of the tailnet's IPv4 peers for the relay (or joins the
// one running) and waits for it while ctx allows. A search that finds the
// relay switches to the new address and saves it to the config file the
// client was built from; relocate then reports true so the call is retried
// once. A caller whose deadline passes returns at once and the search goes
// on for the next call; a cancelled caller does not search, and the search
// stops once every caller that joined it has been cancelled.
func (r *Relay) relocate(ctx context.Context, err error) bool {
	if !unreachable(err) || errors.Is(ctx.Err(), context.Canceled) {
		return false
	}
	old := r.Base()
	done := r.startFind(ctx, old)
	if done == nil {
		return false
	}
	select {
	case <-done:
		return r.Base() != old
	case <-ctx.Done():
		return false
	}
}

// relocation is one running search, shared by the callers that joined it.
// Guarded by Relay.findMu.
type relocation struct {
	done      chan struct{}      // closed when the search has its answer
	live      int                // joined callers not cancelled
	cancel    context.CancelFunc // stops the search
	cancelled bool               // stopped because every caller was cancelled
	ended     bool               // the search has its answer; a refresh may still run
	stops     []func() bool      // unregister each caller's cancel watch
}

// startFind starts a search for the relay, or joins the one running, and
// returns a channel closed when it has its answer; nil when the client has
// no relay key, searched within findEvery, or is still refreshing relay
// info after a move. A search stopped by cancellation does not count
// against findEvery.
func (r *Relay) startFind(ctx context.Context, old string) <-chan struct{} {
	r.findMu.Lock()
	defer r.findMu.Unlock()
	f := r.finding
	if f != nil && f.ended {
		return nil
	}
	if f == nil {
		if r.key == "" || time.Since(r.lastFind) < findEvery {
			return nil
		}
		prev := r.lastFind
		r.lastFind = time.Now()
		search, cancel := context.WithTimeout(context.WithoutCancel(ctx), relocateFor)
		f = &relocation{done: make(chan struct{}), cancel: cancel}
		r.finding = f
		go func() {
			moved := r.follow(search, old)
			cancel()
			r.findMu.Lock()
			f.ended = true
			for _, stop := range f.stops {
				stop()
			}
			if f.cancelled {
				r.lastFind = prev
			}
			r.findMu.Unlock()
			// Callers retry at the new address now; the relay info
			// refresh does not hold them. The search stays registered
			// until the refresh ends, so no other search runs alongside.
			close(f.done)
			if moved {
				refresh, cancel := context.WithTimeout(context.WithoutCancel(search), refreshFor)
				learnAfterMove(refresh, r)
				cancel()
			}
			r.findMu.Lock()
			r.finding = nil
			r.findMu.Unlock()
		}()
	}
	f.live++
	f.stops = append(f.stops, context.AfterFunc(ctx, func() {
		// A deadline that passes is not a cancellation: the search goes
		// on for the next call.
		if !errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		r.findMu.Lock()
		defer r.findMu.Unlock()
		if f.ended {
			return
		}
		if f.live--; f.live == 0 {
			f.cancelled = true
			f.cancel()
		}
	}))
	return f.done
}

// follow searches for the relay and, if it is at a new address, switches
// to it and saves it. It reports whether it moved; the caller then
// refreshes the relay info there.
func (r *Relay) follow(ctx context.Context, old string) bool {
	// A live relay that was only slow (a long poll past the client's
	// timeout) still proves the key where it is; ask it alongside the
	// search so a real move does not wait on it.
	r.findMu.Lock()
	key := r.key
	r.findMu.Unlock()
	stays := make(chan bool, 1)
	go func() { stays <- r.proves(ctx, old, key) }()
	found := r.FindRelay(ctx)
	listed, source, _ := r.LastFind()
	if found == "" || found == old {
		switch source {
		case "":
			log.Printf("tincan: relay at %s did not answer; no local tailnet netmap to search", old)
		default:
			log.Printf("tincan: relay at %s did not answer; listed %d tailnet peers via %s, none proved the relay key", old, listed, source)
		}
		return false
	}
	if <-stays {
		log.Printf("tincan: relay at %s was slow to answer but still proves the relay key; staying", old)
		return false
	}
	r.baseMu.Lock()
	r.base = found
	r.baseMu.Unlock()
	msg := fmt.Sprintf("tincan: the relay moved from %s to %s (listed %d peers via %s)", old, found, listed, source)
	if r.configFile != "" {
		if err := updateSavedRelay(r.configFile, old, found); err != nil {
			msg += fmt.Sprintf("; could not update %s yet: %v", r.configFile, err)
			go r.retrySave(old, found)
		} else {
			msg += "; updated " + r.configFile
		}
	}
	log.Print(msg)
	return true
}

// LastFind is what the last relocate search saw: how many IPv4 netmap
// addresses were probed, and which lookup supplied them ("localapi", "cli",
// or "" if neither LocalAPI nor the Tailscale CLI answered). searched is
// false when no search has run, as when the relay answered with an error.
func (r *Relay) LastFind() (listed int, source string, searched bool) {
	r.findMu.Lock()
	defer r.findMu.Unlock()
	return r.lastListed, r.lastSource, r.searched
}

// FindRelay asks every IPv4 tailnet address, on the port of the current
// relay URL, to prove it holds the relay key, and returns the URL of the
// first that does, or "".
func (r *Relay) FindRelay(ctx context.Context) string {
	r.findMu.Lock()
	key, known := r.key, r.known
	r.findMu.Unlock()
	if key == "" {
		return ""
	}
	base := r.Base()
	// The relay's own advertised addresses and the tailnet are searched at
	// the same time: the advertised ones work for agents that cannot list
	// the tailnet, and neither search waits on the other. The first
	// address to prove the key wins and ends both.
	var cands []string
	for _, u := range known {
		if u = strings.TrimRight(u, "/"); u != "" && u != base {
			cands = append(cands, u)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	found := make(chan string, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		if f := r.firstProving(ctx, cands, key); f != "" {
			found <- f
			cancel()
		}
	})
	wg.Go(func() {
		var peers []string
		var source string
		if r.findRelays != nil {
			peers = r.findRelays(ctx, base)
			if len(peers) > 0 {
				source = "netmap"
			}
		} else {
			lctx, lcancel := context.WithTimeout(ctx, netmapFor)
			var ips []string
			ips, source = netmapIPv4s(lctx)
			lcancel()
			peers = peerURLs(base, ips)
		}
		r.findMu.Lock()
		r.lastListed, r.lastSource, r.searched = len(peers), source, true
		r.findMu.Unlock()
		var fresh []string
		for _, p := range peers {
			if !slices.Contains(cands, p) {
				fresh = append(fresh, p)
			}
		}
		if f := r.firstProving(ctx, fresh, key); f != "" {
			found <- f
			cancel()
		}
	})
	wg.Wait()
	close(found)
	return <-found
}

// netmapFor bounds one listing of the tailnet, every socket and the CLI
// fallback together, so a search keeps time to probe what it finds.
var netmapFor = 5 * time.Second

// firstProving probes cands, probeWorkers at a time, and returns the first
// that proves key, or "" when none does within 8 seconds.
func (r *Relay) firstProving(ctx context.Context, cands []string, key string) string {
	if len(cands) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	found := make(chan string, len(cands))
	slots := make(chan struct{}, probeWorkers)
	var wg sync.WaitGroup
	for _, c := range cands {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			if r.proves(ctx, c, key) {
				found <- c
				cancel()
			}
		})
	}
	wg.Wait()
	close(found)
	return <-found
}

// Proves reports whether the relay at base answers hello with a valid
// proof of this client's relay key. tincan doctor uses it to check each
// address the relay advertises.
func (r *Relay) Proves(ctx context.Context, base string) bool {
	r.findMu.Lock()
	key := r.key
	r.findMu.Unlock()
	return key != "" && r.proves(ctx, strings.TrimRight(base, "/"), key)
}

// proves reports whether the relay at base answers hello with a valid
// proof of key.
func (r *Relay) proves(ctx context.Context, base, key string) bool {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return false
	}
	nonce := hex.EncodeToString(b)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/hello?nonce="+nonce, nil)
	if err != nil {
		return false
	}
	resp, err := r.api.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Service string `json:"service"`
		Proof   string `json:"proof"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4096)).Decode(&out) != nil {
		return false
	}
	return out.Service == HelloService && hmac.Equal([]byte(out.Proof), []byte(HelloProof(key, nonce)))
}

// unreachable reports whether err means nothing answered at the relay's
// address (refused, timed out, no route, unknown host), as opposed to the
// relay answering with an error.
func unreachable(err error) bool {
	var api *APIError
	if errors.As(err, &api) {
		// Through a proxy (Muse), a relay that no longer answers comes
		// back as the proxy's 502 or 504; the relay never sends those.
		return api.Code == http.StatusBadGateway || api.Code == http.StatusGatewayTimeout
	}
	var op *net.OpError
	var dns *net.DNSError
	if errors.As(err, &op) && op.Op == "dial" || errors.As(err, &dns) {
		return true
	}
	var ue *url.Error
	return errors.As(err, &ue) && ue.Timeout()
}

// socksConnectFailed reports whether err is the SOCKS proxy failing to open
// a connection to the relay. In a sandbox that reaches the tailnet through a
// local SOCKS tunnel, this is the tunnel being down (often for a moment,
// right after the sandbox wakes), not the relay moving or refusing. No
// request reached the relay, so the call is safe to make again.
func socksConnectFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "socks connect"
}

// netmapIPv4s lists IPv4 addresses on the local tailnet: LocalAPI first,
// then the Tailscale CLI. source is "localapi", "cli", or "".
func netmapIPv4s(ctx context.Context) (ips []string, source string) {
	ips, err := lookupLocalNetmap(ctx)
	if err == nil {
		return ips, "localapi"
	}
	ips, err = lookupCLINetmap(ctx)
	if err == nil {
		return ips, "cli"
	}
	return nil, ""
}

// TailnetNodes lists the tailnet the way a search for a moved relay does,
// and reports how many IPv4s it found and from where: "localapi", "cli",
// or "" when this machine cannot list the tailnet at all.
func TailnetNodes(ctx context.Context) (n int, source string) {
	ips, source := netmapIPv4s(ctx)
	return len(ips), source
}

// peerURLs builds relay URLs from IPv4s using the scheme and port of base,
// skipping the host already in base.
func peerURLs(base string, ips []string) []string {
	u, err := url.Parse(base)
	if err != nil {
		return nil
	}
	port := u.Port()
	seen := map[string]bool{}
	if h := u.Hostname(); h != "" {
		seen[h] = true
	}
	var out []string
	for _, ip := range ips {
		if seen[ip] {
			continue
		}
		seen[ip] = true
		host := ip
		if port != "" {
			host = net.JoinHostPort(ip, port)
		}
		out = append(out, u.Scheme+"://"+host)
	}
	return out
}

// goos is runtime.GOOS; tests replace it to take another platform's path.
var goos = runtime.GOOS

// localStatus asks LocalAPI for the netmap. Tests replace it.
var localStatus = func(ctx context.Context, lc *local.Client) (*ipnstate.Status, error) {
	return lc.Status(ctx)
}

func localAPINetmap(ctx context.Context) ([]string, error) {
	socks, err := tailscaledSockets(ctx)
	if err != nil {
		// A missing socket is not a Tailscale node; fail immediately
		// rather than waiting on LocalAPI's dial timeout, then the CLI
		// can run.
		return nil, err
	}
	type result struct {
		ips []string
		err error
	}
	// Every socket is asked at once, under the caller's deadline and at
	// most 5 seconds, so a dead one cannot hold up the others.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	results := make([]result, len(socks))
	var wg sync.WaitGroup
	for i, sock := range socks {
		wg.Go(func() {
			lc := &local.Client{}
			if sock != "" {
				lc.Socket = sock
				lc.UseSocketOnly = true
			}
			st, err := localStatus(ctx, lc)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			results[i] = result{ips: ipv4sFromLocalStatus(st)}
		})
	}
	wg.Wait()
	var ips []string
	var lastErr error
	ok := false
	for _, r := range results {
		if r.err != nil {
			lastErr = r.err
			continue
		}
		ok = true
		ips = appendNew(ips, r.ips)
	}
	if !ok {
		return nil, lastErr
	}
	return ips, nil
}

// appendNew appends the items of add not already in to.
func appendNew(to, add []string) []string {
	for _, a := range add {
		if !slices.Contains(to, a) {
			to = append(to, a)
		}
	}
	return to
}

// procRoot is where a Linux userspace tailscaled is looked for; tests
// replace it, and userHome.
var (
	procRoot = "/proc"
	userHome = os.UserHomeDir
)

// tailscaledSockets are the tailscaled LocalAPI sockets to ask: TS_SOCKET
// when set; "" on macOS and Windows when it is not (the Tailscale app's
// LocalAPI is a localhost port the local client finds itself); else the
// default socket; or, when that is missing, every socket a tailscaled
// started in userspace uses (userspaceSockets). A host can run more than
// one, on different tailnets: all are listed, and the hello proof picks
// the relay. It fails when there is none.
func tailscaledSockets(ctx context.Context) ([]string, error) {
	if sock := strings.TrimSpace(os.Getenv("TS_SOCKET")); sock != "" {
		if goos != "windows" {
			if _, err := os.Stat(sock); err != nil {
				return nil, err
			}
		}
		return []string{sock}, nil
	}
	if goos == "darwin" || goos == "windows" {
		return []string{""}, nil
	}
	def := paths.DefaultTailscaledSocket()
	_, err := os.Stat(def)
	if err == nil {
		return []string{def}, nil
	}
	if socks := userspaceSockets(ctx); len(socks) > 0 {
		return socks, nil
	}
	return nil, err
}

// homeSockets are where userspace tailscaled setups keep their socket,
// under the home folder.
var homeSockets = []string{".tailscale*/tailscaled.sock", ".cache/tailscale/tailscaled.sock", ".config/tailscale/tailscaled.sock", ".local/share/tailscale/tailscaled.sock", ".local/state/tailscale/tailscaled.sock"}

// userspaceSockets finds the LocalAPI sockets of tailscaleds that are not
// at the default path, as a sandbox or a rebuilt box runs them: the
// --socket of each running tailscaled, then any tailscaled.sock where such
// setups keep it. It stops early when ctx ends, so a slow host cannot use
// up a search's deadline.
func userspaceSockets(ctx context.Context) []string {
	var out []string
	if dirs, err := os.ReadDir(procRoot); err == nil {
		for _, d := range dirs {
			if ctx.Err() != nil {
				return out
			}
			if _, err := strconv.Atoi(d.Name()); err != nil {
				continue // not a process
			}
			raw, err := os.ReadFile(filepath.Join(procRoot, d.Name(), "cmdline"))
			if err != nil || len(raw) == 0 {
				continue
			}
			if sock := socketFlag(strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")); sock != "" && isSocket(sock) {
				out = appendNew(out, []string{sock})
			}
		}
	}
	home, err := userHome()
	if err != nil || home == "" {
		return out
	}
	for _, g := range homeSockets {
		if ctx.Err() != nil {
			return out
		}
		matches, _ := filepath.Glob(filepath.Join(home, g))
		for _, m := range matches {
			if isSocket(m) {
				out = appendNew(out, []string{m})
			}
		}
	}
	return out
}

// socketFlag is the --socket value in a tailscaled command line, "" when
// args are not tailscaled's or name no socket.
func socketFlag(args []string) string {
	if len(args) == 0 || !strings.HasPrefix(filepath.Base(args[0]), "tailscaled") {
		return ""
	}
	for i, a := range args[1:] {
		for _, f := range []string{"--socket", "-socket"} {
			if v, ok := strings.CutPrefix(a, f+"="); ok {
				return v
			}
			if a == f && i+2 < len(args) {
				return args[i+2]
			}
		}
	}
	return ""
}

func isSocket(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

func ipv4sFromLocalStatus(st *ipnstate.Status) []string {
	if st == nil {
		return nil
	}
	var out []string
	add := func(ips []netip.Addr) {
		for _, ip := range ips {
			if ip.Is4() {
				out = append(out, ip.String())
				return
			}
		}
	}
	if st.Self != nil {
		add(st.Self.TailscaleIPs)
	}
	for _, p := range st.Peer {
		if p != nil {
			add(p.TailscaleIPs)
		}
	}
	return out
}

func cliStatusNetmap(ctx context.Context) ([]string, error) {
	bin := tailscaleBinary()
	if bin == "" {
		return nil, errors.New("tailscale CLI not found")
	}
	socks, err := tailscaledSockets(ctx)
	if err != nil {
		socks = []string{""} // the CLI may still find tailscaled itself
	}
	type result struct {
		ips []string
		err error
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	results := make([]result, len(socks))
	var wg sync.WaitGroup
	for i, sock := range socks {
		wg.Go(func() {
			args := []string{"status", "--json"}
			if sock != "" && sock != paths.DefaultTailscaledSocket() {
				args = append([]string{"--socket", sock}, args...)
			}
			raw, err := exec.CommandContext(ctx, bin, args...).Output()
			if err == nil {
				var got []string
				if got, err = ipv4sFromStatusJSON(raw); err == nil {
					results[i] = result{ips: got}
					return
				}
			}
			results[i] = result{err: err}
		})
	}
	wg.Wait()
	var ips []string
	var lastErr error
	ok := false
	for _, r := range results {
		if r.err != nil {
			lastErr = r.err
			continue
		}
		ok = true
		ips = appendNew(ips, r.ips)
	}
	if !ok {
		return nil, lastErr
	}
	return ips, nil
}

// ipv4sFromStatusJSON reads Self and Peer TailscaleIPs from `tailscale
// status --json`, keeping every IPv4, including offline nodes. Host names
// are ignored.
func ipv4sFromStatusJSON(raw []byte) ([]string, error) {
	var st struct {
		Self *struct {
			TailscaleIPs []string
		}
		Peer map[string]struct {
			TailscaleIPs []string
		}
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	var out []string
	add := func(ips []string) {
		for _, ip := range ips {
			parsed := net.ParseIP(ip)
			if parsed == nil || parsed.To4() == nil {
				continue
			}
			out = append(out, parsed.To4().String())
			return
		}
	}
	if st.Self != nil {
		add(st.Self.TailscaleIPs)
	}
	for _, p := range st.Peer {
		add(p.TailscaleIPs)
	}
	return out, nil
}

func tailscaleBinary() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	cands := []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale", "/opt/homebrew/bin/tailscale"}
	if runtime.GOOS == "darwin" {
		cands = append(cands, "/Applications/Tailscale.app/Contents/MacOS/Tailscale")
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// saveRetries and saveRetryEvery bound how long a move whose save failed
// (another process held the config lock) keeps trying to save; package vars
// so tests can shorten them.
var (
	saveRetries    = 6
	saveRetryEvery = 5 * time.Second
)

// retrySave retries a relay move's save with the same rule as the first
// try (only while the file still names old), and stops once it saves, the
// client moves again, or the retries run out; any later process whose saved
// address is dead finds the relay by its own search.
func (r *Relay) retrySave(old, found string) {
	for range saveRetries {
		time.Sleep(saveRetryEvery)
		if r.Base() != found {
			return
		}
		if err := updateSavedRelay(r.configFile, old, found); err != nil {
			continue
		}
		// updateSavedRelay leaves a file another writer changed meanwhile.
		if c, err := loadSavedConfig(r.configFile); err == nil && strings.TrimRight(c.Relay, "/") == found {
			log.Printf("tincan: saved the relay move to %s in %s", found, r.configFile)
			// The refresh after the move skipped this file while it still
			// named the old relay; refresh its relay info now.
			ctx, cancel := context.WithTimeout(context.Background(), refreshFor)
			learnAfterMove(ctx, r)
			cancel()
		} else {
			log.Printf("tincan: left %s as another writer changed it; the relay move to %s was not saved", r.configFile, found)
		}
		return
	}
}

// updateSavedRelay rewrites the relay URL in the config file at path, but
// only while it still says old, so a config someone changed meanwhile is
// kept.
func updateSavedRelay(path, old, found string) error {
	unlock, err := lockConfig(path)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := loadSavedConfig(path)
	if err != nil {
		return err
	}
	if strings.TrimRight(c.Relay, "/") != old {
		return nil
	}
	c.Relay = found
	return SaveConfigTo(path, c)
}

// loadSavedConfig reads the config file at path without environment
// overrides.
func loadSavedConfig(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(raw, &c)
}

// relayInfoEvery is how often a client refreshes what the relay says about
// itself (its key and addresses).
var relayInfoEvery = 24 * time.Hour

// NeedsRelayInfo reports whether c should ask the relay about itself.
func NeedsRelayInfo(c Config) bool {
	return c.RelayKey == "" || time.Since(c.RelayInfoAt) > relayInfoEvery
}

// LearnRelayKey asks the relay (whoami) for its key and advertised
// addresses, keeps them on r, and saves them to the config file r was built
// from (NewRelayFor or NewRelayForFile), so this client can find the relay
// again if its address changes. It is quiet on failure: the information is
// only needed later.
func LearnRelayKey(ctx context.Context, r *Relay) {
	var out struct {
		RelayKey  string   `json:"relay_key"`
		RelayURLs []string `json:"relay_urls"`
	}
	if r.callOnce(ctx, r.api, "GET", "/v1/whoami", nil, &out) != nil || out.RelayKey == "" {
		return
	}
	r.findMu.Lock()
	r.key, r.known = out.RelayKey, out.RelayURLs
	r.findMu.Unlock()
	if r.configFile == "" {
		return
	}
	unlock, err := lockConfig(r.configFile)
	if err != nil {
		return
	}
	defer unlock()
	c, err := loadSavedConfig(r.configFile)
	if err != nil || strings.TrimRight(c.Relay, "/") != r.Base() {
		return
	}
	c.RelayKey, c.RelayURLs, c.RelayInfoAt = out.RelayKey, out.RelayURLs, time.Now().UTC()
	_ = SaveConfigTo(r.configFile, c)
}
