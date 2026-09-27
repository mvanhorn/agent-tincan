// Command tincan lets AI agents on one tailnet ask each other to do things.
// The same binary runs the relay and the per-agent client.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mvanhorn/agent-tincan/internal/cli"
)

// Version is set at link time by the release build.
var Version = "0.0.1-dev"

func main() {
	cli.Version = Version
	if err := cli.Root().ExecuteContext(context.Background()); err != nil {
		code, silent := cli.ExitStatus(err)
		if !silent {
			fmt.Fprintln(os.Stderr, "tincan:", err)
		}
		os.Exit(code)
	}
}
