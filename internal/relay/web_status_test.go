package relay

import (
	"context"
	"fmt"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"net/http"
	"sync"
	"testing"
)

func TestWebStatusRouteAndTarget(t *testing.T) {
	h, clk, _ := wakeHarness(t)
	body := fmt.Sprintf(`{"site":"chatgpt.com","state":"signed_out","observed_at":%q}`, clk.Now().Format("2006-01-02T15:04:05.999999999Z07:00"))
	h.do(grokAddr, "PUT", "/v1/agents/self/web-status", body, http.StatusNoContent, nil)
	a := agentInfo(t, h, macAddr, "grokbot")
	if a.SignedOutSite != "chatgpt.com" || a.SignedOutSince.IsZero() {
		t.Fatalf("missing fact: %+v", a)
	}
	var sent envelope.SendResponse
	h.do(instinctAddr, "POST", "/v1/send", `{"to":"grokbot","body":"hello"}`, http.StatusCreated, &sent)
	if sent.Target == nil || sent.Target.SignedOutSite != "chatgpt.com" {
		t.Fatalf("missing target: %+v", sent)
	}
	for _, bad := range []string{
		`{"site":"evil.example","state":"signed_out"}`, `{"site":"chatgpt.com","state":"unknown"}`, body[:len(body)-1] + `,"agent":"muse"}`, body + ` {}`,
	} {
		h.do(grokAddr, "PUT", "/v1/agents/self/web-status", bad, http.StatusBadRequest, nil)
	}
}

type statusPreparer struct{}

func (*statusPreparer) Prepare(context.Context, *envelope.Request) error { return nil }
func (*statusPreparer) NotifyDestination() string                        { return "" }

func TestWebStatusConcurrentPreparerReplacement(t *testing.T) {
	s := &Server{}
	s.SetPreparer(&statusPreparer{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1000 {
			s.SetPreparer(&statusPreparer{})
		}
	})
	for range 1000 {
		s.notifyWebStatus(t.Context())
	}
	wg.Wait()
}
