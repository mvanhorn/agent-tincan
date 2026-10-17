package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/history"
)

// claudeSocketWriteTimeout bounds one push: the session's socket is local,
// so a write that takes longer means the session is stuck and the next
// announcement will try again.
const claudeSocketWriteTimeout = 3 * time.Second

// claudeSessionInfo is the part of a Claude Code session registry file
// (<config dir>/sessions/<pid>.json) tincan reads. Status is "idle" or
// "busy" and changes as the session works, so callers re-read it.
type claudeSessionInfo struct {
	PID                 int    `json:"pid"`
	SessionID           string `json:"sessionId"`
	Status              string `json:"status"`
	ProcStart           string `json:"procStart"`
	MessagingSocketPath string `json:"messagingSocketPath"`
}

// claudeSocketPusher announces waiting items to the Claude Code session that
// started this process by writing a user message to the session's messaging
// socket, which starts a turn even when the session is idle and was not
// started with channels. It satisfies channelPusher, so pushWaiting drives it
// like the channel transport. Content is sent as given: callers pass the
// channel notice, which carries counts and senders only.
type claudeSocketPusher struct {
	socket   string
	token    string
	registry string
	ppid     int
	// procStart is the registered process's start time when verified, so
	// idle can tell the session from a later process given the same pid.
	procStart string
	verified  bool
	ready     chan struct{}
}

// newClaudeSocketPusher reads the session's socket path and token from
// getenv and checks that ppid is the Claude Code session that owns that
// socket. Programs run from the session's Bash tool inherit the same
// variables, and the registry check is what stops them from pushing into the
// session above them: only a process whose parent is the registered session
// passes. Ready closes only when the check passes; on failure the pusher is
// returned with the reason and never pushes. Errors never include the token.
func newClaudeSocketPusher(getenv func(string) string, ppid int) (*claudeSocketPusher, error) {
	p := &claudeSocketPusher{
		socket:   getenv("CLAUDE_CODE_MESSAGING_SOCKET"),
		token:    getenv("CLAUDE_CODE_MESSAGING_TOKEN"),
		registry: filepath.Join(history.ClaudeConfigDir(getenv), "sessions", strconv.Itoa(ppid)+".json"),
		ppid:     ppid,
		ready:    make(chan struct{}),
	}
	if p.socket == "" {
		return p, errors.New("claude session socket: CLAUDE_CODE_MESSAGING_SOCKET is not set")
	}
	s, err := p.session()
	if err != nil {
		return p, err
	}
	if s.PID != ppid {
		return p, fmt.Errorf("claude session socket %s: registry %s names pid %d, parent is %d", p.socket, p.registry, s.PID, ppid)
	}
	if s.MessagingSocketPath != p.socket {
		return p, fmt.Errorf("claude session socket %s: registry %s names socket %s", p.socket, p.registry, s.MessagingSocketPath)
	}
	p.procStart = s.ProcStart
	p.verified = true
	close(p.ready)
	return p, nil
}

// session reads the parent session's registry file. It reads the file on
// every call because the session rewrites it as its status changes.
func (p *claudeSocketPusher) session() (claudeSessionInfo, error) {
	return readClaudeSession(p.registry)
}

// readClaudeSession reads one Claude Code session registry file.
func readClaudeSession(path string) (claudeSessionInfo, error) {
	var s claudeSessionInfo
	b, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("claude session registry: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("claude session registry %s: %w", path, err)
	}
	return s, nil
}

// Ready is closed once the parent session has been verified.
func (p *claudeSocketPusher) Ready() <-chan struct{} { return p.ready }

// Push writes content to the session as one user message. Each push opens a
// fresh connection and closes it, so a session restart between pushes costs
// nothing. meta is unused: the socket takes a plain user message, and the
// notice text already says what to do.
func (p *claudeSocketPusher) Push(ctx context.Context, content string, meta map[string]string) error {
	if !p.verified {
		return errors.New("claude session socket: parent session not verified")
	}
	var frames []byte
	if p.token != "" {
		auth, _ := json.Marshal(map[string]string{"type": "auth", "token": p.token})
		frames = append(append(frames, auth...), '\n')
	}
	user, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]string{"role": "user", "content": content},
	})
	frames = append(append(frames, user...), '\n')

	ctx, cancel := context.WithTimeout(ctx, claudeSocketWriteTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", p.socket)
	if err != nil {
		return fmt.Errorf("claude session socket %s: %w", p.socket, err)
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	if err := c.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("claude session socket %s: %w", p.socket, err)
	}
	if _, err := c.Write(frames); err != nil {
		return fmt.Errorf("claude session socket %s: write: %w", p.socket, err)
	}
	return nil
}

// hostStatusCheck is how often the forwarder re-reads the session registry
// to catch a change between keepalives.
const hostStatusCheck = 5 * time.Second

// hostStatusReporter is the part of the relay client a hostStatusForwarder
// uses.
type hostStatusReporter interface {
	ReportHostStatus(ctx context.Context, busy bool) error
}

// hostStatusForwarder forwards the Claude Code session's own idle or busy
// state to the relay, so the roster shows the agent busy while the owner
// is chatting with it. It is structured state the host writes, never a
// read of the screen or the conversation.
type hostStatusForwarder struct {
	r hostStatusReporter
	// busy reads the session's current state; ok is false when it cannot
	// be read, and nothing is sent then.
	busy func() (busy, ok bool)
	// last is the state last reported and at when; sent is set once one
	// report has gone out.
	last bool
	at   time.Time
	sent bool
	// off is set once the relay turns out to predate host status.
	off bool
}

// newClaudeHostStatus reads the state from the registry file of ppid, the
// Claude Code process that started this server, the same file the socket
// wake and title sharing read. A file that does not name ppid reads as
// unknown.
func newClaudeHostStatus(r hostStatusReporter, getenv func(string) string, ppid int) *hostStatusForwarder {
	registry := filepath.Join(history.ClaudeConfigDir(getenv), "sessions", strconv.Itoa(ppid)+".json")
	return &hostStatusForwarder{r: r, busy: func() (bool, bool) {
		s, err := readClaudeSession(registry)
		if err != nil || s.PID != ppid {
			return false, false
		}
		return s.Status == "busy", true
	}}
}

// check reports the session's state when it changed since the last report
// or a keepalive has passed, so a long turn or a relay restart (which
// forgets every report) does not lapse to idle.
func (f *hostStatusForwarder) check(ctx context.Context, now time.Time) {
	if f.off {
		return
	}
	busy, ok := f.busy()
	if !ok || (f.sent && busy == f.last && now.Sub(f.at) < sessionKeepalive) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := f.r.ReportHostStatus(ctx, busy)
	if errors.Is(err, client.ErrHostStatusUnsupported) {
		// An older relay is not the agent's problem, so it is not logged.
		f.off = true
		return
	}
	if err != nil {
		log.Printf("tincan mcp: host status: %v", err)
		return
	}
	f.last, f.at, f.sent = busy, now, true
}

// startHostStatus checks f now and every hostStatusCheck until ctx ends.
func startHostStatus(ctx context.Context, f *hostStatusForwarder) {
	t := time.NewTicker(hostStatusCheck)
	defer t.Stop()
	for {
		f.check(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
