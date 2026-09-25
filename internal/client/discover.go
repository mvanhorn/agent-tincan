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
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
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

// relocate is called after a failed call. When the failure means nothing
// answered at the relay's address and the client knows the relay key, it
// asks the tailnet's online peers for the relay, and on finding it switches
// to the new address, saves it to the config and reports true so the call
// is retried once.
func (r *Relay) relocate(ctx context.Context, err error) bool {
	if !unreachable(err) || ctx.Err() != nil {
		return false
	}
	r.findMu.Lock()
	defer r.findMu.Unlock()
	if r.key == "" {
		return false
	}
	old := r.Base()
	if time.Since(r.lastFind) < findEvery {
		return false
	}
	r.lastFind = time.Now()
	found := r.FindRelay(ctx)
	if found == "" || found == old {
		return false
	}
	r.baseMu.Lock()
	r.base = found
	r.baseMu.Unlock()
	msg := fmt.Sprintf("tincan: the relay moved from %s to %s", old, found)
	if r.persist {
		if err := updateSavedRelay(old, found); err != nil {
			msg += fmt.Sprintf("; could not update %s: %v", ConfigPath(), err)
		} else {
			msg += "; updated " + ConfigPath()
		}
	}
	log.Print(msg)
	return true
}

// FindRelay asks every online tailnet peer, on the port of the current
// relay URL, to prove it holds the relay key, and returns the URL of the
// first that does, or "". The caller holds findMu or owns r alone.
func (r *Relay) FindRelay(ctx context.Context) string {
	if r.key == "" {
		return ""
	}
	base := r.Base()
	find := r.findRelays
	if find == nil {
		find = tailnetCandidates
	}
	cands := find(ctx, base)
	if len(cands) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	found := make(chan string, len(cands))
	var wg sync.WaitGroup
	for _, c := range cands {
		wg.Go(func() {
			if r.proves(ctx, c) {
				found <- c
				cancel()
			}
		})
	}
	wg.Wait()
	close(found)
	return <-found
}

// proves reports whether the relay at base answers hello with a valid
// proof of the relay key.
func (r *Relay) proves(ctx context.Context, base string) bool {
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
	return out.Service == HelloService && hmac.Equal([]byte(out.Proof), []byte(HelloProof(r.key, nonce)))
}

// unreachable reports whether err means nothing answered at the relay's
// address (refused, timed out, no route, unknown host), as opposed to the
// relay answering with an error.
func unreachable(err error) bool {
	var api *APIError
	if errors.As(err, &api) {
		return false
	}
	var op *net.OpError
	var dns *net.DNSError
	if errors.As(err, &op) && op.Op == "dial" || errors.As(err, &dns) {
		return true
	}
	var ue *url.Error
	return errors.As(err, &ue) && ue.Timeout()
}

// tailnetCandidates lists relay URLs to try: every online peer from
// tailscale status, with the scheme and port of base. None when the
// tailscale CLI is not available (a proxy-only sandbox).
func tailnetCandidates(ctx context.Context, base string) []string {
	u, err := url.Parse(base)
	if err != nil {
		return nil
	}
	port := u.Port()
	bin := tailscaleBinary()
	if bin == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if err != nil {
		return nil
	}
	var st struct {
		Peer map[string]struct {
			TailscaleIPs []string
			Online       bool
		}
	}
	if json.Unmarshal(raw, &st) != nil {
		return nil
	}
	var out []string
	for _, p := range st.Peer {
		if !p.Online {
			continue
		}
		for _, ip := range p.TailscaleIPs {
			if strings.Contains(ip, ":") {
				continue // the relay listens on the IPv4 tailnet address
			}
			host := ip
			if port != "" {
				host = net.JoinHostPort(ip, port)
			}
			out = append(out, u.Scheme+"://"+host)
			break
		}
	}
	return out
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

// updateSavedRelay rewrites the relay URL in the config file, but only
// while it still says old, so a config someone changed meanwhile is kept.
func updateSavedRelay(old, found string) error {
	c, err := loadSavedConfig()
	if err != nil {
		return err
	}
	if strings.TrimRight(c.Relay, "/") != old {
		return nil
	}
	c.Relay = found
	return SaveConfig(c)
}

// loadSavedConfig reads the config file without environment overrides.
func loadSavedConfig() (Config, error) {
	var c Config
	raw, err := os.ReadFile(ConfigPath())
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(raw, &c)
}

// LearnRelayKey saves the relay key from whoami when the config lacks it,
// so this client can find the relay again if its address changes. It is
// quiet on failure: the key is only needed later.
func LearnRelayKey(ctx context.Context, r *Relay) {
	r.findMu.Lock()
	known := r.key != ""
	r.findMu.Unlock()
	if known {
		return
	}
	var out struct {
		RelayKey string `json:"relay_key"`
	}
	if r.callOnce(ctx, r.api, "GET", "/v1/whoami", nil, &out) != nil || out.RelayKey == "" {
		return
	}
	r.findMu.Lock()
	r.key = out.RelayKey
	r.findMu.Unlock()
	if !r.persist {
		return
	}
	c, err := loadSavedConfig()
	if err != nil || c.RelayKey == out.RelayKey || strings.TrimRight(c.Relay, "/") != r.Base() {
		return
	}
	c.RelayKey = out.RelayKey
	_ = SaveConfig(c)
}
