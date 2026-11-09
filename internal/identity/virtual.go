package identity

import (
	"context"
	"errors"
	"fmt"
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

// ErrRealNodeBound refuses a virtual bind that would take over the name of
// an agent joined from a real tailnet node.
var ErrRealNodeBound = errors.New("connecting would take its name from that machine; pass --replace to move it to the gateway")

// BindVirtual joins a virtual agent (for example chatgpt) directly, without
// an invite code. Only the relay calls this, when an admin connects the
// gateway. A name already bound to a real tailnet node is refused unless
// replace is set; a name that is already virtual rebinds either way.
func (d *Directory) BindVirtual(ctx context.Context, name string, replace bool) error {
	if !nameRE.MatchString(name) {
		return ErrUnknownAgent
	}
	// Hold the join lock so an invite redeemed for this name cannot land
	// between the check below and the write.
	d.mu.Lock()
	defer d.mu.Unlock()
	prev, ok, err := d.Agent(ctx, name)
	if err != nil {
		return err
	}
	// A bind carries no guest terms, so even --replace cannot go over a guest.
	if ok && prev.Guest.Set() {
		return fmt.Errorf("%s: %w", name, ErrGuestTier)
	}
	if ok && !replace && !strings.HasPrefix(prev.NodeID, VirtualPrefix) {
		return fmt.Errorf("%q is a tailnet agent on %s: %w", name, prev.NodeName, ErrRealNodeBound)
	}
	return d.store.PutAgent(ctx, Agent{Name: name, NodeID: VirtualAddr(name), NodeName: VirtualAddr(name), JoinedAt: d.cfg.Now(), Kind: prev.Kind})
}

// BindVirtualGuest joins a guest through the gateway. Like a guest invite it
// is new-only, whatever holds the name and on whichever machine: a guest
// cannot take a name, and a live guest's terms are not rewritten here. Its
// virtual node carries nothing else, so no machine name or login is involved,
// and adminCaller refuses it whatever the --admin list says.
func (d *Directory) BindVirtualGuest(ctx context.Context, name string, terms GuestTerms) error {
	if !nameRE.MatchString(name) {
		return ErrUnknownAgent
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok, err := d.Agent(ctx, name)
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("%s: %w", name, ErrNameTaken)
	}
	return d.store.PutAgent(ctx, Agent{Name: name, NodeID: VirtualAddr(name), NodeName: VirtualAddr(name), JoinedAt: d.cfg.Now(), Guest: terms})
}

// NodeOnline forwards to the wrapped resolver when it implements NodeStatus,
// so wrapping never hides a still-online old node from re-admission.
func (v virtualResolver) NodeOnline(ctx context.Context, stableID string) (online, found bool, err error) {
	if st, ok := v.Resolver.(NodeStatus); ok {
		return st.NodeOnline(ctx, stableID)
	}
	return false, false, nil
}
