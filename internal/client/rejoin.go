package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// notJoinedText is the relay's identity.ErrNotJoined message. The client
// matches on it because it does not import the relay's packages.
const notJoinedText = "not a joined agent"

// noRelayText starts the error every client command returns before a relay
// URL is configured.
const noRelayText = "no relay configured"

// WhoAmI asks the relay which agent this client is. On a rebuilt machine the
// relay re-admits the new node on this call when it can.
func (r *Relay) WhoAmI(ctx context.Context) (AgentInfo, error) {
	var out AgentInfo
	err := r.call(ctx, r.api, "GET", "/v1/whoami", nil, &out)
	return out, err
}

// IsNotJoined reports whether err is the relay refusing a machine that is
// not a joined agent (or not the agent it claims to be).
func IsNotJoined(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == http.StatusForbidden && strings.Contains(e.Message, notJoinedText)
}

// RejoinHint adds the self-heal step to a "not a joined agent" or "no relay
// configured" error: a rebuilt machine re-admits itself with tincan rejoin,
// and only a machine that was never joined needs a person. Other errors are
// returned unchanged. relayURL fills in the command when known.
func RejoinHint(err error, relayURL string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.Contains(msg, notJoinedText) && !strings.Contains(msg, noRelayText) {
		return err
	}
	if relayURL == "" {
		relayURL = "<relay url>"
	}
	if !strings.Contains(msg, notJoinedText) {
		// No config at all: a fresh admin device or a never-joined machine
		// is far more common than a rebuilt agent, so say both.
		hint := "If this machine is an agent that was joined before and lost its config (for example after a rebuild), run `tincan rejoin --relay " + relayURL +
			"` yourself (add --proxy <url> if you reach the relay through a proxy). An admin device that never joined does not need this: pass --relay <url> instead."
		return fmt.Errorf("%w\n%s", err, hint)
	}
	hint := "If this machine was rebuilt or lost its config, run `tincan rejoin --relay " + relayURL +
		"` yourself (add --proxy <url> if you reach the relay through a proxy). Only a machine that was never joined needs an invite from an admin."
	return fmt.Errorf("%w\n%s", err, hint)
}
