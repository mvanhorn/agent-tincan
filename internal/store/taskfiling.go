package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	if req.NoParent || req.FollowOf != "" || req.RoomPostID != 0 ||
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
	private := taskServiceKind(fromKind) || taskServiceKind(toKind) || (req.SkipTaskRecovery && roomPost == 0)
	now := s.now().UnixMilli()
	// A scheduled or follow-on fire is independent of what the nominal
	// sender is working on when the relay sends it.
	var parentTask, parentRoomPost int64
	err = sql.ErrNoRows
	if req.ParentID != "" && !req.NoParent && req.FollowOf == "" {
		err = tx.QueryRowContext(ctx, `SELECT q.task_id, COALESCE(t.room_post_id, 0) FROM requests q JOIN tasks t ON t.id = q.task_id AND t.current_request = q.id
		WHERE q.id = ? AND q.to_agent = ? AND q.status = 'claimed'`, req.ParentID, req.From).Scan(&parentTask, &parentRoomPost)
	}
	if err == nil {
		summary := fmt.Sprintf("%s asked %s", req.From, req.To)
		// An unapproved child ask's text must not reach readers of its
		// parent task. Record that the ask happened with no text.
		if !private && req.Status != envelope.StatusHeld && (roomPost == 0 || roomPost == parentRoomPost) {
			summary += ": " + title
		}
		_, err = addTaskEvent(ctx, tx, envelope.TaskEvent{
			TaskID: parentTask, Actor: req.From, Kind: envelope.TaskEventAsked, Body: shortAskLine(summary),
		}, now)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
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

// askRoomPost keeps task visibility tied to the room even when an ask
// continues an older request or begins a new trace in another session.
func askRoomPost(ctx context.Context, tx *sql.Tx, req envelope.Request) (int64, error) {
	if req.RoomPostID != 0 {
		return req.RoomPostID, nil
	}
	if req.TaskRoomScope != 0 {
		return req.TaskRoomScope, nil
	}
	parent := req.TaskContextParentID
	if parent == "" {
		parent = req.ParentID
	}
	if parent == "" || !req.SkipTaskRecovery {
		return 0, nil
	}
	var post int64
	err := tx.QueryRowContext(ctx, `WITH RECURSIVE chain AS (
		SELECT q.parent_id, COALESCE(q.room_post_id, NULLIF(q.task_room_scope, 0), t.room_post_id, 0) AS post
		FROM requests q LEFT JOIN tasks t ON t.id = q.task_id WHERE q.id = ?
		UNION ALL
		SELECT q.parent_id, COALESCE(q.room_post_id, NULLIF(q.task_room_scope, 0), t.room_post_id, 0)
		FROM chain c JOIN requests q ON q.id = c.parent_id LEFT JOIN tasks t ON t.id = q.task_id WHERE c.post = 0
	)
	SELECT COALESCE(MAX(post), 0) FROM chain`, parent).Scan(&post)
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
