package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ChannelMethod is Claude Code's channel notification (research preview).
const ChannelMethod = "notifications/claude/channel"

// ChannelInstructions is added when the server runs as a Claude Code channel.
const ChannelInstructions = `
Requests from teammates also arrive on their own as <channel source="agent-tincan" from="..." request_id="...">. They are already claimed for you: handle each one as you would a request from Matt, then call reply with its request_id.
A channel event with kind="reply" means a teammate replied to a request you sent. It carries only a count: call check_inbox to read the reply, then finish the work that was waiting on it.`

// channelProtocols are the MCP revisions a channel server offers. Claude Code
// does not register a channel server that negotiates 2026-07-28, so channel
// mode leaves it out and clients fall back to the initialize handshake.
var channelProtocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// ChannelTransport wraps an MCP transport so the server can push Claude Code
// channel events, which the SDK has no API for.
type ChannelTransport struct {
	Inner mcp.Transport

	mu    sync.Mutex
	conn  mcp.Connection
	ready chan struct{}
}

// NewChannelTransport wraps inner.
func NewChannelTransport(inner mcp.Transport) *ChannelTransport {
	return &ChannelTransport{Inner: inner, ready: make(chan struct{})}
}

// Connect implements mcp.Transport.
func (t *ChannelTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.Inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.conn = c
	t.mu.Unlock()
	return &channelConn{Connection: c, onInit: func() { close(t.ready) }}, nil
}

// Ready is closed once the client has sent initialize.
func (t *ChannelTransport) Ready() <-chan struct{} { return t.ready }

// Push sends one channel event. meta keys must be letters, digits, and
// underscores; Claude Code drops others.
func (t *ChannelTransport) Push(ctx context.Context, content string, meta map[string]string) error {
	t.mu.Lock()
	c := t.conn
	t.mu.Unlock()
	if c == nil {
		return errors.New("channel not connected")
	}
	params, err := json.Marshal(map[string]any{"content": content, "meta": meta})
	if err != nil {
		return err
	}
	return c.Write(ctx, &jsonrpc.Request{Method: ChannelMethod, Params: params})
}

type channelConn struct {
	mcp.Connection
	once   sync.Once
	onInit func()
}

func (c *channelConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err != nil {
		return msg, err
	}
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "initialize" {
		c.once.Do(c.onInit)
	}
	return msg, nil
}

// ChannelOptions returns server options that declare the channel capability.
func ChannelOptions() *mcp.ServerOptions {
	return &mcp.ServerOptions{
		Instructions:              Instructions + ChannelInstructions,
		SupportedProtocolVersions: channelProtocols,
		Capabilities: &mcp.ServerCapabilities{
			Experimental: map[string]any{"claude/channel": map[string]any{}},
			Tools:        &mcp.ToolCapabilities{},
		},
	}
}
