package cli

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/history"
)

// workingOnReporter is the part of the relay client a titleSharer uses.
type workingOnReporter interface {
	ReportWorkingOn(ctx context.Context, text string) error
	ClearWorkingOn(ctx context.Context) error
}

// titleSharer reports the Claude Code session's title as this MCP server's
// working-on line on the roster (tincan mcp --share-title). It reads only
// only the session's title records, not its prompts; the title itself may
// still echo a prompt when a hook names sessions from their first prompt.
type titleSharer struct {
	r workingOnReporter
	// sessionID is the Claude Code session this server runs in, read on
	// every refresh because /clear or a resume moves the window to a new
	// session id.
	sessionID func() string
	title     func(sessionID string) string
	// reported is set while the relay may hold a line from this server, so
	// an empty title or a clean exit knows to remove it.
	reported bool
	// off is set once the relay turns out to predate working-on lines; the
	// sharer then sends nothing more for the life of the process.
	off bool
}

// newClaudeTitleSharer reads titles from the Claude Code config dir named
// by getenv. The session id comes from the registry file of ppid, the
// Claude Code process that started this server, which the socket wake also
// reads; without one it falls back to CLAUDE_CODE_SESSION_ID.
func newClaudeTitleSharer(r workingOnReporter, getenv func(string) string, ppid int) *titleSharer {
	dir := history.ClaudeConfigDir(getenv)
	registry := claudeRegistryPath(getenv, ppid)
	cc := &history.ClaudeCode{Root: filepath.Join(dir, "projects")}
	return &titleSharer{
		r: r,
		sessionID: func() string {
			if s, err := readClaudeSession(registry); err == nil && s.PID == ppid && s.SessionID != "" {
				return s.SessionID
			}
			return getenv("CLAUDE_CODE_SESSION_ID")
		},
		title: cc.SessionTitle,
	}
}

// refresh reports the current title, or removes the line when the session
// has no title. An unchanged title is reported again, which is what keeps
// the relay from letting it lapse.
func (s *titleSharer) refresh(ctx context.Context) {
	if s.off {
		return
	}
	title := rosterTitle(s.title(s.sessionID()))
	if title == "" {
		if s.reported {
			s.clear(ctx)
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := s.r.ReportWorkingOn(ctx, title)
	if s.unsupported(err) {
		return
	}
	if err != nil {
		log.Printf("tincan mcp: share title: %v", err)
		return
	}
	s.reported = true
}

// rosterTitleRunes is the relay's limit on a roster line (the good_at cap).
const rosterTitleRunes = 120

// rosterTitle makes a session title into a line the relay accepts: valid
// UTF-8, line breaks and other control characters turned into spaces,
// invisible format characters dropped, runs of space collapsed, and at
// most rosterTitleRunes characters. A /rename title can break any of
// those, and the relay refuses the whole line rather than trimming it.
func rosterTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cf, r):
			return -1
		case unicode.IsControl(r):
			return ' '
		}
		return r
	}, strings.ToValidUTF8(title, ""))
	title = strings.Join(strings.Fields(title), " ")
	if r := []rune(title); len(r) > rosterTitleRunes {
		title = strings.TrimSpace(string(r[:rosterTitleRunes]))
	}
	return title
}

// clear removes this server's line from the relay, if it may hold one.
func (s *titleSharer) clear(ctx context.Context) {
	if s.off || !s.reported {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.r.ClearWorkingOn(ctx)
	if s.unsupported(err) {
		return
	}
	if err != nil {
		log.Printf("tincan mcp: share title: %v", err)
		return
	}
	s.reported = false
}

// unsupported turns the sharer off when err says the relay predates
// working-on lines. An older relay is not the agent's problem, so it is
// not logged.
func (s *titleSharer) unsupported(err error) bool {
	if errors.Is(err, client.ErrWorkingOnUnsupported) {
		s.off = true
		return true
	}
	return false
}

// startTitleSharing refreshes s now and every interval until stop is
// called or ctx ends. stop waits for the loop, then removes the line, so a
// clean exit takes the title off the roster at once instead of after the
// relay's session grace. It runs on its own ticker because the channel and
// socket wake loops have no fixed tick of their own to share.
func startTitleSharing(ctx context.Context, s *titleSharer, every time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			s.refresh(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
		s.clear(context.Background())
	}
}
