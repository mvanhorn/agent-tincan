package relay

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// loadRelayKey reads the relay's secret key from relay.key next to the
// store, creating it on first start. Joined agents learn it through
// whoami; a relay that later answers hello with a proof made from it is
// the same relay, wherever it now lives on the tailnet. An in-memory store
// gets a key for this process only.
func loadRelayKey(st *store.Store) string {
	if st == nil || st.Path() == "" {
		return newRelayKey()
	}
	p := filepath.Join(filepath.Dir(st.Path()), "relay.key")
	if raw, err := os.ReadFile(p); err == nil {
		if k := strings.TrimSpace(string(raw)); len(k) == 64 {
			return k
		}
	}
	k := newRelayKey()
	if err := os.WriteFile(p, []byte(k+"\n"), 0o600); err != nil {
		log.Printf("tincan relay: cannot save %s (%v); agents cannot find this relay again if its address changes", p, err)
	}
	return k
}

func newRelayKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// handleHello answers "are you my relay?" without authentication: it
// proves it holds the relay key by returning HMAC(key, nonce), so a client
// looking for its moved relay can check a peer without trusting it.
func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	if len(nonce) < 16 || len(nonce) > 128 {
		writeErr(w, http.StatusBadRequest, errors.New("nonce must be 16 to 128 characters"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"service": client.HelloService, "proof": client.HelloProof(s.key, nonce)})
}

// SetURLs records the addresses the relay advertises to its agents in
// whoami, its stable tailnet name first. Agents try them when the address
// they saved stops answering.
func (s *Server) SetURLs(urls []string) { s.urls = urls }
