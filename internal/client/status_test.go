package client

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ReportHostStatus sends the session's busy state as its own, naming the
// session.
func TestReportHostStatusSendsSessionState(t *testing.T) {
	type call struct {
		method, session string
		busy            bool
	}
	var calls []call
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/self/status" {
			t.Errorf("path %s", r.URL.Path)
		}
		var in struct{ Busy bool }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode: %v", err)
		}
		calls = append(calls, call{r.Method, r.Header.Get(SessionHeader), in.Busy})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	r, err := NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	r.SetSession("s1")
	if err := r.ReportHostStatus(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := r.ReportHostStatus(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	want := []call{{"PUT", "s1", true}, {"PUT", "s1", false}}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
}

// A relay that predates the route answers 404 or 405, which both read as
// unsupported so the caller can stop reporting.
func TestReportHostStatusUnsupported(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		r, err := NewRelay(ts.URL, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.ReportHostStatus(t.Context(), true); !errors.Is(err, ErrHostStatusUnsupported) {
			t.Errorf("PUT %d: err = %v", code, err)
		}
		ts.Close()
	}
}
