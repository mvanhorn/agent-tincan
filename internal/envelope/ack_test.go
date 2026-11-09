package envelope

import (
	"strings"
	"testing"
)

func TestLooksLikeAck(t *testing.T) {
	acks := []string{
		"Working on it: pulling @boardyai posts via X search now. Full report will follow.",
		"On it.",
		"Starting now, I'll get back to you within the hour.",
		"Looking into it. Will report back shortly.",
		"Got it, stand by.",
	}
	for _, b := range acks {
		if !LooksLikeAck(b) {
			t.Errorf("LooksLikeAck(%q) = false, want true", b)
		}
	}
	answers := []string{
		"",
		"Yes.",
		"The build passed on main.",
		"Done: PR #12 is up, full notes will follow.",
		"Here's the report. More detail will follow if you want it.",
		"See https://example.com/report, details to follow.",
		"Donation items were logged.",
		"Working on it\nline\nline\nline\nline",
		"Working on it. " + strings.Repeat("x", MaxAckBytes),
	}
	for _, b := range answers {
		if LooksLikeAck(b) {
			t.Errorf("LooksLikeAck(%q) = true, want false", b)
		}
	}
}
