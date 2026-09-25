package relay_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestHelloProvesTheKeyAgentsLearn(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	var who struct {
		RelayKey string `json:"relay_key"`
	}
	if err := m.Client(t, "muse").Raw(t.Context(), "GET", "/v1/whoami", nil, &who); err != nil || len(who.RelayKey) != 64 {
		t.Fatalf("whoami key %q %v", who.RelayKey, err)
	}
	// hello needs no join: a client looking for its moved relay asks
	// before the relay knows its new address is the same machine.
	resp, err := http.Get(m.URL("stranger") + "/v1/hello?nonce=0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var hello struct{ Service, Proof string }
	if err := json.NewDecoder(resp.Body).Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if hello.Service != client.HelloService || hello.Proof != client.HelloProof(who.RelayKey, "0123456789abcdef") {
		t.Fatalf("hello %+v does not prove the key whoami gave", hello)
	}
	if r, _ := http.Get(m.URL("stranger") + "/v1/hello?nonce=short"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("short nonce: %d", r.StatusCode)
	}
}

func TestRelayKeySurvivesRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "relay.db")
	key := func() string {
		st, err := store.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		return relay.KeyForTest(relay.New(identity.NewDirectory(st, identitytest.New(nil), identity.Config{}), st, relay.Config{}))
	}
	if a, b := key(), key(); a == "" || a != b {
		t.Fatalf("key changed across restarts: %q then %q", a, b)
	}
}
