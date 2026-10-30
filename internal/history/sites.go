package history

// The live sites: everything that differs between chatgpt.com, claude.ai,
// grok.com, gemini.google.com, www.perplexity.ai, copilot.com, an OpenAI
// dot's DM and a Claude Duet channel, in one table. Adding a site is one entry here plus its reader file; nothing
// else branches on which site it is.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// webSite is one live site the extension reads and a web agent fronts.
type webSite struct {
	// imageInput requires live upload, readiness and submitted-image proof.
	// All sites remain disabled pending the acceptance checks in the guide.
	imageInput bool
	source     Source
	// label names the site in replies and logs ("ChatGPT").
	label string
	// dm, when set, names the site's one conversation in replies ("your
	// dot's DM"); otherwise it is "<label> conversation".
	dm string
	// limiter, when set, is who rate-limits the account in replies (a
	// dot's DM is ChatGPT's); otherwise the label.
	limiter string
	// host is the site's host, in error text and conversation URLs.
	host string
	// altHosts are other hosts whose chat URLs ResolveChatURL reads as
	// the site's (copilot.microsoft.com, which sends a personal account
	// to copilot.com).
	altHosts []string
	// agent is the default name of the site's web agent.
	agent string
	// opPrefix starts each of the site's extension operations
	// ("chatgpt" for chatgpt.list and so on).
	opPrefix string
	// fileTakesConversation: the file operation may carry the
	// conversation id.
	fileTakesConversation bool
	// alwaysGranted: the extension had this site's host access before it
	// reported grants, so an extension whose hello lists none has it.
	alwaysGranted bool
	// reader returns the site's history reader over c.
	reader func(c *Client, now func() time.Time) liveReader
	// nodes reads a detail result into the web agent's view of the
	// current branch.
	nodes func(raw json.RawMessage) ([]webNode, error)
	// stable, when set, is the site's text-stability rule: a reply the
	// site does not mark finished counts as finished once it holds still
	// this long.
	stable *stableRule
	// convPath matches a conversation URL's path on host; its first
	// group is the conversation id.
	convPath *regexp.Regexp
	// noteMissingImages: a reply whose generated images could not all be
	// fetched says so, instead of leaving them out silently.
	noteMissingImages bool
	// planLimitCooldown is how long the site is left alone after an
	// answer ends on the account's rate or plan limit (a webNode marked
	// limited).
	planLimitCooldown time.Duration
	// limitText is how the site words its usage limit in the page's
	// alert and status regions (detectLimit). A rule is written only from
	// wording captured live; a site without one sees its limits only as a
	// 429 with Retry-After or, on Grok, a plan-limit answer read from its
	// answer stream (grokLimitText). ChatGPT and Gemini usually move the
	// owner to a smaller model and answer on, which is not a limit.
	limitText []limitRule
	// canonID, when set, turns a conversation id in any of the site's
	// forms into the one form every store (per-asker state, used list,
	// journal, reply footer) holds; ok is false for an id that is not
	// the site's. Without it ids are used as given.
	canonID func(id string) (string, bool)
	// noteLostImages: a reply image that could not be fetched is noted
	// in the reply, since the site's images are captured from the page
	// and may fail where an API download would not.
	noteLostImages bool
	// blockedCooldown, when set, holds every request to the site back
	// this long after it showed an anti-bot check, so the agent does not
	// keep tripping it on the owner's account.
	blockedCooldown time.Duration
	// listInTab: the list operation reads the site's page in a tab the
	// extension opens (Copilot's sidebar), so it gets
	// TabReadClientTimeout.
	listInTab bool
	// notLoggedIn, when set, is the reason a not_logged_in error gives
	// for the site, from the extension's detail: what the owner has to
	// do in Chrome.
	notLoggedIn func(detail string) string
	// webOnly: the site fronts a web agent only and is not a history
	// source. It has no list or file operation, is not in Sources, and the
	// history readers and prose lists leave it out.
	webOnly bool
	// sources: the site's answers carry web source links, which the web
	// agent's reply lists after the answer text (see sourcesFooter).
	sources bool
	// grantAs, when set, is the site whose extension grant this one runs
	// under (dots under chatgpt): the extension's hello does not list it.
	grantAs Source
	// oneThread: the site's web agent serves one fixed conversation (a
	// dot's DM, a Duet channel), given with --thread. Every request goes
	// there, the threading first line is sent as text, and each send names
	// it.
	oneThread bool
	// primary: the extension can name the account's default thread
	// (dots.primary), so web install needs no --thread.
	primary bool
	// kind is the relay kind a one-thread site's agent must have, so the
	// relay holds requests to it for the owner's approval.
	kind string
	// threadName is what a one-thread site's --thread is, in errors.
	threadName string
	// feed is how a one-thread site's replies and asks name who answers
	// there and where.
	feed *feedWords
}

// feedWords name a one-thread site's assistant and its conversation in
// the notes tincan adds to a reply and in the asks it sends for it.
type feedWords struct {
	// who starts a sentence ("your dot"), them is its object ("the dot")
	// and theirs its possessive ("the dot's").
	who, them, theirs string
	// where is the conversation in running text ("the DM"), place its
	// name before a link ("its DM").
	where, place string
	// url is the conversation's address, less the thread id.
	url string
	// from is who an ask came from, in its body ("the owner's dot").
	from string
	// working is set when every read says whether the assistant is
	// working: a read that cannot tell then counts as working, so a reply
	// finishes only once a read confirms the assistant stopped.
	working bool
}

// ConversationName is how a web agent's reply footer names a site's
// conversation ("ChatGPT conversation", "your dot's DM"): the footer is
// "<ConversationName>: <id>".
func ConversationName(src Source) string {
	if s := siteFor(src); s != nil && s.dm != "" {
		return s.dm
	}
	return siteLabel(src) + " conversation"
}

// theConv is ConversationName in running text ("the ChatGPT conversation",
// "your dot's DM").
func theConv(src Source) string {
	if s := siteFor(src); s != nil && s.dm != "" {
		return s.dm
	}
	return "the " + ConversationName(src)
}

// siteLimiter names who rate-limits the account on src ("ChatGPT" for a
// dot).
func siteLimiter(src Source) string {
	if s := siteFor(src); s != nil && s.limiter != "" {
		return s.limiter
	}
	return siteLabel(src)
}

// canonical returns id in the site's canonical form.
func (s *webSite) canonical(id string) (string, bool) {
	if s.canonID == nil {
		return id, validNativeID(id)
	}
	return s.canonID(id)
}

// stableRule: the same reply text on polls consecutive reads spanning at
// least span.
type stableRule struct {
	polls int
	span  time.Duration
}

// liveReader is a site's history reader: a Reader with the shared live
// read flow under it.
type liveReader interface {
	Reader
	live() *live
}

// webSites is the site table, in the order sites are listed.
var webSites = []*webSite{
	{
		source:                SourceChatGPT,
		label:                 "ChatGPT",
		host:                  "chatgpt.com",
		agent:                 "chatgpt-web",
		opPrefix:              "chatgpt",
		fileTakesConversation: true,
		alwaysGranted:         true,
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewChatGPT(c)
			r.Now = now
			return r
		},
		nodes:    chatgptNodes,
		convPath: convURLPattern,
	},
	{
		source:        SourceClaudeAI,
		label:         "claude.ai",
		host:          "claude.ai",
		agent:         "claude-web",
		opPrefix:      "claudeai",
		alwaysGranted: true,
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewClaudeAI(c)
			r.Now = now
			return r
		},
		nodes:     claudeNodes,
		stable:    &stableRule{polls: DefaultClaudeStablePolls, span: DefaultClaudeStableFor},
		convPath:  convURLPattern,
		limitText: claudeLimitRules,
	},
	{
		source:                SourceGrok,
		label:                 "Grok",
		host:                  "grok.com",
		agent:                 "grok-web",
		opPrefix:              "grok",
		fileTakesConversation: true,
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewGrok(c)
			r.Now = now
			return r
		},
		nodes:             grokNodes,
		convPath:          grokConvPath,
		noteMissingImages: true,
		planLimitCooldown: DefaultPlanLimitCooldown,
	},
	{
		source:                SourceGemini,
		label:                 "Gemini",
		host:                  "gemini.google.com",
		agent:                 "gemini-web",
		opPrefix:              "gemini",
		fileTakesConversation: true,
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewGemini(c)
			r.Now = now
			return r
		},
		nodes:           geminiNodes,
		stable:          &stableRule{polls: DefaultGeminiStablePolls, span: DefaultGeminiStableFor},
		convPath:        geminiURLPattern,
		canonID:         geminiCanonicalID,
		noteLostImages:  true,
		blockedCooldown: DefaultBlockedCooldown,
	},
	{
		source:   SourcePerplexity,
		label:    "Perplexity",
		host:     "www.perplexity.ai",
		agent:    "perplexity-web",
		opPrefix: "perplexity",
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewPerplexity(c)
			r.Now = now
			return r
		},
		nodes:           perplexityNodes,
		convPath:        perplexityConvPath,
		blockedCooldown: DefaultBlockedCooldown,
		webOnly:         true,
		sources:         true,
	},
	{
		source:   SourceCopilot,
		label:    "Copilot",
		host:     "copilot.com",
		altHosts: []string{"copilot.microsoft.com"},
		agent:    "copilot-web",
		opPrefix: "copilot",
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewCopilot(c)
			r.Now = now
			return r
		},
		nodes:           copilotNodes,
		stable:          &stableRule{polls: DefaultCopilotStablePolls, span: DefaultCopilotStableFor},
		convPath:        copilotConvPath,
		canonID:         copilotCanonicalID,
		blockedCooldown: DefaultBlockedCooldown,
		listInTab:       true,
		notLoggedIn:     copilotNotLoggedIn,
		sources:         true,
	},
	{
		source:   SourceDots,
		label:    "your dot",
		dm:       "your dot's DM",
		limiter:  "ChatGPT",
		host:     "chatgpt.com",
		agent:    "dot-web",
		opPrefix: "dots",
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewDots(c)
			r.Now = now
			return r
		},
		nodes:       dotsNodes,
		stable:      &stableRule{polls: DefaultDotStablePolls, span: DefaultDotStableFor},
		convPath:    dotsConvPath,
		canonID:     dotsCanonicalID,
		notLoggedIn: dotsNotLoggedIn,
		webOnly:     true,
		grantAs:     SourceChatGPT,
		oneThread:   true,
		primary:     true,
		kind:        "dot-web",
		threadName:  "the dot's thread id (hex and hyphens)",
		feed:        dotsWords,
	},
	{
		source:   SourceDuet,
		label:    "Claude Duet",
		dm:       "your Duet channel",
		limiter:  "claude.ai",
		host:     "claude.ai",
		agent:    "duet-web",
		opPrefix: "duet",
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewDuet(c)
			r.Now = now
			return r
		},
		nodes:       duetNodes,
		stable:      &stableRule{polls: DefaultDotStablePolls, span: DefaultDotStableFor},
		convPath:    duetConvPath,
		canonID:     duetCanonicalID,
		notLoggedIn: duetNotLoggedIn,
		webOnly:     true,
		grantAs:     SourceClaudeAI,
		oneThread:   true,
		kind:        "duet-web",
		threadName:  "the Duet channel's id (chan_..., or its https://claude.ai/messages/<id> address)",
		feed:        duetWords,
	},
}

// copilotNotLoggedIn is the not_logged_in reason for Copilot: a work or
// school account landing gets its own; any other (a Microsoft sign-in or
// terms page, or a page with no account) says to finish it in Chrome.
func copilotNotLoggedIn(detail string) string {
	if strings.Contains(detail, "work or school account") {
		return "Copilot opened with a work or school account; copilot-web needs a personal Microsoft account, so sign in to copilot.com in Chrome with a personal Microsoft account"
	}
	return "not signed in to Copilot in Chrome; open https://copilot.microsoft.com in Chrome, sign in with a personal Microsoft account and finish any Microsoft sign-in or terms prompt"
}

// grokConvPath is grok.com's conversation path, /c/<id>.
var grokConvPath = regexp.MustCompile(`^/c/([A-Za-z0-9][A-Za-z0-9_-]{0,127})/?$`)

// DefaultPlanLimitCooldown is how long a site is left alone after an
// answer ended on the account's rate or plan limit.
const DefaultPlanLimitCooldown = 15 * time.Minute

// siteFor returns src's table entry, or nil when src is not a live site.
func siteFor(src Source) *webSite {
	for _, s := range webSites {
		if s.source == src {
			return s
		}
	}
	return nil
}

// lookupSite is siteFor with an error naming the known sites.
func lookupSite(src Source) (*webSite, error) {
	if s := siteFor(src); s != nil {
		return s, nil
	}
	return nil, fmt.Errorf("unknown site %q (want %s)", src, WebSiteNames())
}

// opKind is what an extension operation does, whatever its site.
type opKind string

const (
	opSession opKind = "session"
	opList    opKind = "list"
	opDetail  opKind = "detail"
	opFile    opKind = "file"
	opSend    opKind = "send"
	opClose   opKind = "close"
	// opPrimary names the account's primary dot (dots.primary), for web
	// install; only a site with a primary thread has it.
	opPrimary opKind = "primary"
)

func (k opKind) valid() bool {
	switch k {
	case opSession, opList, opDetail, opFile, opSend, opClose, opPrimary:
		return true
	}
	return false
}

// op is the site's operation of kind k.
func (s *webSite) op(k opKind) Op { return Op(s.opPrefix + "." + string(k)) }

// hasOp reports whether the site has operations of kind k: a web-only
// site has no list or file operation, and only a site with a primary
// thread (dots) has a primary operation.
func (s *webSite) hasOp(k opKind) bool {
	if k == opPrimary {
		return s.primary
	}
	return !s.webOnly || (k != opList && k != opFile)
}

// resolve returns op's site and kind; ok is false for an operation no
// site has (extension.reload included).
func (op Op) resolve() (*webSite, opKind, bool) {
	prefix, verb, found := strings.Cut(string(op), ".")
	if !found || !opKind(verb).valid() {
		return nil, "", false
	}
	for _, s := range webSites {
		if s.opPrefix == prefix && s.hasOp(opKind(verb)) {
			return s, opKind(verb), true
		}
	}
	return nil, "", false
}

// WebThreadName says what site's --thread is, for errors.
func WebThreadName(src Source) string {
	if s := siteFor(src); s != nil && s.threadName != "" {
		return s.threadName
	}
	return "a thread id"
}

// WebThreadSites lists the --site values that take --thread ("dots or
// duet").
func WebThreadSites() string {
	var names []string
	for _, s := range webSites {
		if s.oneThread {
			names = append(names, string(s.source))
		}
	}
	return joinList(names, "or")
}

// WebSiteHasPrimary reports whether web install can find site's thread
// itself (the account's primary dot), so --thread may be left out.
func WebSiteHasPrimary(src Source) bool {
	s := siteFor(src)
	return s != nil && s.primary
}

// WebSiteKind is the relay kind site's agent must have so the relay holds
// requests to it for the owner's approval ("dot-web"), or "" for a site
// whose agent needs none.
func WebSiteKind(src Source) string {
	if s := siteFor(src); s != nil {
		return s.kind
	}
	return ""
}

// WebSiteNames lists the --site values, for help and errors
// ("chatgpt, claude-ai, grok, gemini, perplexity or copilot").
func WebSiteNames() string {
	names := make([]string, len(webSites))
	for i, s := range webSites {
		names[i] = string(s.source)
	}
	return joinList(names, "or")
}

// SourceNames lists every history source, for errors ("chatgpt,
// claude-ai, grok, gemini, codex, claude-code or grok-cli").
func SourceNames() string {
	names := make([]string, len(Sources))
	for i, s := range Sources {
		names[i] = string(s)
	}
	return joinList(names, "or")
}

// WebAgentNames lists the sites' default web agent names ("chatgpt-web,
// claude-web, grok-web, gemini-web, perplexity-web or copilot-web").
func WebAgentNames() string {
	names := make([]string, len(webSites))
	for i, s := range webSites {
		names[i] = s.agent
	}
	return joinList(names, "or")
}

// LiveSourcesLabel names the live history sources in prose ("ChatGPT,
// claude.ai, Grok and Gemini"); web-only sites are not among them.
func LiveSourcesLabel() string {
	var labels []string
	for _, s := range webSites {
		if !s.webOnly {
			labels = append(labels, s.label)
		}
	}
	return joinList(labels, "and")
}

// historySite returns src's table entry when it is a live history source,
// nil for a local source or a web-only site.
func historySite(src Source) *webSite {
	if s := siteFor(src); s != nil && !s.webOnly {
		return s
	}
	return nil
}

// IsLiveSource reports whether src is a history source read live through
// the extension.
func IsLiveSource(src Source) bool { return historySite(src) != nil }

// NewLiveReader returns the history reader for live source src over c,
// with now as its clock (the real clock when nil); ok is false when src is
// not a live history source.
func NewLiveReader(src Source, c *Client, now func() time.Time) (Reader, bool) {
	s := historySite(src)
	if s == nil {
		return nil, false
	}
	return s.reader(c, now), true
}

// joinList joins items as prose: "a", "a or b", "a, b or c".
func joinList(items []string, conj string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + conj + " " + items[len(items)-1]
}

func siteSources() []Source {
	out := make([]Source, len(webSites))
	for i, s := range webSites {
		out[i] = s.source
	}
	return out
}

// historySiteSources lists the live sites that are history sources, in the
// table's order.
func historySiteSources() []Source {
	var out []Source
	for _, s := range webSites {
		if !s.webOnly {
			out = append(out, s.source)
		}
	}
	return out
}

// perplexityConvPath is www.perplexity.ai's thread path, /search/<slug>.
var perplexityConvPath = regexp.MustCompile(`^/search/([A-Za-z0-9][A-Za-z0-9_-]{0,127})/?$`)

// ResolveChatURL reads a chat URL into the history source and the
// conversation id (in the site's canonical form) it names. A URL on a site
// that is not a history source (Perplexity, a dot) is refused by name.
func ResolveChatURL(ref string) (Source, string, error) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil || u.Scheme != "https" {
		return "", "", fmt.Errorf("%q is not a chat URL (want an https link to a conversation on %s)", ref, LiveSourcesLabel())
	}
	var onHost *webSite
	for _, s := range webSites {
		if u.Host != s.host && !slices.Contains(s.altHosts, u.Host) {
			continue
		}
		if onHost == nil {
			onHost = s
		}
		m := s.convPath.FindStringSubmatch(u.Path)
		if m == nil {
			continue
		}
		if s.webOnly {
			return "", "", fmt.Errorf("%s is not a history source, so its conversations cannot be read", s.label)
		}
		id, ok := s.canonical(m[1])
		if !ok {
			return "", "", fmt.Errorf("%q does not name a %s conversation", ref, s.label)
		}
		return s.source, id, nil
	}
	if onHost != nil {
		return "", "", fmt.Errorf("%q is not a %s conversation URL", ref, onHost.label)
	}
	return "", "", fmt.Errorf("%q is not a chat URL on %s", ref, LiveSourcesLabel())
}
