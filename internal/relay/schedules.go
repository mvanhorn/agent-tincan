package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

const (
	// MinScheduleInterval is the shortest repeat a schedule may have.
	MinScheduleInterval = 15 * time.Minute
	// maxScheduleInterval is the longest; it also keeps every_seconds from
	// overflowing the arithmetic on it.
	maxScheduleInterval = 366 * 24 * time.Hour
	// MaxSchedulePage is the most schedules one page of a listing returns.
	// A preview is 200 characters, so a page stays well under the client's
	// 4 MiB read limit however many agents an admin listing covers.
	MaxSchedulePage = 500
)

// errScheduleDuringClaim refuses a schedule from a caller with an open
// claim. A fire starts a new chain, so a schedule made while handling a
// request would shed that request's loop, hop and allowlist checks.
var errScheduleDuringClaim = errors.New("you hold a claimed request and a scheduled ask starts a new chain; reply to it first, or send an ordinary ask")

// errScheduleDuringGuestAsk refuses a schedule from a caller a guest's ask is
// open with. A fire carries no mark and reads no claims, so one made now,
// claimed or not, would go out as the caller's own work for good.
var errScheduleDuringGuestAsk = errors.New("you have an open request that a guest's ask led to, and a scheduled ask would not carry its mark; finish that request first, or send an ordinary ask")

// holdsClaim reports whether a send from agent in session would continue a
// claim, the way policy.Prepare decides it: the session's own claim, or else
// the agent-wide one, which a session cannot step out of.
func (s *Server) holdsClaim(ctx context.Context, agent, session string) (bool, error) {
	if session != "" {
		if _, ok, err := s.store.OpenClaimFor(ctx, agent, session); ok || err != nil {
			return ok, err
		}
	}
	_, ok, err := s.store.OpenClaim(ctx, agent)
	return ok, err
}

// handleCreateSchedule records an ask from the calling agent to fire later
// (at), on a repeat (every_seconds), or both, with at as the first time.
// Nothing here sends: every send check that can change by then runs again
// at fire time.
func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	from := s.agent(w, r)
	if from == "" {
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	req, err := envelope.ParseSend(raw, from, envelope.DefaultMaxBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		At           time.Time `json:"at"`
		EverySeconds int64     `json:"every_seconds"`
		Also         []string  `json:"also"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode schedule: %w", err))
		return
	}
	now := s.cfg.Now()
	switch {
	case req.Kind != envelope.KindAsk:
		err = errors.New("schedule: only an ask can be scheduled")
	case req.ParentID != "":
		err = errors.New("schedule: a scheduled ask starts a new chain and takes no parent")
	case req.Group != "" || len(in.Also) > 0:
		err = errors.New("schedule: a scheduled ask goes to one teammate")
	case len(req.Attachments) > 0:
		err = errors.New("schedule: a scheduled ask takes no attachments")
	case req.Project != "":
		err = errors.New("schedule: a scheduled ask cannot name a project")
	case req.Then != nil:
		err = errors.New("schedule: a scheduled ask takes no then")
	case in.At.IsZero() && in.EverySeconds == 0:
		err = errors.New("schedule: at or every_seconds is required")
	case !in.At.IsZero() && !in.At.After(now):
		err = errors.New("schedule: at is in the past")
	case in.EverySeconds != 0 && in.EverySeconds < int64(MinScheduleInterval/time.Second):
		err = fmt.Errorf("schedule: the shortest repeat is %v", MinScheduleInterval)
	case in.EverySeconds > int64(maxScheduleInterval/time.Second):
		err = fmt.Errorf("schedule: the longest repeat is %v", maxScheduleInterval)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.checkSendTarget(r.Context(), req.To); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	sess, err := session(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if open, err := s.store.OpenGuestAsks(r.Context(), from); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	} else if open > 0 {
		writeErr(w, http.StatusConflict, errScheduleDuringGuestAsk)
		return
	}
	if held, err := s.holdsClaim(r.Context(), from, sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	} else if held {
		writeErr(w, http.StatusConflict, errScheduleDuringClaim)
		return
	}
	next := in.At
	if next.IsZero() {
		next = now.Add(time.Duration(in.EverySeconds) * time.Second)
	}
	sch, err := s.store.AddSchedule(r.Context(), envelope.Schedule{From: from, To: req.To, Body: req.Body, Urgent: req.Urgent, NextAt: next, EverySeconds: in.EverySeconds})
	switch {
	case errors.Is(err, store.ErrNoAgent):
		writeErr(w, http.StatusNotFound, errors.New("no such agent: "+req.To))
		return
	case errors.Is(err, store.ErrScheduleLimit):
		writeErr(w, http.StatusConflict, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.record(r.Context(), "schedule_created", "", "", from, store.DetailJSON(map[string]any{"schedule": sch.ID, "to": sch.To, "next_at": sch.NextAt, "every_seconds": sch.EverySeconds}))
	writeJSON(w, http.StatusCreated, sch)
}

// handleSchedules lists the calling agent's schedules, or with all=1 every
// agent's, for an admin device. Each body is a preview: full bodies from 16
// schedules are past what Relay.callOnce reads of a response, and an owner
// who cannot list schedules cannot stop them.
func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	owner := ""
	if r.URL.Query().Get("all") == "1" {
		if !s.isAdmin(r) {
			writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
			return
		}
	} else if owner = s.agent(w, r); owner == "" {
		return
	}
	// A client that names no page gets the whole listing, as it always has.
	paged := r.URL.Query().Has("after")
	var after int64
	limit := -1
	if paged {
		var err error
		if after, err = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); err != nil || after < 0 {
			writeErr(w, http.StatusBadRequest, errors.New("after must be a schedule id"))
			return
		}
		limit = MaxSchedulePage + 1
	}
	list, err := s.store.Schedules(r.Context(), owner, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if list == nil {
		list = []envelope.Schedule{}
	}
	out := map[string]any{}
	if paged && len(list) > MaxSchedulePage {
		list = list[:MaxSchedulePage]
		out["next_after"] = list[len(list)-1].ID
	}
	for i := range list {
		list[i].Body = approvalPreview(client.ScheduleText(list[i].Body))
	}
	out["schedules"] = list
	writeJSON(w, http.StatusOK, out)
}

// handleCancelSchedule deletes a schedule of the calling agent, or any
// schedule for an admin device. A schedule that is not the caller's answers
// 404, like one that does not exist.
func (s *Server) handleCancelSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	owner, actor := "", s.remote(r)
	if !s.isAdmin(r) {
		if owner = s.agent(w, r); owner == "" {
			return
		}
		actor = owner
	}
	sch, err := s.store.CancelSchedule(r.Context(), id, owner)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "schedule_cancelled", "", "", actor, store.DetailJSON(map[string]any{"schedule": sch.ID, "from": sch.From, "to": sch.To}))
	writeJSON(w, http.StatusOK, sch)
}

// fireSchedules fires every schedule that has come due. Sweep runs from
// request handlers too, so ctx can end at any moment: once a schedule has
// moved on its fire has to finish, and a pass that ends early leaves the
// rest for the next sweep.
func (s *Server) fireSchedules(ctx context.Context) {
	due, err := s.store.DueSchedules(ctx)
	if err != nil {
		log.Printf("schedules: %v", err)
		return
	}
	for _, sch := range due {
		if ctx.Err() != nil {
			return
		}
		s.fireSchedule(context.WithoutCancel(ctx), sch)
	}
}

// fireSchedule takes one occurrence of sch. It moves the schedule on first,
// and only the sweep that won that sends, so racing sweeps send once; a
// relay that dies between the two loses the occurrence rather than sending
// it twice. A recurring schedule skips an occurrence while its last request
// is open. Everything else goes through submit, so a fire takes the send
// path a teammate's ask takes, as a new chain with no parent.
func (s *Server) fireSchedule(ctx context.Context, sch envelope.Schedule) {
	won, err := s.store.AdvanceSchedule(ctx, sch)
	if err != nil {
		log.Printf("schedule %d: %v", sch.ID, err)
		return
	}
	if !won {
		return
	}
	if ok, err := s.dir.Has(ctx, sch.From); err == nil && !ok {
		return // removed since the sweep read it
	}
	if sch.EverySeconds != 0 && sch.LastRequest != "" {
		open, err := s.requestOpen(ctx, sch.LastRequest)
		if err != nil {
			log.Printf("schedule %d: last request: %v", sch.ID, err)
		}
		if open || err != nil {
			s.record(ctx, "schedule_skipped", sch.LastRequest, "", "relay", store.DetailJSON(map[string]any{"schedule": sch.ID, "to": sch.To}))
			return
		}
	}
	_, err = s.checkSendTarget(ctx, sch.To)
	var req envelope.Request
	if err == nil {
		req, err = s.submit(ctx, envelope.Request{From: sch.From, To: sch.To, Kind: envelope.KindAsk, Body: sch.Body, Urgent: sch.Urgent, NoParent: true})
	}
	if err != nil {
		s.scheduleBlocked(ctx, sch, err)
		return
	}
	if err := s.store.ScheduleFired(ctx, sch.ID, req.ID); err != nil {
		log.Printf("schedule %d: %v", sch.ID, err)
	}
	s.record(ctx, "schedule_fired", req.ID, req.TraceID, "relay", store.DetailJSON(map[string]any{"schedule": sch.ID, "from": sch.From, "to": sch.To}))
}

// requestOpen reports whether request id can still get an answer. A request
// the store no longer has is not open.
func (s *Server) requestOpen(ctx context.Context, id string) (bool, error) {
	_, status, err := s.store.Request(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !(envelope.Result{Status: status}).Done(), nil
}

// scheduleBlocked audits a fire the send checks rejected and tells the
// asker, without the ask's text: always for a one-shot, which is gone now,
// and for a recurring schedule once per run of rejections.
func (s *Server) scheduleBlocked(ctx context.Context, sch envelope.Schedule, cause error) {
	s.record(ctx, "schedule_blocked", "", "", "relay", store.DetailJSON(map[string]any{"schedule": sch.ID, "from": sch.From, "to": sch.To, "reason": cause.Error()}))
	body := fmt.Sprintf("Scheduled ask #%d to %s was not sent: %v. ", sch.ID, sch.To, cause)
	if sch.EverySeconds == 0 {
		body += "It will not run again."
	} else {
		body += "It stays scheduled and tries again at its next time; you will not get this notice again unless a fire goes through first."
	}
	notice := envelope.Request{From: client.RelaySender, To: sch.From, Kind: envelope.KindNotify, Hop: 1, Chain: []string{client.RelaySender}, Body: body}
	ttl := s.requestTTL(ctx, sch.From)
	var err error
	told := true
	if sch.EverySeconds == 0 {
		notice, err = s.store.Enqueue(ctx, notice, ttl)
	} else {
		notice, told, err = s.store.BlockSchedule(ctx, sch.ID, notice, ttl)
	}
	if err != nil {
		log.Printf("schedule %d: notice: %v", sch.ID, err)
		return
	}
	if told {
		s.noticeQueued(ctx, notice)
	}
}
