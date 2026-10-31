package envelope

import (
	"strings"
	"testing"
)

func TestValidProject(t *testing.T) {
	for _, ok := range []string{"", "agent-tincan", "my_repo.2", strings.Repeat("a", MaxProject)} {
		if err := ValidProject(ok); err != nil {
			t.Errorf("ValidProject(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"Upper Case", "Upper", "a b", "a/b", "café", strings.Repeat("a", MaxProject+1)} {
		if ValidProject(bad) == nil {
			t.Errorf("ValidProject(%q) accepted", bad)
		}
	}
}

func TestParseSendProject(t *testing.T) {
	req, err := ParseSend([]byte(`{"to":"muse","body":"x","project":"alpha"}`), "grokbot", DefaultMaxBody)
	if err != nil || req.Project != "alpha" {
		t.Fatalf("parse = %+v %v", req, err)
	}
	if req, err := ParseSend([]byte(`{"to":"muse","body":"x"}`), "grokbot", DefaultMaxBody); err != nil || req.Project != "" {
		t.Fatalf("no project = %+v %v", req, err)
	}
	for _, raw := range []string{`{"to":"muse","body":"x","project":"Upper Case"}`, `{"to":"muse","kind":"ping","project":"alpha"}`} {
		if _, err := ParseSend([]byte(raw), "grokbot", DefaultMaxBody); err == nil {
			t.Errorf("parse %s accepted", raw)
		}
	}
}

// The label is text the sender chose, so a held request's target sees it no
// more than the body.
func TestRedactForHidesProject(t *testing.T) {
	req := Request{From: "grokbot", Body: "secret", Project: "alpha", WasHeld: true}
	sender := req
	sender.RedactFor("grokbot")
	if sender.Project != "alpha" {
		t.Fatalf("sender's copy = %+v", sender)
	}
	req.RedactFor("muse")
	if req.Project != "" {
		t.Fatalf("target's copy = %+v", req)
	}
}
