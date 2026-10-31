package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// The per-request listener's timing. Tests override the first two.
var (
	// listenWorkerPause is the wait between peeks while something stays
	// waiting that no worker can take: a peek answers at once then, so
	// without it the loop would spin on a notify, an unseen reply or a full
	// set of workers. A worker exiting ends the wait early.
	listenWorkerPause = 5 * time.Second
	// listenWorkerReclaimEvery is how often a running worker's claim is
	// renewed. It stays well inside the relay's default urgent claim lease
	// (10 minutes), so a tick lost to a relay restart costs nothing.
	listenWorkerReclaimEvery = 2 * time.Minute
)

const (
	// listenWorkerReplyWindow bounds posting a worker's failure, and the
	// cleanup git commands, so they still run on the way out of a listener
	// whose context has ended, and cannot hold it there.
	listenWorkerReplyWindow = 15 * time.Second
	// listenWorkerWaitDelay bounds the wait for a worker's stdin copy once
	// the worker is gone, in case a surviving child holds the pipe.
	listenWorkerWaitDelay = 5 * time.Second
)

// workerOptions configures listenPerRequest.
type workerOptions struct {
	exec       string
	repo       string // "" runs workers in the listener's directory
	worktrees  string // where worktrees of repo are made
	config     string // absolute config path for workers, "" leaves theirs alone
	maxWorkers int
	timeout    time.Duration
	// newClient returns a client whose calls come from session. A client
	// names one session for life, and a worker's claim must carry its own.
	newClient func(session string) (*client.Relay, error)
}

// listenPerRequest starts one fresh process for each ask waiting for this
// agent, up to o.maxWorkers at a time. Pings are answered here. Notifies and
// replies stay for the agent's own inbox. It returns when ctx ends or the
// relay refuses this agent, with every worker stopped first.
func listenPerRequest(ctx context.Context, r *client.Relay, o workerOptions) error {
	var retries client.PongRetries
	defer retries.Wait()
	var workers sync.WaitGroup
	defer workers.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	running := make(map[string]bool, o.maxWorkers) // the loop's alone
	exited := make(chan string, o.maxWorkers)
	wait := func(d time.Duration) {
		select {
		case <-ctx.Done():
		case id := <-exited:
			delete(running, id)
		case <-time.After(d):
		}
	}
	backoff := time.Second
	warnedKinds := false
	for {
		for drained := false; !drained; {
			select {
			case id := <-exited:
				delete(running, id)
			default:
				drained = true
			}
		}
		w, err := r.Peek(ctx, client.DefaultPollHold)
		n := w.Total - w.Pings
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case client.IsStatus(err, 403), client.IsProxyAuth(err):
			return err
		case err != nil:
			wait(jitter(backoff))
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		if answerPendingPings(ctx, r, w.Pending, &retries) && n <= 0 {
			wait(jitter(backoff))
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		_ = client.ReportUpgradeOn(client.UpgradeSurfaceListen, w.UpgradeAvailable, func(line string) error {
			listenLogf("%s", strings.TrimSpace(line))
			return nil
		})
		if !warnedKinds && w.Queued > w.Pings && !namesKinds(w.Pending) {
			warnedKinds = true
			listenLogf("the relay does not name waiting requests and their kinds, so no worker can start: upgrade the relay")
		}
		started := false
		for _, p := range w.Pending {
			if len(running) >= o.maxWorkers {
				break
			}
			// An id still running here lost its lease and went back to the
			// queue. Its worker's next renewal takes it back.
			if p.Kind != envelope.KindAsk || running[p.ID] {
				continue
			}
			wr, req, err := claimForWorker(ctx, o, p.ID)
			if err != nil {
				if !client.IsStatus(err, 409) && ctx.Err() == nil {
					listenLogf("request %s: claim failed: %v", p.ID, err)
				}
				continue
			}
			running[p.ID], started = true, true
			workers.Go(func() {
				defer func() { exited <- req.ID }()
				runWorker(ctx, wr, req, o)
			})
		}
		if !started && n > 0 {
			wait(listenWorkerPause)
		}
	}
}

// namesKinds reports whether a peek named any ordinary request with its
// kind, which a relay that predates pending kinds never does.
func namesKinds(pending []envelope.Pending) bool {
	return slices.ContainsFunc(pending, func(p envelope.Pending) bool {
		return p.Kind != "" && p.Kind != envelope.KindPing
	})
}

// claimForWorker takes request id under a new session and returns the client
// of that session. The claim records the session, which is how the relay
// files the worker's own asks under this request while others run.
func claimForWorker(ctx context.Context, o workerOptions, id string) (*client.Relay, envelope.Request, error) {
	wr, err := o.newClient(newWorkerSession())
	if err != nil {
		return nil, envelope.Request{}, err
	}
	req, err := wr.Claim(ctx, id)
	return wr, req, err
}

func newWorkerSession() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "worker-" + hex.EncodeToString(b)
}

// runWorker holds req's claim while its process runs, and answers for a
// worker that left the request open. ctx is the listener's.
func runWorker(ctx context.Context, wr *client.Relay, req envelope.Request, o workerOptions) {
	release := sync.OnceFunc(keepWorkerClaim(ctx, wr, req))
	defer release()
	runCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	dir := ""
	if o.repo != "" {
		tree, err := addWorktree(runCtx, o.repo, o.worktrees)
		if err != nil {
			listenLogf("request %s: worktree of %s: %v", req.ID, o.repo, err)
			release()
			postWorkerFailed(ctx, wr, req, "tincan listen could not make this request's worktree, so no worker ran.")
			return
		}
		defer tree.remove(ctx, req.ID)
		dir = tree.dir
	}
	outcome := runWorkerProcess(ctx, runCtx, o, req, wr.Session(), dir)
	release()
	postWorkerFailed(ctx, wr, req, outcome)
}

// runWorkerProcess runs the worker to its end and says how it ended, for the
// failed reply if it left the request open.
func runWorkerProcess(ctx, runCtx context.Context, o workerOptions, req envelope.Request, session, dir string) string {
	c := exec.CommandContext(runCtx, "sh", "-c", o.exec)
	c.Dir = dir
	c.Env = append(c.Environ(), workerEnv(req, session, o.config)...)
	c.Stdin = strings.NewReader(workerStdin(req))
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	c.WaitDelay = listenWorkerWaitDelay
	setWorkerProcessGroup(c)
	err := c.Run()
	if c.Process != nil {
		killWorkerGroup(c)
	}
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("The worker timed out after %s and was killed before it replied.", o.timeout)
	case ctx.Err() != nil:
		return "The listener stopped and killed the worker before it replied."
	case c.ProcessState == nil:
		listenLogf("request %s: worker did not start: %v", req.ID, err)
		return "The worker could not start."
	case c.ProcessState.ExitCode() < 0:
		return fmt.Sprintf("The worker ended without replying (%s).", c.ProcessState)
	}
	return fmt.Sprintf("The worker exited without replying (exit code %d).", c.ProcessState.ExitCode())
}

// workerEnv is what a worker is told beyond the listener's own environment.
// TINCAN_FROM_GUEST carries the relay's guest mark: a process has only its
// environment and stdin, and no standing instructions to read the body's
// guest line for it.
func workerEnv(req envelope.Request, session, config string) []string {
	env := []string{
		"TINCAN_REQUEST_ID=" + req.ID,
		"TINCAN_SESSION=" + session,
		"TINCAN_FROM=" + req.From,
	}
	if req.FromGuest {
		env = append(env, "TINCAN_FROM_GUEST=1")
	}
	if config != "" {
		// A relative TINCAN_CONFIG would not resolve from a worktree.
		env = append(env, "TINCAN_CONFIG="+config)
	}
	return env
}

// workerStdin is the request as data for the worker: its body, then only
// when there are any, its attachments and clarification rounds, which a
// fresh process has no other turn to have seen.
func workerStdin(req envelope.Request) string {
	more := client.FormatAttachments(req.Attachments) + client.FormatExchanges(req.Exchanges)
	if more == "" {
		return req.Body
	}
	return req.Body + "\n---\n" + more
}

// keepWorkerClaim renews req's claim while the worker runs, until the
// returned stop is called. Claiming again renews a live lease and retakes a
// lapsed one under the same session. It ends once the request is no longer
// the worker's to hold: closed, or waiting on the asker. Claiming would pull
// a request the asker just answered out of the queue, away from the fresh
// worker it is waiting for.
func keepWorkerClaim(ctx context.Context, wr *client.Relay, req envelope.Request) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(listenWorkerReclaimEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if workerAsked(ctx, wr, req) {
				return
			}
			_, err := wr.Claim(ctx, req.ID)
			if client.IsStatus(err, 409) {
				return
			}
			if err != nil && ctx.Err() == nil {
				listenLogf("request %s: renew claim: %v", req.ID, err)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// workerAsked reports whether the worker put a clarifying question on req
// since it claimed it. Its answer belongs to a fresh worker, even if the
// asker has already given it and the request is queued again.
func workerAsked(ctx context.Context, wr *client.Relay, req envelope.Request) bool {
	got, err := wr.Get(ctx, req.ID, 0)
	return err == nil && len(got.Exchanges) > len(req.Exchanges)
}

// postWorkerFailed closes a request its worker left open. A request already
// closed answers 409 and is left alone, so a worker's own reply always wins.
// It runs on the way out too, so it outlives ctx by listenWorkerReplyWindow.
func postWorkerFailed(ctx context.Context, wr *client.Relay, req envelope.Request, body string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), listenWorkerReplyWindow)
	defer cancel()
	if workerAsked(ctx, wr, req) {
		return
	}
	for delay := time.Second; ; delay *= 2 {
		_, err := wr.Reply(ctx, req.ID, body, envelope.StatusFailed)
		if err == nil || client.IsStatus(err, 409) {
			return
		}
		var api *client.APIError
		if errors.As(err, &api) && api.Code < 500 {
			listenLogf("request %s: post failed reply: %v", req.ID, err)
			return
		}
		select {
		case <-ctx.Done():
			listenLogf("request %s: post failed reply: %v", req.ID, err)
			return
		case <-time.After(delay):
		}
	}
}

// checkWorktreeRepo returns repo's absolute path, or an error when it cannot
// give a worker a worktree.
func checkWorktreeRepo(ctx context.Context, repo string) (string, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	if _, err := git(ctx, abs, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return "", fmt.Errorf("--worktree %s is not a git repository with a commit", repo)
	}
	return abs, nil
}

// workerTree is one worker's git worktree of repo, on a detached HEAD. gitDir
// is the worktree's own git directory, read before the worker ran.
type workerTree struct {
	repo, dir, gitDir, base string
}

func addWorktree(ctx context.Context, repo, root string) (*workerTree, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "worker-")
	if err != nil {
		return nil, err
	}
	out, err := git(ctx, repo, "worktree", "add", "-q", "--detach", dir, "HEAD")
	if err == nil {
		out, err = git(ctx, dir, "rev-parse", "--absolute-git-dir", "HEAD")
	}
	if err != nil {
		discardWorktree(ctx, repo, dir)
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	gitDir, base, _ := strings.Cut(out, "\n")
	return &workerTree{repo: repo, dir: dir, gitDir: gitDir, base: base}, nil
}

// discardWorktree clears a worktree no worker has run in, which may be half
// made. ctx may have ended, which may be why it is half made.
func discardWorktree(ctx context.Context, repo, dir string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), listenWorkerReplyWindow)
	defer cancel()
	_, _ = git(ctx, repo, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
}

// remove takes the worktree away when the worker left nothing in it. On a
// detached HEAD a new commit has no branch to keep it, so it counts as
// something left. git worktree remove refuses a dirty tree by itself, and it
// is never forced.
//
// The worker can rewrite the tree's .git file, and git run from the tree would
// then take its config, and run its programs, from wherever that points. So
// the checks use the git directory recorded before the worker ran, and skip
// submodules, whose .git is also the worker's. A rewritten .git makes the
// remove refuse too, which keeps the tree.
func (w *workerTree) remove(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), listenWorkerReplyWindow)
	defer cancel()
	switch status, err := git(ctx, w.repo, "--git-dir="+w.gitDir, "--work-tree="+w.dir, "status", "--porcelain", "--ignore-submodules=all"); {
	case err != nil:
		listenLogf("request %s: kept worktree %s: %v", id, w.dir, err)
		return
	case status != "":
		listenLogf("request %s: kept worktree %s: it has uncommitted changes", id, w.dir)
		return
	}
	if head, err := git(ctx, w.repo, "--git-dir="+w.gitDir, "rev-parse", "HEAD"); err != nil || head != w.base {
		listenLogf("request %s: kept worktree %s: it has new commits", id, w.dir)
		return
	}
	if out, err := git(ctx, w.repo, "worktree", "remove", w.dir); err != nil {
		listenLogf("request %s: kept worktree %s: %v: %s", id, w.dir, err, out)
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
