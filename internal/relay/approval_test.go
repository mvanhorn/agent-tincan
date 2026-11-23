package relay_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestAdminSetNotifyDestination(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	const original = `{"gate":{"muse":{"unless":[]}},"hold_ttl":"1h","Notify":"grokbot"}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	admin := m.Client(t, "admin")
	sender := m.Client(t, "grokbot")
	ctx := t.Context()
	if err := sender.SetNotifyDestination(ctx, "instinct"); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("agent set notify: %v", err)
	}
	if err := admin.SetNotifyDestination(ctx, "missing"); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("admin set unknown notify: %v", err)
	}
	if got := a.NotifyDestination(); got != "grokbot" {
		t.Fatalf("rejected updates changed notify to %q", got)
	}

	if err := admin.SetNotifyDestination(ctx, "instinct"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("approval.json mode = %o, want 600", got)
	}
	req := envelope.Request{From: "grokbot", To: "muse"}
	if held, err := a.Held(&req); !held || err != nil || req.HoldTTL != time.Hour || req.ApprovalNotify != "instinct" {
		t.Fatalf("updated policy: held = %v, err = %v, ttl = %v, notify = %q", held, err, req.HoldTTL, req.ApprovalNotify)
	}

	socket := httptest.NewServer(m.Server.AdminHandler())
	defer socket.Close()
	local, err := client.NewRelay(socket.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SetNotifyDestination(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if held, err := a.Held(&req); !held || err != nil || req.HoldTTL != time.Hour || req.ApprovalNotify != "" {
		t.Fatalf("cleared policy: held = %v, err = %v, ttl = %v, notify = %q", held, err, req.HoldTTL, req.ApprovalNotify)
	}
	events, err := m.Store.AuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var updates []string
	for _, e := range events {
		if e.Event == "relay_notify_updated" {
			updates = append(updates, e.Actor+" "+e.Detail)
		}
	}
	if len(updates) != 2 ||
		updates[0] != m.Addr("admin")+` {"notify":"instinct"}` ||
		updates[1] != identity.LocalAdmin+` {"notify":""}` {
		t.Fatalf("notify audit = %v", updates)
	}
}

func TestAdminNotifyCreatesPolicyAndRefusesCorruptPolicy(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	admin := m.Client(t, "admin")
	if err := admin.SetNotifyDestination(t.Context(), "instinct"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != `{"gate":{},"notify":"instinct"}` {
		t.Fatalf("new approval.json = %s, err = %v", raw, err)
	}

	const malformed = `{"gate":`
	if err := os.WriteFile(path, []byte(malformed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := admin.SetNotifyDestination(t.Context(), "muse"); !client.IsStatus(err, http.StatusInternalServerError) {
		t.Fatalf("admin set notify over malformed policy: %v", err)
	}
	raw, err = os.ReadFile(path)
	if err != nil || string(raw) != malformed {
		t.Fatalf("malformed approval.json changed: %s, err = %v", raw, err)
	}
}

type approvalWakes struct {
	mu      sync.Mutex
	targets []string
}

func (w *approvalWakes) Queued(_ context.Context, r envelope.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.targets = append(w.targets, r.To)
}
func (w *approvalWakes) count(target string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, v := range w.targets {
		if v == target {
			n++
		}
	}
	return n
}

func TestOwnerApprovalLifecycle(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"},"instinct":{"from":"*"}},"notify":"instinct","hold_ttl":"1h"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	wakes := &approvalWakes{}
	m.Server.SetEvents(wakes)
	sender, target, admin := m.Client(t, "grokbot"), m.Client(t, "muse"), m.Client(t, "admin")
	send := func() envelope.Request {
		t.Helper()
		r, err := sender.Send(t.Context(), "muse", strings.Repeat("界", 210), envelope.KindAsk, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != envelope.StatusHeld {
			t.Fatalf("send status %s", r.Status)
		}
		return r
	}
	req := send()
	if wakes.count("muse") != 0 || m.Server.QueuedCount("muse") != 0 {
		t.Fatal("held request queued/woken")
	}
	inbox, err := target.Poll(t.Context(), time.Millisecond)
	if err != nil || len(inbox.Requests) != 0 {
		t.Fatalf("held delivered: %+v %v", inbox, err)
	}
	if _, err := target.Claim(t.Context(), req.ID); !client.IsStatus(err, 409) {
		t.Fatalf("held claim: %v", err)
	}
	if _, err := target.Reply(t.Context(), req.ID, "bypass", envelope.StatusAnswered); !client.IsStatus(err, 409) {
		t.Fatalf("held reply: %v", err)
	}
	if res, err := target.Get(t.Context(), req.ID, 0); err != nil || res.Request.Body != "waiting for the owner's approval" || len(res.Request.Attachments) != 0 {
		t.Fatalf("target read held: %v", err)
	}
	var trace struct {
		Steps []envelope.Result `json:"steps"`
	}
	if err := target.Raw(t.Context(), "GET", "/v1/trace/"+req.TraceID, nil, &trace); err != nil {
		t.Fatal(err)
	}
	for _, step := range trace.Steps {
		if strings.Contains(step.Request.Body, "界") {
			t.Fatal("held body leaked through trace")
		}
	}
	res, err := sender.Get(t.Context(), req.ID, time.Second)
	if err != nil || res.Status != envelope.StatusHeld || res.Done() {
		t.Fatalf("held result: %+v %v", res, err)
	}
	raw, _ := json.Marshal(res)
	var old client.Result
	if err := json.Unmarshal(raw, &old); err != nil || string(old.Status) != "held" {
		t.Fatalf("old decode: %v", err)
	}
	for _, who := range []string{"grokbot", "muse", "instinct", "stranger"} {
		for _, action := range []string{"approve", "deny"} {
			if err := m.Client(t, who).Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/"+action, map[string]string{}, nil); !client.IsStatus(err, 403) {
				t.Fatalf("%s %s: %v", who, action, err)
			}
		}
		if err := m.Client(t, who).Raw(t.Context(), "GET", "/v1/admin/held", nil, nil); !client.IsStatus(err, 403) {
			t.Fatalf("%s list: %v", who, err)
		}
	}
	var held []envelope.Request
	if err := admin.Raw(t.Context(), "GET", "/v1/admin/held", nil, &held); err != nil || len(held) != 1 || len([]rune(held[0].Body)) != 200 {
		t.Fatalf("held list: %+v %v", held, err)
	}
	notices, err := m.Client(t, "instinct").Poll(t.Context(), time.Millisecond)
	if err != nil || len(notices.Requests) != 1 {
		t.Fatalf("notice: %+v %v", notices, err)
	}
	notice := notices.Requests[0]
	// instinct is itself gated here: the notice must carry no request text.
	if notice.From != "relay" || notice.Kind != envelope.KindNotify || !strings.Contains(notice.Body, "tincan approve "+req.ID) || strings.Contains(notice.Body, "界") {
		t.Fatalf("bad notice: %+v", notice)
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	if wakes.count("muse") != 1 || m.Server.QueuedCount("muse") != 1 {
		t.Fatal("approval did not queue/wake")
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); !client.IsStatus(err, 409) {
		t.Fatalf("repeat approval: %v", err)
	}
	inbox, err = target.Poll(t.Context(), time.Millisecond)
	if err != nil || len(inbox.Requests) != 1 || !inbox.Requests[0].CreatedAt.Equal(req.CreatedAt) {
		t.Fatalf("approved delivery: %+v %v", inbox, err)
	}
	denied := send()
	// The local admin handler uses the same routes and auth boundary as the socket.
	local := httptest.NewServer(m.Server.AdminHandler())
	defer local.Close()
	socketAdmin := client.NewRelayHTTP(local.URL, http.DefaultClient)
	if err := socketAdmin.Raw(t.Context(), "POST", "/v1/admin/requests/"+denied.ID+"/deny", map[string]string{"reason": "owner said no"}, nil); err != nil {
		t.Fatal(err)
	}
	res, err = sender.Get(t.Context(), denied.ID, 0)
	if err != nil || res.Status != envelope.StatusDeclined || res.Reply == nil || res.Reply.Body != "owner said no" || res.Reply.From != "relay" {
		t.Fatalf("deny: %+v %v", res, err)
	}
	expired := send()
	now := time.Now().Add(2 * time.Hour)
	m.Store.SetClock(func() time.Time { return now })
	m.Server.Sweep(t.Context())
	res, err = sender.Get(t.Context(), expired.ID, 0)
	if err != nil || res.Status != envelope.StatusExpired {
		t.Fatalf("expiry: %+v %v", res, err)
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+expired.ID+"/approve", nil, nil); !client.IsStatus(err, 409) {
		t.Fatalf("approve expired: %v", err)
	}
	events, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"held", "approved", "denied", "hold_expired", "approval_notified"} {
		found := false
		for _, e := range events {
			if e.Event == event {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s audit", event)
		}
	}
	if _, err := m.Store.VerifyAudit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// AE5 on the relay the test rig runs (no approval.json): an agent's ask to a
// council-kind agent comes back held, wakes nobody and pushes no notice, and
// the owner's held listing names the target kind and what it carries.
func TestCouncilAskHeldByDefault(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	if ok, err := m.Store.SetAgentKind(t.Context(), "muse", "council"); !ok || err != nil {
		t.Fatalf("set kind: %v %v", ok, err)
	}
	wakes := &approvalWakes{}
	m.Server.SetEvents(wakes)
	sender, admin := m.Client(t, "grokbot"), m.Client(t, "admin")
	upload, err := sender.UploadAttachment(t.Context(), "plan.md", "text/markdown", strings.NewReader("draft!"), 6)
	if err != nil {
		t.Fatal(err)
	}
	req, err := sender.SendAttached(t.Context(), "muse", "which design?", envelope.KindAsk, "", []string{upload.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != envelope.StatusHeld {
		t.Fatalf("council ask status %q, want held", req.Status)
	}
	if len(wakes.targets) != 0 || m.Server.QueuedCount("muse") != 0 {
		t.Fatalf("held council ask queued or woke %v", wakes.targets)
	}
	for _, who := range []string{"grokbot", "instinct", "muse"} {
		inbox, err := m.Client(t, who).Poll(t.Context(), time.Millisecond)
		if err != nil || len(inbox.Requests) != 0 {
			t.Fatalf("%s got %+v %v; want no notice", who, inbox, err)
		}
	}
	var held []struct {
		envelope.Request
		ToKind string `json:"to_kind"`
	}
	if err := admin.Raw(t.Context(), "GET", "/v1/admin/held", nil, &held); err != nil || len(held) != 1 {
		t.Fatalf("held list: %+v %v", held, err)
	}
	h := held[0]
	if h.ID != req.ID || h.ToKind != "council" || len(h.Attachments) != 1 || h.Attachments[0].Name != "plan.md" || h.Attachments[0].Size != 6 {
		t.Fatalf("held entry = %+v", h)
	}
	// Old clients decode the listing as plain requests.
	var old []envelope.Request
	if err := admin.Raw(t.Context(), "GET", "/v1/admin/held", nil, &old); err != nil || len(old) != 1 || old[0].ID != req.ID {
		t.Fatalf("old decode: %+v %v", old, err)
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	if m.Server.QueuedCount("muse") != 1 {
		t.Fatal("approved council ask not queued")
	}
}

func TestApprovalUsesRecordedChainAndFailsClosed(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"gate":{"muse":{"from":["grokbot"]}}}`)
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	parent, err := m.Client(t, "grokbot").Send(t.Context(), "instinct", "handle this", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "instinct").Claim(t.Context(), parent.ID); err != nil {
		t.Fatal(err)
	}
	var req envelope.Request
	if err := m.Client(t, "instinct").Raw(t.Context(), "POST", "/v1/send", map[string]any{"to": "muse", "body": "chain request", "from": "safe", "chain": []string{"safe"}, "status": "queued"}, &req); err != nil {
		t.Fatal(err)
	}
	if req.Status != envelope.StatusHeld || req.From != "instinct" || strings.Join(req.Chain, ",") != "grokbot,instinct" {
		t.Fatalf("forged chain: %+v", req)
	}
	write(`broken`)
	// Muse's previously non-matching sender restriction must now hold too.
	if err := m.Client(t, "instinct").Raw(t.Context(), "POST", "/v1/requests/"+parent.ID+"/reply", map[string]string{"body": "done"}, nil); err != nil {
		t.Fatal(err)
	}
	req, err = m.Client(t, "instinct").Send(t.Context(), "muse", "after corruption", envelope.KindAsk, "", false)
	if err != nil || req.Status != envelope.StatusHeld {
		t.Fatalf("fail closed: %+v %v", req, err)
	}
	if err := m.Client(t, "instinct").Cancel(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	req, err = m.Client(t, "instinct").Send(t.Context(), "muse", "remove me", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Client(t, "admin").Remove(t.Context(), "muse"); err != nil {
		t.Fatal(err)
	}
	res, err := m.Client(t, "instinct").Get(t.Context(), req.ID, 0)
	if err != nil || res.Status != envelope.StatusCancelled {
		t.Fatalf("remove held: %+v %v", res, err)
	}
}

func TestHeldAttachmentAccess(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	sender, target, admin := m.Client(t, "grokbot"), m.Client(t, "muse"), m.Client(t, "admin")
	upload, err := sender.UploadAttachment(t.Context(), "private.txt", "text/plain", strings.NewReader("secret"), 6)
	if err != nil {
		t.Fatal(err)
	}
	req, err := sender.SendAttached(t.Context(), "muse", "", envelope.KindAsk, "", []string{upload.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	if _, err := target.DownloadAttachment(t.Context(), upload.ID, &body); !client.IsStatus(err, 404) {
		t.Fatalf("held attachment leaked: %v", err)
	}
	if _, err := sender.DownloadAttachment(t.Context(), upload.ID, &body); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.DownloadAttachment(t.Context(), upload.ID, &body); err != nil {
		t.Fatal(err)
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	body.Reset()
	if _, err := target.DownloadAttachment(t.Context(), upload.ID, &body); err != nil || body.String() != "secret" {
		t.Fatalf("approved attachment: %q %v", body.String(), err)
	}
}

func TestApprovalNotificationFailureKeepsHeld(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}},"notify":"missing"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	req, err := m.Client(t, "grokbot").Send(t.Context(), "muse", "hello", envelope.KindNotify, "", false)
	if err != nil || req.Status != envelope.StatusHeld {
		t.Fatalf("held notify: %+v %v", req, err)
	}
	res, err := m.Client(t, "grokbot").Get(t.Context(), req.ID, 0)
	if err != nil || res.Status != envelope.StatusHeld {
		t.Fatalf("notification failure lost hold: %+v %v", res, err)
	}
	events, err := m.Store.AuditForTrace(t.Context(), req.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Event == "approval_notify_failed" {
			return
		}
	}
	t.Fatal("missing notification failure audit")
}

func TestNeverApprovedContentRemainsPrivate(t *testing.T) {
	for _, transition := range []string{"cancelled", "denied", "expired", "approved"} {
		t.Run(transition, func(t *testing.T) {
			m := testrelay.New(t, relay.Config{})
			m.Server.SetAttachmentDir(t.TempDir())
			path := filepath.Join(t.TempDir(), "approval.json")
			if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}},"hold_ttl":"1h"}`), 0600); err != nil {
				t.Fatal(err)
			}
			gate, err := policy.LoadApproval(path)
			if err != nil {
				t.Fatal(err)
			}
			m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: gate}))
			sender, target, admin := m.Client(t, "grokbot"), m.Client(t, "muse"), m.Client(t, "admin")
			upload, err := sender.UploadAttachment(t.Context(), "secret.txt", "text/plain", strings.NewReader("secret"), 6)
			if err != nil {
				t.Fatal(err)
			}
			req, err := sender.SendAttached(t.Context(), "muse", "private body", envelope.KindAsk, "", []string{upload.ID}, false)
			if err != nil {
				t.Fatal(err)
			}
			switch transition {
			case "cancelled":
				err = sender.Cancel(t.Context(), req.ID)
			case "denied":
				err = admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/deny", nil, nil)
			case "expired":
				now := time.Now().Add(2 * time.Hour)
				m.Store.SetClock(func() time.Time { return now })
				m.Server.Sweep(t.Context())
			case "approved":
				err = admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			approved := transition == "approved"
			for _, who := range []string{"grokbot", "muse", "admin"} {
				c := m.Client(t, who)
				visible := approved || who != "muse"
				check := func(r envelope.Request) {
					t.Helper()
					if visible {
						if r.Body != "private body" || len(r.Attachments) != 1 {
							t.Fatalf("%s lost content: %+v", who, r)
						}
					} else if r.Body != "waiting for the owner's approval" || len(r.Attachments) != 0 {
						t.Fatalf("%s leaked content: %+v", who, r)
					}
				}
				if who != "admin" {
					res, err := c.Get(t.Context(), req.ID, 0)
					if err != nil {
						t.Fatal(err)
					}
					check(res.Request)
				}
				var trace struct {
					Steps []envelope.Result `json:"steps"`
				}
				if err := c.Raw(t.Context(), "GET", "/v1/trace/"+req.TraceID, nil, &trace); err != nil {
					t.Fatal(err)
				}
				if len(trace.Steps) != 1 {
					t.Fatalf("trace: %+v", trace)
				}
				check(trace.Steps[0].Request)
				var body strings.Builder
				_, err := c.DownloadAttachment(t.Context(), upload.ID, &body)
				if visible {
					if err != nil || body.String() != "secret" {
						t.Fatalf("%s download: %q %v", who, body.String(), err)
					}
				} else if !client.IsStatus(err, 404) || body.Len() != 0 {
					t.Fatalf("attachment leaked: %q %v", body.String(), err)
				}
			}
			var peek struct {
				Pending []envelope.Pending `json:"pending"`
			}
			if err := target.Raw(t.Context(), "GET", "/v1/poll?peek=1&hold=0", nil, &peek); err != nil {
				t.Fatal(err)
			}
			if approved {
				if len(peek.Pending) != 1 || peek.Pending[0].ID != req.ID {
					t.Fatalf("approved peek: %+v", peek)
				}
			} else if len(peek.Pending) != 0 {
				t.Fatalf("unapproved peek: %+v", peek)
			}
			inbox, err := target.Poll(t.Context(), time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if approved {
				if len(inbox.Requests) != 1 || inbox.Requests[0].Body != "private body" || len(inbox.Requests[0].Attachments) != 1 {
					t.Fatalf("approved poll: %+v", inbox)
				}
			} else if len(inbox.Requests) != 0 {
				t.Fatalf("unapproved poll: %+v", inbox)
			}
		})
	}
}

// A good-at line grants nothing: an ask to a gated teammate with a line is
// still held for the owner (R8).
func TestGoodAtLineDoesNotSkipApproval(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}},"hold_ttl":"1h"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	if _, err := m.Client(t, "admin").SetGoodAt(t.Context(), "muse", "phone calls; fast pickup"); err != nil {
		t.Fatal(err)
	}
	r, err := m.Client(t, "grokbot").Send(t.Context(), "muse", "call the restaurant", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != envelope.StatusHeld || m.Server.QueuedCount("muse") != 0 {
		t.Fatalf("send status %s, queued %d; want held", r.Status, m.Server.QueuedCount("muse"))
	}
}
