package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type authChannel struct {
	calls []Op
	code  string
}

func (c *authChannel) Exchange(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	c.calls = append(c.calls, req.Op)
	r := NativeResponse{OK: true, Result: json.RawMessage(`{}`)}
	if c.code != "" {
		r.OK = false
		r.Error = &NativeError{Code: c.code}
	}
	_, err := recv(r)
	return err
}

func TestWebAuthenticationObservations(t *testing.T) {
	for _, site := range WebSites {
		t.Run(string(site), func(t *testing.T) {
			var logs bytes.Buffer
			c := &authChannel{code: "not_logged_in"}
			w := &WebAgent{Site: site, Native: &Client{Channel: c}, Log: &logs}
			_, _ = w.authRequest(t.Context(), w.site().op(opDetail), OpArgs{ID: "abc"})
			if w.auth.latest.State != "signed_out" {
				t.Fatal("typed failure was lost")
			}
			w.observeAuth(errors.New("disconnected"))
			w.observeAuth(ErrNotLoggedIn)
			if strings.Count(logs.String(), "signed out of") != 1 {
				t.Fatalf("duplicate transition log: %s", &logs)
			}
			c.code = ""
			w.probeSession(t.Context())
			if w.auth.latest.State != "authenticated" {
				t.Fatal("fresh probe did not clear")
			}
			if err := ValidateOp(w.site().op(opSession), OpArgs{ID: "abc"}); err == nil {
				t.Fatal("probe accepted arguments")
			}
		})
	}
}

func TestOldExtensionProbeDoesNotClear(t *testing.T) {
	c := &authChannel{code: "bad_request"}
	w := &WebAgent{Site: SourceChatGPT, Native: &Client{Channel: c}, Log: &bytes.Buffer{}}
	w.observeAuth(ErrNotLoggedIn)
	w.probeSession(t.Context())
	w.probeSession(t.Context())
	if !w.auth.probeUnsupported || len(c.calls) != 1 || w.auth.latest.State != "signed_out" {
		t.Fatalf("old extension: %+v", w.auth.latest)
	}
}

func TestWebStatusIdleProbe(t *testing.T) {
	c := &authChannel{}
	clock := newFakeClock()
	w := &WebAgent{Site: SourceChatGPT, Native: &Client{Channel: c, Cooldown: &SiteCooldown{Now: clock.Now}}, clock: clock, Log: &bytes.Buffer{}}
	next := w.probeIdle(t.Context())
	if len(c.calls) != 1 || !next.Equal(clock.Now().Add(webProbeInterval)) {
		t.Fatal("startup probe missing")
	}
	w.probeIdle(t.Context())
	if len(c.calls) != 1 {
		t.Fatal("fresh evidence did not replace probe")
	}
	if err := clock.Sleep(t.Context(), webProbeInterval); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.probeIdle(t.Context())
	w.mu.Unlock()
	if len(c.calls) != 1 {
		t.Fatal("probe overlapped active send")
	}
	w.Native.cooldown().Note(w.Site, webProbeInterval)
	w.probeIdle(t.Context())
	if len(c.calls) != 1 {
		t.Fatal("probe ignored cooldown")
	}
	if err := clock.Sleep(t.Context(), webProbeInterval); err != nil {
		t.Fatal(err)
	}
	w.probeIdle(t.Context())
	if len(c.calls) != 2 {
		t.Fatal("idle probe did not resume")
	}
}

// A session can stay in the browser long after an incoming ask arrives.
type stalledProbeChannel struct {
	started, release chan struct{}
}

func (c *stalledProbeChannel) Exchange(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	if req.Op == Op("chatgpt.session") {
		close(c.started)
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := recv(NativeResponse{OK: true, Result: json.RawMessage(`{}`)})
		return err
	}
	_, err := recv(NativeResponse{Error: &NativeError{Code: "not_logged_in"}})
	return err
}

type statusTransport func(*http.Request) (*http.Response, error)

func (f statusTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestIdleProbeDoesNotDelayAsk(t *testing.T) {
	c := &stalledProbeChannel{started: make(chan struct{}), release: make(chan struct{})}
	relay := client.NewRelayHTTP("http://relay", &http.Client{Transport: statusTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})})
	w := &WebAgent{Site: SourceChatGPT, Name: "web", Relay: relay, Native: &Client{Channel: c}, Allowlist: StaticAllowlist("codex"), Log: &bytes.Buffer{}}
	probeDone := make(chan struct{})
	go func() { defer close(probeDone); w.probeIdle(t.Context()) }()
	<-c.started
	released := false
	defer func() {
		if !released {
			close(c.release)
		}
		<-probeDone
	}()
	askDone := make(chan struct{})
	go func() {
		defer close(askDone)
		w.Handle(t.Context(), envelope.Request{ID: "ask", From: "codex", To: "web", Chain: []string{"codex"}, Kind: envelope.KindAsk, Body: "hello"})
	}()
	select {
	case <-askDone:
	case <-time.After(time.Second):
		t.Fatal("ask waited for the idle probe")
	}
	if w.auth.latest.State != "signed_out" {
		t.Fatal("ask did not reach the browser send")
	}
	// Release explicitly to inspect the stale result, keeping cleanup safe.
	close(c.release)
	<-probeDone
	released = true
	if w.auth.latest.State != "signed_out" {
		t.Fatal("stale probe cleared the send's sign-out")
	}
}

func TestIndeterminateProbeDoesNotClear(t *testing.T) {
	w := &WebAgent{Site: SourceGrok, Native: &Client{Channel: &authChannel{code: "indeterminate"}}, Log: &bytes.Buffer{}}
	w.observeAuth(ErrNotLoggedIn)
	w.probeSession(t.Context())
	if w.auth.latest.State != "signed_out" {
		t.Fatal("indeterminate probe cleared sign-out")
	}
}
