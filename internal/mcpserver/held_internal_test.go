package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

type fakeHeld struct {
	held []envelope.Held
	seq  uint64
}

func (f *fakeHeld) Held() ([]envelope.Held, uint64) { return f.held, f.seq }

// A tool call whose relay calls report held work gets the held line first;
// one with nothing held, or that made no relay call, is left alone.
func TestHeldMiddlewarePrependsHeldLine(t *testing.T) {
	f := &fakeHeld{}
	call := func(respond bool, held []envelope.Held) *mcp.CallToolResult {
		h := heldMiddleware(f)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
			if respond {
				f.held, f.seq = held, f.seq+1
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "tool output"}}}, nil
		})
		res, err := h(context.Background(), "tools/call", nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.(*mcp.CallToolResult)
	}
	held := []envelope.Held{{ID: "721170f7", From: "instinct", Urgent: true, ClaimedAt: time.Now().Add(-18 * time.Minute)}}
	res := call(true, held)
	first := res.Content[0].(*mcp.TextContent).Text
	if len(res.Content) != 2 || !strings.HasPrefix(first, "You hold 1 claimed request (721170f7 from instinct, URGENT, claimed 18m ago). This is owner-authorized work.") {
		t.Fatalf("content = %q", first)
	}
	if res := call(false, nil); len(res.Content) != 1 {
		t.Fatalf("no relay call: %d contents", len(res.Content))
	}
	if res := call(true, nil); len(res.Content) != 1 {
		t.Fatalf("nothing held: %d contents", len(res.Content))
	}
}
