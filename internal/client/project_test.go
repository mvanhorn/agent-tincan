package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestProjectHint(t *testing.T) {
	if got := ProjectHint("muse", &envelope.Target{ProjectMatch: envelope.ProjectNone}); !strings.Contains(got, "No session of that project") || !strings.Contains(got, "muse") {
		t.Fatalf("hint = %q", got)
	}
	for _, target := range []*envelope.Target{nil, {}, {ProjectMatch: envelope.ProjectLive}} {
		if got := ProjectHint("muse", target); got != "" {
			t.Fatalf("hint for %+v = %q", target, got)
		}
	}
	res := Result{Request: envelope.Request{ID: "r1", To: "muse"}, Status: envelope.StatusQueued, Target: &envelope.Target{ProjectMatch: envelope.ProjectNone}}
	if got := FormatResult(res); !strings.Contains(got, "No session of that project") {
		t.Fatalf("pending ask does not say so: %q", got)
	}
}

// A relay that predates projects sees an unknown JSON field and header and
// no more: the send works, the header goes on every call, and nothing is
// said about a match.
func TestProjectAgainstOldRelay(t *testing.T) {
	var header, field string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get(ProjectHeader)
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Project string `json:"project"`
		}
		json.Unmarshal(raw, &in)
		field = in.Project
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"r1","to":"muse","body":"x"}`))
	}))
	defer srv.Close()
	r := NewRelayHTTP(srv.URL, srv.Client())
	r.SetProject("beta")
	sent, err := r.SendResult(WithProject(t.Context(), "alpha"), "muse", "x", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if header != "beta" || field != "alpha" {
		t.Fatalf("header %q, field %q", header, field)
	}
	if got := ProjectHint("muse", sent.Target); got != "" {
		t.Fatalf("hint from an old relay = %q", got)
	}
	if _, err := r.Send(t.Context(), "muse", "x", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	if field != "" {
		t.Fatalf("a send with no project sent %q", field)
	}
}
