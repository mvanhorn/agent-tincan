package identity

import (
	"context"
	"strings"
)

// VirtualPrefix marks callers that are not tailnet machines, such as ChatGPT
// arriving through the OAuth-protected gateway. The gateway sets this as the
// remote address on requests it forwards in-process after checking a token.
// Real connections always have an IP:port remote address, so they can never
// carry this prefix.
const VirtualPrefix = "virtual:"

// VirtualAddr is the remote address the gateway uses for agent name.
func VirtualAddr(name string) string { return VirtualPrefix + name }

// WithVirtual wraps a resolver so gateway-forwarded callers resolve to a
// stable virtual node for their agent name.
func WithVirtual(r Resolver) Resolver { return virtualResolver{r} }

type virtualResolver struct{ Resolver }

func (v virtualResolver) WhoIs(ctx context.Context, addr string) (Node, error) {
	if name, ok := strings.CutPrefix(addr, VirtualPrefix); ok {
		return Node{ID: VirtualAddr(name), Name: VirtualAddr(name)}, nil
	}
	return v.Resolver.WhoIs(ctx, addr)
}

// BindVirtual joins a virtual agent (for example chatgpt) directly, without
// an invite code. Only the relay calls this, when an admin connects the
// gateway.
func (d *Directory) BindVirtual(ctx context.Context, name string) error {
	if !nameRE.MatchString(name) {
		return ErrUnknownAgent
	}
	prev, _, err := d.Agent(ctx, name)
	if err != nil {
		return err
	}
	return d.store.PutAgent(ctx, Agent{Name: name, NodeID: VirtualAddr(name), NodeName: VirtualAddr(name), JoinedAt: d.cfg.Now(), Kind: prev.Kind})
}

// NodeOnline forwards to the wrapped resolver when it implements NodeStatus,
// so wrapping never hides a still-online old node from re-admission.
func (v virtualResolver) NodeOnline(ctx context.Context, stableID string) (online, found bool, err error) {
	if st, ok := v.Resolver.(NodeStatus); ok {
		return st.NodeOnline(ctx, stableID)
	}
	return false, false, nil
}
