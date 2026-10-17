package relay_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// pageCall is one call from the phone page on the admin device, as the
// browser makes it: same-origin headers, JSON for a body, and the phone's
// agent name once it has one.
func pageCall(t *testing.T, base, method, path, agent string, in, out any) (int, string) {
	t.Helper()
	hdr := map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}
	body := ""
	if in != nil {
		raw, _ := json.Marshal(in)
		body = string(raw)
		maps.Copy(hdr, browserJSON)
	}
	if agent != "" {
		hdr[client.AgentHeader] = agent
	}
	code, resp, _ := browserCall(t, method, base, path, "127.0.0.1", body, hdr)
	if out != nil && code < 300 {
		if err := json.Unmarshal([]byte(resp), out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, resp)
		}
	}
	return code, resp
}

// An unjoined admin phone cannot ask; it joins itself from the page
// (invite, then join, both from the page), shows on the roster as kind
// phone, asks a teammate and reads the reply.
func TestPhoneJoinsAndAsks(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	page := hardened(t, m, "admin")

	if code, body := pageCall(t, page, "GET", "/v1/whoami", "", nil, nil); code != http.StatusForbidden || !strings.Contains(body, identity.ErrNotJoined.Error()) {
		t.Fatalf("whoami before joining: %d %s, want not joined", code, body)
	}
	if code, body := pageCall(t, page, "POST", "/v1/send", "", map[string]string{"to": "muse", "body": "hi", "kind": "ask"}, nil); code != http.StatusForbidden || !strings.Contains(body, identity.ErrNotJoined.Error()) {
		t.Fatalf("ask before joining: %d %s, want not joined", code, body)
	}

	var inv struct{ Code string }
	if code, body := pageCall(t, page, "POST", "/v1/admin/invite", "", map[string]string{"name": "phone", "kind": onboard.KindPhone}, &inv); code != http.StatusOK || inv.Code == "" {
		t.Fatalf("invite from the page: %d %s", code, body)
	}
	var joined struct{ Name string }
	if code, body := pageCall(t, page, "POST", "/v1/join", "", map[string]string{"code": inv.Code}, &joined); code != http.StatusOK || joined.Name != "phone" {
		t.Fatalf("join from the page: %d %s", code, body)
	}
	var roster client.Roster
	if code, body := pageCall(t, page, "GET", "/v1/agents", "phone", nil, &roster); code != http.StatusOK {
		t.Fatalf("roster: %d %s", code, body)
	}
	var found bool
	for _, a := range roster.Agents {
		if a.Name == "phone" {
			found = true
			if a.Kind != onboard.KindPhone || a.GoodAt != "the owner's phone; asks only, never answers" {
				t.Fatalf("phone on the roster: %+v", a)
			}
		}
	}
	if !found {
		t.Fatalf("phone not on the roster: %+v", roster.Agents)
	}

	var sent envelope.SendResponse
	if code, body := pageCall(t, page, "POST", "/v1/send", "phone", map[string]string{"to": "muse", "body": "what is on my calendar?", "kind": "ask"}, &sent); code != http.StatusCreated || sent.ID == "" {
		t.Fatalf("ask from the phone: %d %s", code, body)
	}
	muse := m.Client(t, "muse")
	in, err := muse.Poll(t.Context(), time.Millisecond)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].From != "phone" {
		t.Fatalf("muse inbox: %+v %v", in.Requests, err)
	}
	if _, err := muse.Claim(t.Context(), sent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(t.Context(), sent.ID, "<i>dentist at 3</i>", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	var res envelope.Result
	if code, body := pageCall(t, page, "GET", "/v1/requests/"+sent.ID, "phone", nil, &res); code != http.StatusOK || res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.Body != "<i>dentist at 3</i>" {
		t.Fatalf("reading the reply: %d %s", code, body)
	}

	// The page rebuilds its sent list from the relay's recent chains.
	var recent struct {
		Traces []envelope.Result `json:"traces"`
	}
	if code, body := pageCall(t, page, "GET", "/v1/trace?limit=50&exclude_pings=true", "phone", nil, &recent); code != http.StatusOK {
		t.Fatalf("recent chains: %d %s", code, body)
	}
	var listed bool
	for _, r := range recent.Traces {
		listed = listed || (r.Request.ID == sent.ID && r.Request.From == "phone" && r.Status == envelope.StatusAnswered)
	}
	if !listed {
		t.Fatalf("the phone's ask is not in the recent chains: %+v", recent.Traces)
	}
}

// The phone answers a teammate's clarifying question from the page.
func TestPhoneAnswersNeedsInput(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	page := hardened(t, m, "admin")
	m.JoinOnMachineOf(t, "admin", "phone")
	var sent envelope.SendResponse
	if code, body := pageCall(t, page, "POST", "/v1/send", "phone", map[string]string{"to": "muse", "body": "book a table", "kind": "ask"}, &sent); code != http.StatusCreated {
		t.Fatalf("ask: %d %s", code, body)
	}
	muse := m.Client(t, "muse")
	if _, err := muse.Claim(t.Context(), sent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(t.Context(), sent.ID, "for how many?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	var res envelope.Result
	if code, body := pageCall(t, page, "GET", "/v1/requests/"+sent.ID, "phone", nil, &res); code != http.StatusOK || res.Status != envelope.StatusNeedsInput || len(res.Exchanges) != 1 || res.Exchanges[0].Question != "for how many?" {
		t.Fatalf("needs input: %d %s", code, body)
	}
	if code, body := pageCall(t, page, "POST", "/v1/requests/"+sent.ID+"/answer", "phone", map[string]string{"body": "two"}, nil); code != http.StatusOK {
		t.Fatalf("answer from the page: %d %s", code, body)
	}
}
