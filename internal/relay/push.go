package relay

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"golang.org/x/sync/errgroup"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// PushSubscriber is the contact the relay names to push services in its
// VAPID token. The library adds mailto:.
const PushSubscriber = "relay@agenttincan.com"

// PushKeyFile is the relay's VAPID private key in its state dir: 32 bytes,
// 0600, created on first run like invite-pepper. It signs every push.
const PushKeyFile = "push-vapid-key"

// pushTimeout bounds one push to one subscription.
const pushTimeout = 10 * time.Second

// pushTTL is how long a push service keeps an undelivered alert.
const pushTTL = 3600

// pushParallel bounds how many subscriptions one round sends to at once.
const pushParallel = 4

// pushHosts are the push services the relay sends to: Apple, Google,
// Mozilla, and (by suffix) Microsoft.
var pushHosts = map[string]bool{"web.push.apple.com": true, "fcm.googleapis.com": true, "updates.push.services.mozilla.com": true}

const pushHostSuffix = ".notify.windows.com"

// Pusher sends content-free Web Push alerts for held asks to every
// subscribed admin phone (KTD18). Alerts go out from one background
// worker, so a slow push service never stalls a send; a held ask while a
// round is pending joins that round, whose alert carries the current
// count.
type Pusher struct {
	pub, priv string // VAPID keys, base64url
	client    *http.Client
	timeout   time.Duration
	allow     func(*url.URL) bool
	kick      chan struct{}
}

// NewPusher builds a pusher from the 32-byte VAPID private key.
func NewPusher(key []byte) (*Pusher, error) {
	k, err := ecdh.P256().NewPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("push key: %w (delete %s to make a new one; subscribed phones then subscribe again)", err, PushKeyFile)
	}
	return &Pusher{
		pub:     base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		priv:    base64.RawURLEncoding.EncodeToString(key),
		client:  newPushClient(refusePushAddr),
		timeout: pushTimeout,
		allow:   pushEndpointAllowed,
		kick:    make(chan struct{}, 1),
	}, nil
}

// SetPusher turns on push alerts for held asks and starts the worker,
// which stops when the relay does.
func (s *Server) SetPusher(p *Pusher) {
	s.push = p
	go s.pushLoop(p)
}

// pushEndpointAllowed reports whether u is an https endpoint on one of
// pushHosts, on the default port, with no user info.
func pushEndpointAllowed(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return pushHosts[h] || (strings.HasSuffix(h, pushHostSuffix) && len(h) > len(pushHostSuffix))
}

// tailnetRange is Tailscale's CGNAT range, never a push service.
var tailnetRange = netip.MustParsePrefix("100.64.0.0/10")

// errPushAddr wraps every refusal from refusePushAddr, so a failed send
// can be audited as one without reading the error's text.
var errPushAddr = errors.New("push: refusing to connect to a non-public address")

// refusePushAddr refuses any address that is not a public unicast one:
// private, loopback, link-local, tailnet, unspecified or multicast.
func refusePushAddr(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || tailnetRange.Contains(ip) {
		return fmt.Errorf("%w %s", errPushAddr, ip)
	}
	return nil
}

// newPushClient is the push sender's HTTP client: it checks every address
// it dials after resolution, uses no proxy, follows no redirect, and gives
// up after pushTimeout.
func newPushClient(check func(netip.Addr) error) *http.Client {
	d := &net.Dialer{Timeout: pushTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		return check(ap.Addr())
	}}
	return &http.Client{
		Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: pushTimeout, ResponseHeaderTimeout: pushTimeout, ForceAttemptHTTP2: true, MaxIdleConns: 4, IdleConnTimeout: time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: pushTimeout,
	}
}

// queuePush asks the worker for a round of alerts; it never blocks.
func (s *Server) queuePush() {
	if s.push == nil {
		return
	}
	select {
	case s.push.kick <- struct{}{}:
	default: // a round is already waiting and will cover this ask
	}
}

func (s *Server) pushLoop(p *Pusher) {
	for {
		select {
		case <-s.stopping:
			return
		case <-p.kick:
			s.pushRound(p)
		}
	}
}

// pushRound alerts every subscription once, a few at a time. Nothing it
// does changes a held request: failures are audited and the next held ask
// tries again. The alert carries only the held count, the one field sw.js
// reads; it never names a held ask.
func (s *Server) pushRound(p *Pusher) {
	ctx := context.Background()
	subs, err := s.store.PushSubscriptions(ctx)
	if err != nil {
		log.Printf("push: list subscriptions: %v", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	held, err := s.store.CountHeld(ctx)
	if err != nil {
		log.Printf("push: count held requests: %v", err)
	}
	payload, _ := json.Marshal(map[string]int{"count": max(held, 1)})
	var g errgroup.Group
	g.SetLimit(pushParallel)
	for _, sub := range subs {
		g.Go(func() error {
			// webpush pads the payload in place, so each send gets its own.
			s.pushOne(ctx, p, sub, bytes.Clone(payload))
			return nil
		})
	}
	_ = g.Wait() // pushOne audits its own failures and returns none
}

func (s *Server) pushOne(ctx context.Context, p *Pusher, sub store.PushSubscription, payload []byte) {
	u, err := url.Parse(sub.Endpoint)
	if err != nil || !p.allow(u) {
		// Checked when stored; a row that no longer passes is never sent to.
		s.record(ctx, "push_sent", "", "", "relay", store.DetailJSON(map[string]any{"subscription": sub.ID, "error": "endpoint not allowed"}))
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	resp, err := webpush.SendNotificationWithContext(sendCtx, payload, &webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{Auth: sub.Auth, P256dh: sub.P256dh}},
		&webpush.Options{HTTPClient: p.client, Subscriber: PushSubscriber, TTL: pushTTL, Urgency: webpush.UrgencyHigh, Topic: "tincan-held", VAPIDPublicKey: p.pub, VAPIDPrivateKey: p.priv})
	if err != nil {
		// The error can name the endpoint, a secret; record only its kind.
		kind := "send failed"
		var ne net.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
			kind = "timeout"
		case errors.Is(err, errPushAddr):
			kind = "address refused"
		}
		s.record(ctx, "push_sent", "", "", "relay", store.DetailJSON(map[string]any{"subscription": sub.ID, "error": kind}))
		return
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		if _, err := s.store.DeletePushSubscriptionID(ctx, sub.ID); err != nil {
			log.Printf("push: drop subscription %s: %v", sub.ID, err)
		}
		s.record(ctx, "push_unsubscribed", "", "", "relay", store.DetailJSON(map[string]any{"subscription": sub.ID, "reason": "gone", "status": resp.StatusCode}))
		return
	}
	s.record(ctx, "push_sent", "", "", "relay", store.DetailJSON(map[string]any{"subscription": sub.ID, "status": resp.StatusCode}))
}

// errPushOff answers the push routes on a relay without push.
var errPushOff = errors.New("push alerts are off on this relay")

// handlePushKey returns the VAPID public key a page subscribes with.
func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	if s.push == nil {
		writeErr(w, http.StatusNotFound, errPushOff)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": s.push.pub})
}

// pushSubscriptionIn is a browser's PushSubscription as JSON.
type pushSubscriptionIn struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// decodeKey reads a base64url (or standard) browser key of n bytes.
func decodeKey(s string, n int) bool {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return len(b) == n
		}
	}
	return false
}

// handlePushSubscribe stores the calling admin device's subscription, or
// refreshes it: the page sends it on every open.
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	if s.push == nil {
		writeErr(w, http.StatusNotFound, errPushOff)
		return
	}
	var in pushSubscriptionIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	u, err := url.Parse(in.Endpoint)
	if len(in.Endpoint) > 2048 || err != nil || !s.push.allow(u) {
		writeErr(w, http.StatusBadRequest, errors.New("the endpoint must be https on Apple, Google, Mozilla or Microsoft's push service"))
		return
	}
	if p, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(in.Keys.P256dh, "=")); err != nil || len(p) != 65 || p[0] != 4 || !decodeKey(in.Keys.Auth, 16) {
		writeErr(w, http.StatusBadRequest, errors.New("keys.p256dh must be an uncompressed P-256 key and keys.auth 16 bytes, base64url"))
		return
	}
	n := s.callerNode(r)
	sub, created, err := s.store.UpsertPushSubscription(r.Context(), store.PushSubscription{Endpoint: in.Endpoint, P256dh: in.Keys.P256dh, Auth: in.Keys.Auth, Device: n.Name, Node: n.ID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if created {
		s.record(r.Context(), "push_subscribed", "", "", s.remote(r), store.DetailJSON(map[string]any{"subscription": sub.ID, "device": n.Name}))
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": sub.ID})
}

// handlePushUnsubscribe deletes a subscription by its endpoint.
func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, identity.ErrNotAdmin)
		return
	}
	var in struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Endpoint == "" {
		writeErr(w, http.StatusBadRequest, errors.New("endpoint is required"))
		return
	}
	id, ok, err := s.store.DeletePushSubscription(r.Context(), in.Endpoint)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if ok {
		s.record(r.Context(), "push_unsubscribed", "", "", s.remote(r), store.DetailJSON(map[string]any{"subscription": id, "device": s.callerNode(r).Name}))
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": ok})
}
