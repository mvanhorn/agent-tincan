package onboard

import (
	"encoding/json"
	"regexp"
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
		"Anti-jobs:", "Troubleshooting (in order):", "When reporting, use this shape and stop:", "Needs Matt:"}
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
	for _, kind := range []string{"vm-webhook", "e2b-email", "proxy-sandbox", "claude-code", "chatgpt", "hermes", "openclaw", "codex", "generic", "second-agent", "relay-host"} {
		r := recipe(t, k, kind)
		if r.Title == "" || len(r.Steps) < 2 {
			t.Errorf("recipe %s too thin: %+v", kind, r)
		}
	}
	for _, kind := range []string{"vm-webhook", "e2b-email", "proxy-sandbox", "claude-code", "hermes", "openclaw", "codex", "generic"} {
		r := recipe(t, k, kind)
		all := strings.Join(r.Steps, "\n")
		inv := strings.Index(all, "tincan invite <name> --kind "+kind)
		join := strings.Index(all, "tincan join")
		if inv < 0 || join < 0 || inv > join {
			t.Errorf("recipe %s must invite (admin) before join:\n%s", kind, all)
		}
	}
	host := strings.Join(recipe(t, k, "relay-host").Steps, "\n")
	for _, want := range []string{"tincan relay --listen <tailscale-ip> --port 8787 --admin", "tsnet", "TS_AUTHKEY", "--hostname"} {
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

var (
	codeShape = regexp.MustCompile(`\b[A-Z2-9]{4}-[A-Z2-9]{4}\b`)
	secretish = regexp.MustCompile(`(?i)(https://hooks\.|agentmail_key"\s*:\s*"[^<]|sk-[a-z0-9]{8})`)
)

func TestRenderedOutputHygiene(t *testing.T) {
	kits := []Kit{
		build(t, Options{RelayURL: relayURL, Owner: "Matt", Operator: "grokbot", Roster: append(matts(),
			Member{Name: "hermes", Wake: "webhook"}, Member{Name: "openclaw", Wake: "webhook"},
			Member{Name: "codex", Wake: "command"}, Member{Name: "chatgpt", Wake: "none"},
			Member{Name: "zed", Wake: "command"}, Member{Name: "q", Wake: "none"})}),
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
