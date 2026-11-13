package cli

import (
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// serviceStamp starts a line a serve command writes to its log with the
// local time, in the same layout the services use (client.LogStamp), so a
// restart shows when it happened.
func serviceStamp() string {
	return time.Now().Format(client.LogStamp) + " "
}
