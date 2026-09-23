package cli

import (
	"context"
	"log"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
)

func mcpCmd() *cobra.Command {
	var channel bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the Agent Tincan MCP server over stdio (add this to your agent's MCP config)",
		Long: `Run the Agent Tincan MCP server over stdio.

With --channel it also acts as a Claude Code channel: it holds a long-poll to
the relay and pushes each teammate request straight into the running Claude
Code session. Start Claude Code with:

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
			return mcpserver.NewWithOptions(r, Version, mcpserver.ChannelOptions()).Run(ctx, t)
		},
	}
	cmd.Flags().BoolVar(&channel, "channel", false, "push teammate requests into a running Claude Code session (channels preview)")
	return cmd
}

// pushRequests waits for requests, claims them, and pushes each into the
// Claude Code session as a channel event.
func pushRequests(ctx context.Context, r *client.Relay, t *mcpserver.ChannelTransport) {
	select {
	case <-ctx.Done():
		return
	case <-t.Ready():
	}
	for ctx.Err() == nil {
		reqs, err := waitForRequests(ctx, r, client.DefaultPollHold)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("tincan channel: %v", client.RejoinHint(err, r.Base()))
				time.Sleep(30 * time.Second)
			}
			continue
		}
		for _, req := range reqs {
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
