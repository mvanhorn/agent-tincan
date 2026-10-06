package cli

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// A command's exit code survives withRejoinHints, including when the hint
// wraps the error.
func TestExitCodeSurvivesRejoinHints(t *testing.T) {
	useConfig(t, client.Config{})
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"silent", &ExitError{Code: 2, Silent: true}},
		{"wrapped by hint", &ExitError{Code: 2, Err: errors.New("not a joined agent")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := &cobra.Command{Use: "tincan", SilenceUsage: true, SilenceErrors: true}
			root.AddCommand(&cobra.Command{Use: "get", RunE: func(*cobra.Command, []string) error { return tc.err }})
			withRejoinHints(root)
			root.SetArgs([]string{"get"})
			err := root.ExecuteContext(context.Background())
			code, silent := ExitStatus(err)
			want := tc.err.(*ExitError)
			if code != 2 || silent != want.Silent {
				t.Fatalf("ExitStatus(%v) = %d, %v; want 2, %v", err, code, silent, want.Silent)
			}
		})
	}
}

// A command that failed because the SOCKS proxy could not reach the relay
// says the tunnel is down, that nothing was sent, and that an emailed
// request can be answered by email.
func TestSOCKSFailureGetsTunnelHint(t *testing.T) {
	useConfig(t, client.Config{Relay: "http://tincan-relay"})
	socks := &url.Error{Op: "Get", URL: "http://tincan-relay/v1/inbox", Err: &net.OpError{Op: "socks connect", Net: "tcp", Err: errors.New("unknown error general SOCKS server failure")}}
	root := &cobra.Command{Use: "tincan", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(&cobra.Command{Use: "inbox", RunE: func(*cobra.Command, []string) error { return socks }})
	withRejoinHints(root)
	root.SetArgs([]string{"inbox"})
	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("want the SOCKS failure")
	}
	for _, want := range []string{"socks connect", "tailnet tunnel to the relay is not up", "nothing was sent", "replying to that email"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not say %q", err, want)
		}
	}
}

// Any other error exits 1 with its message; no error exits 0.
func TestExitStatusPlainError(t *testing.T) {
	if code, silent := ExitStatus(errors.New("relay unreachable")); code != 1 || silent {
		t.Fatalf("plain error = %d, %v; want 1 with the message", code, silent)
	}
	if code, silent := ExitStatus(nil); code != 0 || !silent {
		t.Fatalf("nil = %d, %v", code, silent)
	}
}
