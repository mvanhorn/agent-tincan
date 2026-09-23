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
// name equals the agent's recorded one, or equals it after dropping a dedup
// suffix. Every condition must hold: the caller is untagged, it is owned by
// the login recorded at join (when one was recorded), the agent's old node
// is offline or gone, and a claimed name must be the agent's. When several
// agents match, the claim picks one. The agent keeps its name, kind and
// requests, which are keyed by name. ok is false when nothing matched.
func (d *Directory) readmit(ctx context.Context, n Node, claimed string) (Resolved, bool, error) {
	if d.cfg.NoAutoRebind || len(n.Tags) > 0 || strings.HasPrefix(n.ID, VirtualPrefix) {
		return Resolved{}, false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	all, err := d.store.Agents(ctx)
	if err != nil {
		return Resolved{}, false, err
	}
	base := dedupSuffix.ReplaceAllString(n.Name, "")
	var matches []Agent
	for _, a := range all {
		if a.NodeID == n.ID {
			// A concurrent request re-admitted this node first.
			if claimed == "" || a.Name == claimed {
				return Resolved{Name: a.Name}, true, nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(a.NodeID, VirtualPrefix),
			a.NodeName != n.Name && a.NodeName != base,
			a.NodeUser != "" && a.NodeUser != n.User,
			claimed != "" && a.Name != claimed:
			continue
		}
		matches = append(matches, a)
	}
	switch len(matches) {
	case 0:
		return Resolved{}, false, nil
	case 1:
	default:
		names := make([]string, len(matches))
		for i, a := range matches {
			names[i] = a.Name
		}
		return Resolved{}, false, fmt.Errorf("%s was rebuilt and ran %s; run tincan rejoin --name <agent> once per agent: %w", n.Name, strings.Join(names, ", "), ErrAgentAmbiguous)
	}
	a := matches[0]
	if st, ok := d.who.(NodeStatus); ok {
		online, found, err := st.NodeOnline(ctx, a.NodeID)
		if err != nil {
			return Resolved{}, false, fmt.Errorf("check old node of %s: %w", a.Name, err)
		}
		if online && found {
			return Resolved{}, false, fmt.Errorf("%s: %q is still bound to %s, which is online: %w", n.Name, a.Name, a.NodeName, ErrNotJoined)
		}
	}
	rb := &Rebind{Agent: a.Name, OldNode: a.NodeID, OldNodeName: a.NodeName, NewNode: n.ID, NewNodeName: n.Name}
	a.NodeID, a.NodeName, a.NodeUser = n.ID, n.Name, n.User
	if err := d.store.PutAgent(ctx, a); err != nil {
		return Resolved{}, false, err
	}
	return Resolved{Name: a.Name, Rebind: rb}, true, nil
}
