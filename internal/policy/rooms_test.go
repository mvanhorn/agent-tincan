package policy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/relay"
)

var _ relay.RoomPolicy = (*Policy)(nil)

// A room post takes a slot from the same per-minute budget as a send.
func TestChargeSharesSendRate(t *testing.T) {
	f := newFixture(t, Config{PerMinute: 2})
	if err := f.pol.Charge("muse"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(t, "muse", "grokbot", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.pol.Charge("muse"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("third in a minute: %v", err)
	}
	if err := f.pol.Charge("grokbot"); err != nil {
		t.Fatalf("another sender: %v", err)
	}
}

// Gated names every approval.json target, whatever its rule, and nothing
// else; a policy that never loaded holds every name.
func TestGatedTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":["grokbot"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, Config{Approval: a})
	if gated, err := f.pol.Gated("muse"); err != nil || !gated {
		t.Fatalf("muse: %v %v", gated, err)
	}
	if gated, err := f.pol.Gated("codex"); err != nil || gated {
		t.Fatalf("codex: %v %v", gated, err)
	}
	if gated, err := newFixture(t, Config{}).pol.Gated("muse"); err != nil || gated {
		t.Fatalf("no policy: %v %v", gated, err)
	}
	broken := &Approval{path: filepath.Join(t.TempDir(), "approval.json")}
	if err := os.WriteFile(broken.path, []byte(`{`), 0600); err != nil {
		t.Fatal(err)
	}
	if gated, err := newFixture(t, Config{Approval: broken}).pol.Gated("codex"); err == nil || !gated {
		t.Fatalf("unreadable policy: %v %v", gated, err)
	}
}
