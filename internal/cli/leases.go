package cli

import (
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// leaseCmd is tincan lease: advisory, expiring leases on opaque resources,
// so teammates can see who has taken what.
func leaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lease",
		Short: "Advisory work leases: see who has taken what, and take it first",
	}
	cmd.AddCommand(leaseAcquireCmd(), leaseReleaseCmd(), leaseListCmd())
	return cmd
}

// leaseRelay is the configured agent, or with --relay or --socket the
// admin path that lets an admin device or the relay host act on any lease.
func leaseRelay(relayURL, socket string) (*client.Relay, error) {
	if relayURL != "" || socket != "" {
		return adminRelay(socket, relayURL)
	}
	r, _, err := connect()
	return r, err
}

func leaseAcquireCmd() *cobra.Command {
	var ttl time.Duration
	var note string
	cmd := &cobra.Command{
		Use:   "acquire <resource>",
		Short: "Take a lease, or renew one you hold; a trailing * covers a prefix",
		Long: `Take a lease on a resource, or renew one you hold. The resource is any
string the team agrees on, such as agent-tincan:internal/relay/*. A trailing *
covers everything under that prefix. A teammate's overlapping lease fails the
command and names its holder and when it lapses. Leases are advisory and lapse
on their own after --ttl, 24 hours at most.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if ttl <= 0 {
				return errors.New("--ttl must be positive")
			}
			r, _, err := connect()
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			seconds := int64(ttl / time.Second)
			if ttl%time.Second > 0 {
				seconds++
			}
			res, err := client.AcquireLease(cmd.Context(), r, args[0], note, seconds)
			if err != nil {
				return err
			}
			cmd.Println(res.Text(time.Now()))
			return nil
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", time.Hour, "how long the lease lasts, 24h at most")
	cmd.Flags().StringVar(&note, "note", "", "one line on what you are doing, shown to teammates")
	return cmd
}

func leaseReleaseCmd() *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "release <resource>",
		Short: "Drop your lease, named exactly as acquired; an admin can drop any lease with --relay or --socket",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := leaseRelay(relayURL, socket)
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			release := client.ReleaseLease
			if relayURL != "" || socket != "" {
				release = client.AdminReleaseLease
			}
			res, err := release(cmd.Context(), r, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("Released %q, held by %s.\n", res.Resource, res.Holder)
			return nil
		},
	}
	adminFlags(cmd, &relayURL, &socket)
	return cmd
}

func leaseListCmd() *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the team's live leases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := leaseRelay(relayURL, socket)
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			leases, err := client.ListLeases(cmd.Context(), r)
			if err != nil {
				return err
			}
			cmd.Print(client.FormatLeases(leases, time.Now()))
			return nil
		},
	}
	adminFlags(cmd, &relayURL, &socket)
	return cmd
}
