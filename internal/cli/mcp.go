package cli

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

func mcpCmd() *cobra.Command {
	var channel bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the Agent Tincan MCP server over stdio (add this to your agent's MCP config)",
		Long: `Run the Agent Tincan MCP server over stdio.

With --channel it also acts as a Claude Code channel: it holds a long-poll to
the relay and pushes each teammate request straight into the running Claude
Code session, and a short notice when a reply to one of its own requests
arrives (check_inbox then shows it). Start Claude Code with:

  claude --dangerously-load-development-channels server:agent-tincan

(channels are a research preview; custom channels need that flag).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			if !channel {
				return mcpserver.New(r, Version).Run(cmd.Context(), &mcp.StdioTransport{})
			}
			t := mcpserver.NewChannelTransport(&mcp.StdioTransport{})
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			go pushRequests(ctx, r, t)
			go pushReplies(ctx, r, t)
			return mcpserver.NewWithOptions(r, Version, mcpserver.ChannelOptions()).Run(ctx, t)
		},
	}
	cmd.Flags().BoolVar(&channel, "channel", false, "push teammate requests into a running Claude Code session (channels preview)")
	return cmd
}

// pushRequests waits for requests, claims them, and pushes each into the
// Claude Code session as a channel event. Its polls leave replies alone, so
// they stay unseen for check_inbox; pushReplies announces them.
func pushRequests(ctx context.Context, r *client.Relay, t *mcpserver.ChannelTransport) {
	select {
	case <-ctx.Done():
		return
	case <-t.Ready():
	}
	for ctx.Err() == nil {
		in, err := waitForInbox(ctx, r, client.DefaultPollHold, client.RepliesNone)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("tincan channel: %v", client.RejoinHint(err, r.Base()))
				time.Sleep(30 * time.Second)
			}
			continue
		}
		for _, req := range in.Requests {
			if _, err := r.Claim(ctx, req.ID); err != nil {
				log.Printf("tincan channel: claim %s: %v", req.ID, err)
				continue
			}
			meta := map[string]string{"from": req.From, "request_id": req.ID, "trace_id": req.TraceID}
			if err := t.Push(ctx, client.FormatRequest(req), meta); err != nil {
				log.Printf("tincan channel: push %s: %v", req.ID, err)
			}
		}
	}
}

// channelPusher is the part of the channel transport pushReplies uses.
type channelPusher interface {
	Ready() <-chan struct{}
	Push(ctx context.Context, content string, meta map[string]string) error
}

// replyRecheck is how long pushReplies waits before looking again while
// something is still waiting, since a peek returns at once until the agent
// reads it.
var replyRecheck = 15 * time.Second

// pushReplies pushes a short count-only channel event when replies to this
// agent's own requests arrive. It only peeks, so the replies stay unseen
// until the agent calls check_inbox, and it pushes each reply once: an id
// counts as pushed only after a push carrying it succeeds, so a failed push
// is tried again on a later look. Relay errors back off with jitter, as the
// request loop does.
func pushReplies(ctx context.Context, r *client.Relay, t channelPusher) {
	select {
	case <-ctx.Done():
		return
	case <-t.Ready():
	}
	pushed := map[string]bool{}
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
		backoff = time.Second
		var fresh []string
		for _, rep := range w.Replies {
			if id := rep.Request.ID; !pushed[id] {
				fresh = append(fresh, id)
			}
		}
		// Forget replies check_inbox has since marked seen.
		still := map[string]bool{}
		for _, rep := range w.Replies {
			if pushed[rep.Request.ID] {
				still[rep.Request.ID] = true
			}
		}
		pushed = still
		if len(fresh) > 0 {
			meta := map[string]string{"kind": "reply", "request_ids": strings.Join(fresh, ",")}
			if err := t.Push(ctx, wake.WaitingMessage(0, len(fresh)), meta); err != nil {
				log.Printf("tincan channel: push replies: %v", err)
			} else {
				for _, id := range fresh {
					pushed[id] = true
				}
			}
		}
		if w.Total > 0 {
			sleepCtx(ctx, replyRecheck)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
