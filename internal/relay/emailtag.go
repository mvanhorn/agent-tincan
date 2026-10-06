package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// EmailTagKeyFile is the relay-local key that signs request-email tags, in
// the state directory beside invite-pepper. It is created, validated and
// kept private (0600) exactly like invite-pepper, and no route ever returns
// it. It is deliberately not relay.key: whoami hands relay.key to every
// joined agent, and anyone holding the tag key can forge a tag.
const EmailTagKeyFile = "email-tag-key"

// emailTagLabel separates request-email tags from any other use of the key.
const emailTagLabel = "tincan-email-tag:"

// emailTagBytes is how much of the HMAC a tag keeps: 128 bits, as 32 hex
// characters in an email subject.
const emailTagBytes = 16

// EmailTag is the reply tag for request id to agent in clarification round:
// the first 16 bytes, in lowercase hex, of HMAC-SHA256 under key over
// "tincan-email-tag:|<id>|<agent>|<round>". It is stateless, so it verifies
// after a restart, and rotating the key revokes every outstanding tag. An
// empty key mints no tag.
func EmailTag(key []byte, id, agent string, round int) string {
	if len(key) == 0 {
		return ""
	}
	return hex.EncodeToString(emailTagMAC(key, id, agent, round))
}

// VerifyEmailTag reports whether tag is the tag for id, agent and round
// under key, comparing in constant time. Hex case is ignored, since a mail
// client may change it; anything that is not exactly 16 bytes of hex, and
// any tag under an empty key, fails.
func VerifyEmailTag(key []byte, tag, id, agent string, round int) bool {
	if len(key) == 0 || len(tag) != 2*emailTagBytes {
		return false
	}
	got, err := hex.DecodeString(tag)
	if err != nil {
		return false
	}
	return hmac.Equal(got, emailTagMAC(key, id, agent, round))
}

func emailTagMAC(key []byte, id, agent string, round int) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(emailTagLabel + "|" + id + "|" + agent + "|" + strconv.Itoa(round)))
	return m.Sum(nil)[:emailTagBytes]
}

// EmailTagRound is req's clarification round: how many of its exchanges
// have been answered. An answered clarification starts a new round, so an
// email sent before it stops verifying; a question still waiting for its
// answer does not.
func EmailTagRound(req envelope.Request) int {
	n := 0
	for _, ex := range req.Exchanges {
		if ex.Answer != "" {
			n++
		}
	}
	return n
}

// SetEmailTagKey installs the email-tag-key. Call it before the relay
// serves or wakes anyone. Without it the relay mints and verifies no tags.
func (s *Server) SetEmailTagKey(key []byte) { s.emailTagKey = key }

// RequestEmailTag is the reply tag for req's request email: for req's
// target and current round (EmailTagRound). It is "" when the relay has no
// email-tag-key. The waker puts it in the subject; it never goes to a log,
// the audit log, an error or the roster.
func (s *Server) RequestEmailTag(req envelope.Request) string {
	return EmailTag(s.emailTagKey, req.ID, req.To, EmailTagRound(req))
}

// VerifyRequestEmailTag reports whether tag is the current tag for req, the
// stored request: for its target and its current round. A tag from an
// email sent before a clarification round fails.
func (s *Server) VerifyRequestEmailTag(tag string, req envelope.Request) bool {
	return VerifyEmailTag(s.emailTagKey, tag, req.ID, req.To, EmailTagRound(req))
}

// OpenAsks returns agent's open asks for request emails (store.OpenAsks):
// queued, delivered or claimed asks, urgent first and then oldest, never a
// held request, ping or notify.
func (s *Server) OpenAsks(agent string) ([]envelope.Request, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.store.OpenAsks(ctx, agent)
}
