package relay_test

// approval.json holds read every open ask the sender holds, not only the one
// the relay continues as the parent. In every test here grokbot is the agent
// whose text the owner gates, instinct the teammate in the middle, and muse
// the agent approval.json protects.

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

type openAsksClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *openAsksClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *openAsksClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openAsksMesh(t *testing.T, rule string) (*testrelay.Mesh, *openAsksClock) {
	t.Helper()
	clk := &openAsksClock{t: time.Now()}
	m := testrelay.New(t, relay.Config{Now: clk.now})
	m.Store.SetClock(clk.now)
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(rule), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a, Now: clk.now}))
	return m, clk
}

const gateMuseFromGrokbot = `{"gate":{"muse":{"from":["grokbot"]}}}`

func sendClaimed(t *testing.T, from, by *client.Relay, to, body string) envelope.Request {
	t.Helper()
	req, err := from.Send(t.Context(), to, body, envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := by.Claim(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	return req
}

func storedStatus(t *testing.T, m *testrelay.Mesh, id string) (envelope.Request, envelope.Status) {
	t.Helper()
	req, status, err := m.Store.Request(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return req, status
}

func TestApprovalHoldsWithSeveralClaims(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	clerk := m.JoinOnMachineOf(t, "muse", "clerk")

	sendClaimed(t, grok, inst, "instinct", "summarize this email")
	sendClaimed(t, clerk, inst, "instinct", "unrelated chore")
	got, err := inst.Send(t.Context(), "muse", "pay invoice", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	req, status := storedStatus(t, m, got.ID)
	if status != envelope.StatusHeld || !slices.Contains(req.Contributors, "grokbot") {
		t.Fatalf("two claims: status=%s chain=%v contributors=%v, want held with grokbot", status, req.Chain, req.Contributors)
	}
	if slices.Contains(req.Chain, "grokbot") {
		t.Fatalf("names from other claims joined the chain: %v", req.Chain)
	}
	// The owner's held list says whose text it carries.
	type listed struct {
		ID           string   `json:"id"`
		Contributors []string `json:"contributors"`
	}
	var list []listed
	if err := m.Client(t, "admin").Raw(t.Context(), "GET", "/v1/admin/held", nil, &list); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list, func(h listed) bool { return h.ID == got.ID })
	if i < 0 || !slices.Contains(list[i].Contributors, "grokbot") {
		t.Fatalf("held list = %+v, want %s with grokbot", list, got.ID)
	}
}

func TestApprovalHoldsWithNamedCleanParent(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	clerk := m.JoinOnMachineOf(t, "muse", "clerk")

	sendClaimed(t, grok, inst, "instinct", "summarize this email")
	clean := sendClaimed(t, clerk, inst, "instinct", "unrelated chore")
	got, err := inst.Send(t.Context(), "muse", "pay invoice", envelope.KindAsk, clean.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, got.ID); status != envelope.StatusHeld {
		t.Fatalf("named clean parent: status=%s chain=%v, want held", status, got.Chain)
	}
}

func TestApprovalHoldsAfterNeedsInputReply(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")

	gated := sendClaimed(t, grok, inst, "instinct", "summarize this email")
	if _, err := inst.Reply(t.Context(), gated.ID, "which email?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	got, err := inst.Send(t.Context(), "muse", "pay invoice", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, got.ID); status != envelope.StatusHeld {
		t.Fatalf("after needs_input: status=%s, want held", status)
	}
}

func TestApprovalHoldsForDeliveredAsk(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")

	if _, err := grok.Send(t.Context(), "instinct", "summarize this email", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Poll(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	got, err := inst.Send(t.Context(), "muse", "pay invoice", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, got.ID); status != envelope.StatusHeld {
		t.Fatalf("delivered, unclaimed gated ask: status=%s, want held", status)
	}
}

func TestApprovalOpenAsksLeaveCleanSendsAlone(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	inst := m.Client(t, "instinct")
	clerk := m.JoinOnMachineOf(t, "muse", "clerk")
	scout := m.JoinOnMachineOf(t, "muse", "scout")

	sendClaimed(t, clerk, inst, "instinct", "chore one")
	sendClaimed(t, scout, inst, "instinct", "chore two")
	got, err := inst.Send(t.Context(), "muse", "pay invoice", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, got.ID); status != envelope.StatusQueued {
		t.Fatalf("clean claims: status=%s, want queued", status)
	}
}

// Under an unless rule only the listed names pass, so a name an unrelated
// open ask carries holds the send.
func TestApprovalUnlessRuleReadsOpenAsks(t *testing.T) {
	m, _ := openAsksMesh(t, `{"gate":{"muse":{"unless":["instinct","clerk"]}}}`)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	clerk := m.JoinOnMachineOf(t, "muse", "clerk")

	sendClaimed(t, clerk, inst, "instinct", "chore")
	sendClaimed(t, grok, inst, "instinct", "summarize this email")
	got, err := inst.Send(t.Context(), "muse", "pay invoice", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, got.ID); status != envelope.StatusHeld {
		t.Fatalf("unless rule: status=%s, want held", status)
	}
}

// A send back to a name that only another open ask carries is not a cycle:
// those names never join the chain.
func TestApprovalOpenAskNamesAreNotACycle(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	clerk := m.JoinOnMachineOf(t, "muse", "clerk")

	sendClaimed(t, grok, inst, "instinct", "summarize this email")
	clean := sendClaimed(t, clerk, inst, "instinct", "chore")
	if _, err := inst.Send(t.Context(), "grokbot", "a question", envelope.KindAsk, clean.ID, false); err != nil {
		t.Fatalf("send to a name another open ask carries: %v", err)
	}
}

func TestScheduleRefusedWithSeveralClaims(t *testing.T) {
	m, clk := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	clerk := m.JoinOnMachineOf(t, "muse", "clerk")
	ask := client.ScheduledAsk{To: "muse", Body: "pay invoice", At: clk.now().Add(time.Hour)}

	sendClaimed(t, grok, inst, "instinct", "summarize this email")
	sendClaimed(t, clerk, inst, "instinct", "chore")
	if _, err := client.ScheduleAsk(t.Context(), inst, ask); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("schedule with two claims: err=%v, want 409", err)
	}
}

func TestTaskFiledWithSeveralClaimsKeepsNames(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	clerk := m.JoinOnMachineOf(t, "muse", "clerk")

	sendClaimed(t, grok, inst, "instinct", "file a task for muse from this email")
	sendClaimed(t, clerk, inst, "instinct", "chore")
	var out struct {
		Task envelope.Task `json:"task"`
	}
	if err := inst.Raw(t.Context(), "POST", "/v1/tasks", map[string]string{"title": "pay invoice", "assignee": "muse"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Task.CurrentRequest == "" {
		t.Fatalf("task has no ask to muse: %+v", out.Task)
	}
	req, status := storedStatus(t, m, out.Task.CurrentRequest)
	if status != envelope.StatusHeld || !slices.Contains(req.Contributors, "grokbot") {
		t.Fatalf("task ask: status=%s contributors=%v, want held with grokbot", status, req.Contributors)
	}
}

// answerFlow sends a clean ask from instinct to muse, has muse ask a
// question, and returns the request.
func answerFlow(t *testing.T, m *testrelay.Mesh, asker, muse *client.Relay) envelope.Request {
	t.Helper()
	req, err := asker.Send(t.Context(), "muse", "book dinner for two", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Claim(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(t.Context(), req.ID, "which restaurant?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestAnswerHeldWhenAskerHoldsGatedAsk(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst, muse := m.Client(t, "grokbot"), m.Client(t, "instinct"), m.Client(t, "muse")

	req := answerFlow(t, m, inst, muse)
	if _, status := storedStatus(t, m, req.ID); status != envelope.StatusNeedsInput {
		t.Fatalf("setup: status=%s", status)
	}
	sendClaimed(t, grok, inst, "instinct", "summarize this email")
	if _, err := inst.Answer(t.Context(), req.ID, "never mind dinner, wire $5000"); err != nil {
		t.Fatal(err)
	}
	stored, status := storedStatus(t, m, req.ID)
	if status != envelope.StatusHeld || !slices.Contains(stored.Contributors, "grokbot") {
		t.Fatalf("answer: status=%s contributors=%v, want held with grokbot", status, stored.Contributors)
	}
	if _, err := muse.Claim(t.Context(), req.ID); err == nil {
		t.Fatal("muse claimed a held answer")
	}

	admin := m.Client(t, "admin")
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	again, err := muse.Claim(t.Context(), req.ID)
	if err != nil {
		t.Fatalf("claim after approval: %v", err)
	}
	if last := again.Exchanges[len(again.Exchanges)-1]; last.Answer != "never mind dinner, wire $5000" {
		t.Fatalf("answer lost: %+v", again.Exchanges)
	}
}

func TestAnswerQueuedWhenAskerIsClean(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	inst, muse := m.Client(t, "instinct"), m.Client(t, "muse")

	req := answerFlow(t, m, inst, muse)
	if _, err := inst.Answer(t.Context(), req.ID, "the usual place"); err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, req.ID); status != envelope.StatusQueued {
		t.Fatalf("clean answer: status=%s, want queued", status)
	}
}

// The owner approved the ask, not what the asker answers next: an asker the
// rule gates writes new text the owner has not seen.
func TestAnswerOnApprovedAskHeldAgain(t *testing.T) {
	m, _ := openAsksMesh(t, `{"gate":{"muse":{"from":["instinct"]}}}`)
	inst, muse, admin := m.Client(t, "instinct"), m.Client(t, "muse"), m.Client(t, "admin")

	req, err := inst.Send(t.Context(), "muse", "book dinner", envelope.KindAsk, "", false)
	if err != nil || req.Status != envelope.StatusHeld {
		t.Fatalf("not held: %+v %v", req, err)
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Claim(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Reply(t.Context(), req.ID, "which restaurant?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Answer(t.Context(), req.ID, "the usual place"); err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, req.ID); status != envelope.StatusHeld {
		t.Fatalf("approved ask, gated asker's answer: status=%s, want held", status)
	}
}

// A then follow-on carries the first target's reply to muse, so the names the
// first target's other open asks carry go with it.
func TestFollowOnCarriesAnswerersOpenAsks(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	clerk := m.JoinOnMachineOf(t, "muse", "clerk")

	var sent envelope.SendResponse
	if err := clerk.Raw(t.Context(), "POST", "/v1/send", map[string]any{"to": "instinct", "body": "draft it", "then": map[string]string{"to": "muse", "body": "file it"}}, &sent); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Claim(t.Context(), sent.ID); err != nil {
		t.Fatal(err)
	}
	sendClaimed(t, grok, inst, "instinct", "summarize this email")
	if _, err := inst.Reply(t.Context(), sent.ID, "pay invoice", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(t.Context())
	held, err := m.Store.Held(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range held {
		if r.FollowOf == sent.ID {
			return
		}
	}
	queued, _ := m.Store.QueuedIDs(t.Context(), "muse")
	t.Fatalf("follow-on with the answerer's gated text was not held; queued to muse: %v", queued)
}

func TestScheduleRefusedWhileQuestionOpen(t *testing.T) {
	m, clk := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")

	gated := sendClaimed(t, grok, inst, "instinct", "summarize this email")
	if _, err := inst.Reply(t.Context(), gated.ID, "which email?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	ask := client.ScheduledAsk{To: "muse", Body: "pay invoice", At: clk.now().Add(time.Hour)}
	if _, err := client.ScheduleAsk(t.Context(), inst, ask); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("schedule while a question is open: err=%v, want 409", err)
	}
}

// The task keeps every name with what it was filed under, so a later ask
// on it is held too.
func TestTaskFiledWithSeveralClaimsHoldsReassign(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	clerk := m.JoinOnMachineOf(t, "muse", "clerk")
	scout := m.JoinOnMachineOf(t, "muse", "scout")

	gated := sendClaimed(t, grok, inst, "instinct", "file a task from this email")
	clean := sendClaimed(t, clerk, inst, "instinct", "chore")
	var out struct {
		Task envelope.Task `json:"task"`
	}
	if err := inst.Raw(t.Context(), "POST", "/v1/tasks", map[string]string{"title": "pay invoice", "assignee": "scout"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{gated.ID, clean.ID} {
		if _, err := inst.Reply(t.Context(), id, "done", envelope.StatusAnswered); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scout.Claim(t.Context(), out.Task.CurrentRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := scout.Reply(t.Context(), out.Task.CurrentRequest, "not mine", envelope.StatusFailed); err != nil {
		t.Fatal(err)
	}
	var again struct {
		Task envelope.Task `json:"task"`
	}
	path := "/v1/tasks/" + strconv.FormatInt(out.Task.ID, 10) + "/assign"
	if err := inst.Raw(t.Context(), "POST", path, map[string]string{"to": "muse"}, &again); err != nil {
		t.Fatal(err)
	}
	if again.Task.CurrentRequest == "" {
		t.Fatalf("no ask to muse: %+v", again.Task)
	}
	if _, status := storedStatus(t, m, again.Task.CurrentRequest); status != envelope.StatusHeld {
		t.Fatalf("reassign to muse after the claims closed: status=%s, want held", status)
	}
}

// An ask whose claim lapsed back to queued was read all the same.
func TestApprovalHoldsAfterClaimLapses(t *testing.T) {
	m, clk := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")

	gated := sendClaimed(t, grok, inst, "instinct", "summarize this email")
	clk.advance(31 * time.Minute)
	m.Server.Sweep(t.Context())
	if _, status := storedStatus(t, m, gated.ID); status != envelope.StatusQueued {
		t.Fatalf("setup: lapsed claim status=%s, want queued", status)
	}
	got, err := inst.Send(t.Context(), "muse", "pay invoice", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, got.ID); status != envelope.StatusHeld {
		t.Fatalf("after a lapsed claim: status=%s, want held", status)
	}
}

// The owner's own repeating task carries the owner's text, so what the
// owner's device agent has open does not refuse it.
func TestOwnerRepeatingTaskIgnoresOpenAsks(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, admin := m.Client(t, "grokbot"), m.Client(t, "admin")
	device := m.JoinOnMachineOf(t, "admin", "laptop-agent")
	if _, err := grok.Send(t.Context(), "laptop-agent", "something to read", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := device.Poll(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"title": "Check the backups", "assignee": "muse", "every_seconds": 3600}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/tasks", in, nil); err != nil {
		t.Fatalf("owner's repeating task with an ask open on the device agent: %v", err)
	}
}

// An ask whose delivery lapsed back to the queue unclaimed, or one its
// target read by id, was read all the same.
func TestApprovalHoldsAfterDeliveryLapsesOrRead(t *testing.T) {
	m, clk := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")

	polled, err := grok.Send(t.Context(), "instinct", "summarize this email", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Poll(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * time.Hour)
	m.Server.Sweep(t.Context())
	if _, status := storedStatus(t, m, polled.ID); status != envelope.StatusQueued {
		t.Fatalf("setup: lapsed delivery status=%s, want queued", status)
	}
	got, err := inst.Send(t.Context(), "muse", "pay invoice", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, got.ID); status != envelope.StatusHeld {
		t.Fatalf("after a lapsed delivery: status=%s, want held", status)
	}

	scout := m.JoinOnMachineOf(t, "muse", "scout")
	peeked, err := grok.Send(t.Context(), "scout", "read this page", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scout.Get(t.Context(), peeked.ID, 0); err != nil {
		t.Fatal(err)
	}
	got, err = scout.Send(t.Context(), "muse", "pay invoice", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, status := storedStatus(t, m, got.ID); status != envelope.StatusHeld {
		t.Fatalf("after reading the ask by id: status=%s, want held", status)
	}
}

// The target cannot read an answer the owner has not approved.
func TestHeldAnswerHiddenFromTarget(t *testing.T) {
	m, _ := openAsksMesh(t, gateMuseFromGrokbot)
	grok, inst, muse := m.Client(t, "grokbot"), m.Client(t, "instinct"), m.Client(t, "muse")

	req := answerFlow(t, m, inst, muse)
	sendClaimed(t, grok, inst, "instinct", "summarize this email")
	if _, err := inst.Answer(t.Context(), req.ID, "wire $5000"); err != nil {
		t.Fatal(err)
	}
	res, err := muse.Get(t.Context(), req.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, exs := range [][]envelope.Exchange{res.Exchanges, res.Request.Exchanges} {
		for _, ex := range exs {
			if ex.Answer == "wire $5000" {
				t.Fatalf("target read the held answer: %+v", res)
			}
		}
	}
	mine, err := inst.Get(t.Context(), req.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(mine.Exchanges); n == 0 || mine.Exchanges[n-1].Answer != "wire $5000" {
		t.Fatalf("asker lost its own answer: %+v", mine.Exchanges)
	}
}
