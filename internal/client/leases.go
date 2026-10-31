package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrLeasesUnsupported identifies a relay that predates work leases.
var ErrLeasesUnsupported = errors.New("relay does not support work leases; upgrade the relay")

// Lease is an agent's advisory claim on an opaque resource. Renewed is set
// only on an acquire that extended a lease the caller already held.
type Lease struct {
	Resource  string    `json:"resource"`
	Holder    string    `json:"holder"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Renewed   bool      `json:"renewed,omitempty"`
}

// Text is what tincan lease acquire and the lease tool print for l.
func (l Lease) Text(now time.Time) string {
	verb := "Leased"
	if l.Renewed {
		verb = "Renewed"
	}
	return fmt.Sprintf("%s %q until %s. The lease is advisory: renew it by acquiring again, and release it when you finish.", verb, l.Resource, clockTime(l.ExpiresAt, now))
}

// leaseCall makes one lease call. A relay that predates leases answers it
// 404 or 405, so only such a failure asks the relay's capabilities; any other
// 404, such as a release of no lease, comes back unchanged.
func leaseCall(ctx context.Context, c Rawer, method, path string, in, out any) error {
	err := c.Raw(ctx, method, path, in, out)
	if !IsStatus(err, http.StatusNotFound) && !IsStatus(err, http.StatusMethodNotAllowed) {
		return err
	}
	caps, cerr := capabilities(ctx, c)
	if cerr != nil {
		return fmt.Errorf("check relay capabilities: %w", cerr)
	}
	if !caps.Leases {
		return ErrLeasesUnsupported
	}
	return err
}

// AcquireLease takes resource for ttlSeconds (0 means the relay's default),
// or renews it if the caller already holds it. A teammate's overlapping lease
// comes back as a 409 whose message names its holder and expiry.
func AcquireLease(ctx context.Context, c Rawer, resource, note string, ttlSeconds int64) (Lease, error) {
	in := struct {
		Resource   string `json:"resource"`
		TTLSeconds int64  `json:"ttl_seconds,omitempty"`
		Note       string `json:"note,omitempty"`
	}{resource, ttlSeconds, note}
	var out Lease
	err := leaseCall(ctx, c, "POST", "/v1/leases", in, &out)
	return out, err
}

// ReleaseLease drops the caller's own lease on exactly resource, returning
// it.
func ReleaseLease(ctx context.Context, c Rawer, resource string) (Lease, error) {
	return releaseLease(ctx, c, resource, false)
}

// AdminReleaseLease drops anyone's lease on exactly resource, for an admin
// device or the local admin socket, returning it. Any other caller gets only
// what ReleaseLease gives it.
func AdminReleaseLease(ctx context.Context, c Rawer, resource string) (Lease, error) {
	return releaseLease(ctx, c, resource, true)
}

func releaseLease(ctx context.Context, c Rawer, resource string, admin bool) (Lease, error) {
	in := struct {
		Resource string `json:"resource"`
		Admin    bool   `json:"admin,omitempty"`
	}{resource, admin}
	var out Lease
	err := leaseCall(ctx, c, "DELETE", "/v1/leases", in, &out)
	return out, err
}

// ListLeases returns every live lease on the team.
func ListLeases(ctx context.Context, c Rawer) ([]Lease, error) {
	var out struct {
		Leases []Lease `json:"leases"`
	}
	err := leaseCall(ctx, c, "GET", "/v1/leases", nil, &out)
	return out.Leases, err
}

// FormatLeases is the listing tincan lease list and list_leases print, one
// lease per line. Resources and notes are teammates' text, so they are
// quoted.
func FormatLeases(leases []Lease, now time.Time) string {
	if len(leases) == 0 {
		return "No leases held.\n"
	}
	var b strings.Builder
	for _, l := range leases {
		fmt.Fprintf(&b, "%q held by %s until %s", l.Resource, l.Holder, clockTime(l.ExpiresAt, now))
		if l.Note != "" {
			fmt.Fprintf(&b, " note=%q", l.Note)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
