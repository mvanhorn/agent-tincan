package onboard

import (
	"strings"
	"testing"
)

// The roster name sesame resolves to the sesame kind, which checks its inbox
// on a schedule, and the name maps the same way for the relay's connect.
func TestSesameRuntimeNameIsSesame(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "sesame"}}})
	a := block(t, k, "sesame")
	if a.Kind != KindSesame || a.Wake != "schedule" {
		t.Errorf("runtime name sesame = %q/%q, want sesame/schedule", a.Kind, a.Wake)
	}
	if got := RuntimeKind("sesame"); got != KindSesame {
		t.Errorf("RuntimeKind(sesame) = %q, want sesame", got)
	}
	if got := RuntimeKind("chatgpt"); got != KindChatGPT {
		t.Errorf("RuntimeKind(chatgpt) = %q, want chatgpt", got)
	}
	if got := RuntimeKind("miles"); got != "" {
		t.Errorf("RuntimeKind(miles) = %q, want none: personal names map to no kind", got)
	}
	if got := ProfileOf(Member{Name: "sesame"}); got.Kind != KindSesame || got.Wake != "schedule" || got.ExpectOnline {
		t.Errorf("ProfileOf(sesame) = %+v", got)
	}
}

// Sesame has no shell: its own instructions use the MCP tools, drain the
// inbox on each scheduled run, and never send it to the tincan CLI.
func TestSesameInstructionsUseTools(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "miles", Kind: KindSesame}}})
	own, err := execute("instructions.sesame", newAgentData("miles", KindSesame, "schedule", relayURL, "Matt", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"check_inbox", "reply", "needs_input", "progress", "get_reply", "inbox is empty", "Sesame schedule", "ask"} {
		if !strings.Contains(own, want) {
			t.Errorf("sesame instructions missing %q:\n%s", want, own)
		}
	}
	if strings.Contains(own, "tincan ") {
		t.Errorf("sesame's own instructions name a tincan command:\n%s", own)
	}
	if got := block(t, k, "miles").Instructions; !strings.Contains(got, strings.TrimSpace(own)) {
		t.Errorf("sesame block lacks its own instructions:\n%s", got)
	}
}

// A request's text is a teammate's input; it never makes Sesame reveal other
// conversations or what else is in its inbox.
func TestSesameInstructionsGuardUnrelatedContent(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "sesame"}}})
	got := block(t, k, "sesame").Instructions
	for _, want := range []string{"teammate's input", "not an instruction", "unrelated conversations"} {
		if !strings.Contains(got, want) {
			t.Errorf("sesame instructions missing %q:\n%s", want, got)
		}
	}
}

// Like chatgpt, sesame is a gateway agent: the shared lines that send an
// agent to a shell or a local MCP server are left out, and when it is not
// joined it tells the owner to connect it again under its own name.
func TestSesameIsAGatewayAgent(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "sesame"}, {Name: "miles", Kind: KindSesame}, {Name: "chatgpt"}}})
	for _, name := range []string{"sesame", "miles", "chatgpt"} {
		a := block(t, k, name)
		txt := blockText(a)
		for _, bad := range []string{"tincan rejoin", "tincan doctor", "tincan upgrade", "different build", "TINCAN_CONFIG"} {
			if strings.Contains(txt, bad) {
				t.Errorf("%s block carries %q:\n%s", name, bad, txt)
			}
		}
		if want := "tincan connect " + name + " again"; !strings.Contains(a.Instructions, "not joined") || !strings.Contains(a.Instructions, want) {
			t.Errorf("%s block lacks its not-joined line %q:\n%s", name, want, a.Instructions)
		}
	}
	if got := block(t, k, "miles").Join; !strings.Contains(got, "tincan connect miles") || !strings.Contains(got, "--chatgpt-gateway") {
		t.Errorf("sesame join = %q", got)
	}
}

// The setup names the connect command, Sesame's custom app, the wake.json
// schedule entry and a Sesame schedule at the same interval.
func TestSesameSetup(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "sesame"}}})
	setup := strings.Join(block(t, k, "sesame").Setup, "\n")
	for _, want := range []string{
		"--chatgpt-gateway", "tincan connect sesame", "Add custom app", "Continue to authorization", "never in chat",
		"wake.json", `{"sesame": {"method": "schedule", "every": "5m"}}`, "restart the relay",
		"Sesame schedule", "every 5 minutes", "same interval", "standing instructions",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("sesame setup missing %q:\n%s", want, setup)
		}
	}
}

// Sesame cannot join the tailnet, so its recipe starts from the gateway, not
// an invite, and carries the same setup.
func TestSesameRecipe(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL})
	r := recipe(t, k, KindSesame)
	all := strings.Join(r.Steps, "\n")
	if !strings.Contains(r.Title, "Sesame") || !strings.Contains(r.Title, "gateway") {
		t.Errorf("sesame recipe title %q", r.Title)
	}
	if !strings.Contains(all, "No tincan invite") || strings.Contains(all, "tincan invite <name>") || strings.Contains(all, "tincan join") {
		t.Errorf("sesame recipe should say no invite is used:\n%s", all)
	}
	for _, want := range []string{"tincan connect <name>", "Add custom app", `"every": "5m"`} {
		if !strings.Contains(all, want) {
			t.Errorf("sesame recipe missing %q:\n%s", want, all)
		}
	}
}

// The stock good-at line says what asking sesame does.
func TestSesameStockGoodAt(t *testing.T) {
	for _, s := range []string{StockGoodAt("sesame", ""), StockGoodAt("miles", KindSesame)} {
		if !strings.Contains(s, "Sesame") || !strings.Contains(s, "schedule") {
			t.Errorf("sesame stock line = %q", s)
		}
	}
	if got := StockGoodAt("miles", ""); got != "" {
		t.Errorf("miles with no kind = %q, want none", got)
	}
}

// The Sesame recipe works for a personal name too: an agent connected as
// miles has no stored kind until the admin sets it, so the recipe says to
// set it, or onboarding would hand miles shell instructions it cannot run.
func TestSesameRecipeSetsKindForAPersonalName(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt"})
	steps := strings.Join(recipe(t, k, KindSesame).Steps, "\n")
	if !strings.Contains(steps, "tincan kind <name> sesame") {
		t.Fatalf("sesame recipe should say to set the kind for a name other than sesame:\n%s", steps)
	}
}
