package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// RoomPolicy is the part of the installed Preparer that rooms use: a post
// charges the poster's send rate, and an agent approval.json can hold asks
// to is kept out of rooms. Without it posts are not rate-limited and only
// the kinds held by default are kept out.
type RoomPolicy interface {
	Charge(sender string) error
	Gated(target string) (bool, error)
}

// roomNameRE is what a room name may look like: the same shape as an agent
// name.
var roomNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// roomHeldKinds are the kinds whose asks the relay holds for the owner by
// default; roomServiceKinds are Go services whose answers come from the
// owner's private data. Neither may be in a room: a room would hand them
// unapproved text, or hand what they retrieve to every member.
var (
	roomHeldKinds    = map[string]bool{onboard.KindCouncil: true, onboard.KindDotWeb: true}
	roomServiceKinds = map[string]bool{onboard.KindHistory: true, onboard.KindNotes: true, "pass": true}
)

// roomVendorKinds are the members that send the thread on to an outside
// vendor: the web chat agents type it into the owner's account, and the
// gateway agents (ChatGPT, Sesame) read it through the vendor's own app.
func roomVendorKind(kind string) bool {
	_, web := webKindSites[kind]
	return web || kind == onboard.KindChatGPT || kind == onboard.KindSesame
}

// roomRefusal says why agent a may not be in a room, or "" when it may.
// It is checked when a member is added and again on every read and post,
// so an agent that approval.json starts holding is shut out at once.
func (s *Server) roomRefusal(a identity.Agent) (string, error) {
	switch {
	case roomHeldKinds[a.Kind]:
		return "asks to a " + a.Kind + " agent are held for the owner's approval", nil
	case roomServiceKinds[a.Kind]:
		return a.Kind + " is a service agent", nil
	}
	rp, ok := s.preparer().(RoomPolicy)
	if !ok {
		return "", nil
	}
	gated, err := rp.Gated(a.Name)
	if gated {
		return "approval.json holds asks to " + a.Name, err
	}
	return "", err
}

// roomAgent checks that agent may use room now: it is eligible (see
// roomRefusal) and a member. It writes the error and returns false
// otherwise.
func (s *Server) roomAgent(w http.ResponseWriter, ctx context.Context, room, agent string) bool {
	a, ok, err := s.dir.Agent(ctx, agent)
	if err != nil || !ok {
		writeErr(w, http.StatusForbidden, errors.New("unknown agent"))
		return false
	}
	reason, err := s.roomRefusal(a)
	if reason != "" || err != nil {
		if reason == "" {
			reason = err.Error()
		}
		writeErr(w, http.StatusForbidden, errors.New("not allowed in rooms: "+reason))
		return false
	}
	if room == "" {
		return true
	}
	member, err := s.store.IsRoomMember(ctx, room, agent)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return false
	}
	if !member {
		writeErr(w, http.StatusForbidden, store.ErrNotRoomMember)
		return false
	}
	return true
}

// handleRooms lists the rooms the calling agent is in.
func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" || !s.roomAgent(w, r.Context(), "", name) {
		return
	}
	rooms, err := s.store.Rooms(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}

// handleRoomRead returns a page of a room's posts after the after cursor
// (a post id), oldest first, to a member.
func (s *Server) handleRoomRead(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	room := r.PathValue("room")
	if name == "" || !s.roomAgent(w, r.Context(), room, name) {
		return
	}
	after, err := cursorParam(r, "after")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	limit, err := cursorParam(r, "limit")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rm, err := s.store.Room(r.Context(), room)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	posts, err := s.store.RoomPosts(r.Context(), room, after, int(limit))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	next := after
	if len(posts) > 0 {
		next = posts[len(posts)-1].ID
	}
	writeJSON(w, http.StatusOK, envelope.RoomThread{Room: room, Members: rm.Members, Posts: posts, Next: next})
}

// cursorParam reads a non-negative integer query parameter, 0 when absent.
func cursorParam(r *http.Request, key string) (int64, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad %s", key)
	}
	return n, nil
}

// handleRoomPost stores the calling member's post. It takes a slot of the
// poster's send rate first, so a busy room cannot outrun the limit sends
// have. The audit row records ids and the byte count, never the text.
func (s *Server) handleRoomPost(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	room := r.PathValue("room")
	if name == "" || !s.roomAgent(w, r.Context(), room, name) {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := decodeOne(r.Body, &in, "post object"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	switch {
	case strings.TrimSpace(in.Body) == "":
		writeErr(w, http.StatusBadRequest, errors.New("empty post"))
		return
	case len(in.Body) > envelope.MaxRoomPost:
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("post is %d bytes, over the %d byte limit", len(in.Body), envelope.MaxRoomPost))
		return
	case !utf8.ValidString(in.Body):
		writeErr(w, http.StatusBadRequest, errors.New("post is not UTF-8"))
		return
	}
	if rp, ok := s.preparer().(RoomPolicy); ok {
		if err := rp.Charge(name); err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
	}
	post, err := s.store.AddRoomPost(r.Context(), room, name, in.Body)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "room_posted", "", "", name, store.DetailJSON(map[string]any{"room": room, "post": post.ID, "bytes": len(in.Body)}))
	writeJSON(w, http.StatusCreated, envelope.RoomPostResponse{Post: post})
}

// handleCreateRoom creates an empty room (admin only).
func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeOne(r.Body, &in, "room object"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !roomNameRE.MatchString(in.Name) {
		writeErr(w, http.StatusBadRequest, errors.New("room names are 1 to 32 lowercase letters, digits or dashes"))
		return
	}
	rm, err := s.store.CreateRoom(r.Context(), in.Name)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.record(r.Context(), "room_created", "", "", s.remote(r), store.DetailJSON(map[string]any{"room": in.Name}))
	writeJSON(w, http.StatusCreated, rm)
}

// handleAdminRooms lists every room with its members (admin only).
func (s *Server) handleAdminRooms(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	rooms, err := s.store.Rooms(r.Context(), "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}

// handleRoomMember adds (POST) or removes (DELETE) a room member (admin
// only). An agent rooms exclude is refused with the reason; a member that
// sends the thread to its vendor is added with a warning saying so.
func (s *Server) handleRoomMember(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	var in struct {
		Agent string `json:"agent"`
	}
	if err := decodeOne(r.Body, &in, "member object"); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	room := r.PathValue("room")
	out := client.RoomMemberResult{Room: room, Agent: in.Agent}
	if r.Method == http.MethodDelete {
		removed, err := s.store.RemoveRoomMember(r.Context(), room, in.Agent)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		if removed {
			s.record(r.Context(), "room_member_removed", "", "", s.remote(r), store.DetailJSON(map[string]any{"room": room, "agent": in.Agent}))
		}
		out.Changed = removed
		writeJSON(w, http.StatusOK, out)
		return
	}
	a, ok, err := s.dir.Agent(r.Context(), in.Agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%s: %w", in.Agent, identity.ErrUnknownAgent))
		return
	}
	reason, err := s.roomRefusal(a)
	if reason != "" || err != nil {
		if reason == "" {
			reason = err.Error()
		}
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%s cannot be in a room: %s", in.Agent, reason))
		return
	}
	added, err := s.store.AddRoomMember(r.Context(), room, in.Agent)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if added {
		s.record(r.Context(), "room_member_added", "", "", s.remote(r), store.DetailJSON(map[string]any{"room": room, "agent": in.Agent}))
	}
	out.Changed = added
	if roomVendorKind(a.Kind) {
		out.Warning = fmt.Sprintf("%s is a %s agent: the room's posts it reads or is sent go to that vendor, under the owner's account there", in.Agent, a.Kind)
	}
	writeJSON(w, http.StatusOK, out)
}
