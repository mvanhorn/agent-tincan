package relay

import (
	"context"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// WebSendBudget is the budget a web member's mention ask is packed to.
const WebSendBudget = webSendBudget

// JoinNoncesLive counts the live decision nonces for join request id.
func (s *Server) JoinNoncesLive(id string) int {
	s.nonces.mu.Lock()
	defer s.nonces.mu.Unlock()
	n := 0
	for _, hs := range s.nonces.byID[joinNonceID(id)] {
		n += len(hs)
	}
	return n
}

// KeyForTest exposes the relay key to tests.
func KeyForTest(s *Server) string { return s.key }

// LoosenPusherForTest lets p send to plain-HTTP push services on loopback
// (test servers) and gives each push timeout. Redirects are still not
// followed.
func LoosenPusherForTest(p *Pusher, timeout time.Duration) {
	p.client = newPushClient(func(netip.Addr) error { return nil })
	p.timeout = timeout
	p.allow = func(u *url.URL) bool { return u.Scheme == "http" && u.Hostname() == "127.0.0.1" }
}

// PushAddrRefused reports whether the push sender refuses to dial ip.
func PushAddrRefused(ip string) bool {
	return refusePushAddr(netip.MustParseAddr(ip)) != nil
}

// DialPushForTest makes one request to hostPort with the real push
// client.
func DialPushForTest(ctx context.Context, hostPort string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+hostPort+"/", nil)
	if err != nil {
		return err
	}
	resp, err := newPushClient(refusePushAddr).Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// Routes lists the patterns Handler serves, and GuestAllowed the ones it
// serves to a guest.
func (s *Server) Routes() []string { return s.routes().patterns }

var GuestAllowed = guestRoutes

// TaskAssign is taskAssign, for tests outside the package.
type TaskAssign = taskAssign

// AssignTask, ReassignTask and DropTask let tests drive the task paths
// that the task routes call.
func (s *Server) AssignTask(ctx context.Context, a TaskAssign) (envelope.Request, error) {
	return s.assignTask(ctx, a)
}

func (s *Server) ReassignTask(ctx context.Context, a TaskAssign) (envelope.Request, error) {
	return s.reassignTask(ctx, a)
}

func (s *Server) DropTask(ctx context.Context, id int64, actor, note string) (envelope.Task, error) {
	return s.dropTask(ctx, id, actor, note)
}

// WriteTaskNowAs answers w as a task route answers agent after it changed
// task id, so a test can see what such an answer hands a task host for a
// task no route lets it change.
func (s *Server) WriteTaskNowAs(w http.ResponseWriter, r *http.Request, agent string, id int64) error {
	view, err := s.hostTaskView(r.Context(), agent)
	if err != nil {
		return err
	}
	s.writeTaskNow(w, r, view, id, http.StatusOK, nil)
	return nil
}
