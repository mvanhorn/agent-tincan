package client_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestWhoAmI(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	me, err := m.Client(t, "muse").WhoAmI(context.Background())
	if err != nil || me.Name != "muse" {
		t.Fatalf("whoami = %+v, %v", me, err)
	}
	_, err = m.Client(t, "stranger").WhoAmI(context.Background())
	if !client.IsStatus(err, http.StatusForbidden) || !client.IsNotJoined(err) {
		t.Fatalf("stranger whoami: want 403 not joined, got %v", err)
	}
}

// A rebuilt machine is re-admitted on its first call and keeps its name.
func TestWhoAmIAfterRebuild(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	url := m.Rebuild(t, "instinct", "instinct")
	r, err := client.NewRelay(url, "")
	if err != nil {
		t.Fatal(err)
	}
	if me, err := r.WhoAmI(context.Background()); err != nil || me.Name != "instinct" {
		t.Fatalf("whoami after rebuild = %+v, %v", me, err)
	}
}

func TestRejoinHint(t *testing.T) {
	notJoined := &client.APIError{Code: 403, Message: "instinct: not a joined agent"}
	err := client.RejoinHint(notJoined, "http://tincan-relay")
	if !strings.Contains(err.Error(), "tincan rejoin --relay http://tincan-relay") {
		t.Fatalf("hint = %v", err)
	}
	if !client.IsStatus(err, 403) {
		t.Fatal("hinted error must still unwrap to the relay error")
	}
	// The two branches say different things: only the no-relay hint
	// speaks to an admin device that never joined, and only the
	// not-joined hint says a never-joined machine needs an invite.
	const adminLine = "An admin device that never joined does not need this"
	const inviteLine = "Only a machine that was never joined needs an invite from an admin"
	if msg := err.Error(); strings.Contains(msg, adminLine) || !strings.Contains(msg, inviteLine) {
		t.Fatalf("not-joined hint = %v", err)
	}
	got := client.RejoinHint(errors.New("no relay configured"), "")
	if msg := got.Error(); !strings.Contains(msg, "tincan rejoin --relay <relay url>") || !strings.Contains(msg, adminLine) || strings.Contains(msg, inviteLine) {
		t.Fatalf("hint without relay = %v", got)
	}
	other := errors.New("no such agent: bob")
	if client.RejoinHint(other, "") != other {
		t.Fatal("unrelated errors pass through unchanged")
	}
	if client.RejoinHint(nil, "") != nil {
		t.Fatal("nil stays nil")
	}
}
