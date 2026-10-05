package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// defaultHoldTTL is how long a held request waits for approval when
// approval.json sets no hold_ttl, or there is no file.
const defaultHoldTTL = 2 * time.Hour

type approvalRule struct {
	From   json.RawMessage `json:"from,omitempty"`
	Unless []string        `json:"unless,omitempty"`
}

type approvalConfig struct {
	Gate    map[string]approvalRule `json:"gate"`
	Notify  string                  `json:"notify,omitempty"`
	HoldTTL string                  `json:"hold_ttl,omitempty"`
}

// Approval reloads the owner's policy and retains its last valid target set.
type Approval struct {
	mu      sync.Mutex
	path    string
	good    *approvalConfig
	info    os.FileInfo
	ttl     time.Duration
	missing bool
}

// LoadApproval validates the initial policy. A missing file disables the gate.
func LoadApproval(path string) (*Approval, error) {
	a := &Approval{path: path}
	if err := a.reload(); err != nil {
		return nil, err
	}
	return a, nil
}

var approvalName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func (a *Approval) reload() error {
	entry, err := os.Lstat(a.path)
	if errors.Is(err, os.ErrNotExist) {
		a.missing = true
		a.info = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("read approval policy: %w", err)
	}
	if !entry.Mode().IsRegular() {
		return errors.New("approval.json must be a regular file")
	}
	f, err := os.Open(a.path)
	if err != nil {
		return fmt.Errorf("read approval policy: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("approval.json must be a regular file with mode 0600")
	}
	// Read the file on every check (it is capped at 1 MiB): an in-place edit
	// that keeps the size and modification time must still take effect.
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("approval.json too large")
	}
	if err := approvalJSON(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return errors.New("invalid approval.json: duplicate key, null, or malformed JSON")
	}
	var c approvalConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return errors.New("invalid approval.json")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("approval.json must contain one object")
	}
	if c.Gate == nil {
		return errors.New("approval.json requires gate object")
	}
	ttl := defaultHoldTTL
	if c.HoldTTL != "" {
		ttl, err = time.ParseDuration(c.HoldTTL)
		if err != nil || ttl < time.Millisecond {
			return errors.New("hold_ttl must be a positive duration of at least 1ms")
		}
	}
	if c.Notify != "" && !approvalName.MatchString(c.Notify) {
		return errors.New("invalid approval notify agent")
	}
	for target, rule := range c.Gate {
		if !approvalName.MatchString(target) {
			return errors.New("invalid approval target")
		}
		if (len(rule.From) == 0) == (rule.Unless == nil) {
			return errors.New("approval rule requires exactly one of from or unless")
		}
		names := rule.Unless
		if len(rule.From) > 0 {
			var wildcard string
			if json.Unmarshal(rule.From, &wildcard) == nil && wildcard == "*" {
				rule.From = json.RawMessage(`"*"`)
				c.Gate[target] = rule
				continue
			}
			if err := json.Unmarshal(rule.From, &names); err != nil || names == nil {
				return errors.New("from must be '*' or an array of agent names")
			}
		}
		for _, name := range names {
			if !approvalName.MatchString(name) {
				return errors.New("invalid approval agent name")
			}
		}
	}
	a.missing = false
	a.good = &c
	a.info = info
	a.ttl = ttl
	return nil
}

// Held evaluates the relay-recorded chain and sender. On reload failure it
// holds all last-known gated targets; without a valid copy callers must reject.
// HoldTTL and ApprovalNotify are internal metadata, never accepted from clients.
func (a *Approval) Held(req *envelope.Request) (bool, error) {
	if a == nil {
		return false, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.reload()
	if a.good == nil || (err == nil && a.missing) {
		return false, err
	}
	rule, ok := a.good.Gate[req.To]
	if !ok {
		return false, nil
	}
	req.HoldTTL = a.ttl
	req.ApprovalNotify = a.good.Notify
	if err != nil {
		return true, err
	}
	if string(rule.From) == `"*"` {
		return true, nil
	}
	names := rule.Unless
	if len(rule.From) > 0 {
		_ = json.Unmarshal(rule.From, &names)
	}
	for _, name := range append(slices.Clone(req.Chain), req.From) {
		match := slices.Contains(names, name)
		if (rule.Unless != nil && !match) || (rule.Unless == nil && match) {
			return true, nil
		}
	}
	return false, nil
}

// holdDefaults reports whether the policy Held last read has an entry for
// target, and the hold TTL and notify target a default hold should use: the
// file's, or 2 hours and no notice when there is no file. Nil-safe.
func (a *Approval) holdDefaults(target string) (entry bool, ttl time.Duration, notify string) {
	if a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
	}
	if a == nil || a.good == nil || a.missing {
		return false, defaultHoldTTL, ""
	}
	_, entry = a.good.Gate[target]
	return entry, a.ttl, a.good.Notify
}

// Reject ambiguous JSON rather than letting a repeated field replace a rule.
func approvalJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("null")
	}
	switch token {
	case json.Delim('{'):
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			name = strings.ToLower(name)
			if !ok || keys[name] {
				return errors.New("duplicate key")
			}
			keys[name] = true
			if err := approvalJSON(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case json.Delim('['):
		for d.More() {
			if err := approvalJSON(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	return nil
}

// NotifyDestination reloads the operator destination. Invalid configuration
// never sends a notice using a stale destination.
func (a *Approval) NotifyDestination() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reload() != nil || a.missing || a.good == nil {
		return ""
	}
	return a.good.Notify
}
