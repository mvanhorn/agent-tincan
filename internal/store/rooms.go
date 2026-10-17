package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

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
// was not a member.
func (s *Store) RemoveRoomMember(ctx context.Context, room, agent string) (bool, error) {
	id, err := roomID(ctx, s.db, room)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM room_members WHERE room_id = ? AND agent = ?`, id, agent)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// IsRoomMember reports whether agent is a member of room, or ErrNoRoom.
func (s *Store) IsRoomMember(ctx context.Context, room, agent string) (bool, error) {
	id, err := roomID(ctx, s.db, room)
	if err != nil {
		return false, err
	}
	var ok bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM room_members WHERE room_id = ? AND agent = ?)`, id, agent).Scan(&ok)
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
		rm.Members = strings.Fields(members)
		if rm.Members == nil {
			rm.Members = []string{}
		}
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

// RoomPosts returns up to limit of room's posts after the post with id
// after, oldest first.
func (s *Store) RoomPosts(ctx context.Context, room string, after int64, limit int) ([]envelope.RoomPost, error) {
	id, err := roomID(ctx, s.db, room)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxRoomPage {
		limit = MaxRoomPage
	}
	return s.roomPosts(ctx, room, `SELECT id, author, body, created_at, status, reply_to FROM room_posts WHERE room_id = ? AND id > ? ORDER BY id LIMIT ?`, id, after, limit)
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
