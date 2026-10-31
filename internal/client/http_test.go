package client

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Client-only sandboxes (Muse) reach the tailnet only through HTTP_PROXY, so
// every client must use ProxyFromEnvironment on its own transport.
func TestNewHonorsProxyFromEnvironment(t *testing.T) {
	for _, p := range []Profile{APIClient, PollClient} {
		tr, ok := New(p).Transport.(*http.Transport)
		if !ok {
			t.Fatalf("profile %v: transport %T, want *http.Transport", p, New(p).Transport)
		}
		if tr.Proxy == nil || reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
			t.Fatalf("profile %v: transport proxy is not http.ProxyFromEnvironment", p)
		}
		if tr == http.DefaultTransport {
			t.Fatalf("profile %v: transport must be a clone, not DefaultTransport", p)
		}
	}
}

// A held long-poll must not be cut off by the relay's write timeout or the
// client's own timeout.
func TestPollTimeoutsExceedHold(t *testing.T) {
	if got := Defaults(RelayAPI).WriteTimeout; got <= DefaultPollHold {
		t.Fatalf("RelayAPI WriteTimeout %v must exceed poll hold %v", got, DefaultPollHold)
	}
	if got := Defaults(PollClient).ClientTimeout; got <= DefaultPollHold {
		t.Fatalf("PollClient timeout %v must exceed poll hold %v", got, DefaultPollHold)
	}
}

func TestConfigureAppliesRelaySettings(t *testing.T) {
	srv := Configure(&http.Server{}, RelayAPI)
	want := Defaults(RelayAPI)
	if srv.ReadHeaderTimeout != want.ReadHeaderTimeout || srv.WriteTimeout != want.WriteTimeout || srv.MaxHeaderBytes != want.MaxHeaderBytes {
		t.Fatalf("Configure did not apply RelayAPI settings: %+v", srv)
	}
}

func TestLimitBodyRejectsOversize(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		LimitBody(w, r, 8)
		_, err := io.ReadAll(r.Body)
		if _, ok := errors.AsType[*http.MaxBytesError](err); !ok {
			t.Errorf("want MaxBytesError, got %v", err)
		}
	})
	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader("more than eight bytes"))
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestUnknownProfileFallsBack(t *testing.T) {
	if got := New(Profile(99)).Timeout; got.Seconds() != 30 {
		t.Fatalf("unknown profile timeout %v, want 30s", got)
	}
}
