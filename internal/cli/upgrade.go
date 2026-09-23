package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func upgradeCmd() *cobra.Command {
	var relayURL string
	var check bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Replace this tincan binary with the release the relay serves",
		Long: `Download the tincan build for this platform from the relay (tincan relay
--dist), verify its sha256 against the relay's manifest, and replace the
running binary. Agents without GitHub access update this way.

The new binary is written next to the old one and renamed over it, so the old
file is never modified in place. Long-running tincan processes (wait or listen
loops, MCP servers) keep running the old build until they are restarted.

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
			return upgrade(cmd.Context(), r, exe, check, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().BoolVar(&check, "check", false, "only report the current and available versions; change nothing")
	return cmd
}

// upgrade replaces the binary at exe with the relay's build for this
// platform. The binary is untouched unless the download matches the
// manifest's sha256. check reports without writing anything.
func upgrade(ctx context.Context, r *client.Relay, exe string, check bool, out io.Writer) error {
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
	if check {
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
	fmt.Fprintf(out, "Upgraded %s from tincan %s to %s.\n"+
		"Restart any long-running tincan processes (tincan wait or listen loops, tincan mcp servers) so they run the new build.\n",
		exe, current, available)
	return nil
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
