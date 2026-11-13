package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/spf13/cobra"
)

func pingCmd() *cobra.Command {
	var wait time.Duration
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ping <agent>",
		Short: "Check a teammate's tincan path without model reasoning",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if wait <= 0 {
				return fmt.Errorf("--wait must be positive")
			}
			r, _, err := connect()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), wait)
			defer cancel()
			start := time.Now()
			req, err := r.Send(ctx, args[0], "", envelope.KindPing, "", false)
			if err != nil {
				return err
			}
			for {
				result, err := r.Get(ctx, req.ID, client.MaxInlineWait)
				if err != nil {
					return fmt.Errorf("ping %s (request %s): %w", args[0], req.ID, err)
				}
				if !result.Done() {
					continue
				}
				elapsed := time.Since(start)
				if asJSON {
					if err := json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
						Result      client.Result `json:"result"`
						RoundTripMS int64         `json:"round_trip_ms"`
					}{result, elapsed.Milliseconds()}); err != nil {
						return err
					}
				} else if result.Reply != nil {
					cmd.Print(forTerminal(cmd.OutOrStdout(), fmt.Sprintf("%s: %s in %s\n", args[0], result.Reply.Body, elapsed.Round(time.Millisecond))))
				}
				if result.Status != envelope.StatusAnswered {
					return fmt.Errorf("ping %s (request %s) ended: %s", args[0], req.ID, result.Status)
				}
				return nil
			}
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", time.Minute, "maximum time to wait for pong")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result and round trip as JSON")
	return cmd
}
