package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// responseTimeout is the error a client gets when the relay took a post but
// its answer never came back.
type responseTimeout struct{}

func (responseTimeout) Error() string   { return "timeout awaiting response headers" }
func (responseTimeout) Timeout() bool   { return true }
func (responseTimeout) Temporary() bool { return true }

// losesFirstSendAnswer is a transport that delivers every call as agent, but
// drops the relay's answer to the first POST /v1/send, after the relay has
// taken it. keys records the idempotency_key of each send it posts.
type losesFirstSendAnswer struct {
	agent string
	mu    sync.Mutex
	keys  []any
}

func (l *losesFirstSendAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if l.agent != "" {
		req.Header.Set(client.AgentHeader, l.agent)
	}
	if req.Method != http.MethodPost || req.URL.Path != "/v1/send" {
		return http.DefaultTransport.RoundTrip(req)
	}
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	var in map[string]any
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.keys = append(l.keys, in["idempotency_key"])
	first := len(l.keys) == 1
	l.mu.Unlock()
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || !first {
		return resp, err
	}
	resp.Body.Close()
	return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: responseTimeout{}}
}

// TestSendRetryAfterLostAnswerIsOneAsk: when the answer to a send is lost
// after the relay queued it, the client posts it once more under the same
// key, gets the queued ask back, and the recipient is handed one request.
func TestSendRetryAfterLostAnswerIsOneAsk(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	tr := &losesFirstSendAnswer{agent: "grokbot"}
	asker := client.NewRelayHTTP(m.URL("grokbot"), &http.Client{Transport: tr})
	got, err := asker.Ask(context.Background(), "muse", "deploy build 42", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.keys) != 2 || tr.keys[0] == nil || tr.keys[0] != tr.keys[1] {
		t.Fatalf("send keys = %v, want one key posted twice", tr.keys)
	}
	var n int
	if err := m.Store.DB().QueryRow(`SELECT COUNT(*) FROM requests WHERE from_agent = 'grokbot'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || got.Request.ID == "" || !asker.SentHere(got.Request.ID) {
		t.Fatalf("stored %d requests, client got id %q; want the one ask", n, got.Request.ID)
	}
	in, err := m.Client(t, "muse").Poll(context.Background(), 0)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].ID != got.Request.ID {
		t.Fatalf("recipient inbox = %+v, %v; want only %s", in.Requests, err, got.Request.ID)
	}
}

// TestSendWithoutKeyToOlderRelay: a relay that does not advertise the key
// (here one that predates /v1/capabilities) gets sends without one, as
// before, and a send whose answer was lost is not posted again, since that
// relay would queue it twice.
func TestSendWithoutKeyToOlderRelay(t *testing.T) {
	var mu sync.Mutex
	var sends []string
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/send" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sends = append(sends, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(envelope.Request{ID: "r1", To: "muse"})
	}))
	defer old.Close()
	tr := &losesFirstSendAnswer{}
	asker := client.NewRelayHTTP(old.URL, &http.Client{Transport: tr})
	if _, err := asker.Send(context.Background(), "muse", "deploy", envelope.KindAsk, "", false); err == nil {
		t.Fatal("send whose answer was lost reported success")
	}
	if len(tr.keys) != 1 || tr.keys[0] != nil || len(sends) != 1 {
		t.Fatalf("posted %d sends with keys %v, want one send without a key", len(sends), tr.keys)
	}
	// A later send whose answer arrives works as before.
	if req, err := asker.Send(context.Background(), "muse", "deploy", envelope.KindAsk, "", false); err != nil || req.ID != "r1" {
		t.Fatalf("second send: %+v, %v", req, err)
	}
}
