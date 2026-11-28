package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// ownerRelay is the relay host's view of the mesh: a client of its admin
// socket handler, which treats every caller as the local admin.
func ownerRelay(t *testing.T, m *testrelay.Mesh) *client.Relay {
	t.Helper()
	srv := httptest.NewServer(m.Server.AdminHandler())
	t.Cleanup(srv.Close)
	r, err := client.NewRelay(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// newBox puts a fresh unjoined machine named box on the mesh's tailnet and
// returns its endpoint.
func newBox(t *testing.T, m *testrelay.Mesh) string {
	t.Helper()
	return m.AddMachine(t, "box", testrelay.TailnetNode("nBOX", "box", testrelay.Login))
}

// running is a tincan command running in the background.
type running struct {
	cancel context.CancelFunc
	done   chan struct{}
	out    bytes.Buffer
	err    error
}

// startCmd runs tincan args in the background, as a terminal does while
// the user waits.
func startCmd(t *testing.T, args ...string) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan struct{})}
	cmd := Root()
	cmd.SetOut(&r.out)
	cmd.SetErr(&r.out)
	cmd.SetArgs(args)
	go func() {
		defer close(r.done)
		r.err = cmd.ExecuteContext(ctx)
	}()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

// wait waits for the command to end and returns its output and error.
func (r *running) wait(t *testing.T) (string, error) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		t.Fatal("command still running after 20s")
	}
	return r.out.String(), r.err
}

// stop interrupts the command, as Ctrl-C does, and waits for it.
func (r *running) stop(t *testing.T) (string, error) {
	t.Helper()
	r.cancel()
	return r.wait(t)
}

// pendingRequest waits until the relay lists exactly one pending request
// and the requester has saved its pending file, and returns the listing.
func pendingRequest(t *testing.T, owner *client.Relay) client.JoinRequestListing {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := owner.JoinRequests(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, found, _ := client.LoadPendingJoinRequest(client.JoinRequestPath())
		if len(list) == 1 && found {
			return list[0]
		}
		if len(list) > 1 {
			t.Fatalf("%d pending requests, want 1", len(list))
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pending request after 10s (listed %d, file %v)", len(list), found)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func decide(t *testing.T, owner *client.Relay, id string, approve bool) {
	t.Helper()
	if _, err := owner.DecideJoinRequest(t.Context(), id, approve); err != nil {
		t.Fatal(err)
	}
}

func noPendingFile(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(client.JoinRequestPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending file still at %s: %v", client.JoinRequestPath(), err)
	}
}

func mustConfig(t *testing.T, path, relayURL, agent string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMode(fi, 0o600) {
		t.Fatalf("%s mode %v, want 0600", path, fi.Mode().Perm())
	}
	cfg, err := client.LoadConfigFrom(path)
	if err != nil || cfg.Relay != relayURL || cfg.Agent != agent {
		t.Fatalf("%s = %+v, %v; want relay %s agent %s", path, cfg, err, relayURL, agent)
	}
}

func agentKind(t *testing.T, m *testrelay.Mesh, name string) string {
	t.Helper()
	a, ok, err := m.Dir.Agent(t.Context(), name)
	if err != nil || !ok {
		t.Fatalf("agent %s: found %v, %v", name, ok, err)
	}
	return a.Kind
}

// Covers R1, R9. One name: the request is filed with the name as its kind,
// the code is printed with where to compare it, the pending file is 0600 in
// a 0700 folder while it waits, and after the owner approves the config is
// written at ConfigPath() and the pending file removed.
func TestJoinRequestOneName(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	cfgPath := filepath.Join(t.TempDir(), "tincan", "client.json")
	t.Setenv("TINCAN_CONFIG", cfgPath)
	box, owner := newBox(t, m), ownerRelay(t, m)

	j := startCmd(t, "join", "--request", "codex", "--relay", box)
	l := pendingRequest(t, owner)
	if len(l.Agents) != 1 || l.Agents[0] != (client.JoinRequestAgent{Name: "codex", Kind: "codex"}) || l.NodeName != "box" {
		t.Fatalf("listing %+v", l)
	}
	for path, want := range map[string]os.FileMode{client.JoinRequestPath(): 0o600, filepath.Dir(cfgPath): 0o700} {
		if fi, err := os.Stat(path); err != nil || !hasMode(fi, want) {
			t.Fatalf("%s: %v %v, want mode %v", path, fi, err, want)
		}
	}
	p, _, _ := client.LoadPendingJoinRequest(client.JoinRequestPath())
	if p.Relay != box || p.ID != l.ID || p.Code != l.Code || len(p.Agents) != 1 || p.Agents[0].Kind != "codex" {
		t.Fatalf("pending file %+v", p)
	}
	decide(t, owner, l.ID, true)
	out, err := j.wait(t)
	if err != nil {
		t.Fatalf("join --request: %v\n%s", err, out)
	}
	for _, want := range []string{"Matching code: " + l.Code, "tincan join-requests", "phone", `Joined as "codex"`, cfgPath} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	mustConfig(t, cfgPath, box, "codex")
	noPendingFile(t)
	if k := agentKind(t, m, "codex"); k != "codex" {
		t.Fatalf("codex kind %q", k)
	}
	// The new config works: whoami as codex from the box.
	cfg, _ := client.LoadConfigFrom(cfgPath)
	r, _ := client.NewRelayForFile(cfg, cfgPath)
	if me, err := r.WhoAmI(t.Context()); err != nil || me.Name != "codex" {
		t.Fatalf("whoami with the new config: %+v %v", me, err)
	}
}

// Covers R1. Two names write <dir>/<name>.json each, print a TINCAN_CONFIG
// line for each, and keep each name's kind; a bare name that is not a
// known kind gets none.
func TestJoinRequestTwoNames(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	dir := filepath.Dir(client.ConfigPath())
	box, owner := newBox(t, m), ownerRelay(t, m)

	j := startCmd(t, "join", "--request", "codex,helper:hermes", "--request", "scout", "--relay", box)
	l := pendingRequest(t, owner)
	want := []client.JoinRequestAgent{{Name: "codex", Kind: "codex"}, {Name: "helper", Kind: "hermes"}, {Name: "scout"}}
	if len(l.Agents) != 3 || l.Agents[0] != want[0] || l.Agents[1] != want[1] || l.Agents[2] != want[2] {
		t.Fatalf("listing agents %+v", l.Agents)
	}
	decide(t, owner, l.ID, true)
	out, err := j.wait(t)
	if err != nil {
		t.Fatalf("join --request: %v\n%s", err, out)
	}
	for _, a := range want {
		path := filepath.Join(dir, a.Name+".json")
		mustConfig(t, path, box, a.Name)
		if !strings.Contains(out, "TINCAN_CONFIG="+path) {
			t.Fatalf("output lacks TINCAN_CONFIG=%s:\n%s", path, out)
		}
		if k := agentKind(t, m, a.Name); k != a.Kind {
			t.Fatalf("%s kind %q, want %q", a.Name, k, a.Kind)
		}
	}
	if _, err := os.Stat(client.ConfigPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("several names must not write %s: %v", client.ConfigPath(), err)
	}
	noPendingFile(t)
}

// Covers KTD9. A target config that already names another agent stops the
// command before anything is filed, for one name and for several.
func TestJoinRequestPreflightRefusesOtherAgent(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	box, owner := newBox(t, m), ownerRelay(t, m)
	useConfig(t, client.Config{Relay: box, Agent: "muse"})
	_, err := run(t, Root(), "join", "--request", "codex", "--relay", box)
	if err == nil || !strings.Contains(err.Error(), `already belongs to agent "muse"`) || !strings.Contains(err.Error(), "TINCAN_CONFIG=") {
		t.Fatalf("want a refusal naming muse, got %v", err)
	}
	other := filepath.Join(filepath.Dir(client.ConfigPath()), "helper.json")
	if err := client.SaveConfigTo(other, client.Config{Relay: box, Agent: "grokbot"}); err != nil {
		t.Fatal(err)
	}
	_, err = run(t, Root(), "join", "--request", "codex,helper", "--relay", box)
	if err == nil || !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), `"grokbot"`) {
		t.Fatalf("want a refusal naming %s, got %v", other, err)
	}
	if list, _ := owner.JoinRequests(t.Context()); len(list) != 0 {
		t.Fatalf("a refused preflight filed %+v", list)
	}
	noPendingFile(t)
}

// Covers KTD9. A configured proxy, saved or given, stops the command
// before anything is filed: the relay would bind the proxy's machine.
func TestJoinRequestRefusesProxy(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	box, owner := newBox(t, m), ownerRelay(t, m)
	useConfig(t, client.Config{Proxy: "http://127.0.0.1:3128"})
	if _, err := run(t, Root(), "join", "--request", "codex", "--relay", box); err == nil || !strings.Contains(err.Error(), "proxy") || !strings.Contains(err.Error(), "invite") {
		t.Fatalf("saved proxy: %v", err)
	}
	useConfig(t, client.Config{})
	if _, err := run(t, Root(), "join", "--request", "codex", "--relay", box, "--proxy", "http://127.0.0.1:3128"); err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("--proxy: %v", err)
	}
	t.Setenv("TINCAN_PROXY", "http://127.0.0.1:3128")
	if _, err := run(t, Root(), "join", "--request", "codex", "--relay", box); err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("TINCAN_PROXY: %v", err)
	}
	if list, _ := owner.JoinRequests(t.Context()); len(list) != 0 {
		t.Fatalf("a refused proxy filed %+v", list)
	}
}

// join --request takes no code and no --replace; join <code> still needs
// its code.
func TestJoinRequestArgs(t *testing.T) {
	useConfig(t, client.Config{})
	if _, err := run(t, Root(), "join", "ABC", "--request", "codex", "--relay", "http://127.0.0.1:1"); err == nil {
		t.Fatal("a code with --request should be refused")
	}
	if _, err := run(t, Root(), "join", "--request", "codex", "--replace", "--relay", "http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("--replace with --request: %v", err)
	}
	if _, err := run(t, Root(), "join", "--relay", "http://127.0.0.1:1"); err == nil {
		t.Fatal("join with neither a code nor --request should be refused")
	}
	if _, err := run(t, Root(), "join", "--request", "codex"); err == nil || !strings.Contains(err.Error(), "--relay") {
		t.Fatalf("no --relay: %v", err)
	}
	for _, bad := range []string{"proxy-sandbox", "codex:proxy-sandbox", "codex:nonsense", "Bad Name", "codex,codex"} {
		if _, err := run(t, Root(), "join", "--request", bad, "--relay", "http://127.0.0.1:1"); err == nil {
			t.Fatalf("--request %q should be refused", bad)
		}
	}
	noPendingFile(t)
}

// Covers KTD9. Interrupting the wait leaves the request open; a rerun with
// the same pairs resumes it without filing again, and a rerun with a
// changed kind is refused and names the open request.
func TestJoinRequestResume(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	box, owner := newBox(t, m), ownerRelay(t, m)

	first := startCmd(t, "join", "--request", "codex,helper:hermes", "--relay", box)
	l := pendingRequest(t, owner)
	out, err := first.stop(t)
	if err == nil || !strings.Contains(err.Error(), "tincan join --request codex,helper:hermes --relay "+box) {
		t.Fatalf("interrupted wait: %v\n%s", err, out)
	}

	_, err = run(t, Root(), "join", "--request", "codex,helper:codex", "--relay", box)
	if err == nil || !strings.Contains(err.Error(), l.ID) || !strings.Contains(err.Error(), "finish or expire") {
		t.Fatalf("changed kind: %v", err)
	}
	if list, _ := owner.JoinRequests(t.Context()); len(list) != 1 || list[0].ID != l.ID {
		t.Fatalf("after the refused rerun: %+v", list)
	}

	again := startCmd(t, "join", "--request", "codex,helper:hermes")
	if l2 := pendingRequest(t, owner); l2.ID != l.ID {
		t.Fatalf("rerun filed %s, want to resume %s", l2.ID, l.ID)
	}
	decide(t, owner, l.ID, true)
	out, err = again.wait(t)
	if err != nil || !strings.Contains(out, l.Code) {
		t.Fatalf("resumed join: %v\n%s", err, out)
	}
	dir := filepath.Dir(client.ConfigPath())
	mustConfig(t, filepath.Join(dir, "codex.json"), box, "codex")
	mustConfig(t, filepath.Join(dir, "helper.json"), box, "helper")
	noPendingFile(t)
}

// Covers KTD9. When the claim's answer is lost, the rerun finds the request
// delivered, confirms each name with whoami, and writes the configs.
func TestJoinRequestLostClaimRecovers(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	box, owner := newBox(t, m), ownerRelay(t, m)

	first := startCmd(t, "join", "--request", "codex,helper", "--relay", box)
	l := pendingRequest(t, owner)
	first.stop(t)
	decide(t, owner, l.ID, true)
	r, _ := client.NewRelay(box, "")
	if st, err := r.ClaimJoinRequest(t.Context(), l.ID); err != nil || st.Status != client.JoinDelivered {
		t.Fatalf("claim: %+v %v", st, err)
	}

	out, err := run(t, Root(), "join", "--request", "codex,helper", "--relay", box)
	if err != nil {
		t.Fatalf("recovery: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already delivered") {
		t.Fatalf("output does not say the request was already delivered:\n%s", out)
	}
	dir := filepath.Dir(client.ConfigPath())
	mustConfig(t, filepath.Join(dir, "codex.json"), box, "codex")
	mustConfig(t, filepath.Join(dir, "helper.json"), box, "helper")
	noPendingFile(t)
}

// Covers KTD9. A config that cannot be written after the claim gets a
// rejoin line with --relay, which works; the pending file stays.
func TestJoinRequestWriteFailureRejoinHint(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	box, owner := newBox(t, m), ownerRelay(t, m)
	dir := filepath.Dir(client.ConfigPath())
	helper := filepath.Join(dir, "helper.json")
	saveJoinConfig = func(path string, c client.Config) error {
		if path == helper {
			return errors.New("disk full")
		}
		return client.SaveConfigTo(path, c)
	}
	t.Cleanup(func() { saveJoinConfig = client.SaveConfigTo })

	run1 := startCmd(t, "join", "--request", "codex,helper", "--relay", box)
	l := pendingRequest(t, owner)
	decide(t, owner, l.ID, true)
	out, err := run1.wait(t)
	hint := "TINCAN_CONFIG=" + helper + " tincan rejoin --relay " + box + " --name helper"
	if err == nil || !strings.Contains(out+err.Error(), hint) || !strings.Contains(out+err.Error(), "disk full") {
		t.Fatalf("want the rejoin hint %q, got %v\n%s", hint, err, out)
	}
	mustConfig(t, filepath.Join(dir, "codex.json"), box, "codex")
	if _, found, _ := client.LoadPendingJoinRequest(client.JoinRequestPath()); !found {
		t.Fatal("the pending file must stay while a config is unsaved")
	}

	saveJoinConfig = client.SaveConfigTo
	t.Setenv("TINCAN_CONFIG", helper)
	if out, err := run(t, Root(), "rejoin", "--relay", box, "--name", "helper"); err != nil {
		t.Fatalf("the hinted rejoin: %v\n%s", err, out)
	}
	mustConfig(t, helper, box, "helper")
}

// A target config that another agent took while the request waited for the
// owner is not overwritten when the agents are collected: that name gets a
// rejoin line for a config of its own, and the pending file stays.
func TestJoinRequestSaveKeepsConfigTakenWhileWaiting(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	box, owner := newBox(t, m), ownerRelay(t, m)
	dir := filepath.Dir(client.ConfigPath())
	helper := filepath.Join(dir, "helper.json")

	j := startCmd(t, "join", "--request", "codex,helper", "--relay", box)
	l := pendingRequest(t, owner)
	if err := client.SaveConfigTo(helper, client.Config{Relay: box, Agent: "grokbot"}); err != nil {
		t.Fatal(err)
	}
	decide(t, owner, l.ID, true)
	out, err := j.wait(t)
	if err == nil || !strings.Contains(err.Error(), `"grokbot"`) || !strings.Contains(err.Error(), "tincan rejoin --relay "+box+" --name helper") {
		t.Fatalf("want a refusal naming grokbot with a rejoin line, got %v\n%s", err, out)
	}
	mustConfig(t, helper, box, "grokbot")
	mustConfig(t, filepath.Join(dir, "codex.json"), box, "codex")
	if _, found, _ := client.LoadPendingJoinRequest(client.JoinRequestPath()); !found {
		t.Fatal("the pending file must stay while a config is unsaved")
	}
}

// Covers R10. An older relay's plain 404 stops with the invite message and
// writes nothing.
func TestJoinRequestOlderRelay(t *testing.T) {
	old := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(old.Close)
	useConfig(t, client.Config{})
	_, err := run(t, Root(), "join", "--request", "codex", "--relay", old.URL)
	if err == nil || !strings.Contains(err.Error(), "does not support join requests") || !strings.Contains(err.Error(), "invite") {
		t.Fatalf("older relay: %v", err)
	}
	noPendingFile(t)
	if _, err := os.Stat(client.ConfigPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("older relay wrote a config: %v", err)
	}
}

// Covers R6. A denied request says so, removes the pending file and writes
// no config.
func TestJoinRequestDenied(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	box, owner := newBox(t, m), ownerRelay(t, m)
	run1 := startCmd(t, "join", "--request", "codex", "--relay", box)
	l := pendingRequest(t, owner)
	decide(t, owner, l.ID, false)
	out, err := run1.wait(t)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied: %v\n%s", err, out)
	}
	noPendingFile(t)
	if _, err := os.Stat(client.ConfigPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied wrote a config: %v", err)
	}
}

// testClock is a clock a test moves by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Covers R9, KTD11. An expired request says to file again, spelling out the
// command, and removes the pending file.
func TestJoinRequestExpired(t *testing.T) {
	clk := &testClock{t: time.Now()}
	m := testrelay.New(t, relay.Config{Now: clk.Now})
	useConfig(t, client.Config{})
	box, owner := newBox(t, m), ownerRelay(t, m)
	run1 := startCmd(t, "join", "--request", "codex", "--relay", box)
	pendingRequest(t, owner)
	clk.Add(identity.JoinRequestTTL + time.Second)
	m.Server.Sweep(t.Context())
	out, err := run1.wait(t)
	if err == nil || !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "tincan join --request codex --relay "+box) {
		t.Fatalf("expired: %v\n%s", err, out)
	}
	noPendingFile(t)
}

// Covers R8. The machine's node key changing voids the request; the
// command says so and removes the pending file.
func TestJoinRequestVoided(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	box, owner := newBox(t, m), ownerRelay(t, m)
	joinStatusHold = time.Second
	t.Cleanup(func() { joinStatusHold = defaultJoinStatusHold })
	run1 := startCmd(t, "join", "--request", "codex", "--relay", box)
	pendingRequest(t, owner)
	n := testrelay.TailnetNode("nBOX", "box", testrelay.Login)
	n.Key = "nodekey:new"
	m.Who.Set(m.Addr("box"), n)
	out, err := run1.wait(t)
	if err == nil || !strings.Contains(err.Error(), "voided") || !strings.Contains(err.Error(), "key") {
		t.Fatalf("voided: %v\n%s", err, out)
	}
	noPendingFile(t)
}

// fakeJoinRelay answers the join request routes with a pending filing, an
// approved status and claim, a claim answer the test chooses.
func fakeJoinRelay(t *testing.T, claimCode int, claimBody string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/join-requests":
			_, _ = w.Write([]byte(`{"id":"j1","status":"pending","code":"ABC-DEF","agents":[{"name":"codex","kind":"codex"}],"expires_at":"` + time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339) + `"}`))
		case r.Method == "GET" && r.URL.Path == "/v1/join-requests/j1":
			_, _ = w.Write([]byte(`{"id":"j1","status":"approved"}`))
		case r.Method == "POST" && r.URL.Path == "/v1/join-requests/j1/claim":
			w.WriteHeader(claimCode)
			_, _ = w.Write([]byte(claimBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Covers KTD5. A claim refused because a name can no longer be joined says
// which name, and removes the pending file.
func TestJoinRequestFailed(t *testing.T) {
	url := fakeJoinRelay(t, http.StatusConflict, `{"id":"j1","status":"failed","name":"codex","error":"codex: a requested name can no longer be joined: name taken"}`)
	useConfig(t, client.Config{})
	_, err := run(t, Root(), "join", "--request", "codex", "--relay", url)
	if err == nil || !strings.Contains(err.Error(), "failed") || !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "name taken") {
		t.Fatalf("failed: %v", err)
	}
	noPendingFile(t)
}

// Covers R5, R6. On the relay host, join-requests lists each request with
// its machine, login, names with kinds and code, rendering requester text
// safely; approve-join and deny-join decide by id over the socket, and a
// --relay is refused.
func TestJoinRequestRelayHostCommands(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	serveAdminSocket(t, m)
	evil := m.AddMachine(t, "evil", testrelay.TailnetNode("nEVIL", "evil\x1b[2Jbox", testrelay.Login))
	r, _ := client.NewRelay(evil, "")
	a, err := r.FileJoinRequest(t.Context(), []client.JoinRequestAgent{{Name: "codex", Kind: "codex"}, {Name: "scout"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := client.NewRelay(newBox(t, m), "")
	if err != nil {
		t.Fatal(err)
	}
	bs, err := b.FileJoinRequest(t.Context(), []client.JoinRequestAgent{{Name: "helper", Kind: "hermes"}})
	if err != nil {
		t.Fatal(err)
	}

	out, err := run(t, Root(), "join-requests")
	if err != nil {
		t.Fatalf("join-requests: %v", err)
	}
	for _, want := range []string{a.ID, a.Code, bs.ID, bs.Code, `evil\u001b[2Jbox`, testrelay.Login, "codex (kind codex)", "scout (no kind)", "helper (kind hermes)", "tincan approve-join " + a.ID} {
		if !strings.Contains(out, want) {
			t.Fatalf("join-requests lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("join-requests printed a raw escape:\n%q", out)
	}

	if _, err := run(t, Root(), "approve-join", a.ID, "--relay", m.URL("admin")); err == nil || !strings.Contains(err.Error(), "phone page") {
		t.Fatalf("approve-join --relay: %v", err)
	}
	if st, _ := r.JoinRequestStatus(t.Context(), a.ID, 0); st.Status != client.JoinPending {
		t.Fatalf("after the refused approve: %+v", st)
	}
	if out, err := run(t, Root(), "approve-join", a.ID); err != nil || !strings.Contains(out, "Approved") {
		t.Fatalf("approve-join: %v\n%s", err, out)
	}
	if st, _ := r.JoinRequestStatus(t.Context(), a.ID, 0); st.Status != client.JoinApproved {
		t.Fatalf("after approve-join: %+v", st)
	}
	if out, err := run(t, Root(), "deny-join", bs.ID); err != nil || !strings.Contains(out, "Denied") {
		t.Fatalf("deny-join: %v\n%s", err, out)
	}
	if st, _ := b.JoinRequestStatus(t.Context(), bs.ID, 0); st.Status != client.JoinDenied {
		t.Fatalf("after deny-join: %+v", st)
	}
	if _, err := run(t, Root(), "approve-join", bs.ID); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("approve after deny: %v", err)
	}
	if out, err := run(t, Root(), "join-requests"); err != nil || !strings.Contains(out, "No join requests") {
		t.Fatalf("empty listing: %v\n%s", err, out)
	}
}

// Without a relay on this machine the relay-host commands say where to
// decide instead.
func TestJoinRequestRelayHostCommandsNeedSocket(t *testing.T) {
	useConfig(t, client.Config{})
	for _, args := range [][]string{{"join-requests"}, {"approve-join", "x"}, {"deny-join", "x"}} {
		if _, err := run(t, Root(), args...); err == nil || !strings.Contains(err.Error(), "relay host") {
			t.Fatalf("%v without a socket: %v", args, err)
		}
	}
}
