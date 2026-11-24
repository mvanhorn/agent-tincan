package relay_test

import (
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// TestSendIdempotencyKeyFilesOneTask: a work ask is filed on the task list
// as it is sent. A retry under the same key gets the same request and task
// back and files nothing new.
func TestSendIdempotencyKeyFilesOneTask(t *testing.T) {
	m := newTaskMesh(t)
	m.host(t, "scout")
	send := map[string]any{"to": "muse", "body": "Check the line", "idempotency_key": "check-1"}
	var first, again envelope.SendResponse
	if err := m.grok.Raw(t.Context(), "POST", "/v1/send", send, &first); err != nil || first.TaskID == 0 {
		t.Fatalf("first send = %+v, %v, want a filed task", first, err)
	}
	if err := m.grok.Raw(t.Context(), "POST", "/v1/send", send, &again); err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.TaskID != first.TaskID {
		t.Fatalf("retry = request %s task %d, want %s task %d", again.ID, again.TaskID, first.ID, first.TaskID)
	}
	var tasks, requests int
	if err := m.Store.DB().QueryRow(`SELECT (SELECT COUNT(*) FROM tasks), (SELECT COUNT(*) FROM requests WHERE from_agent = 'grokbot')`).Scan(&tasks, &requests); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || requests != 1 {
		t.Fatalf("%d tasks and %d requests after a send and its retry, want 1 and 1", tasks, requests)
	}
	if n := m.asks(t, first.TaskID); n != 1 {
		t.Fatalf("task %d has %d asks, want 1", first.TaskID, n)
	}
}
