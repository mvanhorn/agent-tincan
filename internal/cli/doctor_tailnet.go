package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// minTailscale is the oldest Tailscale the doctor accepts without a warning:
// the release with the fixes for bulletins TS-2026-014 to TS-2026-017.
const minTailscale = "1.104.4"

// tailnetSelves reads this machine's Tailscale state. Tests replace it.
var tailnetSelves = client.TailnetSelves

// deviceRole is what the relay says this machine is.
type deviceRole int

const (
	roleUnknown deviceRole = iota
	roleAdmin
	roleAgent
	roleGuest
)

// deviceRoleOf asks the relay whether this is an admin device. Only a 403 is
// the relay's no: a relay without the route or one that errors says nothing.
func deviceRoleOf(ctx context.Context, r *client.Relay) deviceRole {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := r.Raw(ctx, "GET", "/v1/admin/held", nil, &[]any{})
	if err == nil {
		return roleAdmin
	}
	if e, ok := errors.AsType[*client.APIError](err); ok && e.Code == 403 {
		return roleAgent
	}
	return roleUnknown
}

// tailnetChecks reports each tailscaled on this machine, or one skip line when
// there is none to ask. r is the joined relay, nil before a join. The relay
// answers for the node that reached it. With several tailscaleds running that
// could be any of them, and through a remote proxy it is the proxy's node, so
// it is asked only when one is running and no remote proxy is in the way. A
// guest need not ask: the relay refuses it the admin route as it does an
// agent, but its machine is on another account's tailnet, out of the owner's
// tag rule.
func tailnetChecks(ctx context.Context, r *client.Relay, guest bool) []check {
	selves := tailnetSelves(ctx)
	if len(selves) == 0 {
		return []check{{"tailnet", "ok", "skipped: no tailscaled tincan can reach on this machine", ""}}
	}
	running := 0
	for _, s := range selves {
		if s.Running {
			running++
		}
	}
	role := roleUnknown
	switch {
	case guest:
		role = roleGuest
	case r != nil && running == 1 && !r.ThroughRemoteProxy():
		role = deviceRoleOf(ctx, r)
	}
	checks := make([]check, len(selves))
	for i, s := range selves {
		checks[i] = tailnetCheck(s, role)
	}
	return checks
}

// tailnetCheck judges one tailscaled. Tailscale is the security boundary, so
// the check speaks up about it, but it only warns: the host's Tailscale is
// the owner's to update, and tincan still works on an old one.
func tailnetCheck(s client.TailnetSelf, role deviceRole) check {
	status := "ok"
	var detail, fix []string
	warn := func(d, f string) {
		status = "warn"
		detail = append(detail, d)
		fix = append(fix, f)
	}

	sshOn := s.SSH != nil && *s.SSH
	short, _, _ := strings.Cut(s.Version, "-")
	switch atLeast, known := tailscaleAtLeastMin(s.Version); {
	case !known:
		warn(fmt.Sprintf("Tailscale version unknown (%q), minimum %s", s.Version, minTailscale),
			"Run a release build of Tailscale, "+minTailscale+" or later.")
	case !atLeast:
		f := "Update Tailscale to " + minTailscale + " or later (tailscale update, or your package manager or app store). It fixes Tailscale SSH and Serve/Funnel bugs (bulletins TS-2026-014 to TS-2026-017)."
		if sshOn {
			f += " Tailscale SSH is on here, so the SSH fixes apply to this machine."
		}
		warn(fmt.Sprintf("Tailscale %s is older than the minimum %s", short, minTailscale), f)
	default:
		detail = append(detail, "Tailscale "+short)
	}

	switch {
	case s.SSH == nil:
		detail = append(detail, "Tailscale SSH unknown")
	case sshOn:
		detail = append(detail, "Tailscale SSH on")
	default:
		detail = append(detail, "Tailscale SSH off")
	}

	switch {
	case !s.Running:
		detail = append(detail, "tags unknown: tailscaled is not logged in and running")
	case len(s.Tags) > 0:
		detail = append(detail, "tagged "+strings.Join(s.Tags, ", "))
	case role == roleAdmin:
		detail = append(detail, "untagged, as an admin device is by design")
	case role == roleGuest:
		detail = append(detail, "untagged, which is fine for a guest: tags are for the owner's own machines")
	case role == roleAgent:
		warn("this agent machine is untagged",
			"Tag it (for example tag:agent) in the Tailscale admin console: a tagged node is never an admin device, whatever it is called. See docs/trust-model.md.")
	default:
		detail = append(detail, "untagged, tag check skipped: could not tell whether this is an admin device")
	}

	d := strings.Join(detail, "; ")
	if s.Socket != "" {
		d += " (socket " + s.Socket + ")"
	}
	return check{"tailnet", status, d, strings.Join(fix, " ")}
}

// tailscaleRelease matches a release version: x.y.z, bare from a distro
// package, or with the commit hashes every Tailscale build carries. A
// prerelease, dev or dirty build has some other suffix. An unstable build has
// this shape too: tailscaleAtLeastMin tells it apart by its odd minor.
var tailscaleRelease = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-t[0-9a-f]+)?(?:-g[0-9a-f]+)?$`)

// tailscaleAtLeastMin compares a Tailscale long version with minTailscale.
// known is false when the version is not a release, since numbers alone do
// not show what such a build contains. A build below the minimum is older
// whatever kind it is. At or above it, an odd minor is Tailscale's unstable
// track, whose patch number is a commit count, not a release.
func tailscaleAtLeastMin(version string) (atLeast, known bool) {
	have, ok := tailscaleNumbers(version)
	if !ok {
		return false, false
	}
	want, _ := tailscaleNumbers(minTailscale)
	if slices.Compare(have, want) < 0 {
		return false, true
	}
	release := have[1]%2 == 0
	return release, release
}

func tailscaleNumbers(version string) ([]int, bool) {
	m := tailscaleRelease.FindStringSubmatch(version)
	if m == nil {
		return nil, false
	}
	nums := make([]int, 3)
	for i, s := range m[1:] {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, false
		}
		nums[i] = n
	}
	return nums, true
}
