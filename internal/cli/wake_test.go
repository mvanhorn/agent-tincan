package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func TestWakeCommandsRedactAndUseStdin(t *testing.T) {
	// Isolate from the machine's saved tincan config: without this,
	// LoadConfig picks up a saved proxy and the loopback PUT below
	// goes through it instead of straight to the test server.
	useConfig(t, client.Config{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(map[string]string{"grokbot": "webhook", "other": "wait"})
		case "PUT":
			var in map[string]string
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			if in["bearer_token"] != "private-token" || in["webhook"] != "https://example.com/private-path" {
				t.Errorf("wrong input: %#v", in)
			}
			json.NewEncoder(w).Encode(map[string]string{"method": "webhook"})
		}
	}))
	defer srv.Close()
	root := Root()
	root.SetIn(bytes.NewBufferString("private-token\n"))
	out, err := run(t, root, "wake", "set", "grokbot", "--webhook", "https://example.com/private-path", "--bearer-token-stdin", "--relay", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "private-") || !strings.Contains(out, "saved and applied") {
		t.Fatal(out)
	}
	out, err = run(t, Root(), "wake", "list", "--relay", srv.URL)
	if err != nil || out != "grokbot\twebhook\nother\twait\n" {
		t.Fatalf("%q %v", out, err)
	}
}
