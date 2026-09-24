package onboard

import (
	"strings"
	"testing"
)

// Every agent heals itself after a rebuild instead of asking the owner for
// an invite. Tailnet agents run tincan rejoin; the proxy sandbox adds its
// proxy; ChatGPT, which is not a tailnet machine, is told it cannot; the
// history service, which is not a model, carries the rejoin in its setup.
func TestEveryAgentGetsTheSelfHealLine(t *testing.T) {
	var roster []Member
	for _, kind := range Kinds {
		roster = append(roster, Member{Name: "a-" + kind, Kind: kind})
	}
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: roster})
	for _, kind := range Kinds {
		txt := block(t, k, "a-"+kind).Instructions
		if kind == KindChatGPT {
			if !strings.Contains(txt, "not joined") || !strings.Contains(txt, "tincan connect chatgpt") {
				t.Errorf("chatgpt block lacks its not-joined line:\n%s", txt)
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
