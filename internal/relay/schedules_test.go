package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// scheduleClock is the injected clock the relay, its store and its policy
// share, so a test moves all three together.
type scheduleClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *scheduleClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *scheduleClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// scheduleMesh is a mesh on an injected clock, with the real policy.
type scheduleMesh struct {
	*testrelay.Mesh
	clk   *scheduleClock
	grok  *client.Relay
	muse  *client.Relay
	admin *client.Relay
}

func newScheduleMesh(t *testing.T) *scheduleMesh {
	t.Helper()
	clk := &scheduleClock{t: time.Now()}
	m := testrelay.New(t, relay.Config{Now: clk.now})
	m.Store.SetClock(clk.now)
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Now: clk.now}))
	return &scheduleMesh{Mesh: m, clk: clk, grok: m.Client(t, "grokbot"), muse: m.Client(t, "muse"), admin: m.Client(t, "admin")}
}

// create schedules a dummy ask from c to muse and fails the test if the
// relay refuses it.
func (m *scheduleMesh) create(t *testing.T, c *client.Relay, at time.Time, every time.Duration) envelope.Schedule {
	t.Helper()
	sch, err := client.ScheduleAsk(t.Context(), c, client.ScheduledAsk{To: "muse", Body: "weekly report", At: at, Every: every})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	return sch
}

// sentTo lists the requests from grokbot to to, oldest first.
func (m *scheduleMesh) sentTo(t *testing.T, to string) []envelope.Request {
	t.Helper()
	ids, err := m.Store.QueuedIDs(t.Context(), to)
	if err != nil {
		t.Fatal(err)
	}
	var out []envelope.Request
	for _, id := range ids {
		req, _, err := m.Store.Request(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if req.From != client.RelaySender {
			out = append(out, req)
		}
	}
	return out
}

// answer has muse take its one waiting request and answer it.
func (m *scheduleMesh) answer(t *testing.T) {
	t.Helper()
	sent := m.sentTo(t, "muse")
	if len(sent) != 1 {
		t.Fatalf("muse has %d requests waiting: %+v", len(sent), sent)
	}
	if _, err := m.muse.Claim(t.Context(), sent[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.muse.Reply(t.Context(), sent[0].ID, "done", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
}

// audited returns the audit rows of event.
func (m *scheduleMesh) audited(t *testing.T, event string) []string {
	t.Helper()
	events, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Event == event {
			out = append(out, e.Detail)
		}
	}
	return out
}

func (m *scheduleMesh) list(t *testing.T, c *client.Relay) []envelope.Schedule {
	t.Helper()
	got, err := client.Schedules(t.Context(), c, false)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// relayNotices are the relay's notices queued for agent, oldest first.
func (m *scheduleMesh) relayNotices(t *testing.T, agent string) []envelope.Request {
	t.Helper()
	ids, err := m.Store.QueuedIDs(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var out []envelope.Request
	for _, id := range ids {
		req, _, err := m.Store.Request(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if req.From == client.RelaySender {
			out = append(out, req)
		}
	}
	return out
}

// A one-shot fires once at its time, then is gone. The ask's expiry runs
// from the fire, not from when it was scheduled, so one scheduled further
// ahead than the request TTL is alive when it arrives. Audit rows carry no
// ask text.
func TestScheduleOneShotFiresOnceThenGone(t *testing.T) {
	m := newScheduleMesh(t)
	at := m.clk.now().Add(72 * time.Hour)
	sch := m.create(t, m.grok, at, 0)
	if sch.ID == 0 || sch.From != "grokbot" || sch.To != "muse" || !sch.NextAt.Equal(at.Truncate(time.Millisecond)) {
		t.Fatalf("schedule = %+v", sch)
	}
	m.Server.Sweep(t.Context())
	if got := m.sentTo(t, "muse"); len(got) != 0 {
		t.Fatalf("fired early: %+v", got)
	}
	m.clk.advance(72 * time.Hour)
	m.Server.Sweep(t.Context())
	m.Server.Sweep(t.Context())
	got := m.sentTo(t, "muse")
	if len(got) != 1 || got[0].From != "grokbot" || got[0].Body != "weekly report" || got[0].Kind != envelope.KindAsk {
		t.Fatalf("sent = %+v", got)
	}
	if left := m.list(t, m.grok); len(left) != 0 {
		t.Fatalf("still listed: %+v", left)
	}
	if n := len(m.audited(t, "schedule_created")); n != 1 {
		t.Fatalf("created rows = %d", n)
	}
	fired := m.audited(t, "schedule_fired")
	if len(fired) != 1 || strings.Contains(fired[0], "weekly report") {
		t.Fatalf("fired rows = %q", fired)
	}
}

// --every 1h fires three times across three clock advances.
func TestScheduleRecursAcrossAdvances(t *testing.T) {
	m := newScheduleMesh(t)
	m.create(t, m.grok, time.Time{}, time.Hour)
	for range 3 {
		m.clk.advance(time.Hour)
		m.Server.Sweep(t.Context())
		m.answer(t)
	}
	if n := len(m.audited(t, "schedule_fired")); n != 3 {
		t.Fatalf("fired %d times", n)
	}
	if left := m.list(t, m.grok); len(left) != 1 || left[0].LastRequest == "" {
		t.Fatalf("schedule = %+v", left)
	}
}

// A relay down across three occurrences fires one, and the next time lands
// in the future on the schedule's own cadence, with no catch-up burst.
func TestScheduleMissedOccurrencesFireOnce(t *testing.T) {
	m := newScheduleMesh(t)
	first := m.create(t, m.grok, time.Time{}, time.Hour)
	m.clk.advance(3*time.Hour + 20*time.Minute)
	m.Server.Sweep(t.Context())
	m.Server.Sweep(t.Context())
	if got := m.sentTo(t, "muse"); len(got) != 1 {
		t.Fatalf("sent = %+v", got)
	}
	left := m.list(t, m.grok)
	if len(left) != 1 || !left[0].NextAt.After(m.clk.now()) || !left[0].NextAt.Equal(first.NextAt.Add(3*time.Hour)) {
		t.Fatalf("next = %+v, now %v", left, m.clk.now())
	}
}

// An owner's hold applies at fire time: the ask is held, not delivered,
// and counts as open for the next occurrence.
func TestScheduleHeldAtFireTime(t *testing.T) {
	m := newScheduleMesh(t)
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}},"hold_ttl":"1h"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a, Now: m.clk.now}))
	m.create(t, m.grok, m.clk.now().Add(time.Hour), 0)
	m.clk.advance(time.Hour)
	m.Server.Sweep(t.Context())
	if got := m.sentTo(t, "muse"); len(got) != 0 {
		t.Fatalf("delivered past the hold: %+v", got)
	}
	held, err := m.Store.Held(t.Context())
	if err != nil || len(held) != 1 || held[0].Body != "weekly report" {
		t.Fatalf("held = %+v, %v", held, err)
	}
}

// While the previous fire is still open the occurrence is skipped and
// audited, and the schedule moves on; the next one after it closes fires.
func TestScheduleSkipsWhilePreviousOpen(t *testing.T) {
	m := newScheduleMesh(t)
	m.create(t, m.grok, time.Time{}, time.Hour)
	m.clk.advance(time.Hour)
	m.Server.Sweep(t.Context())
	m.clk.advance(time.Hour)
	m.Server.Sweep(t.Context())
	if got := m.sentTo(t, "muse"); len(got) != 1 {
		t.Fatalf("sent = %+v", got)
	}
	if n := len(m.audited(t, "schedule_skipped")); n != 1 {
		t.Fatalf("skip rows = %d", n)
	}
	if left := m.list(t, m.grok); len(left) != 1 || !left[0].NextAt.After(m.clk.now()) {
		t.Fatalf("schedule = %+v", left)
	}
	m.answer(t)
	m.clk.advance(time.Hour)
	m.Server.Sweep(t.Context())
	if got := m.sentTo(t, "muse"); len(got) != 1 {
		t.Fatalf("after the answer, sent = %+v", got)
	}
}

// A one-shot the policy rejects at fire time is gone, and the asker is told
// without the ask's text.
func TestScheduleRejectedOneShotTellsAsker(t *testing.T) {
	m := newScheduleMesh(t)
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{PerMinute: 1, Now: m.clk.now}))
	m.create(t, m.grok, m.clk.now().Add(time.Hour), 0)
	m.clk.advance(time.Hour)
	if _, err := m.grok.Send(t.Context(), "instinct", "takes the one send slot", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(t.Context())
	if got := m.sentTo(t, "muse"); len(got) != 0 {
		t.Fatalf("sent = %+v", got)
	}
	if left := m.list(t, m.grok); len(left) != 0 {
		t.Fatalf("still listed: %+v", left)
	}
	n := m.relayNotices(t, "grokbot")
	if len(n) != 1 || n[0].Kind != envelope.KindNotify || !strings.Contains(n[0].Body, "muse") || strings.Contains(n[0].Body, "weekly report") {
		t.Fatalf("notices = %+v", n)
	}
	if n := len(m.audited(t, "schedule_blocked")); n != 1 {
		t.Fatalf("blocked rows = %d", n)
	}
}

// refusing refuses every send while on, in front of the real policy.
type refusing struct {
	relay.Preparer
	mu sync.Mutex
	on bool
}

func (r *refusing) set(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = on
}

func (r *refusing) Prepare(ctx context.Context, req *envelope.Request) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.on {
		return &relay.StatusError{Code: http.StatusConflict, Err: errors.New("refused for the test")}
	}
	return r.Preparer.Prepare(ctx, req)
}

// A recurring schedule rejected three times in a row sends one notice and
// three audit rows, and stays. A fire that goes through ends the run, so
// the next rejection tells the asker again.
func TestScheduleBlockedRunSendsOneNotice(t *testing.T) {
	m := newScheduleMesh(t)
	ref := &refusing{Preparer: policy.New(m.Store, policy.Config{Now: m.clk.now}), on: true}
	m.Server.SetPreparer(ref)
	m.create(t, m.grok, time.Time{}, time.Hour)
	for range 3 {
		m.clk.advance(time.Hour)
		m.Server.Sweep(t.Context())
	}
	if n := len(m.relayNotices(t, "grokbot")); n != 1 {
		t.Fatalf("notices = %d", n)
	}
	if n := len(m.audited(t, "schedule_blocked")); n != 3 {
		t.Fatalf("blocked rows = %d", n)
	}
	if left := m.list(t, m.grok); len(left) != 1 || !left[0].Blocked {
		t.Fatalf("schedule = %+v", left)
	}
	ref.set(false)
	m.clk.advance(time.Hour)
	m.Server.Sweep(t.Context())
	m.answer(t)
	if left := m.list(t, m.grok); len(left) != 1 || left[0].Blocked {
		t.Fatalf("after a fire: %+v", left)
	}
	ref.set(true)
	m.clk.advance(time.Hour)
	m.Server.Sweep(t.Context())
	if n := len(m.relayNotices(t, "grokbot")); n != 2 {
		t.Fatalf("notices after a new run = %d", n)
	}
}

// A send check that lives in the send route runs at fire time too: a target
// that became a phone is refused like a send to one.
func TestScheduleFireChecksTarget(t *testing.T) {
	m := newScheduleMesh(t)
	m.create(t, m.grok, m.clk.now().Add(time.Hour), 0)
	if _, err := m.Store.SetAgentKind(t.Context(), "muse", onboard.KindPhone); err != nil {
		t.Fatal(err)
	}
	m.clk.advance(time.Hour)
	m.Server.Sweep(t.Context())
	if got := m.sentTo(t, "muse"); len(got) != 0 {
		t.Fatalf("sent = %+v", got)
	}
	if n := m.relayNotices(t, "grokbot"); len(n) != 1 {
		t.Fatalf("notices = %+v", n)
	}
}

// A fire starts a new chain whatever the asker holds: with parent
// inference on, this one would continue instinct's claim and be refused as
// a loop back to instinct.
func TestScheduleFireWhileClaimedStartsNewChain(t *testing.T) {
	m := newScheduleMesh(t)
	sch, err := client.ScheduleAsk(t.Context(), m.grok, client.ScheduledAsk{To: "instinct", Body: "weekly report", At: m.clk.now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	m.clk.advance(time.Hour)
	asked, err := m.Client(t, "instinct").Send(t.Context(), "grokbot", "unrelated", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.grok.Claim(t.Context(), asked.ID); err != nil {
		t.Fatal(err)
	}
	m.Server.Sweep(t.Context())
	got := m.sentTo(t, "instinct")
	if len(got) != 1 {
		t.Fatalf("schedule %d: sent = %+v", sch.ID, got)
	}
	r := got[0]
	if r.ParentID != "" || r.Hop != 1 || !slices.Equal(r.Chain, []string{"grokbot"}) || r.TraceID == asked.TraceID {
		t.Fatalf("fire continued the claim: %+v", r)
	}
}

// Creating a schedule during a claim is refused, because the fire would
// shed the claim's chain. The lookup is Prepare's, so a session's own claim
// counts though the agent holds several.
func TestScheduleRefusedDuringClaim(t *testing.T) {
	m := newScheduleMesh(t)
	claim := func(c *client.Relay, from string) {
		t.Helper()
		asked, err := m.Client(t, from).Send(t.Context(), "muse", "unrelated", envelope.KindAsk, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Claim(t.Context(), asked.ID); err != nil {
			t.Fatal(err)
		}
	}
	sessionA, sessionB := m.Client(t, "muse"), m.Client(t, "muse")
	sessionA.SetSession("a")
	sessionB.SetSession("b")
	ask := client.ScheduledAsk{To: "instinct", Body: "weekly report", At: m.clk.now().Add(time.Hour)}

	claim(sessionA, "grokbot")
	if _, err := client.ScheduleAsk(t.Context(), m.muse, ask); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("during a claim: %v", err)
	}
	if _, err := client.ScheduleAsk(t.Context(), sessionB, ask); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("another session of the claiming agent: %v", err)
	}

	claim(sessionB, "instinct")
	if _, err := client.ScheduleAsk(t.Context(), sessionB, ask); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("session with its own claim: %v", err)
	}
}

// Two concurrent sweeps give one request.
func TestScheduleConcurrentSweepsFireOnce(t *testing.T) {
	m := newScheduleMesh(t)
	m.create(t, m.grok, m.clk.now().Add(time.Hour), 0)
	m.create(t, m.grok, time.Time{}, time.Hour)
	m.clk.advance(time.Hour)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { m.Server.Sweep(context.Background()) })
	}
	wg.Wait()
	if got := m.sentTo(t, "muse"); len(got) != 2 {
		t.Fatalf("sent = %+v", got)
	}
}

// cancelling cancels the sweep's context as the fire reaches Prepare, as a
// client hanging up on a held list would.
type cancelling struct {
	relay.Preparer
	cancel context.CancelFunc
}

func (c cancelling) Prepare(ctx context.Context, req *envelope.Request) error {
	c.cancel()
	return c.Preparer.Prepare(ctx, req)
}

// A sweep run from a request can lose its context mid-fire. The schedule
// has already moved on by then, so the fire must still go through.
func TestScheduleFireSurvivesCancelledRequest(t *testing.T) {
	m := newScheduleMesh(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.Server.SetPreparer(cancelling{Preparer: policy.New(m.Store, policy.Config{Now: m.clk.now}), cancel: cancel})
	m.create(t, m.grok, m.clk.now().Add(time.Hour), 0)
	m.clk.advance(time.Hour)
	m.Server.Sweep(ctx)
	if got := m.sentTo(t, "muse"); len(got) != 1 {
		t.Fatalf("sent = %+v", got)
	}
}

// The admin routes that sweep before they answer fire what is due.
func TestScheduleFiresFromHeldSweep(t *testing.T) {
	m := newScheduleMesh(t)
	m.create(t, m.grok, m.clk.now().Add(time.Hour), 0)
	m.clk.advance(time.Hour)
	if err := m.admin.Raw(t.Context(), "GET", "/v1/admin/held", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := m.sentTo(t, "muse"); len(got) != 1 {
		t.Fatalf("sent = %+v", got)
	}
}

func TestScheduleCreateRefusals(t *testing.T) {
	m := newScheduleMesh(t)
	soon := m.clk.now().Add(time.Hour)
	for name, tc := range map[string]struct {
		body map[string]any
		want int
	}{
		"no time":        {map[string]any{"to": "muse", "body": "x"}, http.StatusBadRequest},
		"short interval": {map[string]any{"to": "muse", "body": "x", "every_seconds": 300}, http.StatusBadRequest},
		"long interval":  {map[string]any{"to": "muse", "body": "x", "every_seconds": int64(1) << 40}, http.StatusBadRequest},
		"past at":        {map[string]any{"to": "muse", "body": "x", "at": m.clk.now().Add(-time.Minute)}, http.StatusBadRequest},
		"parent":         {map[string]any{"to": "muse", "body": "x", "at": soon, "parent_id": "abc"}, http.StatusBadRequest},
		"group":          {map[string]any{"to": "muse", "body": "x", "at": soon, "group": "g1"}, http.StatusBadRequest},
		"also":           {map[string]any{"to": "muse", "body": "x", "at": soon, "also": []string{"instinct"}}, http.StatusBadRequest},
		"attachment":     {map[string]any{"to": "muse", "body": "x", "at": soon, "attachments": []map[string]string{{"id": "a1"}}}, http.StatusBadRequest},
		"notify":         {map[string]any{"to": "muse", "body": "x", "at": soon, "kind": "notify"}, http.StatusBadRequest},
		"project":        {map[string]any{"to": "muse", "body": "x", "at": soon, "project": "tincan"}, http.StatusBadRequest},
		"project, every": {map[string]any{"to": "muse", "body": "x", "every_seconds": 3600, "project": "tincan"}, http.StatusBadRequest},
		"then":           {map[string]any{"to": "muse", "body": "x", "at": soon, "then": map[string]string{"to": "instinct", "body": "y"}}, http.StatusBadRequest},
		"then, every":    {map[string]any{"to": "muse", "body": "x", "every_seconds": 3600, "then": map[string]string{"to": "instinct", "body": "y"}}, http.StatusBadRequest},
		"self":           {map[string]any{"to": "grokbot", "body": "x", "at": soon}, http.StatusBadRequest},
		"unknown target": {map[string]any{"to": "nobody", "body": "x", "at": soon}, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			if err := m.grok.Raw(t.Context(), "POST", "/v1/schedules", tc.body, nil); !client.IsStatus(err, tc.want) {
				t.Fatalf("got %v, want %d", err, tc.want)
			}
		})
	}
	if _, err := m.Store.SetAgentKind(t.Context(), "instinct", onboard.KindPhone); err != nil {
		t.Fatal(err)
	}
	if err := m.grok.Raw(t.Context(), "POST", "/v1/schedules", map[string]any{"to": "instinct", "body": "x", "at": soon}, nil); !client.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("phone target: %v", err)
	}
	if left := m.list(t, m.grok); len(left) != 0 {
		t.Fatalf("created despite refusal: %+v", left)
	}
}

// A listing carries previews, so an agent's full 32 schedules at the longest
// body stay inside what a client reads of a response, for the owner and for
// an admin device. What fires is the whole body.
func TestScheduleListFitsClientLimit(t *testing.T) {
	m := newScheduleMesh(t)
	body := strings.Repeat("x", envelope.DefaultMaxBody)
	var last envelope.Schedule
	for range store.MaxSchedules {
		var err error
		last, err = client.ScheduleAsk(t.Context(), m.grok, client.ScheduledAsk{To: "muse", Body: body, Every: time.Hour})
		if err != nil {
			t.Fatalf("schedule: %v", err)
		}
	}
	for name, list := range map[string]func() ([]envelope.Schedule, error){
		"owner": func() ([]envelope.Schedule, error) { return client.Schedules(t.Context(), m.grok, false) },
		"admin": func() ([]envelope.Schedule, error) { return client.Schedules(t.Context(), m.admin, true) },
	} {
		got, err := list()
		if err != nil {
			t.Fatalf("%s list: %v", name, err)
		}
		if len(got) != store.MaxSchedules {
			t.Fatalf("%s list has %d schedules", name, len(got))
		}
		if b := got[0].Body; len(b) >= len(body) || !strings.HasPrefix(body, b) {
			t.Fatalf("%s preview is %d bytes", name, len(b))
		}
	}
	if sch, err := client.CancelSchedule(t.Context(), m.grok, last.ID); err != nil || sch.Body != body {
		t.Fatalf("cancelled body is %d bytes, %v", len(sch.Body), err)
	}
}

// An admin listing covers any number of agents, so it comes in pages a
// client can read whole.
func TestScheduleListPagesAcrossAgents(t *testing.T) {
	m := newScheduleMesh(t)
	body := strings.Repeat("<", envelope.DefaultMaxBody)
	owners := relay.MaxSchedulePage/store.MaxSchedules + 1
	want := 0
	for i := range owners {
		name := fmt.Sprintf("agent%d", i)
		if err := m.Store.PutAgent(t.Context(), identity.Agent{Name: name, Kind: "cli"}); err != nil {
			t.Fatal(err)
		}
		for range store.MaxSchedules {
			if _, err := m.Store.AddSchedule(t.Context(), envelope.Schedule{From: name, To: "muse", Body: body, EverySeconds: 3600, NextAt: m.clk.now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			want++
		}
	}
	var page struct {
		Schedules []envelope.Schedule `json:"schedules"`
		NextAfter int64               `json:"next_after"`
	}
	if err := m.admin.Raw(t.Context(), "GET", "/v1/schedules?all=1&after=0", nil, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Schedules) != relay.MaxSchedulePage || page.NextAfter != page.Schedules[len(page.Schedules)-1].ID {
		t.Fatalf("page has %d schedules, next_after %d", len(page.Schedules), page.NextAfter)
	}
	// A client from before pages reads one response, so it gets everything.
	page.Schedules, page.NextAfter = nil, 0
	if err := m.admin.Raw(t.Context(), "GET", "/v1/schedules?all=1", nil, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Schedules) != want || page.NextAfter != 0 {
		t.Fatalf("unpaged listing has %d schedules, next_after %d", len(page.Schedules), page.NextAfter)
	}
	list, err := client.Schedules(t.Context(), m.admin, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != want {
		t.Fatalf("listed %d schedules, want %d", len(list), want)
	}
	for i := 1; i < len(list); i++ {
		if list[i].ID <= list[i-1].ID {
			t.Fatalf("schedule %d repeats or goes back: %d after %d", i, list[i].ID, list[i-1].ID)
		}
	}
}

// The preview is cut after the text is cleaned, so padding cannot push an
// ask's words out of what the owner sees.
func TestSchedulePreviewSkipsPadding(t *testing.T) {
	m := newScheduleMesh(t)
	pad := strings.Repeat(" \n\x1b", 1000)
	if _, err := client.ScheduleAsk(t.Context(), m.grok, client.ScheduledAsk{To: "muse", Body: pad + "wire the money", Every: time.Hour}); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	list, err := client.Schedules(t.Context(), m.grok, false)
	if err != nil || len(list) != 1 || list[0].Body != "wire the money" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestScheduleLimitPerAgent(t *testing.T) {
	m := newScheduleMesh(t)
	soon := m.clk.now().Add(time.Hour)
	for range 32 {
		m.create(t, m.grok, soon, 0)
	}
	if _, err := client.ScheduleAsk(t.Context(), m.grok, client.ScheduledAsk{To: "muse", Body: "x", At: soon}); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("33rd: %v", err)
	}
	m.create(t, m.Client(t, "instinct"), soon, 0)
}

// Removing the owner or the target deletes the schedule, so nothing fires
// from or to a name joined again later.
func TestScheduleGoesWithRemovedAgent(t *testing.T) {
	for _, gone := range []string{"grokbot", "muse"} {
		t.Run(gone, func(t *testing.T) {
			m := newScheduleMesh(t)
			m.create(t, m.grok, m.clk.now().Add(time.Hour), 0)
			var out any
			if err := m.admin.Raw(t.Context(), "POST", "/v1/admin/remove", map[string]string{"name": gone}, &out); err != nil {
				t.Fatal(err)
			}
			all, err := client.Schedules(t.Context(), m.admin, true)
			if err != nil || len(all) != 0 {
				t.Fatalf("after removal: %+v, %v", all, err)
			}
		})
	}
}

// Only the owner lists and cancels its schedule; another agent sees none
// and gets 404. An admin device lists every schedule and cancels any.
func TestScheduleOwnershipAndAdmin(t *testing.T) {
	m := newScheduleMesh(t)
	soon := m.clk.now().Add(time.Hour)
	mine, theirs := m.create(t, m.grok, soon, 0), m.create(t, m.grok, soon, 0)
	if got := m.list(t, m.muse); len(got) != 0 {
		t.Fatalf("muse lists %+v", got)
	}
	if _, err := client.Schedules(t.Context(), m.muse, true); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("agent list --all: %v", err)
	}
	if _, err := client.CancelSchedule(t.Context(), m.muse, mine.ID); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("muse cancel: %v", err)
	}
	if _, err := client.CancelSchedule(t.Context(), m.grok, mine.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CancelSchedule(t.Context(), m.grok, mine.ID); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("second cancel: %v", err)
	}
	all, err := client.Schedules(t.Context(), m.admin, true)
	if err != nil || len(all) != 1 || all[0].ID != theirs.ID {
		t.Fatalf("admin list = %+v, %v", all, err)
	}
	if _, err := client.CancelSchedule(t.Context(), m.admin, theirs.ID); err != nil {
		t.Fatal(err)
	}
	if left := m.list(t, m.grok); len(left) != 0 {
		t.Fatalf("left = %+v", left)
	}
	if n := len(m.audited(t, "schedule_cancelled")); n != 2 {
		t.Fatalf("cancel rows = %d", n)
	}
	m.clk.advance(time.Hour)
	m.Server.Sweep(t.Context())
	if got := m.sentTo(t, "muse"); len(got) != 0 {
		t.Fatalf("a cancelled schedule fired: %+v", got)
	}
}

func TestCapabilitiesAdvertiseSchedules(t *testing.T) {
	m := newScheduleMesh(t)
	caps, err := m.grok.Capabilities(t.Context())
	if err != nil || !caps.Schedules {
		t.Fatalf("caps = %+v, %v", caps, err)
	}
}

// A schedule's wire form names the time fields clients read.
func TestScheduleWireForm(t *testing.T) {
	m := newScheduleMesh(t)
	var raw json.RawMessage
	if err := m.grok.Raw(t.Context(), "POST", "/v1/schedules", map[string]any{"to": "muse", "body": "x", "every_seconds": 3600}, &raw); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["every_seconds"] != float64(3600) || got["from"] != "grokbot" || got["next_at"] == nil {
		t.Fatalf("schedule = %s", raw)
	}
}
