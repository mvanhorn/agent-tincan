package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

var (
	// ErrNoRoom means no room has that name.
	ErrNoRoom = errors.New("no such room")
	// ErrRoomExists means a room with that name already exists.
	ErrRoomExists = errors.New("a room with that name already exists")
	// ErrNotRoomMember means the agent is not a member of the room.
	ErrNotRoomMember = errors.New("not a member of this room")
)

// MaxRoomPage bounds how many posts one read of a room returns.
const MaxRoomPage = 200

// MaxRoomPageBytes bounds a page of posts as JSON, so a read stays well
// under the client's 4 MiB response limit however much the posts' text
// grows when escaped. A page always holds at least one post.
const MaxRoomPageBytes = 1 << 20

// migrateRooms creates the rooms, room_members and room_posts tables. Like
// watches, members and posts name agents with no foreign key to agents,
// since a rebind re-inserts the agent row; DeleteAgent drops an agent's
// memberships instead, and its posts stay under its name. Posts are read by
// room in id order, which is also time order. A mirrored reply (a reply to
// a mention, posted as the replier's post) names its request and reply
// generation, and the partial unique index lets each generation post once.
func (s *Store) migrateRooms() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS rooms (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS room_members (
  room_id  INTEGER NOT NULL REFERENCES rooms(id),
  agent    TEXT NOT NULL,
  added_at INTEGER NOT NULL,
  PRIMARY KEY (room_id, agent)
);
CREATE INDEX IF NOT EXISTS room_members_agent ON room_members(agent);
CREATE TABLE IF NOT EXISTS room_posts (
  id               INTEGER PRIMARY KEY,
  room_id          INTEGER NOT NULL REFERENCES rooms(id),
  author           TEXT NOT NULL,
  body             TEXT NOT NULL,
  created_at       INTEGER NOT NULL,
  status           TEXT NOT NULL DEFAULT '',
  reply_to         TEXT NOT NULL DEFAULT '',
  reply_generation INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS room_posts_room ON room_posts(room_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS room_posts_reply ON room_posts(reply_to, reply_generation) WHERE reply_to != '';`)
	if err != nil {
		return err
	}
	// room_post_id marks an ask built from a mention: the post it came
	// from, NULL for every other request.
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'room_post_id')`).Scan(&has); err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE requests ADD COLUMN room_post_id INTEGER`); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS requests_room_post ON requests(room_post_id) WHERE room_post_id IS NOT NULL`)
	return err
}

// CreateRoom creates an empty room called name.
func (s *Store) CreateRoom(ctx context.Context, name string) (envelope.Room, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO rooms (name, created_at) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`, name, now.UnixMilli())
	if err != nil {
		return envelope.Room{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.Room{}, ErrRoomExists
	}
	return envelope.Room{Name: name, Members: []string{}, CreatedAt: now.UTC().Truncate(time.Millisecond)}, nil
}

// roomID returns the id of the room called name, or ErrNoRoom.
func roomID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM rooms WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoRoom
	}
	return id, err
}

// AddRoomMember adds agent to room. It reports false when agent was
// already a member.
func (s *Store) AddRoomMember(ctx context.Context, room, agent string) (bool, error) {
	id, err := roomID(ctx, s.db, room)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO room_members (room_id, agent, added_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, id, agent, s.now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RemoveRoomMember takes agent out of room. It reports false when agent
// was not a member. In the same transaction it cancels agent's open asks
// built from mentions in room, each with a relay note for the poster, and
// returns their ids: a member the owner took out answers nothing more from
// the room.
func (s *Store) RemoveRoomMember(ctx context.Context, room, agent string) (bool, []string, error) {
	id, err := roomID(ctx, s.db, room)
	if err != nil {
		return false, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM room_members WHERE room_id = ? AND agent = ?`, id, agent)
	if err != nil {
		return false, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	now := s.now().UnixMilli()
	rows, err := tx.QueryContext(ctx, `UPDATE requests SET status = ?, lease_until = 0, updated_at = ?
		WHERE to_agent = ? AND status IN (?, ?, ?, ?, ?) AND room_post_id IN (SELECT id FROM room_posts WHERE room_id = ?) RETURNING id`,
		string(envelope.StatusCancelled), now, agent,
		string(envelope.StatusQueued), string(envelope.StatusDelivered), string(envelope.StatusClaimed), string(envelope.StatusNeedsInput), string(envelope.StatusHeld), id)
	if err != nil {
		return false, nil, err
	}
	var ids []string
	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			return false, nil, err
		}
		ids = append(ids, rid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, nil, err
	}
	note := fmt.Sprintf("Cancelled: %s was removed from room %s.", agent, room)
	for _, rid := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO relay_notes (request_id, kind, episode, note, at) VALUES (?, ?, 0, ?, ?)
			ON CONFLICT(request_id, kind, episode) DO NOTHING`, rid, NoteRoomRemoved, note, now); err != nil {
			return false, nil, err
		}
	}
	return n > 0, ids, tx.Commit()
}

// RoomPostMember reports whether agent is now a member of the room post
// postID is in; false when there is no such post.
func (s *Store) RoomPostMember(ctx context.Context, postID int64, agent string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM room_posts p JOIN room_members m ON m.room_id = p.room_id AND m.agent = ? WHERE p.id = ?)`, agent, postID).Scan(&ok)
	return ok, err
}

// IsRoomMember reports whether agent is a member of room, or ErrNoRoom.
func (s *Store) IsRoomMember(ctx context.Context, room, agent string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM room_members m WHERE m.room_id = r.id AND m.agent = ?) FROM rooms r WHERE r.name = ?`, agent, room).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNoRoom
	}
	return ok, err
}

// Room returns the room called name with its members, or ErrNoRoom.
func (s *Store) Room(ctx context.Context, name string) (envelope.Room, error) {
	rooms, err := s.rooms(ctx, `WHERE r.name = ?`, name)
	if err != nil {
		return envelope.Room{}, err
	}
	if len(rooms) == 0 {
		return envelope.Room{}, ErrNoRoom
	}
	return rooms[0], nil
}

// Rooms lists the rooms member is in, by name, each with its members.
// member "" lists every room.
func (s *Store) Rooms(ctx context.Context, member string) ([]envelope.Room, error) {
	if member == "" {
		return s.rooms(ctx, ``)
	}
	return s.rooms(ctx, `WHERE r.id IN (SELECT room_id FROM room_members WHERE agent = ?)`, member)
}

func (s *Store) rooms(ctx context.Context, where string, args ...any) ([]envelope.Room, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.name, r.created_at, COALESCE((SELECT MAX(id) FROM room_posts WHERE room_id = r.id), 0),
		COALESCE((SELECT group_concat(agent, ' ') FROM (SELECT agent FROM room_members WHERE room_id = r.id ORDER BY agent)), '')
		FROM rooms r `+where+` ORDER BY r.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []envelope.Room{}
	for rows.Next() {
		var rm envelope.Room
		var created int64
		var members string
		if err := rows.Scan(&rm.Name, &created, &rm.LastPost, &members); err != nil {
			return nil, err
		}
		rm.CreatedAt = time.UnixMilli(created).UTC()
		rm.Members = strings.Fields(members) // never nil, so JSON shows []
		out = append(out, rm)
	}
	return out, rows.Err()
}

// AddRoomPost stores author's post in room. The membership check and the
// insert are one statement, so an author removed meanwhile posts nothing
// (ErrNotRoomMember).
func (s *Store) AddRoomPost(ctx context.Context, room, author, body string) (envelope.RoomPost, error) {
	id, err := roomID(ctx, s.db, room)
	if err != nil {
		return envelope.RoomPost{}, err
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO room_posts (room_id, author, body, created_at)
		SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM room_members WHERE room_id = ? AND agent = ?)`,
		id, author, body, now.UnixMilli(), id, author)
	if err != nil {
		return envelope.RoomPost{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return envelope.RoomPost{}, ErrNotRoomMember
	}
	postID, err := res.LastInsertId()
	if err != nil {
		return envelope.RoomPost{}, err
	}
	return envelope.RoomPost{ID: postID, Room: room, Author: author, Body: body, CreatedAt: now.UTC().Truncate(time.Millisecond)}, nil
}

// mirrorReplyTx posts rep, the reply just written in tx, into the room of
// the mention its request came from, as the replier's post: with the
// reply's status, and under the request id and reply generation, which
// may post once. It does nothing for a request that is not a mention, for
// a reply marked NoMirror (the relay found the replier no longer allowed
// in rooms), or when the replier is no longer a member of the room; a
// caller holding the request can skip it when RoomPostID is 0. Running inside the
// reply's own transaction, the reply and its room post commit together.
func (s *Store) mirrorReplyTx(ctx context.Context, tx *sql.Tx, rep envelope.Reply) error {
	if rep.NoMirror {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO room_posts (room_id, author, body, created_at, status, reply_to, reply_generation)
		SELECT p.room_id, ?, ?, ?, ?, q.id, ? FROM requests q
		JOIN room_posts p ON p.id = q.room_post_id
		JOIN room_members m ON m.room_id = p.room_id AND m.agent = ?
		WHERE q.id = ? AND q.room_post_id IS NOT NULL
		ON CONFLICT DO NOTHING`,
		rep.From, mirroredBody(rep), s.now().UnixMilli(), string(rep.Status), rep.Generation, rep.From, rep.RequestID)
	return err
}

// mirroredBody is the room text of a mirrored reply: its body, cut to
// MaxRoomPost with a pointer to the request, which keeps the whole reply
// and any files.
func mirroredBody(rep envelope.Reply) string {
	body := rep.Body
	note := ""
	if len(rep.Attachments) > 0 {
		note = fmt.Sprintf("\n[%d attachment(s) on request %s]", len(rep.Attachments), rep.RequestID)
	}
	if len(body)+len(note) > envelope.MaxRoomPost {
		note = fmt.Sprintf("\n[cut; the whole reply is on request %s]", rep.RequestID) + note
		cut := max(envelope.MaxRoomPost-len(note), 0)
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut]
	}
	return body + note
}

// MirroredPost returns the room post that mirrors generation gen of the
// reply to request id, and false when there is none (the request is not a
// mention, or its replier had left the room).
func (s *Store) MirroredPost(ctx context.Context, id string, gen int64) (envelope.RoomPost, bool, error) {
	var p envelope.RoomPost
	var created int64
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT p.id, r.name, p.author, p.body, p.created_at, p.status, p.reply_to FROM room_posts p JOIN rooms r ON r.id = p.room_id
		WHERE p.reply_to = ? AND p.reply_generation = ?`, id, gen).Scan(&p.ID, &p.Room, &p.Author, &p.Body, &created, &status, &p.ReplyTo)
	if errors.Is(err, sql.ErrNoRows) {
		return envelope.RoomPost{}, false, nil
	}
	if err != nil {
		return envelope.RoomPost{}, false, err
	}
	p.CreatedAt, p.Status = time.UnixMilli(created).UTC(), envelope.Status(status)
	return p, true, nil
}

// RecentRoomPosts returns up to limit of room's posts before the post with
// id before, newest first, without their mentions.
func (s *Store) RecentRoomPosts(ctx context.Context, room string, before int64, limit int) ([]envelope.RoomPost, error) {
	id, err := roomID(ctx, s.db, room)
	if err != nil {
		return nil, err
	}
	return s.roomPosts(ctx, room, `SELECT id, author, body, created_at, status, reply_to FROM room_posts WHERE room_id = ? AND id < ? ORDER BY id DESC LIMIT ?`, id, before, limit)
}

// RoomPosts returns up to limit of room's posts after the post with id
// after, oldest first, each with the asks its mentions became.
func (s *Store) RoomPosts(ctx context.Context, room string, after int64, limit int) ([]envelope.RoomPost, error) {
	id, err := roomID(ctx, s.db, room)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxRoomPage {
		limit = MaxRoomPage
	}
	posts, err := s.roomPosts(ctx, room, `SELECT id, author, body, created_at, status, reply_to FROM room_posts WHERE room_id = ? AND id > ? ORDER BY id LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	if err := s.addMentions(ctx, posts); err != nil {
		return nil, err
	}
	return fitPage(posts, MaxRoomPageBytes)
}

// fitPage keeps the leading posts whose JSON fits in budget bytes, and
// always the first.
func fitPage(posts []envelope.RoomPost, budget int) ([]envelope.RoomPost, error) {
	size := 0
	for i, p := range posts {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		size += len(b) + 1 // the comma between posts
		if i > 0 && size > budget {
			return posts[:i], nil
		}
	}
	return posts, nil
}

// MaxRoomIDs bounds how many posts one read by id returns.
const MaxRoomIDs = 50

// RoomPostsByID returns the posts of room among ids, oldest first, each
// with the asks its mentions became; ids of other rooms' posts, or of no
// post, are left out. At most MaxRoomIDs ids are looked at.
func (s *Store) RoomPostsByID(ctx context.Context, room string, ids []int64) ([]envelope.RoomPost, error) {
	id, err := roomID(ctx, s.db, room)
	if err != nil {
		return nil, err
	}
	ids = ids[:min(len(ids), MaxRoomIDs)]
	if len(ids) == 0 {
		return []envelope.RoomPost{}, nil
	}
	args := []any{id}
	for _, p := range ids {
		args = append(args, p)
	}
	posts, err := s.roomPosts(ctx, room, `SELECT id, author, body, created_at, status, reply_to FROM room_posts WHERE room_id = ? AND id IN (?`+strings.Repeat(", ?", len(ids)-1)+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	return posts, s.addMentions(ctx, posts)
}

func (s *Store) roomPosts(ctx context.Context, room, query string, args ...any) ([]envelope.RoomPost, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []envelope.RoomPost{}
	for rows.Next() {
		p := envelope.RoomPost{Room: room}
		var created int64
		var status string
		if err := rows.Scan(&p.ID, &p.Author, &p.Body, &created, &status, &p.ReplyTo); err != nil {
			return nil, err
		}
		p.CreatedAt, p.Status = time.UnixMilli(created).UTC(), envelope.Status(status)
		out = append(out, p)
	}
	return out, rows.Err()
}

// addMentions fills in each post's mentions: the asks built from it, in
// the order they were sent, with their current status.
func (s *Store) addMentions(ctx context.Context, posts []envelope.RoomPost) error {
	if len(posts) == 0 {
		return nil
	}
	at := make(map[int64]int, len(posts))
	args := make([]any, 0, len(posts))
	for i, p := range posts {
		at[p.ID] = i
		args = append(args, p.ID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT room_post_id, id, to_agent, status FROM requests WHERE room_post_id IN (?`+strings.Repeat(", ?", len(args)-1)+`) ORDER BY created_at, rowid`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var post int64
		var m envelope.RoomMention
		var status string
		if err := rows.Scan(&post, &m.RequestID, &m.Agent, &status); err != nil {
			return err
		}
		m.Status = envelope.Status(status)
		i := at[post]
		posts[i].Mentions = append(posts[i].Mentions, m)
	}
	return rows.Err()
}
