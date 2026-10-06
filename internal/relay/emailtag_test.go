package relay_test

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

var (
	tagKey   = bytes.Repeat([]byte{0x11}, 32)
	otherKey = bytes.Repeat([]byte{0x22}, 32)
)

// A tag is a fixed-length hex HMAC of the request id, agent and round under
// the email-tag-key: the same inputs always give the same tag, and changing
// any of them, or the key, stops it verifying.
func TestEmailTagMintAndVerify(t *testing.T) {
	tag := relay.EmailTag(tagKey, "8c186fab0af61f2f049f", "instinct", 0)
	if len(tag) != 32 {
		t.Fatalf("tag %q: want 32 hex characters", tag)
	}
	if _, err := hex.DecodeString(tag); err != nil {
		t.Fatalf("tag %q is not hex: %v", tag, err)
	}
	if again := relay.EmailTag(tagKey, "8c186fab0af61f2f049f", "instinct", 0); again != tag {
		t.Fatalf("same inputs gave %q then %q", tag, again)
	}
	if !relay.VerifyEmailTag(tagKey, tag, "8c186fab0af61f2f049f", "instinct", 0) {
		t.Fatal("a freshly minted tag does not verify")
	}
	if !relay.VerifyEmailTag(tagKey, strings.ToUpper(tag), "8c186fab0af61f2f049f", "instinct", 0) {
		t.Fatal("an upper-cased tag (a mail client changed its case) does not verify")
	}
	for name, ok := range map[string]bool{
		"another round":   relay.VerifyEmailTag(tagKey, tag, "8c186fab0af61f2f049f", "instinct", 1),
		"another agent":   relay.VerifyEmailTag(tagKey, tag, "8c186fab0af61f2f049f", "grokbot", 0),
		"another request": relay.VerifyEmailTag(tagKey, tag, "0000000000000000049f", "instinct", 0),
		"another key":     relay.VerifyEmailTag(otherKey, tag, "8c186fab0af61f2f049f", "instinct", 0),
		"empty tag":       relay.VerifyEmailTag(tagKey, "", "8c186fab0af61f2f049f", "instinct", 0),
		"cut tag":         relay.VerifyEmailTag(tagKey, tag[:30], "8c186fab0af61f2f049f", "instinct", 0),
		"not hex":         relay.VerifyEmailTag(tagKey, strings.Repeat("z", 32), "8c186fab0af61f2f049f", "instinct", 0),
		"no key":          relay.VerifyEmailTag(nil, relay.EmailTag(nil, "8c186fab0af61f2f049f", "instinct", 0), "8c186fab0af61f2f049f", "instinct", 0),
	} {
		if ok {
			t.Errorf("%s: tag verified", name)
		}
	}
}

// The relay mints a request's tag for its target and current round, which
// counts answered clarification exchanges: after a clarification round the
// earlier email's tag no longer verifies. A tag keyed by relay.key, which
// whoami hands to every agent, never verifies, and neither key nor tag is
// in a whoami response.
func TestRelayRequestEmailTag(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetEmailTagKey(tagKey)
	if _, err := m.Client(t, "muse").Send(t.Context(), "instinct", "find a generator quote", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	asks, err := m.Server.OpenAsks("instinct")
	if err != nil || len(asks) != 1 || asks[0].Body != "find a generator quote" {
		t.Fatalf("open asks = %+v, %v", asks, err)
	}
	req := asks[0]
	tag := m.Server.RequestEmailTag(req)
	if tag != relay.EmailTag(tagKey, req.ID, "instinct", 0) {
		t.Fatalf("request tag %q is not the round-0 tag for instinct", tag)
	}
	if !m.Server.VerifyRequestEmailTag(tag, req) {
		t.Fatal("the relay does not verify its own tag")
	}

	resp, err := http.Get(m.URL("instinct") + "/v1/whoami")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var who struct {
		RelayKey string `json:"relay_key"`
	}
	if err := m.Client(t, "instinct").Raw(t.Context(), "GET", "/v1/whoami", nil, &who); err != nil || who.RelayKey == "" {
		t.Fatalf("whoami relay key %q, %v", who.RelayKey, err)
	}
	for _, secret := range []string{hex.EncodeToString(tagKey), string(tagKey), tag} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("whoami %s leaks the email tag key or a tag", raw)
		}
	}
	relayKey, _ := hex.DecodeString(who.RelayKey)
	for _, k := range [][]byte{[]byte(who.RelayKey), relayKey} {
		if m.Server.VerifyRequestEmailTag(relay.EmailTag(k, req.ID, "instinct", 0), req) {
			t.Fatal("a tag computed with relay.key verifies")
		}
	}

	answered := req
	answered.Exchanges = []envelope.Exchange{{Question: "which house?", Answer: "the lake house"}}
	if m.Server.VerifyRequestEmailTag(tag, answered) {
		t.Fatal("a round-0 tag verifies after a clarification round")
	}
	if next := m.Server.RequestEmailTag(answered); next != relay.EmailTag(tagKey, req.ID, "instinct", 1) || !m.Server.VerifyRequestEmailTag(next, answered) {
		t.Fatalf("round-1 tag %q does not verify", next)
	}
	pending := req
	pending.Exchanges = []envelope.Exchange{{Question: "which house?"}}
	if m.Server.RequestEmailTag(pending) != tag {
		t.Fatal("an unanswered question started a new round")
	}
}

// Without an email-tag-key the relay mints no tag and verifies none.
func TestRelayWithoutEmailTagKey(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	req := envelope.Request{ID: "8c186fab0af61f2f049f", To: "instinct"}
	if tag := m.Server.RequestEmailTag(req); tag != "" {
		t.Fatalf("tag without a key = %q", tag)
	}
	if m.Server.VerifyRequestEmailTag(relay.EmailTag(nil, req.ID, "instinct", 0), req) {
		t.Fatal("a tag verified without a key")
	}
}
