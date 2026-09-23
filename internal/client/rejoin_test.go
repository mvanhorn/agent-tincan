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
	if got := client.RejoinHint(errors.New("no relay configured"), ""); !strings.Contains(got.Error(), "tincan rejoin --relay <relay url>") {
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
