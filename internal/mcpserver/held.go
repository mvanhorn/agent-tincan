package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// HeldReporter is the part of a Backend that reports the claimed asks the
// relay said this agent holds. *client.Relay implements it.
type HeldReporter interface {
	Held() ([]envelope.Held, uint64)
}

// heldMiddleware puts the held-work line before every tool result, when a
// relay call made during that tool call reported claimed asks this agent
// has not replied to. A tool that made no relay call shows nothing, so an
// old list is never repeated.
func heldMiddleware(h HeldReporter) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			_, before := h.Held()
			res, err := next(ctx, method, req)
			if err != nil {
				return res, err
			}
			held, after := h.Held()
			if r, ok := res.(*mcp.CallToolResult); ok && r != nil && after != before {
				if n := client.FormatHeld(held, time.Now()); n != "" {
					r.Content = append([]mcp.Content{&mcp.TextContent{Text: n}}, r.Content...)
				}
			}
			return res, err
		}
	}
}
