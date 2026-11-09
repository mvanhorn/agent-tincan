package client

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// shortcutLine matches a good-at line that opens with a shortcut, such as
// `cc <task>: opens a new Claude Code window`: a short lowercase word, then a
// placeholder in angle brackets and a colon.
var shortcutLine = regexp.MustCompile(`^([a-z][a-z0-9-]{0,15}) <[^<>]{1,30}>:`)

// Shortcut returns the shortcut word a good-at line opens with, or "".
func Shortcut(goodAt string) string {
	m := shortcutLine.FindStringSubmatch(strings.TrimSpace(goodAt))
	if m == nil {
		return ""
	}
	return m[1]
}

// Rosterer lists the team, as the relay client does.
type Rosterer interface {
	Agents(ctx context.Context) ([]AgentInfo, error)
}

// ShortcutHint says what to do with a request whose first word is a
// teammate's shortcut: ask that teammate with the rest of it, word for word.
// It is "" for any other request, and never names the request's own agent.
func ShortcutHint(req envelope.Request, agents []AgentInfo) string {
	word, rest, ok := strings.Cut(strings.TrimSpace(req.Body), " ")
	rest = strings.TrimSpace(rest)
	if !ok || rest == "" {
		return ""
	}
	word = strings.ToLower(word)
	for _, a := range agents {
		if a.Name == req.To || Shortcut(a.GoodAt) != word {
			continue
		}
		return fmt.Sprintf("This request starts with %q, the shortcut in %s's good_at line. Do not do the task yourself: ask %s with the rest of the request, word for word (%q), and reply with what %s answers.\n", word, a.Name, a.Name, rest, a.Name)
	}
	return ""
}

// mayBeShortcut reports whether a request could start with a shortcut, so
// the roster is fetched only when one might.
func mayBeShortcut(req envelope.Request) bool {
	word, rest, ok := strings.Cut(strings.TrimSpace(req.Body), " ")
	return ok && strings.TrimSpace(rest) != "" && shortcutLine.MatchString(strings.ToLower(word)+" <x>:")
}

// shortcutHints returns each request's shortcut hint by id, fetching the
// roster once and only when some request may start with a shortcut.
func shortcutHints(ctx context.Context, c any, reqs []envelope.Request) map[string]string {
	ros, ok := c.(Rosterer)
	if !ok {
		return nil
	}
	var agents []AgentInfo
	hints := map[string]string{}
	for _, req := range reqs {
		if req.Kind != envelope.KindAsk || !mayBeShortcut(req) {
			continue
		}
		if agents == nil {
			var err error
			if agents, err = ros.Agents(ctx); err != nil {
				return hints
			}
		}
		if h := ShortcutHint(req, agents); h != "" {
			hints[req.ID] = h
		}
	}
	return hints
}
