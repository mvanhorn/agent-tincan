package council

import (
	"regexp"
	"slices"
	"strings"
)

// Web agents end each reply with a tail after the answer text: an
// optional truncation notice, a "Sources:" list, notes, the
// "<Site> conversation: <id>" footer, and image lines. A dot's reply
// ends in "<label>'s DM: <id>" instead ("your dot's DM", sites.go), a
// Duet channel's in "your Duet channel: <id>", and either may carry
// dotRun's attachment and delegation notes (history/dots.go).
// The footer and notes name the site, so reviewers get the answer text
// only.
var (
	webNotes = []*regexp.Regexp{
		regexp.MustCompile(`^Sources:\n`),
		regexp.MustCompile(`^\(reply truncated: showing \d+ of \d+ bytes\)$`),
		regexp.MustCompile(`^Your previous .{1,40} conversation \(id [^)]*\) .*new chat\.$`),
		regexp.MustCompile(`^.{1,40}'s reply had \d+ images? that could not be attached; open the conversation to see (?:it|them)\.$`),
		regexp.MustCompile(`^\(your dot also sent (?:an attachment|\d+ attachments); open the DM to see them\)$`),
		regexp.MustCompile(`^\(your dot asked \S+ for help; tincan will pass \S+'s answer to the dot, and the dot's final answer will be in its DM(?:: \S+)?\)$`),
		regexp.MustCompile(`^\(Claude also sent (?:an attachment|\d+ attachments); open the channel to see them\)$`),
		regexp.MustCompile(`^\(Claude asked \S+ for help; tincan will pass \S+'s answer to Claude, and Claude's final answer will be in the channel(?:: \S+)?\)$`),
	}
)

// stripWebTail returns a web agent's reply without its tail. A reply
// without the footer is returned as is.
func stripWebTail(body string) string {
	// The real footer is the last one: an answer may quote a line that
	// looks like either kind earlier on.
	loc := lastFooter(webFooterLine, body)
	dot := false
	if dm := lastFooter(dmFooterLine, body); dm != nil && (loc == nil || dm[0] > loc[0]) {
		loc, dot = dm, true
	}
	if loc == nil {
		return body
	}
	paras := strings.Split(body[:loc[0]], "\n\n")
	for len(paras) > 1 && isWebNote(paras[len(paras)-1]) {
		paras = paras[:len(paras)-1]
	}
	if dot {
		// A dot or Claude in a Duet channel that delegated wrote an
		// @tincan ask message; it would name its source in a blind review.
		paras = slices.DeleteFunc(paras, func(p string) bool {
			return len(paras) > 1 && dotAskLine.MatchString(p)
		})
	}
	return strings.TrimSpace(strings.Join(paras, "\n\n"))
}

// dotAskLine is a dot's outbound ask paragraph (see internal/history
// dots_out.go parseDotAsk).
var dotAskLine = regexp.MustCompile(`(?i)^[*_\x60\s]*@tincan\s+ask\b`)

// Footer lines: a footer is one of these lines after a blank line, with
// anything after it (image lines) running to the end.
var (
	webFooterLine = regexp.MustCompile(`^[^\n]{1,40} conversation: \S+$`)
	dmFooterLine  = regexp.MustCompile(`^(?:[^\n]{1,40}'s DM|your Duet channel): \S+$`)
)

// lastFooter is the start (at its preceding blank line) of the last line
// in body that line matches and that follows a blank line, found in one
// backward pass over the lines.
func lastFooter(line *regexp.Regexp, body string) []int {
	end := len(body)
	for end > 0 {
		i := strings.LastIndexByte(body[:end], '\n')
		if i < 0 {
			return nil
		}
		// body[i+1:end] is one line; it must follow "\n\n".
		if i > 0 && body[i-1] == '\n' && line.MatchString(body[i+1:end]) {
			return []int{i - 1, len(body)}
		}
		end = i
	}
	return nil
}

func isWebNote(p string) bool {
	for _, re := range webNotes {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// modelPlaceholder replaces a self-identifying name or phrase.
const modelPlaceholder = "[model]"

// selfNames matches vendor and model names and "as an AI model" phrases,
// with the version and tier words that follow a model name.
var selfNames = regexp.MustCompile(`(?i)\bas an? (?:AI|artificial intelligence)(?: language)? model\b` +
	`|\bas a large language model\b` +
	`|\b(?:Claude|Gemini|Grok|Llama|Copilot|Perplexity|DeepSeek|Qwen|Mistral|Kimi|Codex)(?:[ -](?:\d+(?:\.\d+)*|Opus|Sonnet|Haiku|Fable|Pro|Flash|Ultra|Nano|Mini|Code|Heavy))*\b` +
	`|\b(?:ChatGPT|GPT-?\d[\w.]*|o\d-(?:mini|pro))(?:[ -](?:mini|turbo|pro|Thinking))*\b` +
	`|\b(?:Anthropic|OpenAI|Google|DeepMind|xAI|Meta AI|Microsoft|Moonshot)\b`)

// redact replaces self-identifying names and phrases in an answer with
// modelPlaceholder. Writing style can still give a member away.
func redact(s string) string {
	return selfNames.ReplaceAllString(s, modelPlaceholder)
}

// reviewerCopy is an answer as reviewers and the chairman see it: without
// a web agent's tail (web is whether the member is a web agent) and with
// self-identification redacted. The report keeps the full reply.
func reviewerCopy(body string, web bool) string {
	if web {
		body = stripWebTail(body)
	}
	return redact(strings.TrimSpace(body))
}
