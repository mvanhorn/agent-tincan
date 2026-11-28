package identity

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store for tests and ephemeral runs.
type MemoryStore struct {
	mu      sync.Mutex
	agents  map[string]Agent
	invites map[string]Invite
	joins   map[string]JoinRequest
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{agents: map[string]Agent{}, invites: map[string]Invite{}, joins: map[string]JoinRequest{}}
}

func (m *MemoryStore) PutAgent(_ context.Context, a Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Only SetAgentGoodAt and SetAgentFallback write those; a rewrite keeps
	// them. The task host mark stays only on a row that carries it and
	// leaves the agent on the node it had, and is never set here.
	old := m.agents[a.Name]
	a.GoodAt, a.Fallback = old.GoodAt, old.Fallback
	a.TaskHost = a.TaskHost && old.TaskHost && old.NodeID == a.NodeID
	m.agents[a.Name] = a
	return nil
}

func (m *MemoryStore) DeleteAgent(_ context.Context, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.agents[name]
	delete(m.agents, name)
	for n, a := range m.agents {
		if a.Fallback == name {
			a.Fallback = ""
			m.agents[n] = a
		}
	}
	return ok, nil
}

func (m *MemoryStore) Agents(_ context.Context) ([]Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Agent, 0, len(m.agents))
	for _, a := range m.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemoryStore) AgentsByNode(_ context.Context, nodeID string) ([]Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Agent
	for _, a := range m.agents {
		if a.NodeID == nodeID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemoryStore) SetAgentKind(_ context.Context, name, kind string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[name]
	if ok {
		a.Kind = kind
		m.agents[name] = a
	}
	return ok, nil
}

func (m *MemoryStore) SetAgentGoodAt(_ context.Context, name, line string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[name]
	if ok {
		a.GoodAt = line
		m.agents[name] = a
	}
	return ok, nil
}

func (m *MemoryStore) AgentByName(_ context.Context, name string) (Agent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[name]
	return a, ok, nil
}

func (m *MemoryStore) PutInvite(_ context.Context, inv Invite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for code, old := range m.invites {
		if old.Name == inv.Name {
			delete(m.invites, code)
		}
	}
	m.invites[inv.Code] = inv
	return nil
}

func (m *MemoryStore) TakeInvite(_ context.Context, code string) (Invite, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[code]
	delete(m.invites, code)
	return inv, ok, nil
}

func (m *MemoryStore) SetAgentFallback(_ context.Context, name, to string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[name]
	if ok {
		a.Fallback = to
		m.agents[name] = a
	}
	return ok, nil
}

func (m *MemoryStore) SetAgentTaskHost(_ context.Context, name string, on bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[name]
	if ok {
		a.TaskHost = on
		m.agents[name] = a
	}
	return ok, nil
}

func (m *MemoryStore) LiveInviteNamed(_ context.Context, name string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inv := range m.invites {
		if inv.Name == name && !now.After(inv.Expires) {
			return true, nil
		}
	}
	return false, nil
}

// InsertJoinRequest implements JoinRequestStore with the same caps as the
// SQLite store.
func (m *MemoryStore) InsertJoinRequest(_ context.Context, r JoinRequest, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	open, hour, pending := 0, 0, 0
	for _, o := range m.joins {
		o = o.AsOf(now)
		if o.NodeID == r.NodeID && o.Open() {
			open++
		}
		if o.NodeID == r.NodeID && o.Filed.After(now.Add(-time.Hour)) {
			hour++
		}
		if o.Status == JoinPending {
			pending++
		}
	}
	if err := JoinCapRefusal(open, hour, pending); err != nil {
		return err
	}
	r.Agents = slices.Clone(r.Agents)
	m.joins[r.ID] = r
	return nil
}

func (m *MemoryStore) JoinRequest(_ context.Context, id string, now time.Time) (JoinRequest, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.joins[id]
	return r.AsOf(now), ok, nil
}

func (m *MemoryStore) PendingJoinRequests(_ context.Context, now time.Time) ([]JoinRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []JoinRequest
	for _, r := range m.joins {
		if r = r.AsOf(now); r.Status == JoinPending {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filed.Before(out[j].Filed) })
	return out, nil
}

// moveJoin moves request id to status to when CanMoveTo allows it, applying
// set to the request first. It returns the request as decided and stores it
// slim when to is final.
func (m *MemoryStore) moveJoin(id, to string, now time.Time, set func(*JoinRequest)) (JoinRequest, error) {
	r, ok := m.joins[id]
	if !ok {
		return JoinRequest{}, ErrJoinNotFound
	}
	r = r.AsOf(now)
	if !r.CanMoveTo(to) {
		return r, ErrJoinState
	}
	r.Status = to
	if set != nil {
		set(&r)
	}
	if !r.Open() {
		r.Closed = now
	}
	m.joins[id] = slimJoin(r)
	return r, nil
}

// slimJoin is r as stored: without names or code once final.
func slimJoin(r JoinRequest) JoinRequest {
	if !r.Open() {
		r.Agents, r.Code = nil, ""
	}
	return r
}

func (m *MemoryStore) ApproveJoinRequest(_ context.Context, id, by string, now time.Time) (JoinRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.moveJoin(id, JoinApproved, now, func(r *JoinRequest) { r.Decided, r.DecidedBy = now, by })
}

func (m *MemoryStore) DenyJoinRequest(_ context.Context, id, by string, now time.Time) (JoinRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.moveJoin(id, JoinDenied, now, func(r *JoinRequest) { r.Decided, r.DecidedBy = now, by })
}

func (m *MemoryStore) VoidJoinRequest(_ context.Context, id string, now time.Time) (JoinRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.moveJoin(id, JoinVoided, now, nil)
}

func (m *MemoryStore) FailJoinRequest(_ context.Context, id string, now time.Time) (JoinRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.moveJoin(id, JoinFailed, now, nil)
}

// ClaimJoinRequest implements JoinRequestStore: every agent row or none.
func (m *MemoryStore) ClaimJoinRequest(_ context.Context, id string, n Node, now time.Time) (JoinRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.joins[id]
	if !ok {
		return JoinRequest{}, ErrJoinNotFound
	}
	if r.NodeID != n.ID || r.NodeKey != n.Key {
		return JoinRequest{}, ErrJoinWrongNode
	}
	if !r.AsOf(now).CanMoveTo(JoinDelivered) {
		return r.AsOf(now), ErrJoinState
	}
	for _, a := range r.Agents {
		if _, taken := m.agents[a.Name]; taken {
			return JoinRequest{}, fmt.Errorf("%s: %w", a.Name, ErrNameTaken)
		}
	}
	for _, a := range r.Agents {
		m.agents[a.Name] = Agent{Name: a.Name, NodeID: n.ID, NodeName: n.Name, NodeUser: n.User, JoinedAt: now, Kind: a.Kind}
	}
	return m.moveJoin(id, JoinDelivered, now, nil)
}

func (m *MemoryStore) JoinNameHeld(_ context.Context, name, exceptID string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.joins {
		if id == exceptID || !r.AsOf(now).Open() {
			continue
		}
		if slices.ContainsFunc(r.Agents, func(a RequestedAgent) bool { return a.Name == name }) {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemoryStore) SweepJoinRequests(_ context.Context, now time.Time) ([]JoinRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var expired []JoinRequest
	for id, r := range m.joins {
		switch {
		case r.Open() && r.AsOf(now).Status == JoinExpired:
			r.Status, r.Closed = JoinExpired, now
			expired = append(expired, r)
			m.joins[id] = slimJoin(r)
		case !r.Open() && !now.Before(r.Closed.Add(JoinFinalKeep)):
			delete(m.joins, id)
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].Filed.Before(expired[j].Filed) })
	return expired, nil
}
