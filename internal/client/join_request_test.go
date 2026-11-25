package client_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// adminSocketRelay is a client of the mesh's admin socket handler, which
// treats every caller as the relay host's local admin.
func adminSocketRelay(t *testing.T, m *testrelay.Mesh) *client.Relay {
	t.Helper()
	srv := httptest.NewServer(m.Server.AdminHandler())
	t.Cleanup(srv.Close)
	r, err := client.NewRelay(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The requester files, reads its status, and after the owner approves over
// the socket claims its agents once; the listing shows what was asked for.
func TestJoinRequestRoundTrip(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	box, err := client.NewRelay(m.AddMachine(t, "box", testrelay.TailnetNode("nBOX", "box", testrelay.Login)), "")
	if err != nil {
		t.Fatal(err)
	}
	filed, err := box.FileJoinRequest(t.Context(), []client.JoinRequestAgent{{Name: "codex", Kind: "codex"}, {Name: "helper"}})
	if err != nil || filed.Status != client.JoinPending || filed.ID == "" || filed.Code == "" || filed.ExpiresAt.IsZero() {
		t.Fatalf("file: %+v, %v", filed, err)
	}
	st, err := box.JoinRequestStatus(t.Context(), filed.ID, 0)
	if err != nil || st.Status != client.JoinPending || st.Code != filed.Code || len(st.Agents) != 2 {
		t.Fatalf("status: %+v, %v", st, err)
	}

	owner := adminSocketRelay(t, m)
	list, err := owner.JoinRequests(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("listing: %+v, %v", list, err)
	}
	l := list[0]
	if l.ID != filed.ID || l.Code != filed.Code || l.NodeName != "box" || l.NodeUser != testrelay.Login || len(l.Agents) != 2 || l.Agents[0].Kind != "codex" {
		t.Fatalf("listing entry %+v", l)
	}

	// A status read with wait returns once the owner approves.
	done := make(chan client.JoinRequestState, 1)
	go func() {
		st, _ := box.JoinRequestStatus(t.Context(), filed.ID, 20*time.Second)
		done <- st
	}()
	time.Sleep(50 * time.Millisecond)
	if d, err := owner.DecideJoinRequest(t.Context(), filed.ID, true); err != nil || d.Status != client.JoinApproved {
		t.Fatalf("approve: %+v, %v", d, err)
	}
	select {
	case st := <-done:
		if st.Status != client.JoinApproved {
			t.Fatalf("waited status %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting status did not return after approval")
	}

	got, err := box.ClaimJoinRequest(t.Context(), filed.ID)
	if err != nil || got.Status != client.JoinDelivered || len(got.Agents) != 2 {
		t.Fatalf("claim: %+v, %v", got, err)
	}
	again, err := box.ClaimJoinRequest(t.Context(), filed.ID)
	if err != nil || again.Status != client.JoinDelivered || len(again.Agents) != 0 {
		t.Fatalf("second claim: %+v, %v", again, err)
	}
}

// Denying over the socket is final; a second decision is an error.
func TestJoinRequestDeny(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	box, _ := client.NewRelay(m.URL("stranger"), "")
	filed, err := box.FileJoinRequest(t.Context(), []client.JoinRequestAgent{{Name: "codex", Kind: "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	owner := adminSocketRelay(t, m)
	if d, err := owner.DecideJoinRequest(t.Context(), filed.ID, false); err != nil || d.Status != client.JoinDenied {
		t.Fatalf("deny: %+v, %v", d, err)
	}
	if _, err := owner.DecideJoinRequest(t.Context(), filed.ID, true); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("approve after deny: %v, want 409", err)
	}
	if st, err := box.JoinRequestStatus(t.Context(), filed.ID, 0); err != nil || st.Status != client.JoinDenied {
		t.Fatalf("status after deny: %+v, %v", st, err)
	}
}

// A claim the relay refuses because a name can no longer be joined answers
// 409 with status failed; the client returns that status, not an error.
func TestJoinRequestClaimFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"id":"j1","status":"failed","name":"codex","error":"codex: a requested name can no longer be joined: taken"}`))
	}))
	t.Cleanup(srv.Close)
	r, _ := client.NewRelay(srv.URL, "")
	st, err := r.ClaimJoinRequest(t.Context(), "j1")
	if err != nil || st.Status != client.JoinFailed || st.Name != "codex" || st.Error == "" {
		t.Fatalf("claim: %+v, %v", st, err)
	}
}

// Only an older relay's plain-text 404 means the routes are missing; the
// relay's JSON 404 for an unknown id does not.
func TestIsOlderRelay(t *testing.T) {
	old := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(old.Close)
	r, _ := client.NewRelay(old.URL, "")
	_, err := r.FileJoinRequest(t.Context(), []client.JoinRequestAgent{{Name: "codex"}})
	if !client.IsOlderRelay(err) {
		t.Fatalf("older relay: %v", err)
	}
	m := testrelay.New(t, relay.Config{})
	box, _ := client.NewRelay(m.URL("stranger"), "")
	_, err = box.JoinRequestStatus(t.Context(), "nope", 0)
	if !client.IsStatus(err, http.StatusNotFound) || client.IsOlderRelay(err) {
		t.Fatalf("unknown id: %v", err)
	}
}

// The pending-request file is written 0600 in a 0700 folder, reads back
// as written, and is gone after removal.
func TestPendingJoinRequestFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tincan")
	path := filepath.Join(dir, "join-request.json")
	if _, found, err := client.LoadPendingJoinRequest(path); found || err != nil {
		t.Fatalf("missing file: found %v, %v", found, err)
	}
	p := client.PendingJoinRequest{Relay: "http://tincan-relay", ID: "j1", Code: "ABC-DEF",
		Agents: []client.JoinRequestAgent{{Name: "codex", Kind: "codex"}}, ExpiresAt: time.Now().Add(time.Minute).UTC().Truncate(time.Second)}
	if err := client.SavePendingJoinRequest(path, p); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %v", fi.Mode().Perm())
		}
	}
	got, found, err := client.LoadPendingJoinRequest(path)
	if err != nil || !found || got.ID != "j1" || got.Relay != p.Relay || got.Code != p.Code || len(got.Agents) != 1 || !got.ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatalf("load: %+v %v %v", got, found, err)
	}
	if err := client.RemovePendingJoinRequest(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still there: %v", err)
	}
	if err := client.RemovePendingJoinRequest(path); err != nil {
		t.Fatalf("removing a missing file: %v", err)
	}
	t.Setenv("TINCAN_CONFIG", filepath.Join(dir, "agent.json"))
	if got := client.JoinRequestPath(); got != path {
		t.Fatalf("JoinRequestPath = %s, want %s", got, path)
	}
}
