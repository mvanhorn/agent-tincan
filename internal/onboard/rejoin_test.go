package onboard

import (
	"strings"
	"testing"
)

// Every agent heals itself after a rebuild instead of asking the owner for
// an invite. Tailnet agents run tincan rejoin; the proxy sandbox adds its
// proxy; the gateway agents (ChatGPT, Sesame), which are not tailnet
// machines, are told they cannot and to have the owner connect them again
// under their own name; the history and web agent services, which are not
// models, carry the rejoin in their setup.
func TestEveryAgentGetsTheSelfHealLine(t *testing.T) {
	var roster []Member
	for _, kind := range Kinds {
		roster = append(roster, Member{Name: "a-" + kind, Kind: kind})
	}
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: roster})
	for _, kind := range Kinds {
		txt := block(t, k, "a-"+kind).Instructions
		if kind == KindChatGPT || kind == KindSesame {
			if !strings.Contains(txt, "not joined") || !strings.Contains(txt, "tincan connect a-"+kind+" again") || strings.Contains(txt, "tincan rejoin") {
				t.Errorf("%s block lacks its not-joined line:\n%s", kind, txt)
			}
			continue
		}
		if kind == KindHistory {
			// A service, not a model: the owner rejoins it from its setup steps.
			setup := strings.Join(block(t, k, "a-"+kind).Setup, "\n")
			if want := "TINCAN_CONFIG=~/.config/tincan/history.json tincan rejoin --relay " + relayURL + " --name a-history"; !strings.Contains(setup, "not joined") || !strings.Contains(setup, want) {
				t.Errorf("history setup lacks its rejoin line %q:\n%s", want, setup)
			}
			continue
		}
		if kind == KindNotes || kind == KindCouncil || kind == KindChatGPTWeb || kind == KindClaudeWeb || kind == KindGrokWeb || kind == KindGeminiWeb || kind == KindPerplexityWeb || kind == KindCopilotWeb || kind == KindDotWeb {
			// Services too, with the fixed config path their service sets.
			setup := strings.Join(block(t, k, "a-"+kind).Setup, "\n")
			if want := "TINCAN_CONFIG=~/.config/tincan/" + kind + ".json tincan rejoin --relay " + relayURL + " --name a-" + kind; !strings.Contains(setup, "not joined") || !strings.Contains(setup, want) {
				t.Errorf("%s setup lacks its rejoin line %q:\n%s", kind, want, setup)
			}
			continue
		}
		want := "tincan rejoin --relay " + relayURL
		if kind == KindProxySandbox {
			want += " --proxy "
		}
		for _, s := range []string{"not joined", "no relay configured", want, "yourself", "Never ask Matt for an invite unless", "never joined"} {
			if !strings.Contains(txt, s) {
				t.Errorf("%s block missing %q:\n%s", kind, s, txt)
			}
		}
	}
}

func TestSandboxRecipesKeepMachineName(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: []Member{{Name: "instinct", Kind: KindE2BEmail}, {Name: "muse", Kind: KindProxySandbox}}})
	for _, kind := range []string{KindE2BEmail, KindProxySandbox} {
		if all := strings.Join(recipe(t, k, kind).Steps, "\n"); !strings.Contains(all, "same machine name") {
			t.Errorf("%s recipe should say to keep the same machine name across rebuilds:\n%s", kind, all)
		}
	}
	if !strings.Contains(strings.Join(block(t, k, "instinct").Setup, "\n"), "same machine name") {
		t.Error("e2b-email setup should say to keep the same machine name")
	}
}

func TestOperatorKnowsAboutRebinds(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: matts()})
	p := k.Operator
	for _, want := range []string{"re-admitted automatically", "rebind", "never joined"} {
		if !strings.Contains(p, want) {
			t.Errorf("operator prompt missing %q", want)
		}
	}
	if strings.Contains(p, `"not a joined agent" means that machine needs an invite`) {
		t.Error("operator prompt still sends every not-joined machine to the owner")
	}
}

// TestEmailAgentActsOnRequestEmails pins the e2b-email rules that let an
// email-woken agent act on a request email when its tailnet path is down:
// the email is work, trust comes only from the relay's sending address and
// the [tincan tag, an answer counts only once "recorded" comes back, and the
// backup check is a scheduled task. The owner's setup names include_requests.
func TestEmailAgentActsOnRequestEmails(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Roster: []Member{{Name: "instinct", Kind: KindE2BEmail}}})
	b := block(t, k, "instinct")
	instr := b.Instructions
	for _, want := range []string{"is work", "[tincan ", "sending address", "untrusted", "reply to that email", `"recorded"`, "scheduled task", "failed:", "declined:"} {
		if !strings.Contains(instr, want) {
			t.Errorf("e2b-email instructions missing %q:\n%s", want, instr)
		}
	}
	setup := strings.Join(b.Setup, "\n")
	for _, want := range []string{"include_requests", "sending address", "email-tag-key", "dedicated"} {
		if !strings.Contains(setup, want) {
			t.Errorf("e2b-email setup missing %q:\n%s", want, setup)
		}
	}
	if !strings.Contains(setup, "file only") {
		t.Errorf("e2b-email setup should still keep secret fields in the file only:\n%s", setup)
	}
}
