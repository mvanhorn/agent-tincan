package identity

// Unit tests for invite-code hashing and pepper handling. The digest
// algorithm is pinned by a golden vector so a future change to the hash
// cannot slip past silently; normalization and pepper management are explicit.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// testPepper returns bytes 0x00..0x1f, the pepper the golden vector below was
// computed with: python3 hmac.new(bytes(range(32)), b"ABCD-EFGH",
// hashlib.sha256).hexdigest().
func testPepper() []byte {
	p := make([]byte, 32)
	for i := range p {
		p[i] = byte(i)
	}
	return p
}

const goldenDigest = "006b56a3494c3fbd7344f48e485fc430f64490de35288044a61826d862878dd9"

// The digest is HMAC-SHA256 of the normalized code under the pepper, and it
// matches the independently computed vector.
func TestHashInviteCodeGoldenVector(t *testing.T) {
	pepper := testPepper()
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte("ABCD-EFGH"))
	if want := hex.EncodeToString(mac.Sum(nil)); want != goldenDigest {
		t.Fatalf("independent HMAC-SHA256 = %s, want golden %s", want, goldenDigest)
	}
	if got := hashInviteCode(pepper, "ABCD-EFGH"); got != goldenDigest {
		t.Fatalf("hashInviteCode = %s, want %s", got, goldenDigest)
	}
}

// Case and surrounding whitespace do not change the digest: an admin reading
// a code aloud ("abcd-efgh") redeems the minted "ABCD-EFGH".
func TestHashInviteCodeNormalizes(t *testing.T) {
	pepper := testPepper()
	for _, variant := range []string{"abcd-efgh", "  ABCD-EFGH  ", "\tAbCd-EfGh\n", "Abcd-Efgh"} {
		if got := hashInviteCode(pepper, variant); got != goldenDigest {
			t.Fatalf("hashInviteCode(%q) = %s, want %s", variant, got, goldenDigest)
		}
	}
}

// The digest is always 64 lowercase hex chars.
func TestHashInviteCodeShape(t *testing.T) {
	d := hashInviteCode(testPepper(), "ABCD-EFGH")
	if len(d) != 64 {
		t.Fatalf("digest length = %d, want 64", len(d))
	}
	raw, err := hex.DecodeString(d)
	if err != nil || len(raw) != 32 {
		t.Fatalf("digest %q is not 64 hex chars: %v", d, err)
	}
	if strings.ToLower(d) != d {
		t.Fatalf("digest %q is not lowercase hex", d)
	}
}

// A different pepper digests to a different value: the pepper is what keeps
// the 40-bit code space unsearchable to anyone holding only the database.
func TestHashInviteCodePepperMatters(t *testing.T) {
	other := testPepper()
	other[0] ^= 0xff
	if got, want := hashInviteCode(other, "ABCD-EFGH"), goldenDigest; got == want {
		t.Fatal("different pepper produced the same digest")
	}
}

// The Directory must copy the configured pepper, not retain the caller's
// buffer: a caller mutating its slice after NewDirectory must not invalidate
// outstanding invites (or worse, swap the key).
func TestNewDirectoryCopiesPepper(t *testing.T) {
	pepper := []byte("0123456789abcdef0123456789abcdef")
	dir := NewDirectory(NewMemoryStore(), nil, Config{InvitePepper: pepper})
	for i := range pepper {
		pepper[i] = 'x'
	}
	if string(dir.invitePepper) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("directory pepper mutated with caller buffer: %q", dir.invitePepper)
	}
}

// A configured pepper must be exactly 32 bytes; anything else fails fast.
// Empty stays allowed: it means an ephemeral pepper for tests.
func TestNewDirectoryRejectsBadPepperLength(t *testing.T) {
	for _, n := range []int{1, 16, 31, 33, 64} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewDirectory with %d-byte pepper did not panic", n)
				}
			}()
			NewDirectory(NewMemoryStore(), nil, Config{InvitePepper: make([]byte, n)})
		}()
	}
	for _, ok := range [][]byte{nil, {}, testPepper()} {
		func() {
			defer func() {
				if recover() != nil {
					t.Fatalf("NewDirectory with %d-byte pepper panicked", len(ok))
				}
			}()
			NewDirectory(NewMemoryStore(), nil, Config{InvitePepper: ok})
		}()
	}
}

type stubResolver struct{ node Node }

func (s stubResolver) WhoIs(ctx context.Context, remoteAddr string) (Node, error) {
	return s.node, nil
}

// Mint with one Directory, redeem with another sharing the store and pepper:
// outstanding invites survive a relay restart. A directory with a different
// pepper, and a normalized-code variant, behave as expected.
func TestInviteJoinRoundTripAcrossRestart(t *testing.T) {
	ctx := context.Background()
	pepper := testPepper()
	store := NewMemoryStore()
	node := Node{ID: "nMUSE", Name: "muse"}
	who := stubResolver{node: node}

	mint := NewDirectory(store, nil, Config{InvitePepper: pepper})
	code, err := mint.Invite(ctx, LocalAdmin, "muse")
	if err != nil {
		t.Fatal(err)
	}
	// The raw code is nowhere in the store: only its digest was persisted.
	if _, ok, err := store.TakeInvite(ctx, code); err != nil || ok {
		t.Fatalf("TakeInvite(raw code) = %v, %v; raw code must never be persisted", ok, err)
	}

	after := NewDirectory(store, who, Config{InvitePepper: pepper})
	if name, err := after.Join(ctx, "100.0.0.4:1", code); err != nil || name != "muse" {
		t.Fatalf("Join after restart = %q, %v", name, err)
	}

	// A restarted relay with a *different* pepper cannot redeem the code.
	code2, err := mint.Invite(ctx, LocalAdmin, "codex")
	if err != nil {
		t.Fatal(err)
	}
	other := testPepper()
	other[31] ^= 0xff
	wrongPepper := NewDirectory(store, who, Config{InvitePepper: other})
	if _, err := wrongPepper.Join(ctx, "100.0.0.4:1", code2); err != ErrBadInvite {
		t.Fatalf("Join with wrong pepper = %v, want ErrBadInvite", err)
	}

	// Normalization applies at redemption: lowercase with whitespace works.
	code3, err := mint.Invite(ctx, LocalAdmin, "grok")
	if err != nil {
		t.Fatal(err)
	}
	if name, err := after.Join(ctx, "100.0.0.4:1", "  "+strings.ToLower(code3)+"\n"); err != nil || name != "grok" {
		t.Fatalf("Join with normalized code = %q, %v", name, err)
	}
}
