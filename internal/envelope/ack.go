package envelope

import "strings"

// MaxAckBytes bounds the replies LooksLikeAck considers. A real answer can be
// short, but an acknowledgment is never long.
const MaxAckBytes = 280

// ackOpeners are how an acknowledgment starts: the replier saying it has
// begun. They must open the reply (after an optional ackLead), so a result
// that mentions work in passing ("Booked; the venue is working on it") is
// not taken for one.
var ackOpeners = []string{
	"working on it", "working on this", "working on that",
	"on it",
	"starting now", "starting on it", "starting on this", "starting on that",
	"looking into it", "looking into this", "looking into that",
	"stand by",
}

// ackLeads may come before an opener: "Got it, working on it."
var ackLeads = []string{"ok", "okay", "got it", "sure", "thanks", "ack", "acknowledged", "roger", "on it"}

// laterNouns name the work an acknowledgment promises: "Full report will
// follow." Only these count, so "Confirmation will follow by email" after a
// finished call is not taken for an acknowledgment.
var laterNouns = []string{"report", "results", "result", "answer", "findings", "analysis", "details", "summary", "write-up", "writeup", "reply", "response"}

// doneMarkers say the reply already carries a result, so it is not just an
// acknowledgment even when it also promises more.
var doneMarkers = []string{
	"done", "finished", "complete", "completed", "here's", "here is", "attached",
	"http://", "https://", "```",
	// Finished real-world actions.
	"called", "booked", "paid", "sent", "submitted", "emailed", "texted",
	"ordered", "scheduled", "cancelled", "canceled", "confirmed", "bought",
	"purchased", "filed", "posted", "reserved", "signed",
}

// LooksLikeAck reports whether body reads as an acknowledgment that work has
// started ("Working on it: full report will follow.") rather than the work
// itself. The relay keeps a request open when its answered reply looks like
// one, because closing it would strand the real answer: nothing could attach
// to a closed request, and the asker would never be woken for it.
//
// A wrong yes delays a finished answer until the claim lapses and the relay
// closes the request with it, so the test stays narrow: the reply must open
// with an acknowledgment or promise the report later, and must show no
// result.
func LooksLikeAck(body string) bool {
	b := strings.ToLower(strings.TrimSpace(body))
	if b == "" || len(b) > MaxAckBytes || strings.Count(b, "\n") > 2 {
		return false
	}
	for _, m := range doneMarkers {
		if containsWord(b, m) || strings.HasPrefix(m, "http") && strings.Contains(b, m) || m == "```" && strings.Contains(b, m) {
			return false
		}
	}
	return opensWithAck(b) || promisesLater(b)
}

func opensWithAck(b string) bool {
	if startsWithOpener(b) {
		return true
	}
	for _, lead := range ackLeads {
		if r, ok := cutWord(b, lead); ok && r != "" && strings.ContainsAny(r[:1], ",.!:;-") {
			return startsWithOpener(strings.TrimLeft(r, ",.!:;- "))
		}
	}
	return false
}

func startsWithOpener(s string) bool {
	for _, o := range ackOpeners {
		if _, ok := cutWord(s, o); ok {
			return true
		}
	}
	return false
}

// promisesLater reports a "<report> will follow" or "<report> to follow".
func promisesLater(b string) bool {
	for _, n := range laterNouns {
		for _, tail := range []string{" will follow", " to follow", " coming"} {
			if containsWord(b, n+tail) {
				return true
			}
		}
	}
	return false
}

// cutWord reports whether s starts with word as a whole word, and returns
// what follows it.
func cutWord(s, word string) (string, bool) {
	if !strings.HasPrefix(s, word) {
		return "", false
	}
	rest := s[len(word):]
	if rest != "" && isLetter(rest[0]) {
		return "", false
	}
	return rest, true
}

// containsWord reports whether phrase occurs in s bounded by non-letters.
func containsWord(s, phrase string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], phrase)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(phrase)
		if (start == 0 || !isLetter(s[start-1])) && (end == len(s) || !isLetter(s[end])) {
			return true
		}
		i = start + 1
	}
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '\'' }
