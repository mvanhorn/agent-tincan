package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// Errors a SelfUpgrader returns, wrapped, to pick the reply's status.
var (
	// ErrUpgradeNotNewer: the release is not newer than the running build
	// and the admin did not force it (409).
	ErrUpgradeNotNewer = errors.New("the release is not newer than the running relay")
	// ErrUpgradePending: an upgrade already succeeded and the relay is
	// restarting (409).
	ErrUpgradePending = errors.New("the relay was already upgraded and is restarting")
	// ErrUpgradeNotWritable: the relay user cannot replace its own binary
	// (409).
	ErrUpgradeNotWritable = errors.New("the relay cannot replace its own binary")
	// ErrUpgradeUnavailable: there is no usable release for the relay's
	// platform: no VERSION, no binary, or no checksums.txt entry (422).
	ErrUpgradeUnavailable = errors.New("no installable release for this relay")
	// ErrUpgradeChecksum: a binary does not match its checksums.txt (422).
	ErrUpgradeChecksum = errors.New("checksum mismatch")
	// ErrUpgradeFetch: the release download failed (502).
	ErrUpgradeFetch = errors.New("release download failed")
)

// SelfUpgrader installs a release over the running relay binary. Upgrade
// leaves the binary untouched on any error. Restart is called once the
// reply has been sent; it must not block.
type SelfUpgrader interface {
	Upgrade(ctx context.Context, dist string, in client.RelayUpgradeRequest) (client.RelayUpgradeResult, error)
	Restart()
}

// selfUpgradeTimeout bounds one self-upgrade, which may download a release.
const selfUpgradeTimeout = distDownloadTimeout

// SetSelfUpgrader enables POST /v1/admin/relay/upgrade. It needs a dist
// directory (SetDist) too.
func (s *Server) SetSelfUpgrader(u SelfUpgrader) { s.upgrader = u }

// IsDistBinary reports whether name is a release binary name the dist
// serves: tincan_<linux|darwin>_<amd64|arm64> or
// tincan_windows_<amd64|arm64>.exe.
func IsDistBinary(name string) bool { return distBinary.MatchString(name) }

// handleSelfUpgrade installs a release over the relay's own binary (admin
// only), writes and flushes the reply with the old and new versions, and
// only then asks for the restart.
func (s *Server) handleSelfUpgrade(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	var in client.RelayUpgradeRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.dist == nil {
		writeErr(w, http.StatusNotFound, errors.New("this relay has no dist directory to upgrade from (start it with --dist <dir>)"))
		return
	}
	if s.upgrader == nil {
		writeErr(w, http.StatusNotFound, errors.New("this relay cannot upgrade itself"))
		return
	}
	// A download can outlast the API write timeout, and a caller that
	// hangs up must not cut an install short.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(selfUpgradeTimeout + time.Minute))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), selfUpgradeTimeout)
	defer cancel()
	res, err := s.upgrader.Upgrade(ctx, s.dist.dir, in)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrUpgradeNotNewer), errors.Is(err, ErrUpgradePending), errors.Is(err, ErrUpgradeNotWritable):
			code = http.StatusConflict
		case errors.Is(err, ErrUpgradeUnavailable), errors.Is(err, ErrUpgradeChecksum):
			code = http.StatusUnprocessableEntity
		case errors.Is(err, ErrUpgradeFetch):
			code = http.StatusBadGateway
		}
		writeErr(w, code, err)
		return
	}
	source := "dist"
	if in.FromGitHub != "" {
		source = "github"
	}
	s.record(ctx, "relay_upgraded", "", "", s.remote(r), store.DetailJSON(map[string]any{"from": res.From, "to": res.To, "source": source}))
	log.Printf("relay upgraded from %s to %s; restarting (%s)", res.From, res.To, res.Restart)
	// A complete, sized reply is on the wire before the restart begins, so
	// the admin sees the result even though the relay then goes away.
	body, _ := json.Marshal(res)
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		log.Printf("write upgrade reply: %v", err)
	}
	_ = http.NewResponseController(w).Flush()
	s.upgrader.Restart()
}
