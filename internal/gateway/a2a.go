package gateway

// A2A publishes chosen teammates to A2A 1.0 clients, over JSON-RPC only: no
// streaming, push notifications or attachments. Each published teammate has
// its own card and endpoint, so a standard client needs no private field.
//
// This file adds no rights. a2a.json says which teammates have a card and an
// endpoint, and grants nothing: every call is a relay call by a guest, through
// the relay's own handler, so guestGate and the send checks decide what it may
// do, on every call. A guest's own list decides whom it may ask. See
// docs/a2a.md and docs/trust-model.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

const (
	a2aVersion = "1.0"
	a2aMaxFile = 1 << 20
	// a2aWait bounds a blocking SendMessage. The spec has it wait for a final
	// or interrupted state, but a teammate can take days, so past this it
	// returns the state so far and the client polls GetTask.
	a2aWait = client.DefaultPollHold
)

// JSON-RPC and A2A error codes.
const (
	rpcParse          = -32700
	rpcInvalidRequest = -32600
	rpcNoMethod       = -32601
	rpcInvalidParams  = -32602
	rpcInternal       = -32603
	a2aTaskNotFound   = -32001
	a2aNotCancelable  = -32002
	a2aNoPush         = -32003
	a2aUnsupported    = -32004
	a2aContentType    = -32005
	a2aBadVersion     = -32009
)

// a2aReasons are the ErrorInfo reasons of the A2A errors. Only TASK_NOT_FOUND
// is spelled out in the spec's example; the rest follow its error names.
var a2aReasons = map[int]string{
	a2aTaskNotFound:  "TASK_NOT_FOUND",
	a2aNotCancelable: "TASK_NOT_CANCELABLE",
	a2aNoPush:        "PUSH_NOTIFICATION_NOT_SUPPORTED",
	a2aUnsupported:   "UNSUPPORTED_OPERATION",
	a2aContentType:   "CONTENT_TYPE_NOT_SUPPORTED",
	a2aBadVersion:    "VERSION_NOT_SUPPORTED",
}

// a2aOtherMethods are A2A 1.0 methods this binding does not serve. Any other
// unknown name is not an A2A method at all.
var a2aOtherMethods = map[string]bool{
	"SendStreamingMessage": true, "ListTasks": true, "SubscribeToTask": true, "CreateTaskPushNotificationConfig": true,
	"GetTaskPushNotificationConfig": true, "ListTaskPushNotificationConfigs": true, "DeleteTaskPushNotificationConfig": true,
	"GetExtendedAgentCard": true,
}

// a2aTaskID keeps a caller's id inside one relay path segment, whatever the
// client's escaping does. Nothing shorter or stranger is a request id.
var a2aTaskID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// a2aTeammate is one published teammate. Its description goes on public cards.
type a2aTeammate struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// loadA2A reads a2a.json. A missing file publishes nobody and is no error. It
// follows approval.json's rules: a regular file with mode 0600, read whole.
func loadA2A(path string) ([]a2aTeammate, error) {
	if path == "" {
		return nil, nil
	}
	entry, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !entry.Mode().IsRegular() {
		return nil, errors.New("a2a.json must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("a2a.json must be a regular file with mode 0600")
	}
	raw, err := io.ReadAll(io.LimitReader(f, a2aMaxFile+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > a2aMaxFile {
		return nil, errors.New("a2a.json too large")
	}
	var c struct {
		Teammates []a2aTeammate `json:"teammates"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid a2a.json: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("a2a.json must contain one object")
	}
	seen := map[string]bool{}
	for _, m := range c.Teammates {
		if !identity.ValidName(m.Name) || seen[m.Name] {
			return nil, fmt.Errorf("a2a.json: bad or repeated name %q", m.Name)
		}
		if strings.TrimSpace(m.Description) == "" {
			return nil, fmt.Errorf("a2a.json: %s has no description", m.Name)
		}
		seen[m.Name] = true
	}
	return c.Teammates, nil
}

// published returns who a2a.json publishes now. It reads the file on each
// call, so the owner can turn A2A off without a restart. A bad file publishes
// nobody.
func published(path string) []a2aTeammate {
	list, err := loadA2A(path)
	if err != nil {
		log.Printf("gateway a2a: %v (nothing is published)", err)
		return nil
	}
	return list
}

// SetA2AFile names the a2a.json to read. Without one nothing is published.
func (g *Gateway) SetA2AFile(path string) { g.a2aFile = path }

// Unpublished returns the names a2a.json does not publish.
func (c Connector) Unpublished(names []string) []string {
	list := published(c.A2AFile)
	return slices.DeleteFunc(slices.Clone(names), func(n string) bool {
		return slices.ContainsFunc(list, func(m a2aTeammate) bool { return m.Name == n })
	})
}

// publishedAs finds name in a2a.json.
func (g *Gateway) publishedAs(name string) (a2aTeammate, bool) {
	list := published(g.a2aFile)
	i := slices.IndexFunc(list, func(m a2aTeammate) bool { return m.Name == name })
	if i < 0 {
		return a2aTeammate{}, false
	}
	return list[i], true
}

// card is an agent card serving the listed teammates. It says only what
// a2a.json says, so nothing from the roster can reach a public page, and its
// version is the protocol's, since the relay's build is nobody's business on
// a page with no login.
func (g *Gateway) card(name, description string, list []a2aTeammate) map[string]any {
	interfaces, skills := []any{}, []any{}
	for _, m := range list {
		interfaces = append(interfaces, map[string]any{"url": g.base + "/a2a/" + m.Name, "protocolBinding": "JSONRPC", "protocolVersion": a2aVersion})
		skills = append(skills, map[string]any{"id": m.Name, "name": m.Name, "description": m.Description, "tags": []string{"teammate"}})
	}
	return map[string]any{
		"name": name, "description": description, "version": a2aVersion,
		"supportedInterfaces": interfaces, "skills": skills,
		"capabilities":      map[string]any{"streaming": false, "pushNotifications": false},
		"defaultInputModes": []string{"text/plain"}, "defaultOutputModes": []string{"text/plain"},
		"securitySchemes": map[string]any{"oauth2": map[string]any{"oauth2SecurityScheme": map[string]any{
			"flows": map[string]any{"authorizationCode": map[string]any{
				"authorizationUrl": g.base + "/authorize", "tokenUrl": g.base + "/token", "refreshUrl": g.base + "/token",
				"scopes": map[string]string{}, "pkceRequired": true,
			}},
			"oauth2MetadataUrl": g.base + "/.well-known/oauth-authorization-server",
		}}},
		"securityRequirements": []any{map[string]any{"schemes": map[string]any{"oauth2": map[string]any{"list": []string{}}}}},
	}
}

func writeCard(w http.ResponseWriter, card map[string]any) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, card)
}

func (g *Gateway) rootCard(w http.ResponseWriter, r *http.Request) {
	list := published(g.a2aFile)
	if len(list) == 0 {
		http.NotFound(w, r)
		return
	}
	writeCard(w, g.card("Agent Tincan", "Teammates published by this Agent Tincan team. Each skill is one teammate, with its own card and endpoint.", list))
}

func (g *Gateway) teammateCard(w http.ResponseWriter, r *http.Request) {
	m, ok := g.publishedAs(r.PathValue("teammate"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeCard(w, g.card(m.Name, m.Description, []a2aTeammate{m}))
}

// whenPublished answers 404 for a teammate a2a.json does not publish, ahead
// of the token check.
func (g *Gateway) whenPublished(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := g.publishedAs(r.PathValue("teammate")); !ok {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// callerIsGuest asks the relay whether the token's agent is a guest. It only
// sorts callers between MCP and A2A: the relay's gate still rules on each
// call, so a tier change after this answer cannot widen what a call may do.
// status is the HTTP status to refuse with when err is set.
func callerIsGuest(ctx context.Context, rc *client.Relay) (guest bool, status int, err error) {
	me, err := rc.WhoAmI(ctx)
	if client.IsStatus(err, http.StatusForbidden) {
		return false, http.StatusForbidden, err
	}
	if err != nil {
		return false, http.StatusServiceUnavailable, errors.New("the relay did not answer")
	}
	return !me.GuestUntil.IsZero(), 0, nil
}

// refuseGuests keeps a gateway guest off MCP. It is an A2A client, and the MCP
// tools are written for teammates: their standing instructions say a request
// is the owner's, and they carry no limit of A2A's own.
func (g *Gateway) refuseGuests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, _ := r.Context().Value(agentKey{}).(string)
		guest, status, err := callerIsGuest(r.Context(), g.relayFor(agent))
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		if guest {
			http.Error(w, "a guest connects over A2A, not MCP", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    []any  `json:"data,omitempty"`
}

func rpcErr(code int, message string) *rpcError {
	e := &rpcError{Code: code, Message: message}
	if reason, ok := a2aReasons[code]; ok {
		e.Data = []any{map[string]string{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": reason, "domain": "a2a-protocol.org"}}
	}
	return e
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = result
	}
	writeJSON(w, http.StatusOK, out)
}

// a2aValidID reports whether id, already known to be JSON, is a string or a
// number.
func a2aValidID(id json.RawMessage) bool {
	return len(id) > 0 && (id[0] == '"' || id[0] == '-' || (id[0] >= '0' && id[0] <= '9'))
}

// a2aCall is one JSON-RPC call by guest to teammate.
type a2aCall struct {
	rc              *client.Relay
	guest, teammate string
}

// a2a serves POST /a2a/{teammate}. A refusal of the caller itself is an HTTP
// status; anything after that is a JSON-RPC error.
func (g *Gateway) a2a(w http.ResponseWriter, r *http.Request) {
	agent, _ := r.Context().Value(agentKey{}).(string)
	rc := g.relayFor(agent)
	guest, status, err := callerIsGuest(r.Context(), rc)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	if !guest {
		http.Error(w, "A2A is for guests; run tincan connect with --guest", http.StatusForbidden)
		return
	}
	var in struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&in); err != nil {
		if _, batch := errors.AsType[*json.UnmarshalTypeError](err); batch {
			writeRPC(w, nil, nil, rpcErr(rpcInvalidRequest, "expected one JSON-RPC request"))
			return
		}
		writeRPC(w, nil, nil, rpcErr(rpcParse, "not JSON"))
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		writeRPC(w, nil, nil, rpcErr(rpcParse, "more than one JSON value"))
		return
	}
	// A call with no id could get no answer, and a send must have one. An id
	// that is no id is not echoed, so the answer's id is null.
	if !a2aValidID(in.ID) {
		writeRPC(w, nil, nil, rpcErr(rpcInvalidRequest, "expected a JSON-RPC 2.0 request with an id"))
		return
	}
	if in.JSONRPC != "2.0" || in.Method == "" {
		writeRPC(w, in.ID, nil, rpcErr(rpcInvalidRequest, "expected a JSON-RPC 2.0 request with an id"))
		return
	}
	if v := r.Header.Get("A2A-Version"); v != a2aVersion && !strings.HasPrefix(v, a2aVersion+".") {
		writeRPC(w, in.ID, nil, rpcErr(a2aBadVersion, "this agent speaks A2A 1.0"))
		return
	}
	call := a2aCall{rc: rc, guest: agent, teammate: r.PathValue("teammate")}
	var result any
	var rerr *rpcError
	switch {
	case in.Method == "SendMessage":
		result, rerr = call.sendMessage(r.Context(), in.Params)
	case in.Method == "GetTask":
		result, rerr = call.getTask(r.Context(), in.Params)
	case in.Method == "CancelTask":
		result, rerr = call.cancelTask(r.Context(), in.Params)
	case a2aOtherMethods[in.Method]:
		rerr = rpcErr(a2aUnsupported, in.Method+" is not supported")
	default:
		rerr = rpcErr(rpcNoMethod, "no such method")
	}
	writeRPC(w, in.ID, result, rerr)
}

// a2aPart is one message part. Only text is taken; the other fields are here
// so a part that carries one can be refused.
type a2aPart struct {
	Text *string         `json:"text"`
	Raw  json.RawMessage `json:"raw"`
	URL  json.RawMessage `json:"url"`
	Data json.RawMessage `json:"data"`
}

type a2aMessage struct {
	MessageID string    `json:"messageId"`
	ContextID string    `json:"contextId"`
	TaskID    string    `json:"taskId"`
	Role      string    `json:"role"`
	Parts     []a2aPart `json:"parts"`
}

// text checks a message and returns its body.
func (m a2aMessage) text() (string, *rpcError) {
	if m.MessageID == "" || m.Role != "ROLE_USER" || len(m.Parts) == 0 {
		return "", rpcErr(rpcInvalidParams, "a message needs a messageId, the role ROLE_USER and parts")
	}
	lines := make([]string, 0, len(m.Parts))
	for _, p := range m.Parts {
		if p.Text == nil || len(p.Raw) > 0 || len(p.URL) > 0 || len(p.Data) > 0 {
			return "", rpcErr(a2aContentType, "only text parts are accepted")
		}
		lines = append(lines, *p.Text)
	}
	body := strings.Join(lines, "\n")
	if strings.TrimSpace(body) == "" {
		return "", rpcErr(rpcInvalidParams, "the message has no text")
	}
	return body, nil
}

func (c a2aCall) sendMessage(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var in struct {
		Message       *a2aMessage `json:"message"`
		Configuration struct {
			ReturnImmediately bool            `json:"returnImmediately"`
			Push              json.RawMessage `json:"taskPushNotificationConfig"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.Message == nil {
		return nil, rpcErr(rpcInvalidParams, "params need a message")
	}
	if len(in.Configuration.Push) > 0 {
		return nil, rpcErr(a2aNoPush, "push notifications are not supported")
	}
	body, rerr := in.Message.text()
	if rerr != nil {
		return nil, rerr
	}
	wait := a2aWait
	if in.Configuration.ReturnImmediately {
		wait = 0
	}
	var id string
	var sent envelope.Result
	if in.Message.TaskID != "" {
		res, rerr := c.ownTask(ctx, in.Message.TaskID)
		if rerr != nil {
			return nil, rerr
		}
		if in.Message.ContextID != "" && in.Message.ContextID != res.Request.ID {
			return nil, rpcErr(rpcInvalidParams, "contextId does not match the task")
		}
		if res.Status != envelope.StatusNeedsInput {
			return nil, rpcErr(a2aUnsupported, "this task is not waiting for input")
		}
		if err := envelope.ValidateInput(body); err != nil {
			return nil, rpcErr(rpcInvalidParams, err.Error())
		}
		req, err := c.rc.Answer(ctx, res.Request.ID, body)
		if err != nil {
			return nil, relayRPCErr(err)
		}
		id, sent = req.ID, envelope.Result{Request: req, Status: envelope.StatusQueued}
	} else {
		req, err := c.rc.Send(ctx, c.teammate, body, envelope.KindAsk, "", false)
		if err != nil {
			return nil, relayRPCErr(err)
		}
		id, sent = req.ID, envelope.Result{Request: req, Status: req.Status}
	}
	if wait > 0 {
		res, err := c.rc.Get(ctx, id, wait)
		if err == nil {
			sent = res
		} else {
			// The task exists. The client must learn its id, and can poll.
			log.Printf("gateway a2a: wait on %s: %v", id, err)
		}
	}
	return map[string]any{"task": a2aTaskOf(sent)}, nil
}

func taskID(params json.RawMessage) (string, *rpcError) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.ID == "" {
		return "", rpcErr(rpcInvalidParams, "params need an id")
	}
	return in.ID, nil
}

func (c a2aCall) getTask(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	id, rerr := taskID(params)
	if rerr != nil {
		return nil, rerr
	}
	res, rerr := c.ownTask(ctx, id)
	if rerr != nil {
		return nil, rerr
	}
	return a2aTaskOf(res), nil
}

// cancelTask withdraws a task only until a teammate has been given it. The
// mark on what a teammate sends rests on its open claim, so a client that
// could cancel delivered work could drop the mark while the teammate acts on
// what it read, right after answering a question the owner never saw. The
// relay refuses a claimed request itself, and a guest's request that was
// claimed before or answered; this also covers delivered.
func (c a2aCall) cancelTask(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	id, rerr := taskID(params)
	if rerr != nil {
		return nil, rerr
	}
	res, rerr := c.ownTask(ctx, id)
	if rerr != nil {
		return nil, rerr
	}
	if a2aState(res.Status) != a2aSubmitted {
		return nil, rpcErr(a2aNotCancelable, "this task can no longer be canceled")
	}
	if err := c.rc.Cancel(ctx, id); err != nil {
		if client.IsStatus(err, http.StatusConflict) {
			return nil, rpcErr(a2aNotCancelable, "this task can no longer be canceled")
		}
		return nil, relayRPCErr(err)
	}
	if res, rerr = c.ownTask(ctx, id); rerr != nil {
		return nil, rerr
	}
	return a2aTaskOf(res), nil
}

// ownTask reads a task of this guest's at this teammate's endpoint. Any other
// id is not found, so an id says nothing about requests that are not its own.
func (c a2aCall) ownTask(ctx context.Context, id string) (envelope.Result, *rpcError) {
	if !a2aTaskID.MatchString(id) {
		return envelope.Result{}, rpcErr(a2aTaskNotFound, "task not found")
	}
	res, err := c.rc.Get(ctx, id, 0)
	if client.IsStatus(err, http.StatusNotFound) {
		return envelope.Result{}, rpcErr(a2aTaskNotFound, "task not found")
	}
	if err != nil {
		return envelope.Result{}, relayRPCErr(err)
	}
	if res.Request.From != c.guest || res.Request.To != c.teammate {
		return envelope.Result{}, rpcErr(a2aTaskNotFound, "task not found")
	}
	return res, nil
}

// relayRPCErr turns a relay refusal into a JSON-RPC error. A refusal's words
// are the ones a guest would read from the relay. Anything else stays here.
func relayRPCErr(err error) *rpcError {
	api, ok := errors.AsType[*client.APIError](err)
	if !ok {
		log.Printf("gateway a2a: %v", err)
		return rpcErr(rpcInternal, "the relay did not answer")
	}
	switch api.Code {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return rpcErr(rpcInvalidParams, api.Message)
	case http.StatusForbidden, http.StatusConflict:
		return rpcErr(a2aUnsupported, api.Message)
	case http.StatusTooManyRequests:
		return rpcErr(rpcInternal, api.Message)
	}
	log.Printf("gateway a2a: %v", err)
	return rpcErr(rpcInternal, "the relay could not do that")
}

// Task states.
const (
	a2aSubmitted = "TASK_STATE_SUBMITTED"
	a2aWorking   = "TASK_STATE_WORKING"
	a2aInput     = "TASK_STATE_INPUT_REQUIRED"
	a2aCompleted = "TASK_STATE_COMPLETED"
	a2aFailed    = "TASK_STATE_FAILED"
	a2aRejected  = "TASK_STATE_REJECTED"
	a2aCanceled  = "TASK_STATE_CANCELED"
)

// a2aState maps a request's status. A send's own response has no status
// unless the request was held, and that is a queued request.
func a2aState(s envelope.Status) string {
	switch s {
	case envelope.StatusDelivered, envelope.StatusClaimed:
		return a2aWorking
	case envelope.StatusNeedsInput:
		return a2aInput
	case envelope.StatusAnswered:
		return a2aCompleted
	case envelope.StatusFailed, envelope.StatusExpired:
		return a2aFailed
	case envelope.StatusDeclined:
		return a2aRejected
	case envelope.StatusCancelled:
		return a2aCanceled
	}
	return a2aSubmitted
}

// a2aTaskOf builds the task for a request. The task id is the request id, and
// the context is the same id, since a guest's asks share no conversation. Only
// the reply's text goes out: an answer as an artifact, as the spec has task
// output, and anything else as the status message. Progress notes and the
// relay's own notes do not.
func a2aTaskOf(res envelope.Result) map[string]any {
	id, state := res.Request.ID, a2aState(res.Status)
	status := map[string]any{"state": state}
	task := map[string]any{"id": id, "contextId": id, "status": status}
	if res.Reply == nil || res.Reply.Body == "" {
		return task
	}
	parts := []any{map[string]any{"text": res.Reply.Body}}
	switch state {
	case a2aCompleted:
		task["artifacts"] = []any{map[string]any{"artifactId": "reply", "parts": parts}}
	case a2aFailed, a2aRejected, a2aInput:
		status["message"] = map[string]any{"messageId": id + "-" + res.Reply.CreatedAt.UTC().Format("20060102T150405.000"), "role": "ROLE_AGENT", "taskId": id, "contextId": id, "parts": parts}
	}
	return task
}
