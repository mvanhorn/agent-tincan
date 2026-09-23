// Package identitytest provides a fake WhoIs resolver for tests. Production
// code never imports it.
package identitytest

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// Resolver maps remote addresses to nodes. It also answers NodeOnline: a
// node is on the tailnet while some address maps to it or SetOnline named
// it, and online only after SetOnline(id, true).
type Resolver struct {
	mu     sync.Mutex
	nodes  map[string]identity.Node
	online map[string]bool
}

// New returns a Resolver seeded with addr -> node.
func New(nodes map[string]identity.Node) *Resolver {
	cp := make(map[string]identity.Node, len(nodes))
	maps.Copy(cp, nodes)
	return &Resolver{nodes: cp, online: map[string]bool{}}
}

// Set maps addr to node.
func (r *Resolver) Set(addr string, n identity.Node) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[addr] = n
}

// Remove drops addr, as if its node left the tailnet.
func (r *Resolver) Remove(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nodes, addr)
}

// SetOnline marks the node with stable id as on the tailnet and online or
// offline.
func (r *Resolver) SetOnline(id string, online bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.online[id] = online
}

// NodeOnline implements identity.NodeStatus.
func (r *Resolver) NodeOnline(_ context.Context, id string) (online, found bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on, ok := r.online[id]; ok {
		return on, true, nil
	}
	for _, n := range r.nodes {
		if n.ID == id {
			return false, true, nil
		}
	}
	return false, false, nil
}

// WhoIs implements identity.Resolver.
func (r *Resolver) WhoIs(_ context.Context, addr string) (identity.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[addr]
	if !ok {
		return identity.Node{}, fmt.Errorf("whois %s: not a tailnet peer", addr)
	}
	return n, nil
}
