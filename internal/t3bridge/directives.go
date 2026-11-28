package t3bridge

import (
	"net/url"
	"strings"
)

// DirectiveKind classifies an inbound Agent Tincan request addressed to the t3 teammate.
type DirectiveKind string

const (
	DirectiveLaunch  DirectiveKind = "launch"
	DirectiveWatch   DirectiveKind = "watch"
	DirectiveUnwatch DirectiveKind = "unwatch"
	DirectiveStatus  DirectiveKind = "status"
	DirectiveSend    DirectiveKind = "send"
)

// Directive is the parsed deterministic action from an inbound request body.
type Directive struct {
	Kind     DirectiveKind
	ThreadID string
	Project  string
	Message  string
	Note     string
}

// TargetsExistingThread reports whether the directive reads or mutates an
// existing T3 thread (and must therefore be refused for guests).
func (d Directive) TargetsExistingThread() bool {
	switch d.Kind {
	case DirectiveWatch, DirectiveUnwatch, DirectiveStatus, DirectiveSend:
		return true
	}
	return false
}

// ParseDirective inspects the first non-empty line of body for deterministic
// T3 directives, falling back to launching a new thread.
func ParseDirective(body, reqProject string) Directive {
	trimmed := strings.TrimSpace(body)
	first, rest, hasRest := strings.Cut(trimmed, "\n")
	firstTrim := strings.TrimSpace(first)
	lower := strings.ToLower(firstTrim)
	restTrim := strings.TrimSpace(rest)

	switch {
	case strings.HasPrefix(lower, "watch ") || strings.HasPrefix(lower, "watch:"):
		arg := strings.TrimSpace(firstTrim[len("watch"):])
		arg = strings.TrimSpace(strings.TrimPrefix(arg, ":"))
		tok, note := splitFirstToken(arg)
		if tid := ExtractThreadID(tok); tid != "" {
			if restTrim != "" {
				if note != "" {
					note += "\n" + restTrim
				} else {
					note = restTrim
				}
			}
			return Directive{Kind: DirectiveWatch, ThreadID: tid, Note: note}
		}
	case strings.HasPrefix(lower, "unwatch ") || strings.HasPrefix(lower, "unwatch:"):
		arg := strings.TrimSpace(firstTrim[len("unwatch"):])
		arg = strings.TrimSpace(strings.TrimPrefix(arg, ":"))
		tok, _ := splitFirstToken(arg)
		if tid := ExtractThreadID(tok); tid != "" {
			return Directive{Kind: DirectiveUnwatch, ThreadID: tid}
		}
	case strings.HasPrefix(lower, "status ") || strings.HasPrefix(lower, "status:"):
		arg := strings.TrimSpace(firstTrim[len("status"):])
		arg = strings.TrimSpace(strings.TrimPrefix(arg, ":"))
		tok, _ := splitFirstToken(arg)
		if tid := ExtractThreadID(tok); tid != "" {
			return Directive{Kind: DirectiveStatus, ThreadID: tid}
		}
	case strings.HasPrefix(lower, "thread:"):
		arg := strings.TrimSpace(firstTrim[len("thread:"):])
		tok, inlineMsg := splitFirstToken(arg)
		if tid := ExtractThreadID(tok); tid != "" {
			msg := joinLines(inlineMsg, restTrim)
			return Directive{Kind: DirectiveSend, ThreadID: tid, Message: msg}
		}
	case strings.HasPrefix(lower, "send "):
		arg := strings.TrimSpace(firstTrim[len("send "):])
		tok, inlineMsg := splitFirstToken(arg)
		if trimmed, ok := strings.CutSuffix(tok, ":"); ok {
			tok = trimmed
		} else if !strings.Contains(tok, "://") {
			if target, after, ok := strings.Cut(arg, ":"); ok {
				tok = strings.TrimSpace(target)
				inlineMsg = strings.TrimSpace(after)
			}
		}
		if tid := ExtractThreadID(tok); tid != "" {
			msg := joinLines(inlineMsg, restTrim)
			return Directive{Kind: DirectiveSend, ThreadID: tid, Message: msg}
		}
	case strings.HasPrefix(lower, "project:"):
		proj := strings.TrimSpace(firstTrim[len("project:"):])
		if proj != "" && hasRest && restTrim != "" {
			return Directive{Kind: DirectiveLaunch, Project: proj, Message: restTrim}
		}
	}

	return Directive{
		Kind:    DirectiveLaunch,
		Project: strings.TrimSpace(reqProject),
		Message: trimmed,
	}
}

// ExtractThreadID pulls a T3 thread identifier from a raw ID, an app.t3.codes
// URL, a t3-thread:// link, or a markdown link wrapping either.
func ExtractThreadID(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Unwrap markdown link: [title](url)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, ")") {
		if idx := strings.LastIndex(s, "]("); idx >= 0 {
			s = s[idx+2 : len(s)-1]
		}
	}
	s = strings.Trim(s, "<>\"'.,;")
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "https" && scheme != "http" && scheme != "t3-thread" {
			return ""
		}
		rawPath := u.EscapedPath()
		segs := strings.Split(strings.Trim(rawPath, "/"), "/")
		if len(segs) == 0 {
			return ""
		}
		last := segs[len(segs)-1]
		if last == "" || last == "threads" {
			return ""
		}
		decoded, err := url.PathUnescape(last)
		if err != nil {
			return ""
		}
		return sanitizeThreadID(decoded)
	}
	return sanitizeThreadID(s)
}

func sanitizeThreadID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 256 {
		return ""
	}
	for _, r := range id {
		if r <= 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return ""
		}
	}
	return id
}

func splitFirstToken(s string) (first, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	// Handle markdown links whose title contains spaces: [title text](url) rest
	if strings.HasPrefix(s, "[") {
		if idx := strings.Index(s, "]("); idx >= 0 {
			if end := strings.Index(s[idx+2:], ")"); end >= 0 {
				cut := idx + 2 + end + 1
				return strings.TrimSpace(s[:cut]), strings.TrimSpace(s[cut:])
			}
		}
	}
	for i, r := range s {
		if r == ' ' || r == '\t' {
			return s[:i], strings.TrimSpace(s[i+1:])
		}
	}
	return s, ""
}

func joinLines(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n" + b
	}
}
