package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

type askFiling int

const (
	noTask askFiling = iota
	workTask
	quietTask
)

// classifyAsk uses the prepared request, including the origin and context
// that policy verified.
func classifyAsk(req envelope.Request, fromKind, toKind string, openGuest bool) askFiling {
	if req.Kind != envelope.KindAsk || req.TaskID != 0 || req.From == TaskRelay ||
		req.FromGuest || openGuest || req.RecoverySource != "" || strings.HasPrefix(req.Group, envelope.PassGroupPrefix) {
		return noTask
	}
	if req.QuietAsk || req.NoParent || req.FollowOf != "" || req.RoomPostID != 0 ||
		req.SkipTaskRecovery || taskServiceKind(fromKind) || taskServiceKind(toKind) {
		return quietTask
	}
	return workTask
}

func taskServiceKind(kind string) bool {
	switch kind {
	case onboard.KindNotes, onboard.KindHistory, onboard.KindPass, onboard.KindCouncil:
		return true
	}
	return false
}

func (s *Store) fileAskTx(ctx context.Context, tx *sql.Tx, req *envelope.Request, requireHost bool) error {
	var openGuest bool
	var fromKind, toKind string
	err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT kind FROM agents WHERE name = ?1), ''),
		COALESCE((SELECT kind FROM agents WHERE name = ?2), ''),
		EXISTS (SELECT 1 FROM requests WHERE to_agent = ?1 AND `+openGuestAsk+`)
		WHERE NOT ?3 OR EXISTS (SELECT 1 FROM agents WHERE task_host = 1)`,
		req.From, req.To, requireHost).Scan(&fromKind, &toKind, &openGuest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	filing := classifyAsk(*req, fromKind, toKind, openGuest)
	if filing == noTask {
		if req.Kind == envelope.KindAsk && req.TaskID == 0 && req.RecoverySource != "" &&
			!req.FromGuest && !openGuest && !strings.HasPrefix(req.Group, envelope.PassGroupPrefix) &&
			(req.About != "" || taskReference.MatchString(req.Body)) {
			return s.fileOutsideFollowupTx(ctx, tx, req)
		}
		return nil
	}
	line, _, _ := strings.Cut(req.Body, "\n")
	title := strings.TrimSpace(line)
	if title == "" {
		title = "Ask to " + req.To
	}
	roomPost, err := askRoomPost(ctx, tx, *req)
	if err != nil {
		return err
	}
	req.TaskRoomScope = roomPost
	multipleRooms := roomPost == multipleRoomScope
	if multipleRooms {
		roomPost = 0
	}
	if req.TaskRoomScope != 0 {
		req.SkipTaskRecovery = true
	}
	private := multipleRooms || taskServiceKind(fromKind) || taskServiceKind(toKind) || (req.SkipTaskRecovery && roomPost == 0)
	now := s.now().UnixMilli()
	sensitive, err := hasSensitiveAskContext(ctx, tx, req.From, false)
	if err != nil {
		return err
	}
	private = private || sensitive
	// Follow-ups with their own text restrictions must keep their own task.
	// Scheduled and follow-on sends are independent of a sender's claim.
	if !private && roomPost == 0 && req.Status != envelope.StatusHeld && !req.NoParent && req.FollowOf == "" {
		parent, err := s.findTaskFollowup(ctx, tx, *req)
		if err != nil {
			return err
		}
		if parent != 0 {
			summary := fmt.Sprintf("%s asked %s: %s", req.From, req.To, title)
			_, err = addTaskEvent(ctx, tx, envelope.TaskEvent{
				TaskID: parent, Actor: req.From, Kind: envelope.TaskEventAsked, Body: shortAskLine(summary),
			}, now)
			req.AboutTaskID = parent
			req.SkipTaskRecovery = true
			return err
		}
	}
	// This also supplies the ordinary assignment path in enqueueTx with an
	// open task. The explicit-add limit must not refuse an otherwise valid ask.
	task := envelope.Task{
		Title: shortAskLine(title), Detail: req.Body, Status: envelope.TaskOpen, Filer: req.From,
		NamedFor: req.To, Quiet: filing == quietTask, RoomPostID: roomPost, ParticipantOnly: private,
	}
	id, err := insertTask(ctx, tx, task, now)
	if err != nil {
		return err
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{
		TaskID: id, Actor: req.From, Kind: envelope.TaskEventAdded, Status: task.Status, Chain: append(req.Chain, req.Contributors...),
	}, now); err != nil {
		return err
	}
	req.TaskID, req.TaskFiledOnSend = id, true
	return nil
}

// A task number needs a task label so issue and change numbers stay separate.
var bareNumber = regexp.MustCompile(`#[0-9]+`)

// HasTaskFollowupReference reports whether body names a task or request.
func HasTaskFollowupReference(body string) bool {
	return taskReference.MatchString(body)
}

var taskReference = regexp.MustCompile(`(?i)\b[0-9a-f]{20}\b|\btask[ \t]+#[1-9][0-9]{0,18}\b`)

func (s *Store) findTaskFollowup(ctx context.Context, tx *sql.Tx, req envelope.Request) (int64, error) {
	lookup := func(ref string) (int64, error) {
		ref = strings.ToLower(ref)
		if strings.HasPrefix(ref, "task") {
			ref = strings.TrimPrefix(strings.Fields(ref)[1], "#")
		}
		var byID, parent int64
		if len(ref) != 20 || strings.HasPrefix(ref, "#") {
			byID, _ = strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64)
		}
		err := tx.QueryRowContext(ctx, `SELECT t.id`+taskFrom+`
			WHERE t.status IN ('open', 'working') AND (t.id = ?2 OR
				(q.id = ?3 AND q.status IN ('queued', 'delivered', 'claimed') AND q.expires_at > ?4))
			AND t.room_post_id IS NULL AND t.participant_only = 0
			AND (`+taskVisible+` OR q.from_agent = ?1 OR (?5 AND `+taskHostVisible+`
				AND EXISTS (SELECT 1 FROM agents WHERE name = ?1 AND task_host = 1)))
			AND NOT `+taskHostWithheld,
			req.From, byID, ref, s.now().UnixMilli(), req.AboutHost).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		return parent, err
	}
	if req.About != "" {
		return lookup(req.About)
	}
	refs := taskReference.FindAllString(req.Body, -1)
	if len(refs) == 0 {
		if req.ParentID == "" || bareNumber.MatchString(req.Body) {
			return 0, nil
		}
		return lookup(req.ParentID)
	}
	var parent int64
	seen := make(map[string]bool)
	for _, ref := range refs {
		ref = strings.ToLower(ref)
		if seen[ref] {
			continue
		}
		seen[ref] = true
		match, err := lookup(ref)
		if err != nil {
			return 0, err
		}
		if match != 0 {
			if parent != 0 && parent != match {
				return 0, nil
			}
			parent = match
		}
	}
	return parent, nil
}

func (s *Store) fileOutsideFollowupTx(ctx context.Context, tx *sql.Tx, req *envelope.Request) error {
	now := s.now().UnixMilli()
	task := envelope.Task{
		Title: recoveredTaskTitle(req.Body, req.To), Detail: req.Body, Filer: req.From,
		Source: req.RecoverySource, Status: envelope.TaskProposed, SuggestedAssignee: req.To, NamedFor: req.To,
	}
	id, err := insertTask(ctx, tx, task, now)
	if err != nil {
		return err
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{
		TaskID: id, Actor: req.From, Kind: envelope.TaskEventAdded, Status: task.Status,
		Chain: append(req.Chain, req.Contributors...),
	}, now); err != nil {
		return err
	}
	req.SkipTaskRecovery = true
	return taskNoticeTx(ctx, tx, id, task.Status, req.From, "", now)
}

// A follow-up may have been written alongside a private or outside ask in
// another session. Such text stays in the request even if the task is public.
func hasSensitiveAskContext(ctx context.Context, tx *sql.Tx, agent string, rooms bool) (bool, error) {
	var sensitive bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM requests r LEFT JOIN task_followups f ON f.request_id = r.id
		WHERE r.to_agent = ?1 AND r.kind = 'ask'
		AND (r.status IN ('delivered', 'claimed', 'needs_input')
			OR (r.status = 'queued' AND (r.claimed_at > 0 OR r.read_by = r.to_agent)))
		AND (r.from_guest = 1 OR r.recovery_source != '' OR ?2 AND (r.room_post_id IS NOT NULL OR r.task_room_scope != 0)
			OR (r.skip_task_recovery = 1 AND r.room_post_id IS NULL AND r.task_room_scope = 0 AND f.request_id IS NULL))
	)`, agent, rooms).Scan(&sensitive)
	return sensitive, err
}

func (s *Store) taskFollowupReplyTx(ctx context.Context, tx *sql.Tx, rep envelope.Reply) error {
	var task int64
	err := tx.QueryRowContext(ctx, `SELECT task_id FROM task_followups WHERE request_id = ?`, rep.RequestID).Scan(&task)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	body := fmt.Sprintf("Follow-up %s: %s", rep.RequestID, rep.Status)
	var chain []string
	sensitive, err := hasSensitiveAskContext(ctx, tx, rep.From, true)
	if err != nil {
		return err
	}
	if !sensitive && rep.Body != "" {
		body += "\n" + rep.Body
		authors, err := taskResultAuthorsTx(ctx, tx, rep)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(authors), &chain); err != nil {
			return err
		}
	}
	_, err = addTaskEvent(ctx, tx, envelope.TaskEvent{
		TaskID: task, Actor: rep.From, Kind: envelope.TaskEventComment, Body: body, Chain: chain,
	}, s.now().UnixMilli())
	return err
}

// cancelTaskFollowupsTx releases live requests recorded under a closing task.
func (s *Store) cancelTaskFollowupsTx(ctx context.Context, tx *sql.Tx, task int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `UPDATE requests SET status = 'cancelled', lease_until = 0, updated_at = ?
		WHERE id IN (SELECT request_id FROM task_followups WHERE task_id = ?)
		AND status IN ('queued', 'delivered', 'claimed', 'held', 'needs_input') RETURNING id`, s.now().UnixMilli(), task)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := s.closeNoticesTx(ctx, tx, id); err != nil {
			return nil, err
		}
		if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{
			TaskID: task, Actor: TaskRelay, Kind: envelope.TaskEventAsked, Body: "closed follow-up " + id,
		}, s.now().UnixMilli()); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// closeQuietTaskAfterReplyTx closes a quiet task after its real answer. Its
// returned event still counts the answer in the next briefing.
func (s *Store) closeQuietTaskAfterReplyTx(ctx context.Context, tx *sql.Tx, reqID string) ([]string, error) {
	var task int64
	err := tx.QueryRowContext(ctx, `UPDATE tasks SET status = 'done' WHERE id = (SELECT task_id FROM requests WHERE id = ?1)
		AND result_request = ?1 AND status = 'returned' AND quiet = 1
		AND NOT EXISTS (SELECT 1 FROM schedules WHERE task_id = tasks.id)
		RETURNING id`, reqID).Scan(&task)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := addTaskEvent(ctx, tx, envelope.TaskEvent{
		TaskID: task, Actor: TaskRelay, Kind: envelope.TaskEventStatus, Status: envelope.TaskDone,
		Body: "closed it: answered",
	}, s.now().UnixMilli()); err != nil {
		return nil, err
	}
	if err := s.closeTaskReminderTx(ctx, tx, task); err != nil {
		return nil, err
	}
	return s.cancelTaskFollowupsTx(ctx, tx, task)
}

// CloseReturnedQuietTasks finishes quiet answers stored by an older relay.
// A held acknowledgment still expects a real reply.
func (s *Store) CloseReturnedQuietTasks(ctx context.Context) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT t.result_request FROM tasks t
		JOIN requests r ON r.id = t.result_request AND r.held_ack = ''
		JOIN replies p ON p.request_id = r.id AND p.status = 'answered'
		WHERE t.status = 'returned' AND t.quiet = 1 AND r.status = 'answered'
		AND NOT EXISTS (SELECT 1 FROM schedules WHERE task_id = t.id)
		ORDER BY t.id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	var requests []string
	for rows.Next() {
		var req string
		if err := rows.Scan(&req); err != nil {
			rows.Close()
			return nil, err
		}
		requests = append(requests, req)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var cancelled []string
	for _, req := range requests {
		ids, err := s.closeQuietTaskAfterReplyTx(ctx, tx, req)
		if err != nil {
			return nil, err
		}
		cancelled = append(cancelled, ids...)
	}
	return cancelled, tx.Commit()
}

// multipleRoomScope restricts text to the participants and owner when
// the ask could carry context from several rooms. It follows descendants.
const multipleRoomScope = -1

// askRoomPost checks every open ask that could have reached the sender,
// independently of the single verified parent used for task history.
func askRoomPost(ctx context.Context, tx *sql.Tx, req envelope.Request) (int64, error) {
	parent := req.TaskContextParentID
	if parent == "" {
		parent = req.ParentID
	}
	var post int64
	err := tx.QueryRowContext(ctx, `WITH RECURSIVE scope(parent_id, post) AS (
		SELECT '', ?2
		UNION SELECT '', ?3
		UNION
		SELECT q.parent_id, COALESCE(NULLIF(q.task_room_scope, 0), q.room_post_id, 0)
		FROM requests q
		WHERE q.id = ?4
		UNION
		SELECT q.parent_id, COALESCE(NULLIF(q.task_room_scope, 0), q.room_post_id, 0)
		FROM requests q
		WHERE NOT ?5 AND q.to_agent = ?1 AND q.kind = 'ask'
			AND (q.status IN ('delivered', 'claimed', 'needs_input')
				OR (q.status = 'queued' AND (q.claimed_at > 0 OR q.read_by = q.to_agent)))
		UNION
		SELECT q.parent_id, COALESCE(NULLIF(q.task_room_scope, 0), q.room_post_id, 0)
		FROM scope s JOIN requests q ON q.id = s.parent_id WHERE s.post = 0
	)
	SELECT CASE WHEN MIN(s.post) < 0 OR COUNT(DISTINCT p.room_id) > 1 THEN ?6
		ELSE COALESCE(MAX(p.id), 0) END
	FROM scope s LEFT JOIN room_posts p ON p.id = s.post`,
		req.From, req.RoomPostID, req.TaskRoomScope, parent, req.NoParent, multipleRoomScope).Scan(&post)
	return post, err
}

func shortAskLine(line string) string {
	const max = 200
	if len(line) <= max {
		return line
	}
	end := max - len("…")
	for !utf8.RuneStart(line[end]) {
		end--
	}
	return line[:end] + "…"
}

// EscalateQuietQuestions makes stalled clarifications visible and queues
// their first task notices without ending the requests.
func (s *Store) EscalateQuietQuestions(ctx context.Context, after time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	rows, err := tx.QueryContext(ctx, `UPDATE tasks SET quiet = 2
		WHERE id IN (
			SELECT t.id FROM requests r JOIN tasks t ON t.id = r.task_id AND t.current_request = r.id
			WHERE r.status = 'needs_input' AND r.updated_at <= ? AND t.quiet = 1 AND t.status = 'question'
			ORDER BY r.updated_at, t.id LIMIT 20
		) RETURNING id, current_request, assignee`, now.Add(-after).UnixMilli())
	if err != nil {
		return err
	}
	type question struct {
		task         int64
		id, assignee string
	}
	var questions []question
	for rows.Next() {
		var q question
		if err := rows.Scan(&q.task, &q.id, &q.assignee); err != nil {
			rows.Close()
			return err
		}
		questions = append(questions, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, q := range questions {
		ev := envelope.TaskEvent{
			TaskID: q.task, Actor: TaskRelay, Kind: envelope.TaskEventStatus,
			Status: envelope.TaskQuestion, Body: "clarification is still unanswered",
		}
		if _, err := addTaskEvent(ctx, tx, ev, now.UnixMilli()); err != nil {
			return err
		}
		if err := taskAskNoticeTx(ctx, tx, q.id, envelope.TaskQuestion, q.assignee, now.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
