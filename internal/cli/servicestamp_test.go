package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func TestServiceStampIsTheServiceLogLayout(t *testing.T) {
	s := serviceStamp()
	if !strings.HasSuffix(s, " ") {
		t.Fatalf("stamp %q must end with a space", s)
	}
	if _, err := time.Parse(client.LogStamp, strings.TrimSuffix(s, " ")); err != nil {
		t.Fatalf("stamp %q does not parse as client.LogStamp: %v", s, err)
	}
}
