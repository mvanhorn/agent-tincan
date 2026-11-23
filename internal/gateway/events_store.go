package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const eventsSchema = `
CREATE TABLE IF NOT EXISTS gw_event_subscriptions (
  id                   TEXT PRIMARY KEY,
  agent                TEXT NOT NULL,
  client_id            TEXT NOT NULL,
  event_name           TEXT NOT NULL,
  filters_json         TEXT NOT NULL DEFAULT '{}',
  callback_url         TEXT NOT NULL,
  signing_secret       TEXT NOT NULL,
  expires_at           INTEGER NOT NULL,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  has_gap              INTEGER NOT NULL DEFAULT 0,
  last_error           TEXT NOT NULL DEFAULT '',
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS gw_event_subs_agent ON gw_event_subscriptions(agent, expires_at);
CREATE INDEX IF NOT EXISTS gw_event_subs_expires ON gw_event_subscriptions(expires_at);

CREATE TABLE IF NOT EXISTS gw_event_deliveries (
  event_id        TEXT PRIMARY KEY,
  subscription_id TEXT NOT NULL,
  agent           TEXT NOT NULL,
  event_name      TEXT NOT NULL,
  payload_json    TEXT NOT NULL,
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS gw_event_deliveries_due ON gw_event_deliveries(next_attempt_at, created_at);
CREATE INDEX IF NOT EXISTS gw_event_deliveries_sub ON gw_event_deliveries(subscription_id);
`

// Subscription and delivery limits.
const (
	DefaultSubTTL              = 24 * time.Hour
	MaxSubTTL                  = 7 * 24 * time.Hour
	MinSubTTL                  = time.Minute
	MaxSubsPerAgent            = 16
	MaxPendingDeliveriesPerSub = 100
	DeliveryRetention          = 24 * time.Hour
)

var (
	// ErrTooManySubscriptions means an agent already has MaxSubsPerAgent active subscriptions.
	ErrTooManySubscriptions = errors.New("too many active event subscriptions for this agent")
	// ErrSubscriptionNotFound means the subscription does not exist, expired, or belongs to another caller.
	ErrSubscriptionNotFound = errors.New("event subscription not found")
)

// EventSubscription is one active webhook subscription bound to (Agent, ClientID).
type EventSubscription struct {
	ID                  string
	Agent               string
	ClientID            string
	EventName           string
	Filters             map[string]any
	CallbackURL         string
	SigningSecret       string // never returned over API or logged
	ExpiresAt           time.Time
	ConsecutiveFailures int
	HasGap              bool
	LastError           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// EventSubscriptionSummary is the safe, secret-free view for doctor and admin inspection.
type EventSubscriptionSummary struct {
	ID                  string    `json:"id"`
	Agent               string    `json:"agent"`
	EventName           string    `json:"event_name"`
	Callback            string    `json:"callback"` // sanitized: scheme://host/path, no query string
	ExpiresAt           time.Time `json:"expires_at"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
	HasGap              bool      `json:"has_gap,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

// EventDelivery is one queued outbound webhook event delivery.
type EventDelivery struct {
	EventID        string
	SubscriptionID string
	Agent          string
	EventName      string
	PayloadJSON    string
	Attempts       int
	NextAttemptAt  time.Time
	CreatedAt      time.Time
}

// EventStore persists gateway MCP Events subscriptions and queued deliveries in SQLite.
type EventStore struct {
	db              *sql.DB
	now             func() time.Time
	maxSubsPerAgent int
}

// NewEventStore initializes the gw_event_* tables in db.
func NewEventStore(db *sql.DB, now func() time.Time) (*EventStore, error) {
	if now == nil {
		now = time.Now
	}
	if _, err := db.Exec(eventsSchema); err != nil {
		return nil, err
	}
	return &EventStore{
		db:              db,
		now:             now,
		maxSubsPerAgent: MaxSubsPerAgent,
	}, nil
}

func clampSubTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return DefaultSubTTL
	}
	if ttl < MinSubTTL {
		return MinSubTTL
	}
	if ttl > MaxSubTTL {
		return MaxSubTTL
	}
	return ttl
}

func marshalFilters(filters map[string]any) (string, error) {
	if len(filters) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(filters)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalFilters(raw string) map[string]any {
	if raw == "" || raw == "{}" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// CountActiveForAgent returns the number of unexpired subscriptions for agent.
func (s *EventStore) CountActiveForAgent(ctx context.Context, agent string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM gw_event_subscriptions WHERE agent = ? AND expires_at > ?`,
		agent, s.now().UnixMilli(),
	).Scan(&n)
	return n, err
}

// CreateSubscription sweeps expired subscriptions for the agent, enforces the
// per-agent cap, and persists sub.
func (s *EventStore) CreateSubscription(ctx context.Context, sub EventSubscription, ttl time.Duration) (EventSubscription, error) {
	filtersJSON, err := marshalFilters(sub.Filters)
	if err != nil {
		return EventSubscription{}, err
	}
	now := s.now().UTC()
	nowMs := now.UnixMilli()
	exp := now.Add(clampSubTTL(ttl))
	expMs := exp.UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventSubscription{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_deliveries WHERE subscription_id IN (SELECT id FROM gw_event_subscriptions WHERE expires_at <= ?)`, nowMs); err != nil {
		return EventSubscription{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_subscriptions WHERE expires_at <= ?`, nowMs); err != nil {
		return EventSubscription{}, err
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM gw_event_subscriptions WHERE agent = ? AND expires_at > ?`, sub.Agent, nowMs).Scan(&count); err != nil {
		return EventSubscription{}, err
	}
	if count >= s.maxSubsPerAgent {
		if err := tx.Commit(); err != nil {
			return EventSubscription{}, err
		}
		return EventSubscription{}, ErrTooManySubscriptions
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gw_event_subscriptions(id, agent, client_id, event_name, filters_json, callback_url, signing_secret, expires_at, consecutive_failures, has_gap, last_error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, '', ?, ?)`,
		sub.ID, sub.Agent, sub.ClientID, sub.EventName, filtersJSON, sub.CallbackURL, sub.SigningSecret, expMs, nowMs, nowMs,
	); err != nil {
		return EventSubscription{}, err
	}
	if err := tx.Commit(); err != nil {
		return EventSubscription{}, err
	}
	sub.ExpiresAt = exp
	sub.CreatedAt = now
	sub.UpdatedAt = now
	if sub.Filters == nil {
		sub.Filters = map[string]any{}
	}
	return sub, nil
}

// GetOwnedSubscription loads an unexpired subscription owned by (agent, clientID).
func (s *EventStore) GetOwnedSubscription(ctx context.Context, id, agent, clientID string) (EventSubscription, error) {
	nowMs := s.now().UnixMilli()
	row := s.db.QueryRowContext(ctx,
		`SELECT id, agent, client_id, event_name, filters_json, callback_url, signing_secret, expires_at, consecutive_failures, has_gap, last_error, created_at, updated_at
		 FROM gw_event_subscriptions WHERE id = ? AND agent = ? AND client_id = ? AND expires_at > ?`,
		id, agent, clientID, nowMs,
	)
	sub, err := scanSubscription(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EventSubscription{}, ErrSubscriptionNotFound
	}
	return sub, err
}

// Subscription loads a subscription by ID regardless of owner (internal delivery use only).
func (s *EventStore) Subscription(ctx context.Context, id string) (EventSubscription, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, agent, client_id, event_name, filters_json, callback_url, signing_secret, expires_at, consecutive_failures, has_gap, last_error, created_at, updated_at
		 FROM gw_event_subscriptions WHERE id = ?`,
		id,
	)
	sub, err := scanSubscription(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EventSubscription{}, false, nil
	}
	if err != nil {
		return EventSubscription{}, false, err
	}
	return sub, true, nil
}

// RefreshSubscription extends expires_at for an active subscription owned by (agent, clientID).
func (s *EventStore) RefreshSubscription(ctx context.Context, id, agent, clientID string, ttl time.Duration) (EventSubscription, error) {
	now := s.now().UTC()
	nowMs := now.UnixMilli()
	exp := now.Add(clampSubTTL(ttl))
	expMs := exp.UnixMilli()

	res, err := s.db.ExecContext(ctx,
		`UPDATE gw_event_subscriptions SET expires_at = ?, updated_at = ?
		 WHERE id = ? AND agent = ? AND client_id = ? AND expires_at > ?`,
		expMs, nowMs, id, agent, clientID, nowMs,
	)
	if err != nil {
		return EventSubscription{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return EventSubscription{}, err
	}
	if n == 0 {
		return EventSubscription{}, ErrSubscriptionNotFound
	}
	return s.GetOwnedSubscription(ctx, id, agent, clientID)
}

// DeleteSubscription removes a subscription owned by (agent, clientID) and its pending deliveries.
func (s *EventStore) DeleteSubscription(ctx context.Context, id, agent, clientID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM gw_event_subscriptions WHERE id = ? AND agent = ? AND client_id = ?`,
		id, agent, clientID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_deliveries WHERE subscription_id = ?`, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ForceDeleteSubscription deletes a subscription by ID (e.g. on 410 Gone or failure cap)
// and returns the deleted row so the caller can send a best-effort terminated notice.
func (s *EventStore) ForceDeleteSubscription(ctx context.Context, id string) (EventSubscription, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventSubscription{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		`SELECT id, agent, client_id, event_name, filters_json, callback_url, signing_secret, expires_at, consecutive_failures, has_gap, last_error, created_at, updated_at
		 FROM gw_event_subscriptions WHERE id = ?`,
		id,
	)
	sub, err := scanSubscription(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EventSubscription{}, false, tx.Commit()
	}
	if err != nil {
		return EventSubscription{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_deliveries WHERE subscription_id = ?`, id); err != nil {
		return EventSubscription{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_subscriptions WHERE id = ?`, id); err != nil {
		return EventSubscription{}, false, err
	}
	return sub, true, tx.Commit()
}

// DeleteAgentSubscriptions deletes all subscriptions and pending deliveries for agent,
// returning the removed subscriptions so best-effort terminated notices can be sent.
func (s *EventStore) DeleteAgentSubscriptions(ctx context.Context, agent string) ([]EventSubscription, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, agent, client_id, event_name, filters_json, callback_url, signing_secret, expires_at, consecutive_failures, has_gap, last_error, created_at, updated_at
		 FROM gw_event_subscriptions WHERE agent = ?`,
		agent,
	)
	if err != nil {
		return nil, err
	}
	subs, err := scanSubscriptions(rows)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_deliveries WHERE agent = ?`, agent); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_subscriptions WHERE agent = ?`, agent); err != nil {
		return nil, err
	}
	return subs, tx.Commit()
}

// ListSubscriptionsForAgent returns active subscriptions owned by (agent, clientID).
func (s *EventStore) ListSubscriptionsForAgent(ctx context.Context, agent, clientID string) ([]EventSubscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent, client_id, event_name, filters_json, callback_url, signing_secret, expires_at, consecutive_failures, has_gap, last_error, created_at, updated_at
		 FROM gw_event_subscriptions WHERE agent = ? AND client_id = ? AND expires_at > ? ORDER BY created_at`,
		agent, clientID, s.now().UnixMilli(),
	)
	if err != nil {
		return nil, err
	}
	return scanSubscriptions(rows)
}

// ActiveSubscriptionsForEvent returns all unexpired subscriptions for agent matching eventName.
func (s *EventStore) ActiveSubscriptionsForEvent(ctx context.Context, agent, eventName string) ([]EventSubscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent, client_id, event_name, filters_json, callback_url, signing_secret, expires_at, consecutive_failures, has_gap, last_error, created_at, updated_at
		 FROM gw_event_subscriptions WHERE agent = ? AND event_name = ? AND expires_at > ? ORDER BY created_at`,
		agent, eventName, s.now().UnixMilli(),
	)
	if err != nil {
		return nil, err
	}
	return scanSubscriptions(rows)
}

// HasActiveSubscription reports whether agent currently has at least one unexpired subscription.
func (s *EventStore) HasActiveSubscription(ctx context.Context, agent string) (bool, error) {
	var has bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM gw_event_subscriptions WHERE agent = ? AND expires_at > ?)`,
		agent, s.now().UnixMilli(),
	).Scan(&has)
	return has, err
}

// Summary returns a sanitized summary of every active subscription for doctor/admin views.
func (s *EventStore) Summary(ctx context.Context) ([]EventSubscriptionSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent, event_name, callback_url, expires_at, consecutive_failures, has_gap, last_error, created_at
		 FROM gw_event_subscriptions WHERE expires_at > ? ORDER BY agent, event_name, created_at`,
		s.now().UnixMilli(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EventSubscriptionSummary{}
	for rows.Next() {
		var item EventSubscriptionSummary
		var cb string
		var expMs, createdMs int64
		var hasGap int
		if err := rows.Scan(&item.ID, &item.Agent, &item.EventName, &cb, &expMs, &item.ConsecutiveFailures, &hasGap, &item.LastError, &createdMs); err != nil {
			return nil, err
		}
		item.Callback = SanitizeCallbackURL(cb)
		item.ExpiresAt = time.UnixMilli(expMs).UTC()
		item.HasGap = hasGap != 0
		item.CreatedAt = time.UnixMilli(createdMs).UTC()
		out = append(out, item)
	}
	return out, rows.Err()
}

// SweepExpired deletes expired subscriptions and orphaned deliveries.
func (s *EventStore) SweepExpired(ctx context.Context) (int64, error) {
	nowMs := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_deliveries WHERE subscription_id IN (SELECT id FROM gw_event_subscriptions WHERE expires_at <= ?)`, nowMs); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM gw_event_subscriptions WHERE expires_at <= ?`, nowMs)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_deliveries WHERE subscription_id NOT IN (SELECT id FROM gw_event_subscriptions)`); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// EnqueueDelivery inserts a delivery row. If the subscription already has
// MaxPendingDeliveriesPerSub pending rows, the oldest rows are dropped and
// has_gap is set on the subscription.
func (s *EventStore) EnqueueDelivery(ctx context.Context, d EventDelivery) error {
	now := s.now().UTC()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	if d.NextAttemptAt.IsZero() {
		d.NextAttemptAt = now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM gw_event_deliveries WHERE subscription_id = ?`, d.SubscriptionID).Scan(&pending); err != nil {
		return err
	}
	if pending >= MaxPendingDeliveriesPerSub {
		drop := pending - MaxPendingDeliveriesPerSub + 1
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM gw_event_deliveries WHERE event_id IN (
				SELECT event_id FROM gw_event_deliveries WHERE subscription_id = ? ORDER BY created_at, event_id LIMIT ?
			)`, d.SubscriptionID, drop); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE gw_event_subscriptions SET has_gap = 1, updated_at = ? WHERE id = ?`, now.UnixMilli(), d.SubscriptionID); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gw_event_deliveries(event_id, subscription_id, agent, event_name, payload_json, attempts, next_attempt_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.EventID, d.SubscriptionID, d.Agent, d.EventName, d.PayloadJSON, d.Attempts, d.NextAttemptAt.UnixMilli(), d.CreatedAt.UnixMilli(),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// DueDeliveries returns up to limit deliveries whose next_attempt_at <= now.
func (s *EventStore) DueDeliveries(ctx context.Context, limit int) ([]EventDelivery, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, subscription_id, agent, event_name, payload_json, attempts, next_attempt_at, created_at
		 FROM gw_event_deliveries WHERE next_attempt_at <= ? ORDER BY next_attempt_at, created_at LIMIT ?`,
		s.now().UnixMilli(), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventDelivery
	for rows.Next() {
		var d EventDelivery
		var nextMs, createdMs int64
		if err := rows.Scan(&d.EventID, &d.SubscriptionID, &d.Agent, &d.EventName, &d.PayloadJSON, &d.Attempts, &nextMs, &createdMs); err != nil {
			return nil, err
		}
		d.NextAttemptAt = time.UnixMilli(nextMs).UTC()
		d.CreatedAt = time.UnixMilli(createdMs).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDelivery deletes one delivery row without altering subscription failure counters.
func (s *EventStore) DeleteDelivery(ctx context.Context, eventID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM gw_event_deliveries WHERE event_id = ?`, eventID)
	return err
}

// DeferDelivery updates next_attempt_at (and optionally payload_json) without counting as a failed attempt.
func (s *EventStore) DeferDelivery(ctx context.Context, eventID string, nextAttempt time.Time, payloadJSON string) error {
	if payloadJSON != "" {
		_, err := s.db.ExecContext(ctx, `UPDATE gw_event_deliveries SET next_attempt_at = ?, payload_json = ? WHERE event_id = ?`,
			nextAttempt.UnixMilli(), payloadJSON, eventID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE gw_event_deliveries SET next_attempt_at = ? WHERE event_id = ?`,
		nextAttempt.UnixMilli(), eventID)
	return err
}

// MarkDelivered deletes the delivery row and resets the subscription's failure state.
func (s *EventStore) MarkDelivered(ctx context.Context, eventID, subscriptionID string) error {
	nowMs := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_deliveries WHERE event_id = ?`, eventID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gw_event_subscriptions SET consecutive_failures = 0, last_error = '', updated_at = ? WHERE id = ?`, nowMs, subscriptionID); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearHasGap clears has_gap and resets consecutive_failures after a gap notice is accepted.
func (s *EventStore) ClearHasGap(ctx context.Context, subscriptionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE gw_event_subscriptions SET has_gap = 0, consecutive_failures = 0, last_error = '', updated_at = ? WHERE id = ?`,
		s.now().UnixMilli(), subscriptionID,
	)
	return err
}

// SetHasGap marks has_gap = 1 on a subscription.
func (s *EventStore) SetHasGap(ctx context.Context, subscriptionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE gw_event_subscriptions SET has_gap = 1, updated_at = ? WHERE id = ?`,
		s.now().UnixMilli(), subscriptionID,
	)
	return err
}

// RecordAttemptFailure increments attempt count and consecutive_failures. If
// maxAttempts is reached or the delivery aged out past DeliveryRetention, the
// delivery is dropped and has_gap is set on the subscription.
func (s *EventStore) RecordAttemptFailure(ctx context.Context, eventID, subscriptionID string, nextAttempt time.Time, maxAttempts int, errMsg string) (dropped bool, consecutiveFailures int, err error) {
	now := s.now().UTC()
	nowMs := now.UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var attempts int
	var createdMs int64
	if err := tx.QueryRowContext(ctx, `SELECT attempts, created_at FROM gw_event_deliveries WHERE event_id = ?`, eventID).Scan(&attempts, &createdMs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, 0, tx.Commit()
		}
		return false, 0, err
	}
	attempts++
	agedOut := now.Sub(time.UnixMilli(createdMs)) >= DeliveryRetention
	dropped = attempts >= maxAttempts || agedOut

	if dropped {
		if _, err := tx.ExecContext(ctx, `DELETE FROM gw_event_deliveries WHERE event_id = ?`, eventID); err != nil {
			return false, 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE gw_event_subscriptions SET consecutive_failures = consecutive_failures + 1, has_gap = 1, last_error = ?, updated_at = ? WHERE id = ?`,
			truncateErr(errMsg), nowMs, subscriptionID,
		); err != nil {
			return false, 0, err
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE gw_event_deliveries SET attempts = ?, next_attempt_at = ? WHERE event_id = ?`,
			attempts, nextAttempt.UnixMilli(), eventID,
		); err != nil {
			return false, 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE gw_event_subscriptions SET consecutive_failures = consecutive_failures + 1, last_error = ?, updated_at = ? WHERE id = ?`,
			truncateErr(errMsg), nowMs, subscriptionID,
		); err != nil {
			return false, 0, err
		}
	}

	if err := tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM gw_event_subscriptions WHERE id = ?`, subscriptionID).Scan(&consecutiveFailures); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, 0, err
	}
	return dropped, consecutiveFailures, tx.Commit()
}

// RecordGapFailure records a failed attempt to deliver a gap notice.
func (s *EventStore) RecordGapFailure(ctx context.Context, subscriptionID, errMsg string) (int, error) {
	nowMs := s.now().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE gw_event_subscriptions SET consecutive_failures = consecutive_failures + 1, last_error = ?, updated_at = ? WHERE id = ?`,
		truncateErr(errMsg), nowMs, subscriptionID,
	); err != nil {
		return 0, err
	}
	var consecutive int
	err := s.db.QueryRowContext(ctx, `SELECT consecutive_failures FROM gw_event_subscriptions WHERE id = ?`, subscriptionID).Scan(&consecutive)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return consecutive, err
}

func truncateErr(s string) string {
	const maxLen = 200
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSubscription(row rowScanner) (EventSubscription, error) {
	var sub EventSubscription
	var filtersJSON string
	var expMs, createdMs, updatedMs int64
	var hasGap int
	if err := row.Scan(
		&sub.ID, &sub.Agent, &sub.ClientID, &sub.EventName, &filtersJSON,
		&sub.CallbackURL, &sub.SigningSecret, &expMs, &sub.ConsecutiveFailures,
		&hasGap, &sub.LastError, &createdMs, &updatedMs,
	); err != nil {
		return EventSubscription{}, err
	}
	sub.Filters = unmarshalFilters(filtersJSON)
	sub.ExpiresAt = time.UnixMilli(expMs).UTC()
	sub.HasGap = hasGap != 0
	sub.CreatedAt = time.UnixMilli(createdMs).UTC()
	sub.UpdatedAt = time.UnixMilli(updatedMs).UTC()
	return sub, nil
}

func scanSubscriptions(rows *sql.Rows) ([]EventSubscription, error) {
	defer rows.Close()
	var out []EventSubscription
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}
