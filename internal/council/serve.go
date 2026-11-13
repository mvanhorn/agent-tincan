package council

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/textfit"
)

// DefaultAgentName is Council's Tincan name.
const DefaultAgentName = "council"

// DefaultRenewEvery is how often every queued and running council's
// claim is renewed with a progress note. It stays well under the relay's
// 30-minute claim lease, which a council waiting in the queue can outlast.
const DefaultRenewEvery = 5 * time.Minute

// handleTimeout bounds handling one delivered request on the poll loop:
// a claim and a queue entry, or a leaderboard. Councils run off the loop.
const handleTimeout = 2 * time.Minute

// finishTimeout bounds saving, storing, and replying once a council has
// run. It is detached from the service's context, so a council that ran
// is not run again because the service was stopping.
const finishTimeout = 2 * time.Minute

// finishRetryWaits spaces the retries of a failed final write: a completed
// council's record and scores are retried briefly before the reply goes out
// without its leaderboard scores.
var finishRetryWaits = []time.Duration{time.Second, 3 * time.Second}

// replyTimeout bounds sending one reply or progress note.
const replyTimeout = 30 * time.Second

// ServiceConfig is everything a Council service needs. Relay and Store
// are required.
type ServiceConfig struct {
	// Relay is the client the service polls, asks members, and replies
	// with, as its own agent identity.
	Relay *client.Relay
	// Config is council.json.
	Config Config
	// Store keeps councils and scores.
	Store *Store
	// ReportDir is where reports and cards are written.
	ReportDir string
	// Hold is the long-poll hold (client.DefaultPollHold when zero).
	Hold time.Duration
	// RenewEvery is how often queued and running claims are renewed
	// (DefaultRenewEvery when zero).
	RenewEvery time.Duration
	// Log receives one line per request, stage, and error (stderr when
	// nil), each starting with the time on Now.
	Log io.Writer
	// Now is the clock log lines are stamped with (time.Now when nil).
	Now func() time.Time
}

// Service is the Council teammate. It claims each convening request as
// soon as it is delivered and runs councils one at a time from a queue,
// since each web member handles one ask at a time anyway.
type Service struct {
	relay      *client.Relay
	cfg        Config
	store      *Store
	reportDir  string
	hold       time.Duration
	renewEvery time.Duration
	log        io.Writer
	now        func() time.Time

	mu      sync.Mutex
	queue   []envelope.Request
	running string            // request id of the council running, "" when idle
	notes   map[string]string // request id -> the last progress note posted
	wake    chan struct{}
}

// NewService builds a service from cfg.
func NewService(cfg ServiceConfig) (*Service, error) {
	switch {
	case cfg.Relay == nil:
		return nil, errors.New("council: no relay client")
	case cfg.Store == nil:
		return nil, errors.New("council: no store")
	case cfg.ReportDir == "":
		return nil, errors.New("council: no report folder")
	}
	s := &Service{
		relay:      cfg.Relay,
		cfg:        cfg.Config,
		store:      cfg.Store,
		reportDir:  cfg.ReportDir,
		hold:       cfg.Hold,
		renewEvery: cfg.RenewEvery,
		log:        cfg.Log,
		now:        cfg.Now,
		notes:      map[string]string{},
		wake:       make(chan struct{}, 1),
	}
	if s.renewEvery <= 0 {
		s.renewEvery = DefaultRenewEvery
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

func (s *Service) logf(format string, args ...any) {
	w := s.log
	if w == nil {
		w = os.Stderr
	}
	_, _ = io.WriteString(w, client.LogLines(s.now(), fmt.Sprintf("tincan council: "+format, args...)))
}

// Run refuses to serve unless the relay stores kind council for this
// agent: only then does the relay hold agents' councils for the owner's
// approval. It then queues the councils a previous run left
// unfinished, and polls, runs, and renews claims until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	me, err := s.relay.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("council: could not confirm this agent's kind with the relay: %w", err)
	}
	if me.Kind != onboard.KindCouncil {
		return fmt.Errorf("council serve refuses to run: the relay stores kind %q for %s, not %q, so it would not hold agents' councils for the owner's approval. "+
			"Upgrade the relay first, then on an admin device run: tincan kind %s %s", me.Kind, me.Name, onboard.KindCouncil, me.Name, onboard.KindCouncil)
	}
	s.resume(ctx)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { s.work(rctx) })
	wg.Go(func() { s.renewLoop(rctx) })
	err = history.RunPolling(rctx, s.PollOnce, s.logf)
	cancel()
	wg.Wait()
	return err
}

// PollOnce waits up to the hold for requests and handles each one.
// PollAndHandle is serial, so handling only claims and queues a council:
// the relay's 2-minute delivery lease would expire on a request waiting
// behind a running council.
func (s *Service) PollOnce(ctx context.Context) (int, error) {
	return history.PollAndHandle(ctx, s.relay, s.hold, "council-serve", s.handleSafely)
}

// resume queues the councils the store says are queued or running: a
// previous run stopped before finishing them, and their claims may still
// hold, so waiting for redelivery could take up to the claim lease.
func (s *Service) resume(ctx context.Context) {
	recs, err := s.store.UnfinishedCouncils(ctx)
	if err != nil {
		s.logf("unfinished councils: %v", err)
		return
	}
	for _, rec := range recs {
		var req envelope.Request
		if err := json.Unmarshal([]byte(rec.Request), &req); err != nil || req.ID != rec.RequestID {
			s.logf("request %s: stored request unreadable, not resuming it: %v", rec.RequestID, err)
			continue
		}
		s.enqueue(req)
		s.logf("request %s from %s: unfinished at the last stop, queued again", req.ID, req.From)
	}
}

// handleSafely handles one request so that nothing it does, including a
// panic, stops the loop.
func (s *Service) handleSafely(ctx context.Context, req envelope.Request) {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handleTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			s.logf("request %s: panic: %v", req.ID, p)
			s.sendFinal(hctx, req, "Council hit an internal error handling this request.", envelope.StatusFailed)
		}
	}()
	s.Handle(hctx, req)
}

// Handle claims one request, then resends a finished council's stored
// reply, queues a new or unfinished council, or answers a leaderboard
// request at once.
func (s *Service) Handle(ctx context.Context, req envelope.Request) {
	claimed, err := s.relay.Claim(ctx, req.ID)
	if err != nil {
		s.logf("request %s from %s: claim failed: %v", req.ID, req.From, err)
		return
	}
	if claimed.ID != "" {
		req = claimed
	}

	rec, err := s.store.Council(ctx, req.ID)
	switch {
	case err == nil && rec.State != CouncilQueued && rec.State != CouncilRunning:
		s.logf("request %s from %s: redelivered after the council %s, resending its reply", req.ID, req.From, rec.State)
		reply := rec.Reply
		if strings.TrimSpace(reply) == "" {
			reply = droppedReply
		}
		s.sendFinal(ctx, req, reply, rec.State.replyStatus(), rec.ReportPath, rec.CardPath)
		return
	case err == nil:
		if s.enqueue(req) {
			s.logf("request %s from %s: redelivered unfinished council, queued again", req.ID, req.From)
			return
		}
		s.logf("request %s from %s: redelivered while queued or running", req.ID, req.From)
		s.renewOne(ctx, req.ID)
		return
	case !errors.Is(err, ErrCouncilNotFound):
		// Left claimed: the relay redelivers it once the lease runs out.
		s.logf("request %s: store: %v", req.ID, err)
		return
	}

	r, err := ParseRequest(req.Body, s.cfg)
	if err != nil {
		s.logf("request %s from %s: declined: %v", req.ID, req.From, err)
		s.sendFinal(ctx, req, "Council declined: "+err.Error(), envelope.StatusDeclined)
		return
	}
	if r.Op == OpLeaderboard {
		s.leaderboard(ctx, req, r)
		return
	}
	rec, ok := s.newRecord(req, CouncilQueued)
	if !ok {
		return
	}
	if err := s.store.PutCouncil(ctx, rec); err != nil {
		s.logf("request %s: store: %v", req.ID, err)
		return
	}
	s.enqueue(req)
	s.logf("request %s from %s: council queued", req.ID, req.From)
	s.renewOne(ctx, req.ID)
}

// droppedReply is the reply stored for a council dropped because its
// request was no longer open when it came up to run, so a redelivery never
// resends an empty reply.
const droppedReply = "Council did not run: the request was no longer open (cancelled, expired, or already answered) when its turn came."

// newRecord is req's council record in state, carrying req serialized so
// a restart can run it again. When req cannot be serialized it logs why
// and returns false, with the record's Request left empty.
func (s *Service) newRecord(req envelope.Request, state CouncilState) (CouncilRecord, bool) {
	rec := CouncilRecord{RequestID: req.ID, State: state}
	raw, err := json.Marshal(req)
	if err != nil {
		s.logf("request %s: store: %v", req.ID, err)
		return rec, false
	}
	rec.Request = string(raw)
	return rec, true
}

// enqueue adds req to the queue unless it is already queued or running.
func (s *Service) enqueue(req envelope.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == req.ID || slices.ContainsFunc(s.queue, func(q envelope.Request) bool { return q.ID == req.ID }) {
		return false
	}
	s.queue = append(s.queue, req)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

// next waits for the next queued council and marks it running.
func (s *Service) next(ctx context.Context) (envelope.Request, bool) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			req := s.queue[0]
			s.queue = s.queue[1:]
			s.running = req.ID
			s.mu.Unlock()
			return req, true
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return envelope.Request{}, false
		case <-s.wake:
		}
	}
}

func (s *Service) done(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = ""
	delete(s.notes, id)
}

// work runs queued councils one at a time until ctx ends.
func (s *Service) work(ctx context.Context) {
	for {
		req, ok := s.next(ctx)
		if !ok {
			return
		}
		s.runSafely(ctx, req)
		s.done(req.ID)
	}
}

// renewLoop renews every queued and running claim each renewEvery.
func (s *Service) renewLoop(ctx context.Context) {
	t := time.NewTicker(s.renewEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.renew(ctx)
		}
	}
}

// renew posts a progress note on every queued and running council, which
// renews its claim: the running one repeats its last stage note, and each
// queued one says how many councils are ahead of it.
func (s *Service) renew(ctx context.Context) {
	s.mu.Lock()
	ids := s.queueIDs()
	notes := make([]string, len(ids))
	for i, id := range ids {
		notes[i] = s.noteLocked(id)
	}
	s.mu.Unlock()
	for i, id := range ids {
		s.postProgress(ctx, id, notes[i])
	}
}

// queueIDs is the running council, then the queued ones in order; s.mu
// must be held.
func (s *Service) queueIDs() []string {
	var ids []string
	if s.running != "" {
		ids = append(ids, s.running)
	}
	for _, q := range s.queue {
		ids = append(ids, q.ID)
	}
	return ids
}

// noteLocked is the progress note that renews id's claim: the running
// council's last stage note, or a queued one's place; s.mu must be held.
func (s *Service) noteLocked(id string) string {
	if id == s.running {
		if n := s.notes[id]; n != "" {
			return n
		}
		return "Council starting"
	}
	ahead := slices.Index(s.queueIDs(), id)
	if ahead <= 0 {
		return "Council starting"
	}
	return fmt.Sprintf("Waiting behind another council (%d ahead)", ahead)
}

// renewOne posts id's renewing progress note.
func (s *Service) renewOne(ctx context.Context, id string) {
	s.mu.Lock()
	note := s.noteLocked(id)
	s.mu.Unlock()
	s.postProgress(ctx, id, note)
}

// progress records note as id's latest stage and posts it.
func (s *Service) progress(ctx context.Context, id, note string) {
	s.mu.Lock()
	s.notes[id] = note
	s.mu.Unlock()
	s.logf("request %s: %s", id, note)
	s.postProgress(ctx, id, note)
}

// postProgress posts a progress note; a failure is only logged.
func (s *Service) postProgress(ctx context.Context, id, note string) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)
	defer cancel()
	if n, cut := textfit.Truncate(note, envelope.MaxProgressNote); cut {
		note = n
	}
	if err := s.relay.Progress(pctx, id, note); err != nil {
		s.logf("request %s: progress note failed: %v", id, err)
	}
}

// runSafely runs one council so that a panic fails it rather than the
// service.
func (s *Service) runSafely(ctx context.Context, req envelope.Request) {
	defer func() {
		if p := recover(); p != nil {
			s.logf("request %s: panic: %v", req.ID, p)
			f := Finished{RequestID: req.ID, Question: req.Body, At: convenedAt(req),
				Outcome: Outcome{State: CouncilFailed, Reason: "Council hit an internal error running it"}}
			s.finish(ctx, req, f)
		}
	}()
	s.run(ctx, req)
}

// run runs one queued council from claim to reply. A council interrupted
// by the service stopping stays unfinished in the store and runs again at
// the next start.
func (s *Service) run(ctx context.Context, req envelope.Request) {
	// Claim again: the claim may have lapsed while the council was queued
	// or the service was stopped, and a request the convener cancelled or
	// that expired must not be run.
	cur, err := s.relay.Claim(ctx, req.ID)
	if err != nil {
		// Only a relay that no longer has the request open (gone, or
		// cancelled, expired, or answered) drops it. Any other error, a 5xx
		// or a proxy's 502 among them, may be transient.
		if gone := client.IsStatus(err, http.StatusNotFound) || client.IsStatus(err, http.StatusConflict); gone && ctx.Err() == nil {
			s.logf("request %s: no longer open (%v), dropping the council", req.ID, err)
			rec, _ := s.newRecord(req, CouncilFailed)
			rec.Reply = droppedReply
			if err := s.store.PutCouncil(context.WithoutCancel(ctx), rec); err != nil {
				s.logf("request %s: store: %v", req.ID, err)
			}
			return
		}
		// Left unfinished: a redelivery or the next start queues it again.
		s.logf("request %s: claim failed, leaving it for redelivery: %v", req.ID, err)
		return
	}
	if cur.ID != "" {
		req = cur
	}
	rec, _ := s.newRecord(req, CouncilRunning)
	if err := s.store.PutCouncil(ctx, rec); err != nil {
		s.logf("request %s: store: %v", req.ID, err)
	}

	f := Finished{RequestID: req.ID, Question: req.Body, At: convenedAt(req)}
	r, err := ParseRequest(req.Body, s.cfg)
	if err != nil {
		f.Outcome = Outcome{State: CouncilDeclined, Reason: err.Error()}
		s.finish(ctx, req, f)
		return
	}
	f.Question = r.Question
	agents, err := s.relay.Agents(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		f.Outcome = Outcome{State: CouncilFailed, Reason: "Council could not read the team roster: " + err.Error()}
		s.finish(ctx, req, f)
		return
	}
	roster := make([]onboard.Member, len(agents))
	for i, a := range agents {
		roster[i] = onboard.Member{Name: a.Name, Wake: a.Wake, Online: a.Online, Kind: a.Kind}
	}
	el := Resolve(roster, req, s.cfg, r)
	f.Excluded = el.Excluded
	if el.Decline != "" {
		f.Outcome = Outcome{State: CouncilDeclined, Reason: el.Decline}
		s.finish(ctx, req, f)
		return
	}
	var seats []Seat
	for _, name := range el.Members {
		i := slices.IndexFunc(roster, func(m onboard.Member) bool { return m.Name == name })
		seats = append(seats, Seat{Name: name, Web: onboard.ProfileOf(roster[i]).Web})
	}
	var webChairmen []string
	for _, name := range el.Chairmen {
		i := slices.IndexFunc(roster, func(m onboard.Member) bool { return m.Name == name })
		if i >= 0 && onboard.ProfileOf(roster[i]).Web {
			webChairmen = append(webChairmen, name)
		}
	}
	f.Asked = el.Members
	s.logf("request %s from %s: convening %s", req.ID, req.From, strings.Join(el.Members, ", "))

	e := &Engine{Relay: s.relay, Config: s.cfg}
	out, err := e.Run(ctx, Council{Request: req, Question: r.Question, Members: seats, Chairmen: el.Chairmen, WebChairmen: webChairmen,
		Progress: func(note string) { s.progress(ctx, req.ID, note) }})
	if err != nil {
		s.logf("request %s: stopped mid-council, it runs again at the next start: %v", req.ID, err)
		return
	}
	f.Outcome = out
	s.finish(ctx, req, f)
}

// finish saves the report and scorecard, stores the council's final state
// and reply together with a completed council's scores, and replies with
// the report and scorecard attached. A completed council that cannot be
// stored gets no reply and stays unfinished, so it is never answered with
// its scores lost.
func (s *Service) finish(ctx context.Context, req envelope.Request, f Finished) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if f.Outcome.State != CouncilDeclined {
		var err error
		if f.ReportPath, f.CardPath, err = SaveArtifacts(s.reportDir, f); err != nil {
			s.logf("request %s: %v", req.ID, err)
		}
	}
	body, status := Reply(f)
	rec, _ := s.newRecord(req, f.Outcome.State)
	rec.Reply, rec.ReportPath, rec.CardPath = body, f.ReportPath, f.CardPath
	if f.Outcome.State == CouncilCompleted {
		rec.Category = f.category()
	}
	err := s.store.FinishCouncil(fctx, rec, f.Outcome.Standings)
	for _, wait := range finishRetryWaits {
		if err == nil || f.Outcome.State != CouncilCompleted {
			break
		}
		select {
		case <-fctx.Done():
		case <-time.After(wait):
		}
		err = s.store.FinishCouncil(fctx, rec, f.Outcome.Standings)
	}
	if err != nil {
		s.logf("request %s: store: %v", req.ID, err)
		// A verdict goes out only once its record is saved, so the store never
		// contradicts an answered request. If only the scores failed, the
		// convener still gets the verdict and just the leaderboard row is lost.
		// If storage is down, the council stays unfinished and runs again on
		// redelivery or at the next start.
		if perr := s.store.PutCouncil(fctx, rec); perr != nil {
			s.logf("request %s: store: %v; council left unfinished, it runs again on redelivery or at the next start", req.ID, perr)
			return
		}
		if f.Outcome.State == CouncilCompleted {
			s.logf("request %s: leaderboard scores for this council were not recorded", req.ID)
		}
	}
	s.logf("request %s from %s: council %s (%s)", req.ID, req.From, f.Outcome.State, f.statusLine())
	s.sendFinal(fctx, req, body, status, f.ReportPath, f.CardPath)
}

// leaderboard answers a leaderboard request with the standings as text
// and the leaderboard card attached.
func (s *Service) leaderboard(ctx context.Context, req envelope.Request, r Request) {
	rows, err := s.store.Leaderboard(ctx, r.Category)
	if err != nil {
		s.logf("request %s: leaderboard: %v", req.ID, err)
		s.sendFinal(ctx, req, "Council could not read its leaderboard.", envelope.StatusFailed)
		return
	}
	body := leaderboardText(rows, r.Category)
	card, err := SaveLeaderboardCard(s.reportDir, rows, r.Category, time.Now())
	if err != nil {
		s.logf("request %s: %v", req.ID, err)
		card = ""
	} else {
		body += "\nCard: " + card + "\n"
	}
	s.logf("request %s from %s: leaderboard answered", req.ID, req.From)
	s.sendFinal(ctx, req, body, envelope.StatusAnswered, card)
}

// leaderboardText is the leaderboard for category ("" is overall) as
// plain text: members ranked by wins, then mean peer score.
func leaderboardText(rows []LeaderboardRow, category string) string {
	scope := "overall"
	if category != "" {
		scope = category
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Council leaderboard (%s), ranked by wins, then mean score from blind peer review (0 to 1):\n", scope)
	if len(rows) == 0 {
		b.WriteString("No scored councils yet.\n")
	}
	for i, r := range rows {
		fmt.Fprintf(&b, "%d. %s: %d %s in %d %s, mean score %.2f\n", i+1, r.Member,
			r.Wins, plural(r.Wins, "win", "wins"), r.Councils, plural(r.Councils, "council", "councils"), r.MeanScore)
	}
	return b.String()
}

// sendFinal replies with the files at paths attached. When they cannot be
// attached the reply still goes out, and says so: its text names where
// the files are saved on the owner's machine.
func (s *Service) sendFinal(ctx context.Context, req envelope.Request, body string, status envelope.Status, paths ...string) {
	var files []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			s.logf("request %s: %v", req.ID, err)
			continue
		}
		files = append(files, p)
	}
	var ids []string
	if len(files) > 0 {
		ups, err := s.relay.UploadFiles(ctx, files)
		switch {
		case errors.Is(err, client.ErrAttachmentsUnsupported):
			body += "\nThe files could not be attached: this relay does not support attachments. They are saved on the owner's machine at the paths above.\n"
		case err != nil:
			s.logf("request %s: attach: %v", req.ID, err)
			body += "\nThe files could not be attached: the upload to the relay failed. They are saved on the owner's machine at the paths above.\n"
		default:
			ids = client.AttachmentIDs(ups)
		}
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)
	defer cancel()
	if _, err := s.relay.ReplyFinal(rctx, req.ID, body, status, ids, true); err != nil { // the model's text is the whole answer; nothing follows up
		s.logf("request %s: reply failed: %v", req.ID, err)
	}
}

// convenedAt is when req was sent, or now for a request without a time.
func convenedAt(req envelope.Request) time.Time {
	if req.CreatedAt.IsZero() {
		return time.Now()
	}
	return req.CreatedAt
}
