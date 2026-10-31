//go:build unix

package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// workerRig is a relay, the asker grokbot, and the fixture directory for a
// per-request listener running as muse.
type workerRig struct {
	t       *testing.T
	m       *testrelay.Mesh
	grokbot *client.Relay
	dir     string
	log     *syncBuffer
}

func newWorkerRig(t *testing.T, cfg relay.Config) *workerRig {
	t.Helper()
	log := fastListen(t, time.Second, time.Minute, time.Second)
	oldPause, oldEvery := listenWorkerPause, listenWorkerReclaimEvery
	listenWorkerPause, listenWorkerReclaimEvery = 10*time.Millisecond, time.Hour
	t.Cleanup(func() { listenWorkerPause, listenWorkerReclaimEvery = oldPause, oldEvery })
	if cfg.PollHold == 0 {
		cfg.PollHold = time.Second
	}
	m := testrelay.New(t, cfg)
	return &workerRig{t: t, m: m, grokbot: m.Client(t, "grokbot"), dir: t.TempDir(), log: log}
}

// fixture is the default worker: it records what it was given, then waits
// for its release file and exits 0 without replying.
func (w *workerRig) fixture() string {
	return `d=` + w.dir + `; id=$TINCAN_REQUEST_ID
echo "$id" >> $d/starts
printf %s "$TINCAN_FROM" > $d/$id.from
printf %s "$TINCAN_SESSION" > $d/$id.session
pwd -P > $d/$id.cwd
cat > $d/$id.body
touch $d/$id.started
while [ ! -e $d/$id.release ]; do sleep 0.01; done`
}

func (w *workerRig) options(execCmd string) workerOptions {
	return workerOptions{
		exec:       execCmd,
		maxWorkers: 2,
		timeout:    time.Minute,
		newClient:  w.client,
	}
}

// client is a muse client in session, as the listener and a real worker use.
func (w *workerRig) client(session string) (*client.Relay, error) {
	r, err := client.NewRelayFor(client.Config{Relay: w.m.URL("muse"), Agent: "muse"})
	if err == nil {
		r.SetSession(session)
	}
	return r, err
}

func (w *workerRig) session(id string) *client.Relay {
	w.t.Helper()
	got, err := os.ReadFile(filepath.Join(w.dir, id+".session"))
	if err != nil {
		w.t.Fatal(err)
	}
	r, err := w.client(string(got))
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

// listen runs the per-request listener until the returned stop is called.
func (w *workerRig) listen(o workerOptions) (stop func()) {
	w.t.Helper()
	ctx, cancel := context.WithCancel(w.t.Context())
	done := make(chan error, 1)
	r, err := w.client("")
	if err != nil {
		w.t.Fatal(err)
	}
	go func() { done <- listenPerRequest(ctx, r, o) }()
	stopped := false
	stop = func() {
		w.t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != context.Canceled {
				w.t.Errorf("listenPerRequest = %v, want context.Canceled", err)
			}
		case <-time.After(30 * time.Second):
			w.t.Error("listenPerRequest did not return after its context ended")
		}
	}
	w.t.Cleanup(stop)
	return stop
}

func (w *workerRig) ask(body string) string {
	w.t.Helper()
	req, err := w.grokbot.Send(w.t.Context(), "muse", body, envelope.KindAsk, "", false)
	if err != nil {
		w.t.Fatal(err)
	}
	return req.ID
}

func (w *workerRig) started(id string) { w.t.Helper(); waitForFile(w.t, w.file(id+".started")) }

func (w *workerRig) release(id string) {
	w.t.Helper()
	if err := os.WriteFile(w.file(id+".release"), nil, 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *workerRig) file(name string) string { return filepath.Join(w.dir, name) }

func (w *workerRig) read(name string) string {
	w.t.Helper()
	got, err := os.ReadFile(w.file(name))
	if err != nil {
		w.t.Fatal(err)
	}
	return string(got)
}

func (w *workerRig) starts() []string {
	got, err := os.ReadFile(w.file("starts"))
	if err != nil {
		return nil
	}
	return strings.Fields(string(got))
}

// result waits for request id to reach want.
func (w *workerRig) result(id string, want envelope.Status) client.Result {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := w.grokbot.Get(w.t.Context(), id, 0)
		if err == nil && got.Status == want {
			return got
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("request %s: %s %v, want %s; log: %s", id, got.Status, err, want, w.log)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (w *workerRig) status(id string) envelope.Status {
	w.t.Helper()
	got, err := w.grokbot.Get(w.t.Context(), id, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	return got.Status
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPerRequestStartsAWorkerPerRequestUpToMaxWorkers(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	w.listen(w.options(w.fixture()))
	marker := w.file("injected")
	first := w.ask("one $(touch " + marker + ")")
	second := w.ask("two")
	third := w.ask("three")
	w.started(first)
	w.started(second)

	// The body is data: it reaches the worker on stdin and no shell reads it.
	if got := w.read(first + ".body"); got != "one $(touch "+marker+")" {
		t.Fatalf("stdin = %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the body ran: %v", err)
	}
	if got := w.read(first + ".from"); got != "grokbot" {
		t.Fatalf("TINCAN_FROM = %q", got)
	}
	a, b := w.read(first+".session"), w.read(second+".session")
	if a == "" || b == "" || a == b {
		t.Fatalf("worker sessions %q and %q", a, b)
	}

	// At --max-workers 2 the third stays queued, so no lease runs on it.
	time.Sleep(100 * time.Millisecond)
	if got := w.status(third); got != envelope.StatusQueued {
		t.Fatalf("third request is %s, want queued", got)
	}
	if got := w.starts(); len(got) != 2 {
		t.Fatalf("starts = %v, want two", got)
	}

	if _, err := w.session(first).Reply(t.Context(), first, "done", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	w.release(first)
	w.started(third)
	w.release(second)
	w.release(third)
}

// A request the relay marked reaches the worker's environment, and one it did
// not leaves the variable unset.
func TestPerRequestTellsAWorkerOfTheGuestMark(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	w.m = testrelay.NewWithAdminLogin(t, relay.Config{PollHold: time.Second})
	w.grokbot = w.m.Client(t, "grokbot")
	guest := w.m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600, NoHold: true})
	w.listen(w.options(`printf %s "${TINCAN_FROM_GUEST-unset}" > ` + w.dir + `/$TINCAN_REQUEST_ID.guest; ` + w.fixture()))
	req, err := guest.Send(t.Context(), "muse", "hello", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	w.started(req.ID)
	if got := w.read(req.ID + ".guest"); got != "1" {
		t.Fatalf("guest's ask: TINCAN_FROM_GUEST = %q", got)
	}
	plain := w.ask("hello")
	w.started(plain)
	if got := w.read(plain + ".guest"); got != "unset" {
		t.Fatalf("teammate's ask: TINCAN_FROM_GUEST = %q", got)
	}
}

func TestPerRequestExitWithoutReplyPostsFailedWithExitCode(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	w.listen(w.options("exit 3"))
	id := w.ask("work")
	got := w.result(id, envelope.StatusFailed)
	if got.Reply == nil || !strings.Contains(got.Reply.Body, "exit code 3") {
		t.Fatalf("failed reply: %+v", got.Reply)
	}
}

func TestPerRequestLeavesAWorkersReplyAlone(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	stop := w.listen(w.options(w.fixture()))
	id := w.ask("work")
	w.started(id)
	if _, err := w.session(id).Reply(t.Context(), id, "all done", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	w.release(id)
	stop()
	got := w.result(id, envelope.StatusAnswered)
	if got.Reply == nil || got.Reply.Body != "all done" {
		t.Fatalf("reply: %+v", got.Reply)
	}
}

func TestPerRequestTimeoutKillsProcessGroupAndPostsFailed(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	o := w.options(`sleep 300 & echo $! > ` + w.file("child") + `; wait`)
	o.timeout = 300 * time.Millisecond
	w.listen(o)
	id := w.ask("work")
	got := w.result(id, envelope.StatusFailed)
	if got.Reply == nil || !strings.Contains(got.Reply.Body, "timed out") {
		t.Fatalf("failed reply: %+v", got.Reply)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(w.read("child")))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the worker's child to die", func() bool { return !processAlive(pid) })
}

func TestPerRequestExitKillsWhatTheWorkerLeftRunning(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	w.listen(w.options(`sleep 300 & echo $! > ` + w.file("child")))
	id := w.ask("work")
	w.result(id, envelope.StatusFailed)
	pid, err := strconv.Atoi(strings.TrimSpace(w.read("child")))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the worker's child to die", func() bool { return !processAlive(pid) })
}

func TestPerRequestStoppingTheListenerStopsWorkers(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	stop := w.listen(w.options(`sleep 300 & echo $! > ` + w.file("child") + `; touch ` + w.file("ready") + `; wait`))
	id := w.ask("work")
	waitForFile(t, w.file("ready"))
	stop()
	got := w.result(id, envelope.StatusFailed)
	if got.Reply == nil || !strings.Contains(got.Reply.Body, "listener stopped") {
		t.Fatalf("failed reply: %+v", got.Reply)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(w.read("child")))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the worker's child to die", func() bool { return !processAlive(pid) })
}

func TestPerRequestNotifyStartsNoProcessAndPostsNoReply(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	w.listen(w.options(w.fixture()))
	notify, err := w.grokbot.Send(t.Context(), "muse", "fyi", envelope.KindNotify, "", false)
	if err != nil {
		t.Fatal(err)
	}
	ask := w.ask("work")
	w.started(ask)
	w.release(ask)
	if got := w.starts(); len(got) != 1 || got[0] != ask {
		t.Fatalf("starts = %v, want only the ask", got)
	}
	if got := w.status(notify.ID); got != envelope.StatusQueued {
		t.Fatalf("notify is %s, want queued for the agent's inbox", got)
	}
}

func TestPerRequestPingIsAnsweredWithoutAWorker(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	w.listen(w.options(w.fixture()))
	var ping envelope.Request
	eventually(t, "the relay to admit a ping", func() bool {
		var err error
		ping, err = w.grokbot.Send(t.Context(), "muse", "", envelope.KindPing, "", false)
		return err == nil
	})
	w.result(ping.ID, envelope.StatusAnswered)
	if got := w.starts(); len(got) != 0 {
		t.Fatalf("starts = %v, want none", got)
	}
}

// A lease that lapses while the worker runs requeues the request. The
// listener must not start it again, and must take it back.
func TestPerRequestWorkerOutlivingItsLeaseIsNotStartedTwice(t *testing.T) {
	w := newWorkerRig(t, relay.Config{ClaimLease: 40 * time.Millisecond, UrgentClaimLease: 40 * time.Millisecond})
	listenWorkerReclaimEvery = 200 * time.Millisecond
	w.listen(w.options(w.fixture()))
	id := w.ask("slow")
	w.started(id)
	session := w.read(id + ".session")

	requeued := false
	eventually(t, "the claim to lapse and come back", func() bool {
		w.m.Server.Sweep(t.Context())
		st := w.status(id)
		requeued = requeued || st == envelope.StatusQueued
		return requeued && st == envelope.StatusClaimed
	})
	// A second process would start from a later peek, so outlast a few.
	for end := time.Now().Add(3 * listenWorkerReclaimEvery); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		w.m.Server.Sweep(t.Context())
	}
	if got := w.starts(); len(got) != 1 {
		t.Fatalf("starts = %v, want one", got)
	}

	// The claim came back under the worker's session, so its chain holds.
	r, err := w.client(session)
	if err != nil {
		t.Fatal(err)
	}
	var sent envelope.Request
	eventually(t, "an ask inside the renewed claim", func() bool {
		sent, err = r.Send(t.Context(), "instinct", "help", envelope.KindAsk, "", false)
		if err != nil {
			t.Fatal(err)
		}
		return sent.ParentID == id
	})
	w.release(id)
}

func TestPerRequestKeepsItsClaimAlive(t *testing.T) {
	w := newWorkerRig(t, relay.Config{ClaimLease: 300 * time.Millisecond, UrgentClaimLease: 300 * time.Millisecond})
	listenWorkerReclaimEvery = 20 * time.Millisecond
	w.listen(w.options(w.fixture()))
	id := w.ask("slow")
	w.started(id)
	deadline := time.Now().Add(900 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.m.Server.Sweep(t.Context())
		if got := w.status(id); got != envelope.StatusClaimed {
			t.Fatalf("request is %s, want claimed", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.release(id)
}

// Each worker's session owns its own claim, so an ask that names no parent
// continues that worker's request even with two claims open.
func TestPerRequestConcurrentWorkersContinueTheirOwnChains(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	w.listen(w.options(w.fixture()))
	first, second := w.ask("one"), w.ask("two")
	w.started(first)
	w.started(second)
	for _, id := range []string{first, second} {
		sent, err := w.session(id).Send(t.Context(), "instinct", "help with "+id, envelope.KindAsk, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if sent.ParentID != id {
			t.Fatalf("ask from %s's worker has parent %q", id, sent.ParentID)
		}
	}
	w.release(first)
	w.release(second)
}

// A worker that asks its asker a question ends its turn. It posts no failure,
// and the answer starts a fresh worker with the exchange on stdin.
func TestPerRequestNeedsInputResumesInAFreshWorker(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	listenWorkerReclaimEvery = 20 * time.Millisecond
	w.listen(w.options(w.fixture()))
	id := w.ask("which repo?")
	w.started(id)
	if _, err := w.session(id).Reply(t.Context(), id, "name the repo", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	// The asker answers while the first worker is still winding down.
	if _, err := w.grokbot.Answer(t.Context(), id, "agent-tincan"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := w.starts(); len(got) != 1 {
		t.Fatalf("starts = %v, want the first worker only while it runs", got)
	}
	w.release(id)
	eventually(t, "a fresh worker", func() bool { return len(w.starts()) == 2 })
	got := w.result(id, envelope.StatusFailed)
	if !strings.Contains(got.Reply.Body, "exit code 0") {
		t.Fatalf("fresh worker's reply: %+v", got.Reply)
	}
	body := w.read(id + ".body")
	if !strings.HasPrefix(body, "which repo?") || !strings.Contains(body, "agent-tincan") {
		t.Fatalf("stdin of the fresh worker: %q", body)
	}
}

func TestPerRequestOldRelayWithoutPendingKindsStartsNothing(t *testing.T) {
	log := fastListen(t, time.Second, time.Minute, time.Second)
	old := listenWorkerPause
	listenWorkerPause = time.Millisecond
	t.Cleanup(func() { listenWorkerPause = old })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "ran")
	ts := newOldRelay(t, cancel)
	err := listenPerRequest(ctx, ts, workerOptions{
		exec: "touch " + marker, maxWorkers: 2, timeout: time.Minute,
		newClient: func(string) (*client.Relay, error) { return ts, nil },
	})
	if err != context.Canceled {
		t.Fatalf("listenPerRequest = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a worker ran: %v", err)
	}
	if got := strings.Count(log.String(), "upgrade the relay"); got != 1 {
		t.Fatalf("%d upgrade lines in the log: %q", got, log)
	}
}

// newOldRelay answers peeks as a relay that names no pending requests, and
// ends ctx after a few of them.
func newOldRelay(t *testing.T, cancel context.CancelFunc) *client.Relay {
	t.Helper()
	peeks := 0
	return newFakeListenRelayHandler(t, func(rw http.ResponseWriter, _ *http.Request) {
		if peeks++; peeks > 5 {
			cancel()
		}
		fmt.Fprint(rw, `{"waiting":1,"queued":1}`)
	})
}

func gitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "-q", "--allow-empty", "-m", "start"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return repo
}

func worktreeCount(t *testing.T, repo string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(out), "worktree ")
}

func TestPerRequestWorktrees(t *testing.T) {
	for _, tc := range []struct {
		name, worker string
		kept         string // log text when the worktree stays, "" when removed
	}{
		{name: "clean", worker: `true`},
		{name: "dirty", worker: `touch left-behind`, kept: "uncommitted changes"},
		{name: "committed", worker: `git commit -q --allow-empty -m work`, kept: "new commits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkerRig(t, relay.Config{})
			repo := gitRepo(t)
			root := filepath.Join(t.TempDir(), "worktrees")
			o := w.options(`pwd -P > ` + w.file("cwd") + `; ` + tc.worker)
			o.repo, o.worktrees = repo, root
			w.listen(o)
			id := w.ask("work")
			w.result(id, envelope.StatusFailed)
			waitForFile(t, w.file("cwd"))
			cwd := strings.TrimSpace(w.read("cwd"))
			realRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Dir(cwd) != realRoot {
				t.Fatalf("worker ran in %s, want a directory under %s", cwd, realRoot)
			}
			if tc.kept == "" {
				eventually(t, "the worktree to go", func() bool {
					_, err := os.Stat(cwd)
					return os.IsNotExist(err) && worktreeCount(t, repo) == 1
				})
				return
			}
			eventually(t, "the kept line", func() bool { return strings.Contains(w.log.String(), tc.kept) })
			if _, err := os.Stat(cwd); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(w.log.String(), filepath.Base(cwd)) {
				t.Fatalf("log does not name the worktree: %q", w.log)
			}
			if got := worktreeCount(t, repo); got != 2 {
				t.Fatalf("%d worktrees, want the repo and the kept one", got)
			}
		})
	}
}

// A worker confined to its tree can still rewrite the tree's .git file. The
// listener runs git on the tree unconfined, so what that file points at must
// not decide what git runs.
func TestPerRequestRewrittenGitFileRunsNothingInTheListener(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	repo := gitRepo(t)
	marker := w.file("hook-ran")
	o := w.options(`mkdir -p evil/objects evil/refs
echo 'ref: refs/heads/main' > evil/HEAD
printf '#!/bin/sh\ntouch ` + marker + `\n' > evil/fsmonitor
chmod +x evil/fsmonitor
printf '[core]\n\trepositoryformatversion = 0\n\tfsmonitor = %s\n' "$PWD/evil/fsmonitor" > evil/config
printf 'gitdir: %s\n' "$PWD/evil" > .git
pwd -P > ` + w.file("cwd"))
	o.repo, o.worktrees = repo, filepath.Join(t.TempDir(), "worktrees")
	w.listen(o)
	id := w.ask("work")
	w.result(id, envelope.StatusFailed)
	eventually(t, "the kept line", func() bool { return strings.Contains(w.log.String(), "kept worktree") })
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the listener ran the worker's program: %v", err)
	}
	// The rewrite does work on git run from the tree, so the check above can fail.
	if out, err := exec.Command("git", "-C", strings.TrimSpace(w.read("cwd")), "status", "--porcelain").CombinedOutput(); err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the rewritten .git did not take effect: %v", err)
	}
}

func TestPerRequestHalfMadeWorktreeIsCleared(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	repo := gitRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "worktrees")
	o := w.options(w.fixture())
	o.repo, o.worktrees = repo, root
	w.listen(o)
	id := w.ask("work")
	got := w.result(id, envelope.StatusFailed)
	if !strings.Contains(got.Reply.Body, "worktree") {
		t.Fatalf("failed reply: %+v", got.Reply)
	}
	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 || worktreeCount(t, repo) != 1 {
		t.Fatalf("left behind: %v, %d worktrees", left, worktreeCount(t, repo))
	}
}

func TestWorkerPathsAreAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TINCAN_CONFIG", filepath.Join("conf", "client.json"))
	config, worktrees, err := workerPaths("muse")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(config) || !filepath.IsAbs(worktrees) {
		t.Fatalf("config %q, worktrees %q", config, worktrees)
	}
	wantDir := filepath.Dir(config)
	if filepath.Base(wantDir) != "conf" || !strings.HasPrefix(worktrees, wantDir+string(filepath.Separator)) {
		t.Fatalf("config %q, worktrees %q", config, worktrees)
	}
	env := workerEnv(envelope.Request{ID: "r1", From: "grokbot"}, "worker-1", config)
	if !slices.Contains(env, "TINCAN_CONFIG="+config) {
		t.Fatalf("env %v", env)
	}
}

func TestPerRequestWorktreeThatCannotBeMadeFailsTheRequest(t *testing.T) {
	w := newWorkerRig(t, relay.Config{})
	repo := gitRepo(t)
	o := w.options(w.fixture())
	o.repo, o.worktrees = repo, filepath.Join(t.TempDir(), "worktrees")
	w.listen(o)
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	id := w.ask("work")
	got := w.result(id, envelope.StatusFailed)
	if !strings.Contains(got.Reply.Body, "worktree") {
		t.Fatalf("failed reply: %+v", got.Reply)
	}
	if len(w.starts()) != 0 {
		t.Fatal("the worker ran without its worktree")
	}
}

func TestListenFlagChecks(t *testing.T) {
	repo := gitRepo(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"worktree needs per-request", []string{"--exec", "true", "--worktree", repo}, "--per-request"},
		{"max-workers needs per-request", []string{"--exec", "true", "--max-workers", "3"}, "--per-request"},
		{"timeout needs per-request", []string{"--exec", "true", "--timeout", "1m"}, "--per-request"},
		{"once does not apply", []string{"--exec", "true", "--per-request", "--once"}, "--once"},
		{"no workers", []string{"--exec", "true", "--per-request", "--max-workers", "0"}, "--max-workers"},
		{"no timeout", []string{"--exec", "true", "--per-request", "--timeout", "0s"}, "--timeout"},
		{"worktree is not a repo", []string{"--exec", "true", "--per-request", "--worktree", t.TempDir()}, "not a git repository"},
		{"worktree is missing", []string{"--exec", "true", "--per-request", "--worktree", filepath.Join(repo, "nope")}, "not a git repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := listenCmd()
			cmd.SetArgs(tc.args)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			err := cmd.ExecuteContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("listen %v = %v, want an error naming %s", tc.args, err, tc.want)
			}
		})
	}
}
