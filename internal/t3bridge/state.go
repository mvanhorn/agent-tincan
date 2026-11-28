package t3bridge

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// DefaultStateFileName stores active tasks, watches, and event cursors.
	DefaultStateFileName = "t3-state.json"
	// DefaultRoutesFileName stores outbound T3 thread request_id -> thread_id routes.
	DefaultRoutesFileName = "t3-routes.json"

	routeRetention = 7 * 24 * time.Hour
	askRetention   = 7 * 24 * time.Hour
)

// StatePathForConfig returns the default t3-state.json path beside configPath.
func StatePathForConfig(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), DefaultStateFileName)
}

// RoutesPathForConfig returns the default t3-routes.json path beside configPath.
func RoutesPathForConfig(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), DefaultRoutesFileName)
}

// DispatchedAsk tracks one inbound Tincan ask mapped to a T3 thread.
type DispatchedAsk struct {
	RequestID        string    `json:"request_id"`
	From             string    `json:"from"`
	ThreadID         string    `json:"thread_id"`
	Link             string    `json:"link,omitempty"`
	PendingRequestID string    `json:"pending_request_id,omitempty"`
	AnsweredCount    int       `json:"answered_count,omitempty"`
	PermissionNoted  bool      `json:"permission_noted,omitempty"`
	Done             bool      `json:"done,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ThreadWatch tracks an opt-in watch on an existing interactive T3 thread.
type ThreadWatch struct {
	ID          string    `json:"id,omitempty"`
	ThreadID    string    `json:"thread_id"`
	NotifyAgent string    `json:"notify_agent"`
	RequestID   string    `json:"request_id,omitempty"`
	OneShot     bool      `json:"one_shot,omitempty"`
	Note        string    `json:"note,omitempty"`
	Events      []string  `json:"events,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Key returns a stable identity for w so state updates remove only handled
// watches without overwriting concurrent watch/unwatch edits.
func (w ThreadWatch) Key() string {
	if w.ID != "" {
		return "id:" + w.ID
	}
	if w.RequestID != "" {
		return "req:" + w.RequestID
	}
	return w.ThreadID + "|" + w.NotifyAgent + "|" + w.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + w.Note
}

// StateData is the persisted JSON body of t3-state.json.
type StateData struct {
	Cursors map[string]string        `json:"cursors,omitempty"`
	Asks    map[string]DispatchedAsk `json:"asks,omitempty"`
	Watches map[string][]ThreadWatch `json:"watches,omitempty"`
}

// StateStore provides cross-process locked, atomic persistence for t3-state.json.
type StateStore struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

// NewStateStore opens a StateStore backed by path.
func NewStateStore(path string) *StateStore {
	return &StateStore{path: path, now: time.Now}
}

// Path returns the state file path.
func (s *StateStore) Path() string { return s.path }

// Snapshot reads the latest state from disk under file lock.
func (s *StateStore) Snapshot() (StateData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out StateData
	err := withFileLock(s.path, func() error {
		var err error
		out, err = s.readLocked()
		return err
	})
	return out, err
}

// Update reloads the latest state under file lock, applies fn, prunes stale
// completed asks, and writes atomically.
func (s *StateStore) Update(fn func(*StateData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return withFileLock(s.path, func() error {
		st, err := s.readLocked()
		if err != nil {
			return err
		}
		if err := fn(&st); err != nil {
			return err
		}
		s.prune(&st)
		b, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		return writePrivateAtomic(s.path, b)
	})
}

func (s *StateStore) readLocked() (StateData, error) {
	st := StateData{
		Cursors: map[string]string{},
		Asks:    map[string]DispatchedAsk{},
		Watches: map[string][]ThreadWatch{},
	}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if len(raw) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, err
	}
	if st.Cursors == nil {
		st.Cursors = map[string]string{}
	}
	if st.Asks == nil {
		st.Asks = map[string]DispatchedAsk{}
	}
	if st.Watches == nil {
		st.Watches = map[string][]ThreadWatch{}
	}
	return st, nil
}

func (s *StateStore) prune(st *StateData) {
	cutoff := s.now().Add(-askRetention)
	for id, ask := range st.Asks {
		if ask.Done && !ask.UpdatedAt.IsZero() && ask.UpdatedAt.Before(cutoff) {
			delete(st.Asks, id)
		}
	}
}

// OutboundRoute maps an outbound Tincan request sent from a T3 thread back to
// that originating T3 thread for late-reply delivery.
type OutboundRoute struct {
	RequestID    string    `json:"request_id"`
	ThreadID     string    `json:"thread_id,omitempty"`
	ToAgent      string    `json:"to_agent,omitempty"`
	DeliveredGen int64     `json:"delivered_gen,omitempty"`
	InlineDone   bool      `json:"inline_done,omitempty"`
	Notice       bool      `json:"notice,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// RouteData is the persisted JSON body of t3-routes.json.
type RouteData struct {
	Routes map[string]OutboundRoute `json:"routes,omitempty"`
}

// RouteStore provides cross-process locked persistence for t3-routes.json.
type RouteStore struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

// NewRouteStore opens a RouteStore backed by path.
func NewRouteStore(path string) *RouteStore {
	return &RouteStore{path: path, now: time.Now}
}

// Path returns the route file path.
func (r *RouteStore) Path() string { return r.path }

// Record registers a newly sent outbound ask from threadID.
func (r *RouteStore) Record(requestID, threadID, toAgent string) error {
	if requestID == "" || threadID == "" {
		return nil
	}
	return r.Update(func(d *RouteData) {
		now := r.now().UTC()
		d.Routes[requestID] = OutboundRoute{
			RequestID: requestID,
			ThreadID:  threadID,
			ToAgent:   toAgent,
			CreatedAt: now,
			UpdatedAt: now,
		}
	})
}

// RecordNotice records a bridge-originated notification ask (for example a watch
// alert) whose replies should be acknowledged automatically without routing to a
// T3 thread.
func (r *RouteStore) RecordNotice(requestID, toAgent string) error {
	if requestID == "" {
		return nil
	}
	return r.Update(func(d *RouteData) {
		now := r.now().UTC()
		d.Routes[requestID] = OutboundRoute{
			RequestID: requestID,
			ToAgent:   toAgent,
			Notice:    true,
			CreatedAt: now,
			UpdatedAt: now,
		}
	})
}

// MarkInlineDone marks requestID as already received inline by the asking thread.
func (r *RouteStore) MarkInlineDone(requestID string) error {
	if requestID == "" {
		return nil
	}
	return r.Update(func(d *RouteData) {
		if rt, ok := d.Routes[requestID]; ok {
			rt.InlineDone = true
			rt.UpdatedAt = r.now().UTC()
			d.Routes[requestID] = rt
		}
	})
}

// Lookup reads the latest route entry for requestID from disk.
func (r *RouteStore) Lookup(requestID string) (OutboundRoute, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out OutboundRoute
	var ok bool
	err := withFileLock(r.path, func() error {
		d, err := r.readLocked()
		if err != nil {
			return err
		}
		out, ok = d.Routes[requestID]
		return nil
	})
	return out, ok, err
}

// MarkDelivered records that reply generation gen was accepted by T3 Code.
func (r *RouteStore) MarkDelivered(requestID string, gen int64) error {
	return r.Update(func(d *RouteData) {
		if rt, ok := d.Routes[requestID]; ok {
			if gen > rt.DeliveredGen {
				rt.DeliveredGen = gen
			}
			rt.UpdatedAt = r.now().UTC()
			d.Routes[requestID] = rt
		}
	})
}

// Update reloads, mutates, prunes old entries, and atomically writes t3-routes.json.
func (r *RouteStore) Update(fn func(*RouteData)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return withFileLock(r.path, func() error {
		d, err := r.readLocked()
		if err != nil {
			return err
		}
		fn(&d)
		cutoff := r.now().Add(-routeRetention)
		for id, rt := range d.Routes {
			ts := rt.UpdatedAt
			if ts.IsZero() {
				ts = rt.CreatedAt
			}
			if !ts.IsZero() && ts.Before(cutoff) {
				delete(d.Routes, id)
			}
		}
		b, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		return writePrivateAtomic(r.path, b)
	})
}

func (r *RouteStore) readLocked() (RouteData, error) {
	d := RouteData{Routes: map[string]OutboundRoute{}}
	raw, err := os.ReadFile(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if len(raw) == 0 {
		return d, nil
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, err
	}
	if d.Routes == nil {
		d.Routes = map[string]OutboundRoute{}
	}
	return d, nil
}
