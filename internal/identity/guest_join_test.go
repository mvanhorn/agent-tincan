package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A plain code that outlives its name becoming a guest dies at join: the
// tier does not change quietly, even with a code already in hand.
func TestPlainCodeCannotTakeAGuestName(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	who := staticResolver{"100.0.0.5:1": Node{ID: "nNEW", Name: "newer", User: "owner@example.com"}}
	d := NewDirectory(st, who, Config{})
	if err := st.PutAgent(ctx, Agent{Name: "friend", NodeID: "nOLD", Guest: GuestTerms{Until: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutInvite(ctx, Invite{Code: hashInviteCode(d.invitePepper, "AAAA-BBBB"), Name: "friend", Expires: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Join(ctx, "100.0.0.5:1", "AAAA-BBBB"); !errors.Is(err, ErrGuestTier) {
		t.Fatalf("join = %v", err)
	}
	if a, _, _ := st.AgentByName(ctx, "friend"); a.NodeID != "nOLD" || !a.Guest.Set() {
		t.Fatalf("agent = %+v", a)
	}
}

// Two guest codes redeemed at once on one machine leave one guest. The relay's
// route check cannot stop the second: both calls come from a machine with no
// guest yet.
func TestConcurrentGuestCodesLeaveOneGuest(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	who := staticResolver{"100.0.0.5:1": Node{ID: "nNEW", Name: "newer", User: "friend@example.net"}}
	d := NewDirectory(st, who, Config{})
	terms := GuestTerms{Until: time.Now().Add(time.Hour), Asks: []string{"muse"}}
	codes := map[string]string{"AAAA-BBBB": "friend", "CCCC-DDDD": "other"}
	for code, name := range codes {
		if err := st.PutInvite(ctx, Invite{Code: hashInviteCode(d.invitePepper, code), Name: name, Expires: time.Now().Add(time.Minute), NewOnly: true, Guest: terms}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, len(codes))
	for code := range codes {
		wg.Go(func() {
			<-start
			_, err := d.Join(ctx, "100.0.0.5:1", code)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	joined := 0
	for err := range errs {
		switch {
		case err == nil:
			joined++
		case !errors.Is(err, ErrGuestNode):
			t.Fatalf("join = %v", err)
		}
	}
	agents, err := st.AgentsByNode(ctx, "nNEW")
	if err != nil {
		t.Fatal(err)
	}
	if joined != 1 || len(agents) != 1 {
		t.Fatalf("%d joins, agents = %+v", joined, agents)
	}
}

type staticResolver map[string]Node

func (r staticResolver) WhoIs(_ context.Context, addr string) (Node, error) {
	n, ok := r[addr]
	if !ok {
		return Node{}, errors.New("unknown node")
	}
	return n, nil
}
