package client_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// A long multibyte request body is cut to the "You asked" limit on a rune
// boundary, on one line, and marked as cut.
func TestFormatReplyTruncatesLongMultibyteAsk(t *testing.T) {
	ask := strings.Repeat("日本語 テキスト\n", 100) // 3-byte runes, newlines folded away
	r := client.Result{
		Request: envelope.Request{ID: "r1", To: "muse", Body: ask},
		Status:  envelope.StatusAnswered,
		Reply:   &envelope.Reply{From: "muse", Status: envelope.StatusAnswered, Body: "done"},
	}
	got := client.FormatReply(r)
	line := between(got, "You asked: ", "\n")
	if !utf8.ValidString(line) {
		t.Fatalf("truncated ask is not valid UTF-8: %q", line)
	}
	cut, ok := strings.CutSuffix(line, "...")
	if !ok || len(cut) > 300 || len(cut) < 297 {
		t.Fatalf("truncated ask = %d bytes (%q), want at most 300 plus ...", len(cut), line)
	}
	if !strings.HasPrefix(strings.Join(strings.Fields(ask), " "), cut) {
		t.Fatalf("truncated ask is not a prefix of the folded ask: %q", cut)
	}
	short := client.FormatReply(client.Result{Request: envelope.Request{ID: "r2", To: "muse", Body: "短い"}, Status: envelope.StatusExpired})
	if !strings.Contains(short, "You asked: 短い\n") {
		t.Fatalf("short ask = %q", short)
	}
}

// An inbox cut short by the relay's reply budget says more replies wait.
func TestFormatInboxSaysMoreRepliesWait(t *testing.T) {
	in := client.Inbox{
		Replies:          []client.Result{{Request: envelope.Request{ID: "r1", To: "muse", Body: "x"}, Status: envelope.StatusAnswered, Reply: &envelope.Reply{From: "muse", Body: "y"}}},
		RepliesRemaining: 3,
	}
	got := client.FormatInbox(context.Background(), nil, in)
	if !strings.Contains(got, "3 more replies are waiting") {
		t.Fatalf("inbox = %q", got)
	}
	if ids := in.ReplyIDs(); len(ids) != 1 || ids[0] != "r1" {
		t.Fatalf("ReplyIDs = %v", ids)
	}
}

func between(s, a, b string) string {
	_, rest, _ := strings.Cut(s, a)
	out, _, _ := strings.Cut(rest, b)
	return out
}

// A reply to an ask made while handling another request names that request
// and, while it is still open, tells the agent to finish and reply to it.
func TestFormatReplyNamesOpenParent(t *testing.T) {
	r := client.Result{
		Request: envelope.Request{ID: "r2", From: "instinct", To: "muse", Body: "which airline?"},
		Status:  envelope.StatusAnswered,
		Reply:   &envelope.Reply{From: "muse", Status: envelope.StatusAnswered, Body: "ANA"},
		Parent:  &envelope.Parent{ID: "r1", From: "grokbot", Body: "book the\nflight " + strings.Repeat("x", 400), Status: envelope.StatusClaimed},
	}
	got := client.FormatReply(r)
	want := "This answers the question you asked while handling request r1 from grokbot: book the flight "
	if !strings.Contains(got, want) {
		t.Fatalf("reply missing parent line %q:\n%s", want, got)
	}
	if preview := between(got, "from grokbot: ", ". That request"); len(preview) > 303 || !strings.HasSuffix(preview, "...") {
		t.Fatalf("parent preview not truncated: %q", preview)
	}
	for _, s := range []string{"That request is still open (status claimed).", "reply to it with `tincan reply r1 \"...\"` (or the reply tool)", "You asked: which airline?"} {
		if !strings.Contains(got, s) {
			t.Fatalf("reply missing %q:\n%s", s, got)
		}
	}
}

// When the parent is already closed, the reply says so instead of telling
// the agent to reply to it; with no parent there is no parent line at all.
func TestFormatReplyNamesClosedParent(t *testing.T) {
	r := client.Result{
		Request: envelope.Request{ID: "r2", To: "muse", Body: "which airline?"},
		Status:  envelope.StatusAnswered,
		Reply:   &envelope.Reply{From: "muse", Status: envelope.StatusAnswered, Body: "ANA"},
		Parent:  &envelope.Parent{ID: "r1", From: "grokbot", Body: "book the flight", Status: envelope.StatusAnswered},
	}
	got := client.FormatReply(r)
	if !strings.Contains(got, "while handling request r1 from grokbot: book the flight.") ||
		!strings.Contains(got, "That request is already closed (status answered)") ||
		strings.Contains(got, "tincan reply r1") {
		t.Fatalf("closed parent reply:\n%s", got)
	}
	r.Parent = nil
	if got := client.FormatReply(r); strings.Contains(got, "while handling request") {
		t.Fatalf("reply without parent mentions one:\n%s", got)
	}
}
