package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Lease is an agent's advisory claim on an opaque resource until Expires.
// The relay does not read the resource: it means what the team says it does.
type Lease struct {
	Resource string
	Holder   string
	Note     string
	Created  time.Time
	Expires  time.Time
}

// LeaseGrant is AcquireLease's answer. Renewed says the holder already held
// this resource. Lapsed lists expired leases the acquire cleared, which the
// next ExpireLeases will not return.
type LeaseGrant struct {
	Lease   Lease
	Renewed bool
	Lapsed  []Lease
}

// LeaseHeldError is AcquireLease's refusal: a teammate's live lease overlaps
// the resource.
type LeaseHeldError struct {
	Lease Lease
}

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("%q is held by %s until %s", e.Lease.Resource, e.Lease.Holder, e.Lease.Expires.UTC().Format(time.RFC3339))
}

var (
	// ErrLeaseLimit is AcquireLease's answer when a new lease would take
	// the holder past its limit.
	ErrLeaseLimit = errors.New("too many live leases")
	// ErrNoLease is ReleaseLease's answer when no live lease has that
	// resource.
	ErrNoLease = errors.New("no such lease")
	// ErrNotLeaseHolder is ReleaseLease's answer to anyone but the holder.
	ErrNotLeaseHolder = errors.New("only the holder or an admin can release a lease")
)

// migrateLeases creates the work_leases table. Like watches it has no
// foreign key to agents, since a rebind re-inserts the agent row; DeleteAgent
// drops an agent's leases instead.
func (s *Store) migrateLeases() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS work_leases (
  resource   TEXT PRIMARY KEY,
  holder     TEXT NOT NULL,
  note       TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
)`)
	return err
}

// LeasesOverlap reports whether two resources cover anything in common. A
// resource ending in * is a prefix (a/* and a/** both cover a/b); any other
// resource, stars inside it included, is exact.
func LeasesOverlap(a, b string) bool {
	ap, bp := strings.TrimRight(a, "*"), strings.TrimRight(b, "*")
	aPrefix, bPrefix := ap != a, bp != b
	switch {
	case aPrefix && bPrefix:
		return strings.HasPrefix(ap, bp) || strings.HasPrefix(bp, ap)
	case aPrefix:
		return strings.HasPrefix(b, ap)
	case bPrefix:
		return strings.HasPrefix(a, bp)
	}
	return a == b
}

// AcquireLease gives holder the resource for ttl, in one transaction that
// opens with a write, so racing acquires run one after another and exactly
// one wins. A teammate's overlapping live lease refuses it with a
// *LeaseHeldError. The holder's own leases never conflict, and acquiring a
// resource it holds renews it: the expiry restarts from now and the note
// changes only if a new one is given. A new lease past limit live leases is
// ErrLeaseLimit. The holder must still be in the directory (ErrNoAgent),
// checked after that first write so a DeleteAgent cannot land in between and
// leave a lease that a later agent joined under the name would inherit. The
// clock is read after that write too, so time spent waiting for the write
// lock cannot use up a short lease or keep a lapsed one in the way.
func (s *Store) AcquireLease(ctx context.Context, holder, resource, note string, ttl time.Duration, limit int) (LeaseGrant, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LeaseGrant{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM work_leases WHERE 0`); err != nil { // takes the write lock
		return LeaseGrant{}, err
	}
	now := s.now()
	lapsed, err := expireLeasesTx(ctx, tx, now)
	if err != nil {
		return LeaseGrant{}, err
	}
	var known int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE name = ?`, holder).Scan(&known); err != nil {
		return LeaseGrant{}, err
	}
	if known == 0 {
		return LeaseGrant{}, ErrNoAgent
	}
	live, err := queryLeases(ctx, tx, `SELECT resource, holder, note, created_at, expires_at FROM work_leases ORDER BY resource`)
	if err != nil {
		return LeaseGrant{}, err
	}
	mine, renewed := 0, false
	for _, l := range live {
		switch {
		case l.Holder == holder:
			mine++
			renewed = renewed || l.Resource == resource
		case LeasesOverlap(l.Resource, resource):
			return LeaseGrant{}, &LeaseHeldError{Lease: l}
		}
	}
	if !renewed && mine >= limit {
		return LeaseGrant{}, ErrLeaseLimit
	}
	l, err := scanLease(tx.QueryRowContext(ctx, `INSERT INTO work_leases (resource, holder, note, created_at, expires_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(resource) DO UPDATE SET expires_at = excluded.expires_at, note = CASE WHEN excluded.note != '' THEN excluded.note ELSE note END
		RETURNING resource, holder, note, created_at, expires_at`,
		resource, holder, note, now.UnixMilli(), now.Add(ttl).UnixMilli()))
	if err != nil {
		return LeaseGrant{}, err
	}
	return LeaseGrant{Lease: l, Renewed: renewed, Lapsed: lapsed}, tx.Commit()
}

// ReleaseLease drops the lease on exactly resource and returns it. A
// non-empty holder must hold it (ErrNotLeaseHolder); empty is an admin's
// release of anyone's. It returns ErrNoLease when no live lease matches.
func (s *Store) ReleaseLease(ctx context.Context, resource, holder string) (Lease, error) {
	now := s.now().UnixMilli()
	l, err := scanLease(s.db.QueryRowContext(ctx, `DELETE FROM work_leases WHERE resource = ? AND expires_at > ? AND (? = '' OR holder = ?)
		RETURNING resource, holder, note, created_at, expires_at`, resource, now, holder, holder))
	if !errors.Is(err, sql.ErrNoRows) {
		return l, err
	}
	var live bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_leases WHERE resource = ? AND expires_at > ?)`, resource, now).Scan(&live); err != nil {
		return Lease{}, err
	}
	if live {
		return Lease{}, ErrNotLeaseHolder
	}
	return Lease{}, ErrNoLease
}

// Leases lists the live leases by resource. Expired rows are skipped here
// and left for ExpireLeases.
func (s *Store) Leases(ctx context.Context) ([]Lease, error) {
	return queryLeases(ctx, s.db, `SELECT resource, holder, note, created_at, expires_at FROM work_leases WHERE expires_at > ? ORDER BY resource`, s.now().UnixMilli())
}

// ExpireLeases deletes every lease past its expiry and returns them, so the
// relay's sweep can audit each lapse once.
func (s *Store) ExpireLeases(ctx context.Context) ([]Lease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	lapsed, err := expireLeasesTx(ctx, tx, s.now())
	if err != nil {
		return nil, err
	}
	return lapsed, tx.Commit()
}

func expireLeasesTx(ctx context.Context, tx *sql.Tx, now time.Time) ([]Lease, error) {
	return queryLeases(ctx, tx, `DELETE FROM work_leases WHERE expires_at <= ? RETURNING resource, holder, note, created_at, expires_at`, now.UnixMilli())
}

// leaseQuerier is what queryLeases needs from a *sql.DB or a *sql.Tx.
type leaseQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryLeases(ctx context.Context, q leaseQuerier, query string, args ...any) ([]Lease, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanLease(sc scanner) (Lease, error) {
	var l Lease
	var created, expires int64
	if err := sc.Scan(&l.Resource, &l.Holder, &l.Note, &created, &expires); err != nil {
		return Lease{}, err
	}
	l.Created, l.Expires = time.UnixMilli(created), time.UnixMilli(expires)
	return l, nil
}
