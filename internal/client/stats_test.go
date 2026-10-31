package client_test

import (
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func TestStatsField(t *testing.T) {
	for _, tc := range []struct {
		stats client.AgentStats
		want  string
	}{
		{client.AgentStats{}, ""},
		{client.AgentStats{Answered: 12, Failed: 1, MedianReplyMS: new(int64(180_000))}, "answered=12/13 median=3m"},
		{client.AgentStats{Answered: 12, Expired: 1, MedianReplyMS: new(int64(45_000))}, "answered=12/13 median=45s"},
		{client.AgentStats{Answered: 1, MedianReplyMS: new(int64(300))}, "answered=1/1 median=<1s"},
		{client.AgentStats{Answered: 1, MedianReplyMS: new(int64(0))}, "answered=1/1 median=<1s"},
		{client.AgentStats{Answered: 1}, "answered=1/1"},
		{client.AgentStats{Answered: 1, MedianReplyMS: new(int64(2 * 3_600_000))}, "answered=1/1 median=2h"},
		{client.AgentStats{Failed: 1, Expired: 2}, "answered=0/3"},
	} {
		if got := (client.AgentInfo{Stats: tc.stats}).StatsField(); got != tc.want {
			t.Errorf("StatsField(%+v) = %q, want %q", tc.stats, got, tc.want)
		}
	}
}
