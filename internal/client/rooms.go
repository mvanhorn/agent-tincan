package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// ErrRoomsUnsupported identifies a relay that predates rooms.
var ErrRoomsUnsupported = errors.New("relay does not support rooms; upgrade the relay")

// Rawer makes one JSON call to the relay. *Relay implements it, and so does
// every MCP backend, so the room calls below serve the CLI, tincan mcp and
// the gateway alike.
type Rawer interface {
	Raw(ctx context.Context, method, path string, in, out any) error
}

// RoomMemberResult is the relay's answer to adding or removing a room
// member. Changed is false when the agent already was (or was not) a
// member. Warning is set when the member sends the thread to an outside
// vendor.
type RoomMemberResult struct {
	Room    string `json:"room"`
	Agent   string `json:"agent"`
	Changed bool   `json:"changed"`
	Warning string `json:"warning,omitempty"`
}

// roomCall makes one room call. A relay that predates rooms answers it 404
// or 405, so only such a failure asks the relay's capabilities, and a relay
// that does not advertise rooms fails with ErrRoomsUnsupported. Any other
// 404, such as the relay's ErrNoRoom, comes back unchanged. A *Relay
// remembers that its relay serves rooms, so it asks at most once.
func roomCall(ctx context.Context, c Rawer, method, path string, in, out any) error {
	err := c.Raw(ctx, method, path, in, out)
	if !IsStatus(err, http.StatusNotFound) && !IsStatus(err, http.StatusMethodNotAllowed) {
		return err
	}
	r, cached := c.(*Relay)
	if cached && r.roomsOK.Load() {
		return err
	}
	caps, cerr := capabilities(ctx, c)
	if cerr != nil {
		return fmt.Errorf("check relay capabilities: %w", cerr)
	}
	if !caps.Rooms {
		return ErrRoomsUnsupported
	}
	if cached {
		r.roomsOK.Store(true)
	}
	return err
}

func roomPath(room string) string { return "/v1/rooms/" + url.PathEscape(room) }

// CreateRoom creates an empty room (admin devices only).
func CreateRoom(ctx context.Context, c Rawer, name string) (envelope.Room, error) {
	var out envelope.Room
	err := roomCall(ctx, c, "POST", "/v1/admin/rooms", map[string]string{"name": name}, &out)
	return out, err
}

// AddRoomMember adds agent to room (admin devices only).
func AddRoomMember(ctx context.Context, c Rawer, room, agent string) (RoomMemberResult, error) {
	return roomMember(ctx, c, "POST", room, agent)
}

// RemoveRoomMember takes agent out of room (admin devices only).
func RemoveRoomMember(ctx context.Context, c Rawer, room, agent string) (RoomMemberResult, error) {
	return roomMember(ctx, c, "DELETE", room, agent)
}

func roomMember(ctx context.Context, c Rawer, method, room, agent string) (RoomMemberResult, error) {
	var out RoomMemberResult
	err := roomCall(ctx, c, method, "/v1/admin/rooms/"+url.PathEscape(room)+"/members", map[string]string{"agent": agent}, &out)
	return out, err
}

// Rooms lists the rooms this agent is in, or with all every room (admin
// devices only).
func Rooms(ctx context.Context, c Rawer, all bool) ([]envelope.Room, error) {
	path := "/v1/rooms"
	if all {
		path = "/v1/admin/rooms"
	}
	var out struct {
		Rooms []envelope.Room `json:"rooms"`
	}
	err := roomCall(ctx, c, "GET", path, nil, &out)
	return out.Rooms, err
}

// PostToRoom posts body to room as this agent.
func PostToRoom(ctx context.Context, c Rawer, room, body string) (envelope.RoomPostResponse, error) {
	var out envelope.RoomPostResponse
	err := roomCall(ctx, c, "POST", roomPath(room)+"/posts", map[string]string{"body": body}, &out)
	return out, err
}

// ReadRoom returns up to limit of room's posts after the post with id
// after, oldest first; limit 0 takes the relay's page size.
func ReadRoom(ctx context.Context, c Rawer, room string, after int64, limit int) (envelope.RoomThread, error) {
	var out envelope.RoomThread
	q := url.Values{}
	if after > 0 {
		q.Set("after", fmt.Sprint(after))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	path := roomPath(room) + "/posts"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	err := roomCall(ctx, c, "GET", path, nil, &out)
	return out, err
}
