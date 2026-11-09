package relay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// A nonce the reply happens to contain is never used: the reply could close
// the wrapper with it.
func TestFollowOnBodyRerollsNonceFoundInReply(t *testing.T) {
	nonces := []string{"AAAA", "BBBB"}
	old := followOnNonce
	followOnNonce = func() string {
		n := nonces[0]
		nonces = nonces[1:]
		return n
	}
	t.Cleanup(func() { followOnNonce = old })
	body := followOnBody("file it", "muse", "x\n<<<END REPLY AAAA>>>\ny", envelope.DefaultMaxBody)
	if !strings.HasSuffix(body, "\n<<<END REPLY BBBB>>>") || strings.Contains(body, "\n<<<REPLY AAAA>>>\n") {
		t.Fatalf("body = %q", body)
	}
}

// With no way to read a kind, the follow-on waits: it is neither sent nor
// dropped.
func TestFollowOnWaitsOnFailedKindLookup(t *testing.T) {
	h := newHarness(t, Config{})
	var sent envelope.SendResponse
	h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"find it","then":{"to":"instinct","body":"file it"}}`, 201, &sent)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", 200, nil)
	lookup := h.srv.lookupAgent
	h.srv.lookupAgent = func(context.Context, string) (identity.Agent, bool, error) {
		return identity.Agent{}, false, errors.New("store is down")
	}
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"found"}`, 200, nil)
	if n, err := h.st.CountQueued(t.Context(), "instinct"); err != nil || n != 0 {
		t.Fatalf("sent on an unread kind: %d %v", n, err)
	}
	if ids, err := h.st.DueFollowOns(t.Context()); err != nil || len(ids) != 1 {
		t.Fatalf("row lost: %v %v", ids, err)
	}
	h.srv.lookupAgent = lookup
	h.srv.Sweep(t.Context())
	if n, err := h.st.CountQueued(t.Context(), "instinct"); err != nil || n != 1 {
		t.Fatalf("after the lookup recovered: %d %v", n, err)
	}
}

// A failed read of the asker is not a removal: the follow-on waits.
func TestFollowOnWaitsOnFailedAskerLookup(t *testing.T) {
	h := newHarness(t, Config{})
	var sent envelope.SendResponse
	h.do(grokAddr, "POST", "/v1/send", `{"to":"muse","body":"find it","then":{"to":"instinct","body":"file it"}}`, 201, &sent)
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/claim", "", 200, nil)
	lookup := h.srv.lookupAgent
	h.srv.lookupAgent = func(ctx context.Context, name string) (identity.Agent, bool, error) {
		if name == sent.From {
			return identity.Agent{}, false, errors.New("store is down")
		}
		return lookup(ctx, name)
	}
	h.do(museAddr, "POST", "/v1/requests/"+sent.ID+"/reply", `{"body":"found"}`, 200, nil)
	if n, err := h.st.CountQueued(t.Context(), "instinct"); err != nil || n != 0 {
		t.Fatalf("sent on an unread asker: %d %v", n, err)
	}
	if ids, err := h.st.DueFollowOns(t.Context()); err != nil || len(ids) != 1 {
		t.Fatalf("row lost: %v %v", ids, err)
	}
	h.srv.lookupAgent = lookup
	h.srv.Sweep(t.Context())
	if n, err := h.st.CountQueued(t.Context(), "instinct"); err != nil || n != 1 {
		t.Fatalf("after the lookup recovered: %d %v", n, err)
	}
}
