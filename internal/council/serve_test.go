package council

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// syncBuffer is a log writer the service and the test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// serviceRig is a council rig whose council agent has kind council and a
// Council service built over it with tiny stage limits. alpha, bravo, and
// charlie answer honestly, and alpha chairs.
type serviceRig struct {
	*councilRig
	store *Store
	svc   *Service
	log   *syncBuffer
	dir   string
}

func newServiceRig(t *testing.T, rc relay.Config, renew time.Duration) *serviceRig {
	t.Helper()
	r := &serviceRig{councilRig: newCouncilRig(t, rc), log: &syncBuffer{}, dir: t.TempDir()}
	if rc.SweepEvery > 0 {
		// Lapse leases as a running relay does.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.mesh.Server.Run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	if ok, err := r.mesh.Store.SetAgentKind(t.Context(), "council", "council"); !ok || err != nil {
		t.Fatalf("set kind: %v %v", ok, err)
	}
	st, err := OpenStore(filepath.Join(r.dir, "council.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r.store = st
	cfg := DefaultConfig()
	cfg.Members = []string{"alpha", "bravo", "charlie"}
	cfg.Chairmen = []string{"alpha"}
	cfg.AnswerLimit, cfg.ReviewLimit, cfg.ChairmanLimit = 5*time.Second, 5*time.Second, 5*time.Second
	r.svc, err = NewService(ServiceConfig{Relay: r.council, Config: cfg, Store: st, ReportDir: filepath.Join(r.dir, "reports"),
		Hold: time.Second, RenewEvery: renew, Log: r.log})
	if err != nil {
		t.Fatal(err)
	}
	r.member("alpha", chairing("alpha", func(string) string { return verdictBlock("debugging", "Use a queue.") }))
	r.member("bravo", honest("bravo", ""))
	r.member("charlie", honest("charlie", ""))
	return r
}

// start runs the service until the test ends.
func (r *serviceRig) start() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.svc.Run(ctx) }()
	r.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && ctx.Err() == nil {
			r.t.Errorf("service: %v", err)
		}
	})
}

// ungate lets asks to council through without the owner's approval.
func (r *serviceRig) ungate() { r.gate("council", "nobody") }

// send asks council body from codex without claiming it.
func (r *serviceRig) send(body string) envelope.Request {
	r.t.Helper()
	sent, err := r.mesh.Client(r.t, "codex").SendAttached(r.t.Context(), "council", body, envelope.KindAsk, "", nil, false)
	if err != nil {
		r.t.Fatal(err)
	}
	return sent
}

// await waits for id's reply as codex sees it.
func (r *serviceRig) await(id string, within time.Duration) client.Result {
	r.t.Helper()
	codex := r.mesh.Client(r.t, "codex")
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		res, err := codex.Get(r.t.Context(), id, time.Second)
		if err == nil && res.Done() {
			return res
		}
	}
	r.t.Fatalf("no reply to %s within %v; log:\n%s", id, within, r.log.String())
	return client.Result{}
}

func (r *serviceRig) status(id string) envelope.Result {
	r.t.Helper()
	res, err := r.mesh.Client(r.t, "codex").Get(r.t.Context(), id, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// F2: an agent-convened council is held for the owner, runs once approved,
// and answers the convener with its report and scorecard attached. The
// stage notes go out in order, and the scores are recorded under the
// chairman's category.
func TestServiceRunsApprovedCouncil(t *testing.T) {
	r := newServiceRig(t, relay.Config{}, time.Minute)
	r.start()
	sent := r.send("Which queue should we use?")
	if sent.Status != envelope.StatusHeld {
		t.Fatalf("council ask status %s, want held", sent.Status)
	}
	time.Sleep(1500 * time.Millisecond)
	if got := r.status(sent.ID).Status; got != envelope.StatusHeld {
		t.Fatalf("held council ran before approval: %s", got)
	}
	if err := r.mesh.Client(t, "admin").Raw(t.Context(), "POST", "/v1/admin/requests/"+url.PathEscape(sent.ID)+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	res := r.await(sent.ID, 30*time.Second)
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s:\n%s", res.Status, res.Reply.Body)
	}
	body := res.Reply.Body
	for _, want := range []string{"Recommendation: Use a queue.", "Chairman: alpha. Category: debugging.", "```council-result"} {
		if !strings.Contains(body, want) {
			t.Errorf("reply lacks %q:\n%s", want, body)
		}
	}
	var names []string
	for _, a := range res.Reply.Attachments {
		names = append(names, filepath.Ext(a.Name))
	}
	if strings.Join(names, ",") != ".html,.png" {
		t.Errorf("attachments = %q, want the report and the scorecard", names)
	}

	log := r.log.String()
	last := -1
	for _, note := range []string{"Answers: asked 3 members", "Answers: 3 of 3 in", "Review: 3 members ranking the answers blind", "Review: 3 of 3 ballots counted", "Chairman alpha writing the verdict"} {
		i := strings.Index(log, sent.ID+": "+note)
		if i <= last {
			t.Fatalf("progress note %q missing or out of order in log:\n%s", note, log)
		}
		last = i
	}

	rows, err := r.store.Leaderboard(t.Context(), "debugging")
	if err != nil || len(rows) != 3 {
		t.Fatalf("debugging leaderboard = %+v, %v", rows, err)
	}
	rec, err := r.store.Council(t.Context(), sent.ID)
	if err != nil || rec.State != CouncilCompleted || rec.Reply != body || rec.Category != "debugging" {
		t.Fatalf("stored council = %+v, %v", rec, err)
	}
}

// The service refuses to run as an agent whose relay kind is not council:
// the relay would not hold agents' councils for the owner.
func TestServiceRefusesWithoutCouncilKind(t *testing.T) {
	r := newServiceRig(t, relay.Config{}, time.Minute)
	if _, err := r.mesh.Store.SetAgentKind(t.Context(), "council", "generic"); err != nil {
		t.Fatal(err)
	}
	r.ungate()
	sent := r.send("Which queue should we use?")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := r.svc.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), `not "council"`) || !strings.Contains(err.Error(), "tincan kind council council") {
		t.Fatalf("Run = %v, want a refusal naming the kind fix", err)
	}
	if got := r.status(sent.ID).Status; got != envelope.StatusQueued {
		t.Fatalf("request status %s after refusal, want queued", got)
	}
}

// A council arriving while another runs is claimed at once, within the
// delivery lease, and told it is waiting; both then run in turn.
func TestServiceClaimsSecondCouncilWhileRunning(t *testing.T) {
	r := newServiceRig(t, relay.Config{DeliveryLease: 2 * time.Second, SweepEvery: 100 * time.Millisecond}, time.Minute)
	r.join("delta") // sits but never answers, so the first council runs out its answer limit
	r.svc.cfg.Members = append(r.svc.cfg.Members, "delta")
	r.svc.cfg.AnswerLimit = 4 * time.Second
	r.ungate()
	r.start()
	first := r.send("Which queue should we use?")
	time.Sleep(500 * time.Millisecond)
	second := r.send("Which broker should we use?")
	time.Sleep(1500 * time.Millisecond)
	st := r.status(second.ID)
	if st.Status != envelope.StatusClaimed || st.Progress == nil || st.Progress.Note != "Waiting behind another council (1 ahead)" {
		t.Fatalf("second council = %s %+v, want claimed and waiting", st.Status, st.Progress)
	}
	if r.status(first.ID).Status != envelope.StatusClaimed {
		t.Fatal("first council not running")
	}
	for _, id := range []string{first.ID, second.ID} {
		if res := r.await(id, 60*time.Second); res.Status != envelope.StatusAnswered {
			t.Fatalf("%s: %s\n%s", id, res.Status, res.Reply.Body)
		}
	}
}

// A council queued longer than the claim lease keeps its claim through
// renewals, and the running council keeps the claim its member asks hang
// from, so neither is requeued or redelivered.
func TestServiceRenewsClaimsPastTheLease(t *testing.T) {
	r := newServiceRig(t, relay.Config{ClaimLease: 1500 * time.Millisecond, SweepEvery: 100 * time.Millisecond}, 300*time.Millisecond)
	r.join("delta")
	r.svc.cfg.Members = append(r.svc.cfg.Members, "delta")
	r.svc.cfg.AnswerLimit = 4 * time.Second
	r.ungate()
	r.start()
	first := r.send("Which queue should we use?")
	time.Sleep(300 * time.Millisecond)
	second := r.send("Which broker should we use?")
	for _, id := range []string{first.ID, second.ID} {
		if res := r.await(id, 60*time.Second); res.Status != envelope.StatusAnswered {
			t.Fatalf("%s: %s\n%s", id, res.Status, res.Reply.Body)
		}
	}
	if log := r.log.String(); strings.Contains(log, "redelivered") {
		t.Fatalf("a claim lapsed and the request was redelivered:\n%s", log)
	}
}

// A redelivery of a council still in the queue does not queue it twice.
func TestServiceRedeliveryWhileQueuedIsNotQueuedTwice(t *testing.T) {
	r := newServiceRig(t, relay.Config{}, time.Minute)
	r.ungate()
	sent := r.send("Which queue should we use?")
	req := sent
	r.svc.Handle(t.Context(), req)
	r.svc.Handle(t.Context(), req)
	if n := len(r.svc.queue); n != 1 {
		t.Fatalf("queue holds %d entries, want 1", n)
	}
	if !strings.Contains(r.log.String(), "redelivered while queued or running") {
		t.Fatalf("log:\n%s", r.log.String())
	}
}

// A council that already finished and is redelivered is answered from the
// store: no member is asked again.
func TestServiceResendsStoredReply(t *testing.T) {
	r := newServiceRig(t, relay.Config{}, time.Minute)
	r.ungate()
	sent := r.send("Which queue should we use?")
	stored := "Recommendation: stored answer.\n"
	if err := r.store.PutCouncil(t.Context(), CouncilRecord{RequestID: sent.ID, State: CouncilCompleted, Reply: stored}); err != nil {
		t.Fatal(err)
	}
	r.start()
	res := r.await(sent.ID, 20*time.Second)
	if res.Status != envelope.StatusAnswered || res.Reply.Body != stored {
		t.Fatalf("reply %s %q, want the stored one", res.Status, res.Reply.Body)
	}
	for _, m := range []string{"alpha", "bravo", "charlie"} {
		if n := len(r.asks(m)); n != 0 {
			t.Errorf("%s was asked %d times", m, n)
		}
	}
}

// A council the last run left unfinished runs again at startup, without
// waiting for redelivery, and its scores are recorded once.
func TestServiceResumesUnfinishedCouncil(t *testing.T) {
	r := newServiceRig(t, relay.Config{}, time.Minute)
	r.ungate()
	sent := r.send("Which queue should we use?")
	req, err := r.council.Claim(t.Context(), sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(req)
	if err := r.store.PutCouncil(t.Context(), CouncilRecord{RequestID: req.ID, State: CouncilRunning, Request: string(raw)}); err != nil {
		t.Fatal(err)
	}
	r.start()
	if res := r.await(req.ID, 30*time.Second); res.Status != envelope.StatusAnswered {
		t.Fatalf("%s:\n%s", res.Status, res.Reply.Body)
	}
	rows, err := r.store.Leaderboard(t.Context(), "")
	if err != nil || len(rows) != 3 {
		t.Fatalf("leaderboard = %+v, %v", rows, err)
	}
	for _, row := range rows {
		if row.Councils != 1 {
			t.Errorf("%s counted in %d councils, want 1", row.Member, row.Councils)
		}
	}
	for _, m := range []string{"alpha", "bravo", "charlie"} {
		if n := len(r.asks(m)); n > 3 {
			t.Errorf("%s asked %d times, want one council's asks", m, n)
		}
	}
}

// When the report and scorecard cannot be uploaded (the council's
// attachment quota is full), the reply still arrives with their local
// paths.
func TestServiceRepliesWithPathsWhenUploadFails(t *testing.T) {
	r := newServiceRig(t, relay.Config{Attachments: relay.AttachmentConfig{PerAgentBytes: 1024}}, time.Minute)
	r.ungate()
	r.start()
	sent := r.send("Which queue should we use?")
	res := r.await(sent.ID, 30*time.Second)
	if res.Status != envelope.StatusAnswered || len(res.Reply.Attachments) != 0 {
		t.Fatalf("reply %s with %d attachments", res.Status, len(res.Reply.Attachments))
	}
	body := res.Reply.Body
	if !strings.Contains(body, "could not be attached") {
		t.Errorf("reply does not say the files were not attached:\n%s", body)
	}
	for _, prefix := range []string{"Report: ", "Scorecard: "} {
		i := strings.Index(body, prefix)
		if i < 0 {
			t.Fatalf("reply lacks %q:\n%s", prefix, body)
		}
		path, _, _ := strings.Cut(body[i+len(prefix):], "\n")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s%s: %v", prefix, path, err)
		}
	}
}

// A leaderboard request is answered at once with the standings as text
// and the leaderboard card attached.
func TestServiceAnswersLeaderboard(t *testing.T) {
	r := newServiceRig(t, relay.Config{}, time.Minute)
	for _, c := range []struct {
		id, category string
		scores       []Standing
	}{
		{"c1", "debugging", []Standing{{Member: "alpha", Score: 1, Placement: 1, Ballots: 2}, {Member: "bravo", Score: 0.5, Placement: 2, Ballots: 2}}},
		{"c2", "writing", []Standing{{Member: "bravo", Score: 1, Placement: 1, Ballots: 2}, {Member: "alpha", Score: 0, Placement: 2, Ballots: 2}}},
		{"c3", "debugging", []Standing{{Member: "alpha", Score: 1, Placement: 1, Ballots: 2}, {Member: "bravo", Score: 0, Placement: 2, Ballots: 2}}},
	} {
		if err := r.store.PutCouncil(t.Context(), CouncilRecord{RequestID: c.id, State: CouncilCompleted}); err != nil {
			t.Fatal(err)
		}
		if err := r.store.RecordScores(t.Context(), c.id, c.category, c.scores); err != nil {
			t.Fatal(err)
		}
	}
	r.ungate()
	r.start()
	sent := r.send(`council: {"op":"leaderboard","category":"debugging"}`)
	res := r.await(sent.ID, 20*time.Second)
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s:\n%s", res.Status, res.Reply.Body)
	}
	for _, want := range []string{"Council leaderboard (debugging)", "1. alpha: 2 wins in 2 councils, mean score 1.00", "2. bravo: 0 wins in 2 councils, mean score 0.25"} {
		if !strings.Contains(res.Reply.Body, want) {
			t.Errorf("reply lacks %q:\n%s", want, res.Reply.Body)
		}
	}
	if len(res.Reply.Attachments) != 1 || filepath.Ext(res.Reply.Attachments[0].Name) != ".png" {
		t.Errorf("attachments = %+v, want the leaderboard card", res.Reply.Attachments)
	}
	for _, m := range []string{"alpha", "bravo", "charlie"} {
		if n := len(r.asks(m)); n != 0 {
			t.Errorf("%s was asked %d times", m, n)
		}
	}
}

// faultyService rebuilds r.svc over a client that reaches the relay
// through a reverse proxy. While the returned code is non-zero the proxy
// answers every claim with that status instead of passing it on.
func (r *serviceRig) faultyService() *atomic.Int32 {
	r.t.Helper()
	target, err := url.Parse(r.mesh.URL("council"))
	if err != nil {
		r.t.Fatal(err)
	}
	fault := &atomic.Int32{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if code := int(fault.Load()); code != 0 && strings.HasSuffix(req.URL.Path, "/claim") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"injected"}`))
			return
		}
		proxy.ServeHTTP(w, req)
	}))
	r.t.Cleanup(ts.Close)
	c, err := client.NewRelayForFile(client.Config{Relay: ts.URL, Agent: "council"}, "")
	if err != nil {
		r.t.Fatal(err)
	}
	cfg := r.svc.cfg
	r.svc, err = NewService(ServiceConfig{Relay: c, Config: cfg, Store: r.store, ReportDir: filepath.Join(r.dir, "reports"),
		Hold: time.Second, RenewEvery: time.Minute, Log: r.log})
	if err != nil {
		r.t.Fatal(err)
	}
	return fault
}

// A transient relay error on the claim just before a queued council runs
// leaves the council queued for redelivery: it is not recorded as failed
// and no reply is sent.
func TestServiceTransientClaimErrorLeavesCouncilQueued(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusBadRequest} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			r := newServiceRig(t, relay.Config{}, time.Minute)
			fault := r.faultyService()
			r.ungate()
			sent := r.send("Which queue should we use?")
			r.svc.Handle(t.Context(), sent)
			req, ok := r.svc.next(t.Context())
			if !ok || req.ID != sent.ID {
				t.Fatalf("next = %+v %v, want the queued council", req, ok)
			}
			fault.Store(int32(code))
			r.svc.run(t.Context(), req)
			r.svc.done(req.ID)

			rec, err := r.store.Council(t.Context(), sent.ID)
			if err != nil || rec.State != CouncilQueued {
				t.Fatalf("stored council = %+v, %v; want still queued", rec, err)
			}
			if st := r.status(sent.ID).Status; st != envelope.StatusClaimed {
				t.Fatalf("request status %s, want still claimed (no reply sent)", st)
			}
			if !strings.Contains(r.log.String(), "leaving it for redelivery") {
				t.Fatalf("log:\n%s", r.log.String())
			}
			for _, m := range []string{"alpha", "bravo", "charlie"} {
				if n := len(r.asks(m)); n != 0 {
					t.Errorf("%s was asked %d times", m, n)
				}
			}
		})
	}
}

// A queued council whose claim lapsed (the service was stopped) and that
// the convener then cancels is dropped when it comes up to run: no member
// is asked, it is recorded as failed, and its stored reply is an
// explanation rather than empty, so a redelivery never resends nothing.
func TestServiceDropsCouncilCancelledWhileQueued(t *testing.T) {
	r := newServiceRig(t, relay.Config{ClaimLease: 500 * time.Millisecond, SweepEvery: 50 * time.Millisecond}, time.Minute)
	r.ungate()
	sent := r.send("Which queue should we use?")
	r.svc.Handle(t.Context(), sent)
	req, ok := r.svc.next(t.Context())
	if !ok || req.ID != sent.ID {
		t.Fatalf("next = %+v %v, want the queued council", req, ok)
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.status(sent.ID).Status == envelope.StatusClaimed {
		if time.Now().After(deadline) {
			t.Fatal("claim never lapsed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := r.mesh.Client(t, "codex").Cancel(t.Context(), sent.ID); err != nil {
		t.Fatal(err)
	}
	r.svc.run(t.Context(), req)
	r.svc.done(req.ID)

	rec, err := r.store.Council(t.Context(), sent.ID)
	if err != nil || rec.State != CouncilFailed {
		t.Fatalf("stored council = %+v, %v; want failed", rec, err)
	}
	if strings.TrimSpace(rec.Reply) == "" {
		t.Fatalf("dropped council stored an empty reply: %+v", rec)
	}
	if !strings.Contains(r.log.String(), "no longer open") {
		t.Fatalf("log:\n%s", r.log.String())
	}
	time.Sleep(500 * time.Millisecond)
	for _, m := range []string{"alpha", "bravo", "charlie"} {
		if n := len(r.asks(m)); n != 0 {
			t.Errorf("%s was asked %d times", m, n)
		}
	}
}

// A score write that fails for a moment is retried, so the convener is
// answered promptly and the council's scores are recorded once, without
// asking the members again.
func TestServiceRetriesTransientScoreWriteFailure(t *testing.T) {
	restore := finishRetryWaits
	finishRetryWaits = []time.Duration{300 * time.Millisecond, 300 * time.Millisecond}
	t.Cleanup(func() { finishRetryWaits = restore })
	r := newServiceRig(t, relay.Config{}, time.Minute)
	r.ungate()
	allow := failScoreInserts(t, r.store)
	time.AfterFunc(100*time.Millisecond, allow)
	sent := r.send("Which queue should we use?")
	r.svc.Handle(t.Context(), sent)
	req, ok := r.svc.next(t.Context())
	if !ok || req.ID != sent.ID {
		t.Fatalf("next = %+v %v, want the queued council", req, ok)
	}
	r.svc.run(t.Context(), req)
	r.svc.done(req.ID)
	if res := r.await(sent.ID, 30*time.Second); res.Status != envelope.StatusAnswered {
		t.Fatalf("%s:\n%s", res.Status, res.Reply.Body)
	}
	rows, err := r.store.Leaderboard(t.Context(), "debugging")
	if err != nil || len(rows) != 3 {
		t.Fatalf("leaderboard = %+v, %v; want the council's three members", rows, err)
	}
}

// When the score write keeps failing, the convener still gets the verdict
// instead of waiting out the claim lease for a rerun; the council is stored
// as completed and only its leaderboard scores are lost.
func TestServicePersistentScoreWriteFailureStillReplies(t *testing.T) {
	restore := finishRetryWaits
	finishRetryWaits = []time.Duration{50 * time.Millisecond}
	t.Cleanup(func() { finishRetryWaits = restore })
	r := newServiceRig(t, relay.Config{}, time.Minute)
	r.ungate()
	failScoreInserts(t, r.store)
	sent := r.send("Which queue should we use?")
	r.svc.Handle(t.Context(), sent)
	req, ok := r.svc.next(t.Context())
	if !ok || req.ID != sent.ID {
		t.Fatalf("next = %+v %v, want the queued council", req, ok)
	}
	r.svc.run(t.Context(), req)
	r.svc.done(req.ID)
	if res := r.await(sent.ID, 30*time.Second); res.Status != envelope.StatusAnswered {
		t.Fatalf("%s:\n%s", res.Status, res.Reply.Body)
	}
	rec, err := r.store.Council(t.Context(), sent.ID)
	if err != nil || rec.State != CouncilCompleted || rec.Reply == "" {
		t.Fatalf("stored council = %+v, %v; want completed with its reply", rec, err)
	}
	if rows, err := r.store.Leaderboard(t.Context(), ""); err != nil || len(rows) != 0 {
		t.Fatalf("leaderboard = %+v, %v; want empty", rows, err)
	}
}

// When storage is down entirely, not even the council's record can be
// saved: no verdict goes out, so the local record can never contradict an
// answered request, and the council stays unfinished for a later rerun.
func TestServiceStorageDownSendsNoVerdict(t *testing.T) {
	restore := finishRetryWaits
	finishRetryWaits = []time.Duration{50 * time.Millisecond}
	t.Cleanup(func() { finishRetryWaits = restore })
	r := newServiceRig(t, relay.Config{}, time.Minute)
	r.ungate()
	sent := r.send("Which queue should we use?")
	r.svc.Handle(t.Context(), sent)
	req, ok := r.svc.next(t.Context())
	if !ok || req.ID != sent.ID {
		t.Fatalf("next = %+v %v, want the queued council", req, ok)
	}
	failCouncilWrites(t, r.store)
	r.svc.run(t.Context(), req)
	r.svc.done(req.ID)
	if st := r.status(sent.ID).Status; st != envelope.StatusClaimed {
		t.Fatalf("request status %s, want still claimed (no verdict sent)", st)
	}
}

// Every log line starts with the service clock's local time, to the
// millisecond, so it lines up with the relay's UTC trace.
func TestLogLinesAreTimestampedWithTheServiceClock(t *testing.T) {
	at := time.Date(2026, 11, 8, 21, 55, 7, 137_000_000, time.FixedZone("PST", -8*3600))
	var log bytes.Buffer
	svc, err := NewService(ServiceConfig{Relay: &client.Relay{}, Store: &Store{}, ReportDir: t.TempDir(), Log: &log, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	svc.logf("request %s: queued", "r1")
	if got, want := log.String(), "2026-11-08T21:55:07.137-08:00 tincan council: request r1: queued\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}
