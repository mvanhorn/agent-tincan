package cli

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"tailscale.com/tsnet"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/gateway"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/pass"
	"github.com/mvanhorn/agent-tincan/internal/phone"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

type relayFlags struct {
	urgentPerHour int
	listen        string
	hostname      string
	stateDir      string
	port          int
	admins        []string
	adminLogins   []string
	noRebind      bool
	replyGrace    time.Duration
	notesTTL      time.Duration
	wakeGrace     time.Duration
	urgentGrace   time.Duration
	urgentLease   time.Duration
	ownerNotice   int
	dist          string
	upgradeExit   bool
	releaseURL    string

	gateway         bool
	gatewayHostname string
	gatewayListen   string
	gatewayURL      string
}

// relayHostErr refuses to run the relay where it is not supported.
func relayHostErr(goos string) error {
	if goos == "windows" {
		return errors.New("tincan relay runs on Linux and macOS; on Windows, tincan is a client only (experimental, CI tested only)")
	}
	return nil
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

Admin devices can also open the phone page at /phone/ (see docs/phone.md).
When the tailnet issues the relay an HTTPS certificate, the relay serves it
over HTTPS on port 443 under its MagicDNS name too; it logs the page's
address at startup either way.

Wake settings (webhook URLs, email addresses, keys) live in wake.json in the
state dir, chmod 600. They are never sent to agents. A webhook or email agent
is also woken when a reply to its own request is still unread after
--reply-grace. A webhook or email agent that has not checked in by
--wake-grace after a wake shows as unanswered in tincan agents and top, and
is woken again every --wake-grace (--urgent-wake-grace while an urgent request
to it is queued), on the next of its wake.json fallback paths when it has
any. When a request has waited through such a silent grace, its
asker is told once, with the teammates online now. A claim on an urgent
request lasts --urgent-claim-lease (each progress note renews it); when it
runs out with no reply the request is requeued, the agent woken again and
the asker told. An asker is also told when a request expires unanswered.
When a webhook or email agent leaves --owner-notice-after wakes in a row
unanswered, the owner is told once per silent episode, through the notify
destination of the approval policy.

An unanswered request expires after 24 hours, except one to a notes-kind
agent, which waits --notes-ttl (30 days by default) so a sleeping notes Mac
loses nothing.

A rebuilt machine (a new Tailscale node with the same machine name, or that
name plus a "-1" style suffix) is re-admitted as its old agent on its first
call when it is untagged, owned by the login recorded at join, and the old
node is offline or gone. Each one is audited as a "rebind" event. Turn this
off with --no-auto-rebind.

With --dist <dir>, the relay serves tincan release binaries from dir to joined
agents and admins, so tincan upgrade works on machines without GitHub access.
Put the raw binaries there as tincan_<os>_<arch> (linux or darwin, amd64 or
arm64) and tincan_windows_<arch>.exe, plus checksums.txt and a VERSION file
naming the release. An admin can
then upgrade the relay itself with tincan relay-upgrade: it installs the dist
build over this binary (which the relay user must own) and re-executes, or
with --upgrade-exit exits with status 75 for its supervisor to restart it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.urgentPerHour < 1 {
				return fmt.Errorf("--urgent-per-hour must be at least 1, got %d", f.urgentPerHour)
			}
			if f.notesTTL <= 0 {
				return fmt.Errorf("--notes-ttl must be positive, got %s", f.notesTTL)
			}
			if f.wakeGrace <= 0 {
				return fmt.Errorf("--wake-grace must be positive, got %s", f.wakeGrace)
			}
			if f.urgentGrace <= 0 {
				return fmt.Errorf("--urgent-wake-grace must be positive, got %s", f.urgentGrace)
			}
			if f.ownerNotice < 1 {
				return fmt.Errorf("--owner-notice-after must be at least 1, got %d", f.ownerNotice)
			}
			if f.urgentLease <= 0 {
				return fmt.Errorf("--urgent-claim-lease must be positive, got %s", f.urgentLease)
			}
			if f.releaseURL != "" && !strings.HasPrefix(f.releaseURL, "https://") {
				return fmt.Errorf("--release-url must be an https URL, got %q", f.releaseURL)
			}
			if err := relayHostErr(runtime.GOOS); err != nil {
				return err
			}
			err := runRelay(cmd.Context(), f)
			if rs, ok := errors.AsType[*errRelayRestart](err); ok {
				return rs.u.restart()
			}
			return err
		},
	}
	cmd.Flags().StringVar(&f.listen, "listen", "", "bind this host tailnet IP (100.x.y.z) instead of starting tsnet")
	cmd.Flags().StringVar(&f.hostname, "hostname", "tincan-relay", "tsnet node name")
	cmd.Flags().StringVar(&f.stateDir, "state-dir", defaultStateDir(), "relay state: database, tsnet state, admin socket")
	cmd.Flags().IntVar(&f.port, "port", 80, "port to serve the agent API on")
	cmd.Flags().StringSliceVar(&f.admins, "admin", nil, "machine names allowed to run admin commands (e.g. macbook-pro-44,iphone182)")
	cmd.Flags().StringSliceVar(&f.adminLogins, "admin-login", nil, "if set, admin machines must also be owned by one of these Tailscale logins")
	cmd.Flags().BoolVar(&f.noRebind, "no-auto-rebind", false, "do not re-admit rebuilt machines automatically; they need a new invite")
	cmd.Flags().IntVar(&f.urgentPerHour, "urgent-per-hour", 5, "maximum urgent requests per sender per hour")
	cmd.Flags().DurationVar(&f.replyGrace, "reply-grace", wake.DefaultReplyGrace, "how long a reply may go unread before a webhook or email agent is woken to read it")
	cmd.Flags().DurationVar(&f.wakeGrace, "wake-grace", relay.DefaultWakeGrace, "how long a webhook or email agent may go without checking in after a wake before it shows as unanswered and the relay sends the same wake again")
	cmd.Flags().DurationVar(&f.urgentGrace, "urgent-wake-grace", relay.DefaultUrgentWakeGrace, "--wake-grace while an urgent request to the agent is queued, when shorter: how soon a silent webhook or email agent is woken again and the asker is told")
	cmd.Flags().DurationVar(&f.urgentLease, "urgent-claim-lease", relay.DefaultUrgentClaimLease, "how long a claim on an urgent request lasts without a reply or progress note before it is requeued, the target woken again and the asker told (other requests keep 30m)")
	cmd.Flags().IntVar(&f.ownerNotice, "owner-notice-after", relay.DefaultOwnerNoticeAfter, "how many wakes in a row a webhook or email agent may leave unanswered before the owner is told, once per silent episode, through the approval policy's notify destination")
	cmd.Flags().DurationVar(&f.notesTTL, "notes-ttl", 30*24*time.Hour, "how long a request to a notes-kind agent waits unanswered before it expires (other kinds keep 24h)")
	cmd.Flags().StringVar(&f.dist, "dist", "", "serve tincan release binaries (tincan_<os>_<arch>, checksums.txt, VERSION) from this directory for tincan upgrade")
	cmd.Flags().BoolVar(&f.upgradeExit, "upgrade-exit", false, "after tincan relay-upgrade, exit with status 75 for a supervisor to restart the relay instead of re-executing it")
	cmd.Flags().StringVar(&f.releaseURL, "release-url", "", "let tincan relay-upgrade --from-github download releases from <url>/<tag>/<file> (for this project: "+GitHubReleaseURL+"); off when empty")
	cmd.Flags().BoolVar(&f.gateway, "chatgpt-gateway", false, "serve the public ChatGPT MCP gateway through Tailscale Funnel (OAuth-protected)")
	cmd.Flags().StringVar(&f.gatewayHostname, "gateway-hostname", "tincan-gateway", "tsnet node name for the Funnel gateway")
	cmd.Flags().StringVar(&f.gatewayListen, "gateway-listen", "", "serve the gateway on this plain-HTTP address instead of Funnel (put your own TLS proxy in front)")
	cmd.Flags().StringVar(&f.gatewayURL, "gateway-url", "", "public https URL of the gateway when using --gateway-listen")
	return cmd
}

// relayConfig maps the relay flags onto the relay server.
func (f relayFlags) relayConfig() relay.Config {
	return relay.Config{Version: Version, NotesRequestTTL: f.notesTTL, WakeGrace: f.wakeGrace, UrgentWakeGrace: f.urgentGrace, UrgentClaimLease: f.urgentLease, OwnerNoticeAfter: f.ownerNotice,
		ParsePass: parsePassRef}
}

// parsePassRef reads a pass request for the relay's held listing and
// pass_sent audit, exactly as the pass service will.
func parsePassRef(body string) (relay.PassRef, error) {
	r, err := pass.ParseRequest(body)
	return relay.PassRef{Source: string(r.Source), ConversationID: r.ID, To: r.To}, err
}

// directoryConfig maps the relay flags onto the identity directory.
func (f relayFlags) directoryConfig() identity.Config {
	return identity.Config{Admins: f.admins, AdminLogins: f.adminLogins, NoAutoRebind: f.noRebind}
}

// wakerOptions maps relay flags and the live server onto the waker.
func wakerOptions(f relayFlags, srv *relay.Server) wake.Options {
	return wake.Options{
		Online:          srv.Online,
		Queued:          srv.QueuedCount,
		UrgentQueued:    srv.UrgentQueuedCount,
		UnseenReplies:   srv.UnseenReplies,
		QueuedIDs:       srv.QueuedIDs,
		UnseenReplyIDs:  srv.UnseenReplyIDs,
		LastPoll:        srv.LastPoll,
		Unanswered:      srv.TellAskers,
		ReplyGrace:      f.replyGrace,
		WakeGrace:       f.wakeGrace,
		UrgentWakeGrace: f.urgentGrace,
		OpenAsks:        srv.OpenAsks,
		RequestTag:      srv.RequestEmailTag,
	}
}

// emailReplyInboxes lists the agents in wake.json with include_requests on
// (always on an email primary path; LoadConfig refuses it elsewhere), by
// name, with the AgentMail inbox their request emails are sent from and
// the address whose replies count. The relay polls each one for replies
// to its request emails (relay.Server.PollEmailReplies).
func emailReplyInboxes(cfg wake.Config) []relay.EmailInbox {
	var out []relay.EmailInbox
	for _, name := range slices.Sorted(maps.Keys(cfg)) {
		t := cfg[name]
		if t.Method != wake.Email || !t.IncludeRequests {
			continue
		}
		out = append(out, relay.EmailInbox{Agent: name, Address: t.EmailTo, Mail: wake.NewAgentMail("", t.AgentMailFrom, t.AgentMailKey, nil)})
	}
	return out
}

// loadOrCreateInvitePepper returns the relay's invite-code pepper, creating
// and persisting a fresh 256-bit one on first run (see loadOrCreateKeyFile).
// A missing key beside an existing database is never silently replaced: it
// is either a first upgrade (expected) or an accidental loss (outstanding
// invites are stranded), and the operator is told which.
func loadOrCreateInvitePepper(stateDir string) ([]byte, error) {
	b, created, err := loadOrCreateKeyFile(stateDir, "invite-pepper", "invite pepper")
	if err != nil {
		return nil, err
	}
	if created {
		if _, err := os.Stat(filepath.Join(stateDir, "relay.db")); err == nil {
			log.Printf("warning: created a fresh invite pepper next to an existing relay.db: " +
				"on a first upgrade from raw-code invites this is expected (legacy invites are invalidated); " +
				"if the old invite-pepper file was lost, outstanding invites are stranded — restore it from backup")
		}
	}
	return b, nil
}

// loadOrCreateEmailTagKey returns the relay's email-tag-key
// (relay.EmailTagKeyFile), which signs the reply tags in request emails,
// creating a fresh 256-bit one on first run exactly like invite-pepper. A
// new key only revokes tags in emails already sent, so a missing one is
// simply created.
func loadOrCreateEmailTagKey(stateDir string) ([]byte, error) {
	b, _, err := loadOrCreateKeyFile(stateDir, relay.EmailTagKeyFile, "email tag key")
	return b, err
}

// loadOrCreatePusher returns the relay's Web Push sender, signing with the
// VAPID key in relay.PushKeyFile, which is created on first run exactly
// like invite-pepper. A new key only means subscribed phones subscribe
// again on their next open.
func loadOrCreatePusher(stateDir string) (*relay.Pusher, error) {
	b, _, err := loadOrCreateKeyFile(stateDir, relay.PushKeyFile, "push key")
	if err != nil {
		return nil, err
	}
	return relay.NewPusher(b)
}

// loadOrCreateKeyFile returns the 32-byte relay-local key in stateDir/name,
// creating and persisting a fresh 256-bit one on first run, and whether it
// did. A private temporary file is fully written, synced and closed before
// an atomic, no-replace link publishes it. Exactly one writer wins; others
// read its complete key. Filesystems without hard-link support fail closed.
// An existing file is validated before use — it must be a regular file
// (never a symlink), exactly 32 bytes, and inaccessible to group/other — and
// anything else fails closed. label names the key in errors.
func loadOrCreateKeyFile(stateDir, name, label string) ([]byte, bool, error) {
	path := filepath.Join(stateDir, name)
	if b, err := readKeyFileWithOpen(path, label, openInvitePepperFile); err == nil {
		return b, false, nil
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, false, fmt.Errorf("generate %s: %w", label, err)
	}
	// Never expose a partially written file at the final name. O_EXCL at
	// that name elects one writer but still lets concurrent readers see an
	// empty file before its first write. CreateTemp uses owner-only mode.
	f, err := os.CreateTemp(stateDir, "."+name+"-*")
	if err != nil {
		return nil, false, fmt.Errorf("create temporary %s: %w", label, err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return nil, false, fmt.Errorf("write %s: %w", label, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, false, fmt.Errorf("sync %s: %w", label, err)
	}
	if err := f.Close(); err != nil {
		return nil, false, fmt.Errorf("close %s: %w", label, err)
	}
	// Link is atomic and does not replace an existing destination. Rename
	// would let a losing initializer overwrite the winner on Unix.
	if err := os.Link(f.Name(), path); err != nil {
		if os.IsExist(err) {
			winner, readErr := readKeyFileWithOpen(path, label, openInvitePepperFile)
			if readErr != nil {
				return nil, false, fmt.Errorf("read the winning %s: %w", label, readErr)
			}
			return winner, false, nil
		}
		return nil, false, fmt.Errorf("publish %s: %w", label, err)
	}
	return b, true, nil
}

// readInvitePepperFile reads an existing pepper key, refusing symlinks,
// non-regular files, wrong permissions, and wrong lengths. A missing file
// returns an os.IsNotExist error so the caller can create it.
func readInvitePepperFile(path string) ([]byte, error) {
	return readInvitePepperFileWithOpen(path, openInvitePepperFile)
}

// The opener is passed explicitly so validation can be exercised against
// the actual opened file without timing-dependent filesystem tests.
func readInvitePepperFileWithOpen(path string, open func(string) (*os.File, error)) ([]byte, error) {
	return readKeyFileWithOpen(path, "invite pepper", open)
}

// readKeyFileWithOpen reads an existing 32-byte key file through open (which
// refuses symlinks), validating the opened handle: a regular file, private
// to its owner, exactly 32 bytes. label names the key in errors.
func readKeyFileWithOpen(path, label string, open func(string) (*os.File, error)) ([]byte, error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Validate the handle that will be read, never a separately resolved path.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %s is not a regular file", label, path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s %s is accessible beyond its owner (mode %04o): refusing to use it", label, path, perm)
	}
	if fi.Size() != 32 {
		return nil, fmt.Errorf("%s %s has %d bytes, want 32", label, path, fi.Size())
	}
	// Bound reads even if another process changes the file after Stat.
	b, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("%s %s has %d bytes, want 32", label, path, len(b))
	}
	return b, nil
}

func defaultStateDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "tincan-relay")
	}
	return ".tincan-relay"
}

// listenAddr parses --listen: a tailnet IPv4 address (100.x.y.z), with
// an optional :port that overrides port. Anything else is refused, so a
// typo fails before the relay creates its state.
func listenAddr(listen string, port int) (string, error) {
	bad := fmt.Errorf("--listen must be a tailnet 100.x address, got %q", listen)
	host := strings.TrimSpace(listen)
	if h, p, err := net.SplitHostPort(host); err == nil {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > 65535 {
			return "", bad
		}
		host, port = h, n
	}
	ip := net.ParseIP(host).To4()
	if ip == nil || ip[0] != 100 || strings.Contains(host, ":") {
		return "", bad
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

// advertisePort is the port SelfURLs should name: the port --listen bound,
// which may differ from --port when --listen carried :port.
func advertisePort(listenAt string, flagPort int) int {
	if listenAt == "" {
		return flagPort
	}
	_, p, err := net.SplitHostPort(listenAt)
	if err != nil {
		return flagPort
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return flagPort
	}
	return n
}

func runRelay(ctx context.Context, f relayFlags) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Parse --listen fully before creating anything in the state dir.
	var listenAt string
	if f.listen != "" {
		addr, err := listenAddr(f.listen, f.port)
		if err != nil {
			return err
		}
		listenAt = addr
	}
	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		return err
	}
	pepper, err := loadOrCreateInvitePepper(f.stateDir)
	if err != nil {
		return err
	}
	emailTagKey, err := loadOrCreateEmailTagKey(f.stateDir)
	if err != nil {
		return err
	}
	pusher, err := loadOrCreatePusher(f.stateDir)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(f.stateDir, "relay.db"))
	if err != nil {
		return err
	}
	closeStore := sync.OnceFunc(func() {
		if err := st.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	})
	defer closeStore()
	approval, err := policy.LoadApproval(filepath.Join(f.stateDir, "approval.json"))
	if err != nil {
		return err
	}

	ln, who, closeNetFn, err := openRelayNet(ctx, f, listenAt)
	if err != nil {
		return err
	}
	closeNet := sync.OnceFunc(closeNetFn)
	defer closeNet()
	if len(f.admins) == 0 {
		log.Printf("no --admin machines set: invites only work from the local admin socket")
	}

	dirCfg := f.directoryConfig()
	dirCfg.InvitePepper = pepper
	dir := identity.NewDirectory(st, identity.WithVirtual(who), dirCfg)
	srv := relay.New(dir, st, f.relayConfig())
	srv.SetEmailTagKey(emailTagKey)
	srv.SetPusher(pusher)
	urls := who.SelfURLs(ctx, advertisePort(listenAt, f.port))
	srv.SetURLs(urls)
	log.Printf("tincan relay advertises %s to its agents", strings.Join(urls, ", "))
	hosts, err := relayHosts(who.SelfNames(ctx), urls, listenAt)
	if err != nil {
		return err
	}
	log.Printf("tincan relay accepts requests addressed to %s", strings.Join(hosts, ", "))
	if f.listen != "" {
		log.Printf("warning: with --listen the relay's address is this host's tailnet address, which changes if the host re-joins Tailscale. "+
			"Agents with tailscale find it again by themselves; proxy-only agents may need tincan rejoin. "+
			"Without --listen the relay is its own tailnet node (%s) and keeps its name while --state-dir is kept.", f.hostname)
	}
	srv.SetPreparer(policy.New(st, policy.Config{Approval: approval, UrgentPerHour: f.urgentPerHour}))
	if f.dist != "" {
		if fi, err := os.Stat(f.dist); err != nil || !fi.IsDir() {
			return fmt.Errorf("--dist %s: not a directory", f.dist)
		}
		srv.SetDist(f.dist)
	}
	ctx, up := withRelayUpgrader(ctx, srv, f)
	wakeCfg, err := wake.LoadConfig(filepath.Join(f.stateDir, "wake.json"))
	if err != nil {
		return err
	}
	wopts := wakerOptions(f, srv)
	if d, ok := who.(tailnetDialer); ok {
		// Wake webhooks at tailnet addresses go out through the relay's own
		// node: the host may have no route to the tailnet at all.
		wopts.HTTP = wakeHTTPClient(d.DialTailnet)
	}
	waker := wake.New(wakeCfg, st, wopts)
	waker.SetConfigPath(filepath.Join(f.stateDir, "wake.json"))
	srv.SetWakeSettings(waker)
	if inboxes := emailReplyInboxes(wakeCfg); len(inboxes) > 0 {
		srv.SetEmailInboxes(inboxes)
		log.Printf("polling AgentMail for email replies from %d agents with include_requests on", len(inboxes))
	}
	srv.SetEvents(waker)
	srv.SetWakeNamer(waker)
	if err := resumeReplyWakes(ctx, st, waker); err != nil {
		log.Printf("reschedule reply wakes: %v", err) // replies stay unseen for the agent's next check
	}
	if err := resumeRequestWakes(ctx, st, waker); err != nil {
		log.Printf("reschedule request wakes: %v", err) // requests stay queued for the agent's next check
	}
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		srv.Run(runCtx)
	}()

	// Only the tailnet listener is hardened against web pages: the gateway
	// below calls srv.Handler() in process with Host tincan-relay.
	api := client.Configure(&http.Server{Handler: relay.HardenForBrowsers(srv.Handler(), hosts)}, client.RelayAPI)
	adminSock := filepath.Join(f.stateDir, "admin.sock")
	os.Remove(adminSock)
	aln, err := client.ListenUnix(adminSock)
	if err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	if err := os.Chmod(adminSock, 0o600); err != nil {
		return err
	}
	admin := client.Configure(&http.Server{Handler: srv.AdminHandler()}, client.RelayAPI)

	errc := make(chan error, 4)
	servers := []*http.Server{api, admin}
	closeGW := func() {}
	go func() { errc <- api.Serve(ln) }()
	go func() { errc <- admin.Serve(aln) }()
	// The phone page is served on every tailnet listener. With HTTPS from
	// the tailnet it also gets its own TLS listener, hardened like the
	// plain one with the certificate name added to the names it accepts;
	// without it the page works over HTTP, minus push alerts.
	if tln, certName, err := phoneTLS(ctx, who, advertisePort(listenAt, f.port)); err != nil {
		log.Printf("phone page: %s (no HTTPS: %v; push alerts are off, so the approval notify teammate is the alert path)", phonePageURL(urls, ln), err)
	} else {
		defer tln.Close()
		tlsHosts := append(slices.Clone(hosts), certName)
		web := client.Configure(&http.Server{Handler: relay.HardenForBrowsers(srv.Handler(), tlsHosts)}, client.RelayAPI)
		go func() { errc <- web.Serve(tln) }()
		servers = append(servers, web)
		log.Printf("phone page: https://%s%s", certName, phone.Prefix)
	}
	if f.gateway {
		gln, base, closeGWFn, err := gatewayListener(ctx, f)
		if err != nil {
			return fmt.Errorf("chatgpt gateway: %w", err)
		}
		closeGW = sync.OnceFunc(closeGWFn)
		defer closeGW()
		oauth, err := gateway.NewOAuth(st.DB(), nil)
		if err != nil {
			return err
		}
		a2aFile := filepath.Join(f.stateDir, "a2a.json")
		srv.SetConnector(gateway.Connector{Dir: dir, OAuth: oauth, Base: base, A2AFile: a2aFile})
		pub := gateway.New(base, oauth, srv.Handler(), Version)
		pub.SetA2AFile(a2aFile)
		gw := client.Configure(&http.Server{Handler: pub.Handler()}, client.RelayAPI)
		go func() { errc <- gw.Serve(gln) }()
		servers = append(servers, gw)
		log.Printf("chatgpt gateway serving at %s/mcp", base)
	}
	log.Printf("tincan relay serving on %s (admin socket %s)", ln.Addr(), adminSock)

	var served error
	select {
	case <-ctx.Done():
	case served = <-errc:
		if errors.Is(served, http.ErrServerClosed) {
			served = nil
		}
	}
	// From here a second SIGINT or SIGTERM gets the default handling and
	// ends the process at once.
	stop()
	log.Printf("tincan relay shutting down")
	var step atomic.Value
	watchdog := time.AfterFunc(relayExitTimeout, func() {
		log.Printf("tincan relay: shutdown stuck in %s after %s; exiting now", step.Load(), relayExitTimeout)
		os.Exit(1)
	})
	defer watchdog.Stop()
	for _, s := range []struct {
		name string
		do   func()
	}{
		// Held long polls and waits answer "nothing yet" first, so the
		// drain below only waits for calls that are really working.
		{"ending held polls", srv.Stop},
		{"draining connections", func() { shutdown(servers) }},
		// Run also polls AgentMail for email replies; ending it ends a
		// poll in flight, Retry-After waits included.
		{"stopping sweeps and email reply polls", func() { stopRun(); <-ran }},
		{"stopping wakes", waker.Stop},
		{"stopping push alerts", srv.WaitPush},
		{"closing the gateway", closeGW},
		{"closing the tailnet listener", closeNet},
		{"closing the store", closeStore},
	} {
		step.Store(s.name)
		s.do()
	}
	log.Printf("tincan relay stopped")
	if served != nil {
		return served
	}
	// After tincan relay-upgrade: the store and listeners are closed, so
	// the caller can now re-exec the new build or exit for a supervisor.
	return up.restartErr()
}

// relayHosts are the names the tailnet API accepts in Host: the node's own
// tailnet names, the host of every URL it advertises, and the --listen
// address. An empty list would refuse every caller, so it is an error.
func relayHosts(names, urls []string, listenAt string) ([]string, error) {
	out := slices.Clone(names)
	for _, raw := range urls {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			out = append(out, u.Hostname())
		}
	}
	if host, _, err := net.SplitHostPort(listenAt); err == nil {
		out = append(out, host)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil, errors.New("cannot read this relay's tailnet names from Tailscale; refusing to serve, since the API accepts only requests addressed to them")
	}
	return out, nil
}

// phonePageURL is the phone page on the relay's plain listener: under the
// first address it advertises, else the listener's own address.
func phonePageURL(urls []string, ln net.Listener) string {
	base := "http://" + ln.Addr().String()
	if len(urls) > 0 {
		base = strings.TrimSuffix(urls[0], "/")
	}
	return base + phone.Prefix
}

// tlsTailnet is a relay resolver whose tailnet can also serve HTTPS: it
// opens a TLS listener with the node's tailnet certificate and returns the
// certificate's name.
type tlsTailnet interface {
	ListenTLS(ctx context.Context) (net.Listener, string, error)
}

// errNoCertificates is why there is no HTTPS when the tailnet issues the
// relay no certificate name.
var errNoCertificates = errors.New("this tailnet has HTTPS certificates turned off (Tailscale admin console, DNS page, HTTPS Certificates)")

// errAPIOn443 is why there is no HTTPS when the API listener already holds
// port 443: the HTTPS listener would bind the same port. The API listener
// there is plain HTTP, so the page is served over HTTP on port 443.
var errAPIOn443 = errors.New("the API listener already uses port 443 (--port 443 or --listen ip:443) and serves plain HTTP there; pick another port for HTTPS")

// phoneTLS opens the phone page's HTTPS listener (KTD15), or says why it
// cannot. apiPort is the port the plain API listener bound. A tailscale
// serve front is not supported: the relay would see every request come
// from 127.0.0.1 and could not tell devices apart.
func phoneTLS(ctx context.Context, who relayResolver, apiPort int) (net.Listener, string, error) {
	if apiPort == 443 {
		return nil, "", errAPIOn443
	}
	t, ok := who.(tlsTailnet)
	if !ok {
		return nil, "", errors.New("this relay's tailnet connection cannot serve HTTPS")
	}
	return t.ListenTLS(ctx)
}

// relayDrainTimeout bounds how long shutdown waits for calls in flight
// before closing their connections. Held long polls do not count: they
// answer as soon as shutdown starts.
var relayDrainTimeout = 10 * time.Second

// relayExitTimeout bounds the whole shutdown. A relay still stuck after it
// (a tailnet node that will not close, say) logs the step and exits anyway.
var relayExitTimeout = relayDrainTimeout + 10*time.Second

// shutdown drains servers, closing whatever is still open when
// relayDrainTimeout runs out.
func shutdown(servers []*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), relayDrainTimeout)
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

// relayResolver is what the relay needs from its tailnet: caller identity,
// node liveness for rebinds, the URLs it advertises, and the names it
// accepts in Host.
type relayResolver interface {
	identity.Resolver
	identity.NodeStatus
	SelfURLs(ctx context.Context, port int) []string
	SelfNames(ctx context.Context) []string
}

// openRelayNet opens the relay's agent API listener and identity resolver:
// the host tailnet address through tailscaled with --listen, or its own
// tsnet node. Tests replace it to serve on loopback with a fake tailnet.
var openRelayNet = func(ctx context.Context, f relayFlags, listenAt string) (net.Listener, relayResolver, func(), error) {
	if f.listen != "" {
		who := identity.NewLocalResolverAt("")
		if err := who.Probe(ctx); err != nil {
			return nil, nil, nil, fmt.Errorf("refusing to start without WhoIs: %w", err)
		}
		ln, err := net.Listen("tcp", listenAt)
		if err != nil {
			return nil, nil, nil, err
		}
		host, _, _ := net.SplitHostPort(listenAt)
		return ln, hostTailnet{who, host}, func() {}, nil
	}
	ts := &tsnet.Server{Hostname: f.hostname, Dir: filepath.Join(f.stateDir, "tsnet"), AuthKey: os.Getenv("TS_AUTHKEY")}
	fail := func(err error) (net.Listener, relayResolver, func(), error) {
		ts.Close()
		return nil, nil, nil, err
	}
	if _, err := ts.Up(ctx); err != nil {
		return fail(fmt.Errorf("tsnet up: %w", err))
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return fail(err)
	}
	ln, err := ts.Listen("tcp", fmt.Sprintf(":%d", f.port))
	if err != nil {
		return fail(err)
	}
	listenTLS := func(context.Context) (net.Listener, string, error) {
		domains := ts.CertDomains()
		if len(domains) == 0 {
			return nil, "", errNoCertificates
		}
		ln, err := ts.ListenTLS("tcp", ":443")
		if err != nil {
			return nil, "", err
		}
		return ln, domains[0], nil
	}
	return ln, tailnetResolver{identity.NewLocalResolver(lc), ts.Dial, listenTLS}, func() { ts.Close() }, nil
}

// hostTailnet is the --listen relay's resolver: the host's tailscaled,
// which also serves HTTPS on the --listen address with the host's tailnet
// certificate.
type hostTailnet struct {
	*identity.LocalResolver
	ip string
}

// ListenTLS listens on port 443 of the --listen address with the host
// tailscaled's certificate for this machine.
func (h hostTailnet) ListenTLS(ctx context.Context) (net.Listener, string, error) {
	domains, err := h.CertDomains(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(domains) == 0 {
		return nil, "", errNoCertificates
	}
	ln, err := tls.Listen("tcp", net.JoinHostPort(h.ip, "443"), &tls.Config{GetCertificate: h.GetCertificate, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, "", err
	}
	return ln, domains[0], nil
}

// tailnetDialer is a relay resolver that can also open connections to
// tailnet machines from the relay's own node.
type tailnetDialer interface {
	DialTailnet(ctx context.Context, network, addr string) (net.Conn, error)
}

// tailnetResolver is the tsnet relay's resolver together with its node's
// dialer, so the waker can reach tailnet webhooks even when the host's
// tailscaled has no TUN device (wakeHTTPClient).
type tailnetResolver struct {
	relayResolver
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	listenTLS func(ctx context.Context) (net.Listener, string, error)
}

// ListenTLS opens the node's HTTPS listener on port 443.
func (r tailnetResolver) ListenTLS(ctx context.Context) (net.Listener, string, error) {
	return r.listenTLS(ctx)
}

// DialTailnet dials addr from the relay's tsnet node.
func (r tailnetResolver) DialTailnet(ctx context.Context, network, addr string) (net.Conn, error) {
	return r.dial(ctx, network, addr)
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

// resumeRequestWakes schedules a request wake for every agent that still has
// queued requests. A nudge scheduled before a restart lived only in the old
// process, so without this a webhook or email agent sent a request just
// before the relay stopped would never be woken for it. Each wake counts the
// queued requests when it fires and is dropped if a poller took them.
func resumeRequestWakes(ctx context.Context, st *store.Store, w *wake.Waker) error {
	agents, err := st.AgentsWithQueuedRequests(ctx)
	if err != nil {
		return err
	}
	for _, a := range agents {
		w.RequestsWaiting(a)
	}
	return nil
}
