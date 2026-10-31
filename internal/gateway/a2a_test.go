package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

const museAndGrokbot = `{"teammates":[{"name":"muse","description":"Writes and edits."},{"name":"grokbot","description":"Reads the news."}]}`

// a2aSetup is setup with a2a.json holding cfg, as an owner who turned A2A on
// has it.
func a2aSetup(t *testing.T, cfg string) *env {
	t.Helper()
	e := setup(t)
	if err := os.WriteFile(e.a2a, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// guest connects name as a gateway guest for asks and logs it in.
func (e *env) guest(t *testing.T, name string, asks ...string) session {
	t.Helper()
	out, err := e.m.Client(t, "admin").ConnectGuest(t.Context(), name, client.GuestInvite{Ask: asks, ForSeconds: 3600})
	if err != nil {
		t.Fatalf("connect guest %s: %v", name, err)
	}
	s, _ := e.loginSession(t, out.Code)
	return s
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    []struct {
		Type   string `json:"@type"`
		Reason string `json:"reason"`
		Domain string `json:"domain"`
	} `json:"data"`
}

type a2aTask struct {
	ID        string `json:"id"`
	ContextID string `json:"contextId"`
	Status    struct {
		State   string `json:"state"`
		Message *struct {
			MessageID string `json:"messageId"`
			Role      string `json:"role"`
			TaskID    string `json:"taskId"`
			Parts     []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"message"`
	} `json:"status"`
	Artifacts []struct {
		ArtifactID string `json:"artifactId"`
		Parts      []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"artifacts"`
}

// post sends a raw body to teammate's endpoint.
func (e *env) post(t *testing.T, tok, teammate, version, body string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST", e.gw.URL+"/a2a/"+teammate, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if version != "" {
		req.Header.Set("A2A-Version", version)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

// rpcAt calls method at teammate's endpoint with version and returns its
// result or error. Any answer other than a 200 JSON-RPC response fails.
func (e *env) rpcAt(t *testing.T, tok, teammate, version, method string, params any) (json.RawMessage, *rpcError) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	status, raw, hdr := e.post(t, tok, teammate, version, string(body))
	if status != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("%s: status %d, type %q: %s", method, status, hdr.Get("Content-Type"), raw)
	}
	var out struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.JSONRPC != "2.0" || string(out.ID) != "7" {
		t.Fatalf("%s: bad response %s (%v)", method, raw, err)
	}
	return out.Result, out.Error
}

func (e *env) rpc(t *testing.T, tok, method string, params any) (json.RawMessage, *rpcError) {
	t.Helper()
	return e.rpcAt(t, tok, "muse", "1.0", method, params)
}

func wantCode(t *testing.T, got *rpcError, code int) {
	t.Helper()
	if got == nil || got.Code != code {
		t.Fatalf("error = %+v, want code %d", got, code)
	}
}

func textMessage(id, text string) map[string]any {
	return map[string]any{"messageId": id, "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}}
}

func decodeTask(t *testing.T, raw json.RawMessage) a2aTask {
	t.Helper()
	var task a2aTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("task %s: %v", raw, err)
	}
	return task
}

// send sends text as a new task and returns it. SendMessage's result wraps
// the task, unlike GetTask's.
func (e *env) send(t *testing.T, tok, text string) a2aTask {
	t.Helper()
	raw, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": textMessage("m-"+text, text)})
	if rerr != nil {
		t.Fatalf("SendMessage: %+v", rerr)
	}
	var out struct {
		Task *a2aTask `json:"task"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Task == nil {
		t.Fatalf("SendMessage result %s: %v", raw, err)
	}
	return *out.Task
}

func (e *env) get(t *testing.T, tok, id string) a2aTask {
	t.Helper()
	raw, rerr := e.rpc(t, tok, "GetTask", map[string]any{"id": id})
	if rerr != nil {
		t.Fatalf("GetTask: %+v", rerr)
	}
	return decodeTask(t, raw)
}

func wantState(t *testing.T, task a2aTask, state string) {
	t.Helper()
	if task.Status.State != state {
		t.Fatalf("task %s state = %s, want %s", task.ID, task.Status.State, state)
	}
}

func (e *env) approve(t *testing.T, id string) {
	t.Helper()
	if err := e.m.Client(t, "admin").Raw(t.Context(), "POST", "/v1/admin/requests/"+id+"/approve", nil, nil); err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
}

// claim has teammate poll for id and claim it.
func (e *env) claim(t *testing.T, teammate, id string) envelope.Request {
	t.Helper()
	c := e.m.Client(t, teammate)
	in, err := c.Poll(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range in.Requests {
		if q.ID == id {
			if _, err := c.Claim(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			return q
		}
	}
	t.Fatalf("%s was not delivered %s: %+v", teammate, id, in.Requests)
	return envelope.Request{}
}

func getCard(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// With no a2a.json nothing is published: the cards and the endpoints are not
// there, even for a good guest token, and the rest of the gateway is as it was.
func TestA2AOffWithoutFile(t *testing.T) {
	e := setup(t)
	s := e.guest(t, "pal", "muse")
	for _, p := range []string{"/.well-known/agent-card.json", "/a2a/muse/.well-known/agent-card.json"} {
		if status, raw := getCard(t, e.gw.URL+p); status != http.StatusNotFound {
			t.Errorf("%s: %d %s", p, status, raw)
		}
	}
	if status, raw, _ := e.post(t, s.AccessToken, "muse", "1.0", `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`); status != http.StatusNotFound {
		t.Errorf("endpoint: %d %s", status, raw)
	}
}

// A file the owner did not lock down or got wrong publishes nothing.
func TestA2ABadFilePublishesNothing(t *testing.T) {
	e := setup(t)
	for name, write := range map[string]func() error{
		"group readable": func() error {
			if err := os.WriteFile(e.a2a, []byte(museAndGrokbot), 0o600); err != nil {
				return err
			}
			return os.Chmod(e.a2a, 0o640)
		},
		"a symlink": func() error {
			target := e.a2a + ".real"
			if err := os.WriteFile(target, []byte(museAndGrokbot), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, e.a2a)
		},
		"malformed": func() error { return os.WriteFile(e.a2a, []byte(`{"teammates":[`), 0o600) },
		"unknown field": func() error {
			return os.WriteFile(e.a2a, []byte(`{"teammate":[{"name":"muse","description":"x"}]}`), 0o600)
		},
		"no description": func() error { return os.WriteFile(e.a2a, []byte(`{"teammates":[{"name":"muse"}]}`), 0o600) },
		"bad name": func() error {
			return os.WriteFile(e.a2a, []byte(`{"teammates":[{"name":"../muse","description":"x"}]}`), 0o600)
		},
		"listed twice": func() error {
			return os.WriteFile(e.a2a, []byte(`{"teammates":[{"name":"muse","description":"x"},{"name":"muse","description":"y"}]}`), 0o600)
		},
		"nobody is published": func() error { return os.WriteFile(e.a2a, []byte(`{"teammates":[]}`), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.Remove(e.a2a); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := write(); err != nil {
				t.Fatal(err)
			}
			if status, raw := getCard(t, e.gw.URL+"/a2a/muse/.well-known/agent-card.json"); status != http.StatusNotFound {
				t.Fatalf("card: %d %s", status, raw)
			}
		})
	}
}

// The file is read on each call, so removing it turns A2A off at once.
func TestA2ATurnsOffWithoutARestart(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	if status, raw := getCard(t, e.gw.URL+"/a2a/muse/.well-known/agent-card.json"); status != http.StatusOK {
		t.Fatalf("card: %d %s", status, raw)
	}
	if err := os.Remove(e.a2a); err != nil {
		t.Fatal(err)
	}
	if status, _ := getCard(t, e.gw.URL+"/a2a/muse/.well-known/agent-card.json"); status != http.StatusNotFound {
		t.Fatalf("card after removal: %d", status)
	}
}

// A teammate's card has every field A2A 1.0 requires, and says only what the
// owner wrote: nothing from the roster, and no teammate left unpublished.
func TestA2ACardShowsOnlyWhatIsPublished(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	e.guest(t, "pal", "muse") // a guest is a roster row the card must not show
	if _, err := e.m.Client(t, "admin").SetGoodAt(t.Context(), "muse", "private roster line"); err != nil {
		t.Fatal(err)
	}
	status, raw := getCard(t, e.gw.URL+"/a2a/muse/.well-known/agent-card.json")
	if status != http.StatusOK {
		t.Fatalf("card: %d %s", status, raw)
	}
	for _, bad := range []string{"instinct", "grokbot", "pal", "private roster line", "relay_key", "virtual:"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Errorf("card shows %q: %s", bad, raw)
		}
	}
	var card struct {
		Name                string `json:"name"`
		Description         string `json:"description"`
		Version             string `json:"version"`
		SupportedInterfaces []struct {
			URL             string `json:"url"`
			ProtocolBinding string `json:"protocolBinding"`
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"supportedInterfaces"`
		Capabilities       map[string]any `json:"capabilities"`
		DefaultInputModes  []string       `json:"defaultInputModes"`
		DefaultOutputModes []string       `json:"defaultOutputModes"`
		Skills             []struct {
			ID, Name, Description string
			Tags                  []string
		} `json:"skills"`
		SecuritySchemes map[string]struct {
			OAuth2 *struct {
				Flows struct {
					AuthorizationCode *struct {
						AuthorizationURL string            `json:"authorizationUrl"`
						TokenURL         string            `json:"tokenUrl"`
						RefreshURL       string            `json:"refreshUrl"`
						Scopes           map[string]string `json:"scopes"`
						PkceRequired     bool              `json:"pkceRequired"`
					} `json:"authorizationCode"`
				} `json:"flows"`
				MetadataURL string `json:"oauth2MetadataUrl"`
			} `json:"oauth2SecurityScheme"`
		} `json:"securitySchemes"`
		SecurityRequirements []struct {
			Schemes map[string]struct {
				List []string `json:"list"`
			} `json:"schemes"`
		} `json:"securityRequirements"`
	}
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("card %s: %v", raw, err)
	}
	if card.Name != "muse" || card.Description != "Writes and edits." || card.Version != "1.0" {
		t.Errorf("card = %+v", card)
	}
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].URL != e.gw.URL+"/a2a/muse" ||
		card.SupportedInterfaces[0].ProtocolBinding != "JSONRPC" || card.SupportedInterfaces[0].ProtocolVersion != "1.0" {
		t.Errorf("interfaces = %+v", card.SupportedInterfaces)
	}
	if card.Capabilities == nil || card.Capabilities["streaming"] != false || card.Capabilities["pushNotifications"] != false {
		t.Errorf("capabilities = %v", card.Capabilities)
	}
	if len(card.DefaultInputModes) != 1 || card.DefaultInputModes[0] != "text/plain" || len(card.DefaultOutputModes) != 1 || card.DefaultOutputModes[0] != "text/plain" {
		t.Errorf("modes = %v %v", card.DefaultInputModes, card.DefaultOutputModes)
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "muse" || card.Skills[0].Description != "Writes and edits." || len(card.Skills[0].Tags) == 0 {
		t.Errorf("skills = %+v", card.Skills)
	}
	scheme, ok := card.SecuritySchemes["oauth2"]
	if !ok || scheme.OAuth2 == nil || scheme.OAuth2.Flows.AuthorizationCode == nil {
		t.Fatalf("security schemes = %s", raw)
	}
	flow := scheme.OAuth2.Flows.AuthorizationCode
	if flow.AuthorizationURL != e.gw.URL+"/authorize" || flow.TokenURL != e.gw.URL+"/token" || flow.RefreshURL != e.gw.URL+"/token" ||
		flow.Scopes == nil || !flow.PkceRequired || scheme.OAuth2.MetadataURL != e.gw.URL+"/.well-known/oauth-authorization-server" {
		t.Errorf("oauth flow = %+v, metadata %q", flow, scheme.OAuth2.MetadataURL)
	}
	if len(card.SecurityRequirements) != 1 {
		t.Fatalf("security requirements = %+v", card.SecurityRequirements)
	}
	if _, ok := card.SecurityRequirements[0].Schemes["oauth2"]; !ok {
		t.Errorf("security requirements = %+v", card.SecurityRequirements)
	}
}

// The root card lists each published teammate as a skill, with its endpoint.
func TestA2ARootCardListsPublishedTeammates(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	status, raw := getCard(t, e.gw.URL+"/.well-known/agent-card.json")
	if status != http.StatusOK {
		t.Fatalf("card: %d %s", status, raw)
	}
	var card struct {
		SupportedInterfaces []struct {
			URL string `json:"url"`
		} `json:"supportedInterfaces"`
		Skills []struct {
			ID string `json:"id"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	if len(card.Skills) != 2 || card.Skills[0].ID != "muse" || card.Skills[1].ID != "grokbot" {
		t.Errorf("skills = %+v", card.Skills)
	}
	if len(card.SupportedInterfaces) != 2 || card.SupportedInterfaces[0].URL != e.gw.URL+"/a2a/muse" || card.SupportedInterfaces[1].URL != e.gw.URL+"/a2a/grokbot" {
		t.Errorf("interfaces = %+v", card.SupportedInterfaces)
	}
	if bytes.Contains(raw, []byte("instinct")) {
		t.Errorf("root card shows an unpublished teammate: %s", raw)
	}
}

// A client logs in, refreshes its token, and follows one task through each
// state to the answer.
func TestA2AClientFollowsATaskToItsAnswer(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	s := e.guest(t, "pal", "muse")
	fresh, status := e.refresh(t, s.RefreshToken, s.ClientID)
	if status != http.StatusOK {
		t.Fatalf("refresh: %d", status)
	}
	tok := fresh.AccessToken

	task := e.send(t, tok, "what is in the report")
	wantState(t, task, "TASK_STATE_SUBMITTED") // held for the owner
	if task.ID == "" || task.ContextID == "" {
		t.Fatalf("task = %+v", task)
	}
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_SUBMITTED")

	e.approve(t, task.ID)
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_SUBMITTED") // queued

	muse := e.m.Client(t, "muse")
	in, err := muse.Poll(t.Context(), 0)
	if err != nil || len(in.Requests) != 1 || in.Requests[0].ID != task.ID {
		t.Fatalf("muse got %+v, %v", in.Requests, err)
	}
	got := in.Requests[0]
	if got.From != "pal" || !got.FromGuest || !got.GuestApproved || !strings.HasPrefix(got.Body, envelope.GuestLine) {
		t.Fatalf("muse's request = %+v", got)
	}
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_WORKING") // delivered
	if _, err := muse.Claim(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_WORKING") // claimed

	if _, err := muse.Reply(t.Context(), task.ID, "it says yes", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	done := e.get(t, tok, task.ID)
	wantState(t, done, "TASK_STATE_COMPLETED")
	if len(done.Artifacts) != 1 || done.Artifacts[0].ArtifactID == "" || len(done.Artifacts[0].Parts) != 1 || done.Artifacts[0].Parts[0].Text != "it says yes" {
		t.Fatalf("done = %+v", done)
	}
	if _, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": map[string]any{
		"messageId": "m9", "role": "ROLE_USER", "taskId": task.ID, "parts": []any{map[string]any{"text": "and?"}},
	}}); rerr == nil || rerr.Code != -32004 {
		t.Fatalf("message to a finished task: %+v", rerr)
	}
}

// Each way a task ends has the state the plan names.
func TestA2ATerminalStates(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	muse := e.m.Client(t, "muse")

	task := e.send(t, tok, "fails")
	e.approve(t, task.ID)
	e.claim(t, "muse", task.ID)
	if _, err := muse.Reply(t.Context(), task.ID, "cannot do that", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	failed := e.get(t, tok, task.ID)
	wantState(t, failed, "TASK_STATE_FAILED")
	if failed.Status.Message == nil || failed.Status.Message.Role != "ROLE_AGENT" || failed.Status.Message.MessageID == "" ||
		len(failed.Status.Message.Parts) != 1 || failed.Status.Message.Parts[0].Text != "cannot do that" {
		t.Fatalf("failed = %+v", failed)
	}

	task = e.send(t, tok, "declined")
	e.approve(t, task.ID)
	e.claim(t, "muse", task.ID)
	if _, err := muse.Reply(t.Context(), task.ID, "not for me", envelope.StatusDeclined); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_REJECTED")

	task = e.send(t, tok, "expires")
	if _, err := e.m.Store.DB().Exec(`UPDATE requests SET status = 'expired' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_FAILED")
}

// A task in needs_input takes the client's next message as the answer. That
// answer is outside input the owner did not see, so the teammate's next ask
// is marked and held, over A2A as for any guest.
func TestA2ANeedsInputContinues(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	muse := e.m.Client(t, "muse")

	task := e.send(t, tok, "book it")
	e.approve(t, task.ID)
	e.claim(t, "muse", task.ID)
	if _, err := muse.Reply(t.Context(), task.ID, "which day?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	waiting := e.get(t, tok, task.ID)
	wantState(t, waiting, "TASK_STATE_INPUT_REQUIRED")
	if waiting.Status.Message == nil || len(waiting.Status.Message.Parts) != 1 || waiting.Status.Message.Parts[0].Text != "which day?" {
		t.Fatalf("question = %+v", waiting)
	}

	msg := textMessage("m2", "tuesday")
	msg["taskId"], msg["contextId"] = task.ID, "some-other-context"
	if _, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": msg}); rerr == nil || rerr.Code != -32602 {
		t.Fatalf("mismatched context: %+v", rerr)
	}
	msg["contextId"] = waiting.ContextID
	raw, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": msg, "configuration": map[string]any{"returnImmediately": true}})
	if rerr != nil {
		t.Fatalf("answer: %+v", rerr)
	}
	var out struct {
		Task a2aTask `json:"task"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Task.ID != task.ID {
		t.Fatalf("answer result %s: %v", raw, err)
	}
	wantState(t, out.Task, "TASK_STATE_SUBMITTED")

	again := e.claim(t, "muse", task.ID)
	if !again.Resumed || len(again.Exchanges) != 1 || !strings.Contains(again.Exchanges[0].Answer, "tuesday") || !strings.Contains(again.Exchanges[0].Answer, "<<<") {
		t.Fatalf("resumed = %+v", again)
	}
	next, err := muse.Send(t.Context(), "grokbot", "look this up", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !next.FromGuest || next.GuestApproved || next.Status != envelope.StatusHeld {
		t.Fatalf("muse's next ask = %+v", next)
	}
}

// A blocking SendMessage waits for the teammate, and hands back whatever
// state the task has reached when the wait runs out.
func TestA2ABlockingAnswerWaitsForTheReply(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	muse := e.m.Client(t, "muse")
	task := e.send(t, tok, "book it")
	e.approve(t, task.ID)
	e.claim(t, "muse", task.ID)
	if _, err := muse.Reply(t.Context(), task.ID, "which day?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		for range 40 {
			in, err := muse.Poll(ctx, 0)
			if err != nil {
				done <- err
				return
			}
			if len(in.Requests) == 1 {
				if _, err := muse.Claim(ctx, task.ID); err != nil {
					done <- err
					return
				}
				_, err := muse.Reply(ctx, task.ID, "booked", envelope.StatusAnswered)
				done <- err
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		done <- context.DeadlineExceeded
	}()
	msg := textMessage("m2", "tuesday")
	msg["taskId"] = task.ID
	raw, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": msg})
	if rerr != nil {
		t.Fatalf("answer: %+v", rerr)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var out struct {
		Task a2aTask `json:"task"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Task.Status.State != "TASK_STATE_COMPLETED" {
		// The wait is the relay's MaxWait; on a loaded machine it can run out.
		wantState(t, e.get(t, tok, task.ID), "TASK_STATE_COMPLETED")
	}
}

// A connected agent that is not a guest gets nowhere on A2A, and a gateway
// guest gets no MCP tools: it is an A2A client and nothing else.
func TestA2AOnlyGuests(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	code, _, err := e.conn.Connect(context.Background(), "chatgpt", false)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.login(t, code)
	status, raw, _ := e.post(t, tok, "muse", "1.0", `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{}}`)
	if status != http.StatusForbidden {
		t.Fatalf("teammate token: %d %s", status, raw)
	}
	if n := countRows(t, e, `SELECT count(*) FROM requests`); n != 0 {
		t.Fatalf("%d requests after a refused token", n)
	}
	// The teammate's own MCP session still works.
	if res, err := e.mcpSession(t, tok).CallTool(t.Context(), &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("teammate's MCP: %v %+v", err, res)
	}

	g := e.guest(t, "pal", "muse")
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.gw.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+g.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("guest on MCP: %d", resp.StatusCode)
	}
}

func TestA2ANeedsAToken(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	for _, tok := range []string{"", "made-up"} {
		status, _, hdr := e.post(t, tok, "muse", "1.0", `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`)
		if status != http.StatusUnauthorized || !strings.Contains(hdr.Get("WWW-Authenticate"), "resource_metadata=") {
			t.Fatalf("token %q: %d, %q", tok, status, hdr.Get("WWW-Authenticate"))
		}
	}
}

// One guest's task id is not found for another, nor is a teammate's request,
// nor is a task at another teammate's endpoint.
func TestA2ATasksBelongToTheirGuest(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	a := e.guest(t, "pal", "muse", "grokbot").AccessToken
	b := e.guest(t, "buddy", "muse").AccessToken
	task := e.send(t, a, "mine")
	teammates, err := e.m.Client(t, "muse").Send(t.Context(), "grokbot", "private", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GetTask", "CancelTask"} {
		_, rerr := e.rpc(t, b, method, map[string]any{"id": task.ID})
		wantCode(t, rerr, -32001)
		if len(rerr.Data) != 1 || rerr.Data[0].Reason != "TASK_NOT_FOUND" || rerr.Data[0].Domain != "a2a-protocol.org" || rerr.Data[0].Type != "type.googleapis.com/google.rpc.ErrorInfo" {
			t.Fatalf("error data = %+v", rerr.Data)
		}
		_, rerr = e.rpc(t, a, method, map[string]any{"id": teammates.ID})
		wantCode(t, rerr, -32001)
		_, rerr = e.rpcAt(t, a, "grokbot", "1.0", method, map[string]any{"id": task.ID})
		wantCode(t, rerr, -32001)
	}
	msg := textMessage("m", "barging in")
	msg["taskId"] = task.ID
	_, rerr := e.rpc(t, b, "SendMessage", map[string]any{"message": msg})
	wantCode(t, rerr, -32001)
}

func TestA2AVersionHeader(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	params := map[string]any{"message": textMessage("m1", "hi")}
	for _, v := range []string{"", "0.3", "2.0", "1.0x"} {
		_, rerr := e.rpcAt(t, tok, "muse", v, "SendMessage", params)
		wantCode(t, rerr, -32009)
	}
	if n := countRows(t, e, `SELECT count(*) FROM requests`); n != 0 {
		t.Fatalf("%d requests sent past a bad version", n)
	}
	for _, v := range []string{"1.0", "1.0.0"} {
		if _, rerr := e.rpcAt(t, tok, "muse", v, "GetTask", map[string]any{"id": "nope"}); rerr == nil || rerr.Code != -32001 {
			t.Fatalf("version %s: %+v", v, rerr)
		}
	}
}

func TestA2ARefusesAnythingButText(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	for name, part := range map[string]map[string]any{
		"raw":            {"raw": "aGk=", "filename": "a.png", "mediaType": "image/png"},
		"url":            {"url": "https://example.com/a.png"},
		"data":           {"data": map[string]any{"a": 1}},
		"text and a url": {"text": "hi", "url": "https://example.com/a.png"},
	} {
		msg := map[string]any{"messageId": "m", "role": "ROLE_USER", "parts": []any{map[string]any{"text": "look"}, part}}
		_, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": msg})
		if rerr == nil || rerr.Code != -32005 {
			t.Errorf("%s part: %+v", name, rerr)
		}
	}
	for name, msg := range map[string]map[string]any{
		"no message id": {"role": "ROLE_USER", "parts": []any{map[string]any{"text": "hi"}}},
		"agent role":    {"messageId": "m", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "hi"}}},
		"no role":       {"messageId": "m", "parts": []any{map[string]any{"text": "hi"}}},
		"no parts":      {"messageId": "m", "role": "ROLE_USER"},
		"blank text":    {"messageId": "m", "role": "ROLE_USER", "parts": []any{map[string]any{"text": "  "}}},
	} {
		_, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": msg})
		if rerr == nil || rerr.Code != -32602 {
			t.Errorf("%s: %+v", name, rerr)
		}
	}
	_, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": textMessage("m", "hi"),
		"configuration": map[string]any{"taskPushNotificationConfig": map[string]any{"url": "https://example.com/hook"}}})
	wantCode(t, rerr, -32003)
	if n := countRows(t, e, `SELECT count(*) FROM requests`); n != 0 {
		t.Fatalf("%d requests sent past a refused message", n)
	}
}

// Multiple text parts are one body, and nothing else in a message reaches
// the relay's send, where a guest would be refused any field but to and body.
func TestA2AOnlyTextReachesTheRelay(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	raw, rerr := e.rpc(t, tok, "SendMessage", map[string]any{
		"metadata": map[string]any{"urgent": true, "then": "instinct"},
		"message": map[string]any{"messageId": "m", "role": "ROLE_USER", "metadata": map[string]any{"parent_id": "x"}, "referenceTaskIds": []string{"x"},
			"parts": []any{map[string]any{"text": "one"}, map[string]any{"text": "two"}}},
	})
	if rerr != nil {
		t.Fatalf("send: %+v", rerr)
	}
	var out struct {
		Task a2aTask `json:"task"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	e.approve(t, out.Task.ID)
	got := e.claim(t, "muse", out.Task.ID)
	if !strings.Contains(got.Body, "one\ntwo") || got.Urgent || got.ParentID != "" {
		t.Fatalf("muse got %+v", got)
	}
}

func TestA2AMethodsAndEnvelope(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	for _, m := range []string{"SendStreamingMessage", "ListTasks", "SubscribeToTask", "CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig",
		"ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig", "GetExtendedAgentCard"} {
		_, rerr := e.rpc(t, tok, m, map[string]any{})
		wantCode(t, rerr, -32004)
	}
	_, rerr := e.rpc(t, tok, "tasks/send", map[string]any{})
	wantCode(t, rerr, -32601)
	for body, code := range map[string]int{
		`not json`: -32700,
		`[{"jsonrpc":"2.0","id":1,"method":"GetTask"}]`:                  -32600,
		`{"jsonrpc":"2.0","method":"GetTask"}`:                           -32600,
		`{"jsonrpc":"1.0","id":1,"method":"GetTask"}`:                    -32600,
		`{"jsonrpc":"2.0","id":1,"method":"GetTask"}`:                    -32602,
		`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":""}}`: -32602,
	} {
		status, raw, _ := e.post(t, tok, "muse", "1.0", body)
		var out struct {
			Error *rpcError `json:"error"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || status != http.StatusOK || out.Error == nil || out.Error.Code != code {
			t.Errorf("%s: %d %s, want %d", body, status, raw, code)
		}
	}

	// A send that is not exactly one well-formed call makes no task, and a bad
	// id does not come back in the answer.
	const send = `"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}]},"configuration":{"returnImmediately":true}}}`
	for body, code := range map[string]int{
		`{"jsonrpc":"2.0","id":1,` + send + `garbage`:                  -32700,
		`{"jsonrpc":"2.0","id":1,` + send + `{"jsonrpc":"2.0","id":2}`: -32700,
		`{"jsonrpc":"2.0","id":{"a":1},` + send:                        -32600,
		`{"jsonrpc":"2.0","id":[1],` + send:                            -32600,
		`{"jsonrpc":"2.0","id":true,` + send:                           -32600,
	} {
		status, raw, _ := e.post(t, tok, "muse", "1.0", body)
		var out struct {
			ID    json.RawMessage `json:"id"`
			Error *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || status != http.StatusOK || out.Error == nil || out.Error.Code != code || string(out.ID) != "null" {
			t.Errorf("%s: %d %s, want %d and a null id", body, status, raw, code)
		}
	}
	if got := countRows(t, e, `SELECT count(*) FROM requests`); got != 0 {
		t.Fatalf("malformed sends made %d tasks", got)
	}

	// A string id, and whitespace after the call, are fine.
	status, raw, _ := e.post(t, tok, "muse", "1.0", `{"jsonrpc":"2.0","id":"a",`+send+"\n")
	var out struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Task a2aTask `json:"task"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || status != http.StatusOK || string(out.ID) != `"a"` || out.Result.Task.ID == "" {
		t.Fatalf("well-formed send: %d %s", status, raw)
	}
}

// An unpublished teammate has no endpoint, even for a guest listed for it,
// and a published teammate the guest is not listed for refuses it. a2a.json
// grants nothing: only the guest's own list does.
func TestA2APublishedAndListedAreSeparate(t *testing.T) {
	e := a2aSetup(t, `{"teammates":[{"name":"muse","description":"Writes."},{"name":"grokbot","description":"News."}]}`)
	tok := e.guest(t, "pal", "muse", "instinct").AccessToken
	status, raw, _ := e.post(t, tok, "instinct", "1.0", `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`)
	if status != http.StatusNotFound {
		t.Fatalf("unpublished: %d %s", status, raw)
	}
	if status, raw := getCard(t, e.gw.URL+"/a2a/instinct/.well-known/agent-card.json"); status != http.StatusNotFound {
		t.Fatalf("unpublished card: %d %s", status, raw)
	}
	_, rerr := e.rpcAt(t, tok, "grokbot", "1.0", "SendMessage", map[string]any{"message": textMessage("m", "hi")})
	wantCode(t, rerr, -32004)
	if n := countRows(t, e, `SELECT count(*) FROM requests`); n != 0 {
		t.Fatalf("%d requests after a refused send", n)
	}
}

// A task can be withdrawn until a teammate has been given it. Once it has
// been, withdrawing would end the claim the guest mark depends on while the
// teammate may be acting on what it read.
func TestA2ACancelStopsAtDelivery(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken

	task := e.send(t, tok, "never mind")
	raw, rerr := e.rpc(t, tok, "CancelTask", map[string]any{"id": task.ID})
	if rerr != nil {
		t.Fatalf("cancel held: %+v", rerr)
	}
	wantState(t, decodeTask(t, raw), "TASK_STATE_CANCELED")
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_CANCELED")
	_, rerr = e.rpc(t, tok, "CancelTask", map[string]any{"id": task.ID})
	wantCode(t, rerr, -32002)

	task = e.send(t, tok, "keep going")
	e.approve(t, task.ID)
	in, err := e.m.Client(t, "muse").Poll(t.Context(), 0)
	if err != nil || len(in.Requests) != 1 {
		t.Fatalf("poll: %+v %v", in.Requests, err)
	}
	_, rerr = e.rpc(t, tok, "CancelTask", map[string]any{"id": task.ID})
	wantCode(t, rerr, -32002)
	if _, err := e.m.Client(t, "muse").Claim(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	_, rerr = e.rpc(t, tok, "CancelTask", map[string]any{"id": task.ID})
	wantCode(t, rerr, -32002)
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_WORKING")
	marked, _, err := e.m.Store.OpenGuestClaims(t.Context(), "muse")
	if err != nil || marked != 1 {
		t.Fatalf("open marked claims = %d, %v", marked, err)
	}
}

// A task that went back to the queue after a teammate had it stays out of the
// client's reach: cancelling would stop the claim that marks what the teammate
// sends from ever coming back.
func TestA2ACancelStopsAfterAnAnswer(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	task := e.send(t, tok, "book it")
	e.approve(t, task.ID)
	e.claim(t, "muse", task.ID)
	if _, err := e.m.Client(t, "muse").Reply(t.Context(), task.ID, "which day?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	msg := textMessage("m2", "tuesday")
	msg["taskId"] = task.ID
	if _, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": msg, "configuration": map[string]any{"returnImmediately": true}}); rerr != nil {
		t.Fatalf("answer: %+v", rerr)
	}
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_SUBMITTED")
	_, rerr := e.rpc(t, tok, "CancelTask", map[string]any{"id": task.ID})
	wantCode(t, rerr, -32002)
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_SUBMITTED")
	if again := e.claim(t, "muse", task.ID); !again.FromGuest {
		t.Fatalf("claim after the refused cancel = %+v", again)
	}
}

func TestA2ACancelStopsAfterALapsedClaim(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := setupWith(t, relay.Config{MaxWait: 3 * time.Second, Now: clock, ClaimLease: time.Minute})
	e.m.Store.SetClock(clock)
	if err := os.WriteFile(e.a2a, []byte(museAndGrokbot), 0o600); err != nil {
		t.Fatal(err)
	}
	tok := e.guest(t, "pal", "muse").AccessToken
	task := e.send(t, tok, "do the thing")
	e.approve(t, task.ID)
	e.claim(t, "muse", task.ID)
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	e.m.Server.Sweep(t.Context())
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_SUBMITTED")
	_, rerr := e.rpc(t, tok, "CancelTask", map[string]any{"id": task.ID})
	wantCode(t, rerr, -32002)
	wantState(t, e.get(t, tok, task.ID), "TASK_STATE_SUBMITTED")
}

// A task can be continued only while its question waits.
func TestA2ASendToATaskNotWaiting(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	task := e.send(t, tok, "first")
	msg := textMessage("m2", "more")
	msg["taskId"] = task.ID
	_, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": msg})
	wantCode(t, rerr, -32004)
}

// Nothing a caller controls on the public side reaches GET /v1/hello: the
// gateway serves no such route, and a task id stays inside one path segment.
// A proof fetched here could be passed off to a teammate looking for a moved
// relay.
func TestA2AGetsNoHelloProof(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	tok := e.guest(t, "pal", "muse").AccessToken
	const nonce = "0123456789abcdef"
	for _, p := range []string{"/v1/hello?nonce=" + nonce, "/a2a/v1/hello?nonce=" + nonce, "/a2a/muse/v1/hello?nonce=" + nonce, "/a2a/../v1/hello?nonce=" + nonce} {
		resp, err := http.Get(e.gw.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if bytes.Contains(raw, []byte("proof")) {
			t.Errorf("%s answered a proof: %s", p, raw)
		}
	}
	for _, id := range []string{"../hello?nonce=" + nonce, "..%2Fhello?nonce=" + nonce, "../hello", "../../v1/hello", "x/../../hello", "a b", strings.Repeat("a", 300)} {
		for _, method := range []string{"GetTask", "CancelTask"} {
			raw, rerr := e.rpc(t, tok, method, map[string]any{"id": id})
			wantCode(t, rerr, -32001)
			if bytes.Contains(raw, []byte("proof")) || strings.Contains(rerr.Message, "proof") {
				t.Errorf("%s %q: %s %+v", method, id, raw, rerr)
			}
		}
		msg := textMessage("m", "hi")
		msg["taskId"] = id
		_, rerr := e.rpc(t, tok, "SendMessage", map[string]any{"message": msg})
		wantCode(t, rerr, -32001)
	}
}

// When the term ends the relay refuses every call at once, before any sweep.
// The sweep then revokes the tokens and cancels what was open.
func TestA2AGuestTermEnds(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	s := e.guest(t, "pal", "muse")
	task := e.send(t, s.AccessToken, "still there?")
	if _, err := e.m.Store.DB().Exec(`UPDATE agents SET guest_until = ? WHERE name = 'pal'`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"` + task.ID + `"}}`
	if status, raw, _ := e.post(t, s.AccessToken, "muse", "1.0", body); status != http.StatusForbidden {
		t.Fatalf("past term: %d %s", status, raw)
	}
	e.m.Server.Sweep(t.Context())
	if status, raw, _ := e.post(t, s.AccessToken, "muse", "1.0", body); status != http.StatusUnauthorized {
		t.Fatalf("after sweep: %d %s", status, raw)
	}
	if _, status := e.refresh(t, s.RefreshToken, s.ClientID); status != http.StatusBadRequest {
		t.Fatalf("refresh after sweep: %d", status)
	}
	var st string
	if err := e.m.Store.DB().QueryRow(`SELECT status FROM requests WHERE id = ?`, task.ID).Scan(&st); err != nil || st != "cancelled" {
		t.Fatalf("task after sweep: %s %v", st, err)
	}
}

// A gateway guest is a guest at the relay, by its virtual address alone: the
// gate, the send field check and the roster cut all apply, and a call cannot
// claim to be anyone else.
func TestGatewayGuestIsAGuestAtTheRelay(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	e.guest(t, "pal", "muse")
	call := func(method, path, agent, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = identity.VirtualAddr("pal")
		if agent != "" {
			req.Header.Set(client.AgentHeader, agent)
		}
		rec := httptest.NewRecorder()
		e.m.Server.Handler().ServeHTTP(rec, req)
		return rec
	}
	for _, r := range [][2]string{{"GET", "/v1/admin/held"}, {"POST", "/v1/admin/invite"}, {"GET", "/v1/rooms"}, {"POST", "/v1/join"}} {
		if rec := call(r[0], r[1], "", "{}"); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: %d %s", r[0], r[1], rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{"to":"muse","body":"hi","urgent":true}`, `{"to":"muse","body":"hi","then":{"to":"instinct","body":"x"}}`, `{"to":"instinct","body":"hi"}`} {
		if rec := call("POST", "/v1/send", "", body); rec.Code != http.StatusForbidden {
			t.Errorf("send %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := call("GET", "/v1/whoami", "muse", ""); rec.Code != http.StatusForbidden {
		t.Errorf("whoami claiming muse: %d %s", rec.Code, rec.Body)
	}
	rec := call("GET", "/v1/whoami", "", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "relay_key") || strings.Contains(rec.Body.String(), "relay_urls") {
		t.Errorf("whoami: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "/v1/agents", "", ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "instinct") {
		t.Errorf("roster: %d %s", rec.Code, rec.Body)
	}
}

// Teammates' record leaves out a gateway guest, as their roster does.
func TestGatewayGuestNotInTeammatesStats(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	e.guest(t, "pal", "muse")
	var report client.StatsReport
	if err := e.m.Client(t, "instinct").Raw(t.Context(), "GET", "/v1/stats", nil, &report); err != nil {
		t.Fatal(err)
	}
	for _, a := range report.Agents {
		if a.Agent == "pal" {
			t.Fatalf("stats = %+v", report.Agents)
		}
	}
}

// The relay itself refuses what would make a gateway guest more than a held
// guest, whatever a client sends. The CLI never sends these.
func TestConnectGuestRefusesNoHoldAndReplace(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	admin := e.m.Client(t, "admin")
	terms := client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600}
	noHold := terms
	noHold.NoHold = true
	if _, err := admin.ConnectGuest(t.Context(), "pal", noHold); !client.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("no_hold: %v", err)
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/connect", map[string]any{"name": "pal", "replace": true, "guest": terms}, nil); !client.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("replace: %v", err)
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/connect", map[string]any{"name": "muse", "replace": true, "guest": terms}, nil); !client.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("replace over a teammate: %v", err)
	}
	if _, ok, _ := e.m.Dir.Agent(t.Context(), "pal"); ok {
		t.Fatal("a refused connect left pal behind")
	}
	if a, ok, _ := e.m.Dir.Agent(t.Context(), "muse"); !ok || a.Guest.Set() {
		t.Fatalf("muse = %+v, %v", a, ok)
	}
}

// A connect that could not mint a code gave nobody a way in, so it must not
// keep the name: the owner's retry would be refused as taken.
func TestConnectGuestFreesTheNameWhenNoCodeIsMade(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	db := e.m.Store.DB()
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER no_codes BEFORE INSERT ON gw_codes BEGIN SELECT RAISE(ABORT, 'no codes'); END`); err != nil {
		t.Fatal(err)
	}
	admin := e.m.Client(t, "admin")
	terms := client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600}
	if _, err := admin.ConnectGuest(t.Context(), "pal", terms); err == nil {
		t.Fatal("connect succeeded with no code")
	}
	if _, ok, _ := e.m.Dir.Agent(t.Context(), "pal"); ok {
		t.Fatal("a failed connect left pal behind")
	}
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER no_codes`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ConnectGuest(t.Context(), "pal", terms); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// A removal on a relay with no gateway leaves tokens on disk. They must not
// come back to life when the name is connected again, least of all as a
// teammate's.
func TestConnectDropsAFormerGuestsTokens(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	s := e.guest(t, "pal", "muse")
	if _, err := e.m.Store.DeleteAgent(t.Context(), "pal"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.conn.Connect(t.Context(), "pal", false); err != nil {
		t.Fatal(err)
	}
	if _, status := e.refresh(t, s.RefreshToken, s.ClientID); status == http.StatusOK {
		t.Fatal("a former guest's refresh token works for the teammate now holding the name")
	}
	status, raw, _ := e.post(t, s.AccessToken, "muse", "1.0", `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("old access token: %d %s", status, raw)
	}
}

// tincan remove cuts a gateway guest off and frees the name.
func TestA2ARemoveEndsAGuest(t *testing.T) {
	e := a2aSetup(t, museAndGrokbot)
	s := e.guest(t, "pal", "muse")
	if err := e.m.Client(t, "admin").Remove(t.Context(), "pal"); err != nil {
		t.Fatal(err)
	}
	status, raw, _ := e.post(t, s.AccessToken, "muse", "1.0", `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("after remove: %d %s", status, raw)
	}
	if _, err := e.m.Client(t, "admin").ConnectGuest(t.Context(), "pal", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600}); err != nil {
		t.Fatalf("name not freed: %v", err)
	}
}

// A guest's requests carry the mark for good, but what a teammate sends
// carries it only while the claim is open. These are the limits
// docs/trust-model.md states for A2A, pinned so a change to them is seen.
func TestA2AMarkEndsWithTheClaim(t *testing.T) {
	askAfter := func(t *testing.T, end func(e *env, muse *client.Relay, id string)) envelope.Request {
		t.Helper()
		e := a2aSetup(t, museAndGrokbot)
		tok := e.guest(t, "pal", "muse").AccessToken
		muse := e.m.Client(t, "muse")
		task := e.send(t, tok, "do the thing")
		e.approve(t, task.ID)
		e.claim(t, "muse", task.ID)
		during, err := muse.Send(t.Context(), "grokbot", "while it is open", envelope.KindAsk, "", false)
		if err != nil || !during.FromGuest {
			t.Fatalf("ask under the claim = %+v, %v", during, err)
		}
		end(e, muse, task.ID)
		after, err := muse.Send(t.Context(), "instinct", "afterwards", envelope.KindAsk, "", false)
		if err != nil {
			t.Fatal(err)
		}
		return after
	}
	for name, end := range map[string]func(e *env, muse *client.Relay, id string){
		"after the reply": func(_ *env, muse *client.Relay, id string) {
			if _, err := muse.Reply(context.Background(), id, "done", envelope.StatusAnswered); err != nil {
				panic(err)
			}
		},
		"during needs_input": func(_ *env, muse *client.Relay, id string) {
			if _, err := muse.Reply(context.Background(), id, "which?", envelope.StatusNeedsInput); err != nil {
				panic(err)
			}
		},
		"after tincan remove": func(e *env, _ *client.Relay, _ string) {
			if err := e.m.Client(t, "admin").Remove(context.Background(), "pal"); err != nil {
				panic(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			if after := askAfter(t, end); after.FromGuest || after.Status == envelope.StatusHeld {
				t.Fatalf("the limit moved: %+v. Update docs/trust-model.md", after)
			}
		})
	}
}
