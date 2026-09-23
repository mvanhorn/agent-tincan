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
