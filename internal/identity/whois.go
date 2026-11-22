package identity

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
)

// Node is the tailnet machine a connection came from.
type Node struct {
	ID   string // Tailscale stable node id; the key agents are bound to
	Name string // short machine name, for display and the admin list
	User string // owning login
	Tags []string
}

// Resolver answers which tailnet node a connection came from.
type Resolver interface {
	WhoIs(ctx context.Context, remoteAddr string) (Node, error)
}

// Peer is one tailnet node as the tailnet sees it now.
type Peer struct {
	Node
	// Online reports whether the node is connected to the control plane.
	Online bool
	// Self marks the relay's own node.
	Self bool
}

// PeerLister is an optional Resolver extension that lists every node on
// the tailnet as the tailnet sees it now, this node first.
type PeerLister interface {
	Peers(ctx context.Context) ([]Peer, error)
}

// LocalResolver asks a tailscaled LocalAPI. The relay uses the tsnet
// server's LocalClient in tsnet mode and the host tailscaled in --listen mode.
type LocalResolver struct {
	lc *local.Client
}

// NewLocalResolver wraps an existing LocalAPI client (for example
// tsnet.Server.LocalClient()).
func NewLocalResolver(lc *local.Client) *LocalResolver {
	return &LocalResolver{lc: lc}
}

// NewLocalResolverAt talks to the host tailscaled at socket. An empty socket
// means the platform default.
func NewLocalResolverAt(socket string) *LocalResolver {
	lc := &local.Client{}
	if socket != "" {
		lc.Socket = socket
		lc.UseSocketOnly = true
	}
	return &LocalResolver{lc: lc}
}

// WhoIs implements Resolver.
func (r *LocalResolver) WhoIs(ctx context.Context, remoteAddr string) (Node, error) {
	res, err := r.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Node{}, fmt.Errorf("whois %s: %w", remoteAddr, err)
	}
	if res.Node == nil {
		return Node{}, fmt.Errorf("whois %s: no node", remoteAddr)
	}
	n := Node{ID: string(res.Node.StableID), Name: shortName(res.Node.Name), Tags: res.Node.Tags}
	if res.UserProfile != nil {
		n.User = res.UserProfile.LoginName
	}
	return n, nil
}

// NodeOnline implements NodeStatus from the tailnet peer list: found when a
// peer (or this node) has the stable id, online when it is connected to the
// control plane.
func (r *LocalResolver) NodeOnline(ctx context.Context, stableID string) (online, found bool, err error) {
	st, err := r.lc.Status(ctx)
	if err != nil {
		return false, false, fmt.Errorf("tailnet status: %w", err)
	}
	if st.Self != nil && string(st.Self.ID) == stableID {
		return st.Self.Online, true, nil
	}
	for _, p := range st.Peer {
		if string(p.ID) == stableID {
			return p.Online, true, nil
		}
	}
	return false, false, nil
}

// Peers implements PeerLister from the tailnet peer list, this node
// first, then the rest by machine name.
func (r *LocalResolver) Peers(ctx context.Context) ([]Peer, error) {
	st, err := r.lc.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailnet status: %w", err)
	}
	var out []Peer
	if st.Self != nil {
		out = append(out, peerStatus(st, st.Self, true))
	}
	for _, p := range st.Peer {
		out = append(out, peerStatus(st, p, false))
	}
	if len(out) > 1 {
		slices.SortFunc(out[1:], func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	}
	return out, nil
}

func peerStatus(st *ipnstate.Status, p *ipnstate.PeerStatus, self bool) Peer {
	peer := Peer{Online: p.Online, Self: self}
	if p == nil {
		return peer
	}
	peer.ID, peer.Name = string(p.ID), shortName(p.DNSName)
	if p.Tags != nil {
		peer.Tags = p.Tags.AsSlice()
	}
	if u, ok := st.User[p.UserID]; ok {
		peer.User = u.LoginName
	}
	return peer
}

// LookupNode implements NodeLookup from the tailnet peer list.
func (r *LocalResolver) LookupNode(ctx context.Context, stableID string) (Node, bool, error) {
	st, err := r.lc.Status(ctx)
	if err != nil {
		return Node{}, false, fmt.Errorf("tailnet status: %w", err)
	}
	peers := []*ipnstate.PeerStatus{st.Self}
	for _, p := range st.Peer {
		peers = append(peers, p)
	}
	for _, p := range peers {
		if p == nil || string(p.ID) != stableID {
			continue
		}
		n := Node{ID: stableID, Name: shortName(p.DNSName)}
		if p.Tags != nil {
			n.Tags = p.Tags.AsSlice()
		}
		if u, ok := st.User[p.UserID]; ok {
			n.User = u.LoginName
		}
		return n, true, nil
	}
	return Node{}, false, nil
}

// Probe fails when the LocalAPI is unreachable. --listen mode calls it at
// startup and refuses to run rather than skip attribution.
func (r *LocalResolver) Probe(ctx context.Context) error {
	if _, err := r.lc.StatusWithoutPeers(ctx); err != nil {
		return fmt.Errorf("tailscaled LocalAPI unreachable: %w", err)
	}
	return nil
}

// shortName turns "grok-bot.tail2b6977.ts.net." into "grok-bot".
func shortName(fqdn string) string {
	name, _, _ := strings.Cut(strings.TrimSuffix(fqdn, "."), ".")
	return name
}

var (
	_ NodeStatus = (*LocalResolver)(nil)
	_ NodeLookup = (*LocalResolver)(nil)
)

// SelfURLs are the relay URLs this tailnet node answers on for port: its
// MagicDNS name first (it outlives an IP change), then its IPv4 address.
func (r *LocalResolver) SelfURLs(ctx context.Context, port int) []string {
	st, err := r.lc.StatusWithoutPeers(ctx)
	if err != nil || st.Self == nil {
		return nil
	}
	hostPort := func(h string) string {
		if port == 80 {
			return "http://" + h
		}
		return "http://" + net.JoinHostPort(h, strconv.Itoa(port))
	}
	var out []string
	if name := strings.TrimSuffix(st.Self.DNSName, "."); name != "" {
		out = append(out, hostPort(name))
	}
	for _, ip := range st.Self.TailscaleIPs {
		if ip.Is4() {
			out = append(out, hostPort(ip.String()))
			break
		}
	}
	return out
}

// SelfNames are the names this tailnet node answers to, read from the same
// status as SelfURLs: its MagicDNS full and short names and every tailnet
// IP. The relay accepts only these (and its --listen address) in Host on
// its tailnet listener (relay.HardenForBrowsers).
func (r *LocalResolver) SelfNames(ctx context.Context) []string {
	st, err := r.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return nil
	}
	return selfNames(st.Self)
}

func selfNames(self *ipnstate.PeerStatus) []string {
	if self == nil {
		return nil
	}
	var out []string
	if name := strings.TrimSuffix(self.DNSName, "."); name != "" {
		out = append(out, name, shortName(name))
	}
	for _, ip := range self.TailscaleIPs {
		out = append(out, ip.String())
	}
	return out
}

// CertDomains are the HTTPS certificate names Tailscale issues for this
// node, empty when HTTPS certificates are off for the tailnet.
func (r *LocalResolver) CertDomains(ctx context.Context) ([]string, error) {
	st, err := r.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailnet status: %w", err)
	}
	return st.CertDomains, nil
}

// GetCertificate gets this node's tailnet certificate for a TLS handshake.
func (r *LocalResolver) GetCertificate(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.lc.GetCertificate(hi)
}
