package store

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// AgentStat is one agent's record of asks that finished in a window.
// MedianReply is over answered asks only. HasMedian is false when none has a
// known reply time, since a median of zero is a real one.
type AgentStat struct {
	Answered    int
	Failed      int
	Expired     int
	MedianReply time.Duration
	HasMedian   bool
}

// agentStatsQuery has one arm for each way an ask finishes, because history
// is kept for good and the read holds the store's one connection. Replies
// come through replies_created, so that arm costs what the window holds;
// CROSS JOIN keeps SQLite from starting at requests instead. Expiries are the
// requests_status_to rows, which only expired asks add to.
const agentStatsQuery = `SELECT r.to_agent, r.status,
	CASE WHEN r.was_held = 1 THEN (SELECT MAX(a.at) FROM audit a WHERE a.request_id = r.id AND a.event = 'approved') ELSE r.created_at END,
	q.created_at
	FROM replies q CROSS JOIN requests r ON r.id = q.request_id
	WHERE q.created_at >= ? AND r.status IN ('answered', 'failed') AND r.kind NOT IN ('ping', 'notify')
	AND NOT (r.was_held = 1 AND r.approved = 0)
	UNION ALL
	SELECT r.to_agent, r.status, NULL, NULL
	FROM requests r LEFT JOIN replies q ON q.request_id = r.id
	WHERE r.status = 'expired' AND r.kind NOT IN ('ping', 'notify')
	AND NOT (r.was_held = 1 AND r.approved = 0)
	AND COALESCE(q.status, '') != 'needs_input'
	AND MAX(r.expires_at, r.progress_at) >= ?`

// AgentStats returns the record of every agent with an ask that finished at
// or after since: counts and durations, never text. Pings and notifies are
// not work asked of a teammate. A decline, a cancel and an ask still open
// are not outcomes that say whether it answers, a hold the owner never
// approved never reached it, and an ask that expired on an unanswered
// clarification was waiting on its asker: only Answer removes the question's
// reply row. A held ask's reply time starts at its approval,
// so the owner's delay is not charged to the teammate; with no approval
// event to read, it counts but has no reply time. An expired ask finished at
// its deadline, or at its last progress if that kept the claim alive past it:
// updated_at says only when Sweep ran, which a relay that was down runs late.
func (s *Store) AgentStats(ctx context.Context, since time.Time) (map[string]AgentStat, error) {
	rows, err := s.db.QueryContext(ctx, agentStatsQuery, since.UnixMilli(), since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]AgentStat)
	replies := make(map[string][]int64)
	for rows.Next() {
		var agent, status string
		var started, replied sql.NullInt64
		if err := rows.Scan(&agent, &status, &started, &replied); err != nil {
			return nil, err
		}
		stat := out[agent]
		switch envelope.Status(status) {
		case envelope.StatusAnswered:
			stat.Answered++
			if started.Valid && replied.Valid {
				replies[agent] = append(replies[agent], max(replied.Int64-started.Int64, 0))
			}
		case envelope.StatusFailed:
			stat.Failed++
		case envelope.StatusExpired:
			stat.Expired++
		}
		out[agent] = stat
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for agent, ms := range replies {
		stat := out[agent]
		stat.MedianReply, stat.HasMedian = time.Duration(medianMillis(ms))*time.Millisecond, true
		out[agent] = stat
	}
	return out, nil
}

// medianMillis is the median of ms, sorting it in place. An even count takes
// the midpoint of the middle two.
func medianMillis(ms []int64) int64 {
	slices.Sort(ms)
	n := len(ms)
	if n%2 == 1 {
		return ms[n/2]
	}
	return ms[n/2-1] + (ms[n/2]-ms[n/2-1])/2
}
