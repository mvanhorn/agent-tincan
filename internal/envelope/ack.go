package envelope

import "strings"

// MaxAckBytes bounds the replies LooksLikeAck considers. A real answer can be
// short, but an acknowledgment is never long.
const MaxAckBytes = 280

// ackPhrases say the work has started or is still to come.
var ackPhrases = []string{
	"working on it",
	"on it",
	"will follow",
	"to follow",
	"will send",
	"will report",
	"will get back",
	"i'll get back",
	"get back to you",
	"will update",
	"will reply",
	"will let you know",
	"starting now",
	"starting on it",
	"looking into it",
	"in progress",
	"stand by",
	"one moment",
	"give me a few",
}

// doneMarkers say the reply already carries a result, so it is not just an
// acknowledgment even when it also promises more.
var doneMarkers = []string{
	"done", "finished", "complete", "here's", "here is", "attached",
	"found", "result", "http://", "https://", "```",
}

// LooksLikeAck reports whether body reads as an acknowledgment that work has
// started ("Working on it: full report will follow.") rather than the work
// itself. The relay keeps a request open when its answered reply looks like
// one, because closing it would strand the real answer: nothing could attach
// to a closed request, and the asker would never be woken for it.
func LooksLikeAck(body string) bool {
	b := strings.ToLower(strings.TrimSpace(body))
	if b == "" || len(b) > MaxAckBytes || strings.Count(b, "\n") > 2 {
		return false
	}
	for _, m := range doneMarkers {
		if strings.Contains(b, m) {
			return false
		}
	}
	for _, p := range ackPhrases {
		if containsWord(b, p) {
			return true
		}
	}
	return false
}

// containsWord reports whether phrase occurs in s bounded by non-letters, so
// "on it" does not match inside "donation items".
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
