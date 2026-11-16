package relay

import (
	"context"
	"net/http"
	"testing"
)

// A host joined before logins were recorded gets its login on its next call
// from its own node. That rewrites its row on the same node, so the task
// host mark stays and no audit row says it was cleared.
func TestTaskHostKeptByLoginBackfill(t *testing.T) {
	h := newHarness(t, Config{})
	ctx := context.Background()
	h.do(macAddr, "PUT", "/v1/agents/instinct/task-host", `{"task_host":true}`, http.StatusOK, nil)
	if a, _, err := h.st.AgentByName(ctx, "instinct"); err != nil || a.NodeUser != "" || !a.TaskHost {
		t.Fatalf("instinct before the backfill = %+v, %v", a, err)
	}
	h.recordInstinctLogin()
	if a, _, err := h.st.AgentByName(ctx, "instinct"); err != nil || a.NodeUser != login || !a.TaskHost {
		t.Fatalf("instinct after the backfill = %+v, %v; want the login recorded and the mark kept", a, err)
	}
	if n := auditMatches(t, h.st, "task-host", `"task_host":false`); n != 0 {
		t.Fatalf("the backfill wrote %d audit rows for a cleared mark", n)
	}
}
