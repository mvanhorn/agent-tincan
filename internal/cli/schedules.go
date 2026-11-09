package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// scheduleAsk is tincan ask with --at or --every.
func scheduleAsk(cmd *cobra.Command, r *client.Relay, a client.ScheduledAsk, at, every string, asJSON bool) error {
	now := time.Now()
	var err error
	if a.At, a.Every, err = client.ParseWhen(at, every, now); err != nil {
		return err
	}
	sch, err := client.ScheduleAsk(cmd.Context(), r, a)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), sch)
	}
	cmd.Print(client.FormatSchedule(sch, now))
	return nil
}

// schedulesCmd lists scheduled asks.
func schedulesCmd() *cobra.Command {
	var all, asJSON bool
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "schedules",
		Short: "List your scheduled asks, or with --all everyone's (admin devices only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := scheduleRelay(all, socket, relayURL)
			if err != nil {
				return err
			}
			list, err := client.Schedules(cmd.Context(), r, all)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), list)
			}
			cmd.Print(client.FormatSchedules(list, time.Now()))
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list every agent's schedules (admin devices only)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	adminFlags(cmd, &relayURL, &socket)
	return cmd
}

// unscheduleCmd stops a scheduled ask.
func unscheduleCmd() *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "unschedule <id>",
		Short: "Stop a scheduled ask (the relay host, or an admin device with --relay or --socket, can stop anyone's)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("schedule id %q is not a number", args[0])
			}
			// On the relay host the local admin socket stops anything
			// tincan schedules --all shows, with no flags, as for every
			// admin command.
			r, err := scheduleRelay(relayURL != "" || socket != "" || localAdminSocket() != "", socket, relayURL)
			if err != nil {
				return err
			}
			sch, err := client.CancelSchedule(cmd.Context(), r, id)
			if err != nil {
				return err
			}
			cmd.Printf("Cancelled scheduled ask #%d from %s to %s.\n", sch.ID, sch.From, sch.To)
			return nil
		},
	}
	adminFlags(cmd, &relayURL, &socket)
	return cmd
}

// scheduleRelay is the admin relay for an admin call, else this agent's.
func scheduleRelay(admin bool, socket, relayURL string) (*client.Relay, error) {
	if admin {
		return adminRelay(socket, relayURL)
	}
	r, _, err := connect()
	return r, err
}
