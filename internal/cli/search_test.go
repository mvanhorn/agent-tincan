package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestSearchCLI(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	req := answered(t, m, "restaurant booking", "confirmed Tuesday")
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	out, err := run(t, Root(), "search", "restaurant", "--limit", "1")
	if err != nil || !strings.Contains(out, req.TraceID) || !strings.Contains(out, "[restaurant]") {
		t.Fatalf("search = %s, %v", out, err)
	}
	out, err = run(t, Root(), "search", "restaurant", "Tuesday")
	if err != nil || !strings.Contains(out, "[restaurant]") || !strings.Contains(out, "[Tuesday]") {
		t.Fatalf("split excerpts = %s, %v", out, err)
	}
	out, err = run(t, Root(), "search", "Tuesday", "--json")
	var hits []envelope.SearchResult
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].RequestID != req.ID {
		t.Fatalf("JSON = %s", out)
	}
	out, err = run(t, Root(), "search", "missing", "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty JSON = %q, %v", out, err)
	}
	for _, limit := range []string{"0", "51"} {
		if _, err := run(t, Root(), "search", "restaurant", "--limit", limit); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
}

// Search shows teammates' text as data: a body or reply cannot rewrite the
// owner's terminal with control or bidi runes.
func TestSearchCLIIsTerminalSafe(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	answered(t, m, "restaurant \x1b[2K\u202ebooking", "confirmed\x1b]0;owned\x07 Tuesday")
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	out, err := run(t, Root(), "search", "restaurant", "Tuesday")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\x1b", "\x07", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Fatalf("search kept %q: %q", bad, out)
		}
	}
	if !strings.Contains(out, "[restaurant]") || !strings.Contains(out, "[Tuesday]") {
		t.Fatalf("search = %q", out)
	}
}

func TestSearchAttachmentNamesBounded(t *testing.T) {
	names := []string{strings.Repeat("界", 100), "two\nlines", "three", "four", "five", "hidden", "also hidden"}
	got := searchAttachmentNames(names)
	want := strings.Repeat("界", 79) + "…, two lines, three, four, five, and 2 more"
	if got != want {
		t.Fatalf("names = %q, want %q", got, want)
	}
	if got := searchAttachmentNames(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

func TestSearchCLILabelsQuestion(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	muse := m.Client(t, "muse")
	req, err := m.Client(t, "grokbot").Send(ctx, "muse", "book dinner", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(ctx, req.ID, "which restaurant?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	out, err := run(t, Root(), "search", "restaurant")
	if err != nil || !strings.Contains(out, "  Question: which [restaurant]?") || strings.Contains(out, "Reply:") {
		t.Fatalf("search = %s, %v", out, err)
	}
}
