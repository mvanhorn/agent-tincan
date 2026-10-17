package onboard

import (
	"slices"
	"strings"
	"testing"
)

// The phone is a kind of its own: no wake, a stock line saying it only
// asks, and a block and recipe that send the owner to the phone page
// instead of a model's standing instructions.
func TestPhoneKind(t *testing.T) {
	if !KnownKind(KindPhone) || KindPhone != "phone" || !slices.Contains(Kinds, KindPhone) {
		t.Fatalf("phone is not a known kind: %v", Kinds)
	}
	if defaultWake[KindPhone] != "none" {
		t.Fatalf("phone wake = %q, want none", defaultWake[KindPhone])
	}
	if got := StockGoodAt("myphone", KindPhone); got != "the owner's phone; asks only, never answers" {
		t.Fatalf("phone stock line = %q", got)
	}
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "phone", Kind: KindPhone}}})
	b := block(t, k, "phone")
	if b.Wake != "none" || !strings.Contains(b.Join, "Join this phone") || strings.Contains(b.Join, "tincan join") {
		t.Errorf("phone block join/wake: %+v", b)
	}
	if !strings.Contains(b.Instructions, "never answers") || strings.Contains(b.Instructions, "check_inbox") {
		t.Errorf("phone block instructions should say it never answers, with no model instructions:\n%s", b.Instructions)
	}
	setup := strings.Join(b.Setup, "\n")
	for _, want := range []string{"--admin", "/phone/", "Join this phone"} {
		if !strings.Contains(setup, want) {
			t.Errorf("phone setup lacks %q:\n%s", want, setup)
		}
	}
	r := strings.Join(recipe(t, k, KindPhone).Steps, "\n")
	if !strings.Contains(r, "Join this phone") || strings.Contains(r, "tincan invite <name>") {
		t.Errorf("phone recipe:\n%s", r)
	}
}
