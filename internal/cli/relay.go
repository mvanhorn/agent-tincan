package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"tailscale.com/tsnet"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/gateway"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

type relayFlags struct {
	listen      string
	hostname    string
	stateDir    string
	port        int
	admins      []string
	adminLogins []string
	noRebind    bool
	replyGrace  time.Duration

	gateway         bool
	gatewayHostname string
	gatewayListen   string
	gatewayURL      string
}

func relayCmd() *cobra.Command {
	var f relayFlags
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "Run the relay (on the always-on machine, e.g. the Grok Bot VM)",
		Long: `Run the relay. By default it joins the tailnet as its own node with tsnet
(set TS_AUTHKEY, or follow the login URL it prints). With --listen it binds
this host's tailnet IP and uses the host's tailscaled instead.

Admin commands (invite, remove) are accepted from the local admin socket in
the state dir and from machines named in --admin that carry no Tailscale tags
(and, with --admin-login, are owned by a listed login). Tag agent machines
(e.g. tag:agent) so they can never be admins.

Wake settings (webhook URLs, email addresses, keys) live in wake.json in the
state dir, chmod 600. They are never sent to agents. A webhook or email agent
is also woken when a reply to its own request is still unread after
--reply-grace.

A rebuilt machine (a new Tailscale node with the same machine name, or that
name plus a "-1" style suffix) is re-admitted as its old agent on its first
call when it is untagged, owned by the login recorded at join, and the old
node is offline or gone. Each one is audited as a "rebind" event. Turn this
off with --no-auto-rebind.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return runRelay(cmd.Context(), f) },
	}
	cmd.Flags().StringVar(&f.listen, "listen", "", "bind this host tailnet IP (100.x.y.z) instead of starting tsnet")
	cmd.Flags().StringVar(&f.hostname, "hostname", "tincan-relay", "tsnet node name")
	cmd.Flags().StringVar(&f.stateDir, "state-dir", defaultStateDir(), "relay state: database, tsnet state, admin socket")
	cmd.Flags().IntVar(&f.port, "port", 80, "port to serve the agent API on")
	cmd.Flags().StringSliceVar(&f.admins, "admin", nil, "machine names allowed to run admin commands (e.g. macbook-pro-44,iphone182)")
	cmd.Flags().StringSliceVar(&f.adminLogins, "admin-login", nil, "if set, admin machines must also be owned by one of these Tailscale logins")
	cmd.Flags().BoolVar(&f.noRebind, "no-auto-rebind", false, "do not re-admit rebuilt machines automatically; they need a new invite")
	cmd.Flags().DurationVar(&f.replyGrace, "reply-grace", wake.DefaultReplyGrace, "how long a reply may go unread before a webhook or email agent is woken to read it")
	cmd.Flags().BoolVar(&f.gateway, "chatgpt-gateway", false, "serve the public ChatGPT MCP gateway through Tailscale Funnel (OAuth-protected)")
	cmd.Flags().StringVar(&f.gatewayHostname, "gateway-hostname", "tincan-gateway", "tsnet node name for the Funnel gateway")
	cmd.Flags().StringVar(&f.gatewayListen, "gateway-listen", "", "serve the gateway on this plain-HTTP address instead of Funnel (put your own TLS proxy in front)")
	cmd.Flags().StringVar(&f.gatewayURL, "gateway-url", "", "public https URL of the gateway when using --gateway-listen")
	return cmd
}

// directoryConfig maps the relay flags onto the identity directory.
func (f relayFlags) directoryConfig() identity.Config {
	return identity.Config{Admins: f.admins, AdminLogins: f.adminLogins, NoAutoRebind: f.noRebind}
}

func defaultStateDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "tincan-relay")
	}
	return ".tincan-relay"
}

func runRelay(ctx context.Context, f relayFlags) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(f.stateDir, "relay.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	var ln net.Listener
	var who *identity.LocalResolver
	if f.listen != "" {
		if !strings.HasPrefix(f.listen, "100.") {
			return fmt.Errorf("--listen must be a tailnet 100.x address, got %q", f.listen)
		}
		who = identity.NewLocalResolverAt("")
		if err := who.Probe(ctx); err != nil {
			return fmt.Errorf("refusing to start without WhoIs: %w", err)
		}
		ln, err = net.Listen("tcp", net.JoinHostPort(f.listen, fmt.Sprint(f.port)))
		if err != nil {
			return err
		}
	} else {
		ts := &tsnet.Server{Hostname: f.hostname, Dir: filepath.Join(f.stateDir, "tsnet"), AuthKey: os.Getenv("TS_AUTHKEY")}
		defer ts.Close()
		if _, err := ts.Up(ctx); err != nil {
			return fmt.Errorf("tsnet up: %w", err)
		}
		lc, err := ts.LocalClient()
		if err != nil {
			return err
		}
		who = identity.NewLocalResolver(lc)
		ln, err = ts.Listen("tcp", fmt.Sprintf(":%d", f.port))
		if err != nil {
			return err
		}
	}
	if len(f.admins) == 0 {
		log.Printf("no --admin machines set: invites only work from the local admin socket")
	}

	dir := identity.NewDirectory(st, identity.WithVirtual(who), f.directoryConfig())
	srv := relay.New(dir, st, relay.Config{})
	srv.SetPreparer(policy.New(st, policy.Config{}))
	wakeCfg, err := wake.LoadConfig(filepath.Join(f.stateDir, "wake.json"))
	if err != nil {
		return err
	}
	waker := wake.New(wakeCfg, st, wake.Options{Online: srv.Online, UnseenReplies: srv.UnseenReplies, ReplyGrace: f.replyGrace})
	srv.SetEvents(waker)
	srv.SetWakeNamer(waker)
	if err := resumeReplyWakes(ctx, st, waker); err != nil {
		log.Printf("reschedule reply wakes: %v", err) // replies stay unseen for the agent's next check
	}
	go srv.Run(ctx)

	api := client.Configure(&http.Server{Handler: srv.Handler()}, client.RelayAPI)
	adminSock := filepath.Join(f.stateDir, "admin.sock")
	os.Remove(adminSock)
	aln, err := net.Listen("unix", adminSock)
	if err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	if err := os.Chmod(adminSock, 0o600); err != nil {
		return err
	}
	admin := client.Configure(&http.Server{Handler: srv.AdminHandler()}, client.RelayAPI)

	errc := make(chan error, 3)
	servers := []*http.Server{api, admin}
	go func() { errc <- api.Serve(ln) }()
	go func() { errc <- admin.Serve(aln) }()
	if f.gateway {
		gln, base, closeGW, err := gatewayListener(ctx, f)
		if err != nil {
			return fmt.Errorf("chatgpt gateway: %w", err)
		}
		defer closeGW()
		oauth, err := gateway.NewOAuth(st.DB(), nil)
		if err != nil {
			return err
		}
		srv.SetConnector(gateway.Connector{Dir: dir, OAuth: oauth, Base: base})
		gw := client.Configure(&http.Server{Handler: gateway.New(base, oauth, srv.Handler(), Version).Handler()}, client.RelayAPI)
		go func() { errc <- gw.Serve(gln) }()
		servers = append(servers, gw)
		log.Printf("chatgpt gateway serving at %s/mcp", base)
	}
	log.Printf("tincan relay serving on %s (admin socket %s)", ln.Addr(), adminSock)

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown(servers)
	return nil
}

// shutdown drains servers gracefully so held long-polls finish rather than
// being severed on restart, falling back to Close if the drain times out.
func shutdown(servers []*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), client.DefaultPollHold+5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() {
			if err := s.Shutdown(ctx); err != nil {
				s.Close()
			}
		})
	}
	wg.Wait()
}

// gatewayListener returns the public listener and base URL for the ChatGPT
// gateway: a Funnel listener on its own tsnet node by default.
func gatewayListener(ctx context.Context, f relayFlags) (net.Listener, string, func(), error) {
	if f.gatewayListen != "" {
		if f.gatewayURL == "" {
			return nil, "", nil, fmt.Errorf("--gateway-url is required with --gateway-listen")
		}
		ln, err := net.Listen("tcp", f.gatewayListen)
		return ln, f.gatewayURL, func() {}, err
	}
	ts := &tsnet.Server{Hostname: f.gatewayHostname, Dir: filepath.Join(f.stateDir, "tsnet-gateway"), AuthKey: os.Getenv("TS_AUTHKEY")}
	if _, err := ts.Up(ctx); err != nil {
		return nil, "", nil, err
	}
	ln, err := ts.ListenFunnel("tcp", ":443")
	if err != nil {
		ts.Close()
		return nil, "", nil, fmt.Errorf("%w (Funnel needs HTTPS certificates and the funnel node attribute in your tailnet policy)", err)
	}
	domains := ts.CertDomains()
	if len(domains) == 0 {
		ts.Close()
		return nil, "", nil, errors.New("no HTTPS domain for the gateway node; enable HTTPS certificates in the Tailscale admin console")
	}
	return ln, "https://" + domains[0], func() { ts.Close() }, nil
}

// resumeReplyWakes schedules a reply wake for every agent that still holds
// unseen replies. The waker keeps its grace-period timers in memory, so
// without this a relay restart inside the grace window would never wake the
// asker. Each wake still waits out the grace period and is dropped if the
// reply was read by then.
func resumeReplyWakes(ctx context.Context, st *store.Store, w *wake.Waker) error {
	agents, err := st.AgentsWithUnseenReplies(ctx)
	if err != nil {
		return err
	}
	for _, a := range agents {
		w.ReplyWaiting(a)
	}
	return nil
}
