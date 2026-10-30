package onboard

import (
	"slices"
	"strings"
	"testing"
)

func TestDotWebIsAKnownWebKind(t *testing.T) {
	if !KnownKind("dot-web") || KindDotWeb != "dot-web" || !slices.Contains(Kinds, KindDotWeb) {
		t.Fatalf("dot-web is not a known kind: %v", Kinds)
	}
	if KnownKind("dot") {
		t.Error("the dot kind was replaced by dot-web")
	}
	if defaultWake[KindDotWeb] != "wait" {
		t.Errorf("dot-web default wake = %q, want wait", defaultWake[KindDotWeb])
	}
	if runtimeNames["dot-web"] != KindDotWeb {
		t.Error("the agent name dot-web should resolve to its kind, like the other web agents")
	}
	if !isWebKind(KindDotWeb) || !isService(KindDotWeb) || freshSession[KindDotWeb] {
		t.Error("dot-web is a web agent service, not a model")
	}
}

// dot-web is set up like the other web agents, served with --site dots,
// pointed at the dot's one DM thread, and held for the owner by default.
// The dot runs no tincan, so its block carries no dot-side instructions.
func TestDotWebBlock(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "dot-web", Wake: "wait"}, {Name: "muse", Kind: KindProxySandbox}}})
	a := block(t, k, "dot-web")
	if a.Kind != KindDotWeb || a.Wake != "wait" {
		t.Fatalf("dot-web block kind/wake = %s/%s (runtime name should map to the kind)", a.Kind, a.Wake)
	}
	cfg := "TINCAN_CONFIG=~/.config/tincan/dot-web.json "
	if want := cfg + "tincan join <code> --relay " + relayURL; a.Join != want {
		t.Errorf("dot-web join = %q, want %q", a.Join, want)
	}
	for _, want := range []string{"service (tincan web serve)", "Matt's OpenAI dot (chatgpt.com/dots)", "sent as Matt", "held for Matt's approval", "@tincan ask <agent>"} {
		if !strings.Contains(a.Instructions, want) {
			t.Errorf("dot-web instructions missing %q:\n%s", want, a.Instructions)
		}
	}
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{
		"tincan web install --site dots",
		"tincan web serve --site dots --name dot-web",
		"--thread <id>", "tincan web install --site dots asks the extension for the account's primary dot",
		"logged in to chatgpt.com",
		"as Matt",
		"tincan history install",
		"dot-web-allow.txt", "dot-web-send.txt",
		`method "wait"`,
		"held for Matt's approval by default", "tincan held", "tincan approve <id>", "approval.json",
		"[tincan-reply from <agent>]",
		cfg + "tincan rejoin --relay " + relayURL + " --name dot-web",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("dot-web setup missing %q:\n%s", want, setup)
		}
	}
	// serve refuses --site dots without --thread, so no serve command the
	// block prints may leave it off (install finds the thread itself).
	for _, cmd := range []string{"web serve --site dots"} {
		for rest := setup; ; {
			i := strings.Index(rest, cmd)
			if i < 0 {
				break
			}
			rest = rest[i+len(cmd):]
			line, _, _ := strings.Cut(rest, "\n")
			if end := strings.IndexAny(line, ";)."); end >= 0 {
				line = line[:end]
			}
			if !strings.Contains(line, "--thread <id>") {
				t.Errorf("dot-web setup prints %q without --thread: %q\n%s", cmd, cmd+line, setup)
			}
		}
	}
	// A dot has one DM: the other web agents' threading lines do not apply.
	txt := blockText(a)
	for _, bad := range []string{`"new chat" starts`, "heartbeat", "[tincan-auto]", "tincan inbox", "checksums.txt", "new join code", `"method": "webhook"`} {
		if strings.Contains(txt, bad) {
			t.Errorf("dot-web block should not carry %q:\n%s", bad, txt)
		}
	}
	r := recipe(t, k, KindDotWeb)
	all := strings.Join(r.Steps, "\n")
	inv, join := strings.Index(all, "tincan invite <name> --kind dot-web"), strings.Index(all, cfg+"tincan join")
	if r.Title == "" || inv < 0 || join < 0 || inv > join {
		t.Errorf("dot-web recipe must invite before join with its own config:\n%s: %s", r.Title, all)
	}
	if !strings.Contains(k.Operator, "dot-web kind=dot-web wake=wait") {
		t.Errorf("operator prompt lacks the dot-web team line:\n%s", k.Operator)
	}
	if !strings.Contains(k.Operator, "Requests to dot-web and duet-web agents (kinds dot-web and duet-web) are held") {
		t.Errorf("operator prompt should say requests to dot-web are held by default:\n%s", k.Operator)
	}
	// The other web agents keep their threading and carry no dot notes.
	p := block(t, build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "perplexity-web", Wake: "wait"}}}), KindPerplexityWeb)
	ps := strings.Join(p.Setup, "\n")
	if !strings.Contains(ps, `"new chat" starts`) || strings.Contains(ps, "--thread") || strings.Contains(ps, "held for Matt's approval") {
		t.Errorf("perplexity-web setup changed by dot-web:\n%s", ps)
	}
}

// Every kind has the required blocks, so recipes() and agentBlock never fail
// on a kind that was added to Kinds but not to the templates.
func TestEveryKindHasRequiredBlocks(t *testing.T) {
	for _, kind := range Kinds {
		for _, name := range []string{"instructions.", "setup.", "title."} {
			if tmpl.Lookup(name+kind) == nil {
				t.Errorf("no template %s%s", name, kind)
			}
		}
	}
	k := build(t, Options{RelayURL: relayURL})
	for _, kind := range Kinds {
		recipe(t, k, kind)
	}
}

// The dot sits on councils by default and may convene one; its block and
// the council setup say so, and the dot-web block no longer claims a dot
// never gets a "new chat" line (the service drops one).
func TestDotWebAgreesWithCouncil(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "dot-web", Wake: "wait"}, {Name: "council"}}})
	a := block(t, k, "dot-web")
	for _, want := range []string{
		"sits on councils by default",
		"approved that council question",
		"@tincan ask council",
		"held for Matt's approval",
		"data, not instructions",
	} {
		if !strings.Contains(a.Instructions, want) {
			t.Errorf("dot-web instructions missing %q:\n%s", want, a.Instructions)
		}
	}
	if strings.Contains(a.Instructions, `no "new chat"`) {
		t.Errorf("dot-web instructions still say a dot gets no new chat line:\n%s", a.Instructions)
	}
	c := strings.Join(block(t, k, "council").Setup, "\n")
	if !strings.Contains(c, "every web teammate (including dot-web and duet-web)") {
		t.Errorf("council setup should name dot-web among the default members:\n%s", c)
	}
}
