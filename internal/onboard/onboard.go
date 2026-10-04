// Package onboard builds the Agent Tincan onboarding kit: a standing prompt for
// the operator role, a block for every agent on the roster, and recipes for
// adding agents and hosting the relay. It is a pure function of its inputs.
// The roster it reads carries only names, wake method names and kinds, so the
// kit can never contain wake secrets.
package onboard

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var tmpl = template.Must(template.New("onboard").Funcs(template.FuncMap{
	"join": strings.Join,
}).ParseFS(templateFS, "templates/*.tmpl"))

// Kinds an agent block or recipe can be tailored to.
const (
	KindVMWebhook     = "vm-webhook"
	KindE2BEmail      = "e2b-email"
	KindProxySandbox  = "proxy-sandbox"
	KindClaudeCode    = "claude-code"
	KindChatGPT       = "chatgpt"
	KindHermes        = "hermes"
	KindOpenClaw      = "openclaw"
	KindCodex         = "codex"
	KindGeminiCLI     = "gemini-cli"
	KindGrokCLI       = "grok-cli"
	KindHistory       = "history"
	KindNotes         = "notes"
	KindCouncil       = "council"
	KindChatGPTWeb    = "chatgpt-web"
	KindClaudeWeb     = "claude-web"
	KindGrokWeb       = "grok-web"
	KindGeminiWeb     = "gemini-web"
	KindPerplexityWeb = "perplexity-web"
	KindCopilotWeb    = "copilot-web"
	KindDotWeb        = "dot-web"
	KindScheduled     = "scheduled"
	KindGeneric       = "generic"
)

// Kinds lists every agent kind in recipe order.
var Kinds = []string{KindVMWebhook, KindE2BEmail, KindProxySandbox, KindClaudeCode, KindChatGPT, KindHermes, KindOpenClaw, KindCodex, KindGeminiCLI, KindGrokCLI, KindHistory, KindNotes, KindCouncil, KindChatGPTWeb, KindClaudeWeb, KindGrokWeb, KindGeminiWeb, KindPerplexityWeb, KindCopilotWeb, KindDotWeb, KindScheduled, KindGeneric}

// KnownKind reports whether kind is empty (no kind) or one of Kinds.
func KnownKind(kind string) bool { return kind == "" || slices.Contains(Kinds, kind) }

// Extra recipe kinds that are not agent kinds.
const (
	RecipeSecondAgent = "second-agent"
	RecipeRelayHost   = "relay-host"
)

// Sections accepted by Render.
var Sections = []string{"operator", "agents", "recipes", "all"}

// runtimeNames maps runtime names to kinds when nothing else says. Only
// product runtimes and product agents (history, notes, council) belong here, never anyone's
// personal agent names.
var runtimeNames = map[string]string{
	"claude-code":    KindClaudeCode,
	"chatgpt":        KindChatGPT,
	"hermes":         KindHermes,
	"openclaw":       KindOpenClaw,
	"codex":          KindCodex,
	"gemini-cli":     KindGeminiCLI,
	"grok-cli":       KindGrokCLI,
	"history":        KindHistory,
	"notes":          KindNotes,
	"council":        KindCouncil,
	"chatgpt-web":    KindChatGPTWeb,
	"claude-web":     KindClaudeWeb,
	"grok-web":       KindGrokWeb,
	"gemini-web":     KindGeminiWeb,
	"perplexity-web": KindPerplexityWeb,
	"copilot-web":    KindCopilotWeb,
	"dot-web":        KindDotWeb,
}

// defaultWake is the wake method a kind normally uses.
var defaultWake = map[string]string{
	KindVMWebhook:     "webhook",
	KindE2BEmail:      "email",
	KindProxySandbox:  "wait",
	KindClaudeCode:    "channel",
	KindChatGPT:       "none",
	KindHermes:        "webhook",
	KindOpenClaw:      "webhook",
	KindCodex:         "command",
	KindGeminiCLI:     "command",
	KindGrokCLI:       "command",
	KindHistory:       "wait",
	KindNotes:         "wait",
	KindCouncil:       "wait",
	KindChatGPTWeb:    "wait",
	KindClaudeWeb:     "wait",
	KindGrokWeb:       "wait",
	KindGeminiWeb:     "wait",
	KindPerplexityWeb: "wait",
	KindCopilotWeb:    "wait",
	KindDotWeb:        "wait",
	KindScheduled:     "schedule",
	KindGeneric:       "none",
}

// stockGoodAt is the good-at line a kind shows when the owner has set none.
// Service kinds and product-tool kinds have one; hosting shapes, Hermes and
// OpenClaw do not, because the owner decides what runs there.
var stockGoodAt = map[string]string{
	KindHistory:       "finds the owner's past ChatGPT, claude.ai, Grok, Gemini, Copilot, Codex, Claude Code and Grok CLI chats and images",
	KindNotes:         "saves, searches and reads the owner's Agent Notes on their Mac; never edits or deletes one",
	KindCouncil:       "asks web, CLI and webhook agents (not Claude Code) one question, ranks answers blind; slow, held for approval by default",
	KindChatGPTWeb:    "asks ChatGPT (chatgpt.com) in the owner's browser and replies with the answer and any images ChatGPT made",
	KindClaudeWeb:     "asks Claude (claude.ai) in the owner's browser and replies with the answer as text; Claude makes no images",
	KindGrokWeb:       "asks Grok (grok.com) in the owner's browser and replies with the answer and any images Grok made; no videos",
	KindGeminiWeb:     "asks Gemini (gemini.google.com) in the owner's browser; it can draw on their connected Gmail, Drive and Calendar",
	KindPerplexityWeb: "asks Perplexity (www.perplexity.ai) in the owner's browser and replies with the answer and its source links",
	KindCopilotWeb:    "asks Microsoft Copilot (copilot.com) in the owner's browser and replies with the answer and its source links",
	KindDotWeb:        "asks the owner's OpenAI dot (chatgpt.com/dots), which acts in their connected apps; held for approval by default",
	KindClaudeCode:    "Claude Code in a terminal on the owner's machine: reads, edits and runs code; answers only while a session is open",
	KindCodex:         "OpenAI Codex run unattended on the owner's machine: reads code anywhere, edits and runs it in its own work folder",
	KindGeminiCLI:     "a Gemini coding agent run unattended on the owner's machine: reads, edits and runs code",
	KindGrokCLI:       "xAI's Grok Build CLI run unattended on the owner's machine: reads code, edits and runs it in its own work folder",
	KindChatGPT:       "ChatGPT in the owner's ChatGPT app; acts only while the owner is chatting with it, so it cannot pick up asks later",
}

// StockGoodAt returns the stock good-at line for an agent: its stored kind's
// line, or with no stored kind the line of the product its name is, "" for
// none. Unlike resolveKind there is no generic fallback.
func StockGoodAt(name, kind string) string {
	if kind == "" {
		kind = runtimeNames[name]
	}
	return stockGoodAt[kind]
}

// wakeMethodOrder is every wake method, in the order the operator prompt
// lists them.
var wakeMethodOrder = []string{"webhook", "email", "wait", "channel", "command", "schedule", "none"}

// freshSession lists the kinds whose every wake starts a fresh session with
// no memory of the last one; their instructions carry instructions.fresh.
// They commonly share a machine with another agent, so their tincan commands
// carry a per-agent TINCAN_CONFIG and their rejoin names the agent. A new
// command-woken CLI kind is added here, not to template conditions.
var freshSession = map[string]bool{
	KindHermes:    true,
	KindOpenClaw:  true,
	KindCodex:     true,
	KindGeminiCLI: true,
	KindGrokCLI:   true,
}

// Member is one roster entry as the relay reports it.
type Member struct {
	Name   string `json:"name"`
	Wake   string `json:"wake"`
	Online bool   `json:"online"`
	Kind   string `json:"kind,omitempty"`
}

// Options are the inputs to Build.
type Options struct {
	RelayURL      string
	Owner         string            // human who owns the mesh; empty reads as "the owner"
	Operator      string            // agent that runs the operator prompt; empty gives a neutral line
	Roster        []Member          // ignored when Offline
	KindOverrides map[string]string // agent name to kind, wins over everything
	Offline       bool              // no roster: operator prompt and recipes only
}

// Kit is the onboarding output. CLI --json and the MCP tool return it as is.
type Kit struct {
	RelayURL string       `json:"relay_url"`
	Owner    string       `json:"owner"`
	Operator string       `json:"operator"`                // rendered operator prompt text
	Host     string       `json:"operator_host,omitempty"` // operator agent's name
	Agents   []AgentBlock `json:"agents"`
	Recipes  []Recipe     `json:"recipes"`
}

// AgentBlock tells one roster agent how it joins, wakes and behaves.
type AgentBlock struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Wake         string   `json:"wake"`
	Join         string   `json:"join"`
	Instructions string   `json:"instructions"` // paste into the agent's standing instructions
	Setup        []string `json:"setup"`        // steps for the owner; secrets are named, never shown
}

// Recipe is an ordered set of steps for adding an agent or hosting the relay.
type Recipe struct {
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Steps []string `json:"steps"`
}

const relayPlaceholder = "<relay-url>"

// agentData feeds the per-agent templates.
type agentData struct {
	Name, Kind, Wake, RelayURL, Owner, OwnerPoss string
	Team                                         []string
	// OwnConfig: every tincan command in the block carries TINCAN_CONFIG
	// and the rejoin names the agent (fresh-session kinds and services).
	OwnConfig bool
	// Service: a Go service whose config file is named after its kind.
	Service bool
}

func newAgentData(name, kind, wake, relay, owner string, team []string) agentData {
	return agentData{Name: name, Kind: kind, Wake: wake, RelayURL: relay, Owner: owner, OwnerPoss: possessive(owner), Team: team,
		OwnConfig: freshSession[kind] || isService(kind), Service: isService(kind)}
}

type teamLine struct {
	Name, Kind, Wake string
	ExpectOnline     bool
}

type operatorData struct {
	Owner, OwnerPoss, RelayURL, Host string
	HostSet                          bool
	History                          string // name of the history agent on the roster, if any
	Team                             []teamLine
	WakeMethods                      []string
	Troubleshooting                  []string
}

// Build assembles the kit.
func Build(o Options) (Kit, error) {
	relay := strings.TrimSpace(o.RelayURL)
	if relay == "" {
		if !o.Offline {
			return Kit{}, errors.New("no relay configured: pass --relay <url> (or --offline to skip the roster)")
		}
		relay = relayPlaceholder
	}
	owner := strings.TrimSpace(o.Owner)
	if owner == "" {
		owner = "the owner"
	}
	for name, kind := range o.KindOverrides {
		// An empty override names no kind, so it is refused here.
		if kind == "" || !KnownKind(kind) {
			return Kit{}, fmt.Errorf("unknown kind %q for %s (want one of %s)", kind, name, strings.Join(Kinds, ", "))
		}
	}
	var roster []Member
	if !o.Offline {
		roster = o.Roster
	}
	names := make([]string, len(roster))
	for i, m := range roster {
		names[i] = m.Name
	}

	k := Kit{RelayURL: relay, Owner: owner, Host: strings.TrimSpace(o.Operator), Agents: []AgentBlock{}}
	op := operatorData{Owner: owner, OwnerPoss: possessive(owner), RelayURL: relay, Host: k.Host, HostSet: k.Host != ""}
	wakes := map[string]bool{}
	for _, m := range roster {
		kind := resolveKind(m, o.KindOverrides)
		wake := m.Wake
		if wake == "" {
			wake = defaultWake[kind]
		}
		wakes[wake] = true
		d := newAgentData(m.Name, kind, wake, relay, owner, others(names, m.Name))
		b, err := agentBlock(d)
		if err != nil {
			return Kit{}, err
		}
		k.Agents = append(k.Agents, b)
		if kind == KindHistory && op.History == "" {
			op.History = m.Name
		}
		op.Team = append(op.Team, teamLine{Name: m.Name, Kind: kind, Wake: wake, ExpectOnline: expectOnline(kind, wake)})
	}
	if len(roster) == 0 {
		for _, w := range wakeMethodOrder {
			wakes[w] = true
		}
	}
	for _, w := range wakeMethodOrder {
		if wakes[w] {
			op.WakeMethods = append(op.WakeMethods, w)
		}
	}
	ts, err := lines("troubleshooting", op)
	if err != nil {
		return Kit{}, err
	}
	for i, t := range ts {
		op.Troubleshooting = append(op.Troubleshooting, fmt.Sprintf("%d. %s", i+1, t))
	}
	if k.Operator, err = execute("operator", op); err != nil {
		return Kit{}, err
	}
	if k.Recipes, err = recipes(relay, owner); err != nil {
		return Kit{}, err
	}
	return k, nil
}

// resolveKind applies the order: override, stored kind, runtime name, generic.
func resolveKind(m Member, overrides map[string]string) string {
	if k := overrides[m.Name]; k != "" {
		return k
	}
	if slices.Contains(Kinds, m.Kind) {
		return m.Kind
	}
	if k, ok := runtimeNames[m.Name]; ok {
		return k
	}
	return KindGeneric
}

func expectOnline(kind, wake string) bool {
	switch kind {
	case KindVMWebhook, KindProxySandbox, KindHermes, KindOpenClaw:
		return true
	case KindGeneric:
		return wake == "webhook" || wake == "wait"
	}
	return false
}

// isService reports whether kind is a Tincan Go service rather than a
// model: history, notes, council, and the web agents.
func isService(kind string) bool {
	return kind == KindHistory || kind == KindNotes || kind == KindCouncil || isWebKind(kind)
}

// isWebKind reports whether kind is one of the web agents.
func isWebKind(kind string) bool {
	return kind == KindChatGPTWeb || kind == KindClaudeWeb || kind == KindGrokWeb || kind == KindGeminiWeb || kind == KindPerplexityWeb || kind == KindCopilotWeb || kind == KindDotWeb
}

func agentBlock(d agentData) (AgentBlock, error) {
	join, err := execute(templateFor("join."+d.Kind, "join.default"), d)
	if err != nil {
		return AgentBlock{}, err
	}
	common, err := execute("instructions.common", d)
	if err != nil {
		return AgentBlock{}, err
	}
	specific, err := execute("instructions."+d.Kind, d)
	if err != nil {
		return AgentBlock{}, err
	}
	instructions := strings.TrimSpace(common) + "\n" + strings.TrimSpace(specific)
	if isService(d.Kind) {
		// A Go service, not a model: there are no standing instructions.
		instructions = strings.TrimSpace(specific)
	}
	setup, err := lines("setup."+d.Kind, d)
	if err != nil {
		return AgentBlock{}, err
	}
	return AgentBlock{Name: d.Name, Kind: d.Kind, Wake: d.Wake, Join: strings.TrimSpace(join),
		Instructions: instructions, Setup: setup}, nil
}

// recipes builds the add-agent recipes from the same join and setup templates
// the agent blocks use, preceded by the admin invite step.
func recipes(relay, owner string) ([]Recipe, error) {
	var out []Recipe
	for _, kind := range Kinds {
		d := newAgentData("<name>", kind, defaultWake[kind], relay, owner, nil)
		title, err := execute("title."+kind, d)
		if err != nil {
			return nil, err
		}
		steps, err := lines(templateFor("recipe.invite."+kind, "recipe.invite"), d)
		if err != nil {
			return nil, err
		}
		setup, err := lines("setup."+kind, d)
		if err != nil {
			return nil, err
		}
		steps = append(steps, setup...)
		tail, err := lines("recipe.tail", d)
		if err != nil {
			return nil, err
		}
		out = append(out, Recipe{Kind: kind, Title: strings.TrimSpace(title), Steps: append(steps, tail...)})
	}
	for _, kind := range []string{RecipeSecondAgent, RecipeRelayHost} {
		d := agentData{RelayURL: relay, Owner: owner, OwnerPoss: possessive(owner)}
		ls, err := lines("recipe."+kind, d)
		if err != nil {
			return nil, err
		}
		out = append(out, Recipe{Kind: kind, Title: ls[0], Steps: ls[1:]})
	}
	return out, nil
}

// templateFor returns name when that template exists, else fallback, so a
// kind only defines the templates it overrides.
func templateFor(name, fallback string) string {
	if tmpl.Lookup(name) != nil {
		return name
	}
	return fallback
}

func execute(name string, data any) (string, error) {
	t := tmpl.Lookup(name)
	if t == nil {
		return "", fmt.Errorf("onboard: no template %q", name)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("onboard: %s: %w", name, err)
	}
	return b.String(), nil
}

// lines executes a template and returns its non-blank lines, trimmed.
func lines(name string, data any) ([]string, error) {
	s, err := execute(name, data)
	if err != nil {
		return nil, err
	}
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

func others(names []string, self string) []string {
	var out []string
	for _, n := range names {
		if n != self {
			out = append(out, n)
		}
	}
	return out
}

func possessive(owner string) string {
	if strings.HasSuffix(owner, "s") {
		return owner + "'"
	}
	return owner + "'s"
}

// Render formats the kit for people. section is one of Sections; anything
// else renders nothing.
func Render(k Kit, section string) string {
	var b strings.Builder
	op := section == "operator" || section == "all"
	ag := section == "agents" || section == "all"
	rc := section == "recipes" || section == "all"
	if op {
		b.WriteString("Operator prompt (paste into Agent Tincan's standing instructions)\n\n")
		b.WriteString(k.Operator)
		b.WriteString("\n")
	}
	if ag {
		if op {
			b.WriteString("\n")
		}
		if len(k.Agents) == 0 {
			b.WriteString("Agents: none on the roster. Use a recipe below, then re-run tincan onboard.\n")
		}
		for _, a := range k.Agents {
			fmt.Fprintf(&b, "Agent: %s (kind %s, wake %s)\n", a.Name, a.Kind, a.Wake)
			fmt.Fprintf(&b, "Join: %s\n", a.Join)
			b.WriteString("Standing instructions:\n")
			b.WriteString(indent(a.Instructions))
			b.WriteString("Setup:\n")
			for _, s := range a.Setup {
				fmt.Fprintf(&b, "  %s\n", s)
			}
			b.WriteString("\n")
		}
	}
	if rc {
		if op && !ag {
			b.WriteString("\n")
		}
		for _, r := range k.Recipes {
			fmt.Fprintf(&b, "Recipe: %s (%s)\n", r.Title, r.Kind)
			for i, s := range r.Steps {
				fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func indent(s string) string {
	var b strings.Builder
	for l := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}

// Profile is how the kit reads one roster entry.
type Profile struct {
	Kind, Wake string
	// Service: a Tincan Go service rather than a model agent. Web agents
	// are services that front a model, so Web is set with it.
	Service, Web bool
	// ExpectOnline: the agent is normally connected, not woken on demand.
	ExpectOnline bool
}

// ProfileOf resolves m's kind and wake method the way Build does (stored
// kind, then runtime name, then generic; the kind's default wake when m
// has none), so other packages classify agents exactly as the kit does.
func ProfileOf(m Member) Profile {
	kind := resolveKind(m, nil)
	wake := m.Wake
	if wake == "" {
		wake = defaultWake[kind]
	}
	return Profile{Kind: kind, Wake: wake, Service: isService(kind), Web: isWebKind(kind), ExpectOnline: expectOnline(kind, wake)}
}
