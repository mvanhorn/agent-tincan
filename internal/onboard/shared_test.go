package onboard

import (
	"regexp"
	"strings"
	"testing"
)

// OpenClaw must be woken on /hooks/agent, which starts a turn; /hooks/wake
// only queues the text for the next heartbeat.
func TestOpenClawUsesHooksAgent(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: []Member{{Name: "claw", Kind: KindOpenClaw}}})
	for label, txt := range map[string]string{
		"block":  blockText(block(t, k, "claw")),
		"recipe": strings.Join(recipe(t, k, KindOpenClaw).Steps, "\n"),
	} {
		if !strings.Contains(txt, "POST to /hooks/agent") || !strings.Contains(txt, "gateway /hooks/agent URL") {
			t.Errorf("openclaw %s should name /hooks/agent for the POST and the url:\n%s", label, txt)
		}
		if strings.Contains(txt, "POST to /hooks/wake") || strings.Contains(txt, "gateway /hooks/wake URL") {
			t.Errorf("openclaw %s still points the wake at /hooks/wake:\n%s", label, txt)
		}
	}
}

var tincanCmd = regexp.MustCompile(`(?:TINCAN_CONFIG=\S+ )?tincan (?:join|rejoin|listen|mcp)\b`)

// Kinds that commonly share a machine carry a per-agent TINCAN_CONFIG on
// every generated tincan command, set it in their MCP entry, and name the
// agent on rejoin, so a second agent never acts as the first.
func TestSharedKindsCarryPerAgentConfig(t *testing.T) {
	shared := []string{KindHermes, KindOpenClaw, KindCodex}
	var roster []Member
	for _, kind := range shared {
		roster = append(roster, Member{Name: "a-" + kind, Kind: kind})
	}
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: roster})
	for _, kind := range shared {
		name := "a-" + kind
		prefix := "TINCAN_CONFIG=~/.config/tincan/" + name + ".json "
		a := block(t, k, name)
		if !strings.HasPrefix(a.Join, prefix+"tincan join") {
			t.Errorf("%s join %q lacks %q", kind, a.Join, prefix)
		}
		wantRejoin := prefix + "tincan rejoin --relay " + relayURL + " --name " + name
		if !strings.Contains(a.Instructions, wantRejoin) {
			t.Errorf("%s self-heal line lacks %q:\n%s", kind, wantRejoin, a.Instructions)
		}
		txt := blockText(a)
		for _, m := range tincanCmd.FindAllString(a.Join+"\n"+strings.Join(a.Setup, "\n"), -1) {
			if !strings.HasPrefix(m, prefix) && !strings.HasSuffix(m, "tincan mcp") {
				t.Errorf("%s command %q lacks the per-agent config", kind, m)
			}
		}
		setup := strings.Join(a.Setup, "\n")
		if !strings.Contains(setup, "TINCAN_CONFIG") || !strings.Contains(setup, "~/.config/tincan/"+name+".json") {
			t.Errorf("%s MCP setup should set env TINCAN_CONFIG:\n%s", kind, setup)
		}
		if kind == KindCodex && !strings.Contains(setup, prefix+"tincan listen --exec") {
			t.Errorf("codex listener lacks the per-agent config:\n%s", setup)
		}
		if strings.Contains(txt, "If another agent already runs on this machine") {
			t.Errorf("%s already prefixes; it should not get the hint line", kind)
		}
		r := strings.Join(recipe(t, k, kind).Steps, "\n")
		if !strings.Contains(r, "TINCAN_CONFIG=~/.config/tincan/<name>.json tincan join") {
			t.Errorf("%s recipe join lacks the per-agent config:\n%s", kind, r)
		}
	}
}

func TestClaudeCodeAndGenericGetConfigHint(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{
		{Name: "cc", Kind: KindClaudeCode}, {Name: "zed", Kind: KindGeneric, Wake: "command"},
	}})
	for _, n := range []string{"cc", "zed"} {
		a := block(t, k, n)
		want := "If another agent already runs on this machine, prefix every tincan command (join, mcp, listen, rejoin) with TINCAN_CONFIG=~/.config/tincan/" + n + ".json."
		if !strings.Contains(strings.Join(a.Setup, "\n"), want) {
			t.Errorf("%s setup lacks the config hint:\n%s", n, strings.Join(a.Setup, "\n"))
		}
		if strings.HasPrefix(a.Join, "TINCAN_CONFIG") || strings.Contains(a.Instructions, "--name") {
			t.Errorf("%s should keep its plain join and rejoin: %q\n%s", n, a.Join, a.Instructions)
		}
	}
}

func TestOtherKindsUnchangedByConfigRules(t *testing.T) {
	kinds := []string{KindVMWebhook, KindE2BEmail, KindProxySandbox, KindChatGPT}
	var roster []Member
	for _, kind := range kinds {
		roster = append(roster, Member{Name: "a-" + kind, Kind: kind})
	}
	k := build(t, Options{RelayURL: relayURL, Roster: roster})
	for _, kind := range kinds {
		txt := blockText(block(t, k, "a-"+kind))
		if strings.Contains(txt, "TINCAN_CONFIG") || strings.Contains(txt, " --name ") {
			t.Errorf("%s block should not carry per-agent config rules:\n%s", kind, txt)
		}
	}
}

func TestExpectOnline(t *testing.T) {
	cases := []struct {
		kind, wake string
		want       bool
	}{
		{KindVMWebhook, "webhook", true},
		{KindProxySandbox, "wait", true},
		{KindHermes, "webhook", true},
		{KindOpenClaw, "webhook", true},
		{KindOpenClaw, "none", true}, // kind wins over wake
		{KindGeneric, "webhook", true},
		{KindGeneric, "wait", true},
		{KindGeneric, "email", false},
		{KindGeneric, "channel", false},
		{KindGeneric, "command", false},
		{KindGeneric, "none", false},
		{KindGeneric, "", false},
		{KindE2BEmail, "email", false},
		{KindClaudeCode, "channel", false},
		{KindChatGPT, "none", false},
		{KindCodex, "command", false},
		{KindCodex, "webhook", false}, // non-generic kinds ignore wake
		{"", "webhook", false},
	}
	for _, c := range cases {
		if got := expectOnline(c.kind, c.wake); got != c.want {
			t.Errorf("expectOnline(%q, %q) = %v, want %v", c.kind, c.wake, got, c.want)
		}
	}
}

func TestOperatorPromptOnlineMarkers(t *testing.T) {
	roster := []Member{
		{Name: "vm", Kind: KindVMWebhook}, {Name: "sb", Kind: KindE2BEmail}, {Name: "px", Kind: KindProxySandbox},
		{Name: "cc", Kind: KindClaudeCode}, {Name: "gpt", Kind: KindChatGPT}, {Name: "her", Kind: KindHermes},
		{Name: "claw", Kind: KindOpenClaw}, {Name: "cx", Kind: KindCodex},
		{Name: "gw", Wake: "webhook"}, {Name: "gn", Wake: "none"},
	}
	online := map[string]bool{"vm": true, "px": true, "her": true, "claw": true, "gw": true}
	k := build(t, Options{RelayURL: relayURL, Roster: roster})
	for _, m := range roster {
		marker := " (may sleep or be off)"
		if online[m.Name] {
			marker = " (expected online)"
		}
		a := block(t, k, m.Name)
		line := m.Name + " kind=" + a.Kind + " wake=" + a.Wake + marker + "\n"
		if !strings.Contains(k.Operator, line) {
			t.Errorf("operator prompt lacks team line %q:\n%s", line, k.Operator)
		}
	}
}
