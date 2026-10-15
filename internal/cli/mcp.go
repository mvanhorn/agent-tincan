package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
)

func mcpCmd() *cobra.Command {
	var channel bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the Agent Tincan MCP server over stdio (add this to your agent's MCP config)",
		Long: `Run the Agent Tincan MCP server over stdio.

With --channel it also acts as a Claude Code channel: it watches the relay
without taking anything and pushes a short notice into the running Claude
Code session when teammate requests, or replies to its own requests, are
waiting. The notice carries only a count and senders; the model takes the
items by calling check_inbox, so a session that drops the notice leaves them
for another session or a later check. Start Claude Code with:

  claude --dangerously-load-development-channels server:agent-tincan

(channels are a research preview; custom channels need that flag).`,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			// Record the launch before anything can fail, so tincan doctor
			// can tell an app that never starts tincan from one that does.
			rec := mcpserver.StartRecorder(mcpserver.LaunchDir(client.ConfigPath()), Version, channel)
			defer func() { rec.End(err) }()
			r, cfg, err := connect()
			if err != nil {
				return err
			}
			if r.Session() == "" {
				// One Claude Code session runs one MCP server, so a fresh
				// id per process gives each session the replies to its own
				// asks. One-shot CLI commands name no session and see all.
				r.SetSession(newSession())
			}
			files := mcpserver.LocalFiles(client.AttachmentDir(cfg))
			watch := mcpserver.Watch(mcpserver.NewBuildWatch(ownBinary(), Version))
			if !channel {
				return mcpserver.New(r, Version, files, watch).Run(cmd.Context(), mcpserver.StdioRecorded(rec))
			}
			t := mcpserver.NewChannelTransport(mcpserver.StdioRecorded(rec))
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			go pushWaiting(ctx, r, t)
			return mcpserver.NewWithOptions(r, Version, mcpserver.ChannelOptions(), files, watch).Run(ctx, t)
		},
	}
	cmd.Flags().BoolVar(&channel, "channel", false, "announce waiting teammate requests and replies in a running Claude Code session (channels preview)")
	return cmd
}

// newSession returns a random session id for one MCP server process.
func newSession() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "mcp-" + hex.EncodeToString(b)
}

// ownBinary is the path of the running tincan binary with symlinks
// resolved, or "" when it cannot be found. It is read at startup: on Linux
// the path is gone from /proc/self/exe once an upgrade renames a new file
// over it.
func ownBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}

// channelPusher is the part of the channel transport pushWaiting uses.
type channelPusher interface {
	Ready() <-chan struct{}
	Push(ctx context.Context, content string, meta map[string]string) error
}

// waitingRecheck is how long pushWaiting waits before looking again while
// something is still waiting, since a peek returns at once until the agent
// takes it.
var waitingRecheck = 15 * time.Second

// reannounceAfter is how long pushWaiting stays quiet about items it already
// announced that are still waiting, so a push the session dropped (idle, or
// started without channels) is not the last word.
var reannounceAfter = 10 * time.Minute

// pushWaiting announces teammate requests and replies to this agent's own
// requests with a short channel notice. It only peeks, so it never claims a
// request or marks a reply seen: the model takes them by calling check_inbox.
// Pings are the exception: it claims and answers them without a notice.
// Every open Claude Code session runs one of these; the first model to call
// check_inbox gets the items and the others find an empty inbox. An item is
// announced once per process (an id counts only after a push carrying it
// succeeds), and again after reannounceAfter if it is still waiting. Relay
// errors back off with jitter.
func pushWaiting(ctx context.Context, r *client.Relay, t channelPusher) {
	select {
	case <-ctx.Done():
		return
	case <-t.Ready():
	}
	var a announcer
	// Peeks advertise ping support, so this loop answers pings itself, as
	// listen does; failed pongs retry in the background.
	var retries client.PongRetries
	defer retries.Wait()
	backoff := time.Second
	for ctx.Err() == nil {
		w, err := r.Peek(ctx, client.DefaultPollHold)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("tincan channel: %v", client.RejoinHint(err, r.Base()))
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
		// Channels are Claude Code's, so the reload step is its.
		if err := client.ReportUpgradeReload(client.UpgradeSurfaceChannel, w.UpgradeAvailable, mcpserver.ReloadStep(mcpserver.HostClaudeCode), func(line string) error {
			return t.Push(ctx, line, map[string]string{"kind": "upgrade", "upgrade_available": w.UpgradeAvailable})
		}); err != nil {
			log.Printf("tincan channel: push upgrade: %v", err)
		}
		if n, ok := a.next(w, time.Now()); ok {
			if err := t.Push(ctx, n.content, n.meta); err != nil {
				log.Printf("tincan channel: push: %v", err)
			} else {
				a.pushed(n, time.Now())
			}
		}
		if w.Total > 0 {
			sleepCtx(ctx, waitingRecheck)
		}
	}
}

// answerChannelPings answers the pings a peek lists and returns the peek
// without them, so they never become a channel notice, and whether any
// ping failed.
func answerChannelPings(ctx context.Context, r client.PingResponder, w client.Waiting, retries *client.PongRetries) (client.Waiting, bool) {
	failed := false
	rest := w.Pending[:0:0]
	for _, p := range w.Pending {
		if p.Kind != envelope.KindPing {
			rest = append(rest, p)
			continue
		}
		_, retry, err := client.AnswerPings(ctx, r, client.Inbox{Requests: []envelope.Request{{ID: p.ID, Kind: envelope.KindPing}}}, "channel")
		if err != nil {
			failed = true
		}
		retries.Go(ctx, retry)
	}
	w.Pending = rest
	w.Total = max(w.Total-w.Pings, 0)
	w.Queued = max(w.Queued-w.Pings, 0)
	w.Pings = 0
	return w, failed
}

// announcer decides when a peek is worth a channel notice.
type announcer struct {
	seen map[string]bool // item keys announced and still waiting
	last time.Time       // last successful push
	// urgent is the urgent count last announced, lowered as urgent requests
	// are taken. A rise means a new urgent request even when it sits past
	// the capped pending list, so its id is not there to be seen.
	urgent int
}

type notice struct {
	content string
	meta    map[string]string
	keys    []string
	urgent  int
}

// next returns the notice for what w shows waiting, and whether to push it:
// only when some item is new, the urgent count rose, or all were announced
// over reannounceAfter ago.
func (a *announcer) next(w client.Waiting, now time.Time) (notice, bool) {
	var keys, ids, from []string
	urgent := 0
	senders := map[string]bool{}
	addFrom := func(name string) {
		if name != "" && !senders[name] {
			senders[name] = true
			from = append(from, name)
		}
	}
	for _, p := range w.Pending {
		if p.Urgent {
			urgent++
		}
		keys, ids = append(keys, "q:"+p.ID), append(ids, p.ID)
		addFrom(p.From)
	}
	urgent = max(urgent, w.Urgent)
	if len(w.Pending) == 0 && w.Queued > 0 {
		// A relay that predates pending gives only a count.
		keys = append(keys, fmt.Sprintf("q#%d", w.Queued))
	}
	for _, rep := range w.Replies {
		keys, ids = append(keys, "r:"+rep.Request.ID), append(ids, rep.Request.ID)
		addFrom(rep.Request.To)
	}
	fresh := false
	still := map[string]bool{}
	for _, k := range keys {
		if a.seen[k] {
			still[k] = true
		} else {
			fresh = true
		}
	}
	a.seen = still // forget items someone has since taken
	a.urgent = min(a.urgent, urgent)
	if urgent > a.urgent {
		fresh = true
	}
	if len(keys) == 0 || (!fresh && now.Sub(a.last) < reannounceAfter) {
		return notice{}, false
	}
	count := max(w.Total, len(keys))
	kind := "request"
	switch {
	case len(w.Replies) > 0 && w.Queued > 0:
		kind = "mixed"
	case len(w.Replies) > 0:
		kind = "reply"
	}
	content := channelNotice(count, from)
	if urgent > 0 {
		noun := "requests"
		if urgent == 1 {
			noun = "request"
		}
		content = fmt.Sprintf("%d urgent %s waiting. %s", urgent, noun, content)
	}
	return notice{
		content: content,
		meta: map[string]string{
			"kind":        kind,
			"count":       strconv.Itoa(count),
			"from":        strings.Join(from, ","),
			"request_ids": strings.Join(ids, ","),
		},
		keys:   keys,
		urgent: urgent,
	}, true
}

// pushed records a notice the session received.
func (a *announcer) pushed(n notice, now time.Time) {
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	for _, k := range n.keys {
		a.seen[k] = true
	}
	a.urgent = n.urgent
	a.last = now
}

// channelNotice is the text of a channel notice. It carries counts and
// senders only; the model reads the items through check_inbox.
func channelNotice(count int, from []string) string {
	items, them := "items", "them"
	if count == 1 {
		items, them = "item", "it"
	}
	who := ""
	if len(from) > 0 {
		who = " from " + strings.Join(from, ", ")
	}
	return fmt.Sprintf("%d Agent Tincan %s waiting%s. Call check_inbox to take %s, then reply to each request.", count, items, who, them)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
