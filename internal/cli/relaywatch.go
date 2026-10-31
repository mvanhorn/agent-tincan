package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/watch"
)

// relayProbeFor bounds one probe. It is longer than a search for a moved
// relay, so a probe that sets one off waits for its answer.
const relayProbeFor = 30 * time.Second

// relayWatchFlags are the flags relay-watch and relay-watch install share.
type relayWatchFlags struct {
	config, alertCmd string
	after, every     time.Duration
}

func (f *relayWatchFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.config, "config", "", "joined client config to probe with (default: $TINCAN_CONFIG, else this machine's client config)")
	cmd.Flags().StringVar(&f.alertCmd, "alert-cmd", "", "command run with /bin/sh -c on outage and on recovery; the alert text is in $TINCAN_WATCH_MESSAGE (required)")
	cmd.Flags().DurationVar(&f.after, "after", 10*time.Minute, "how long the relay must stay unreachable before the alert")
	cmd.Flags().DurationVar(&f.every, "every", time.Minute, "how often to probe the relay")
}

// check validates the flags and returns the absolute config path.
func (f *relayWatchFlags) check() (string, error) {
	switch {
	case f.alertCmd == "":
		return "", errors.New("--alert-cmd is required: the command that tells you the relay is down (see examples/watch/imessage-alert.sh)")
	case f.after <= 0:
		return "", errors.New("--after must be positive")
	case f.every <= 0:
		return "", errors.New("--every must be positive")
	}
	if err := needsShell(runtime.GOOS, "tincan relay-watch"); err != nil {
		return "", err
	}
	path := f.config
	if path == "" {
		path = client.ConfigPath()
	}
	return filepath.Abs(expandHome(path))
}

func relayWatchCmd() *cobra.Command {
	var f relayWatchFlags
	cmd := &cobra.Command{
		Use:   "relay-watch",
		Short: "Alert the owner, without going through the relay, when the relay is unreachable",
		Long: "Probes the relay every --every with this machine's joined client config, finding a relay that\n" +
			"moved the way every client does. When the relay has been unreachable for --after it runs\n" +
			"--alert-cmd once, and once more when the relay answers again. The command gets the alert in\n" +
			"TINCAN_WATCH_MESSAGE, plus TINCAN_WATCH_EVENT (down or up), TINCAN_WATCH_RELAY and\n" +
			"TINCAN_WATCH_SINCE; a command that fails is retried at the next probe. Run it on an always-on\n" +
			"device other than the relay's; tincan relay-watch install sets it up as a service.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := f.check()
			if err != nil {
				return err
			}
			cfg, err := client.LoadConfigFrom(path)
			if err != nil {
				return err
			}
			if cfg.Relay == "" {
				return fmt.Errorf("%s names no relay: point --config at this machine's joined agent config", path)
			}
			r, err := client.NewRelayForFile(cfg, path)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if client.NeedsRelayInfo(cfg) {
				// Discovery of a moved relay needs the relay key.
				learnRelayKeyWithin(ctx, r)
			}
			w := &watch.Watcher{
				Relay: r.Base,
				After: f.after,
				Probe: relayProbe(r),
				Alert: watch.Command(f.alertCmd),
				Logf:  log.New(cmd.ErrOrStderr(), "", log.LstdFlags).Printf,
			}
			cmd.PrintErrf("tincan relay-watch: watching %s every %s, alerting after %s\n", cfg.Relay, f.every, f.after)
			if err := w.Run(ctx, f.every); ctx.Err() == nil {
				return err
			}
			cmd.PrintErrln("tincan relay-watch: stopped")
			return nil
		},
	}
	f.add(cmd)
	cmd.AddCommand(relayWatchInstallCmd())
	return cmd
}

// relayProbe asks the relay who this client is, which finds a relay that
// moved (internal/client/discover.go). Any answer from the relay, even an
// error, means it is up, except a 502, 503 or 504: a proxy answering for a
// relay that is gone, or a relay that cannot check who is calling, which
// serves no agent either.
func relayProbe(r *client.Relay) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, relayProbeFor)
		defer cancel()
		_, err := r.WhoAmI(ctx)
		if api, ok := errors.AsType[*client.APIError](err); ok && !relayDownCode(api.Code) {
			return nil
		}
		return err
	}
}

func relayDownCode(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func relayWatchInstallCmd() *cobra.Command {
	var f relayWatchFlags
	var binary string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the relay watchdog service definition (launchd on macOS, systemd on Linux); does not start it",
		Long: "Writes a launchd agent (" + watch.ServiceLabel + ") on macOS, or a systemd user unit on Linux, that runs\n" +
			"tincan relay-watch with these flags and keeps it running. It prints the command that starts the\n" +
			"service and never starts it itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := f.check()
			if err != nil {
				return err
			}
			res, err := watch.InstallService(watch.ServiceOptions{
				Binary:   binary,
				Config:   path,
				AlertCmd: f.alertCmd,
				After:    f.after,
				Every:    f.every,
			})
			if err != nil {
				return err
			}
			cmd.Printf("relay watchdog service definition: %s (not started)\n", res.Path)
			if cfg, err := client.LoadConfigFrom(path); err != nil || cfg.Relay == "" {
				cmd.Printf("%s holds no joined agent; pass --config with this machine's joined agent config\n", path)
			}
			cmd.Printf("start it with:\n  %s\n", res.Next)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	return cmd
}
