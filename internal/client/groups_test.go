package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestGroupPartialAttachmentsAndSenderScope(t *testing.T) {
	m := attachMesh(t)
	c := m.Client(t, "grokbot")
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	g, err := c.SendGroup(t.Context(), []string{"instinct", "muse", "instinct"}, "question", envelope.KindAsk, "", []string{path}, false)
	if err != nil || len(g.Results) != 2 {
		t.Fatalf("%+v %v", g, err)
	}
	a, b := g.Results[0].Request, g.Results[1].Request
	if a.Group != g.Group || b.Group != g.Group || len(a.Attachments) != 1 || len(b.Attachments) != 1 || a.Attachments[0].ID == b.Attachments[0].ID {
		t.Fatalf("requests: %+v %+v", a, b)
	}
	if _, err := m.Client(t, "instinct").Reply(t.Context(), a.ID, "answer", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	g, err = c.GetGroup(t.Context(), g.Group, 0)
	if err != nil || g.Outcome != "partial" || g.ExitCode() != 2 {
		t.Fatalf("%+v %v", g, err)
	}
	if !strings.Contains(client.FormatGroup(g), "answer") || !strings.Contains(client.FormatGroup(g), b.ID) {
		t.Fatal(client.FormatGroup(g))
	}
	if _, err := m.Client(t, "muse").GetGroup(t.Context(), g.Group, 0); !client.IsStatus(err, 404) {
		t.Fatalf("other sender: %v", err)
	}
	fresh := m.Client(t, "grokbot")
	if got, err := fresh.GetGroup(t.Context(), g.Group, 0); err != nil || len(got.Results) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := m.Client(t, "muse").Reply(t.Context(), b.ID, "second", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if got, err := fresh.GetGroup(t.Context(), g.Group, time.Second); err != nil || got.Outcome != "answered" || got.ExitCode() != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestGroupOlderRelay(t *testing.T) {
	requests := map[string]envelope.Request{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/capabilities":
			http.NotFound(w, r)
		case r.URL.Path == "/v1/send":
			var req envelope.Request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			req.ID, req.Group = req.To, ""
			requests[req.ID] = req
			_ = json.NewEncoder(w).Encode(req)
		case strings.HasPrefix(r.URL.Path, "/v1/requests/"):
			req := requests[strings.TrimPrefix(r.URL.Path, "/v1/requests/")]
			_ = json.NewEncoder(w).Encode(client.Result{Request: req, Status: envelope.StatusQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.SendGroup(t.Context(), []string{"a", "b"}, "hello", envelope.KindAsk, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	g, err = c.GetGroup(t.Context(), g.Group, 0)
	if err != nil || len(g.Results) != 2 || g.Outcome != "pending" {
		t.Fatalf("%+v %v", g, err)
	}
	fresh, _ := client.NewRelay(ts.URL, "")
	if _, err := fresh.GetGroup(t.Context(), g.Group, 0); err == nil {
		t.Fatal("expected missing local record")
	}
}

func TestGroupValidationAndSendFailure(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	c := m.Client(t, "grokbot")
	for _, targets := range [][]string{{}, {"instinct", "grokbot"}, {"instinct", ""}, strings.Split("a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q,r,s,t,u,v,w,x,y,z,aa,ab,ac,ad,ae", ",")} {
		if _, err := c.SendGroup(t.Context(), targets, "hello", envelope.KindAsk, "", nil, false); err == nil {
			t.Fatalf("accepted %v", targets)
		}
	}
	g, err := c.SendGroup(t.Context(), []string{"instinct", "missing"}, "hello", envelope.KindAsk, "", nil, false)
	if err != nil || g.Results[0].Request.ID == "" || g.Results[1].Status != envelope.StatusFailed {
		t.Fatalf("%+v %v", g, err)
	}
	g, err = c.GetGroup(t.Context(), g.Group, 0)
	if err != nil || len(g.Results) != 2 || g.Outcome != "partial" {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestGroupOutcomes(t *testing.T) {
	for _, tc := range []struct {
		statuses []envelope.Status
		code     int
	}{
		{[]envelope.Status{envelope.StatusAnswered, envelope.StatusAnswered}, 0},
		{[]envelope.Status{envelope.StatusAnswered, envelope.StatusDeclined}, 1},
		{[]envelope.Status{envelope.StatusFailed, envelope.StatusQueued}, 2},
	} {
		var g client.GroupResult
		for _, s := range tc.statuses {
			g.Results = append(g.Results, client.GroupEntry{Status: s})
		}
		if got := g.ExitCode(); got != tc.code {
			t.Fatalf("%v: %d", tc.statuses, got)
		}
	}
}

func TestGroupWaitPollsConcurrently(t *testing.T) {
	var started atomic.Int32
	ready := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if started.Add(1) == 2 {
			close(ready)
		}
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		status := envelope.StatusQueued
		if strings.HasSuffix(r.URL.Path, "/a") {
			status = envelope.StatusAnswered
		}
		_ = json.NewEncoder(w).Encode(client.Result{Request: envelope.Request{ID: r.URL.Path}, Status: status})
	}))
	defer ts.Close()
	c, _ := client.NewRelay(ts.URL, "")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	g, err := c.WaitGroup(ctx, client.GroupResult{Group: "group-test", Results: []client.GroupEntry{
		{Request: envelope.Request{ID: "a"}}, {Request: envelope.Request{ID: "b"}},
	}}, time.Second)
	if err != nil || g.Outcome != "partial" {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestGroupMaximumBodies(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	c := m.Client(t, "grokbot")
	targets := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	peers := make([]*client.Relay, len(targets))
	for i, name := range targets {
		peers[i] = m.JoinOnMachineOf(t, "instinct", name)
	}
	body := strings.Repeat("<", 128<<10)
	g, err := c.SendGroup(t.Context(), targets, body, envelope.KindAsk, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, res := range g.Results {
		if _, err := peers[i].Reply(t.Context(), res.Request.ID, body, envelope.StatusAnswered); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Get(c.Base() + "/v1/groups/" + g.Group)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var members []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(members) != 8 {
		t.Fatalf("status=%d members=%v", resp.StatusCode, members)
	}
	for _, member := range members {
		if len(member) != 2 || member["id"] == "" || member["to"] == "" {
			t.Fatalf("unexpected membership: %v", member)
		}
	}
	if n, err := m.Store.CountUnseenReplies(t.Context(), "grokbot"); err != nil || n != 8 {
		t.Fatalf("unseen after membership lookup=%d err=%v", n, err)
	}
	got, err := m.Client(t, "grokbot").GetGroup(t.Context(), g.Group, 0)
	if err != nil || got.Outcome != "answered" || len(got.Results) != 8 {
		t.Fatalf("outcome=%s count=%d err=%v", got.Outcome, len(got.Results), err)
	}
	for _, res := range got.Results {
		if res.Request.Body != body || res.Reply == nil || res.Reply.Body != body {
			t.Fatalf("missing full bodies for %s", res.Request.ID)
		}
	}
}

func TestGroupSuccessivePollErrorPreservesReply(t *testing.T) {
	var polls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			http.NotFound(w, r)
		case "/v1/send":
			_ = json.NewEncoder(w).Encode(envelope.Request{ID: "request-a", To: "a"})
		case "/v1/requests/request-a":
			if polls.Add(1) == 2 {
				http.Error(w, "poll unavailable", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(client.Result{
				Request: envelope.Request{ID: "request-a", To: "a"}, Status: envelope.StatusAnswered,
				Reply: &envelope.Reply{From: "a", Status: envelope.StatusAnswered, Body: "saved answer"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.SendGroup(t.Context(), []string{"a"}, "question", envelope.KindAsk, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for poll := 1; poll <= 3; poll++ {
		got, err := c.GetGroup(t.Context(), g.Group, 0)
		if err != nil {
			t.Fatal(err)
		}
		entry := got.Results[0]
		if got.Outcome != "answered" || entry.Status != envelope.StatusAnswered || entry.Reply == nil || entry.Reply.Body != "saved answer" {
			t.Fatalf("poll %d lost answered reply: %+v", poll, got)
		}
		if poll == 2 {
			if !strings.Contains(entry.Error, "poll unavailable") {
				t.Fatalf("missing polling error: %+v", entry)
			}
		} else if entry.Error != "" {
			t.Fatalf("poll %d retained error: %+v", poll, entry)
		}
	}
}

func TestGroupPollErrorPreservesResults(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/failed") {
			http.Error(w, "poll unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(client.Result{
			Request: envelope.Request{ID: "success", To: "a"}, Status: envelope.StatusAnswered,
			Reply: &envelope.Reply{From: "a", Status: envelope.StatusAnswered, Body: "saved answer"},
		})
	}))
	defer ts.Close()
	c, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.WaitGroup(t.Context(), client.GroupResult{Group: "group-errors", Results: []client.GroupEntry{
		{Request: envelope.Request{ID: "success", To: "a"}, Status: envelope.StatusQueued},
		{Request: envelope.Request{ID: "failed", To: "b"}, Status: envelope.StatusQueued},
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if g.Results[1].Status != envelope.StatusQueued || !strings.Contains(g.Results[1].Error, "poll unavailable") || g.Results[0].Reply == nil {
		t.Fatalf("lost partial results: %+v", g)
	}
	encoded, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{client.FormatGroup(g), string(encoded)} {
		for _, want := range []string{"group-errors", "success", "failed", "saved answer", "poll unavailable"} {
			if !strings.Contains(output, want) {
				t.Fatalf("missing %q in %s", want, output)
			}
		}
	}
}

func TestGroupRecoversLostSendResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			_, _ = w.Write([]byte(`{"groups":true}`))
		case "/v1/send":
			var req envelope.Request
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.To == "a" {
				// Accepted by the relay, but the response body was lost.
				w.WriteHeader(http.StatusCreated)
			} else {
				http.Error(w, `{"error":"rejected"}`, http.StatusBadRequest)
			}
		case "/v1/requests/accepted":
			_ = json.NewEncoder(w).Encode(client.Result{Request: envelope.Request{ID: "accepted", To: "a"}, Status: envelope.StatusAnswered, Reply: &envelope.Reply{Body: "recovered", Status: envelope.StatusAnswered}})
		default:
			_ = json.NewEncoder(w).Encode([]envelope.GroupMember{{ID: "accepted", To: "a"}})
		}
	}))
	defer ts.Close()
	c, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.SendGroup(t.Context(), []string{"a", "b"}, "hello", envelope.KindAsk, "", nil, false)
	if err != nil || g.Results[0].Status != envelope.StatusFailed || g.Results[0].Request.ID != "" {
		t.Fatalf("%+v %v", g, err)
	}
	for range 2 {
		g, err = c.GetGroup(t.Context(), g.Group, 0)
		if err != nil || len(g.Results) != 2 || g.Results[0].Request.ID != "accepted" || g.Results[0].Reply.Body != "recovered" || g.Results[1].Status != envelope.StatusFailed || g.Results[1].Request.ID != "" {
			t.Fatalf("%+v %v", g, err)
		}
	}
}

func TestGroupConcurrentPollDoesNotRegress(t *testing.T) {
	for _, advanced := range []envelope.Status{envelope.StatusAnswered, envelope.StatusFailed} {
		t.Run(string(advanced), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := advanced
				if calls.Add(1) == 1 {
					close(started)
					<-release
					status = envelope.StatusQueued
				}
				_ = json.NewEncoder(w).Encode(client.Result{Request: envelope.Request{ID: "r", To: "a"}, Status: status})
			}))
			defer ts.Close()
			c, err := client.NewRelay(ts.URL, "")
			if err != nil {
				t.Fatal(err)
			}
			g := client.GroupResult{Group: "group-r", Results: []client.GroupEntry{{Request: envelope.Request{ID: "r", To: "a"}, Status: envelope.StatusQueued}}}
			done := make(chan client.GroupResult, 1)
			go func() { result, _ := c.WaitGroup(t.Context(), g, 0); done <- result }()
			<-started
			newer, err := c.WaitGroup(t.Context(), g, 0)
			close(release)
			older := <-done
			if err != nil || newer.Results[0].Status != advanced || older.Results[0].Status != advanced {
				t.Fatalf("new=%+v stale=%+v err=%v", newer, older, err)
			}
		})
	}
}

// A lapsed lease sends a claimed request back to queued; a later poll must
// show that, not the stale claim.
func TestGroupPollFollowsRequeue(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := envelope.StatusClaimed
		if calls.Add(1) > 1 {
			status = envelope.StatusQueued
		}
		_ = json.NewEncoder(w).Encode(client.Result{Request: envelope.Request{ID: "r", To: "a"}, Status: status})
	}))
	defer ts.Close()
	c, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	g := client.GroupResult{Group: "group-requeue", Results: []client.GroupEntry{{Request: envelope.Request{ID: "r", To: "a"}, Status: envelope.StatusQueued}}}
	if g, err = c.WaitGroup(t.Context(), g, 0); err != nil || g.Results[0].Status != envelope.StatusClaimed {
		t.Fatalf("first poll = %+v %v", g, err)
	}
	if g, err = c.WaitGroup(t.Context(), g, 0); err != nil || g.Results[0].Status != envelope.StatusQueued {
		t.Fatalf("requeued request shows %s, want queued", g.Results[0].Status)
	}
}

func TestGroupForwardsUrgent(t *testing.T) {
	for _, urgent := range []bool{false, true} {
		m := testrelay.New(t, relay.Config{})
		c := m.Client(t, "grokbot")
		g, err := c.SendGroup(t.Context(), []string{"instinct", "muse"}, "time critical", envelope.KindAsk, "", nil, urgent)
		if err != nil || len(g.Results) != 2 {
			t.Fatalf("%+v %v", g, err)
		}
		for _, r := range g.Results {
			if r.Request.Urgent != urgent {
				t.Fatalf("urgent=%v: member %+v", urgent, r.Request)
			}
			got, err := c.Get(t.Context(), r.Request.ID, 0)
			if err != nil || got.Request.Urgent != urgent {
				t.Fatalf("urgent=%v: stored %+v %v", urgent, got.Request, err)
			}
		}
	}
}

// A member the relay holds for the owner's approval reports held, as a
// single send does, not queued.
func TestGroupReportsHeldMembers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in envelope.Request
		_ = json.NewDecoder(r.Body).Decode(&in)
		out := envelope.Request{ID: "r-" + in.To, To: in.To, Group: in.Group}
		if in.To == "muse" {
			out.Status = envelope.StatusHeld
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	c, err := client.NewRelay(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.SendGroup(t.Context(), []string{"instinct", "muse"}, "hello", envelope.KindAsk, "", nil, false)
	if err != nil || len(g.Results) != 2 {
		t.Fatalf("%+v %v", g, err)
	}
	if g.Results[0].Status != envelope.StatusQueued || g.Results[1].Status != envelope.StatusHeld {
		t.Fatalf("statuses = %s, %s", g.Results[0].Status, g.Results[1].Status)
	}
}
