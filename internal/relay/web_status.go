package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mvanhorn/agent-tincan/internal/client"
	"io"
	"net/http"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

var webKindSites = map[string]string{
	"chatgpt-web": "chatgpt.com", "dot-web": "chatgpt.com", "claude-web": "claude.ai", "grok-web": "grok.com", "gemini-web": "gemini.google.com", "perplexity-web": "www.perplexity.ai", "copilot-web": "copilot.com",
}

func (s *Server) handleWebStatus(w http.ResponseWriter, r *http.Request) {
	name := s.agent(w, r)
	if name == "" {
		return
	}
	var in envelope.WebStatus
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := d.Decode(&in)
	if err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("expected one observation")
		}
	}
	validSite := false
	for _, site := range webKindSites {
		validSite = validSite || in.Site == site
	}
	now := s.cfg.Now()
	if err != nil || !validSite || (in.State != "signed_out" && in.State != "authenticated") || in.ObservedAt.Before(now.Add(-5*time.Minute)) || in.ObservedAt.After(now.Add(time.Minute)) {
		writeErr(w, http.StatusBadRequest, errors.New("invalid web status observation"))
		return
	}
	a, ok, err := s.dir.Agent(r.Context(), name)
	if err != nil || !ok {
		writeErr(w, http.StatusForbidden, errors.New("unknown agent"))
		return
	}
	if a.Kind != "" && webKindSites[a.Kind] != in.Site {
		writeErr(w, http.StatusBadRequest, errors.New("site conflicts with agent kind"))
		return
	}
	if err := s.store.SetWebStatus(r.Context(), a, in, now); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.notifyWebStatus(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) notifyWebStatus(ctx context.Context) {
	notifier, ok := s.preparer().(interface{ NotifyDestination() string })
	if !ok {
		return
	}
	to := notifier.NotifyDestination()
	if to == "" {
		return
	}
	facts, err := s.store.WebStatuses(ctx)
	if err != nil {
		return
	}
	for name, status := range facts {
		if !status.Pending || status.Since.IsZero() || s.cfg.Now().Before(status.RetryAt) {
			continue
		}
		a, ok, err := s.dir.Agent(ctx, name)
		if err != nil || !ok {
			continue
		}
		var target envelope.Target
		status.Apply(a, &target)
		if target.SignedOutSite == "" {
			continue
		}
		body := fmt.Sprintf("Browser authentication notice since %s. %s", status.Since.UTC().Format(time.RFC3339), client.SignedOutHint(name, &target))
		req, err := s.store.EnqueueWebNotice(ctx, name, status, to, body, s.requestTTL(ctx, to))
		if err != nil {
			_ = s.store.RetryWebNotice(ctx, name, status, s.cfg.Now().Add(time.Minute))
			s.record(ctx, "web_status_notify_failed", "", "", "relay", "")
			continue
		}
		if req.ID == "" {
			continue
		}
		s.hub.notify(inboxKey(to))
		s.record(ctx, "web_status_notified", req.ID, req.TraceID, "relay", "")
		if s.events != nil {
			s.events.Queued(ctx, req)
		}
	}
}
