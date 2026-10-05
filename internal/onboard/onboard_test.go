package onboard

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const relayURL = "http://100.64.0.9:8787"

func matts() []Member {
	return []Member{
		{Name: "grokbot", Wake: "webhook", Online: true},
		{Name: "instinct", Wake: "email"},
		{Name: "muse", Wake: "wait", Online: true},
		{Name: "claude-code", Wake: "channel"},
	}
}

func build(t *testing.T, o Options) Kit {
	t.Helper()
	k, err := Build(o)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return k
}

func block(t *testing.T, k Kit, name string) AgentBlock {
	t.Helper()
	for _, a := range k.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no block for %q in %+v", name, k.Agents)
	return AgentBlock{}
}

func blockText(a AgentBlock) string {
	return a.Join + "\n" + a.Instructions + "\n" + strings.Join(a.Setup, "\n")
}

func recipe(t *testing.T, k Kit, kind string) Recipe {
	t.Helper()
	for _, r := range k.Recipes {
		if r.Kind == kind {
			return r
		}
	}
	t.Fatalf("no recipe %q", kind)
	return Recipe{}
}

func TestStoredKindsForMattsRoster(t *testing.T) {
	roster := matts()
	kinds := []string{"vm-webhook", "e2b-email", "proxy-sandbox", "claude-code"}
	for i := range roster {
		roster[i].Kind = kinds[i]
	}
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: roster})
	if len(k.Agents) != 4 {
		t.Fatalf("got %d blocks, want 4", len(k.Agents))
	}
	for i, a := range k.Agents {
		if a.Kind != kinds[i] {
			t.Errorf("%s kind = %q, want %q", a.Name, a.Kind, kinds[i])
		}
		if !strings.Contains(a.Join, "--relay "+relayURL) {
			t.Errorf("%s join %q lacks relay URL", a.Name, a.Join)
		}
	}
	if !strings.Contains(block(t, k, "muse").Join, "--proxy") {
		t.Error("proxy-sandbox join should carry --proxy")
	}
}

func TestUnknownAgentGetsGenericForWake(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: []Member{{Name: "zed", Wake: "command"}}})
	a := block(t, k, "zed")
	if a.Kind != "generic" || a.Wake != "command" {
		t.Fatalf("zed = %q/%q, want generic/command", a.Kind, a.Wake)
	}
	if !strings.Contains(blockText(a), "tincan listen --exec") {
		t.Errorf("generic command block should mention tincan listen --exec:\n%s", blockText(a))
	}
}

func TestStoredCodexAndPersonalNameFallback(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: []Member{
		{Name: "cx", Wake: "command", Kind: "codex"},
		{Name: "muse", Wake: "wait"},
		{Name: "codex", Wake: "command"},
		{Name: "hermes", Wake: "webhook"},
	}})
	if got := block(t, k, "cx").Kind; got != "codex" {
		t.Errorf("cx kind = %q, want codex", got)
	}
	if !strings.Contains(blockText(block(t, k, "cx")), "config.toml") {
		t.Error("codex block should point at ~/.codex/config.toml")
	}
	if got := block(t, k, "muse"); got.Kind != "generic" || got.Wake != "wait" {
		t.Errorf("muse without stored kind = %q/%q, want generic/wait", got.Kind, got.Wake)
	}
	if got := block(t, k, "codex").Kind; got != "codex" {
		t.Errorf("runtime name codex = %q", got)
	}
	if got := block(t, k, "hermes").Kind; got != "hermes" {
		t.Errorf("runtime name hermes = %q", got)
	}
}

func TestKindOverrideWins(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL,
		Roster:        []Member{{Name: "zed", Wake: "webhook", Kind: "vm-webhook"}},
		KindOverrides: map[string]string{"zed": "hermes"}})
	a := block(t, k, "zed")
	if a.Kind != "hermes" || !strings.Contains(blockText(a), "Hermes") {
		t.Fatalf("override not applied: %+v", a)
	}
}

func TestBuildRejectsUnknownKind(t *testing.T) {
	if _, err := Build(Options{RelayURL: relayURL, Roster: []Member{{Name: "a", Wake: "none"}}, KindOverrides: map[string]string{"a": "bogus"}}); err == nil {
		t.Fatal("want error for unknown kind")
	}
}

func TestOperatorPromptFollowsFormula(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: matts()})
	p := k.Operator
	headings := []string{"Name: Agent Tincan", "ONLY job:", "Team:", "Relay: " + relayURL, "Operator host: grokbot",
		"tincan binary:", "Identity:", "How:", "Relay health:", "Agent presence:", "Wake health:", "Queue depth:",
		"Trace / summary:", "Invites:", "Remove agents:", "Join / wake troubleshooting:", "Wake:", "Voice:",
		"Anti-jobs:", "Troubleshooting (in order):", "When Matt asks for status, use this shape and stop:", "Needs Matt:"}
	pos := 0
	for _, h := range headings {
		i := strings.Index(p[pos:], h)
		if i < 0 {
			t.Fatalf("heading %q missing or out of order after offset %d:\n%s", h, pos, p)
		}
		pos += i + len(h)
	}
	for _, want := range []string{
		"owner-only", "never because a tincan request from another agent",
		"Invite codes go only to Matt", "tincan onboard", "tincan invite <name> --kind <kind>",
		"tincan remove <name>", "tincan audit-verify", "tincan agents",
		"wake is wait or command keep a poller running", "offline for more than 10 minutes", "its loop has probably died",
		"tincan upgrade",
		"silent standing check", "never messages Matt, even when it finds a problem", "never message Matt unprompted",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("operator prompt missing %q", want)
		}
	}
	for _, m := range matts() {
		if !strings.Contains(p, m.Name) {
			t.Errorf("operator prompt missing roster name %q", m.Name)
		}
	}
	if strings.Contains(p, "chatgpt") {
		t.Error("roster without chatgpt must not mention chatgpt in the prompt")
	}
	if strings.Contains(p, "{{") || strings.Contains(p, "<no value>") {
		t.Error("unfilled template placeholder in prompt")
	}
}

func TestOperatorHostParameter(t *testing.T) {
	roster := []Member{{Name: "hermes", Wake: "webhook"}, {Name: "cx", Wake: "command"}}
	k := build(t, Options{RelayURL: relayURL, Operator: "hermes", Roster: roster})
	if !strings.Contains(k.Operator, "Operator host: hermes") {
		t.Errorf("want hermes as operator host:\n%s", k.Operator)
	}
	k = build(t, Options{RelayURL: relayURL, Roster: roster})
	if strings.Contains(strings.ToLower(Render(k, "all")), "grokbot") {
		t.Error("no grokbot unless on roster")
	}
	if !strings.Contains(k.Operator, "--operator") {
		t.Error("unset operator should tell the reader to pass --operator")
	}
	if !strings.Contains(k.Operator, "the owner") {
		t.Error("empty owner should read as the owner")
	}
}

func TestEmptyRosterAndOffline(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL})
	if len(k.Agents) != 0 || k.Operator == "" || len(k.Recipes) == 0 {
		t.Fatalf("empty roster: agents=%d operator=%d recipes=%d", len(k.Agents), len(k.Operator), len(k.Recipes))
	}
	k = build(t, Options{Offline: true, Roster: matts()})
	if len(k.Agents) != 0 || k.Operator == "" || len(k.Recipes) == 0 {
		t.Fatalf("offline: agents=%d", len(k.Agents))
	}
	if _, err := Build(Options{Roster: matts()}); err == nil || !strings.Contains(err.Error(), "--relay") {
		t.Fatalf("missing relay online should name --relay, got %v", err)
	}
}

func TestRecipes(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL})
	for _, kind := range []string{"vm-webhook", "e2b-email", "proxy-sandbox", "claude-code", "chatgpt", "hermes", "openclaw", "codex", "gemini-cli", "grok-cli", "history", "scheduled", "generic", "second-agent", "relay-host"} {
		r := recipe(t, k, kind)
		if r.Title == "" || len(r.Steps) < 2 {
			t.Errorf("recipe %s too thin: %+v", kind, r)
		}
	}
	for _, kind := range []string{"vm-webhook", "e2b-email", "proxy-sandbox", "claude-code", "hermes", "openclaw", "codex", "gemini-cli", "grok-cli", "history", "scheduled", "generic"} {
		r := recipe(t, k, kind)
		all := strings.Join(r.Steps, "\n")
		inv := strings.Index(all, "tincan invite <name> --kind "+kind)
		join := strings.Index(all, "tincan join")
		if inv < 0 || join < 0 || inv > join {
			t.Errorf("recipe %s must invite (admin) before join:\n%s", kind, all)
		}
	}
	host := strings.Join(recipe(t, k, "relay-host").Steps, "\n")
	for _, want := range []string{"tincan relay --listen <tailscale-ip> --port 8787 --admin", "tsnet", "TS_AUTHKEY", "--hostname", "changes if the host re-joins Tailscale", "proxy-only"} {
		if !strings.Contains(host, want) {
			t.Errorf("relay-host recipe missing %q", want)
		}
	}
	second := strings.Join(recipe(t, k, "second-agent").Steps, "\n")
	if !strings.Contains(second, "TINCAN_CONFIG") {
		t.Error("second-agent recipe must set TINCAN_CONFIG")
	}
}

func TestFreshSessionRulesAndSecretFields(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: []Member{
		{Name: "hermes", Wake: "webhook"}, {Name: "openclaw", Wake: "webhook"}, {Name: "codex", Wake: "command"},
	}})
	for _, n := range []string{"hermes", "openclaw", "codex"} {
		txt := block(t, k, n).Instructions
		if !strings.Contains(txt, "drain the whole inbox") || !strings.Contains(txt, "woken when a reply arrives") {
			t.Errorf("%s block lacks fresh-session rules:\n%s", n, txt)
		}
	}
	if !strings.Contains(blockText(block(t, k, "hermes")), "hmac_secret") {
		t.Error("hermes block should name hmac_secret")
	}
	if !strings.Contains(blockText(block(t, k, "openclaw")), "bearer_token") {
		t.Error("openclaw block should name bearer_token")
	}
}

// Replies to a fresh-session agent's own asks wake it now, so its block must
// say so and drop the old advice to treat every ask as synchronous.
func TestFreshSessionReplyWakeGuidance(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: []Member{
		{Name: "hermes", Wake: "webhook"}, {Name: "openclaw", Wake: "webhook"}, {Name: "codex", Wake: "command"},
	}})
	for _, n := range []string{"hermes", "openclaw", "codex"} {
		txt := block(t, k, n).Instructions
		for _, want := range []string{"may return before", "woken when a reply arrives", "check_inbox shows replies to your requests", "finish the work that was waiting on it", "When check_inbox shows a reply tied to one of your open requests, finish that request and reply to it."} {
			if !strings.Contains(txt, want) {
				t.Errorf("%s block missing %q:\n%s", n, want, txt)
			}
		}
		for _, stale := range []string{"synchronous", "does not wake you"} {
			if strings.Contains(txt, stale) {
				t.Errorf("%s block still says %q:\n%s", n, stale, txt)
			}
		}
	}
	if setup := blockText(block(t, k, "codex")); !strings.Contains(setup, "requests or replies are waiting") {
		t.Errorf("codex listener prompt should cover replies:\n%s", setup)
	}
}

// The Codex recipe wakes through the wake script, which takes a lock and
// runs a sandboxed codex exec, rather than a raw codex exec line, and does
// not assume a repo checkout at a fixed path.
// grok-cli is a command-woken fresh-session kind: its block sets up a wake
// home with only this teammate's tincan server, pins TINCAN_CONFIG, logs
// in there, and wakes through the grok wake script.
func TestGrokCLIBlock(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "grok-cli", Kind: KindGrokCLI}}})
	a := block(t, k, "grok-cli")
	if a.Kind != KindGrokCLI || a.Wake != "command" {
		t.Fatalf("block kind %q wake %q, want grok-cli and command", a.Kind, a.Wake)
	}
	if !strings.Contains(a.Instructions, "drain the whole inbox") {
		t.Errorf("grok-cli instructions lack the fresh-session rules:\n%s", a.Instructions)
	}
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{
		"Grok Build", "grok --version",
		"HOME=~/.config/tincan/grok-cli.wake GROK_HOME=~/.config/tincan/grok-cli.wake/.grok grok mcp add agent-tincan -e TINCAN_CONFIG=<full path of ~/.config/tincan/grok-cli.json> -- tincan mcp",
		"GROK_HOME=~/.config/tincan/grok-cli.wake/.grok grok login", "XAI_API_KEY",
		"examples/grok-cli/grok-wake.sh", "examples/lib/tincan-wake-lib.sh", "chmod +x",
		"TINCAN_CONFIG=~/.config/tincan/grok-cli.json tincan listen --exec ~/bin/grok-wake.sh",
		"--sandbox", "TINCAN_GROK_WRITE_ROOTS",
		`method "command"`, "docs/adapters/grok-cli.md",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("grok-cli setup missing %q:\n%s", want, setup)
		}
	}
	// The runtime name alone maps to the kind, and the recipe invites with it.
	k = build(t, Options{RelayURL: relayURL, Roster: []Member{{Name: "grok-cli"}}})
	if b := block(t, k, "grok-cli"); b.Kind != KindGrokCLI {
		t.Fatalf("runtime name grok-cli resolved to %q", b.Kind)
	}
	r := recipe(t, k, KindGrokCLI)
	if !strings.Contains(r.Title, "Grok") || !strings.Contains(strings.Join(r.Steps, "\n"), "tincan invite <name> --kind grok-cli") {
		t.Fatalf("grok-cli recipe: %+v", r)
	}
	// The history agent's block names Grok CLI among its sources.
	k = build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "history", Kind: KindHistory}}})
	if h := block(t, k, "history"); !strings.Contains(h.Instructions, "Grok CLI") {
		t.Fatalf("history instructions do not name Grok CLI:\n%s", h.Instructions)
	}
}

func TestCodexSetupUsesWakeScript(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: []Member{{Name: "codex", Wake: "command", Kind: "codex"}}})
	setup := strings.Join(block(t, k, "codex").Setup, "\n")
	for _, want := range []string{"examples/codex/codex-wake.sh", "tincan listen --exec ~/bin/codex-wake.sh", "chmod +x", "lock", "--sandbox workspace-write", "TINCAN_CODEX_WRITE_ROOTS"} {
		if !strings.Contains(setup, want) {
			t.Errorf("codex setup missing %q:\n%s", want, setup)
		}
	}
	for _, stale := range []string{`--exec 'codex exec`, "~/agent-tincan/"} {
		if strings.Contains(setup, stale) {
			t.Errorf("codex setup still has %q:\n%s", stale, setup)
		}
	}
}

// gemini-cli is a command-woken fresh-session kind with two engines: its
// block explains both, pins TINCAN_CONFIG in the MCP add step, and wakes
// through the gemini wake script.
func TestGeminiCLIBlock(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "gemini-cli", Kind: KindGeminiCLI}}})
	a := block(t, k, "gemini-cli")
	if a.Kind != KindGeminiCLI || a.Wake != "command" {
		t.Fatalf("block kind %q wake %q, want gemini-cli and command", a.Kind, a.Wake)
	}
	if !strings.Contains(a.Instructions, "drain the whole inbox") {
		t.Errorf("gemini-cli instructions lack the fresh-session rules:\n%s", a.Instructions)
	}
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{
		"agy mcp add", "gemini mcp add", "TINCAN_CONFIG", "~/.config/tincan/gemini-cli.json",
		"GEMINI_API_KEY", "TINCAN_GEMINI_ENGINE", "2026-06-18", "trust",
		"examples/gemini-cli/gemini-wake.sh", "examples/lib/tincan-wake-lib.sh", "chmod +x",
		"TINCAN_CONFIG=~/.config/tincan/gemini-cli.json tincan listen --exec ~/bin/gemini-wake.sh",
		"TINCAN_GEMINI_ALLOW_UNCONFINED", "--sandbox",
		"TINCAN_GEMINI_ALLOW_UNCONFINED=1 TINCAN_CONFIG=~/.config/tincan/gemini-cli.json tincan listen --exec ~/bin/gemini-wake.sh",
		"Set up with a Google account (no API key)", "tincan-gemini-cli-wake/backoff", "tincan ask gemini-cli",
		`method "command"`, "docs/adapters/gemini-cli.md",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("gemini-cli setup missing %q:\n%s", want, setup)
		}
	}
	// The Google-account path (agy and its opt-in) comes before the API-key one.
	if strings.Index(setup, "agy mcp add") > strings.Index(setup, "gemini mcp add") ||
		strings.Index(setup, "TINCAN_GEMINI_ALLOW_UNCONFINED=1") > strings.Index(setup, "GEMINI_API_KEY") {
		t.Errorf("gemini-cli setup does not lead with the agy path:\n%s", setup)
	}
	// The runtime name alone maps to the kind, and the recipe invites with it.
	k = build(t, Options{RelayURL: relayURL, Roster: []Member{{Name: "gemini-cli"}}})
	if b := block(t, k, "gemini-cli"); b.Kind != KindGeminiCLI {
		t.Fatalf("runtime name gemini-cli resolved to %q", b.Kind)
	}
	r := recipe(t, k, KindGeminiCLI)
	if !strings.Contains(r.Title, "Gemini") || !strings.Contains(strings.Join(r.Steps, "\n"), "tincan invite <name> --kind gemini-cli") {
		t.Fatalf("gemini-cli recipe: %+v", r)
	}
}

var (
	codeShape = regexp.MustCompile(`\b[A-Z2-9]{4}-[A-Z2-9]{4}\b`)
	secretish = regexp.MustCompile(`(?i)(https://hooks\.|agentmail_key"\s*:\s*"[^<]|sk-[a-z0-9]{8})`)
)

func TestRenderedOutputHygiene(t *testing.T) {
	kits := []Kit{
		build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: append(matts(),
			Member{Name: "hermes", Wake: "webhook"}, Member{Name: "openclaw", Wake: "webhook"},
			Member{Name: "codex", Wake: "command"}, Member{Name: "chatgpt", Wake: "none"},
			Member{Name: "gemini-cli", Wake: "command", Kind: KindGeminiCLI},
			Member{Name: "zed", Wake: "command"}, Member{Name: "q", Wake: "none"},
			Member{Name: "history", Wake: "wait", Kind: "history"})}),
		build(t, Options{Offline: true}),
		build(t, Options{RelayURL: relayURL}),
	}
	for _, k := range kits {
		raw, _ := json.Marshal(k)
		for _, out := range []string{Render(k, "all"), string(raw)} {
			for _, bad := range []string{"Tin Can bot", "Tincan bot", "\u2014", "\u2013", "**"} {
				if strings.Contains(out, bad) {
					t.Errorf("output contains forbidden %q", bad)
				}
			}
			if m := codeShape.FindString(out); m != "" {
				t.Errorf("invite-code-shaped string %q in output", m)
			}
			if m := secretish.FindString(out); m != "" {
				t.Errorf("secret-looking string %q", m)
			}
		}
		if !strings.Contains(Render(k, "all"), "tincan invite") {
			t.Error("output should carry tincan invite guidance")
		}
	}
}

func TestRenderSections(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: matts()})
	op, ag, rc := Render(k, "operator"), Render(k, "agents"), Render(k, "recipes")
	if !strings.Contains(op, "Name: Agent Tincan") || strings.Contains(op, "Recipe:") {
		t.Error("operator section wrong")
	}
	if !strings.Contains(ag, "grokbot") || strings.Contains(ag, "Name: Agent Tincan") {
		t.Error("agents section wrong")
	}
	if !strings.Contains(rc, "tincan relay --listen") || strings.Contains(rc, "Name: Agent Tincan") {
		t.Error("recipes section wrong")
	}
	all := Render(k, "all")
	if !strings.Contains(all, op) || !strings.Contains(all, rc) {
		t.Error("all should contain every section")
	}
}

// tincan wait also exits for a reply to the agent's own request, printing a
// count rather than a request, so wait-method instructions must say what to
// do then. Relay-side wakes carry replies too, so their blocks say so.
func TestWakeBlocksMentionReplies(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: []Member{
		{Name: "muse", Wake: "wait", Kind: "proxy-sandbox"},
		{Name: "zed", Wake: "wait"},
		{Name: "grokbot", Wake: "webhook", Kind: "vm-webhook"},
		{Name: "instinct", Wake: "email", Kind: "e2b-email"},
		{Name: "hook", Wake: "webhook"},
		{Name: "mail", Wake: "email"},
		{Name: "claude-code", Wake: "channel", Kind: "claude-code"},
	}})
	for _, n := range []string{"muse", "zed"} {
		txt := block(t, k, n).Instructions
		for _, want := range []string{"a count of replies to your own requests", "tincan inbox", "finish the work that was waiting", "start tincan wait & again"} {
			if !strings.Contains(txt, want) {
				t.Errorf("%s wait block missing %q:\n%s", n, want, txt)
			}
		}
		if strings.Contains(txt, "it prints a teammate's request:") {
			t.Errorf("%s wait block still says the wait only ends with a request:\n%s", n, txt)
		}
	}
	for _, n := range []string{"grokbot", "instinct", "hook", "mail", "claude-code"} {
		if txt := block(t, k, n).Instructions; !strings.Contains(txt, "reply to your own request") {
			t.Errorf("%s block should say a wake can mean a reply to its own request:\n%s", n, txt)
		}
	}
}

func historyRoster() []Member {
	return append(matts(), Member{Name: "history", Wake: "wait", Kind: "history"})
}

// The history agent is a Go service, not a model: its block has nothing to
// paste, joins with its own config, installs the native host and service,
// and names the extension install as the one human step.
func TestHistoryBlock(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: historyRoster()})
	a := block(t, k, "history")
	if a.Kind != "history" || a.Wake != "wait" {
		t.Fatalf("history = %q/%q, want history/wait", a.Kind, a.Wake)
	}
	if want := "TINCAN_CONFIG=~/.config/tincan/history.json tincan join <code> --relay " + relayURL; a.Join != want {
		t.Errorf("history join = %q, want %q", a.Join, want)
	}
	for _, want := range []string{"service", "nothing to paste", "asked ChatGPT, claude.ai, Grok, Gemini, Copilot, Codex, Claude Code and Grok CLI"} {
		if !strings.Contains(a.Instructions, want) {
			t.Errorf("history instructions missing %q:\n%s", want, a.Instructions)
		}
	}
	for _, stale := range []string{"check_inbox", "You are history"} {
		if strings.Contains(a.Instructions, stale) {
			t.Errorf("history instructions should not carry model guidance %q:\n%s", stale, a.Instructions)
		}
	}
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{
		"tincan history install", "native messaging host", "service definition",
		"launchctl bootstrap", "systemctl --user",
		"Tincan Chrome extension", "Chrome Web Store", "chrome://extensions", "unpacked",
		"tincan-history-extension.zip", "Developer mode", "Load unpacked", "tincan history install --extension-dir",
		"only human step", "history-allow.txt", `method "wait"`,
		"codex login status",
		"TINCAN_CONFIG=~/.config/tincan/history.json tincan rejoin --relay " + relayURL + " --name history",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("history setup missing %q:\n%s", want, setup)
		}
	}
	if strings.Contains(setup, "load extension/ unpacked") {
		t.Errorf("history setup assumes a repo checkout:\n%s", setup)
	}
	r := recipe(t, k, "history")
	if want := "History service for ChatGPT, claude.ai, Grok, Gemini, Copilot, Codex, Claude Code and Grok CLI chats"; r.Title != want {
		t.Errorf("history recipe title = %q, want %q", r.Title, want)
	}
	all := strings.Join(r.Steps, "\n")
	if !strings.Contains(all, "tincan invite <name> --kind history") || !strings.Contains(all, "TINCAN_CONFIG=~/.config/tincan/history.json tincan join") {
		t.Errorf("history recipe should invite then join with its own config:\n%s", all)
	}
	if got := block(t, build(t, Options{RelayURL: relayURL, Roster: []Member{{Name: "history", Wake: "wait"}}}), "history").Kind; got != "history" {
		t.Errorf("runtime name history = %q, want history", got)
	}
}

// With a history agent on the roster, Agent Tincan routes history questions
// to it verbatim and forwards the reply with its images, without breaking the
// formula's section order or the quiet rule.
func TestOperatorRoutesHistory(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: historyRoster()})
	p := k.Operator
	headings := []string{"Name: Agent Tincan", "ONLY job:", "Team:", "How:", "Trace / summary:", "History questions:",
		"Invites:", "Wake:", "Voice:", "Anti-jobs:", "Troubleshooting (in order):", "When Matt asks for status"}
	pos := 0
	for _, h := range headings {
		i := strings.Index(p[pos:], h)
		if i < 0 {
			t.Fatalf("heading %q missing or out of order after offset %d:\n%s", h, pos, p)
		}
		pos += i + len(h)
	}
	for _, want := range []string{
		"ChatGPT", "claude.ai", "Codex", "Claude Code", "send the image",
		"to history", "as-is", "every image", "tincan attachment get",
		"allowlist", "directly",
		"never messages Matt, even when it finds a problem", "never message Matt unprompted",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("operator prompt missing %q", want)
		}
	}
	plain := build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: matts()}).Operator
	if strings.Contains(plain, "History questions:") {
		t.Error("no history routing unless a history agent is on the roster")
	}
}

func TestRenderedInstructionsIncludeUpgradeGuidance(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: matts()})
	want := "When tincan says a newer release is available from the relay, run tincan upgrade and restart your own long-running tincan processes (wait loop, listener, MCP server). Tell Matt if you cannot."
	for _, a := range k.Agents {
		if !strings.Contains(a.Instructions, want) {
			t.Errorf("%s instructions lack upgrade guidance: %s", a.Name, a.Instructions)
		}
	}
}

// Every model agent, the scheduled kind included, is told to choose a
// teammate by its good_at line in the live roster, to send a real-world
// action to one teammate at a time, and to leave lines to the owner. Service
// blocks carry no model guidance.
func TestRenderedInstructionsIncludeGoodAtGuidance(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: append(matts(),
		Member{Name: "fo", Kind: KindScheduled}, Member{Name: "hermes", Wake: "webhook"},
		Member{Name: "codex", Wake: "command"}, Member{Name: "chatgpt", Wake: "none"},
		Member{Name: "history", Kind: KindHistory}, Member{Name: "notes", Kind: KindNotes},
		Member{Name: "council", Kind: KindCouncil}, Member{Name: "chatgpt-web", Kind: KindChatGPTWeb})})
	want := []string{
		"read the good_at lines in the live roster (list_agents, or tincan agents from a shell)",
		"Send a real-world action (a call, a payment, a booking) to one teammate only",
		"Ask another only after the first declines, fails or hands it back, or after you cancel your request to it.",
		"If the relay tells you a woken teammate has not checked in and the work can't wait, cancel that request and ask another online teammate whose good_at line fits.",
		"Only Matt sets good-at lines",
		"work Matt has already authorized",
		"needs_input is for a missing detail, never for permission",
		"Never ignore a request: every request you receive ends with a reply",
		"Claimed work comes before anything else in a turn, including Matt's chat.",
	}
	var sawScheduled bool
	for _, a := range k.Agents {
		for _, w := range want {
			if got := strings.Contains(a.Instructions, w); got == isService(a.Kind) {
				t.Errorf("%s (%s): contains %q = %v", a.Name, a.Kind, w, got)
			}
		}
		sawScheduled = sawScheduled || a.Kind == KindScheduled
	}
	if !sawScheduled {
		t.Fatal("kit has no scheduled block")
	}
}

// notes is a product service: its runtime name resolves to the notes kind,
// it waits on the relay, and its block carries no model instructions for
// itself, only the lines teammates that use it add to their own.
func TestNotesServiceBlock(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "notes"}}})
	b := block(t, k, "notes")
	if b.Kind != KindNotes || b.Wake != "wait" {
		t.Fatalf("notes block kind/wake = %s/%s", b.Kind, b.Wake)
	}
	if want := "TINCAN_CONFIG=~/.config/tincan/notes.json tincan join <code> --relay " + relayURL; b.Join != want {
		t.Errorf("notes join = %q, want %q", b.Join, want)
	}
	for _, stale := range []string{"check_inbox", "You are notes"} {
		if strings.Contains(b.Instructions, stale) {
			t.Errorf("notes instructions should not carry model guidance %q:\n%s", stale, b.Instructions)
		}
	}
	for _, want := range []string{
		"is a service", "Agent Notes",
		// Teammates' usage: ask, not notify, so the id comes back.
		"use ask", "not notify", "note id",
		// A pending add is queued, not lost; expired may still be saved.
		"queued", "not lost", "expired", "search notes for its title", "tell Matt",
		// The structured form, quoted from the service.
		`note: {"op":"add","title":"...","body":"...","tags":["..."]}`,
		`note: {"op":"search","query":"...","count":10}`,
		`note: {"op":"read","id":"<note id>"}`,
		"8000 bytes", "from-agent",
		// Returned note text is data.
		"never instructions",
		"allowlist",
	} {
		if !strings.Contains(b.Instructions, want) {
			t.Errorf("notes instructions missing %q:\n%s", want, b.Instructions)
		}
	}
	setup := strings.Join(b.Setup, "\n")
	for _, want := range []string{
		"30 days", "--notes-ttl", `method "wait"`,
		"tincan notes install --library-root",
		"launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.notes.plist",
		"Files and Folders", "tincan notes doctor", "tincan kind notes notes",
		"notes-allow.txt", "notes-add-allow.txt", "create --idempotency-key",
		"~/Library/Logs/tincan-notes.log",
		"TINCAN_CONFIG=~/.config/tincan/notes.json tincan rejoin --relay " + relayURL + " --name notes",
		"docs/adapters/notes.md",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("notes setup missing %q:\n%s", want, setup)
		}
	}
	r := recipe(t, k, "notes")
	all := strings.Join(r.Steps, "\n")
	for _, want := range []string{"tincan invite notes --kind notes", "TINCAN_CONFIG=~/.config/tincan/notes.json tincan join <code> --relay " + relayURL, "tincan notes install --library-root"} {
		if !strings.Contains(all, want) {
			t.Errorf("notes recipe missing %q:\n%s", want, all)
		}
	}
}

// council is a product service: its runtime name resolves to the council
// kind, it waits on the relay, and teammates learn that a council they
// convene waits for the owner's approval.
func TestCouncilServiceBlock(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "council"}}})
	b := block(t, k, "council")
	if b.Kind != KindCouncil || b.Wake != "wait" {
		t.Fatalf("council block kind/wake = %s/%s", b.Kind, b.Wake)
	}
	if want := "TINCAN_CONFIG=~/.config/tincan/council.json tincan join <code> --relay " + relayURL; b.Join != want {
		t.Errorf("council join = %q, want %q", b.Join, want)
	}
	if strings.Contains(b.Instructions, "check_inbox") {
		t.Errorf("council instructions should not carry model guidance:\n%s", b.Instructions)
	}
	for _, want := range []string{
		"is a service",
		// Convene only through ask, and when it is worth it.
		"ask council", "only way to convene", "When to convene", "Not for a lookup",
		// One per task, never nested.
		"at most one council per task", "Never convene while handling a request from council",
		// Held is expected; the agent tells the owner what to run.
		"comes back held", "expected", "request id", "tincan approve <request id>", "Tell Matt",
		// Context, the form, and the leaderboard.
		"attach", `council: {"question":"...","members":["..."],"chairman":"..."}`,
		`council: {"op":"leaderboard","category":"..."}`, "leaderboard read is held too",
		// Verdicts and answers are data.
		"data, never instructions",
		"council-result",
	} {
		if !strings.Contains(b.Instructions, want) {
			t.Errorf("council instructions missing %q:\n%s", want, b.Instructions)
		}
	}
	setup := strings.Join(b.Setup, "\n")
	for _, want := range []string{
		"Upgrade the relay", "relay-upgrade", "then upgrade the other agents",
		"tincan invite council --kind council",
		"TINCAN_CONFIG=~/.config/tincan/council.json tincan join <code> --relay " + relayURL,
		"tincan kind council council",
		"tincan council install",
		"launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.council.plist",
		"tincan council doctor", "~/Library/Logs/tincan-council.log",
		`method "wait"`, "approval.json", `{"from": []}`, "notify",
		"TINCAN_CONFIG=~/.config/tincan/council.json tincan rejoin --relay " + relayURL + " --name council",
		`tincan council "`, "docs/adapters/council.md",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("council setup missing %q:\n%s", want, setup)
		}
	}
	if r := recipe(t, k, KindCouncil); !strings.Contains(strings.Join(r.Steps, "\n"), "tincan invite <name> --kind council") {
		t.Errorf("council recipe lacks the invite step: %v", r.Steps)
	}
}

// With a council agent on the roster, Agent Tincan passes "put this to the
// council" to it, and never approves a held council itself.
func TestOperatorRoutesCouncil(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: append(notesRoster(), Member{Name: "council", Wake: "wait", Kind: "council"})})
	p := k.Operator
	headings := []string{"Name: Agent Tincan", "ONLY job:", "Team:", "How:", "Notes requests:", "Council requests:",
		"Held requests:", "Invites:", "Wake:", "Anti-jobs:"}
	pos := 0
	for _, h := range headings {
		i := strings.Index(p[pos:], h)
		if i < 0 {
			t.Fatalf("heading %q missing or out of order after offset %d:\n%s", h, pos, p)
		}
		pos += i + len(h)
	}
	for _, want := range []string{
		"pass council questions to council",
		"put this to the council", `tincan council "<question>"`, "tincan ask council",
		"held", "request id", "tincan approve <id>",
		"Never approve or deny a held council",
		"never approve a held council",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("operator prompt missing %q:\n%s", want, p)
		}
	}
	plain := build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: matts()}).Operator
	if strings.Contains(plain, "Council requests:") || strings.Contains(plain, "council questions") {
		t.Error("no council routing unless a council agent is on the roster")
	}
}

func notesRoster() []Member {
	return append(matts(), Member{Name: "notes", Wake: "wait", Kind: "notes"})
}

// With a notes agent on the roster, Agent Tincan routes requests to save,
// find, or read a note to it, in the formula's section order.
func TestOperatorRoutesNotes(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: append(historyRoster(), Member{Name: "notes", Wake: "wait", Kind: "notes"})})
	p := k.Operator
	headings := []string{"Name: Agent Tincan", "ONLY job:", "Team:", "How:", "History questions:", "Notes requests:",
		"Invites:", "Wake:", "Anti-jobs:"}
	pos := 0
	for _, h := range headings {
		i := strings.Index(p[pos:], h)
		if i < 0 {
			t.Fatalf("heading %q missing or out of order after offset %d:\n%s", h, pos, p)
		}
		pos += i + len(h)
	}
	for _, want := range []string{
		"pass notes requests to notes",
		"save, find, or read a note", "tincan ask notes", "not notify", "expired", "search notes for its title",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("operator prompt missing %q:\n%s", want, p)
		}
	}
	if only := build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: notesRoster()}).Operator; !strings.Contains(only, "Notes requests:") {
		t.Error("notes routing should not depend on a history agent")
	}
	plain := build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: matts()}).Operator
	if strings.Contains(plain, "Notes requests:") || strings.Contains(plain, "notes requests") {
		t.Error("no notes routing unless a notes agent is on the roster")
	}
}

// Each kind with a local tincan mcp server is told how its app reloads it
// after an upgrade, since the server keeps the old build until then.
func TestInstructionsNameMCPReloadPerKind(t *testing.T) {
	cases := map[string]string{
		KindClaudeCode: "quit Claude Code and start it again",
		KindCodex:      "start a new Codex session",
		KindGeminiCLI:  "reload the tincan MCP server in your app's settings",
		KindGeneric:    "reload the tincan MCP server in your app's settings",
	}
	for kind, want := range cases {
		k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "a", Wake: "none", Kind: kind}}})
		if got := block(t, k, "a").Instructions; !strings.Contains(got, want) || !strings.Contains(got, "different build") {
			t.Errorf("%s instructions lack %q: %s", kind, want, got)
		}
	}
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "a", Wake: "none", Kind: KindChatGPT}}})
	if got := block(t, k, "a").Instructions; strings.Contains(got, "different build") {
		t.Errorf("chatgpt runs no local tincan mcp but was told to reload one: %s", got)
	}
}

// A scheduled agent cannot be woken; a platform cron starts a fresh session
// on its own interval. Its instructions must stand alone in that cron job,
// and its setup names the wake.json schedule entry, the matching cron and
// the proxy join.
func TestScheduledBlock(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "fo", Kind: KindScheduled}}})
	a := block(t, k, "fo")
	if a.Wake != "schedule" {
		t.Errorf("scheduled wake = %q, want schedule", a.Wake)
	}
	for _, want := range []string{"tincan inbox", "no memory", "reply", "needs_input", "replies to your own requests", "finish the work that was waiting", "progress", "needs a human", "inbox is empty", "do nothing else"} {
		if !strings.Contains(a.Instructions, want) {
			t.Errorf("scheduled instructions missing %q:\n%s", want, a.Instructions)
		}
	}
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{"wake.json", "chmod 600", `{"fo": {"method": "schedule", "every": "5m"}}`, "restart the relay", "refuses to start", "same interval", "every 5 minutes", "cron job", "tincan join <code> --relay " + relayURL + " --proxy http://localhost:<port>"} {
		if !strings.Contains(setup, want) {
			t.Errorf("scheduled setup missing %q:\n%s", want, setup)
		}
	}
	r := recipe(t, k, KindScheduled)
	if !strings.Contains(r.Title, "schedule") {
		t.Errorf("scheduled recipe title %q", r.Title)
	}
	if all := strings.Join(r.Steps, "\n"); !strings.Contains(all, `"method": "schedule"`) || !strings.Contains(all, "--proxy http://localhost:<port>") {
		t.Errorf("scheduled recipe lacks the schedule entry or proxy tip:\n%s", all)
	}
	if !slices.Contains(opWakes(t, k), "schedule") {
		t.Errorf("operator wake methods should list schedule:\n%s", k.Operator)
	}
}

func opWakes(t *testing.T, k Kit) []string {
	t.Helper()
	const marker = "for each wake method in use ("
	i := strings.Index(k.Operator, marker)
	if i < 0 {
		t.Fatalf("operator prompt lacks the wake-method list:\n%s", k.Operator)
	}
	rest := k.Operator[i+len(marker):]
	return strings.Split(rest[:strings.Index(rest, ")")], ", ")
}

// A generic agent's setup names the every field a schedule entry needs and
// points cron-only agents at the scheduled kind.
func TestGenericSetupExplainsSchedule(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "g", Wake: "none", Kind: KindGeneric}}})
	setup := strings.Join(block(t, k, "g").Setup, "\n")
	for _, want := range []string{`"every": "5m"`, "kind scheduled"} {
		if !strings.Contains(setup, want) {
			t.Errorf("generic setup missing %q:\n%s", want, setup)
		}
	}
}
