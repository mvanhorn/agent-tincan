package council

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStripWebTailRemovesFooterSourcesAndNotes(t *testing.T) {
	answer := "Use a queue.\n\nIt keeps order."
	for name, body := range map[string]string{
		"footer only":      answer + "\n\nChatGPT conversation: 68b1c2d3-aaaa",
		"sources":          answer + "\n\nSources:\n- [1] Queues https://example.com/q\n- [2] More https://example.com/m\n(and 3 more)\n\nPerplexity conversation: abc123",
		"notes and images": answer + "\n\nYour previous Gemini conversation (id c_1) was not found, so this went to a new chat.\n\nGemini's reply had 2 images that could not be attached; open the conversation to see them.\n\nGemini conversation: c_2\n1 image attached.",
		"truncated":        answer + "\n\n(reply truncated: showing 100 of 900 bytes)\n\nClaude conversation: abc",
	} {
		t.Run(name, func(t *testing.T) {
			if got := stripWebTail(body); got != answer {
				t.Fatalf("stripWebTail = %q, want %q", got, answer)
			}
		})
	}
	if got := stripWebTail(answer); got != answer {
		t.Fatalf("no tail: %q", got)
	}
}

// The dot's tail, as internal/history builds it. The helpers are
// unexported there, so the strings are copied: the notes from dotRun in
// internal/history/dots.go (attachment note ~line 185, delegation note
// ~line 192), the truncation notice and the "\n\n%s: %s" footer from
// answer() in internal/history/web.go (the "notice" const and the
// ConversationName tail), and the footer name "your dot's DM" from the dots
// entry of internal/history/sites.go (its dm field).
func dotReply(answer string, files int, askTarget, thread string, truncated bool) string {
	texts := []string{answer}
	if files > 0 {
		what := "an attachment"
		if files > 1 {
			what = fmt.Sprintf("%d attachments", files)
		}
		texts = append(texts, fmt.Sprintf("(your dot also sent %s; open the DM to see them)", what))
	}
	if askTarget != "" {
		dm := "its DM"
		if thread != "" {
			dm = "its DM: https://chatgpt.com/dots/" + thread
		}
		texts = append(texts, fmt.Sprintf("(your dot asked %s for help; tincan will pass %s's answer to the dot, and the dot's final answer will be in %s)", askTarget, askTarget, dm))
	}
	body := strings.Join(texts, "\n\n")
	if truncated {
		body += fmt.Sprintf("\n\n(reply truncated: showing %d of %d bytes)", len(body), len(body)*3)
	}
	return body + fmt.Sprintf("\n\n%s: %s", "your dot's DM", "0f3a9c2e-1b2d-4e5f-8a9b-0c1d2e3f4a5b")
}

func TestStripWebTailRemovesDotFooterAndNotes(t *testing.T) {
	answer := "Use a queue.\n\nIt keeps order."
	const thread = "0f3a9c2e-1b2d-4e5f-8a9b-0c1d2e3f4a5b"
	for name, body := range map[string]string{
		"footer only":                   dotReply(answer, 0, "", "", false),
		"attachment note":               dotReply(answer, 1, "", "", false),
		"attachments note":              dotReply(answer, 3, "", "", false),
		"delegation note":               dotReply(answer, 0, "codex", thread, false),
		"delegation note, no thread":    dotReply(answer, 0, "gemini-web", "", false),
		"truncated, delegation":         dotReply(answer, 0, "codex", thread, true),
		"attachments, delegation, trun": dotReply(answer, 2, "codex", thread, true),
	} {
		t.Run(name, func(t *testing.T) {
			got := stripWebTail(body)
			if got != answer {
				t.Fatalf("stripWebTail = %q, want %q", got, answer)
			}
			if strings.Contains(strings.ToLower(got), "dot") {
				t.Fatalf("stripped reply still names the dot: %q", got)
			}
		})
	}
}

// A chatgpt-web reply strips as before, and a chatgpt-web answer that
// mentions someone's DM mid-text keeps it.
func TestStripWebTailChatGPTUnchanged(t *testing.T) {
	answer := "Ask Sam.\n\nSam's DM: open it first.\n\nThen decide."
	body := answer + "\n\n(reply truncated: showing 10 of 90 bytes)\n\nChatGPT conversation: 68b1c2d3-aaaa\n1 image attached."
	if got := stripWebTail(body); got != answer {
		t.Fatalf("stripWebTail = %q, want %q", got, answer)
	}
}

func TestRedactSelfIdentification(t *testing.T) {
	for in, want := range map[string]string{
		"As Claude, I would pick B.":                  "As [model], I would pick B.",
		"As an AI language model, I cannot browse.":   "[model], I cannot browse.",
		"ChatGPT and GPT-4o from OpenAI agree.":       "[model] and [model] from [model] agree.",
		"Gemini 2.5 Pro by Google; Grok by xAI.":      "[model] by [model]; [model] by [model].",
		"Keep the claudette variable and the google_": "Keep the claudette variable and the google_",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReviewerCopyOnlyStripsWebReplies(t *testing.T) {
	body := "Answer.\n\nClaude conversation: abc"
	if got := reviewerCopy(body, true); got != "Answer." {
		t.Fatalf("web copy = %q", got)
	}
	if got := reviewerCopy(body, false); !strings.Contains(got, "conversation: abc") || strings.Contains(got, "Claude") {
		t.Fatalf("cli copy = %q", got)
	}
}

// A dot that delegates wrote an @tincan ask line; the blind copy drops
// that paragraph so it cannot identify the dot.
func TestStripWebTailDropsDotAskParagraph(t *testing.T) {
	body := "Here is my view.\n\n@tincan ask muse\ncheck the calendar\n\n(your dot asked muse for help; tincan will pass muse's answer to the dot, and the dot's final answer will be in its DM)\n\nyour dot's DM: 0d0d0d0d-1111-7222-8333-000000000001"
	got := stripWebTail(body)
	if got != "Here is my view." {
		t.Fatalf("got %q", got)
	}
	// Another site's answer that quotes the syntax keeps it.
	other := "Write @tincan ask muse to ask.\n\nChatGPT conversation: abc"
	if got := stripWebTail(other); got != "Write @tincan ask muse to ask." {
		t.Fatalf("other: %q", got)
	}
}

// A dot answer that quotes a line looking like another site's footer
// is cut only at the dot's real footer, at the end.
func TestStripWebTailUsesTheLastFooter(t *testing.T) {
	body := "Part one.\n\nChatGPT conversation: abc\n\nPart two.\n\n@tincan ask muse\ncheck x\n\nyour dot's DM: 0d0d0d0d-1111-7222-8333-000000000001"
	if got := stripWebTail(body); got != "Part one.\n\nChatGPT conversation: abc\n\nPart two." {
		t.Fatalf("got %q", got)
	}
}

// Many footer-like lines are found in one pass: a long repetitive answer
// strips quickly and keeps every quoted line.
func TestStripWebTailManyFooterLikeLines(t *testing.T) {
	var b strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&b, "Quoted\n\nChatGPT conversation: c%d\n\n", i)
	}
	answer := strings.TrimSpace(b.String())
	start := time.Now()
	got := stripWebTail(answer + "\n\nyour dot's DM: 0d0d0d0d-1111-7222-8333-000000000001")
	if got != answer {
		t.Fatal("answer changed")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s", d)
	}
}

// A Duet channel's answer loses its footer, its notes and Claude's
// @tincan ask paragraph, so the blind copy does not name its source.
func TestStripWebTailDuetChannel(t *testing.T) {
	body := "Here is my view.\n\n@tincan ask muse\ncheck the calendar\n\n(Claude also sent 2 attachments; open the channel to see them)\n\n(Claude asked muse for help; tincan will pass muse's answer to Claude, and Claude's final answer will be in the channel: https://claude.ai/messages/chan_0000000000000000chan01)\n\nyour Duet channel: chan_0000000000000000chan01"
	if got := stripWebTail(body); got != "Here is my view." {
		t.Fatalf("got %q", got)
	}
}
