package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

const rebuiltAddr = "100.0.0.7:1"

// rebuildInstinct replaces instinct's machine with a new node of the same
// name, as an e2b rebuild does. The old node leaves the tailnet.
func (h *harness) rebuildInstinct(name string) {
	h.who.Remove(instinctAddr)
	h.who.Set(rebuiltAddr, identity.Node{ID: "nINST2", Name: name})
}

func (h *harness) rebindEvents() []store.AuditEvent {
	h.t.Helper()
	events, err := h.st.AuditEvents(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.AuditEvent
	for _, e := range events {
		if e.Event == "rebind" {
			out = append(out, e)
		}
	}
	return out
}

type whoami struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func TestRebuiltMachineIsReadmittedOverHTTP(t *testing.T) {
	h := newHarness(t, Config{})
	h.do(macAddr, "PUT", "/v1/agents/instinct/kind", `{"kind":"e2b-email"}`, http.StatusOK, nil)
	sent := h.send(grokAddr, "instinct", "queued while rebuilding")
	h.rebuildInstinct("instinct")

	var me whoami
	h.do(rebuiltAddr, "GET", "/v1/whoami", "", http.StatusOK, &me)
	if me.Name != "instinct" || me.Kind != "e2b-email" {
		t.Fatalf("whoami after rebuild = %+v", me)
	}
	var got pollResult
	h.do(rebuiltAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || got.Requests[0].ID != sent.ID {
		t.Fatalf("poll after rebuild = %+v", got)
	}
	events := h.rebindEvents()
	if len(events) != 1 || events[0].Actor != "instinct" {
		t.Fatalf("rebind audit = %+v", events)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(events[0].Detail), &d); err != nil {
		t.Fatal(err)
	}
	if d["agent"] != "instinct" || d["old_node"] != "nINST" || d["new_node"] != "nINST2" {
		t.Fatalf("rebind detail = %s", events[0].Detail)
	}
	if _, err := h.st.VerifyAudit(context.Background()); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
}

func TestRebuiltMachineWithSuffixOverHTTP(t *testing.T) {
	h := newHarness(t, Config{})
	h.rebuildInstinct("instinct-1")
	var me whoami
	h.doAs(rebuiltAddr, "instinct", "GET", "/v1/whoami", "", http.StatusOK, &me)
	if me.Name != "instinct" {
		t.Fatalf("whoami = %+v", me)
	}
}

func TestOldNodeStillOnlineIsNotReplaced(t *testing.T) {
	h := newHarness(t, Config{})
	h.who.Set(rebuiltAddr, identity.Node{ID: "nINST2", Name: "instinct-1"})
	h.who.SetOnline("nINST", true)
	rec := h.do(rebuiltAddr, "GET", "/v1/whoami", "", http.StatusForbidden, nil)
	if !strings.Contains(rec.Body.String(), "online") {
		t.Fatalf("refusal should say the old node is online: %s", rec.Body.String())
	}
	// instinct still works from its own machine.
	h.do(instinctAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	if len(h.rebindEvents()) != 0 {
		t.Fatal("no rebind should be audited")
	}
}

func TestNoAutoRebindKeepsRebuiltMachineOut(t *testing.T) {
	h := newHarnessDir(t, Config{}, identity.Config{NoAutoRebind: true})
	h.rebuildInstinct("instinct")
	h.do(rebuiltAddr, "GET", "/v1/whoami", "", http.StatusForbidden, nil)
	if len(h.rebindEvents()) != 0 {
		t.Fatal("no rebind should be audited")
	}
}

func TestWhoAmI(t *testing.T) {
	h := newHarness(t, Config{})
	var me whoami
	h.do(museAddr, "GET", "/v1/whoami", "", http.StatusOK, &me)
	if me.Name != "muse" || me.Kind != "" {
		t.Fatalf("whoami = %+v", me)
	}
	rec := h.do(strangerAddr, "GET", "/v1/whoami", "", http.StatusForbidden, nil)
	if !strings.Contains(rec.Body.String(), identity.ErrNotJoined.Error()) {
		t.Fatalf("stranger whoami = %s", rec.Body.String())
	}
}
