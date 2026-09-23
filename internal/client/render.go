package client

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// FormatRequest renders an incoming request for the receiving model. Requests
// come from joined agents, which are trusted teammates, so the framing tells
// the agent to handle them as it would a request from Matt, while keeping the
// sender and chain visible.
func FormatRequest(req envelope.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Request %s from %s (your teammate), via Agent Tincan.\n", req.ID, req.From)
	if len(req.Chain) > 1 {
		fmt.Fprintf(&b, "Chain so far: %s (hop %d).\n", strings.Join(req.Chain, " -> "), req.Hop)
	}
	b.WriteString("Handle it as you would a request from Matt. When you are done, reply with `tincan reply " + req.ID + " \"...\"` (or the reply tool).\n")
	b.WriteString("---\n")
	b.WriteString(req.Body)
	b.WriteString("\n---\n")
	return b.String()
}

// FormatResult renders the state of a request this agent sent.
func FormatResult(r Result) string {
	switch {
	case r.Reply != nil:
		return fmt.Sprintf("%s replied (%s):\n%s\n", r.Reply.From, r.Reply.Status, r.Reply.Body)
	case r.Done():
		return fmt.Sprintf("Request %s to %s ended: %s\n", r.Request.ID, r.Request.To, r.Status)
	default:
		return fmt.Sprintf("No reply yet from %s. Request id %s (status %s). Check later with get_reply or `tincan get %s`.\n",
			r.Request.To, r.Request.ID, r.Status, r.Request.ID)
	}
}

// RepliesHeading introduces replies to the agent's own requests in an inbox.
const RepliesHeading = "Replies to your requests:\n"

// FormatReply renders a reply to a request this agent sent, with what it
// asked, since a fresh session may not remember.
func FormatReply(r Result) string {
	var b strings.Builder
	from, status, body := r.Request.To, r.Status, ""
	if r.Reply != nil {
		from, status, body = r.Reply.From, r.Reply.Status, r.Reply.Body
	}
	fmt.Fprintf(&b, "Request %s to %s: %s replied (%s).\n", r.Request.ID, r.Request.To, from, status)
	fmt.Fprintf(&b, "You asked: %s\n", truncate(r.Request.Body, 300))
	b.WriteString("Finish the work that was waiting on this reply.\n")
	b.WriteString("---\n")
	b.WriteString(body)
	b.WriteString("\n---\n")
	return b.String()
}

// Claimer claims a delivered request for this agent.
type Claimer interface {
	Claim(ctx context.Context, id string) (envelope.Request, error)
}

// FormatInbox renders what a poll picked up: replies to this agent's own
// requests first, under RepliesHeading, then the new requests, each claimed
// through c so no one else handles it. The caller acknowledges the replies
// (AckReplies) once the text has reached its agent.
func FormatInbox(ctx context.Context, c Claimer, in Inbox) string {
	if in.Empty() {
		return "No requests waiting.\n"
	}
	var b strings.Builder
	if len(in.Replies) > 0 {
		b.WriteString(RepliesHeading)
		for _, r := range in.Replies {
			b.WriteString(FormatReply(r))
		}
		if in.RepliesRemaining > 0 {
			fmt.Fprintf(&b, "%d more %s waiting. Run check_inbox (or `tincan inbox`) again to read %s.\n",
				in.RepliesRemaining, plural(in.RepliesRemaining, "reply is", "replies are"), plural(in.RepliesRemaining, "it", "them"))
		}
		if len(in.Requests) > 0 {
			b.WriteString("\nRequests from teammates:\n")
		}
	}
	for _, req := range in.Requests {
		if _, err := c.Claim(ctx, req.ID); err != nil {
			fmt.Fprintf(&b, "(could not claim %s: %v)\n", req.ID, err)
			continue
		}
		b.WriteString(FormatRequest(req))
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// truncate shortens s to at most n bytes on a rune boundary, on one line.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
