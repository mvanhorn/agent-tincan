package identity

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
)

// Node is the tailnet machine a connection came from.
type Node struct {
	ID   string // Tailscale stable node id; the key agents are bound to
	Name string // short machine name, for display and the admin list
	User string // owning login
	Tags []string
	// Key is the node's current node key ("nodekey:..."). It changes when
	// the machine re-authenticates, while ID stays the same.
	Key string
	// Addrs are the node's tailnet addresses.
	Addrs []netip.Addr
	// DNSName is the node's full MagicDNS name, without the trailing dot.
	DNSName string
	// SharedIn is set for a node that is in the relay's netmap only because
	// it was shared in from, or is owned by a user of, another tailnet.
	SharedIn bool
}

// Resolver answers which tailnet node a connection came from.
type Resolver interface {
	WhoIs(ctx context.Context, remoteAddr string) (Node, error)
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
	return whoIsNode(res), nil
}

// whoIsNode is the Node a WhoIs answer with a node describes.
func whoIsNode(res *apitype.WhoIsResponse) Node {
	tn := res.Node
	n := Node{
		ID:      string(tn.StableID),
		Name:    shortName(tn.Name),
		Tags:    tn.Tags,
		Key:     tn.Key.String(),
		DNSName: strings.TrimSuffix(tn.Name, "."),
		// A sharee node is in the netmap because a user of another tailnet
		// owns it; a sharer marks a node shared in from another tailnet.
		SharedIn: tn.Sharer != 0 || (tn.Hostinfo.Valid() && tn.Hostinfo.ShareeNode()),
	}
	for _, p := range tn.Addresses {
		n.Addrs = append(n.Addrs, p.Addr())
	}
	if res.UserProfile != nil {
		n.User = res.UserProfile.LoginName
	}
	return n
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
		return peerNode(st, p), true, nil
	}
	return Node{}, false, nil
}

// peerNode is the Node a peer in st describes, with the same fields WhoIs
// fills.
func peerNode(st *ipnstate.Status, p *ipnstate.PeerStatus) Node {
	n := Node{
		ID:       string(p.ID),
		Name:     shortName(p.DNSName),
		Key:      p.PublicKey.String(),
		Addrs:    p.TailscaleIPs,
		DNSName:  strings.TrimSuffix(p.DNSName, "."),
		SharedIn: p.ShareeNode || p.AltSharerUserID != 0,
	}
	if p.Tags != nil {
		n.Tags = p.Tags.AsSlice()
	}
	if u, ok := st.User[p.UserID]; ok {
		n.User = u.LoginName
	}
	return n
}

// SelfNode is this tailnet node (the relay's own), with the same fields
// WhoIs fills.
func (r *LocalResolver) SelfNode(ctx context.Context) (Node, error) {
	st, err := r.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return Node{}, fmt.Errorf("tailnet status: %w", err)
	}
	if st.Self == nil {
		return Node{}, fmt.Errorf("tailnet status: no self node")
	}
	return peerNode(st, st.Self), nil
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
