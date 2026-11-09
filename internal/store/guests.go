package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// migrateGuests adds what guests need to tables from before them: a guest's
// limits on its agent row and on its invite, and on each request the relay's
// marker. An agent whose guest_until is 0 is a teammate, so every existing
// agent stays one. After migrateAgents: its rebuild copies only the older
// columns.
func (s *Store) migrateGuests() error {
	for _, col := range []struct{ table, name, definition string }{
		{"agents", "guest_until", "INTEGER NOT NULL DEFAULT 0"},
		{"agents", "guest_asks", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "guest_no_hold", "INTEGER NOT NULL DEFAULT 0"},
		{"agents", "guest_ended", "INTEGER NOT NULL DEFAULT 0"},
		{"invites", "guest_until", "INTEGER NOT NULL DEFAULT 0"},
		{"invites", "guest_asks", "TEXT NOT NULL DEFAULT ''"},
		{"invites", "guest_no_hold", "INTEGER NOT NULL DEFAULT 0"},
		{"requests", "from_guest", "INTEGER NOT NULL DEFAULT 0"},
		{"requests", "guest_approved", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var has bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = ?)`, col.table, col.name).Scan(&has); err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE " + col.table + " ADD COLUMN " + col.name + " " + col.definition); err != nil {
			return err
		}
	}
	return nil
}

// encodeGuest is t's stored form: Until in unix millis, 0 for a teammate, and
// Asks as a JSON list, "" for a teammate.
func encodeGuest(t identity.GuestTerms) (until int64, asks string, err error) {
	if !t.Set() {
		return 0, "", nil
	}
	raw, err := json.Marshal(t.Asks)
	return t.Until.UnixMilli(), string(raw), err
}

// decodeGuest reads what encodeGuest stored.
func decodeGuest(until int64, asks string) (time.Time, []string, error) {
	if until == 0 {
		return time.Time{}, nil, nil
	}
	var list []string
	err := json.Unmarshal([]byte(asks), &list)
	return time.UnixMilli(until), list, err
}

// GuestsEnding lists the guests past their term at now that the sweep has
// not closed out yet.
func (s *Store) GuestsEnding(ctx context.Context, now time.Time) ([]identity.Agent, error) {
	return s.agentsWhere(ctx, "guest_until != 0 AND guest_until <= ? AND guest_ended = 0", now.UnixMilli())
}

// MarkGuestEnded records that the sweep closed out a guest.
func (s *Store) MarkGuestEnded(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET guest_ended = 1 WHERE name = ?`, name)
	return err
}

// OpenGuestClaims counts the asks agent has claimed that a guest's ask led
// to, and how many of those the owner did not approve. Policy reads it across
// every claim, whichever one a new ask names as its parent, so naming another
// claim does not lose the mark.
func (s *Store) OpenGuestClaims(ctx context.Context, agent string) (marked, unapproved int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(guest_approved = 0), 0) FROM requests WHERE to_agent = ? AND status = ? AND kind = ? AND from_guest = 1`,
		agent, string(envelope.StatusClaimed), string(envelope.KindAsk)).Scan(&marked, &unapproved)
	return marked, unapproved, err
}

// OpenGuestAsks counts the asks a guest's ask led to that have reached agent
// and are not finished: more than OpenGuestClaims, since poll shows an ask
// before it is claimed and a needs_input reply or a lapsed lease leaves it
// open with no claim. A held ask is left out, as agent has not seen it. The
// send path must keep to OpenGuestClaims: counting a needs_input ask there
// would let a guest hold a teammate's sends by never answering.
func (s *Store) OpenGuestAsks(ctx context.Context, agent string) (open int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE to_agent = ? AND status IN (?, ?, ?, ?) AND kind = ? AND from_guest = 1`,
		agent, string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed), string(envelope.StatusNeedsInput), string(envelope.KindAsk)).Scan(&open)
	return open, err
}
