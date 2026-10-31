package client

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// oldRelay answers capabilities as a relay does, and records every other call.
type oldRelay struct {
	caps  Capabilities
	calls []string
}

func (o *oldRelay) Raw(_ context.Context, method, path string, _, out any) error {
	if path == "/v1/capabilities" {
		*(out.(*Capabilities)) = o.caps
		return nil
	}
	o.calls = append(o.calls, method+" "+path)
	return nil
}

// A relay that does not advertise schedules is refused before anything is
// sent: nothing may reach an old relay that would send the ask at once.
func TestScheduleAskRefusesOldRelay(t *testing.T) {
	old := &oldRelay{}
	_, err := ScheduleAsk(t.Context(), old, ScheduledAsk{To: "muse", Body: "x", Every: time.Hour})
	if !errors.Is(err, ErrSchedulesUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Schedules(t.Context(), old, false); !errors.Is(err, ErrSchedulesUnsupported) {
		t.Fatalf("list: %v", err)
	}
	if _, err := CancelSchedule(t.Context(), old, 1); !errors.Is(err, ErrSchedulesUnsupported) {
		t.Fatalf("cancel: %v", err)
	}
	if len(old.calls) != 0 {
		t.Fatalf("calls reached the relay: %v", old.calls)
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if at, every, err := ParseWhen("2026-10-31T09:00:00Z", "", now); err != nil || !at.Equal(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)) || every != 0 {
		t.Fatalf("rfc3339 = %v %v %v", at, every, err)
	}
	if at, every, err := ParseWhen("2h", "1h", now); err != nil || !at.Equal(now.Add(2*time.Hour)) || every != time.Hour {
		t.Fatalf("offset = %v %v %v", at, every, err)
	}
	if at, every, err := ParseWhen("", "30m", now); err != nil || !at.IsZero() || every != 30*time.Minute {
		t.Fatalf("every only = %v %v %v", at, every, err)
	}
	for _, bad := range [][2]string{{"tomorrow", ""}, {"", "soon"}, {"", "0s"}, {"", "-1h"}, {"1h", "500ms"}} {
		if _, _, err := ParseWhen(bad[0], bad[1], now); err == nil {
			t.Fatalf("%v parsed", bad)
		}
	}
}

func TestFormatSchedules(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	out := FormatSchedules([]envelope.Schedule{
		{ID: 3, From: "grokbot", To: "muse", Body: "weekly\nreport", NextAt: now.Add(time.Hour), EverySeconds: 3600, Blocked: true},
		{ID: 4, From: "grokbot", To: "codex", Body: strings.Repeat("x", 300), NextAt: now.Add(time.Hour)},
	}, now)
	for _, want := range []string{"#3 grokbot to muse", "every 1h0m0s", "weekly report", "last fire refused", "#4 grokbot to codex", "once"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q lacks %q", out, want)
		}
	}
	if strings.Contains(out, strings.Repeat("x", 200)) {
		t.Errorf("body not cut: %q", out)
	}
	hidden := FormatSchedules([]envelope.Schedule{{ID: 5, From: "grokbot", To: "muse", Body: "\x1b[2K\x1b[1A\u202egone\x07", NextAt: now.Add(time.Hour)}}, now)
	if strings.ContainsAny(hidden, "\x1b\u202e\x07") || !strings.Contains(hidden, "[2K [1A gone") {
		t.Fatalf("control text reached the listing: %q", hidden)
	}
	if got := FormatSchedules(nil, now); !strings.Contains(got, "No scheduled asks") {
		t.Fatalf("empty = %q", got)
	}
}
