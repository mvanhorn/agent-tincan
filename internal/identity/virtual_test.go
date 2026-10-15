package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// Binding a name held by a real tailnet node is refused unless replace is
// set, and the refusal leaves the node binding as it was.
func TestBindVirtualRefusesRealNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	code, err := f.dir.InviteKind(ctx, "100.0.0.1:1", "claude-code", "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.Join(ctx, "100.0.0.3:1", code); err != nil {
		t.Fatal(err)
	}

	err = f.dir.BindVirtual(ctx, "claude-code", false)
	if !errors.Is(err, identity.ErrRealNodeBound) || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("bind without replace: %v", err)
	}
	if a, _, _ := f.dir.Agent(ctx, "claude-code"); a.NodeID != instinctNode.ID {
		t.Fatalf("refused bind changed the node: %+v", a)
	}

	if err := f.dir.BindVirtual(ctx, "claude-code", true); err != nil {
		t.Fatalf("bind with replace: %v", err)
	}
	a, _, _ := f.dir.Agent(ctx, "claude-code")
	if a.NodeID != identity.VirtualAddr("claude-code") || a.Kind != "claude-code" {
		t.Fatalf("after replace: %+v", a)
	}
}

// A new name binds, and a name that is already virtual rebinds without
// replace.
func TestBindVirtualNewAndAlreadyVirtual(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for range 2 {
		if err := f.dir.BindVirtual(ctx, "claude-ai", false); err != nil {
			t.Fatal(err)
		}
	}
	if a, _, _ := f.dir.Agent(ctx, "claude-ai"); a.NodeID != identity.VirtualAddr("claude-ai") {
		t.Fatalf("claude-ai: %+v", a)
	}
}
