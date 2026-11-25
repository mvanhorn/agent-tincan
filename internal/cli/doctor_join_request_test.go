package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// fileForDoctor files a join request from the box for codex and helper
// (kind hermes) and saves the pending file as join --request does.
func fileForDoctor(t *testing.T, m *testrelay.Mesh) (box string, r *client.Relay, p client.PendingJoinRequest) {
	t.Helper()
	box = newBox(t, m)
	r, _ = client.NewRelay(box, "")
	agents := []client.JoinRequestAgent{{Name: "codex", Kind: "codex"}, {Name: "helper", Kind: "hermes"}}
	st, err := r.FileJoinRequest(t.Context(), agents)
	if err != nil {
		t.Fatal(err)
	}
	p = client.PendingJoinRequest{Relay: box, ID: st.ID, Code: st.Code, Agents: agents, FiledAt: st.FiledAt, ExpiresAt: st.ExpiresAt}
	if err := client.SavePendingJoinRequest(client.JoinRequestPath(), p); err != nil {
		t.Fatal(err)
	}
	return box, r, p
}

// doctorJoin runs the join request check and fails unless it is a warn
// whose detail and fix hold the wanted text.
func doctorJoin(t *testing.T, detail []string, fix []string) check {
	t.Helper()
	c, ok := joinRequestCheck(t.Context())
	if !ok {
		t.Fatal("no join request check with a pending file present")
	}
	if c.Name != "join request" || c.Status != "warn" {
		t.Fatalf("check %+v, want a warn named join request", c)
	}
	for _, want := range detail {
		if !strings.Contains(c.Detail, want) {
			t.Fatalf("detail lacks %q: %+v", want, c)
		}
	}
	for _, want := range fix {
		if !strings.Contains(c.Fix, want) {
			t.Fatalf("fix lacks %q: %+v", want, c)
		}
	}
	return c
}

// Covers R14, KTD12. Pending warns with the code and leaves the request
// pending; approved says to rerun the command, spelled out, and does not
// claim; delivered with the pending file still present says the configs
// were not saved and how to finish.
func TestDoctorJoinRequestOpen(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	box, r, p := fileForDoctor(t, m)
	cmdLine := "tincan join --request codex,helper:hermes --relay " + box

	doctorJoin(t, []string{"pending", p.Code, p.ID}, []string{cmdLine})
	if st, _ := r.JoinRequestStatus(t.Context(), p.ID, 0); st.Status != client.JoinPending {
		t.Fatalf("doctor changed the request: %+v", st)
	}

	decide(t, ownerRelay(t, m), p.ID, true)
	doctorJoin(t, []string{"approved"}, []string{cmdLine})
	if st, _ := r.JoinRequestStatus(t.Context(), p.ID, 0); st.Status != client.JoinApproved {
		t.Fatalf("doctor claimed the request: %+v", st)
	}

	if _, err := r.ClaimJoinRequest(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	doctorJoin(t, []string{"delivered", "not saved"}, []string{cmdLine})
}

// Covers R14, KTD12. Denied, expired, voided and an unknown id each warn
// with what happened and a next step naming the relay; none of them fails.
func TestDoctorJoinRequestFinal(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		m := testrelay.New(t, relay.Config{})
		useConfig(t, client.Config{})
		box, _, p := fileForDoctor(t, m)
		decide(t, ownerRelay(t, m), p.ID, false)
		doctorJoin(t, []string{"denied"}, []string{client.JoinRequestPath(), box})
	})
	t.Run("expired", func(t *testing.T) {
		clk := &testClock{t: time.Now()}
		m := testrelay.New(t, relay.Config{Now: clk.Now})
		useConfig(t, client.Config{})
		box, _, _ := fileForDoctor(t, m)
		clk.Add(identity.JoinRequestTTL + time.Second)
		m.Server.Sweep(t.Context())
		doctorJoin(t, []string{"expired"}, []string{client.JoinRequestPath(), "tincan join --request codex,helper:hermes --relay " + box})
	})
	t.Run("voided", func(t *testing.T) {
		m := testrelay.New(t, relay.Config{})
		useConfig(t, client.Config{})
		box, _, _ := fileForDoctor(t, m)
		n := testrelay.TailnetNode("nBOX", "box", testrelay.Login)
		n.Key = "nodekey:new"
		m.Who.Set(m.Addr("box"), n)
		doctorJoin(t, []string{"voided", "key"}, []string{client.JoinRequestPath(), "tincan join --request codex,helper:hermes --relay " + box})
	})
	t.Run("unknown id", func(t *testing.T) {
		m := testrelay.New(t, relay.Config{})
		useConfig(t, client.Config{})
		box := newBox(t, m)
		p := client.PendingJoinRequest{Relay: box, ID: "gone", Code: "ABC-DEF", Agents: []client.JoinRequestAgent{{Name: "codex", Kind: "codex"}}}
		if err := client.SavePendingJoinRequest(client.JoinRequestPath(), p); err != nil {
			t.Fatal(err)
		}
		doctorJoin(t, []string{"does not know", "gone"}, []string{client.JoinRequestPath(), "tincan join --request codex --relay " + box})
	})
}

// Covers R14. A failed request (a name refused at collection) warns with a
// next step.
func TestDoctorJoinRequestFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"j1","status":"failed"}`))
	}))
	t.Cleanup(srv.Close)
	useConfig(t, client.Config{})
	p := client.PendingJoinRequest{Relay: srv.URL, ID: "j1", Agents: []client.JoinRequestAgent{{Name: "scout"}}}
	if err := client.SavePendingJoinRequest(client.JoinRequestPath(), p); err != nil {
		t.Fatal(err)
	}
	doctorJoin(t, []string{"failed"}, []string{client.JoinRequestPath(), "tincan join --request scout --relay " + srv.URL})
}

// Covers R10, R14. An older relay gets the invite hint.
func TestDoctorJoinRequestOlderRelay(t *testing.T) {
	old := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(old.Close)
	useConfig(t, client.Config{})
	p := client.PendingJoinRequest{Relay: old.URL, ID: "j1", Agents: []client.JoinRequestAgent{{Name: "codex", Kind: "codex"}}}
	if err := client.SavePendingJoinRequest(client.JoinRequestPath(), p); err != nil {
		t.Fatal(err)
	}
	doctorJoin(t, []string{"does not support join requests"}, []string{"tincan invite codex", "tincan join <code> --relay " + old.URL})
}

// Covers R14. No pending file adds no check; with one, the full doctor run
// carries the check as a warn and does not fail on it.
func TestDoctorJoinRequestOnlyWithPendingFile(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	if _, ok := joinRequestCheck(t.Context()); ok {
		t.Fatal("a check with no pending file")
	}
	for _, c := range runDoctor(t.Context(), "", nil).Checks {
		if c.Name == "join request" {
			t.Fatalf("doctor added %+v with no pending file", c)
		}
	}
	fileForDoctor(t, m)
	var found bool
	for _, c := range runDoctor(t.Context(), "", nil).Checks {
		if c.Name == "join request" {
			found = c.Status == "warn"
		}
	}
	if !found {
		t.Fatal("doctor run lacks the join request warn")
	}
}
