package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// socketTarget is a session pushSocket can wake: a pusher that also says
// whether the session is idle right now.
type socketTarget interface {
	channelPusher
	idle() bool
}

// socketFailLimit is how many writes in a row may fail before pushSocket
// gives up on the socket, so a session that cannot be woken stops looking
// online to teammates.
const socketFailLimit = 3

// socketRecordKeep is how long a delivered id stays in the shared record
// after a peek last listed it. A window's peek can miss an item another
// window's showed (the pending list is capped), so the record does not
// forget an id the moment one peek leaves it out.
var socketRecordKeep = 10 * time.Minute

// errSocketFailing ends pushSocket after socketFailLimit failed writes.
var errSocketFailing = errors.New("session socket keeps failing")

// pushSocket announces waiting items into one Claude Code session through
// its messaging socket. Unlike a channel notice, every socket message starts
// a turn, so it differs from pushWaiting in four ways:
//
//   - It never pushes the upgrade notice, which reaches the agent on its next
//     tool call anyway.
//   - It pushes only while the session is idle.
//   - Each item is pushed once. A reply to an ask this process sent is pushed
//     by this window first; teammate requests and replies to other asks go to
//     whichever idle window gets there. All of them go through a lock and
//     record shared by the agent's windows on this machine, so one item
//     starts one turn on the machine.
//   - It returns errSocketFailing after socketFailLimit failed writes in a
//     row, and nil when ctx ends.
//
// Pings are answered as pushWaiting answers them.
func pushSocket(ctx context.Context, r *client.Relay, t socketTarget, shared socketShared, own map[string]bool) error {
	select {
	case <-ctx.Done():
		return nil
	case <-t.Ready():
	}
	// The peeks below tell the relay this session wakes by socket, so an
	// asker hears it will be prompted. Once the loop ends nothing pushes,
	// so the advert stops with it.
	r.AdvertiseSocketWake(true)
	defer r.AdvertiseSocketWake(false)
	var retries client.PongRetries
	defer retries.Wait()
	backoff, fails := time.Second, 0
	for ctx.Err() == nil {
		peekedAt := time.Now()
		w, err := r.Peek(ctx, client.DefaultPollHold)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("tincan socket wake: %v", client.RejoinHint(err, r.Base()))
				sleepCtx(ctx, jitter(backoff))
				backoff = min(backoff*2, 30*time.Second)
			}
			continue
		}
		w, pingFailed := answerChannelPings(ctx, r, w, &retries)
		if pingFailed && w.Total <= 0 {
			sleepCtx(ctx, jitter(backoff))
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		// settle counts failed writes in a row; a write that lands resets
		// the count.
		settle := func(tried bool, err error) bool {
			switch {
			case err != nil:
				fails++
				log.Printf("tincan socket wake: push: %v", err)
			case tried:
				fails = 0
			}
			return fails >= socketFailLimit
		}
		mine, rest := splitOwn(r, w)
		if t.idle() {
			wrote, err := pushOwn(ctx, t, mine, own, shared)
			if settle(wrote, err) {
				return errSocketFailing
			}
			// A message just started a turn, so the session is no longer
			// idle; shared items wait for the next recheck and its own idle
			// check rather than landing in a busy session.
			if !wrote && settle(pushShared(ctx, t, rest, shared, peekedAt)) {
				return errSocketFailing
			}
		} else {
			forgetOwn(mine, own)
		}
		if w.Total > 0 {
			sleepCtx(ctx, waitingRecheck)
		}
	}
	return nil
}

// splitOwn splits a peek into the replies to asks this process sent and
// everything else.
func splitOwn(r *client.Relay, w client.Waiting) (mine []client.Result, rest client.Waiting) {
	rest = w
	rest.Replies = nil
	for _, rep := range w.Replies {
		if r.SentHere(rep.Request.ID) {
			mine = append(mine, rep)
		} else {
			rest.Replies = append(rest.Replies, rep)
		}
	}
	return mine, rest
}

// pendingKey and replyKey name one delivery of an item. A request that is
// queued again (a clarification answered, or a lease that ran out) is the
// same request, and an answer it gets again is a new reply generation, so
// each needs its own turn: the request key is forgotten once the request
// leaves the queue, revKey names each return to the queue, and the reply key
// carries its generation.
func pendingKey(id string) string { return "q:" + id }

// revKey names one return of a request to the queue. It sits beside
// pendingKey rather than replacing it, because windows that predate rev
// share this record: they read and write only the request key, drop request
// keys they do not list, and leave a v: key alone. A request key with no rev
// key is such a window's delivery, of the rev now queued.
func revKey(p envelope.Pending) string { return fmt.Sprintf("v:%s@%d", p.ID, p.Rev) }

// revKeyID returns the request id of a revKey.
func revKeyID(k string) string {
	id, _, _ := strings.Cut(strings.TrimPrefix(k, "v:"), "@")
	return id
}

func replyKey(rep client.Result) string {
	gen := int64(0)
	if rep.Reply != nil {
		gen = rep.Reply.Generation
	}
	return fmt.Sprintf("r:%s#%d", rep.Request.ID, gen)
}

// countKey names a queue count from a relay that predates pending lists.
func countKey(n int) string { return fmt.Sprintf("q#%d", n) }

// forgetOwn drops own reply keys no longer waiting.
func forgetOwn(mine []client.Result, own map[string]bool) {
	listed := map[string]bool{}
	for _, rep := range mine {
		listed[replyKey(rep)] = true
	}
	for k := range own {
		if !listed[k] {
			delete(own, k)
		}
	}
}

// pushOwn pushes one notice for this session's own replies not pushed yet,
// and forgets keys no longer waiting. It reports whether it wrote. own
// outlives one pushSocket run, so a recovered socket does not announce a
// reply it already delivered. It goes through the same lock and record as
// shared items: a relay that predates per-session replies shows a reply to
// every window, and the other windows must see that this one delivered it.
// When another window holds the lock it writes nothing and the next recheck
// tries again.
func pushOwn(ctx context.Context, t socketTarget, mine []client.Result, own map[string]bool, shared socketShared) (bool, error) {
	forgetOwn(mine, own)
	var pending []client.Result
	for _, rep := range mine {
		if !own[replyKey(rep)] {
			pending = append(pending, rep)
		}
	}
	if len(pending) == 0 {
		return false, nil
	}
	release, ok, err := shared.tryLock()
	if err != nil {
		// Without the lock nothing can be delivered, so this counts as a
		// failed write: repeated lock errors end socket wake instead of
		// advertising a wake that never comes.
		return true, fmt.Errorf("lock: %w", err)
	}
	if !ok {
		return false, nil
	}
	defer release()
	rec := shared.read()
	var fresh []client.Result
	for _, rep := range pending {
		if _, done := rec[replyKey(rep)]; done {
			// Another window already delivered it on this machine.
			own[replyKey(rep)] = true
			continue
		}
		fresh = append(fresh, rep)
	}
	if len(fresh) == 0 {
		return false, nil
	}
	n := socketNotice(client.Waiting{Replies: fresh, Total: len(fresh)})
	if err := t.Push(ctx, n.content, n.meta); err != nil {
		return true, err
	}
	now := time.Now()
	for _, rep := range fresh {
		own[replyKey(rep)] = true
		rec[replyKey(rep)] = now
	}
	if err := shared.write(rec); err != nil {
		return true, fmt.Errorf("delivered, but the record was not saved: %w", err)
	}
	return true, nil
}

// pushShared pushes one notice for the items no window on this machine has
// delivered yet. It takes the shared lock only for this one decision, so a
// busy window never holds up an idle one. When another window holds the
// lock it writes nothing and the next recheck tries again. peekedAt is when
// the peek behind w started; a delivery saved after it is newer than what
// w shows and is never taken as gone from the queue. A delivery the
// record could not save counts as a failed write: the next peek would send
// it again, so repeated save failures end socket wake like a failing socket.
func pushShared(ctx context.Context, t socketTarget, w client.Waiting, shared socketShared, peekedAt time.Time) (bool, error) {
	release, ok, err := shared.tryLock()
	if err != nil {
		// Without the lock nothing can be delivered, so this counts as a
		// failed write: repeated lock errors end socket wake instead of
		// advertising a wake that never comes.
		return true, fmt.Errorf("lock: %w", err)
	}
	if !ok {
		return false, nil
	}
	defer release()
	now := time.Now()
	rec := shared.read()
	legacy := len(w.Pending) == 0 && w.Queued > 0
	listed := map[string]bool{}
	for _, p := range w.Pending {
		listed[pendingKey(p.ID)] = true
	}
	if legacy {
		listed[countKey(w.Queued)] = true
	}
	for _, id := range w.HiddenIDs {
		// Named for another window's project and still queued: that window's
		// push of it must stand.
		listed[pendingKey(id)] = true
	}
	for _, rep := range w.Replies {
		listed[replyKey(rep)] = true
	}
	// The lists are complete when they hold every queued request, so a
	// request key they leave out has left the queue.
	completeQueue := !legacy && len(w.Pending) >= w.Queued && len(w.HiddenIDs) >= w.Hidden
	changed := false
	for k, at := range rec {
		switch {
		case strings.HasPrefix(k, "v:") && listed[pendingKey(revKeyID(k))]:
			if now.Sub(at) >= socketRecordKeep/2 {
				rec[k] = now
				changed = true
			}
		case listed[k] && strings.HasPrefix(k, "q#"):
			// A count key names no request, so a new request can bring back
			// a count already announced. Do not keep it past socketRecordKeep.
			if now.Sub(at) >= socketRecordKeep {
				delete(rec, k)
				changed = true
			}
		case listed[k]:
			// Still waiting: keep it fresh, so one capped or differently
			// scoped peek that leaves it out does not make it look new.
			if now.Sub(at) >= socketRecordKeep/2 {
				rec[k] = now
				changed = true
			}
		case (strings.HasPrefix(k, "q#") || (completeQueue && (strings.HasPrefix(k, "q:") || strings.HasPrefix(k, "v:")))) && at.Before(peekedAt), now.Sub(at) >= socketRecordKeep:
			delete(rec, k)
			changed = true
		}
	}
	revved := map[string]bool{}
	for k := range rec {
		if strings.HasPrefix(k, "v:") {
			revved[revKeyID(k)] = true
		}
	}
	var fresh client.Waiting
	var keys []string
	for _, p := range w.Pending {
		_, done := rec[pendingKey(p.ID)]
		if done && p.Rev != 0 {
			_, same := rec[revKey(p)]
			done = same || !revved[p.ID]
		}
		if !done {
			fresh.Pending = append(fresh.Pending, p)
			keys = append(keys, pendingKey(p.ID))
			if p.Rev != 0 {
				keys = append(keys, revKey(p))
			}
		}
	}
	fresh.Queued = len(fresh.Pending)
	if legacy {
		if _, done := rec[countKey(w.Queued)]; !done {
			fresh.Queued = w.Queued
			keys = append(keys, countKey(w.Queued))
		}
	}
	for _, rep := range w.Replies {
		if _, done := rec[replyKey(rep)]; !done {
			fresh.Replies = append(fresh.Replies, rep)
			keys = append(keys, replyKey(rep))
		}
	}
	fresh.Urgent = w.Urgent
	fresh.Total = fresh.Queued + len(fresh.Replies)
	if fresh.Total == 0 {
		if changed {
			if err := shared.write(rec); err != nil {
				log.Printf("tincan socket wake: record: %v", err)
			}
		}
		return false, nil
	}
	n := socketNotice(fresh)
	if err := t.Push(ctx, n.content, n.meta); err != nil {
		if changed {
			if err := shared.write(rec); err != nil {
				log.Printf("tincan socket wake: record: %v", err)
			}
		}
		return true, err
	}
	for _, k := range keys {
		rec[k] = now
	}
	if err := shared.write(rec); err != nil {
		return true, fmt.Errorf("delivered, but the record was not saved: %w", err)
	}
	return true, nil
}

// socketNotice is the notice for every item in w, built the way a channel
// notice is.
func socketNotice(w client.Waiting) notice {
	var a announcer
	n, _ := a.next(w, time.Now())
	return n
}

// socketShared is the record the agent's windows on this machine share, so
// that one item is delivered once: dir/<agent>.json maps item keys to when
// they were delivered or last seen waiting, guarded by dir/<agent>.lock.
type socketShared struct {
	dir, agent string
}

func (s socketShared) path() string { return filepath.Join(s.dir, agentFileName(s.agent)+".json") }

// read returns the record, empty when it is missing or unreadable.
func (s socketShared) read() map[string]time.Time {
	rec := map[string]time.Time{}
	b, err := os.ReadFile(s.path())
	if err == nil {
		_ = json.Unmarshal(b, &rec)
	}
	return rec
}

// write replaces the record. The caller holds the lock.
func (s socketShared) write(rec map[string]time.Time) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// socketWakeTiming is how socketWake keeps a session live while the socket
// is failing, and how long it waits before checking the socket again.
type socketWakeTiming struct {
	keepalive, retry time.Duration
}

// socketWake runs pushSocket until ctx ends. When the socket keeps failing,
// it keeps the session live with keepSession, which does not make the agent
// look online, and after timing.retry asks reverify for a fresh pusher; it
// resumes pushing once one checks out.
func socketWake(ctx context.Context, r *client.Relay, t socketTarget, shared socketShared, timing socketWakeTiming, reverify func() (socketTarget, error)) {
	own := map[string]bool{} // own reply keys already delivered, kept across retries
	for ctx.Err() == nil {
		if t != nil {
			if err := pushSocket(ctx, r, t, shared, own); err == nil {
				return
			}
			log.Printf("tincan socket wake: %v; keeping the session live and trying again in %s", errSocketFailing, timing.retry)
		}
		wait, cancel := context.WithTimeout(ctx, timing.retry)
		keepSession(wait, r, timing.keepalive)
		cancel()
		if ctx.Err() != nil {
			return
		}
		next, err := reverify()
		if err != nil {
			log.Printf("tincan socket wake: %v", err)
			t = nil
			continue
		}
		t = next
	}
}

// idle reports whether the session that owns the socket is idle now. It
// re-reads the registry each time and checks that the registered process
// is still the one verified at start, so a pid reused by another program
// never reads as an idle session. statusUpdatedAt is when the status last
// changed, not a heartbeat, so its age is not checked: a window idle for
// hours is the one most in need of waking.
func (p *claudeSocketPusher) idle() bool {
	if !p.verified {
		return false
	}
	s, err := p.session()
	if err != nil {
		return false
	}
	return s.Status == "idle" && s.PID == p.ppid && s.ProcStart == p.procStart && processAlive(s.PID)
}

// socketWakeDir is where the shared delivery records live: beside the
// agent's config, so each agent on the machine has its own.
func socketWakeDir() string {
	return filepath.Join(filepath.Dir(client.ConfigPath()), "socket-wake")
}

// wakePath is how a tincan mcp server keeps its session reachable.
type wakePath int

const (
	wakeByKeepalive wakePath = iota // nothing pushes; whoami keeps the session live
	wakeByChannel                   // the Claude Code channel transport
	wakeBySocket                    // the session's own messaging socket
)

func (w wakePath) String() string {
	return [...]string{"keepalive", "channel", "socket"}[w]
}

// pickWake decides the wake path from the flags. --channel wins, and the
// session socket is read only when socket wake is on, so --wake-socket=false
// neither reads the registry nor logs about it. fromEnv returns nil when
// there is no session socket that checks out; the server then runs the
// keepalive so the window keeps its replies.
func pickWake(channel, socket bool, fromEnv func() *claudeSocketPusher) (wakePath, *claudeSocketPusher) {
	if channel {
		return wakeByChannel, nil
	}
	if !socket {
		return wakeByKeepalive, nil
	}
	if p := fromEnv(); p != nil {
		return wakeBySocket, p
	}
	return wakeByKeepalive, nil
}

// claudeSocketFromEnv returns the socket pusher for the Claude Code session
// that started this process, or nil when there is none. It says why only
// when the session's socket is set but does not check out, so agents that
// are not Claude Code log nothing.
func claudeSocketFromEnv() *claudeSocketPusher {
	if os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET") == "" {
		return nil
	}
	p, err := newClaudeSocketPusher(os.Getenv, os.Getppid())
	if err != nil {
		log.Printf("tincan mcp: socket wake off: %v", err)
		return nil
	}
	return p
}

// agentFileName keeps an agent name safe as a file name.
func agentFileName(name string) string {
	if name == "" {
		return "agent"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, name)
}
