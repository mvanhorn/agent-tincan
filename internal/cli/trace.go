package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// traceResp is the relay's trace response.
type traceResp struct {
	TraceID string             `json:"trace_id"`
	Steps   []envelope.Result  `json:"steps"`
	Events  []store.AuditEvent `json:"events"`
}

// adminRelay lets admin commands go through the local admin socket on the
// relay host instead of the network.
func adminRelay(socket, relayURL string) (*client.Relay, error) {
	if socket == "" && relayURL == "" {
		// On the relay machine itself the local admin socket makes this
		// machine the admin: no --admin device and no flags needed.
		socket = localAdminSocket()
	}
	if socket != "" {
		return client.NewRelaySocket(socket), nil
	}
	r, _, err := relayFor(relayURL)
	return r, err
}

func traceCmd() *cobra.Command {
	var socket, relayURL string
	var limit int
	cmd := &cobra.Command{
		Use:   "trace [trace-id]",
		Short: "Show who asked what: one request chain, or the most recent chains (admin)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				var out struct {
					Traces []envelope.Result `json:"traces"`
				}
				if err := r.Raw(cmd.Context(), "GET", fmt.Sprintf("/v1/trace?limit=%d", limit), nil, &out); err != nil {
					return err
				}
				for _, st := range out.Traces {
					cmd.Printf("%s  %s -> %s  %-9s %s\n", st.Request.TraceID, st.Request.From, st.Request.To, st.Status, oneLine(st.Request.Body))
				}
				if len(out.Traces) == 0 {
					cmd.Println("No requests yet.")
				}
				return nil
			}
			var tr traceResp
			if err := r.Raw(cmd.Context(), "GET", "/v1/trace/"+args[0], nil, &tr); err != nil {
				return err
			}
			cmd.Print(formatTrace(tr))
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().IntVar(&limit, "limit", 20, "how many recent chains to list")
	return cmd
}

func formatTrace(tr traceResp) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trace %s\n", tr.TraceID)
	for _, st := range tr.Steps {
		fmt.Fprintf(&b, "%s%s -> %s  [%s]  %s\n", strings.Repeat("  ", max(st.Request.Hop-1, 0)), st.Request.From, st.Request.To, st.Status, oneLine(st.Request.Body))
		if st.Reply != nil {
			fmt.Fprintf(&b, "%s  reply from %s: %s\n", strings.Repeat("  ", max(st.Request.Hop-1, 0)), st.Reply.From, oneLine(st.Reply.Body))
		}
	}
	if len(tr.Events) > 0 {
		b.WriteString("Events:\n")
		for _, e := range tr.Events {
			fmt.Fprintf(&b, "  #%d %-9s %-10s %s\n", e.Seq, e.Event, e.Actor, e.RequestID)
		}
	}
	return b.String()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

func auditCmd() *cobra.Command {
	var socket, relayURL string
	cmd := &cobra.Command{
		Use:   "audit-verify",
		Short: "Check that the relay's audit log has not been altered (admin)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			var out struct {
				OK      bool   `json:"ok"`
				Checked int    `json:"checked"`
				Error   string `json:"error"`
			}
			if err := r.Raw(cmd.Context(), "GET", "/v1/admin/audit/verify", nil, &out); err != nil {
				return err
			}
			if !out.OK {
				return fmt.Errorf("audit log check failed after %d entries: %s", out.Checked, out.Error)
			}
			cmd.Printf("Audit log intact (%d entries).\n", out.Checked)
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	return cmd
}

// localAdminSocket returns the relay's admin socket in the default state
// dir when a relay runs on this machine, or "".
func localAdminSocket() string {
	p := filepath.Join(defaultStateDir(), "admin.sock")
	if st, err := os.Stat(p); err == nil && st.Mode()&os.ModeSocket != 0 {
		return p
	}
	return ""
}
