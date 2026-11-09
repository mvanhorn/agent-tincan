package envelope

import (
	"strings"
	"testing"
)

func TestLooksLikeAck(t *testing.T) {
	acks := []string{
		"Working on it: pulling @boardyai posts via X search now. Full report will follow.",
		"On it.",
		"On it, will follow up.",
		"Got it, working on it now.",
		"Okay. Looking into it.",
		"Starting now; results to follow.",
		"Pulling posts now. Full report will follow.",
		"Stand by.",
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
		// Finished real-world work with a promise of something else must
		// close: keeping it open could hand the request back and repeat it.
		"Called them. Confirmation will follow by email.",
		"Booked. They will send a confirmation.",
		"Booked; the venue is working on it.",
		"Called the store, they'll get back to you tomorrow.",
		"Done: PR #12 is up, full report will follow.",
		"Here's the report. More detail will follow if you want it.",
		"See https://example.com/report, details to follow.",
		"Ontario is working on it.",
		"Working on it\nline\nline\nline\nline",
		"Working on it. " + strings.Repeat("x", MaxAckBytes),
	}
	for _, b := range answers {
		if LooksLikeAck(b) {
			t.Errorf("LooksLikeAck(%q) = true, want false", b)
		}
	}
}
