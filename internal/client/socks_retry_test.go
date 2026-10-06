package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// socksFailure is what the SOCKS dialer returns when the proxy answers but
// cannot reach the relay: the sandbox's tailnet tunnel is down.
func socksFailure() error {
	return &net.OpError{Op: "socks connect", Net: "tcp", Err: errors.New("unknown error general SOCKS server failure")}
}

// fastSOCKSRetry shortens the SOCKS retry schedule for one test.
func fastSOCKSRetry(t *testing.T) {
	old := socksRetry
	socksRetry = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { socksRetry = old })
}

// flakyRelay is a relay client whose API transport fails with fail for the
// first failures roster calls and then answers them. It counts the roster
// calls, and records whether a search for a moved relay ran.
func flakyRelay(t *testing.T, failures int, fail func() error) (r *Relay, attempts *atomic.Int32, searched *atomic.Bool) {
	t.Helper()
	const key = "k-real"
	savedConfig(t, Config{Relay: "http://relay.test", RelayKey: key})
	r, err := NewRelayFor(Config{Relay: "http://relay.test", RelayKey: key})
	if err != nil {
		t.Fatal(err)
	}
	attempts, searched = new(atomic.Int32), new(atomic.Bool)
	r.findRelays = func(context.Context, string) []string { searched.Store(true); return nil }
	r.api = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/agents" {
			return nil, fail() // the moved-relay search's hello at the old address
		}
		if int(attempts.Add(1)) <= failures {
			return nil, fail()
		}
		body := `{"agents":[{"name":"muse","online":true}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	return r, attempts, searched
}

// A tunnel that blips for a moment costs the caller a short wait, not an
// error: the call is made again and returns the relay's answer.
func TestSOCKSConnectFailureIsRetried(t *testing.T) {
	fastSOCKSRetry(t)
	r, attempts, _ := flakyRelay(t, 2, socksFailure)
	agents, err := r.Agents(t.Context())
	if err != nil || len(agents) != 1 || agents[0].Name != "muse" {
		t.Fatalf("Agents = %+v, %v; want the relay's roster after the retries", agents, err)
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("attempts = %d, want 3 (two SOCKS failures, then the answer)", n)
	}
}

// A tunnel that stays down fails once the schedule is spent, with an error
// that says the tunnel is down, that nothing was sent, and that an emailed
// request can be answered by email. It does not search for a moved relay.
func TestSOCKSConnectFailureGivesUpWithTunnelHint(t *testing.T) {
	fastSOCKSRetry(t)
	r, attempts, searched := flakyRelay(t, 100, socksFailure)
	_, err := r.Agents(t.Context())
	if err == nil {
		t.Fatal("want an error once the retries are spent")
	}
	if n, want := int(attempts.Load()), len(socksRetry)+1; n != want {
		t.Fatalf("attempts = %d, want %d (the first try and one per retry)", n, want)
	}
	if searched.Load() {
		t.Fatal("a SOCKS failure means the local tunnel is down, not that the relay moved; it must not search")
	}
	msg := TunnelHint(err).Error()
	for _, want := range []string{"socks connect", "tailnet tunnel to the relay is not up", "nothing was sent", "replying to that email"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not say %q", msg, want)
		}
	}
	var op *net.OpError
	if !errors.As(TunnelHint(err), &op) || op.Op != "socks connect" {
		t.Fatalf("hinted error must still unwrap to the SOCKS error: %v", err)
	}
}

// Every other failure keeps today's path: one attempt, then a search for a
// moved relay (a refused or failed dial, a timeout after connecting), or
// nothing more for a relay error. None of them gets the tunnel hint.
func TestOtherFailuresAreNotRetriedAsSOCKS(t *testing.T) {
	fastSOCKSRetry(t)
	timeout := func() error { return &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded} }
	for _, tc := range []struct {
		name string
		fail func() error
	}{
		{"connection refused dial", func() error { return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED} }},
		{"plain dial failure", func() error { return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route to host")} }},
		{"timeout after connect", timeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, attempts, searched := flakyRelay(t, 100, tc.fail)
			_, err := r.Agents(t.Context())
			findIdle(t, r)
			if err == nil {
				t.Fatal("want the failure")
			}
			if n := attempts.Load(); n != 1 {
				t.Fatalf("attempts = %d, want 1 (no SOCKS retry)", n)
			}
			if !searched.Load() {
				t.Fatal("a call that found nothing at the relay's address must still search for a moved relay")
			}
			if TunnelHint(err) != err {
				t.Fatalf("only a SOCKS failure gets the tunnel hint: %v", TunnelHint(err))
			}
		})
	}

	t.Run("relay error", func(t *testing.T) {
		savedConfig(t, Config{Relay: "http://relay.test"})
		r, err := NewRelayFor(Config{Relay: "http://relay.test"})
		if err != nil {
			t.Fatal(err)
		}
		var attempts atomic.Int32
		r.api = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts.Add(1)
			return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader(`{"error":"boom"}`)), Request: req}, nil
		})}
		_, err = r.Agents(t.Context())
		if !IsStatus(err, http.StatusInternalServerError) || attempts.Load() != 1 {
			t.Fatalf("500 = %v after %d attempts, want one attempt", err, attempts.Load())
		}
		if TunnelHint(err) != err {
			t.Fatalf("a relay error gets no tunnel hint: %v", TunnelHint(err))
		}
	})
}
