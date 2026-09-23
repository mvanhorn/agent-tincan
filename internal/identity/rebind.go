package identity

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Rebind records an agent moved to a rebuilt machine's new node.
type Rebind struct {
	Agent       string `json:"agent"`
	OldNode     string `json:"old_node"`
	OldNodeName string `json:"old_node_name"`
	NewNode     string `json:"new_node"`
	NewNodeName string `json:"new_node_name"`
}

// Resolved is the agent behind a request, and the rebind that re-admitted
// its machine on this request, if any.
type Resolved struct {
	Name   string
	Rebind *Rebind
}

// NodeStatus is an optional Resolver extension that reports whether a node,
// by stable id, is still on the tailnet (found) and connected (online). With
// a resolver that lacks it, every old node counts as offline.
type NodeStatus interface {
	NodeOnline(ctx context.Context, stableID string) (online, found bool, err error)
}

// dedupSuffix is the "-<digits>" Tailscale appends to a machine name that is
// already taken, as when a rebuilt sandbox joins before its old node expires.
var dedupSuffix = regexp.MustCompile(`-[0-9]+$`)

// readmit re-binds an agent to a rebuilt machine: a new node whose machine
// name equals the agent's recorded one, or matches it once a dedup suffix is
// dropped from either (see sameMachine). Every condition must hold: the
// caller is untagged, it is owned by the login recorded for the agent (an
// agent with no recorded login is never re-admitted; it gains one on its
// next call from its own node), the agent's old node is offline or gone, and
// a claimed name must be the agent's. When several agents match, the claim
// picks one. The agent keeps its name, kind and requests, which are keyed by
// name. ok is false when nothing matched.
//
// The old node's status comes from the resolver (Tailscale LocalAPI IPC), so
// it is checked without holding d.mu; the rebind is then committed under
// d.mu only if the agent is still bound to the node that was checked. If it
// moved in between, the match is evaluated once more from the new state, and
// a second move reports no match.
func (d *Directory) readmit(ctx context.Context, n Node, claimed string) (Resolved, bool, error) {
	if !d.mayReadmit(n) {
		return Resolved{}, false, nil
	}
	for range 2 {
		a, res, ok, err := d.readmitCandidate(ctx, n, claimed)
		if a == nil || err != nil {
			return res, ok, err
		}
		res, committed, err := d.commitRebind(ctx, n, *a)
		if committed || err != nil {
			return res, committed, err
		}
	}
	return Resolved{}, false, nil
}

// mayReadmit reports whether n may take over agents at all.
func (d *Directory) mayReadmit(n Node) bool {
	return !d.cfg.NoAutoRebind && len(n.Tags) == 0 && !strings.HasPrefix(n.ID, VirtualPrefix)
}

// sameMachine reports whether a node named name may be the rebuilt machine
// recorded as recorded: the names are equal, or equal once a trailing
// "-<digits>" is dropped from each, so instinct, instinct-1 and instinct-2
// are one machine across rebuilds.
func sameMachine(recorded, name string) bool {
	return recorded == name || dedupSuffix.ReplaceAllString(recorded, "") == dedupSuffix.ReplaceAllString(name, "")
}

// eligible reports whether a, bound to some other node, may be re-admitted
// by n, before the claim and the old node's status are considered.
func eligible(a Agent, n Node) bool {
	return a.NodeID != n.ID && !strings.HasPrefix(a.NodeID, VirtualPrefix) &&
		a.NodeUser != "" && a.NodeUser == n.User && sameMachine(a.NodeName, n.Name)
}

// oldNodeOnline reports whether a's current node is online. A resolver
// without NodeStatus counts every old node as offline.
func (d *Directory) oldNodeOnline(ctx context.Context, a Agent) (bool, error) {
	st, ok := d.who.(NodeStatus)
	if !ok {
		return false, nil
	}
	online, found, err := st.NodeOnline(ctx, a.NodeID)
	if err != nil {
		return false, fmt.Errorf("check old node of %s: %w: %w", a.Name, ErrRebindCheckFailed, err)
	}
	return online && found, nil
}

// awaitingReadmit returns the agents n could still re-admit: eligible, with
// their old node offline or gone. It is empty when n may not re-admit.
func (d *Directory) awaitingReadmit(ctx context.Context, n Node) ([]Agent, error) {
	if !d.mayReadmit(n) {
		return nil, nil
	}
	all, err := d.store.Agents(ctx)
	if err != nil {
		return nil, err
	}
	var out []Agent
	for _, a := range all {
		if !eligible(a, n) {
			continue
		}
		online, err := d.oldNodeOnline(ctx, a)
		if err != nil {
			return nil, err
		}
		if !online {
			out = append(out, a)
		}
	}
	return out, nil
}

// readmitCandidate finds the one agent n may take over and checks that its
// old node is not online. A nil agent means the returned result is final.
func (d *Directory) readmitCandidate(ctx context.Context, n Node, claimed string) (*Agent, Resolved, bool, error) {
	all, err := d.store.Agents(ctx)
	if err != nil {
		return nil, Resolved{}, false, err
	}
	var matches []Agent
	for _, a := range all {
		if a.NodeID == n.ID {
			// A concurrent request re-admitted this node first.
			if claimed == "" || a.Name == claimed {
				return nil, Resolved{Name: a.Name}, true, nil
			}
			continue
		}
		if eligible(a, n) && (claimed == "" || a.Name == claimed) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return nil, Resolved{}, false, nil
	case 1:
	default:
		return nil, Resolved{}, false, fmt.Errorf("%s was rebuilt and ran %s; run tincan rejoin --name <agent> once per agent: %w", n.Name, strings.Join(agentNames(matches), ", "), ErrAgentAmbiguous)
	}
	a := matches[0]
	online, err := d.oldNodeOnline(ctx, a)
	if err != nil {
		return nil, Resolved{}, false, err
	}
	if online {
		return nil, Resolved{}, false, fmt.Errorf("%s: %q is still bound to %s, which is online: %w", n.Name, a.Name, a.NodeName, ErrNotJoined)
	}
	return &a, Resolved{}, false, nil
}

// commitRebind moves checked to n under d.mu, provided it is still bound to
// the node readmitCandidate checked. committed is false when it moved.
func (d *Directory) commitRebind(ctx context.Context, n Node, checked Agent) (Resolved, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, found, err := d.store.AgentByName(ctx, checked.Name)
	if err != nil {
		return Resolved{}, false, err
	}
	if !found || a.NodeID != checked.NodeID {
		return Resolved{}, false, nil
	}
	rb := &Rebind{Agent: a.Name, OldNode: a.NodeID, OldNodeName: a.NodeName, NewNode: n.ID, NewNodeName: n.Name}
	a.NodeID, a.NodeName, a.NodeUser = n.ID, n.Name, n.User
	if err := d.store.PutAgent(ctx, a); err != nil {
		return Resolved{}, false, err
	}
	return Resolved{Name: a.Name, Rebind: rb}, true, nil
}
