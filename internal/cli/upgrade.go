package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
)

func upgradeCmd() *cobra.Command {
	var relayURL string
	var check, force bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Replace this tincan binary with the release the relay serves",
		Long: `Download the tincan build for this platform from the relay (tincan relay
--dist), verify its sha256 against the relay's manifest, and replace the
running binary. Agents without GitHub access update this way.

The new binary is written next to the old one and renamed over it, so the old
file is never modified in place. Long-running tincan processes (wait or listen
loops, MCP servers) keep running the old build until they are restarted; the
output names each running tincan mcp on the old build and how its app
reloads it.

A client newer than the relay's release is left in place, since installing
the relay's build would downgrade it; pass --force to install it anyway.

With --check, only report the current and available versions.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _ := client.LoadConfig()
			if relayURL != "" {
				cfg.Relay = relayURL
			}
			if cfg.Relay == "" {
				return errors.New("no relay configured: pass --relay http://tincan-relay or join first")
			}
			r, err := client.NewRelayFor(cfg)
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("find the running tincan binary: %w", err)
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return fmt.Errorf("resolve the running tincan binary: %w", err)
			}
			return upgrade(cmd.Context(), r, exe, check, force, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().BoolVar(&check, "check", false, "only report the current and available versions; change nothing")
	cmd.Flags().BoolVar(&force, "force", false, "install the relay's build even when it is older than this one")
	return cmd
}

// upgrade replaces the binary at exe with the relay's build for this
// platform. The binary is untouched unless the download matches the
// manifest's sha256. check reports without writing anything. A relay
// release older than this client is not installed unless force is set.
func upgrade(ctx context.Context, r *client.Relay, exe string, check, force bool, out io.Writer) error {
	m, err := r.Dist(ctx)
	if err != nil {
		if client.IsStatus(err, 404) {
			return fmt.Errorf("the relay does not serve upgrades; its operator must start tincan relay with --dist <dir>: %w", err)
		}
		return err
	}
	name := "tincan_" + runtime.GOOS + "_" + runtime.GOARCH
	want := m.SHA256Of(name)
	available := m.Version
	if available == "" {
		available = "unknown version"
	}
	if want == "" {
		return fmt.Errorf("the relay has no %s build (release %s); ask the relay operator to add %s to its dist directory", name, available, name)
	}
	have, err := fileSHA256(exe)
	if err != nil {
		return fmt.Errorf("read the running tincan binary: %w", err)
	}
	current := strings.TrimPrefix(Version, "v")
	if have == want {
		fmt.Fprintf(out, "tincan %s is up to date (matches the relay's %s build of %s).\n", current, name, available)
		return nil
	}
	if !force && client.Ahead(Version, m.Version) {
		fmt.Fprintf(out, "tincan %s at %s is newer than the relay's release %s; not replacing it with the older build.\n%s\nTo install the relay's %s anyway, run tincan upgrade --force.\n",
			current, exe, strings.TrimPrefix(m.Version, "v"), relayBehindAdvice(Version), strings.TrimPrefix(m.Version, "v"))
		return nil
	}
	if check {
		if client.Ahead(Version, m.Version) {
			fmt.Fprintf(out, "tincan %s at %s is newer than the relay's release %s; installing it would downgrade.\n%s\n", current, exe, strings.TrimPrefix(m.Version, "v"), relayBehindAdvice(Version))
			return nil
		}
		if client.Newer(m.Version, Version) {
			fmt.Fprintf(out, "A newer tincan release is available: %s.\n", m.Version)
		}
		fmt.Fprintf(out, "Current: tincan %s at %s\nAvailable from the relay: %s (%s)\nRun tincan upgrade to install it.\n", current, exe, available, name)
		return nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".tincan-upgrade-*")
	if err != nil {
		return fmt.Errorf("write next to %s: %w", exe, err)
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	if err := r.DownloadDist(ctx, name, io.MultiWriter(tmp, h)); err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s: got sha256 %s, the relay's manifest says %s; %s was not changed", name, got, want, exe)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return err
	}
	// Flush to disk before the rename, so a crash cannot leave exe
	// pointing at a partly written binary.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Rename gives the path a new inode. Overwriting a signed binary in
	// place gets it killed on macOS.
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	done = true
	fmt.Fprintf(out, "Upgraded %s from tincan %s to %s.\n", exe, current, available)
	msg, err := postUpgradeRefresh(exe)
	fmt.Fprint(out, msg)
	if err != nil {
		fmt.Fprintf(out, "tincan services refresh did not finish (%v); run it again to update the macOS services.\n", err)
	}
	fmt.Fprint(out, reloadAdvice(mcpserver.ReadLaunches(mcpserver.LaunchDir(client.ConfigPath())), available, mcpserver.LaunchRunning))
	return nil
}

// postUpgradeRefresh runs the new binary's services refresh on macOS, so the
// LaunchAgents start through the new build's Agent Tincan.app and restart on
// the new binary. Tests replace it.
var postUpgradeRefresh = func(exe string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", nil
	}
	out, err := exec.Command(exe, "services", "refresh", "--restart").CombinedOutput()
	return string(out), err
}

// relayBehindAdvice says how to bring a relay up to clientVersion. A bare
// tincan relay-upgrade installs the relay's own dist release, which is the
// older one, so the release is fetched from GitHub by tag.
func relayBehindAdvice(clientVersion string) string {
	return fmt.Sprintf("To upgrade the relay, ask the owner to run `tincan relay-upgrade --from-github v%s` from an admin device.", strings.TrimPrefix(clientVersion, "v"))
}

// reloadAdvice tells the agent how to get its long-running tincan processes
// onto the new build. Each app reloads tincan mcp its own way, so running
// servers on another build are named with their app's step; with none
// recorded, the step for every app is listed.
func reloadAdvice(ls []mcpserver.Launch, newVersion string, running func(mcpserver.Launch) bool) string {
	var b strings.Builder
	b.WriteString("Restart any long-running tincan processes (tincan wait or listen loops, tincan mcp servers) so they run the new build.\n")
	newVersion = strings.TrimPrefix(newVersion, "v")
	var stale []string
	for _, l := range ls {
		if !l.Ended.IsZero() || l.Version == "" || strings.TrimPrefix(l.Version, "v") == newVersion || !running(l) {
			continue
		}
		who := l.Client
		if who == "" {
			who = "an app"
		}
		kind := mcpserver.HostKind(l.Client)
		stale = append(stale, fmt.Sprintf("  %s (pid %d, tincan %s): %s\n", who, l.PID, l.Version, mcpserver.ReloadStep(kind)))
	}
	if len(stale) > 0 {
		b.WriteString("These tincan mcp servers still run the build they started with:\n")
		for _, s := range stale {
			b.WriteString(s)
		}
		return b.String()
	}
	b.WriteString("To reload tincan mcp in an app:\n")
	for _, kind := range []string{mcpserver.HostClaudeCode, mcpserver.HostCodex, mcpserver.HostCursor, mcpserver.HostGeneric} {
		fmt.Fprintf(&b, "  %s: %s\n", hostLabel(kind), mcpserver.ReloadStep(kind))
	}
	return b.String()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
