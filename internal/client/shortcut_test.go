package client

import (
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestShortcut(t *testing.T) {
	for line, want := range map[string]string{
		"cc <task>: opens a new Claude Code window": "cc",
		"  call <who>: phones a business":           "call",
		"phone calls to businesses only":            "",
		"CC <task>: shouting":                       "",
		"":                                          "",
	} {
		if got := Shortcut(line); got != want {
			t.Errorf("Shortcut(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestShortcutHint(t *testing.T) {
	agents := []AgentInfo{
		{Name: "instinct", GoodAt: "cc <task>: not this one, it is the receiver"},
		{Name: "muse", GoodAt: "phone calls"},
		{Name: "claude-window", GoodAt: "cc <task>: opens a new Claude Code window"},
	}
	req := envelope.Request{To: "instinct", Body: "cc /ce-plan make a donkey plan", Kind: envelope.KindAsk}
	h := ShortcutHint(req, agents)
	if !strings.Contains(h, "ask claude-window") || !strings.Contains(h, `"/ce-plan make a donkey plan"`) {
		t.Errorf("hint %q", h)
	}
	for _, body := range []string{"cc", "ccx do it", "please cc this", "call the bank"} {
		if h := ShortcutHint(envelope.Request{To: "instinct", Body: body}, agents); h != "" {
			t.Errorf("%q got hint %q", body, h)
		}
	}
}
